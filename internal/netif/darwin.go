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
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, fmt.Errorf("read routes: %w", err)
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, rib)
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
// idle scutil notification reader, reaps it on cancellation, and never polls.
// Cancel ctx when the consumer stops receiving. A discovery/reader error closes
// the stream; the subscriber can re-establish it with bounded backoff.
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
	initial, err := List(ctx)
	if err != nil {
		cancel()
		_ = cmd.Wait()
		return nil, err
	}
	output := make(chan []api.Interface, 1)
	output <- initial
	go func() {
		defer func() { cancel(); _ = cmd.Wait(); close(output) }()
		last := initial
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-changes:
				if !ok {
					return
				}
			}
			timer := time.NewTimer(150 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// Coalesce bursts from one network reconfiguration.
			select {
			case _, ok := <-changes:
				if !ok {
					return
				}
			default:
			}
			current, err := List(ctx)
			if err != nil {
				return
			}
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
