//go:build darwin

package capture

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestDarwinRouteDecoding(t *testing.T) {
	messages := []route.Message{
		&route.RouteMessage{Flags: unix.RTF_UP, Index: 1, Addrs: []route.Addr{unix.RTAX_DST: &route.Inet4Addr{}, unix.RTAX_GATEWAY: &route.Inet4Addr{IP: [4]byte{192, 0, 2, 1}}, unix.RTAX_NETMASK: &route.Inet4Addr{}}},
		&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_IFSCOPE, Index: 2, Addrs: []route.Addr{unix.RTAX_DST: &route.Inet4Addr{IP: [4]byte{192, 0, 2, 0}}, unix.RTAX_NETMASK: &route.Inet4Addr{IP: [4]byte{255, 255, 255, 0}}}},
	}
	routes := decodeRoutes(messages, map[int]string{1: "en0", 2: "en5"})
	if len(routes) != 2 || routes[0].Prefix != prefix("0.0.0.0/0") || routes[0].Gateway != addr("192.0.2.1") || routes[1].Prefix != prefix("192.0.2.0/24") || !routes[1].Scoped {
		t.Fatal(routes)
	}
}

func TestDarwinRouteDecodingExcludesKernelCaches(t *testing.T) {
	message := func(destination string, flags, index int, gateway string) route.Message {
		p := prefix(destination)
		toAddr := func(ip netip.Addr) route.Addr {
			if ip.Is4() {
				return &route.Inet4Addr{IP: ip.As4()}
			}
			return &route.Inet6Addr{IP: ip.As16()}
		}
		mask := make([]byte, p.Addr().BitLen()/8)
		for bit := range p.Bits() {
			mask[bit/8] |= byte(0x80 >> uint(bit%8))
		}
		maskAddr, _ := netip.AddrFromSlice(mask)
		addrs := []route.Addr{unix.RTAX_DST: toAddr(p.Addr()), unix.RTAX_NETMASK: toAddr(maskAddr)}
		if gateway != "" {
			addrs[unix.RTAX_GATEWAY] = toAddr(addr(gateway))
		}
		if p.Bits() == p.Addr().BitLen() {
			flags |= unix.RTF_HOST
		}
		return &route.RouteMessage{Flags: unix.RTF_UP | flags, Index: index, Addrs: addrs}
	}
	messages := []route.Message{
		message("0.0.0.0/0", unix.RTF_PRCLONING, 1, "192.0.2.1"),
		message("198.51.100.10/32", unix.RTF_WASCLONED, 1, "192.0.2.1"), // Unscoped clone is not a host-capture conflict.
		message("192.0.2.0/24", unix.RTF_CLONING, 1, ""),
		message("192.0.2.53/32", unix.RTF_LLINFO, 1, ""), // ARP entry is not a DNS-capture conflict.
		message("203.0.113.0/24", unix.RTF_STATIC, 2, ""),
		message("203.0.113.10/32", unix.RTF_WASCLONED|unix.RTF_IFSCOPE, 2, ""),
		message("2001:db8::5/128", unix.RTF_LLINFO, 1, ""), // NDP is filtered independently of WASCLONED.
		message("192.0.2.9/32", unix.RTF_STATIC, 1, "192.0.2.1"),
		message("192.0.2.54/32", unix.RTF_WASCLONED|unix.RTF_LLINFO, 1, ""),
		message("2001:db8:1::1/128", unix.RTF_STATIC, 3, ""),                                    // A static capture route remains visible for cleanup.
		message("2001:db8:1::2/128", unix.RTF_STATIC|unix.RTF_LLINFO, 3, ""),                    // Explicit IPv6 hosts also carry ND metadata.
		message("2001:db8:1::3/128", unix.RTF_STATIC|unix.RTF_LLINFO|unix.RTF_WASCLONED, 3, ""), // Inherited STATIC never promotes a clone.
	}
	routes := decodeRoutes(messages, map[int]string{1: "test0", 2: "utun12", 3: "utun13"})
	want := []Route{
		{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1, Gateway: addr("192.0.2.1")},
		{Prefix: prefix("192.0.2.0/24"), Interface: "test0", Index: 1},
		{Prefix: prefix("203.0.113.0/24"), Interface: "utun12", Index: 2},
		{Prefix: prefix("192.0.2.9/32"), Interface: "test0", Index: 1, Gateway: addr("192.0.2.1")},
		{Prefix: prefix("2001:db8:1::1/128"), Interface: "utun13", Index: 3},
		{Prefix: prefix("2001:db8:1::2/128"), Interface: "utun13", Index: 3},
	}
	if !slices.Equal(routes, want) {
		t.Fatalf("decoded routes = %v; want %v", routes, want)
	}
	if _, err := Build(routes, nil, false, []netip.Prefix{prefix("198.51.100.10/32")}); err != nil {
		t.Fatal("kernel clone caused a scoped capture conflict", err)
	}
	plan, err := Build(routes, []netip.Addr{addr("192.0.2.53")}, false, nil)
	if err != nil || !slices.Contains(plan.Prefixes, prefix("192.0.2.53/32")) {
		t.Fatalf("neighbor cache prevented a LAN DNS capture: plan=%v error=%v", plan, err)
	}
	for _, host := range []string{"192.0.2.9/32", "2001:db8:1::1/128", "2001:db8:1::2/128"} {
		if _, err := Build(routes, nil, true, []netip.Prefix{prefix(host)}); api.Code(err) != api.ErrRouteConflict {
			t.Fatalf("static host route %s must still conflict: %v", host, err)
		}
	}
	for target, iface := range map[string]string{"198.51.100.10": "test0", "192.0.2.53": "test0", "203.0.113.10": "utun12"} {
		if got := SystemInterface(routes, addr(target)); got != iface {
			t.Fatalf("system interface for %s = %s; want %s", target, got, iface)
		}
	}
}

func TestLiveSnapshotIsReadOnly(t *testing.T) {
	driver := nativeDriver{}
	boot, err := driver.BootSession()
	if err != nil || boot == "" {
		t.Fatal("cannot read the current boot session", err)
	}
	again, err := driver.BootSession()
	if err != nil || again != boot {
		t.Fatal("boot session identity is unstable", err)
	}
	routes, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) == 0 {
		t.Fatal("routing snapshot is empty")
	}
	for _, r := range routes {
		if !r.Prefix.IsValid() {
			t.Fatal("invalid route prefix")
		}
	}
	prefixes, err := driver.InterfacePrefixes()
	if err != nil || len(prefixes) == 0 {
		t.Fatal("cannot read assigned interface prefixes", err)
	}
	for _, p := range prefixes {
		if !p.IsValid() {
			t.Fatal("invalid assigned interface prefix")
		}
	}
	// Do not print this Mac's addresses, gateways or network inventory.
}
