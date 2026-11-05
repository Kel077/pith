package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/netif"
	"github.com/Kel077/pith/internal/rules"
)

func TestAddressAndDNSRotationKeepEstablishedConnections(t *testing.T) {
	e, d := startTopology(t)
	g := e.run.guard
	client, peer := net.Pipe()
	defer peer.Close()
	r := egress.New(nil, []api.Interface{{Name: "en0", Up: true}}, func(context.Context, string, string, string) (net.Conn, error) { return client, nil })
	r.Guard = g
	result, err := r.Dial(context.Background(), rules.Decision{}, api.TCP, netip.MustParseAddrPort("203.0.113.9:443"))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Conn.Close()
	for _, prefix := range []string{"2001:db8:10::1/64", "2001:db8:20::2/64", "198.51.100.8/24"} {
		p := netip.MustParsePrefix(prefix)
		d.mu.Lock()
		d.assigned = []netip.Prefix{p}
		d.routes = slices.DeleteFunc(d.routes, func(r capture.Route) bool { return r.Interface == "en0" && r.Prefix.Bits() > 0 })
		d.routes = append(d.routes, capture.Route{Prefix: p.Masked(), Interface: "en0", Index: 1}, capture.Route{Prefix: netip.PrefixFrom(p.Addr(), p.Addr().BitLen()), Interface: "en0", Index: 1, Local: true})
		d.mu.Unlock()
		if err := g.Interfaces([]api.Interface{{Name: "en0", Up: true, IPv6: []string{p.Addr().String()}, DNS: []string{"192.0.2.53"}}}); err != nil {
			t.Fatal(err)
		}
		if err := g.Validate(true); err != nil {
			t.Fatal("address rotation revoked capture", err)
		}
		peer.SetDeadline(time.Now().Add(time.Second))
		result.Conn.SetDeadline(time.Now().Add(time.Second))
		written := make(chan error, 1)
		go func() { _, err := result.Conn.Write([]byte{7}); written <- err }()
		b := make([]byte, 1)
		if _, err := peer.Read(b); err != nil || b[0] != 7 {
			t.Fatal(b, err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuleFallbackAndReindexUseCurrentBinding(t *testing.T) {
	e, d := startTopology(t)
	d.mu.Lock()
	d.indices = map[string]int{"en5": 5}
	d.mu.Unlock()
	current := []api.Interface{{Name: "en5", HardwareAddr: "02:00:00:00:00:05", Up: true}, {Name: "en0", Up: true}}
	var names []string
	r := egress.New(nil, current, func(ctx context.Context, name, _, _ string) (net.Conn, error) {
		names = append(names, name)
		index, _ := d.Index(name)
		if err := netif.ValidateBinding(ctx, index); err != nil {
			return nil, err
		}
		c, p := net.Pipe()
		p.Close()
		return c, nil
	})
	r.Guard = e.run.guard
	r.Current = func([]api.Interface) ([]api.Interface, error) { return current, nil }
	decision := rules.Decision{RuleID: "ordered", Via: []api.InterfaceRef{{Name: "en5", HardwareAddr: current[0].HardwareAddr}, {Name: "en0"}}}
	dial := func(want string, outcome api.Outcome) {
		t.Helper()
		got, err := r.Dial(context.Background(), decision, api.TCP, netip.MustParseAddrPort("203.0.113.9:443"))
		if err != nil {
			t.Fatal(err)
		}
		got.Conn.Close()
		if got.Interface != want || got.Outcome != outcome {
			t.Fatal(got)
		}
	}
	dial("en5", api.Routed)
	old, _ := e.run.guard.Binding("en5")
	current[0].Up = false
	dial("en0", api.Fallback)
	d.mu.Lock()
	d.indices["en5"] = 6
	d.mu.Unlock()
	current[0].Up = true
	dial("en5", api.Routed)
	if old.Check() == nil {
		t.Fatal("old binding adopted new index")
	}
	// Same-name replacement with a different MAC must not inherit the rule.
	current[0].HardwareAddr = "02:00:00:00:00:06"
	dial("en0", api.Fallback)
	if !slices.Equal(names, []string{"en5", "en0", "en5", "en0"}) || e.run.guard.Err() != nil {
		t.Fatal(names, e.run.guard.Err())
	}
}

func TestLiveSystemDefaultWithdrawalAndRebuild(t *testing.T) {
	e, d := startTopology(t)
	g := e.run.guard
	dest := netip.MustParseAddr("203.0.113.9")
	first, err := g.System(dest)
	if err != nil || first.Name != "en0" {
		t.Fatal(first, err)
	}
	setDefault := func(name string, index int) {
		d.mu.Lock()
		d.routes = slices.DeleteFunc(d.routes, func(r capture.Route) bool { return !r.Scoped && r.Prefix.Bits() == 0 })
		if name != "" {
			d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: name, Index: index})
		}
		d.mu.Unlock()
	}
	setDefault("utun50", 50)
	vpn, err := g.System(dest)
	if err != nil || vpn.Name != "utun50" || first.Check() == nil {
		t.Fatal(vpn, err)
	}
	setDefault("en0", 1)
	if _, err := g.System(dest); err == nil {
		t.Fatal("VPN withdrawal immediately fell back to physical")
	}
	if _, err := g.Binding("en0"); err != nil {
		t.Fatal("explicit physical rule became unavailable", err)
	}
	if g.Admit(netip.MustParseAddr("192.0.2.8")) == nil || g.Err() != nil {
		t.Fatal("VPN ingress must drop without stopping capture")
	}
	setDefault("utun51", 51)
	newVPN, err := g.System(dest)
	if err != nil || newVPN.Index != 51 {
		t.Fatal(newVPN, err)
	}
	if vpn.Check() == nil {
		t.Fatal("old system generation remained valid")
	}
	// Equal-specificity alternatives cannot be selected by slice order.
	d.mu.Lock()
	d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0", Index: 1})
	d.mu.Unlock()
	if _, err := g.System(dest); !errors.Is(err, capture.ErrEgressChanged) {
		t.Fatal("ambiguous system route", err)
	}
	if g.Err() != nil {
		t.Fatal(g.Err())
	}
}

func TestVPNRebuildOnlyInvalidatesThatInterfaceLifetime(t *testing.T) {
	e, d := startTopology(t)
	g := e.run.guard
	physical, _ := g.Binding("en0")
	vpn, _ := g.Binding("utun50")
	if err := g.Observe(capture.Change{Index: 50, Interface: true, Up: false}); err != nil {
		t.Fatal(err)
	}
	if vpn.Check() == nil || physical.Check() != nil {
		t.Fatal("incorrect binding invalidation")
	}
	d.mu.Lock()
	d.routes = slices.DeleteFunc(d.routes, func(r capture.Route) bool { return r.Index == 50 })
	d.mu.Unlock()
	if err := g.Validate(true); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "utun51", Index: 51})
	d.mu.Unlock()
	if err := g.Validate(true); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Binding("utun51"); err != nil {
		t.Fatal(err)
	}
	if g.Admit(netip.MustParseAddr("192.0.2.8")) == nil {
		t.Fatal("rebuilt VPN prefix leaked")
	}
}
