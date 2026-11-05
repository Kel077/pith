package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/rules"
)

func TestOrderedFallbackAndSavedSystemRoute(t *testing.T) {
	var attempts []string
	ctx := context.Background()
	remote := netip.MustParseAddrPort("192.0.2.10:443")
	r := New([]capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun3"}}, []api.Interface{{Name: "bridge0", HardwareAddr: "shared", Up: true}, {Name: "en2", HardwareAddr: "shared", Up: true}, {Name: "en5", Up: true}}, func(ctx context.Context, name, network, target string) (net.Conn, error) {
		attempts = append(attempts, name)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
			t.Error("unbounded attempt")
		}
		if network != "tcp4" || target != remote.String() {
			t.Error(network, target)
		}
		if name == "en2" {
			return nil, errors.New("unreachable")
		}
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		return a, nil
	})
	d := rules.Decision{RuleID: "r", Action: api.Route, Via: []api.InterfaceRef{{Name: "en2", HardwareAddr: "shared"}, {Name: "en5"}}}
	result, err := r.Dial(ctx, d, api.TCP, remote)
	if err != nil || result.Outcome != api.Fallback || !slices.Equal(attempts, []string{"en2", "en5"}) {
		t.Fatal(result, attempts, err)
	}
	attempts = nil
	d.Via = d.Via[:1]
	d.Fallback = true
	result, err = r.Dial(ctx, d, api.TCP, remote)
	if err != nil || result.Outcome != api.System || !slices.Equal(attempts, []string{"en2", "utun3"}) {
		t.Fatal(result, attempts, err)
	}
	attempts = nil
	d.Action = api.Block
	result, err = r.Dial(ctx, d, api.TCP, remote)
	if !errors.Is(err, ErrBlocked) || result.Outcome != api.Blocked || len(attempts) != 0 {
		t.Fatal(result, attempts, err)
	}
	d.Action = api.Route
	d.Fallback = false
	d.Via = []api.InterfaceRef{{Name: "gone", HardwareAddr: "shared"}}
	result, err = r.Dial(ctx, d, api.TCP, remote)
	if err == nil || result.Outcome != api.Unreachable || len(attempts) != 0 {
		t.Fatal("resolved an ambiguous MAC", result, attempts, err)
	}
}

func TestPolicySnapshotAndDelegatedOwner(t *testing.T) {
	r := New(nil, nil, nil)
	settings := api.DefaultSettings()
	scene := api.Scene{ID: "scene", Rules: []api.Rule{{ID: "b", Enabled: true, Action: api.Block, Protocol: api.AnyProtocol, Apps: []api.AppRef{{Path: "/Applications/Helper.app"}}}}}
	if err := r.Reload(scene, settings); err != nil {
		t.Fatal(err)
	}
	old := r.Policy()
	a := attribution.Attribution{Delegated: true, App: api.AppRef{Path: "/Applications/Browser.app"}, Process: api.Process{Path: "/Applications/Helper.app/Contents/MacOS/Helper"}}
	remote := netip.MustParseAddrPort("192.0.2.1:443")
	if old.Match(a, api.TCP, remote, "").RuleID != "" {
		t.Fatal("attributed delegated socket to helper")
	}
	scene.Rules[0].Apps = nil
	if err := r.Reload(scene, settings); err != nil {
		t.Fatal(err)
	}
	if old.Match(a, api.TCP, remote, "").RuleID != "" || r.Policy().Match(a, api.TCP, remote, "").Action != api.Block {
		t.Fatal("hot reload mutated an existing flow")
	}
}
