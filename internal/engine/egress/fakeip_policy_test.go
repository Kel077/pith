package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
)

func fakePolicyScene(block api.Rule) api.Scene {
	return api.Scene{ID: "test", Rules: []api.Rule{
		{ID: "route", Enabled: true, Action: api.Route, Protocol: api.AnyProtocol, Targets: []string{"example.com"}, Via: []api.InterfaceRef{{Name: "en0"}, {Name: "en2"}}, Fallback: true},
		block,
	}}
}

func TestFakeIPResolvedBlockStopsInterfaceAndSystemFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proto  api.Protocol
		target string
		real   string
	}{
		{"tcp exact address", api.TCP, "192.0.2.80", "192.0.2.80"},
		{"udp address range", api.UDP, "192.0.2.0/24", "192.0.2.80"},
		{"tcp IPv6 range", api.TCP, "2001:db8::/32", "2001:db8::80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(fakeRoutes(), fakeIfaces(), func(context.Context, string, string, string) (net.Conn, error) {
				t.Fatal("dialed a blocked resolved destination or fallback")
				return nil, nil
			})
			var resolved []string
			r.Resolve = func(_ context.Context, iface api.Interface, _ string) ([]netip.Addr, error) {
				resolved = append(resolved, iface.Name)
				return []netip.Addr{netip.MustParseAddr(tc.real)}, nil
			}
			scene := fakePolicyScene(api.Rule{ID: "real-block", Enabled: true, Action: api.Block, Protocol: tc.proto, Targets: []string{tc.target}})
			if err := r.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			result, err := r.DialFlow(context.Background(), r.Policy(), attribution.Attribution{}, tc.proto, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
			if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.RuleID != "real-block" || result.Conn != nil || !slices.Equal(resolved, []string{"en0"}) {
				t.Fatal("resolved block was bypassed", result, err, resolved)
			}
		})
	}
}

func TestFakeIPResolvedBlocksRespectAppAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner attribution.Attribution
		proto api.Protocol
		block bool
	}{
		{"same app", attribution.Attribution{App: api.AppRef{Path: "/Applications/Example.app"}}, api.TCP, true},
		{"nested helper", attribution.Attribution{Process: api.Process{Path: "/Applications/Example.app/Contents/XPCServices/Worker.xpc/Contents/MacOS/Worker"}}, api.TCP, true},
		{"different app", attribution.Attribution{App: api.AppRef{Path: "/Applications/Other.app"}}, api.TCP, false},
		{"different protocol", attribution.Attribution{App: api.AppRef{Path: "/Applications/Example.app"}}, api.UDP, false},
		{"delegated helper excluded", attribution.Attribution{Delegated: true, App: api.AppRef{Path: "/Applications/Other.app"}, Process: api.Process{Path: "/Applications/Example.app/Contents/MacOS/Worker"}}, api.TCP, false},
		{"delegated app included", attribution.Attribution{Delegated: true, App: api.AppRef{Path: "/Applications/Example.app"}, Process: api.Process{Path: "/Applications/Other.app/Contents/MacOS/Worker"}}, api.TCP, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dials := 0
			r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, _, target string) (net.Conn, error) {
				dials++
				if name != "en0" || target != "192.0.2.80:443" {
					t.Fatal("unexpected route", name, target)
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("192.0.2.80")}, nil
			}
			scene := fakePolicyScene(api.Rule{ID: "app-block", Enabled: true, Action: api.Block, Protocol: api.TCP, Targets: []string{"192.0.2.0/24"}, Apps: []api.AppRef{{Path: "/Applications/Example.app"}}})
			if err := r.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			result, err := r.DialFlow(context.Background(), r.Policy(), tc.owner, tc.proto, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
			if tc.block {
				if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.RuleID != "app-block" || dials != 0 {
					t.Fatal("lost application block", result, err, dials)
				}
			} else if err != nil || result.Outcome != api.Routed || result.RuleID != "route" || dials != 1 {
				t.Fatal("applied another app's or protocol's block", result, err, dials)
			}
		})
	}
}

func TestFakeIPResolvedBlockFiltersCandidatesWithoutReselectingRoute(t *testing.T) {
	for _, allowedSucceeds := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed succeeds", false: "allowed fails"}[allowedSucceeds], func(t *testing.T) {
			var targets, resolved []string
			r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, _, target string) (net.Conn, error) {
				targets = append(targets, target)
				if name != "en0" || target != "198.51.100.80:443" {
					t.Fatal("used blocked answer, changed route rule, or attempted fallback", name, target)
				}
				if !allowedSucceeds {
					return nil, errors.New("allowed destination unavailable")
				}
				return pipeConn(t), nil
			})
			r.Resolve = func(_ context.Context, iface api.Interface, _ string) ([]netip.Addr, error) {
				resolved = append(resolved, iface.Name)
				return []netip.Addr{netip.MustParseAddr("192.0.2.80"), netip.MustParseAddr("198.51.100.80")}, nil
			}
			scene := fakePolicyScene(api.Rule{ID: "real-block", Enabled: true, Action: api.Block, Protocol: api.AnyProtocol, Targets: []string{"192.0.2.0/24"}})
			scene.Rules = append([]api.Rule{{ID: "real-route", Enabled: true, Action: api.Route, Protocol: api.AnyProtocol, Targets: []string{"198.51.100.0/24"}, Via: []api.InterfaceRef{{Name: "en2"}}}}, scene.Rules...)
			if err := r.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			result, err := r.DialFlow(context.Background(), r.Policy(), attribution.Attribution{}, api.TCP, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
			if !slices.Equal(targets, []string{"198.51.100.80:443"}) || !slices.Equal(resolved, []string{"en0"}) {
				t.Fatal("candidate filtering or fallback escaped", targets, resolved)
			}
			if allowedSucceeds {
				if err != nil || result.Outcome != api.Routed || result.RuleID != "route" {
					t.Fatal("allowed answer lost the original route", result, err)
				}
			} else if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.RuleID != "real-block" {
				t.Fatal("known block escaped through fallback", result, err)
			}
		})
	}
}

func TestFakeIPResolvedBlockChecksAnswersBeyondDialBudget(t *testing.T) {
	addresses := make([]netip.Addr, 0, 33)
	for i := byte(1); i <= 32; i++ {
		addresses = append(addresses, netip.AddrFrom4([4]byte{198, 51, 100, i}))
	}
	addresses = append(addresses, netip.MustParseAddr("192.0.2.80"))
	dials := 0
	r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, _, target string) (net.Conn, error) {
		dials++
		if name != "en0" || netip.MustParseAddrPort(target).Addr() == addresses[32] {
			t.Fatal("bypassed a blocked answer beyond the dial budget", name, target)
		}
		return nil, errors.New("allowed destination unavailable")
	})
	r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) { return addresses, nil }
	if err := r.Reload(fakePolicyScene(api.Rule{ID: "real-block", Enabled: true, Action: api.Block, Protocol: api.AnyProtocol, Targets: []string{"192.0.2.80"}}), api.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	result, err := r.DialFlow(context.Background(), r.Policy(), attribution.Attribution{}, api.TCP, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
	if !errors.Is(err, ErrBlocked) || result.RuleID != "real-block" || result.Outcome != api.Blocked || dials != 32 {
		t.Fatal("truncated block evidence", result, err, dials)
	}
}

func TestFakeIPResolvedBlockUsesCapturedPolicyDuringDNS(t *testing.T) {
	for _, oldBlocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "block removed", false: "block added"}[oldBlocked], func(t *testing.T) {
			dials := 0
			r := New(fakeRoutes(), fakeIfaces(), func(_ context.Context, name, _, target string) (net.Conn, error) {
				dials++
				if name != "en0" || target != "192.0.2.80:443" {
					t.Fatal("hot reload changed the retained route", name, target)
				}
				return pipeConn(t), nil
			})
			scene := fakePolicyScene(api.Rule{ID: "real-block", Enabled: oldBlocked, Action: api.Block, Protocol: api.AnyProtocol, Targets: []string{"192.0.2.0/24"}})
			if err := r.Reload(scene, api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			captured := r.Policy()
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				scene.Rules[0].ID = "new-route"
				scene.Rules[0].Via = []api.InterfaceRef{{Name: "en2"}}
				scene.Rules[1].Enabled = !oldBlocked
				if err := r.Reload(scene, api.DefaultSettings()); err != nil {
					t.Fatal(err)
				}
				return []netip.Addr{netip.MustParseAddr("192.0.2.80")}, nil
			}
			result, err := r.DialFlow(context.Background(), captured, attribution.Attribution{}, api.TCP, netip.MustParseAddrPort("198.18.0.7:443"), "example.com")
			if captured == r.Policy() {
				t.Fatal("test did not replace the policy during DNS")
			}
			if oldBlocked {
				if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.RuleID != "real-block" || dials != 0 {
					t.Fatal("removing a block changed an existing flow", result, err, dials)
				}
			} else if err != nil || result.Outcome != api.Routed || result.RuleID != "route" || dials != 1 {
				t.Fatal("adding a block changed an existing flow", result, err, dials)
			}
		})
	}
}

func TestFakeIPFlowBlockWinsBeforeResolution(t *testing.T) {
	for _, tc := range []struct{ name, domain, target string }{
		{"observed domain", "example.com", "example.com"},
		{"unknown domain", "", "198.18.0.0/15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(fakeRoutes(), fakeIfaces(), func(context.Context, string, string, string) (net.Conn, error) {
				t.Fatal("dialed a flow blocked before resolution")
				return nil, nil
			})
			r.Resolve = func(context.Context, api.Interface, string) ([]netip.Addr, error) {
				t.Fatal("resolved a flow blocked before resolution")
				return nil, nil
			}
			if err := r.Reload(fakePolicyScene(api.Rule{ID: "initial-block", Enabled: true, Action: api.Block, Protocol: api.AnyProtocol, Targets: []string{tc.target}}), api.DefaultSettings()); err != nil {
				t.Fatal(err)
			}
			result, err := r.DialFlow(context.Background(), r.Policy(), attribution.Attribution{}, api.UDP, netip.MustParseAddrPort("198.18.0.7:443"), tc.domain)
			if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || result.RuleID != "initial-block" {
				t.Fatal(result, err)
			}
		})
	}
}
