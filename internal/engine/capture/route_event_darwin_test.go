//go:build darwin

package capture

import (
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Synthetic Darwin ABI fixtures, not a captured routing table. Link-local and
// multicast constants exercise kernel formats; public destinations use RFC 3849.
func wireIPv6(ip string, embedded, zone int) []byte {
	b := make([]byte, unix.SizeofSockaddrInet6)
	b[0], b[1] = byte(len(b)), unix.AF_INET6
	a := addr(ip).As16()
	copy(b[8:24], a[:])
	if embedded != 0 {
		binary.BigEndian.PutUint16(b[10:12], uint16(embedded))
	}
	binary.NativeEndian.PutUint32(b[24:28], uint32(zone))
	return b
}

func wireMask(bits int, trimmed bool) []byte {
	b := make([]byte, unix.SizeofSockaddrInet6)
	copy(b[8:24], net.CIDRMask(bits, 128))
	if trimmed {
		for len(b) > 0 && b[len(b)-1] == 0 {
			b = b[:len(b)-1]
		}
	}
	if len(b) != 0 {
		b[0] = byte(len(b))
	}
	return b
}

func wireLink(index int) []byte {
	name := "utun50"
	b := make([]byte, 8+len(name))
	b[0], b[1], b[5] = byte(len(b)), unix.AF_LINK, byte(len(name))
	binary.NativeEndian.PutUint16(b[2:4], uint16(index))
	copy(b[8:], name)
	return b
}

func wireMessage(typ, flags, index int, addrs map[int][]byte, padded bool) []byte {
	size, indexOffset, bitmapOffset := unix.SizeofRtMsghdr, 4, 12
	if typ == unix.RTM_NEWADDR || typ == unix.RTM_DELADDR {
		size, indexOffset, bitmapOffset = unix.SizeofIfaMsghdr, 12, 4
	} else if typ == unix.RTM_IFINFO {
		size, indexOffset, bitmapOffset = unix.SizeofIfMsghdr, 12, 4
	} else if typ == unix.RTM_IFINFO2 {
		size, indexOffset, bitmapOffset = unix.SizeofIfMsghdr2, 12, 4
	}
	b := make([]byte, size)
	b[2], b[3] = unix.RTM_VERSION, byte(typ)
	binary.NativeEndian.PutUint16(b[indexOffset:indexOffset+2], uint16(index))
	binary.NativeEndian.PutUint32(b[8:12], uint32(flags))
	var bitmap uint32
	last := -1
	for slot := range addrs {
		last = max(last, slot)
	}
	for slot := 0; slot <= last; slot++ {
		a, ok := addrs[slot]
		if !ok {
			continue
		}
		bitmap |= 1 << slot
		b = append(b, a...)
		if padded || slot < last {
			space := (max(len(a), 1) + 3) &^ 3
			b = append(b, make([]byte, space-len(a))...)
		}
	}
	binary.NativeEndian.PutUint32(b[bitmapOffset:bitmapOffset+4], bitmap)
	binary.NativeEndian.PutUint16(b[:2], uint16(len(b)))
	return b
}

func TestDarwinIPv6RoutingMessages(t *testing.T) {
	for _, typ := range []int{unix.RTM_ADD, unix.RTM_DELETE, unix.RTM_CHANGE} {
		for _, padded := range []bool{false, true} {
			for _, tc := range []struct {
				name, destination, gateway, prefix string
				flags, embedded, zone, bits        int
				link, omitMask                     bool
			}{
				{"KAME destination and gateway", "fe80::", "fe80::1", "fe80::/64", unix.RTF_IFSCOPE, 50, 0, 64, false, false},
				{"scoped default with zone", "::", "fe80::1", "::/0", unix.RTF_IFSCOPE | unix.RTF_GATEWAY, 0, 50, 0, false, false},
				{"scoped default with embedded scope", "::", "fe80::1", "::/0", unix.RTF_IFSCOPE | unix.RTF_GATEWAY, 50, 0, 0, false, true},
				{"multicast short mask", "ff00::", "", "ff00::/8", unix.RTF_IFSCOPE, 0, 0, 8, true, false},
				{"scoped multicast KAME", "ff02::", "", "ff02::/32", unix.RTF_IFSCOPE, 50, 0, 32, true, false},
				{"host with only link gateway", "2001:db8::9", "", "2001:db8::9/128", unix.RTF_HOST | unix.RTF_STATIC, 0, 0, 128, true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					embedDst := tc.embedded
					if tc.destination == "::" {
						embedDst = 0
					}
					addrs := map[int][]byte{unix.RTAX_DST: wireIPv6(tc.destination, embedDst, tc.zone)}
					if tc.link {
						addrs[unix.RTAX_GATEWAY] = wireLink(50)
					} else {
						addrs[unix.RTAX_GATEWAY] = wireIPv6(tc.gateway, tc.embedded, tc.zone)
					}
					if !tc.omitMask {
						addrs[unix.RTAX_NETMASK] = wireMask(tc.bits, true)
					}
					flags := tc.flags
					if typ != unix.RTM_DELETE {
						flags |= unix.RTF_UP
					}
					b := wireMessage(typ, flags, 50, addrs, padded)
					events := parseRouteEvents(b)
					if len(events) != 1 || events[0].Err != nil || events[0].Unparsed || events[0].Diagnostic != "" || events[0].Index != 50 || events[0].Prefix != prefix(tc.prefix) || events[0].Added != (typ == unix.RTM_ADD) {
						t.Fatalf("type=%d padded=%t: %+v", typ, padded, events)
					}
					meta, _ := routeHeader(b)
					aligned, problem := meta.sockaddrs(b)
					messages, err := route.ParseRIB(route.RIBTypeRoute, aligned)
					if problem != "" || err != nil {
						t.Fatal(problem, err)
					}
					m := messages[0].(*route.RouteMessage)
					m.Flags |= unix.RTF_UP
					routes := decodeRoutes(messages, map[int]string{50: "utun50"})
					if len(routes) != 1 || routes[0].Prefix != events[0].Prefix || tc.gateway != "" && routes[0].Gateway != addr(tc.gateway) {
						t.Fatal("snapshot and event disagree", routes)
					}
					if tc.gateway != "" && (tc.embedded != 0 || tc.zone != 0) {
						if got := m.Addrs[unix.RTAX_GATEWAY].(*route.Inet6Addr).ZoneID; got != 50 {
							t.Fatal("lost gateway scope", got)
						}
					}
				})
			}
		}
	}
}

func TestDarwinAddressMessages(t *testing.T) {
	for _, typ := range []int{unix.RTM_NEWADDR, unix.RTM_DELADDR} {
		b := wireMessage(typ, 0, 50, map[int][]byte{unix.RTAX_NETMASK: wireMask(64, false), unix.RTAX_IFP: wireLink(50), unix.RTAX_IFA: wireIPv6("fe80::9", 50, 0)}, false)
		events := parseRouteEvents(b)
		if len(events) != 1 || events[0].Err != nil || events[0].Unparsed || !events[0].Address || events[0].Index != 50 || events[0].Added != (typ == unix.RTM_NEWADDR) || events[0].Prefix.IsValid() {
			t.Fatalf("type=%d: %+v", typ, events)
		}
	}
}

func TestUnreadableInterfaceNotificationRetainsHeaderIdentity(t *testing.T) {
	for _, typ := range []int{unix.RTM_NEWADDR, unix.RTM_DELADDR, unix.RTM_IFINFO, unix.RTM_IFINFO2} {
		link := wireLink(50)
		link[5] = 254
		b := wireMessage(typ, unix.IFF_UP, 50, map[int][]byte{unix.RTAX_IFP: link}, false)
		events := parseRouteEvents(b)
		address := typ == unix.RTM_NEWADDR || typ == unix.RTM_DELADDR
		if len(events) != 1 || events[0].Err != nil || !events[0].Unparsed || events[0].Index != 50 || events[0].Address != address || events[0].Interface == address || !strings.Contains(events[0].Diagnostic, "family=18/len=14") {
			t.Fatalf("type=%d: %+v", typ, events)
		}
	}
}

func TestMalformedGatewayRetainsOnlyAnIndependentlyDecodedKey(t *testing.T) {
	for _, p := range []string{"fe80::/64", "ff00::/8", "2001:db8::/32"} {
		key := prefix(p)
		link := wireLink(50)
		link[5] = 254 // Impossible name length, but intact sockaddr boundaries.
		b := wireMessage(unix.RTM_CHANGE, unix.RTF_UP, 50, map[int][]byte{unix.RTAX_DST: wireIPv6(key.Addr().String(), 0, 0), unix.RTAX_GATEWAY: link, unix.RTAX_NETMASK: wireMask(key.Bits(), true)}, false)
		events := parseRouteEvents(b)
		if len(events) != 1 || !events[0].Unparsed || events[0].Prefix != key || events[0].Scoped || events[0].NonIP || !strings.Contains(events[0].Diagnostic, "invalid routing sockaddr encoding") {
			t.Fatal("could not isolate a readable key from an unreadable gateway", events)
		}
		if strings.Contains(events[0].Diagnostic, key.Addr().String()) {
			t.Fatal("key leaked into diagnostic")
		}
	}
}

func TestHostRouteArgumentsAndDarwinIPv6NetEcho(t *testing.T) {
	for _, action := range []string{"add", "delete"} {
		for _, p := range []string{"192.0.2.9/32", "2001:db8::9/128", "192.0.2.0/24", "2001:db8::/32"} {
			network := prefix(p)
			family, kind, destination := "-inet", "-net", p
			if network.Addr().Is6() {
				family = "-inet6"
			}
			if network.Bits() == network.Addr().BitLen() {
				kind, destination = "-host", network.Addr().String()
			}
			if got := routeArguments(action, network, "utun42"); !slices.Equal(got, []string{"-n", action, family, kind, destination, "-interface", "utun42"}) {
				t.Fatal(action, p, got)
			}
		}
	}
	// network_cmds route.c omits the IPv6 /128 mask; -net clears HOST too.
	// Do not silently guess a host prefix from that malformed configuration.
	for _, typ := range []int{unix.RTM_ADD, unix.RTM_DELETE} {
		b := wireMessage(typ, unix.RTF_UP|unix.RTF_STATIC, 0, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, false)
		bad := parseRouteEvents(b)
		if len(bad) != 1 || !bad[0].Unparsed || bad[0].Index != 42 || !strings.Contains(bad[0].Diagnostic, "without HOST or netmask") {
			t.Fatalf("old -net /128 echo was not reproduced: %+v", bad)
		}
		binary.NativeEndian.PutUint32(b[8:12], unix.RTF_UP|unix.RTF_STATIC|unix.RTF_HOST)
		good := parseRouteEvents(b)
		if len(good) != 1 || good[0].Unparsed || good[0].Prefix != prefix("2001:db8::9/128") {
			t.Fatal("explicit host echo is not usable", good)
		}
	}
}

func TestRouteDiagnosticMetadataAndFrameIsolation(t *testing.T) {
	bad := wireMessage(unix.RTM_ADD, unix.RTF_UP|unix.RTF_IFSCOPE, 50, map[int][]byte{unix.RTAX_DST: wireIPv6("fe80::9", 50, 0), unix.RTAX_GATEWAY: wireLink(50)}, false)
	good := wireMessage(unix.RTM_DELETE, unix.RTF_HOST, 42, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0)}, false)
	events := parseRouteEvents(append(bad, good...))
	if len(events) != 2 || !events[0].Unparsed || events[0].Err != nil || events[1].Prefix != prefix("2001:db8::9/128") {
		t.Fatalf("unrelated event hid a capture withdrawal: %+v", events)
	}
	for _, want := range []string{"RTM type=1", "flags=0x", "index=50", "0:family=30/len=28", "1:family=18/len=14"} {
		if !strings.Contains(events[0].Diagnostic, want) {
			t.Fatal("missing metadata", want, events[0].Diagnostic)
		}
	}
	for _, secret := range []string{"fe80", "2001:db8", "utun50"} {
		if strings.Contains(events[0].Diagnostic, secret) {
			t.Fatal("address or name in diagnostic")
		}
	}
	// A real truncated payload must not be repaired as alignment.
	truncated := good[:len(good)-1]
	binary.NativeEndian.PutUint16(truncated[:2], uint16(len(truncated)))
	events = parseRouteEvents(truncated)
	if len(events) != 1 || !events[0].Unparsed || !strings.Contains(events[0].Diagnostic, "truncated sockaddr payload") {
		t.Fatal(events)
	}
	for _, malformed := range [][]byte{{}, {1}, {0, 0, unix.RTM_VERSION, unix.RTM_ADD}, {255, 255, unix.RTM_VERSION, unix.RTM_ADD}} {
		events = parseRouteEvents(malformed)
		if len(malformed) > 0 && (len(events) != 1 || events[0].Err == nil) {
			t.Fatal(events)
		}
	}
}

func TestHostPrefixWithoutMaskSlot(t *testing.T) {
	for _, a := range []netip.Addr{addr("192.0.2.9"), addr("2001:db8::9")} {
		var dst route.Addr = &route.Inet6Addr{IP: a.As16()}
		if a.Is4() {
			dst = &route.Inet4Addr{IP: a.As4()}
		}
		routes := decodeRoutes([]route.Message{&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_HOST, Addrs: []route.Addr{dst}}}, nil)
		if len(routes) != 1 || routes[0].Prefix != netip.PrefixFrom(a, a.BitLen()) {
			t.Fatal(routes)
		}
	}
}

func FuzzRouteEventFrames(f *testing.F) {
	f.Add(wireMessage(unix.RTM_ADD, unix.RTF_UP|unix.RTF_HOST, 42, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, false))
	f.Add(wireMessage(unix.RTM_CHANGE, unix.RTF_UP|unix.RTF_IFSCOPE, 50, map[int][]byte{unix.RTAX_DST: wireIPv6("ff00::", 0, 0), unix.RTAX_GATEWAY: wireLink(50), unix.RTAX_NETMASK: wireMask(8, true)}, false))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = parseRouteEvents(b)
	})
}
