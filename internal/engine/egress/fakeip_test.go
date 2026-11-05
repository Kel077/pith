package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/rules"
)

func fakeRoutes() []capture.Route {
	return []capture.Route{
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"},
		{Prefix: netip.MustParsePrefix("198.18.0.0/15"), Interface: "utun3"},
	}
}

func fakeIfaces() []api.Interface {
	return []api.Interface{
		{Name: "en0", Kind: api.KindEthernet, Up: true, DNS: []string{"192.0.2.53"}},
		{Name: "en2", Kind: api.KindWiFi, Up: true, DNS: []string{"192.0.2.54"}},
		{Name: "utun3", Kind: api.KindVPN, Up: true},
	}
}

func pipeConn(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a
}

func requireAttemptDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
		t.Error("unbounded interface attempt")
	}
}

func TestFakeIPPhysicalRouteResolvesTCPAndUDP(t *testing.T) {
	for _, tc := range []struct {
		proto api.Protocol
		fake  string
		real  string
	}{
		{api.TCP, "198.18.0.7:443", "192.0.2.80"},
		{api.UDP, "198.19.255.7:443", "192.0.2.81"},
		{api.TCP, "198.18.0.8:443", "2001:db8::80"},
	} {
		t.Run(string(tc.proto)+"/"+tc.real, func(t *testing.T) {
			remote := netip.MustParseAddrPort(tc.fake)
			real := netip.MustParseAddr(tc.real)
			dials, resolutions := 0, 0
			r := New(fakeRoutes(), fakeIfaces(), func(ctx context.Context, name, network, target string) (net.Conn, error) {
				dials++
				requireAttemptDeadline(t, ctx)
				family := "4"
				if real.Is6() {
					family = "6"
				}
				if name != "en0" || network != string(tc.proto)+family || target != netip.AddrPortFrom(real, remote.Port()).String() {
					t.Error("sent synthetic or incorrect destination", name, network, target)
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(ctx context.Context, iface api.Interface, domain string) ([]netip.Addr, error) {
				resolutions++
				requireAttemptDeadline(t, ctx)
				if iface.Name != "en0" || !slices.Equal(iface.DNS, []string{"192.0.2.53"}) || domain != "example.com" {
					t.Error("lost chosen interface DNS or domain", iface.Name, iface.DNS, domain)
				}
				return []netip.Addr{real}, nil
			}
			d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}}
			result, err := r.DialDomain(context.Background(), d, tc.proto, remote, "example.com")
			if err != nil || result.Outcome != api.Routed || result.Interface != "en0" || result.Problem != "" || dials != 1 || resolutions != 1 {
				t.Fatal(result, err, dials, resolutions)
			}
		})
	}
}

func TestFakeIPUnknownDomainAndUnmatchedPreserveSystemRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		domain string
		d      rules.Decision
		warn   bool
	}{
		{"unknown domain overrides strict physical route", "", rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}}, true},
		{"unmatched known domain", "example.com", rules.Decision{}, false},
		{"unmatched unknown domain", "", rules.Decision{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := netip.MustParseAddrPort("198.18.0.7:443")
			dials := 0
			r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, network, target string) (net.Conn, error) {
				dials++
				if name != "utun3" || target != remote.String() || network != "tcp4" {
					t.Error("lost original fake-IP owner", name, network, target)
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				t.Fatal("resolved a system-routed fake IP")
				return nil, nil
			}
			result, err := r.DialDomain(context.Background(), tc.d, api.TCP, remote, tc.domain)
			if err != nil || result.Outcome != api.System || result.Interface != "utun3" || dials != 1 || (result.Problem != "") != tc.warn {
				t.Fatal(result, err, dials)
			}
		})
	}
}

func TestFakeIPBlockWinsBeforeDNSOrFallback(t *testing.T) {
	r := New(fakeRoutes(), fakeIfaces(), func(context.Context, string, string, string) (net.Conn, error) {
		t.Fatal("dialed a blocked fake IP")
		return nil, nil
	})
	r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
		t.Fatal("resolved a blocked fake IP")
		return nil, nil
	}
	for _, domain := range []string{"", "example.com"} {
		d := rules.Decision{RuleID: "block", Action: api.Block, Via: []api.InterfaceRef{{Name: "en0"}}, Fallback: true}
		result, err := r.DialDomain(context.Background(), d, api.UDP, netip.MustParseAddrPort("198.18.0.7:443"), domain)
		if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.Problem != "" {
			t.Fatal(result, err)
		}
	}
}

func TestFakeIPOrderedInterfacesUseTheirOwnDNS(t *testing.T) {
	for _, failure := range []string{"dns", "fake answer", "dial"} {
		t.Run(failure, func(t *testing.T) {
			var resolved, dialed []string
			r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, network, target string) (net.Conn, error) {
				dialed = append(dialed, name)
				if target != "192.0.2.80:443" || network != "udp4" {
					t.Error("dialed a fake answer", name, network, target)
				}
				if name == "en0" {
					return nil, errors.New("interface cannot reach destination")
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(_ context.Context, iface api.Interface, domain string) ([]netip.Addr, error) {
				resolved = append(resolved, iface.Name)
				if domain != "example.com" || len(iface.DNS) != 1 || iface.Name == "en2" && iface.DNS[0] != "192.0.2.54" {
					t.Error("did not select the fallback interface's DNS")
				}
				if iface.Name == "en0" {
					switch failure {
					case "dns":
						return nil, errors.New("interface DNS unavailable")
					case "fake answer":
						return []netip.Addr{netip.MustParseAddr("198.18.0.99")}, nil
					}
				}
				return []netip.Addr{netip.MustParseAddr("192.0.2.80")}, nil
			}
			d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}, {Name: "en2"}}}
			result, err := r.DialDomain(context.Background(), d, api.UDP, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
			wantDials := []string{"en2"}
			if failure == "dial" {
				wantDials = []string{"en0", "en2"}
			}
			if err != nil || result.Outcome != api.Fallback || result.Interface != "en2" || !slices.Equal(resolved, []string{"en0", "en2"}) || !slices.Equal(dialed, wantDials) {
				t.Fatal(result, err, resolved, dialed)
			}
		})
	}
}

func TestFakeIPRejectsSyntheticDNSAnswersAndPreservesSystemFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		dials := 0
		remote := netip.MustParseAddrPort("198.18.0.7:443")
		r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, network, target string) (net.Conn, error) {
			dials++
			if !fallback || name != "utun3" || network != "tcp4" || target != remote.String() {
				t.Error("synthetic DNS answer escaped through a physical interface", name, target)
			}
			return pipeConn(t), nil
		})
		r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.18.0.8"), netip.MustParseAddr("::ffff:198.19.0.9"), netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("127.0.0.1")}, nil
		}
		d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}, Fallback: fallback}
		result, err := r.DialDomain(context.Background(), d, api.TCP, remote, "example.com")
		if fallback {
			if err != nil || result.Outcome != api.System || dials != 1 {
				t.Fatal(result, err, dials)
			}
		} else if err == nil || !strings.Contains(err.Error(), "no non-fake unicast address") || result.Outcome != api.Unreachable || dials != 0 {
			t.Fatal(result, err, dials)
		}
	}
}

func TestFakeIPVPNRouteKeepsOriginalDestination(t *testing.T) {
	for _, iface := range []api.Interface{
		{Name: "tunnel0", Kind: api.KindVPN, Up: true},
		{Name: "utun3", Kind: api.KindOther, Up: true},
		{Name: "ppp0", Up: true},
		{Name: "ipsec0", Up: true},
	} {
		t.Run(iface.Name, func(t *testing.T) {
			remote := netip.MustParseAddrPort("198.18.0.7:443")
			r := New(fakeRoutes(), []api.Interface{iface}, func(_ context.Context, name, network, target string) (net.Conn, error) {
				if name != iface.Name || network != "tcp4" || target != remote.String() {
					t.Error("changed VPN destination", name, network, target)
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				t.Fatal("resolved through a VPN instead of using its fake-IP map")
				return nil, nil
			}
			d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: iface.Name}}}
			result, err := r.DialDomain(context.Background(), d, api.TCP, remote, "example.com")
			if err != nil || result.Outcome != api.Routed {
				t.Fatal(result, err)
			}
		})
	}
}

func TestOrdinaryAndPrivateAddressesAreNotAssumedFake(t *testing.T) {
	for _, raw := range []string{"192.0.2.80:443", "100.64.0.80:443", "10.0.0.80:443", "[fd00::80]:443"} {
		t.Run(raw, func(t *testing.T) {
			remote := netip.MustParseAddrPort(raw)
			routes := append(fakeRoutes(), capture.Route{Prefix: netip.PrefixFrom(remote.Addr(), remote.Addr().BitLen()), Interface: "utun4"})
			r := New(routes, fakeIfaces(), func(_ context.Context, name, network, target string) (net.Conn, error) {
				if name != "en0" || target != remote.String() {
					t.Error("changed ordinary target because of a tunnel route", name, target)
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				t.Fatal("treated a normal private/VPN destination as fake")
				return nil, nil
			}
			d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}}
			result, err := r.DialDomain(context.Background(), d, api.TCP, remote, "example.com")
			if err != nil || result.Outcome != api.Routed || result.Problem != "" {
				t.Fatal(result, err)
			}
		})
	}
}

func TestFakeIPFiltersMixedAnswersAndSharesInterfaceBudget(t *testing.T) {
	var targets []string
	r := New(fakeRoutes(), fakeIfaces(), func(ctx context.Context, name, network, target string) (net.Conn, error) {
		requireAttemptDeadline(t, ctx)
		targets = append(targets, target)
		if len(targets) == 1 {
			if deadline, _ := ctx.Deadline(); time.Until(deadline) > 1600*time.Millisecond {
				t.Error("first answer consumed the whole interface budget")
			}
			return nil, errors.New("first answer unreachable")
		}
		return pipeConn(t), nil
	})
	r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.18.0.8"), netip.MustParseAddr("192.0.2.80"), netip.MustParseAddr("::ffff:192.0.2.80"), netip.MustParseAddr("2001:db8::80")}, nil
	}
	d := rules.Decision{RuleID: "route", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}}
	result, err := r.DialDomain(context.Background(), d, api.TCP, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
	if err != nil || result.Outcome != api.Routed || !slices.Equal(targets, []string{"192.0.2.80:443", "[2001:db8::80]:443"}) {
		t.Fatal(result, err, targets)
	}
}

func TestFakeIPDNSUsesOnlySelectedInterfaceServers(t *testing.T) {
	iface := api.Interface{Name: "en2", DNS: []string{
		"192.0.2.53", "::ffff:192.0.2.53", "2001:db8::53", "fe80::53", "fe80::54%en9",
		"198.18.0.53", "::ffff:198.19.0.53", "127.0.0.1", "::1", "0.0.0.0", "::", "224.0.0.53", "invalid",
	}}
	if got := physicalDNS(iface); !slices.Equal(got, []string{"192.0.2.53", "2001:db8::53", "fe80::53%en2"}) {
		t.Fatal("accepted fake, local, or another interface's DNS", got)
	}
	// The missing interface name is deliberate: this must fail before looking
	// it up or opening any socket, and must never borrow the system resolver.
	for _, servers := range [][]string{nil, {"198.18.0.53", "127.0.0.1"}} {
		_, err := resolveDomain(context.Background(), api.Interface{Name: "missing-test-interface", DNS: servers}, "example.com")
		if err == nil || !strings.Contains(err.Error(), "no usable physical DNS servers") {
			t.Fatal("empty physical DNS list did not fail closed", err)
		}
	}
}
