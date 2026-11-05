package main

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/testprofile"
	"howett.net/plist"
)

func ipv6Fixture(t *testing.T) (*session, *baselineService, rootcheckOps) {
	t.Helper()
	s, c, ops := baselineFixture(t)
	s.Target = "[2001:db8::20]:443"
	s.TestProfile = &testprofile.Profile{Mode: testprofile.HostIPv6, Lease: time.Minute, Target: s.Target, ServerName: s.ServerName}
	s.Original.Routes = append(s.Original.Routes, capture.Route{Prefix: netip.MustParsePrefix("::/0"), Interface: "example0", Index: 42})
	s.Original.Interfaces[0].IPv6 = []string{"2001:db8:1::1"}
	s.Original.Interfaces[1].IPv6 = []string{"2001:db8:2::1"}
	s.Preflight = []preflight{{TLS: tlsEvidence{PublicIP: "2001:db8:1::1"}}, {Interface: "example0", TLS: tlsEvidence{PublicIP: "2001:db8:1::1"}}, {Interface: "example1", TLS: tlsEvidence{PublicIP: "2001:db8:2::1"}}}
	return s, c, ops
}

func TestIPv6SessionRequiresExactHostFamilyAndAvailableAdapters(t *testing.T) {
	s, c, ops := ipv6Fixture(t)
	prefixes, err := s.scopes()
	if err != nil || !slices.Equal(prefixes, []netip.Prefix{netip.MustParsePrefix("2001:db8::20/128")}) || s.tcpNetwork() != "tcp6" || s.checkTimeout() != 50*time.Second {
		t.Fatal("IPv6 mode did not freeze family/scope/deadline", prefixes, err)
	}
	if err := s.validateSnapshotScope(s.Original); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]string{nil, {"fe80::1"}, {"fd00::1"}, {"::ffff:192.0.2.1"}, {"2001:db8::1%example0"}} {
		s.Original.Interfaces[0].IPv6 = invalid
		if err := checkWithOps(context.Background(), s, c, checkOptions{}, ops); err == nil {
			t.Fatal("IPv6-unavailable adapter reached helper Start")
		}
	}
	s.DNSTarget = "192.0.2.53:53"
	if _, err := s.scopes(); err == nil {
		t.Fatal("IPv6 mode widened to DNS capture")
	}
	s.DNSTarget = ""
	s.Target, s.TestProfile.Target = "192.0.2.20:443", "192.0.2.20:443"
	if _, err := s.scopes(); err == nil {
		t.Fatal("IPv4 endpoint counted as IPv6")
	}
}

func TestIPv6InstallationAndCaptureReceiptCannotBecomeFullCapture(t *testing.T) {
	s, _, _ := ipv6Fixture(t)
	if err := os.WriteFile(s.Helper, []byte("synthetic executable; never run"), 0700); err != nil {
		t.Fatal(err)
	}
	s.HelperSHA256, _ = fileHash(s.Helper)
	plan, err := s.plan()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Prompt, "IPv6 host test helper") || !strings.Contains(plan.Script, "host-ipv6") || strings.Contains(plan.Script, "--capture") {
		t.Fatal("indistinct IPv6 installation")
	}
	s.InstallScriptSHA256 = hashBytes([]byte(plan.Script))
	if _, err := s.reviewedPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	args, _ := s.TestProfile.Arguments()
	args = append([]string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID)}, args...)
	data, _ := plist.Marshal(map[string]any{"Label": helper.Label, "ProgramArguments": args}, plist.XMLFormat)
	path := filepath.Join(s.Dir, "ipv6.plist")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyInstalledPlist(path); err != nil {
		t.Fatal(err)
	}
	id := &api.CaptureIdentity{Token: "synthetic", BootSession: "test-boot", Interface: "utun90", Index: 90, Prefixes: []string{"2001:db8::20/128"}}
	if !s.validCapture(id) {
		t.Fatal("reviewed IPv6 receipt rejected")
	}
	for _, prefixes := range [][]string{{"::/1", "8000::/1"}, {"2001:db8::21/128"}, {"2001:db8::20/128", "192.0.2.20/32"}} {
		id.Prefixes = prefixes
		if s.validCapture(id) {
			t.Fatal("expanded capture receipt accepted")
		}
	}
	s.TestProfile.Lease = 120 * time.Second
	if s.verifyInstalledPlist(path) == nil {
		t.Fatal("IPv6 lease extended")
	}
}

func TestIPv6AppProofsRequireIPv6EchoAndPreserveFamily(t *testing.T) {
	s, _, _ := ipv6Fixture(t)
	cfg := api.DefaultConfig()
	cfg.Settings.SafeStart, cfg.Settings.IPv6 = true, true
	service := &proofService{session: s, since: time.Now(), cfg: cfg}
	self, child := "/tmp/pith-example/rootcheck", "/tmp/pith-example/request-tool/rootcheck-unmatched"
	var report checkReport
	if err := runAppProofs(context.Background(), s, service, cfg, service.since, "utun42", self, child, service.requester(self, 9001), service.requester(child, 9002), &report); err != nil {
		t.Fatal(err)
	}
	for _, saved := range service.saved {
		if !saved.Settings.IPv6 || !saved.Settings.SafeStart {
			t.Fatal("case disabled IPv6 safety")
		}
	}
	for _, entry := range report.Cases {
		if entry.Connection.Remote != s.Target {
			t.Fatal("IPv6 proof used another target")
		}
	}
	entry := checkCase{TLS: tlsEvidence{PublicIP: "192.0.2.1"}}
	if s.checkEcho(&entry, "example0") == nil {
		t.Fatal("IPv4 echo concealed IPv6 loss")
	}
	s.Preflight[1].TLS.PublicIP = "192.0.2.1"
	if s.checkEcho(&entry, "example0") == nil {
		t.Fatal("an invalid preflight echo overrode the frozen IPv6 family")
	}
}

func TestIPv6PreflightAcceptsPrivacyRotationButRejectsPrefixChanges(t *testing.T) {
	s, _, _ := ipv6Fixture(t)
	s.Original.InterfaceAddresses = map[string][]netip.Prefix{
		"example0": {netip.MustParsePrefix("2001:db8:1::1/64")},
		"example1": {netip.MustParsePrefix("2001:db8:2::1/64")},
	}
	after := cloneSnapshot(s.Original)
	after.Interfaces[0].IPv6 = []string{"2001:db8:1::2"}
	after.InterfaceAddresses["example0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:1::2/64")}
	if !stableSnapshots(s.Original, after) {
		t.Fatal("privacy-address rotation within the assigned prefix blocked preflight")
	}
	after.Interfaces[0].IPv6 = []string{"2001:db8:3::2"}
	after.InterfaceAddresses["example0"] = []netip.Prefix{netip.MustParsePrefix("2001:db8:3::2/64")}
	if stableSnapshots(s.Original, after) {
		t.Fatal("preflight silently accepted a new IPv6 prefix")
	}
}
