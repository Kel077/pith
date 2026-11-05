package capture

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestTunnelAddressCandidatesAvoidProxyAndTailnetPools(t *testing.T) {
	// These are protocol/product address pools, not a real network's addresses.
	reserved := []netip.Prefix{
		prefix("198.18.0.0/15"), prefix("100.64.0.0/10"),
		prefix("192.168.0.0/16"), prefix("10.0.0.0/24"),
		prefix("fd7a:115c:a1e0::/48"), prefix("fc00::/18"),
	}
	for _, candidates := range [][]netip.Prefix{ipv4Candidates[:], ipv6Candidates[:]} {
		for _, pool := range candidates {
			if pool != pool.Masked() || !pool.Addr().IsPrivate() {
				t.Fatal("invalid private tunnel candidate", pool)
			}
			for _, p := range reserved {
				if pool.Overlaps(p) {
					t.Fatal("tunnel candidate overlaps a reserved pool", pool, p)
				}
			}
		}
	}
	// An existing proxy pool, tailnet route or split default must not prevent a
	// host-scoped test from selecting an unrelated, unused tunnel subnet.
	routes := []Route{
		{Prefix: prefix("198.18.0.0/15"), Interface: "utun7"},
		{Prefix: prefix("100.64.0.0/10"), Interface: "utun8"},
		{Prefix: prefix("0.0.0.0/1"), Interface: "utun7"},
		{Prefix: prefix("128.0.0.0/1"), Interface: "utun7"},
		{Prefix: prefix("::/0"), Interface: "utun8"},
	}
	got, err := SelectAddresses(routes, nil, true)
	if err != nil || got.IPv4 != ipv4Candidates[0] || got.IPv6 != ipv6Candidates[0] {
		t.Fatal(got, err)
	}
}

func TestTunnelAddressSelectionChecksEveryKindOfOverlap(t *testing.T) {
	v4 := ipv4Candidates[0]
	v6 := ipv6Candidates[0]
	for _, tc := range []struct {
		name     string
		routes   []Route
		assigned []netip.Prefix
		want4    int
		want6    int
	}{
		{name: "specific route", routes: []Route{{Prefix: v4}}, want4: 1},
		{name: "broader route", routes: []Route{{Prefix: netip.PrefixFrom(v4.Addr(), 16).Masked()}}, want4: 1},
		{name: "scoped peer host", routes: []Route{{Prefix: netip.PrefixFrom(v4.Addr().Next().Next(), 32), Scoped: true}}, want4: 1},
		{name: "reject", routes: []Route{{Prefix: v4, Reject: true}}, want4: 1},
		{name: "local", routes: []Route{{Prefix: netip.PrefixFrom(v4.Addr().Next(), 32), Local: true}}, want4: 1},
		{name: "assigned host without route", assigned: []netip.Prefix{netip.PrefixFrom(v4.Addr().Next(), 32)}, want4: 1},
		{name: "assigned network without route", assigned: []netip.Prefix{netip.PrefixFrom(v4.Addr().Next(), 24)}, want4: 1},
		{name: "ipv6 route", routes: []Route{{Prefix: v6}}, want6: 1},
		{name: "ipv6 assigned", assigned: []netip.Prefix{netip.PrefixFrom(v6.Addr().Next(), 64)}, want6: 1},
		{name: "independent families", routes: []Route{{Prefix: v4}}, assigned: []netip.Prefix{v6}, want4: 1, want6: 1},
		{name: "skip multiple", routes: []Route{{Prefix: v4}}, assigned: []netip.Prefix{ipv4Candidates[1]}, want4: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectAddresses(tc.routes, tc.assigned, true)
			if err != nil || got.IPv4 != ipv4Candidates[tc.want4] || got.IPv6 != ipv6Candidates[tc.want6] {
				t.Fatal(got, err)
			}
		})
	}
}

func TestAddressExhaustionAndReadFailurePrecedeTunnelCreation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assigned []netip.Prefix
		readErr  error
		ipv6     bool
	}{
		{name: "IPv4 exhausted", assigned: ipv4Candidates[:]},
		{name: "IPv6 exhausted", assigned: ipv6Candidates[:], ipv6: true},
		{name: "unknown interface addresses", readErr: errors.New("address lookup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDriver{routes: baseRoutes(), assigned: tc.assigned, prefixError: tc.readErr}
			opts := fakeOptions(t, d)
			opts.IPv6 = tc.ipv6
			opts.Scopes = []netip.Prefix{prefix("192.0.2.10/32")}
			_, err := Prepare(context.Background(), opts)
			if err == nil || tc.readErr == nil && api.Code(err) != api.ErrRouteConflict {
				t.Fatal("failed to reject occupied or unknown tunnel subnet", err)
			}
			if d.device != nil || len(d.operations) != 0 {
				t.Fatal("created a tunnel before establishing address availability")
			}
		})
	}
	got, err := SelectAddresses(nil, ipv6Candidates[:], false)
	if err != nil || got.IPv6.IsValid() {
		t.Fatal("IPv4-only capture claimed an IPv6 address", got, err)
	}
}

func TestSelectedAddressesAreConfiguredAndJournaled(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes(), assigned: []netip.Prefix{ipv4Candidates[0]}}
	opts := fakeOptions(t, d)
	opts.IPv6 = true
	// Capturing a candidate destination must not claim that destination locally.
	opts.Scopes = []netip.Prefix{ipv4Candidates[1]}
	c, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	want := TunnelAddresses{IPv4: ipv4Candidates[2], IPv6: ipv6Candidates[0]}
	state, err := readState(opts.StatePath)
	if err != nil || c.Plan.Addresses != want || d.configured != want || state.Plan.Addresses != want {
		t.Fatal("configured addresses differ from the reviewed journal", err)
	}
}
