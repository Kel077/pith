//go:build darwin

package capture

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"golang.org/x/sys/unix"
)

func defaultRouteEvent(t *testing.T, typ int, p netip.Prefix, scoped bool, index int) Change {
	t.Helper()
	flags := unix.RTF_UP | unix.RTF_STATIC
	if scoped {
		flags |= unix.RTF_IFSCOPE
	}
	addrs := map[int][]byte{unix.RTAX_DST: wireIPv4("0.0.0.0"), unix.RTAX_GATEWAY: wireLink(index), unix.RTAX_NETMASK: wireIPv4("0.0.0.0")}
	if p.Addr().Is6() {
		flags |= unix.RTF_GATEWAY
		addrs = map[int][]byte{unix.RTAX_DST: wireIPv6("::", 0, 0), unix.RTAX_GATEWAY: wireIPv6("fe80::1", index, 0), unix.RTAX_NETMASK: wireMask(0, true)}
	}
	events := parseRouteEvents(wireMessage(typ, flags, index, addrs, false))
	if len(events) != 1 || events[0].Err != nil || events[0].Unparsed || events[0].Prefix != p || !events[0].ScopeKnown || events[0].Scoped != scoped || events[0].Index != index || events[0].Deleted != (typ == unix.RTM_DELETE) || events[0].Added != (typ == unix.RTM_ADD) {
		t.Fatal("default route metadata was not preserved", events)
	}
	return events[0]
}

func vpnDefaultGuard(t *testing.T, p netip.Prefix, scoped bool) (*Guard, *liveDriver, *time.Time) {
	t.Helper()
	routes := []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1}, {Prefix: prefix("::/0"), Interface: "test0", Index: 1}}
	if !scoped {
		routes = slices.DeleteFunc(routes, func(r Route) bool { return r.Prefix == p })
	}
	routes = append(routes, Route{Prefix: p, Interface: "utun50", Index: 50, Scoped: scoped},
		Route{Prefix: prefix("192.0.2.0/24"), Interface: "utun50", Index: 50},
		Route{Prefix: prefix("2001:db8:10::/48"), Interface: "utun50", Index: 50})
	d := &liveDriver{fakeDriver: fakeDriver{routes: routes}, indices: map[string]int{"test0": 1, "utun50": 50}}
	c, err := Prepare(context.Background(), Options{Driver: d, IPv6: true, StatePath: filepath.Join(t.TempDir(), "capture.json")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	g, err := NewGuard(c, []api.Interface{{Name: "test0", Up: true}, {Name: "utun50", Up: true}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	g.now = func() time.Time { return now }
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g, d, &now
}

func publicForDefault(p netip.Prefix) netip.Addr {
	if p.Addr().Is6() {
		return addr("2001:db8:20::9")
	}
	return addr("203.0.113.9")
}

func TestScopedVPNDefaultWithdrawalKeepsImmediateSystemEgress(t *testing.T) {
	for _, p := range []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")} {
		for _, scoped := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/scoped=%t", p, scoped), func(t *testing.T) {
				g, d, now := vpnDefaultGuard(t, p, scoped)
				dest := publicForDefault(p)
				want := "utun50"
				if scoped {
					want = "test0"
				}
				if b, err := g.System(dest); err != nil || b.Name != want {
					t.Fatal("incorrect initial exit", b, err)
				}
				physical, _ := g.Binding("test0")
				d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Index == 50 })
				delete(d.indices, "utun50")
				if !scoped {
					d.routes = append(d.routes, Route{Prefix: p, Interface: "test0", Index: 1})
				}
				if err := g.Observe(defaultRouteEvent(t, unix.RTM_DELETE, p, scoped, 50)); err != nil {
					t.Fatal(err)
				}
				b, err := g.System(dest)
				if scoped && (err != nil || b.Name != "test0") {
					t.Fatal("scoped VPN default withdrawal stalled physical system egress", b, err)
				}
				if !scoped && err == nil {
					t.Fatal("unscoped VPN default withdrawal lost its settling bound")
				}
				if scoped && !g.settle[family(p)].IsZero() {
					t.Fatal("scoped event installed a settling timer")
				}
				if physical.Check() != nil || g.Err() != nil || g.Admit(addr("192.0.2.9")) == nil || g.Admit(addr("2001:db8:10::9")) == nil {
					t.Fatal("physical binding, capture lifetime or VPN prefix gate changed")
				}
				*now = now.Add(3 * time.Second)
				if b, err := g.System(dest); err != nil || b.Name != "test0" {
					t.Fatal("stable physical default failed to recover", b, err)
				}
			})
		}
	}
}

func TestEventSettlingRequiresKnownUnscopedDeletion(t *testing.T) {
	for _, p := range []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")} {
		for _, tc := range []struct {
			name               string
			typ                int
			scoped             bool
			unknownScope       bool
			snapshotWithdrawal bool
			settles            bool
		}{
			{"scoped delete", unix.RTM_DELETE, true, false, false, false},
			{"unscoped delete", unix.RTM_DELETE, false, false, false, true},
			{"scoped add", unix.RTM_ADD, true, false, false, false},
			{"unscoped add", unix.RTM_ADD, false, false, false, false},
			{"scoped change", unix.RTM_CHANGE, true, false, false, false},
			{"unscoped change", unix.RTM_CHANGE, false, false, false, false},
			{"unscoped redirect", unix.RTM_REDIRECT, false, false, false, false},
			{"unknown scope", unix.RTM_DELETE, false, true, false, false},
			{"snapshot proves withdrawal despite unknown event scope", unix.RTM_DELETE, false, true, true, true},
		} {
			t.Run(p.String()+"/"+tc.name, func(t *testing.T) {
				g, d, now := vpnDefaultGuard(t, p, tc.scoped)
				event := defaultRouteEvent(t, tc.typ, p, tc.scoped, 50)
				if tc.unknownScope {
					event.ScopeKnown = false
				}
				// Keep the RIB unchanged except in the explicit snapshot control.
				// An unknown-scope event alone cannot imply an unscoped withdrawal.
				if tc.snapshotWithdrawal {
					d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Index == 50 && r.Prefix == p })
					d.routes = append(d.routes, Route{Prefix: p, Interface: "test0", Index: 1})
				}
				if err := g.Observe(event); err != nil {
					t.Fatal(err)
				}
				if tc.settles {
					if !g.settle[family(p)].Equal(now.Add(3 * time.Second)) {
						t.Fatal("known unscoped deletion did not start the bounded wait")
					}
				} else if !g.settle[family(p)].IsZero() {
					t.Fatal("event without known unscoped withdrawal started settling")
				}
			})
		}
	}
}

func TestScopedVPNReconnectionAndReindexKeepPhysicalSystemEgress(t *testing.T) {
	for _, p := range []netip.Prefix{prefix("0.0.0.0/0"), prefix("::/0")} {
		for _, newName := range []string{"utun50", "utun51"} {
			t.Run(p.String()+"/"+newName, func(t *testing.T) {
				g, d, _ := vpnDefaultGuard(t, p, true)
				physical, _ := g.Binding("test0")
				oldVPN, _ := g.Binding("utun50")
				assertPhysical := func() {
					t.Helper()
					for _, dest := range []netip.Addr{addr("203.0.113.9"), addr("2001:db8:20::9")} {
						if b, err := g.System(dest); err != nil || b.Name != "test0" || b.Index != 1 {
							t.Fatal("scoped VPN lifecycle disrupted physical system egress", dest, b, err)
						}
					}
					if physical.Check() != nil || g.Err() != nil || !g.settle[0].IsZero() || !g.settle[1].IsZero() {
						t.Fatal("unrelated established binding or capture was invalidated")
					}
				}
				observeNotification := func(index int, up bool) {
					t.Helper()
					flags := 0
					if up {
						flags = unix.IFF_UP
					}
					events := parseRouteEvents(wireMessage(unix.RTM_IFINFO, flags, index, nil, false))
					if len(events) != 1 || g.Observe(events[0]) != nil {
						t.Fatal("interface notification failed", events)
					}
					assertPhysical()
				}
				observeNotification(50, false)
				vpnRoutes := slices.DeleteFunc(slices.Clone(d.routes), func(r Route) bool { return r.Index != 50 })
				d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Index == 50 })
				delete(d.indices, "utun50")
				if err := g.Observe(defaultRouteEvent(t, unix.RTM_DELETE, p, true, 50)); err != nil {
					t.Fatal(err)
				}
				assertPhysical()
				for i := range vpnRoutes {
					vpnRoutes[i].Interface, vpnRoutes[i].Index = newName, 51
				}
				d.routes = append(d.routes, vpnRoutes...)
				d.indices[newName] = 51
				if err := g.Interfaces([]api.Interface{{Name: "test0", Up: true}, {Name: newName, Up: true}}); err != nil {
					t.Fatal(err)
				}
				observeNotification(51, true)
				for _, typ := range []int{unix.RTM_ADD, unix.RTM_CHANGE} {
					if err := g.Observe(defaultRouteEvent(t, typ, p, true, 51)); err != nil {
						t.Fatal(err)
					}
					assertPhysical()
				}
				if b, err := g.Binding(newName); err != nil || b.Index != 51 || oldVPN.Check() == nil {
					t.Fatal("VPN binding adopted the wrong interface lifetime", b, err)
				}
				if g.Admit(addr("192.0.2.9")) == nil || g.Admit(addr("2001:db8:10::9")) == nil {
					t.Fatal("VPN prefix gate changed during reconstruction")
				}
			})
		}
	}
}
