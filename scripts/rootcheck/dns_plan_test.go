package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/testprofile"
)

func TestDNSPlanFreezesTwoResolverHostsAndQuestion(t *testing.T) {
	s, _, _, _ := dnsFixture(t)
	prefixes, err := s.scopes()
	if err != nil || len(prefixes) != 3 {
		t.Fatal(prefixes, err)
	}
	for _, prefix := range prefixes {
		if prefix.Bits() != 32 {
			t.Fatal("broadened DNS capture", prefix)
		}
	}
	if err := s.writeDNSPlan(); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewedDNSPlan(); err != nil {
		t.Fatal(err)
	}
	original := s.cleanupBinding()
	s.DNSName = "other.example.com"
	if err := s.reviewedDNSPlan(); err == nil {
		t.Fatal("accepted changed query name")
	}
	s.DNSName = "example.com"
	s.ExtraDNSTargets[0] = "203.0.113.53:53"
	if err := s.reviewedDNSPlan(); err == nil {
		t.Fatal("accepted changed resolver")
	}
	s.ExtraDNSTargets[0] = "198.51.100.53:53"
	data, err := os.ReadFile(filepath.Join(s.Dir, "dns-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"batchQueries": 20`) || !strings.Contains(string(data), `"smokeQueries": 1`) {
		t.Fatal("missing frozen budgets")
	}
	s.DNSPlanSHA256 = strings.Repeat("0", 64)
	if original == s.cleanupBinding() {
		t.Fatal("cleanup evidence not bound to DNS plan")
	}
	if err := s.reviewedDNSPlan(); err == nil {
		t.Fatal("accepted changed plan digest")
	}
	s.DNSPlanSHA256 = hashBytes(data)
	if err := writePrivate(filepath.Join(s.Dir, "dns-plan.json"), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewedDNSPlan(); err == nil {
		t.Fatal("accepted modified review file")
	}
}

func TestDNSPlanRejectsExpandedScopeAndUnreviewedModes(t *testing.T) {
	for _, name := range []string{"three-targets", "duplicate", "hostname", "private", "ipv6", "wrong-port", "missing-first", "missing-target", "wildcard", "uppercase", "url", "full-mode", "ipv6-mode"} {
		t.Run(name, func(t *testing.T) {
			s, _, _, _ := dnsFixture(t)
			switch name {
			case "three-targets":
				s.ExtraDNSTargets = append(s.ExtraDNSTargets, "203.0.113.53:53")
			case "duplicate":
				s.ExtraDNSTargets[0] = s.DNSTarget
			case "hostname":
				s.DNSTarget = "dns.example.com:53"
			case "private":
				s.DNSTarget = "10.0.0.53:53"
			case "ipv6":
				s.DNSTarget = "[2001:db8::53]:53"
			case "wrong-port":
				s.DNSTarget = "192.0.2.53:5353"
			case "missing-first":
				s.DNSTarget = ""
			case "missing-target":
				s.DNSTarget = ""
				s.ExtraDNSTargets = nil
			case "wildcard":
				s.DNSName = "*.example.com"
			case "uppercase":
				s.DNSName = "Example.com"
			case "url":
				s.DNSName = "https://example.com"
			case "full-mode":
				s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 120 * time.Second, Target: s.Target, ServerName: s.ServerName}
			case "ipv6-mode":
				s.TestProfile = &testprofile.Profile{Mode: testprofile.HostIPv6, Lease: 60 * time.Second, Target: "[2001:db8::20]:443", ServerName: s.ServerName}
			}
			if _, err := s.scopes(); err == nil {
				t.Fatal("accepted unreviewed DNS scope")
			}
		})
	}
	legacy := session{Target: "192.0.2.20:443", ServerName: "example.com", DNSTarget: "192.0.2.53:53"}
	if scopes, err := legacy.scopes(); err != nil || len(scopes) != 2 {
		t.Fatal("legacy manual DNS scope no longer loads", scopes, err)
	}
	if err := legacy.reviewedDNSPlan(); err == nil {
		t.Fatal("old manual session allowed new automatic requests without a plan")
	}
}

func TestDNSStageAdmissionHasNoSideEffects(t *testing.T) {
	for _, name := range []string{"missing-plan", "missing-tcp-smoke", "missing-dns-smoke", "mixed-mode", "wrong-mode"} {
		t.Run(name, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			s.DNSTarget, s.DNSName = "192.0.2.53:53", "example.com"
			if err := s.writeDNSPlan(); err != nil {
				t.Fatal(err)
			}
			opts := checkOptions{udpDNS: true}
			switch name {
			case "missing-plan":
				s.DNSPlanSHA256 = ""
			case "missing-tcp-smoke":
				s.SmokePassed = false
			case "missing-dns-smoke":
				opts = checkOptions{udpDNSBatch: true}
			case "mixed-mode":
				opts.batch = true
			case "wrong-mode":
				s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4}
			}
			proof := s.Cleanup
			ops.snapshot = func(context.Context) (snapshot, error) { t.Fatal("admission read live state"); return snapshot{}, nil }
			if err := checkWithOps(context.Background(), s, c, opts, ops); err == nil {
				t.Fatal("accepted invalid DNS command")
			}
			if c.starts != 0 || c.stops != 0 || s.Cleanup != proof {
				t.Fatal("admission touched capture or cleanup proof")
			}
		})
	}
}
