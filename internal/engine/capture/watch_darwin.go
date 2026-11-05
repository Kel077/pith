//go:build darwin

package capture

import (
	"context"
	"errors"
	"os"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Watch observes PF_ROUTE without writing to it. SDK net/route.h defines
// RTM_ADD/DELETE/CHANGE, NEWADDR/DELADDR and IFINFO; Darwin has no IFANNOUNCE.
// Address withdrawal and IFINFO retain lifetime transitions between snapshots.
// The nonblocking os.File uses Go's poller; cancellation closes and drains it.
func Watch(ctx context.Context) (<-chan Change, error) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "pith-route-observer")
	ch := make(chan Change, 64)
	stop := context.AfterFunc(ctx, func() { f.Close() })
	go func() {
		defer close(ch)
		defer f.Close()
		defer stop()
		data := make([]byte, 65536)
		for {
			n, err := f.Read(data)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				select {
				case ch <- Change{Err: err}:
				case <-ctx.Done():
				}
				return
			}
			messages, err := route.ParseRIB(route.RIBTypeRoute, data[:n])
			if err != nil {
				select {
				case ch <- Change{Err: err}:
				case <-ctx.Done():
				}
				return
			}
			for _, event := range changes(messages) {
				select {
				case ch <- event:
				default:
					return
				} // Overflow is a closed-stream fault.
			}
		}
	}()
	return ch, nil
}

func changes(messages []route.Message) []Change {
	var result []Change
	for _, message := range messages {
		switch m := message.(type) {
		case *route.RouteMessage:
			if m.Err != nil || m.Flags&(unix.RTF_WASCLONED|unix.RTF_LLINFO) != 0 {
				continue
			}
			if m.Type != unix.RTM_ADD && m.Type != unix.RTM_DELETE && m.Type != unix.RTM_CHANGE && m.Type != unix.RTM_REDIRECT {
				continue
			}
			copy := *m
			copy.Flags |= unix.RTF_UP // Deleted routes need not retain UP.
			routes := decodeRoutes([]route.Message{&copy}, map[int]string{})
			if len(routes) != 1 {
				result = append(result, Change{Err: errors.New("unreadable configured route event")})
				continue
			}
			result = append(result, Change{Index: messageIndex(m.Index, m.Addrs), Prefix: routes[0].Prefix, Added: m.Type == unix.RTM_ADD})
		case *route.InterfaceMessage:
			result = append(result, Change{Index: messageIndex(m.Index, m.Addrs), Interface: true, Up: m.Flags&unix.IFF_UP != 0})
		case *route.InterfaceAddrMessage:
			result = append(result, Change{Index: messageIndex(m.Index, m.Addrs), Added: m.Type == unix.RTM_NEWADDR})
		}
	}
	return result
}

// An unscoped RTM_ADD echo can retain the request's zero header index. Its
// AF_LINK sockaddr still identifies the requested interface. Do not resolve a
// name against the current table and accidentally accept a different lifetime.
// API references (no implementation copied):
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/rtsock.c
// https://github.com/apple-oss-distributions/network_cmds/blob/main/route.tproj/route.c
func messageIndex(index int, addrs []route.Addr) int {
	if index > 0 {
		return index
	}
	for _, addr := range addrs {
		if link, ok := addr.(*route.LinkAddr); ok && link.Index > 0 {
			return link.Index
		}
	}
	return 0
}
