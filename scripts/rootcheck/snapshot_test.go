package main

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

func absentPathStates() []pathState {
	var paths []pathState
	for _, path := range managedPaths() {
		paths = append(paths, pathState{Path: path})
	}
	return paths
}

func TestInterfaceComparisonToleratesIPv6AddressChurnForIPv4Scope(t *testing.T) {
	before := []api.Interface{{
		Name: "example0", Kind: api.KindEthernet, Up: true, HardwareAddr: "02:00:00:00:00:01",
		IPv4: []string{"192.0.2.1", "192.0.2.2"}, IPv6: []string{"2001:db8::1", "2001:db8::2"},
		Router: "192.0.2.254", Router6: "2001:db8::ffff", DNS: []string{"192.0.2.53", "2001:db8::53"}, Primary: true,
	}, {Name: "utun42", Kind: api.KindVPN, IPv6: []string{"2001:db8:1::1"}}}
	after := slices.Clone(before)
	after[0].IPv4 = []string{"192.0.2.2", "192.0.2.1"}
	after[0].IPv6 = []string{"2001:db8::3"}
	after[1].IPv6 = []string{"2001:db8:1::2", "2001:db8:1::3"}
	slices.Reverse(after)
	unchanged := slices.Clone(after[1].IPv4)
	if !sameInterfaces(before, after) {
		t.Fatal("interface/address ordering or IPv6 values/counts changed the IPv4 topology comparison")
	}
	if !reflect.DeepEqual(after[1].IPv4, unchanged) || len(after[0].IPv6) != 2 || len(after[1].IPv6) != 1 {
		t.Fatal("comparison mutated the complete archived interface observations")
	}
	if !sameInterfaces(nil, []api.Interface{}) {
		t.Fatal("empty interface representations should compare equally")
	}
	for _, test := range []struct {
		name   string
		change func(*api.Interface)
	}{
		{"name", func(v *api.Interface) { v.Name = "example2" }},
		{"kind", func(v *api.Interface) { v.Kind = api.KindVPN }},
		{"mac", func(v *api.Interface) { v.HardwareAddr = "02:00:00:00:00:02" }},
		{"down", func(v *api.Interface) { v.Up = false }},
		{"ipv4", func(v *api.Interface) { v.IPv4 = []string{"192.0.2.3"} }},
		{"ipv6-unavailable", func(v *api.Interface) { v.IPv6 = nil }},
		{"router", func(v *api.Interface) { v.Router = "192.0.2.253" }},
		{"router6", func(v *api.Interface) { v.Router6 = "2001:db8::fffe" }},
		{"dns-value", func(v *api.Interface) { v.DNS = []string{"192.0.2.54", "2001:db8::53"} }},
		{"dns-priority", func(v *api.Interface) { v.DNS = []string{"2001:db8::53", "192.0.2.53"} }},
		{"primary", func(v *api.Interface) { v.Primary = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := slices.Clone(before)
			test.change(&changed[0])
			if sameInterfaces(before, changed) {
				t.Fatal("accepted a changed interface configuration")
			}
		})
	}
	if sameInterfaces(before, append(slices.Clone(before), api.Interface{Name: "utun43"})) || sameInterfaces(before, before[:1]) {
		t.Fatal("IPv6 normalization concealed a changed interface inventory")
	}
}

func TestTunnelComparisonRetainsExactInventoryWithoutAddressNoise(t *testing.T) {
	before := []api.Interface{{Name: "example0"}, {Name: "utun42", Up: true, IPv6: []string{"2001:db8::1"}}, {Name: "utun43"}}
	after := []api.Interface{{Name: "utun43", Up: true}, {Name: "utun42", IPv6: []string{"2001:db8::2"}}, {Name: "example1"}}
	if !sameTunnels(before, after) {
		t.Fatal("non-inventory metadata made tunnel comparison unstable")
	}
	for _, changed := range [][]api.Interface{
		{{Name: "utun42"}},
		{{Name: "utun42"}, {Name: "utun43"}, {Name: "utun44"}},
		{{Name: "utun42"}, {Name: "utun44"}},
		{{Name: "utun42"}, {Name: "utun42"}, {Name: "utun43"}},
	} {
		if sameTunnels(before, changed) {
			t.Fatal("missed an extra, missing, renamed, or duplicated utun")
		}
	}
}

func TestAbsentSnapshotRequiresEveryManagedPath(t *testing.T) {
	base := snapshot{Paths: absentPathStates(), LaunchExit: 113}
	for _, path := range managedPaths() {
		for _, unreadable := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/present", true: "/unreadable"}[unreadable], func(t *testing.T) {
				shot := base
				shot.Paths = slices.Clone(base.Paths)
				for i := range shot.Paths {
					if shot.Paths[i].Path == path {
						if unreadable {
							shot.Paths[i].Error = "permission denied"
						} else {
							shot.Paths[i].Present = true
						}
					}
				}
				if shot.requireAbsent() == nil {
					t.Fatal("accepted a present or uninspected managed path")
				}
			})
		}
	}
	for _, paths := range [][]pathState{
		nil,
		base.Paths[:4],
		append(slices.Clone(base.Paths), pathState{Path: "unexpected-path"}),
		{base.Paths[0], base.Paths[0], base.Paths[2], base.Paths[3], base.Paths[4]},
		{{Path: "unexpected-path"}, base.Paths[1], base.Paths[2], base.Paths[3], base.Paths[4]},
	} {
		shot := base
		shot.Paths = paths
		if shot.requireAbsent() == nil {
			t.Fatal("accepted an incomplete, repeated, or unexpected path inventory")
		}
	}
	reordered := base
	reordered.Paths = slices.Clone(base.Paths)
	slices.Reverse(reordered.Paths)
	if err := reordered.requireAbsent(); err != nil {
		t.Fatal(err)
	}
}

func TestExpectOriginalUsesServiceAbsenceNotLaunchdDiagnosticText(t *testing.T) {
	s, _, ops := baselineFixture(t)
	s.Original.Paths = absentPathStates()
	s.Original.LaunchExit, s.Original.Launchd = 113, "old launchd diagnostic text"
	after := cloneSnapshot(s.Original)
	after.Launchd = "new diagnostic formatting and mutable details"
	ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
	if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err != nil {
		t.Fatalf("dynamic launchd text prevented absence verification: %v", err)
	}
	after.LaunchExit = 0
	if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err == nil {
		t.Fatal("accepted a still-registered launchd service")
	}
	ops.snapshot = func(context.Context) (snapshot, error) { return after, errors.New("launchctl lookup failed") }
	if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err == nil {
		t.Fatal("accepted a failed snapshot acquisition")
	}
}

func TestRebaselineAllowsAddressOnlyChurnButPreservesRouteAndTunnelChecks(t *testing.T) {
	for _, change := range []string{"ipv6-addresses-only", "ipv6-local-route", "capture-route", "route-owner", "extra-utun"} {
		t.Run(change, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			before := nextMode(s)
			before.Interfaces[0].IPv6 = []string{"2001:db8::1"}
			after := cloneSnapshot(before)
			after.Interfaces[0].IPv6 = []string{"2001:db8::2", "2001:db8::3"}
			switch change {
			case "ipv6-local-route":
				after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("2001:db8::2/128"), Interface: "lo0", Local: true})
			case "capture-route":
				after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "utun43", Index: 85})
			case "route-owner":
				after.Routes[0].Index++
			case "extra-utun":
				after.Interfaces = append(after.Interfaces, api.Interface{Name: "utun43"})
			}
			snapshots := 0
			ops.snapshot = func(context.Context) (snapshot, error) {
				snapshots++
				if snapshots == 1 {
					return before, nil
				}
				return after, nil
			}
			ops.probe = func(context.Context, *session, string) (tlsEvidence, error) { return tlsEvidence{StatusCode: 200}, nil }
			err := rebaselineWithOps(context.Background(), s, c, "tailnet", ops)
			if (err == nil) != (change == "ipv6-addresses-only") {
				t.Fatalf("rebaseline comparison result for %s: %v", change, err)
			}
			if change == "ipv6-addresses-only" && !reflect.DeepEqual(s.Original.Interfaces[0].IPv6, after.Interfaces[0].IPv6) {
				t.Fatal("accepted baseline discarded full IPv6 evidence")
			}
		})
	}
}
