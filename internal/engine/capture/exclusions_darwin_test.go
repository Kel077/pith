//go:build darwin

package capture

import (
	"net/netip"
	"slices"
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestDarwinAddressExclusionsOnlyCacheEndpoints(t *testing.T) {
	v4 := ipv4Candidates[0].Addr().Next()
	v6 := ipv6Candidates[0].Addr().Next()
	message := func(ip netip.Addr, flags int) *route.RouteMessage {
		var destination route.Addr
		if ip.Is4() {
			destination = &route.Inet4Addr{IP: ip.As4()}
		} else {
			destination = &route.Inet6Addr{IP: ip.As16()}
		}
		return &route.RouteMessage{Flags: flags, Addrs: []route.Addr{unix.RTAX_DST: destination}}
	}
	wideMask := message(addr("198.51.100.10"), unix.RTF_UP|unix.RTF_WASCLONED)
	wideMask.Addrs = append(wideMask.Addrs, nil, &route.Inet4Addr{IP: [4]byte{255, 255, 255, 0}})
	messages := []route.Message{
		message(v4, unix.RTF_UP|unix.RTF_WASCLONED),
		message(v6, unix.RTF_UP|unix.RTF_LLINFO|unix.RTF_IFSCOPE),
		message(v4, unix.RTF_UP|unix.RTF_LLINFO|unix.RTF_WASCLONED),
		wideMask,
		message(addr("192.0.2.10"), unix.RTF_UP|unix.RTF_HOST|unix.RTF_STATIC),
		message(addr("2001:db8::10"), unix.RTF_UP|unix.RTF_HOST|unix.RTF_STATIC),
		message(addr("192.0.2.0"), unix.RTF_UP|unix.RTF_CLONING),
		message(addr("203.0.113.0"), unix.RTF_UP|unix.RTF_PRCLONING),
		message(addr("192.0.2.11"), unix.RTF_WASCLONED),
		message(addr("0.0.0.0"), unix.RTF_UP|unix.RTF_LLINFO),
		message(addr("::"), unix.RTF_UP|unix.RTF_WASCLONED),
		&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_WASCLONED},
		&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_LLINFO, Addrs: []route.Addr{nil}},
		&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_LLINFO, Addrs: []route.Addr{&route.LinkAddr{}}},
		&route.InterfaceMessage{},
	}
	want := []netip.Prefix{netip.PrefixFrom(v4, 32), netip.PrefixFrom(v6, 128), prefix("198.51.100.10/32")}
	if got := decodeAddressExclusions(messages); !slices.Equal(got, want) {
		t.Fatalf("endpoint exclusions = %v; want %v", got, want)
	}
	if got := decodeAddressExclusions(nil); len(got) != 0 {
		t.Fatalf("empty cache excluded addresses: %v", got)
	}
}
