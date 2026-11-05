package capture

import (
	"context"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/captureprefix"
)

func gatewayDNSRoutes() []Route {
	return []Route{
		{Prefix: prefix("0.0.0.0/0"), Gateway: addr("192.0.2.1"), Interface: "example0", Index: 1},
		{Prefix: prefix("192.0.2.0/24"), Interface: "example0", Index: 1},
		// A configured gateway host route (UCS), plus its scoped copy (UCSI).
		// These are forwarding policy, not WASCLONED or LLINFO cache entries.
		{Prefix: prefix("192.0.2.1/32"), Interface: "example0", Index: 1},
		{Prefix: prefix("192.0.2.1/32"), Interface: "example1", Index: 2, Scoped: true},
	}
}

func TestDNSCapturePreservesConfiguredGatewayHostRoutes(t *testing.T) {
	routes := gatewayDNSRoutes()
	before := slices.Clone(routes)
	p, err := Build(routes, []netip.Addr{addr("192.0.2.1"), addr("192.0.2.53"), addr("192.0.2.1")}, false, nil)
	if err != nil {
		t.Fatal("gateway DNS prevented full capture", err)
	}
	want := append(captureprefix.Full(false), prefix("192.0.2.53/32"))
	if !slices.Equal(p.Prefixes, want) || !slices.Equal(routes, before) || !slices.Equal(p.Before, before) {
		t.Fatal("DNS exclusion changed required capture or existing routing", p)
	}
	if !slices.Equal(p.Warnings, []api.Warning{{Code: api.WarnDNSUnobserved, Detail: "192.0.2.1"}}) {
		t.Fatal("missing or duplicated DNS warning", p.Warnings)
	}
	if len(p.SkippedDNS) != 1 || p.SkippedDNS[0].Address != addr("192.0.2.1") || !slices.Equal(p.SkippedDNS[0].Routes, before[2:]) || !strings.Contains(p.SkippedDNS[0].Reason, "TLS SNI, HTTP Host or QUIC SNI") {
		t.Fatal("DNS exclusion lost its reason or existing route evidence", p.SkippedDNS)
	}
	if got := SystemInterface(p.Before, addr("192.0.2.1")); got != "example0" {
		t.Fatal("gateway system path changed", got)
	}
}

func TestDNSExclusionDoesNotRelaxRequiredCaptureConflicts(t *testing.T) {
	for _, owned := range captureprefix.Full(true) {
		t.Run(owned.String(), func(t *testing.T) {
			routes := append(gatewayDNSRoutes(), Route{Prefix: owned, Interface: "utun70", Index: 70})
			if _, err := Build(routes, []netip.Addr{addr("192.0.2.1")}, true, nil); api.Code(err) != api.ErrRouteConflict {
				t.Fatal("DNS exception weakened split-route conflict", err)
			}
		})
	}
	if _, err := Build(gatewayDNSRoutes(), []netip.Addr{addr("192.0.2.1")}, false, []netip.Prefix{prefix("192.0.2.1/32")}); api.Code(err) != api.ErrRouteConflict {
		t.Fatal("an explicit scoped host conflict was silently skipped", err)
	}
}

func TestDNSExclusionPreservesEveryConfiguredHostRouteKind(t *testing.T) {
	for _, existing := range []Route{
		{Prefix: prefix("192.0.2.1/32"), Interface: "example0", Index: 1, Scoped: true},
		{Prefix: prefix("192.0.2.1/32"), Interface: "example0", Index: 1, Local: true},
		{Prefix: prefix("192.0.2.1/32"), Interface: "example0", Index: 1, Reject: true},
		{Prefix: prefix("192.0.2.1/32"), Interface: "example0", Index: 1, Gateway: addr("192.0.2.2")},
		{Prefix: prefix("2001:db8::1/128"), Interface: "example0", Index: 1},
	} {
		routes := append(gatewayDNSRoutes()[:2], existing)
		p, err := Build(routes, []netip.Addr{existing.Prefix.Addr()}, true, nil)
		if err != nil || slices.Contains(p.Prefixes, existing.Prefix) || len(p.SkippedDNS) != 1 || !slices.Equal(p.SkippedDNS[0].Routes, []Route{existing}) {
			t.Fatal("configured DNS host route was captured or lost", existing, p, err)
		}
	}
}

func TestDNSGatewayIsNeverAddedOrDeletedDuringCapture(t *testing.T) {
	d := &fakeDriver{routes: gatewayDNSRoutes()}
	before := slices.Clone(d.routes)
	opts := fakeOptions(t, d)
	opts.DNS = []netip.Addr{addr("192.0.2.1")}
	c, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	state, err := readState(opts.StatePath)
	if err != nil || !reflect.DeepEqual(state.Plan.SkippedDNS, c.Plan.SkippedDNS) || len(state.Plan.SkippedDNS) != 1 {
		t.Fatal("journal omitted DNS exclusion evidence", err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	want := captureOperations(captureprefix.Full(false))
	if !slices.Equal(d.operations, want) || !slices.Equal(d.routes, before) {
		t.Fatal("capture touched an existing gateway route", d.operations, d.routes)
	}
}
