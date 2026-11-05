//go:build darwin

package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
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

func wireIPv4(ip string) []byte {
	b := make([]byte, unix.SizeofSockaddrInet4)
	b[0], b[1] = byte(len(b)), unix.AF_INET
	a := addr(ip).As4()
	copy(b[4:8], a[:])
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
	} else if typ == unix.RTM_NEWMADDR || typ == unix.RTM_DELMADDR {
		size, indexOffset, bitmapOffset = unix.SizeofIfmaMsghdr, 12, 4
	} else if typ == unix.RTM_NEWMADDR2 {
		size, indexOffset, bitmapOffset = unix.SizeofIfmaMsghdr2, 12, 4
	}
	b := make([]byte, size)
	// Darwin embeds if_data (96 bytes) after the 16-byte IFINFO prefix, and
	// if_data64 (128 bytes) after IFINFO2's 32-byte prefix. Statistics are not
	// sockaddrs, even when there are no trailing addresses at all.
	dataOffset := 0
	if typ == unix.RTM_IFINFO {
		dataOffset = 16
	} else if typ == unix.RTM_IFINFO2 {
		dataOffset = 32
	}
	if dataOffset != 0 {
		b[dataOffset] = 1 // IFT_OTHER
		binary.NativeEndian.PutUint32(b[dataOffset+8:dataOffset+12], 1500)
		binary.NativeEndian.PutUint32(b[dataOffset+16:dataOffset+20], 1000000)
		binary.NativeEndian.PutUint32(b[dataOffset+24:dataOffset+28], 7)
	}
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

func TestDarwinIfInfoWithoutSockaddrs(t *testing.T) {
	for _, tc := range []struct{ typ, size int }{{unix.RTM_IFINFO, 16 + 96}, {unix.RTM_IFINFO2, 32 + 128}} {
		b := wireMessage(tc.typ, unix.IFF_UP|unix.IFF_POINTOPOINT|unix.IFF_RUNNING|unix.IFF_MULTICAST, 42, nil, false)
		if len(b) != tc.size {
			t.Fatal("incorrect Darwin interface header including if_data", len(b), tc.size)
		}
		// x/net intentionally skips this normal PF_ROUTE notification because it
		// has no RTA_IFP/name. A watcher still needs its header's index and flags.
		messages, err := route.ParseRIB(route.RIBTypeRoute, b)
		if err != nil || len(messages) != 0 {
			t.Fatal("unexpected dependency behavior", messages, err)
		}
		events := parseRouteEvents(b)
		if len(events) != 1 || !events[0].Interface || !events[0].Up || events[0].Index != 42 || events[0].Err != nil || events[0].Unparsed || events[0].Diagnostic != "" {
			t.Fatalf("normal IFINFO became a capture fault: %+v", events)
		}
	}
}

func TestDarwinNotificationHeaderFamilies(t *testing.T) {
	for _, tc := range []struct {
		typ, size  int
		iface, add bool
	}{
		{unix.RTM_IFINFO, 112, true, false},
		{unix.RTM_IFINFO2, 160, true, false},
		{unix.RTM_NEWADDR, 20, false, true},
		{unix.RTM_DELADDR, 20, false, false},
		{unix.RTM_NEWMADDR, 16, false, true},
		{unix.RTM_DELMADDR, 16, false, false},
		{unix.RTM_NEWMADDR2, 20, false, true},
	} {
		addrs := map[int][]byte{unix.RTAX_IFP: wireLink(42)}
		if !tc.iface {
			addrs[unix.RTAX_IFA] = wireIPv6("ff02::1", 42, 0)
		}
		b := wireMessage(tc.typ, unix.IFF_UP|unix.IFF_MULTICAST, 42, addrs, false)
		if tc.iface && (int(b[tc.size]) != len(wireLink(42)) || b[tc.size+1] != unix.AF_LINK) {
			t.Fatal("sockaddr was not placed after embedded interface data")
		}
		meta, known := routeHeader(b)
		if !known || meta.route || !meta.complete || meta.size != tc.size {
			t.Fatalf("type=%d: wrong Darwin header layout: %+v", tc.typ, meta)
		}
		events := parseRouteEvents(b)
		if len(events) != 1 || events[0].Err != nil || events[0].Unparsed || events[0].Index != 42 || events[0].Interface != tc.iface || events[0].Address == tc.iface || events[0].Added != tc.add || events[0].Up != tc.iface || events[0].Prefix.IsValid() {
			t.Fatalf("type=%d: notification interpreted as routing policy: %+v", tc.typ, events)
		}
	}
}

func TestDarwinUtunStartupSequencePreservesCapture(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		for _, delayed := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv6=%t/delayed=%t", ipv6, delayed), func(t *testing.T) {
				target := prefix("198.51.100.9/32")
				dst, assigned, mask := wireIPv4("198.51.100.9"), wireIPv4("192.0.2.1"), wireIPv4("255.255.255.0")
				if ipv6 {
					target = prefix("2001:db8:2::9/128")
					dst, assigned, mask = wireIPv6("2001:db8:2::9", 0, 0), wireIPv6("2001:db8:1::1", 0, 0), wireMask(126, true)
				}
				d := &notificationDriver{liveDriver: liveDriver{fakeDriver: fakeDriver{routes: []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1}, {Prefix: prefix("::/0"), Interface: "test0", Index: 1}}}, indices: map[string]int{"test0": 1}}, up: true}
				c, err := Prepare(context.Background(), Options{Driver: d, IPv6: ipv6, Scopes: []netip.Prefix{target}, StatePath: filepath.Join(t.TempDir(), "capture.json")})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				g, err := NewGuard(c, []api.Interface{{Name: "test0", Up: true}})
				if err != nil {
					t.Fatal(err)
				}
				up := wireMessage(unix.RTM_IFINFO, unix.IFF_UP|unix.IFF_POINTOPOINT|unix.IFF_RUNNING|unix.IFF_MULTICAST, 42, nil, false)
				addressAddrs := map[int][]byte{unix.RTAX_IFP: wireLink(42), unix.RTAX_IFA: assigned, unix.RTAX_NETMASK: mask}
				flags := unix.RTF_UP | unix.RTF_HOST | unix.RTF_STATIC
				if ipv6 {
					addressAddrs[unix.RTAX_BRD] = nil
					flags |= unix.RTF_LLINFO
				}
				address := wireMessage(unix.RTM_NEWADDR, 0, 42, addressAddrs, false)
				add := wireMessage(unix.RTM_ADD, flags, 42, map[int][]byte{unix.RTAX_DST: dst, unix.RTAX_GATEWAY: wireLink(42)}, false)
				batch := append(append(append([]byte(nil), up...), address...), add...)
				events := parseRouteEvents(batch)
				if len(events) != 3 {
					t.Fatal("startup sequence lost events", events)
				}
				if delayed {
					if err := c.Activate(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				for i, event := range events {
					if i == 2 && !delayed {
						if err := c.Activate(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					reads := d.reads
					if event.Unparsed || event.Err != nil || event.Diagnostic != "" {
						t.Fatalf("normal startup event became a diagnostic: %+v", event)
					}
					if err := g.Observe(event); err != nil || g.Err() != nil || c.closed.Load() || d.reads <= reads {
						t.Fatal("startup sequence stopped capture or skipped verification", i, err)
					}
				}
				if !c.active.Load() || d.device.closed {
					t.Fatal("startup notifications ended capture")
				}
				if binding, err := g.System(target.Addr()); err != nil || binding.Name != "test0" {
					t.Fatal("immediate SafeStart system egress was lost", binding, err)
				}
			})
		}
	}
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

func TestDarwinIPv6PointToPointAddressWithoutPeer(t *testing.T) {
	for _, typ := range []int{unix.RTM_NEWADDR, unix.RTM_DELADDR} {
		for _, padded := range []bool{false, true} {
			// in6.c leaves a non-NULL, AF_UNSPEC/zero-length destination on
			// point-to-point addresses without a peer. rt_newaddrmsg sets RTA_BRD;
			// rt_msg1 copies zero bytes at that final slot, unlike a padded RIB.
			link := append(wireLink(42), make([]byte, 6)...)
			link[0] = byte(len(link))
			mask := wireMask(126, false)
			mask[1] = unix.AF_INET6
			b := wireMessage(typ, 0x100, 42, map[int][]byte{
				unix.RTAX_NETMASK: mask, unix.RTAX_IFP: link,
				unix.RTAX_IFA: wireIPv6("2001:db8:1::1", 0, 0), unix.RTAX_BRD: nil,
			}, padded)
			if !padded && len(b) != 20+28+20+28 {
				t.Fatal("unexpected unpadded ifa_msghdr layout", len(b))
			}
			events := parseRouteEvents(b)
			if len(events) != 1 || !events[0].Address || events[0].Err != nil || events[0].Unparsed || events[0].Diagnostic != "" {
				t.Fatalf("type=%d padded=%t: absent peer was treated as truncated: %+v", typ, padded, events)
			}
		}
	}
}

func TestOmittedSockaddrIsLimitedToKnownEmptyFormats(t *testing.T) {
	for _, tc := range []struct {
		typ   int
		addrs map[int][]byte
	}{
		{unix.RTM_ADD, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: nil}},
		{unix.RTM_NEWADDR, map[int][]byte{unix.RTAX_IFP: wireLink(42), unix.RTAX_IFA: nil}},
		{unix.RTM_NEWADDR, map[int][]byte{unix.RTAX_IFA: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_BRD: {28}}},
	} {
		events := parseRouteEvents(wireMessage(tc.typ, unix.RTF_UP|unix.RTF_HOST|unix.RTF_STATIC, 42, tc.addrs, false))
		if len(events) != 1 || !events[0].Unparsed || events[0].Diagnostic == "" {
			t.Fatal("missing required address payload was silently invented", events)
		}
	}
}

func TestDarwinStaticIPv6HostRetainsNeighborMetadata(t *testing.T) {
	// route(8) sends UP|HOST|STATIC and an AF_LINK gateway. XNU nd6_rtrequest
	// also sets LLINFO for this explicit RTM_ADD on a point-to-point interface.
	const flags = unix.RTF_UP | unix.RTF_HOST | unix.RTF_STATIC | unix.RTF_LLINFO
	addrs := map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}
	b := wireMessage(unix.RTM_GET, flags, 42, addrs, true)
	messages, err := route.ParseRIB(route.RIBTypeRoute, b)
	if err != nil {
		t.Fatal(err)
	}
	routes := decodeRoutes(messages, map[int]string{42: "utun42"})
	if len(routes) != 1 || routes[0].Prefix != prefix("2001:db8::9/128") || routes[0].Scoped || routes[0].Gateway.IsValid() || routes[0].Index != 42 {
		t.Fatal("explicit IPv6 capture was lost as neighbor churn", routes)
	}
	for _, typ := range []int{unix.RTM_ADD, unix.RTM_CHANGE, unix.RTM_DELETE} {
		events := parseRouteEvents(wireMessage(typ, flags, 42, addrs, false))
		if len(events) != 1 || events[0].Unparsed || events[0].Err != nil || events[0].Prefix != routes[0].Prefix {
			t.Fatal("static IPv6 ownership event was filtered", typ, events)
		}
	}
	for _, cache := range []int{unix.RTF_HOST | unix.RTF_LLINFO, unix.RTF_HOST | unix.RTF_STATIC | unix.RTF_WASCLONED, flags | unix.RTF_WASCLONED} {
		if events := parseRouteEvents(wireMessage(unix.RTM_ADD, cache|unix.RTF_UP, 42, addrs, false)); len(events) != 0 {
			t.Fatal("transient neighbor or clone entered the policy event stream", events)
		}
	}
}

func TestDarwinSnapshotDetailsIncludeFilteredCandidates(t *testing.T) {
	const configured = unix.RTF_UP | unix.RTF_HOST | unix.RTF_STATIC | unix.RTF_LLINFO
	var data []byte
	for _, flags := range []int{configured, configured | unix.RTF_IFREF, unix.RTF_UP | unix.RTF_HOST | unix.RTF_LLINFO, unix.RTF_UP | unix.RTF_HOST | unix.RTF_WASCLONED | unix.RTF_STATIC, unix.RTF_HOST | unix.RTF_STATIC} {
		data = append(data, wireMessage(unix.RTM_GET, flags, 42, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, true)...)
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, data)
	if err != nil {
		t.Fatal(err)
	}
	routes, details := decodeRouteSnapshot(messages, map[int]string{42: "utun42"})
	if len(routes) != 2 || routes[0] != routes[1] || len(details) != 5 {
		t.Fatal("policy contains cache churn or detail lost filtered entries", routes, details)
	}
	for i, detail := range details {
		if detail.GatewayFamily != unix.AF_LINK || detail.Prefix != prefix("2001:db8::9/128") || !detail.Raw || detail.Policy != (i < 2) || detail.Scoped {
			t.Fatal("incorrect raw RIB metadata", i, detail)
		}
	}
}

// This memory-only driver exposes the same static IPv6 host through the raw RIB
// decoder on every read. Fake device close and Delete change its underlying
// route set, so cleanup cannot pass merely by dropping LLINFO in the decoder.
type ipv6HostWireDriver struct{ fakeDriver }

func (d *ipv6HostWireDriver) Snapshot() ([]Route, error) {
	routes, _, err := d.SnapshotDetails()
	return routes, err
}
func (d *ipv6HostWireDriver) SnapshotDetails() ([]Route, []RouteDetail, error) {
	var data []byte
	for _, r := range d.routes {
		data = append(data, wireMessage(unix.RTM_GET, unix.RTF_UP|unix.RTF_HOST|unix.RTF_STATIC|unix.RTF_LLINFO, r.Index, map[int][]byte{
			unix.RTAX_DST: wireIPv6(r.Prefix.Addr().String(), 0, 0), unix.RTAX_GATEWAY: wireLink(r.Index),
		}, true)...)
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, data)
	if len(data) == 0 {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	routes, details := decodeRouteSnapshot(messages, map[int]string{42: "utun42"})
	return routes, details, nil
}

func TestDarwinStaticIPv6HostLifecycle(t *testing.T) {
	for _, mode := range []string{"stop", "emergency", "withdrawal"} {
		t.Run(mode, func(t *testing.T) {
			d := &ipv6HostWireDriver{}
			ctx := context.Background()
			p := prefix("2001:db8::9/128")
			opts := Options{Driver: d, IPv6: true, Scopes: []netip.Prefix{p}, StatePath: filepath.Join(t.TempDir(), "capture.json")}
			c, err := Prepare(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			g, err := NewGuard(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Activate(ctx); err != nil {
				t.Fatal(err)
			}
			if err := g.Validate(true); err != nil {
				t.Fatal("static IPv6 host was missing from ownership snapshot", err)
			}
			routes, _ := d.Snapshot()
			if !ownsRoutes(c.state, routes) || Recover(opts.StatePath, d) == nil {
				t.Fatal("journal recovery forgot a live static IPv6 capture")
			}
			switch mode {
			case "stop":
				if err := c.Close(); err != nil || c.Identity().EndedAt.IsZero() {
					t.Fatal("Stop did not confirm closure", err)
				}
			case "emergency":
				if err := cleanupStale(ctx, opts.StatePath, d, func(int) bool { return false }); err != nil {
					t.Fatal(err)
				}
			case "withdrawal":
				frame := wireMessage(unix.RTM_DELETE, unix.RTF_HOST|unix.RTF_STATIC|unix.RTF_LLINFO, 42, map[int][]byte{unix.RTAX_DST: wireIPv6(p.Addr().String(), 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, false)
				events := parseRouteEvents(frame)
				if len(events) != 1 || api.Code(g.Observe(events[0])) != api.ErrNetworkLost {
					t.Fatal("static IPv6 withdrawal was ignored", events)
				}
				return
			}
			if !slices.Contains(d.operations, "delete "+p.String()) {
				t.Fatal("cleanup skipped explicit host deletion", d.operations)
			}
			if _, err := os.Stat(opts.StatePath); !os.IsNotExist(err) {
				t.Fatal("cleaned host retained its journal", err)
			}
		})
	}
}

func TestUnreadableInterfaceNotificationRetainsHeaderIdentity(t *testing.T) {
	for _, typ := range []int{unix.RTM_NEWADDR, unix.RTM_DELADDR, unix.RTM_IFINFO, unix.RTM_IFINFO2, unix.RTM_NEWMADDR, unix.RTM_DELMADDR, unix.RTM_NEWMADDR2} {
		link := wireLink(50)
		link[5] = 254
		b := wireMessage(typ, unix.IFF_UP, 50, map[int][]byte{unix.RTAX_IFP: link}, false)
		events := parseRouteEvents(b)
		address := typ != unix.RTM_IFINFO && typ != unix.RTM_IFINFO2
		if len(events) != 1 || events[0].Err != nil || !events[0].Unparsed || events[0].Index != 50 || events[0].Address != address || events[0].Interface == address || !strings.Contains(events[0].Diagnostic, "family=18/len=14") {
			t.Fatalf("type=%d: %+v", typ, events)
		}
	}
}

func TestTruncatedNotificationRequestsSnapshot(t *testing.T) {
	b := wireMessage(unix.RTM_IFINFO, unix.IFF_UP, 42, nil, false)
	b = b[:20] // Fixed identity fields exist, but most if_data is missing.
	binary.NativeEndian.PutUint16(b[:2], uint16(len(b)))
	events := parseRouteEvents(b)
	if len(events) != 1 || !events[0].Interface || !events[0].Unparsed || events[0].Index != 42 || events[0].Err != nil || !strings.Contains(events[0].Diagnostic, "truncated notification header") {
		t.Fatal("notification payload directly revoked capture", events)
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
	f.Add(wireMessage(unix.RTM_NEWADDR, 0, 42, map[int][]byte{unix.RTAX_IFA: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_BRD: nil}, false))
	f.Add(wireMessage(unix.RTM_DELETE, unix.RTF_HOST|unix.RTF_STATIC|unix.RTF_LLINFO, 42, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, true))
	f.Add(wireMessage(unix.RTM_IFINFO, unix.IFF_UP, 42, nil, false))
	f.Add(wireMessage(unix.RTM_IFINFO2, unix.IFF_UP, 42, map[int][]byte{unix.RTAX_IFP: wireLink(42)}, false))
	f.Add(wireMessage(unix.RTM_NEWMADDR2, 0, 42, map[int][]byte{unix.RTAX_IFA: wireIPv6("ff02::1", 42, 0)}, false))
	f.Add(wireMessage(unix.RTM_ADD, unix.RTF_UP|unix.RTF_HOST, 42, map[int][]byte{unix.RTAX_DST: wireIPv6("2001:db8::9", 0, 0), unix.RTAX_GATEWAY: wireLink(42)}, false))
	f.Add(wireMessage(unix.RTM_CHANGE, unix.RTF_UP|unix.RTF_IFSCOPE, 50, map[int][]byte{unix.RTAX_DST: wireIPv6("ff00::", 0, 0), unix.RTAX_GATEWAY: wireLink(50), unix.RTAX_NETMASK: wireMask(8, true)}, false))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = parseRouteEvents(b)
	})
}
