package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

func reuseFixture(t *testing.T, name string, index int) (*session, *baselineService, rootcheckOps, snapshot) {
	t.Helper()
	s, c, ops, _ := externalFixture(t)
	id := s.Ownership.Captures[0]
	id.TunnelSubnets = []string{"198.51.100.252/30", "2001:db8:ffff::/126"}
	s.Ownership.Captures[0] = id
	id.EndedAt = s.Original.At.Add(time.Minute)
	c.status.LastCapture = &id
	s.Original.Interfaces[2].IPv4 = []string{"203.0.113.7"}
	s.Original.Interfaces[2].IPv6 = []string{"2001:db8:50::7"}
	s.Original.Interfaces[2].Label = "utun50"
	s.Original.Routes = append(s.Original.Routes,
		capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun50", Index: 50, Scoped: true},
		capture.Route{Prefix: netip.MustParsePrefix("::/0"), Interface: "utun50", Index: 50, Scoped: true},
		capture.Route{Prefix: netip.MustParsePrefix("2001:db8:50::/48"), Interface: "utun50", Index: 50})
	s.Original.InterfaceIndices["lo0"] = 1 // Complete inventory includes hidden UI links.
	s.Original.InterfaceAddresses = map[string][]netip.Prefix{
		"lo0":      {netip.MustParsePrefix("127.0.0.1/8"), netip.MustParsePrefix("::1/128")},
		"example0": {netip.MustParsePrefix("192.0.2.1/24")},
		"example1": {netip.MustParsePrefix("198.51.100.1/24")},
		"utun50":   {netip.MustParsePrefix("203.0.113.7/32"), netip.MustParsePrefix("2001:db8:50::7/128"), netip.MustParsePrefix("fe80::50/64")},
	}
	after := cloneSnapshot(s.Original)
	after.At = after.At.Add(2 * time.Minute)
	after.Interfaces[2].Name, after.Interfaces[2].Label = name, name
	for i := range after.Routes {
		if after.Routes[i].Interface == "utun50" {
			after.Routes[i].Interface = name
			after.Routes[i].Index = index
		}
	}
	delete(after.InterfaceIndices, "utun50")
	after.InterfaceIndices[name] = index
	after.InterfaceAddresses[name] = after.InterfaceAddresses["utun50"]
	delete(after.InterfaceAddresses, "utun50")
	after.InterfaceAddresses[name][2] = netip.MustParsePrefix("fe80::90/64")
	ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
	return s, c, ops, after
}

func TestReuseRequiresUniqueMigrationAndExplicitAcceptance(t *testing.T) {
	for _, key := range []interfaceKey{{"utun90", 90}, {"utun90", 91}, {"utun51", 90}} {
		t.Run(key.Name, func(t *testing.T) {
			s, c, ops, after := reuseFixture(t, key.Name, key.Index)
			ctx := context.Background()
			if err := stopWithOps(ctx, s, c, ops); !errors.Is(err, errExternalChange) {
				t.Fatal(err)
			}
			if s.Ownership.Captures[0].EndedAt.IsZero() {
				t.Fatal("lost confirmed closure receipt")
			}
			if s.Cleanup == nil || s.Cleanup.Kind != externalProof {
				t.Fatal("missing evidence")
			}
			data, err := os.ReadFile(filepath.Join(s.Dir, s.Cleanup.File))
			if err != nil {
				t.Fatal(err)
			}
			var record cleanupRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Passed || record.Restored || len(record.External.Migrations) != 1 || record.External.Migrations[0].To != key {
				t.Fatal("missing conservative migration report", record)
			}
			if err := rebaselineWithOps(ctx, s, c, "reused", ops); !errors.Is(err, errExternalChange) {
				t.Fatal("implicit reuse acceptance", err)
			}
			probes := 0
			ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
				probes++
				return tlsEvidence{StatusCode: 200}, nil
			}
			if err := rebaselineWithOps(ctx, s, c, "reused", ops, true); err != nil {
				t.Fatal(err)
			}
			if probes != 3 || s.SmokePassed || s.Cleanup != nil || !sameCleanupBaseline(s.Original, after) {
				t.Fatal("unsafe rebaseline")
			}
			// The accepted foreign identity remains foreign under the next baseline.
			changed := cloneSnapshot(after)
			changed.Routes = append(changed.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.99/32"), Interface: "example0", Index: 42})
			if _, err := s.externalDifferences(changed, c.status); err != nil {
				t.Fatal("accepted baseline was tainted forever", err)
			}
		})
	}
}

func TestReuseRejectsIncompleteOrContradictoryEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*session, *baselineService, *snapshot)
	}{
		{"no-end", func(_ *session, c *baselineService, _ *snapshot) { c.status.LastCapture.EndedAt = time.Time{} }},
		{"end-after-snapshot", func(_ *session, c *baselineService, a *snapshot) {
			c.status.LastCapture.EndedAt = a.At.Add(time.Second)
		}},
		{"restarted-helper", func(_ *session, c *baselineService, _ *snapshot) { c.status.LastCapture = nil }},
		{"missing-subnets", func(s *session, c *baselineService, _ *snapshot) {
			s.Ownership.Captures[0].TunnelSubnets = nil
			c.status.LastCapture.TunnelSubnets = nil
		}},
		{"invalid-subnet", func(s *session, c *baselineService, _ *snapshot) {
			s.Ownership.Captures[0].TunnelSubnets = []string{"198.51.100.0/24"}
			c.status.LastCapture.TunnelSubnets = s.Ownership.Captures[0].TunnelSubnets
		}},
		{"incomplete-addresses", func(_ *session, _ *baselineService, a *snapshot) { delete(a.InterfaceAddresses, "lo0") }},
		{"pith-address-hidden", func(_ *session, _ *baselineService, a *snapshot) {
			a.InterfaceAddresses["lo0"] = append(a.InterfaceAddresses["lo0"], netip.MustParsePrefix("198.51.100.253/32"))
		}},
		{"pith-ipv6-address", func(_ *session, _ *baselineService, a *snapshot) {
			a.InterfaceAddresses["utun90"] = append(a.InterfaceAddresses["utun90"], netip.MustParsePrefix("2001:db8:ffff::1/126"))
		}},
		{"pith-subnet-route", func(_ *session, _ *baselineService, a *snapshot) {
			a.Routes = append(a.Routes, capture.Route{Prefix: netip.MustParsePrefix("198.51.100.254/32"), Interface: "lo0", Index: 1, Local: true})
		}},
		{"pith-prefix-elsewhere", func(_ *session, _ *baselineService, a *snapshot) {
			a.Routes = append(a.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "example0", Index: 42, Scoped: true})
		}},
		{"vpn-still-present", func(s *session, _ *baselineService, a *snapshot) {
			a.Interfaces = append(a.Interfaces, s.Original.Interfaces[2])
			a.InterfaceIndices["utun50"] = 50
			a.InterfaceAddresses["utun50"] = s.Original.InterfaceAddresses["utun50"]
		}},
		{"changed-address", func(_ *session, _ *baselineService, a *snapshot) {
			a.Interfaces[2].IPv4 = []string{"203.0.113.8"}
			a.InterfaceAddresses["utun90"][0] = netip.MustParsePrefix("203.0.113.8/32")
		}},
		{"changed-route", func(_ *session, _ *baselineService, a *snapshot) {
			a.Routes[1].Gateway = netip.MustParseAddr("203.0.113.1")
		}},
		{"extra-route", func(_ *session, _ *baselineService, a *snapshot) {
			a.Routes = append(a.Routes, capture.Route{Prefix: netip.MustParsePrefix("203.0.113.99/32"), Interface: "utun90", Index: 90})
		}},
		{"non-vpn", func(_ *session, _ *baselineService, a *snapshot) { a.Interfaces[2].Kind = api.KindEthernet }},
		{"unended-older-capture", func(s *session, _ *baselineService, _ *snapshot) {
			old := s.Ownership.Captures[0]
			old.Token = "older"
			s.Ownership.Captures = append([]api.CaptureIdentity{old}, s.Ownership.Captures...)
		}},
		{"ambiguous-origin", func(s *session, _ *baselineService, _ *snapshot) {
			old := s.Original.Interfaces[2]
			old.Name = "utun55"
			s.Original.Interfaces = append(s.Original.Interfaces, old)
			s.Original.InterfaceIndices["utun55"] = 55
			s.Original.InterfaceAddresses["utun55"] = slices.Clone(s.Original.InterfaceAddresses["utun50"])
			for _, r := range slices.Clone(s.Original.Routes) {
				if r.Interface == "utun50" {
					r.Interface = "utun55"
					r.Index = 55
					s.Original.Routes = append(s.Original.Routes, r)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, c, _, after := reuseFixture(t, "utun90", 90)
			test.change(s, c, &after)
			if _, err := s.externalDifferences(after, c.status); err == nil {
				t.Fatal("unsafe reuse accepted")
			}
		})
	}
}

func TestReuseRechecksAddressesAfterPreflight(t *testing.T) {
	s, c, ops, after := reuseFixture(t, "utun90", 90)
	ctx := context.Background()
	if err := stopWithOps(ctx, s, c, ops); !errors.Is(err, errExternalChange) {
		t.Fatal(err)
	}
	before := s.baselineDigest()
	calls := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		calls++
		next := cloneSnapshot(after)
		if calls == 2 {
			next.InterfaceAddresses["lo0"] = append(next.InterfaceAddresses["lo0"], netip.MustParsePrefix("198.51.100.253/32"))
		}
		return next, nil
	}
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) { return tlsEvidence{StatusCode: 200}, nil }
	if rebaselineWithOps(ctx, s, c, "reused", ops, true) == nil || s.baselineDigest() != before {
		t.Fatal("late residue changed baseline")
	}
}
