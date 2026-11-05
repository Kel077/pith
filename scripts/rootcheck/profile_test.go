package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/captureprefix"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/testprofile"
	"howett.net/plist"
)

func fullProfileFixture(t *testing.T) *session {
	t.Helper()
	s, _, _ := baselineFixture(t)
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 120 * time.Second, Target: s.Target, ServerName: s.ServerName}
	s.Original.Routes = append(s.Original.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 42})
	s.Original.Interfaces[0].DNS = []string{"192.0.2.53", "100.100.100.100"}
	if err := s.freezeFullScope(s.Original); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFullProfileFreezesProductionDNSScope(t *testing.T) {
	s := fullProfileFixture(t)
	scopes, err := s.scopes()
	if err != nil || !slices.Equal(scopes, append(captureprefix.Full(false), netip.MustParsePrefix("192.0.2.53/32"))) {
		t.Fatal("incorrect full scope", scopes, err)
	}
	if s.checkTimeout() != 110*time.Second {
		t.Fatal("controller timeout can exceed helper lease")
	}
	if err := s.validateSnapshotScope(s.Original); err != nil {
		t.Fatal(err)
	}
	changed := cloneSnapshot(s.Original)
	changed.Interfaces[0].DNS = []string{"192.0.2.54"}
	if s.validateSnapshotScope(changed) == nil {
		t.Fatal("DNS change silently widened/replaced capture")
	}
	changed = cloneSnapshot(s.Original)
	changed.Routes = append(changed.Routes, capture.Route{Prefix: scopes[0], Interface: "utun70", Index: 70})
	if s.validateSnapshotScope(changed) == nil {
		t.Fatal("full capture conflict accepted")
	}
	s.DNSTarget = "192.0.2.55:53"
	if _, err := s.scopes(); err == nil {
		t.Fatal("mixed full/manual DNS mode accepted")
	}
}

func TestFullInstallationIsDistinctAndFrozenInSessionAndPlist(t *testing.T) {
	s := fullProfileFixture(t)
	if err := os.WriteFile(s.Helper, []byte("synthetic reviewed helper; never run"), 0700); err != nil {
		t.Fatal(err)
	}
	s.HelperSHA256, _ = fileHash(s.Helper)
	plan, err := s.plan()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Prompt, "full IPv4 test helper") || !strings.Contains(plan.Script, "--test-profile") || strings.Contains(plan.Script, "<string>--capture</string>") {
		t.Fatal("test installation not distinct from host/production modes")
	}
	s.InstallScriptSHA256 = hashBytes([]byte(plan.Script))
	args, _ := s.TestProfile.Arguments()
	args = append([]string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID)}, args...)
	path := filepath.Join(s.Dir, "reviewed.plist")
	data, err := plist.Marshal(map[string]any{"Label": helper.Label, "ProgramArguments": args}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyInstalledPlist(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reviewedPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(){
		func() { s.TestProfile.Lease = 60 * time.Second },
		func() { s.TestProfile.DNSHosts = nil },
		func() { s.TestProfile = nil },
	} {
		previous := *s.TestProfile
		change()
		if s.verifyInstalledPlist(path) == nil {
			t.Fatal("different installation adopted")
		}
		if _, err := s.reviewedPlan(context.Background()); err == nil {
			t.Fatal("modified profile reused old reviewed script")
		}
		s.TestProfile = &previous
	}
}

func TestFullProfileFreezesGatewayDNSExclusionAndReportsIt(t *testing.T) {
	s, c, ops := baselineFixture(t)
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 120 * time.Second, Target: s.Target, ServerName: s.ServerName}
	s.Original.Routes = append(s.Original.Routes,
		capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 42},
		capture.Route{Prefix: netip.MustParsePrefix("192.0.2.254/32"), Interface: "example0", Index: 42},
		capture.Route{Prefix: netip.MustParsePrefix("192.0.2.254/32"), Interface: "example1", Index: 43, Scoped: true},
	)
	s.Original.Routes[0].Gateway = netip.MustParseAddr("192.0.2.254")
	s.Original.Interfaces[0].DNS = []string{"192.0.2.254", "192.0.2.53", "192.0.2.254"}
	if err := s.freezeFullScope(s.Original); err != nil {
		t.Fatal("gateway DNS prevented preparation", err)
	}
	if !slices.Equal(s.TestProfile.DNSHosts, []netip.Addr{netip.MustParseAddr("192.0.2.53")}) || len(s.SkippedDNS) != 1 || s.SkippedDNS[0].Address.String() != "192.0.2.254" || s.SkippedDNS[0].Reason == "" || len(s.SkippedDNS[0].Routes) != 2 {
		t.Fatal("preparation did not freeze and explain the exclusion", s.TestProfile, s.SkippedDNS)
	}
	plan, err := s.snapshotPlan(s.Original)
	if err != nil || !reflect.DeepEqual(plan.SkippedDNS, s.SkippedDNS) || !slices.Contains(plan.Warnings, api.Warning{Code: api.WarnDNSUnobserved, Detail: "192.0.2.254"}) {
		t.Fatal("controller and production capture disagree", plan, err)
	}
	// A later host route for a previously captured DNS server changes the frozen
	// scope. Conversely, removing the gateway host route must not widen it.
	changed := cloneSnapshot(s.Original)
	changed.Routes = append(changed.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.53/32"), Interface: "example0", Index: 42})
	if s.validateSnapshotScope(changed) == nil {
		t.Fatal("new configured DNS route silently narrowed the frozen scope")
	}
	changed = cloneSnapshot(s.Original)
	changed.Routes = changed.Routes[:2]
	if s.validateSnapshotScope(changed) == nil {
		t.Fatal("removing the configured gateway route silently widened capture")
	}
	proof := cleanupRecord{At: time.Now(), Operation: "stop", Label: s.BaselineLabel, Binding: s.cleanupBinding(), Status: c.status, After: s.Original, Restored: true}
	if err := s.recordCleanup(proof, nil, ops); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(s.Dir)
	if err != nil || !reflect.DeepEqual(loaded.SkippedDNS, s.SkippedDNS) {
		t.Fatal("private session lost preparation diagnostics", err)
	}
	probes := 0
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
		probes++
		return tlsEvidence{StatusCode: 200, BodyBytes: 12}, nil
	}
	if err := rebaselineWithOps(context.Background(), s, c, "gateway-dns", ops); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, s.BaselineFile))
	if err != nil {
		t.Fatal(err)
	}
	var report rebaselineReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if probes != 3 || c.starts != 0 || !reflect.DeepEqual(report.SkippedDNS, s.SkippedDNS) || len(report.SkippedDNS) != 1 {
		t.Fatal("rebaseline lost the DNS exclusion or started capture", report.SkippedDNS)
	}
}
