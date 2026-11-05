package rules

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func routeRule(id string) api.Rule {
	return api.Rule{ID: id, Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{{Name: "en0"}}}
}

func TestPriorityAndReasons(t *testing.T) {
	first, second, block := routeRule("first"), routeRule("second"), routeRule("block")
	first.Apps = []api.AppRef{{Path: "/Applications/Example.app"}}
	first.Targets = []string{"*.example.com", "192.0.2.0/24"}
	second.Protocol = api.TCP
	block.Action, block.Via, block.Targets = api.Block, nil, []string{"192.0.2.7"}
	disabled := routeRule("disabled")
	disabled.Enabled = false
	s, err := New(api.Scene{Rules: []api.Rule{first, second, block, disabled}})
	if err != nil {
		t.Fatal(err)
	}
	flow := Flow{Process: api.Process{Path: "/Applications/Example.app/Contents/XPCServices/Child.app/Contents/MacOS/child"}, Domain: "WWW.Example.COM.", Address: netip.MustParseAddr("192.0.2.7"), Protocol: api.TCP}
	d, checks := s.Explain(flow)
	if d.RuleID != "block" {
		t.Fatalf("decision: %+v", d)
	}
	want := []api.RuleCheck{{RuleID: "first", Result: api.CheckShadowed}, {RuleID: "second", Result: api.CheckShadowed}, {RuleID: "block", Result: api.CheckMatched}, {RuleID: "disabled", Result: api.CheckDisabled}}
	if !reflect.DeepEqual(checks, want) {
		t.Fatalf("checks: %+v", checks)
	}
	flow.Address = netip.MustParseAddr("2001:db8::1")
	if got := s.Match(flow).RuleID; got != "first" {
		t.Fatal(got)
	}
	flow.Domain, flow.Protocol = "elsewhere.example", api.UDP
	_, checks = s.Explain(flow)
	if checks[0].Result != api.CheckTarget || checks[1].Result != api.CheckProtocol {
		t.Fatal(checks)
	}
	flow.Process.Path = "/Applications/Example.app.evil/Contents/MacOS/child"
	_, checks = s.Explain(flow)
	if checks[0].Result != api.CheckApp {
		t.Fatal(checks)
	}
}

func TestAppIdentity(t *testing.T) {
	for _, tt := range []struct {
		ref, app api.AppRef
		process  string
		want     bool
	}{
		{api.AppRef{Path: "/Applications/Example.app"}, api.AppRef{}, "/Applications/Example.app/Contents/Helpers/x", true},
		{api.AppRef{Path: "/Applications/Example.app"}, api.AppRef{}, "/Applications/Example.app2/x", false},
		{api.AppRef{Path: "/usr/bin/tool"}, api.AppRef{}, "/usr/bin/tool/x", false},
		{api.AppRef{Path: "/usr/bin/tool"}, api.AppRef{Path: "/usr/bin/tool"}, "", true},
		{api.AppRef{Path: "/old/Example.app", BundleID: "com.example.app"}, api.AppRef{Path: "/new/Example.app", BundleID: "com.example.app"}, "", true},
		{api.AppRef{}, api.AppRef{}, "", false},
	} {
		if got := MatchApp(tt.ref, tt.app, tt.process); got != tt.want {
			t.Errorf("%+v: %v", tt, got)
		}
	}
	if got := OuterBundle("/Applications/Example.app/Contents/Child.app/x"); got != "/Applications/Example.app" {
		t.Fatal(got)
	}
	if got := OuterBundle("/usr/bin/tool"); got != "" {
		t.Fatal(got)
	}
}

func TestImmutableAndAddressTargets(t *testing.T) {
	r := routeRule("r")
	r.Targets = []string{"2001:db8::/32", "192.0.2.0/24"}
	s, err := New(api.Scene{Rules: []api.Rule{r}})
	if err != nil {
		t.Fatal(err)
	}
	r.Targets[0], r.Via[0].Name = "other.example", "wrong"
	for _, address := range []string{"2001:db8::2", "::ffff:192.0.2.5"} {
		d := s.Match(Flow{Address: netip.MustParseAddr(address), Protocol: api.UDP})
		if d.RuleID != "r" || d.Via[0].Name != "en0" {
			t.Fatal(d)
		}
		d.Via[0].Name = "mutated"
	}
	if s.Match(Flow{Address: netip.MustParseAddr("198.51.100.1")}).RuleID != "" {
		t.Fatal("unexpected match")
	}
	if _, err := New(api.Scene{Rules: []api.Rule{routeRule("same"), routeRule("same")}}); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestInterfaceFallback(t *testing.T) {
	interfaces := []api.Interface{{Name: "en0", HardwareAddr: "02:00:00:00:00:01", Up: true, Primary: true}, {Name: "en2", HardwareAddr: "02:00:00:00:00:02", Up: true}, {Name: "en3"}}
	d := Decision{RuleID: "r", Action: api.Route, Via: []api.InterfaceRef{{Name: "en3"}, {Name: "old", HardwareAddr: "02:00:00:00:00:02"}}}
	if outcome, iface := d.Predict(interfaces); outcome != api.Fallback || iface != "en2" {
		t.Fatal(outcome, iface)
	}
	d.Via = []api.InterfaceRef{{Name: "en0", HardwareAddr: "02:00:00:00:00:ff"}}
	if outcome, _ := d.Predict(interfaces); outcome != api.Unreachable {
		t.Fatal(outcome)
	}
	d.Fallback = true
	if outcome, iface := d.Predict(interfaces); outcome != api.System || iface != "en0" {
		t.Fatal(outcome, iface)
	}
	d.Action = api.Block
	if outcome, iface := d.Predict(interfaces); outcome != api.Blocked || iface != "" {
		t.Fatal(outcome, iface)
	}
	shared := []api.Interface{{Name: "bridge0", HardwareAddr: "02:00:00:00:00:01", Up: true}, {Name: "en0", HardwareAddr: "02:00:00:00:00:01", Up: true}}
	d = Decision{RuleID: "shared", Action: api.Route, Via: []api.InterfaceRef{{Name: "en0", HardwareAddr: "02:00:00:00:00:01"}}}
	if outcome, iface := d.Predict(shared); outcome != api.Routed || iface != "en0" {
		t.Fatal("shared MAC chose a different port", outcome, iface)
	}
	d.Via[0].Name = "missing"
	if outcome, _ := d.Predict(shared); outcome != api.Unreachable {
		t.Fatal("ambiguous shared MAC was guessed", outcome)
	}
}
