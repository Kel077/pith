package main

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
)

func TestScopesAcceptOnlyReviewedHosts(t *testing.T) {
	s := session{Target: "192.0.2.20:443", ServerName: "example.com", DNSTarget: "192.0.2.53:53"}
	got, err := s.scopes()
	if err != nil || len(got) != 2 || got[0].String() != "192.0.2.20/32" || got[1].String() != "192.0.2.53/32" {
		t.Fatalf("scopes = %v, %v", got, err)
	}
	for _, target := range []string{"0.0.0.0:443", "127.0.0.1:443", "10.0.0.1:443", "169.254.1.1:443", "100.64.0.1:443", "224.0.0.1:443", "[2001:db8::1]:443", "192.0.2.20:0", "192.0.2.0/24", "example.com:443"} {
		t.Run(target, func(t *testing.T) {
			if _, err := testAddress(target); err == nil {
				t.Fatal("accepted an unsupported capture target")
			}
		})
	}
	for _, name := range []string{"", "*.example.com", "https://example.com", "example.com:443", "example.com\r\nInjected: true", "192.0.2.20"} {
		s.ServerName = name
		if _, err := s.scopes(); err == nil {
			t.Fatalf("accepted server name %q", name)
		}
	}
}

func TestReviewedPlanRejectsChangedHelper(t *testing.T) {
	s := session{Helper: filepath.Join(t.TempDir(), "pithd"), UID: 501, Target: "192.0.2.20:443", ServerName: "example.com"}
	if err := os.WriteFile(s.Helper, []byte("test executable bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	var err error
	s.HelperSHA256, err = fileHash(s.Helper)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.plan()
	if err != nil {
		t.Fatal(err)
	}
	s.InstallScriptSHA256 = hashBytes([]byte(plan.Script))
	if _, err := s.reviewedPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Script, "192.0.2.20/32") || strings.Contains(plan.Script, "0.0.0.0/1") {
		t.Fatal("installation does not constrain capture to the reviewed host")
	}
	if err := os.WriteFile(s.Helper, []byte("changed executable bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reviewedPlan(context.Background()); err == nil {
		t.Fatal("accepted a helper changed after review")
	}
}

func TestPrivateSessionStaysInsideTemporaryDirectory(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privateTempDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := privateTempDirectory(dir); err == nil {
		t.Fatal("accepted a public session directory")
	}
	for _, invalid := range []string{"relative", "/", os.TempDir()} {
		if err := privateTempDirectory(invalid); err == nil {
			t.Fatalf("accepted invalid session location %q", invalid)
		}
	}
}

func TestEmergencyCommandUsesOwnedCleanupWithoutGlobalRouteChanges(t *testing.T) {
	plan := helper.PlanUninstall()
	command := emergencyCommand(plan)
	if strings.Contains(command, "\n") || !strings.HasPrefix(command, "/usr/bin/osascript -e '") {
		t.Fatal("emergency command must be one shell-quoted command line")
	}
	for _, required := range []string{"bootout", "--cleanup", helper.BinaryPath, helper.PlistPath, helper.DataDir, helper.SocketPath} {
		if !strings.Contains(command, required) {
			t.Fatalf("emergency command omits %q", required)
		}
	}
	for _, forbidden := range []string{"route flush", "killall", "ifconfig utun", "sudo"} {
		if strings.Contains(command, forbidden) {
			t.Fatalf("emergency command contains broad or unapproved action %q", forbidden)
		}
	}
	if shellQuote("a'b\nc") != "'a'\"'\"'b\nc'" {
		t.Fatal("shell quoting lost literal input")
	}
}

func TestSnapshotComparisonDetectsRouteAndTunnelChanges(t *testing.T) {
	a := capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 42}
	b := capture.Route{Prefix: netip.MustParsePrefix("2001:db8::/32"), Interface: "example1", Index: 43}
	if !sameRoutes([]capture.Route{a, b}, []capture.Route{b, a}) {
		t.Fatal("route order is not a routing change")
	}
	changed := a
	changed.Interface, changed.Index = "utun42", 84
	if sameRoutes([]capture.Route{a, b}, []capture.Route{changed, b}) {
		t.Fatal("missed a changed route owner")
	}
	if sameTunnels([]api.Interface{{Name: "utun42"}}, []api.Interface{{Name: "utun42"}, {Name: "utun43"}}) {
		t.Fatal("missed an extra utun")
	}
}
