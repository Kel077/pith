//go:build darwin

package netif

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/rib"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Only show/n.add/n.watch are used: scutil documents them in `help` and its
// manual. Neither changes the dynamic store or persistent system preferences.
func platformMetadata(ctx context.Context) (map[string]api.Interface, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/scutil")
	cmd.Stdin = strings.NewReader("show ^(Setup|State):/Network/(Service/[^/]+(/(Interface|IPv4|IPv6|DNS))?|Global/IPv[46])$ pattern\nquit\n")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read network configuration: %w", err)
	}
	store, err := parseStore(string(data))
	if err != nil {
		return nil, err
	}
	result := storeInterfaces(store)
	raw, err := rib.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, raw)
	if err != nil {
		return nil, err
	}
	links, err := interfaceNames()
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		r, ok := message.(*route.RouteMessage)
		if !ok || r.Flags&unix.RTF_UP == 0 || r.Flags&unix.RTF_HOST != 0 || len(r.Addrs) <= unix.RTAX_NETMASK {
			continue
		}
		dst, mask, gateway := routeAddress(r.Addrs[unix.RTAX_DST]), routeAddress(r.Addrs[unix.RTAX_NETMASK]), routeAddress(r.Addrs[unix.RTAX_GATEWAY])
		if !dst.IsValid() || !dst.IsUnspecified() || (mask.IsValid() && !mask.IsUnspecified()) {
			continue
		}
		name := links[r.Index]
		if name == "" {
			continue
		}
		iface := result[name]
		iface.Name = name
		if gateway.IsValid() {
			if gateway.Is4() {
				iface.Router = gateway.String()
			} else {
				iface.Router6 = gateway.String()
			}
		}
		if r.Flags&unix.RTF_IFSCOPE == 0 {
			iface.Primary = true
		}
		result[name] = iface
	}
	return result, nil
}

func routeAddress(a route.Addr) netip.Addr {
	switch value := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(value.IP)
	case *route.Inet6Addr:
		return netip.AddrFrom16(value.IP)
	default:
		return netip.Addr{}
	}
}

// IP_BOUND_IF and IPV6_BOUND_IF are from the SDK's netinet/in.h and
// netinet6/in6.h. Binding changes this socket only; it does not install routes.
func bindControl(index int) func(context.Context, string, string, syscall.RawConn) error {
	return func(ctx context.Context, network, address string, raw syscall.RawConn) error {
		if err := ValidateBinding(ctx, index); err != nil {
			return err
		}
		var bindErr error
		err := raw.Control(func(fd uintptr) {
			level, option := unix.IPPROTO_IP, unix.IP_BOUND_IF
			if strings.HasSuffix(network, "6") {
				level, option = unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF
			}
			bindErr = unix.SetsockoptInt(int(fd), level, option, index)
		})
		if err != nil {
			return err
		}
		if bindErr != nil {
			return bindErr
		}
		return ValidateBinding(ctx, index)
	}
}

// Watch emits an initial snapshot and later distinct snapshots. It keeps one
// idle scutil notification reader, reaps it on cancellation, and polls only to
// read again after a failed read. Cancel ctx when the consumer stops receiving.
// A reader error closes the stream; the subscriber can re-establish it with
// bounded backoff. A failed read doesn't: see follow.
func Watch(ctx context.Context) (<-chan []api.Interface, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "/usr/sbin/scutil")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	// Keep stdin open: scutil exits on EOF even while notifications are active.
	if _, err := io.WriteString(stdin, "n.add ^(State|Setup):/Network/.* pattern\nn.watch\n"); err != nil {
		cancel()
		stdin.Close()
		_ = cmd.Wait()
		return nil, err
	}
	changes := make(chan struct{}, 1)
	go func() {
		defer close(changes)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}()
	return follow(ctx, changes, List, time.Second, func() { cancel(); _ = cmd.Wait() })
}

// follow sends list's snapshot, then each distinct one read after changes
// says the network configuration changed, until ctx ends or changes closes;
// then it calls stop and closes the channel. A read can fail on a busy Mac, as
// when the route table doesn't come within its bound: the first is tried three
// times, and a later one keeps the last snapshot, and the stream, and is tried
// again at the next change or after pause, which doubles up to half a minute.
func follow(ctx context.Context, changes <-chan struct{}, list func(context.Context) ([]api.Interface, error), pause time.Duration, stop func()) (<-chan []api.Interface, error) {
	initial, err := list(ctx)
	for try := 1; err != nil && try < 3 && sleep(ctx, pause/4); try++ {
		initial, err = list(ctx)
	}
	if err != nil {
		stop()
		return nil, err
	}
	output := make(chan []api.Interface, 1)
	output <- initial
	go func() {
		defer func() { stop(); close(output) }()
		last := initial
		var retry <-chan time.Time
		wait := pause
		for {
			select {
			case <-ctx.Done():
				return
			case <-retry:
			case _, ok := <-changes:
				if !ok {
					return
				}
				// Coalesce bursts from one network reconfiguration.
				if !sleep(ctx, 150*time.Millisecond) {
					return
				}
				select {
				case _, ok := <-changes:
					if !ok {
						return
					}
				default:
				}
			}
			current, err := list(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				retry = time.After(wait)
				wait = min(2*wait, 30*time.Second)
				continue
			}
			retry, wait = nil, pause
			if !reflect.DeepEqual(current, last) {
				select {
				case output <- current:
					last = current
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return output, nil
}

// sleep waits d, or less if ctx ends first: then it reports false.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
