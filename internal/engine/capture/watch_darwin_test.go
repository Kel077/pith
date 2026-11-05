//go:build darwin

package capture

import (
	"errors"
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestRouteEventsPreserveTransitionsButIgnoreCacheChurn(t *testing.T) {
	message := func(typ, flags int) *route.RouteMessage {
		return &route.RouteMessage{Type: typ, Flags: flags, Index: 50, Addrs: []route.Addr{&route.Inet4Addr{IP: [4]byte{192, 0, 2, 9}}, nil, nil}}
	}
	failed := message(unix.RTM_ADD, unix.RTF_HOST)
	failed.Err = errors.New("operation rejected")
	messages := []route.Message{
		message(unix.RTM_ADD, unix.RTF_UP|unix.RTF_HOST|unix.RTF_WASCLONED),
		message(unix.RTM_DELETE, unix.RTF_HOST|unix.RTF_LLINFO),
		message(unix.RTM_GET, unix.RTF_UP|unix.RTF_HOST), failed,
		message(unix.RTM_DELETE, unix.RTF_HOST),
		message(unix.RTM_ADD, unix.RTF_UP|unix.RTF_HOST),
		&route.InterfaceMessage{Index: 50, Flags: 0},
		&route.InterfaceMessage{Index: 50, Flags: unix.IFF_UP},
		&route.InterfaceAddrMessage{Type: unix.RTM_DELADDR, Index: 50},
	}
	events := changes(messages)
	if len(events) != 5 || events[0].Added || !events[1].Added || events[0].Prefix != prefix("192.0.2.9/32") || !events[2].Interface || events[2].Up || !events[3].Up || events[4].Added {
		t.Fatal(events)
	}
}
