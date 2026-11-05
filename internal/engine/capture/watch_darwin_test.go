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

func TestUnscopedAddEchoGetsIndexFromLinkAddress(t *testing.T) {
	m := &route.RouteMessage{Type: unix.RTM_ADD, Flags: unix.RTF_UP | unix.RTF_HOST, Addrs: []route.Addr{&route.Inet4Addr{IP: [4]byte{192, 0, 2, 10}}, &route.LinkAddr{Index: 42, Name: "utun42"}, nil}}
	events := changes([]route.Message{m})
	if len(events) != 1 || events[0].Index != 42 || !events[0].Added {
		t.Fatal("own route acknowledgement lost its interface", events)
	}
	if got := messageIndex(50, m.Addrs); got != 50 {
		t.Fatal("overrode explicit header identity", got)
	}
	if got := messageIndex(0, []route.Addr{&route.LinkAddr{Name: "utun42"}}); got != 0 {
		t.Fatal("guessed a live index from a name")
	}
}
