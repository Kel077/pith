package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
)

func uninstalledExternalFixture(t *testing.T) (*session, rootcheckOps, snapshot) {
	t.Helper()
	s, c, ops := baselineFixture(t)
	s.Original.Interfaces[0].IPv6 = []string{"2001:db8:10::1", "2001:db8:10::2", "2001:db8:20::1"}
	s.Original.InterfaceAddresses = map[string][]netip.Prefix{
		"example0": {netip.MustParsePrefix("192.0.2.1/24"), netip.MustParsePrefix("2001:db8:10::1/64"), netip.MustParsePrefix("2001:db8:10::2/64"), netip.MustParsePrefix("2001:db8:20::1/64")},
		"example1": {netip.MustParsePrefix("198.51.100.1/24")},
	}
	s.Original.Routes = append(s.Original.Routes, capture.Route{Prefix: netip.MustParsePrefix("2001:db8:10::/64"), Interface: "example0", Index: 42})
	id := api.CaptureIdentity{BootSession: s.Original.BootSession, Token: "retained-capture", Interface: "utun90", Index: 90,
		Prefixes: []string{"192.0.2.20/32"}, TunnelSubnets: []string{"198.51.100.252/30", "2001:db8:ffff::/126"}}
	s.Ownership.Captures = []api.CaptureIdentity{id}
	id.EndedAt = s.Original.At.Add(time.Minute)
	c.status.LastCapture = &id
	s.rememberEnded(c.status)
	s.DNSPassed, s.QUICPassed, s.BundlePassed = true, true, true
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	after := cloneSnapshot(s.Original)
	after.At = s.Original.At.Add(2 * time.Minute)
	after.Paths, after.LaunchExit, after.Launchd = absentPathStates(), 113, "Could not find service \""+helper.Label+"\" in domain for system"
	after.Routes = after.Routes[:1]
	after.Interfaces[0].IPv6 = []string{"2001:db8:20::1"}
	after.InterfaceAddresses["example0"] = []netip.Prefix{netip.MustParsePrefix("192.0.2.1/24"), netip.MustParsePrefix("2001:db8:20::1/64")}
	reads := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		reads++
		current := cloneSnapshot(after)
		current.At = current.At.Add(time.Duration(reads) * time.Second)
		return current, nil
	}
	ops.verifyInstalled = func(*session) error {
		t.Fatal("post-removal verification contacted the installed-helper boundary")
		return nil
	}
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
		t.Fatal("post-removal verification probed the network")
		return tlsEvidence{}, nil
	}
	return s, ops, after
}

func TestUninstalledExplicitlyAcceptsStableIPv6PrefixExpiry(t *testing.T) {
	s, ops, _ := uninstalledExternalFixture(t)
	before, digest := cloneSnapshot(s.Original), s.baselineDigest()
	record, err := verifyUninstalledReport(context.Background(), s, ops, true)
	if err != nil {
		t.Fatal(err)
	}
	if !record.ExplicitAccepted || record.Restored || record.Error != "" || len(record.Snapshots) != 2 || record.External == nil || len(record.External.RoutesRemoved) != 1 || len(record.External.AddressesRemoved["example0"]) != 2 || len(record.External.Migrations) != 0 {
		t.Fatalf("incomplete external-only evidence: %+v", record)
	}
	if !reflect.DeepEqual(s.Original, before) || s.baselineDigest() != digest || s.Installed || s.SmokePassed || s.DNSPassed || s.QUICPassed || s.BundlePassed || s.Cleanup != nil {
		t.Fatal("acceptance changed the baseline or retained installed/pass state")
	}
	files, err := filepath.Glob(filepath.Join(s.Dir, "uninstall-verification-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing accepted verification report", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved uninstallVerification
	if err := json.Unmarshal(data, &saved); err != nil || !saved.ExplicitAccepted || saved.Restored || saved.BaselineSHA256 != digest || saved.Ownership.Captures[0].EndedAt.IsZero() {
		t.Fatal("accepted report lost its retained baseline or closure receipt", err)
	}
	loaded, err := loadSession(s.Dir)
	if err != nil || loaded.Installed || loaded.baselineDigest() != digest || !reflect.DeepEqual(loaded.Ownership, s.Ownership) {
		t.Fatal("accepted removal state was not saved without changing history", err)
	}
}

func TestUninstalledExternalChangesRequireExplicitFlag(t *testing.T) {
	s, ops, _ := uninstalledExternalFixture(t)
	digest := s.baselineDigest()
	ops.save = func(*session) error { t.Fatal("implicit external acceptance saved phase state"); return nil }
	record, err := verifyUninstalledReport(context.Background(), s, ops, false)
	if err == nil || !strings.Contains(err.Error(), "verify-uninstalled --accept-external-change") || !s.Installed || record.ExplicitAccepted || record.Restored || s.baselineDigest() != digest {
		t.Fatal("default verification accepted changed network state", record, err)
	}
}

func TestUninstalledExplicitAcceptanceKeepsRestoredCaseDistinct(t *testing.T) {
	s, ops, after := uninstalledExternalFixture(t)
	restored := cloneSnapshot(s.Original)
	restored.Paths, restored.LaunchExit, restored.Launchd = after.Paths, after.LaunchExit, after.Launchd
	reads := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		reads++
		shot := cloneSnapshot(restored)
		shot.At = after.At.Add(time.Duration(reads) * time.Second)
		return shot, nil
	}
	record, err := verifyUninstalledReport(context.Background(), s, ops, true)
	if err != nil || record.ExplicitAccepted || !record.Restored || record.External != nil || reads != 2 {
		t.Fatal("strict restoration was mislabeled as external acceptance", record, err)
	}
}

func TestUninstalledSnapshotIntervalHonorsCancellation(t *testing.T) {
	s, ops, after := uninstalledExternalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		reads++
		cancel()
		return after, nil
	}
	ops.save = func(*session) error { t.Fatal("canceled verification saved successful state"); return nil }
	if err := verifyUninstalledWithOps(ctx, s, ops, true); !errors.Is(err, context.Canceled) || reads != 1 || !s.Installed {
		t.Fatal("snapshot interval ignored cancellation", reads, err)
	}
}

func TestUninstalledExternalAcceptanceRejectsIncompleteHistoryAndResidue(t *testing.T) {
	for _, fault := range []string{"history-unknown", "history-pending", "no-receipts", "unended", "future-end", "equal-end", "duplicate-token", "invalid-token", "invalid-scope", "receipt-boot", "missing-subnets", "invalid-subnets", "baseline-addresses", "current-addresses", "capture-prefix", "scoped-capture-prefix", "owned-name", "owned-index", "baseline-reuse", "tunnel-address", "tunnel-ipv6", "tunnel-route", "tunnel-gateway", "helper-present", "data-present", "path-unreadable", "missing-path", "duplicate-path", "service-present", "service-unknown", "other-service", "similar-service", "wrong-domain", "boot-changed", "untimed"} {
		t.Run(fault, func(t *testing.T) {
			s, ops, after := uninstalledExternalFixture(t)
			switch fault {
			case "history-unknown":
				s.Ownership.Complete = false
			case "history-pending":
				s.Ownership.Pending = true
			case "no-receipts":
				s.Ownership.Captures = nil
			case "unended":
				s.Ownership.Captures[0].EndedAt = time.Time{}
			case "future-end":
				s.Ownership.Captures[0].EndedAt = after.At.Add(time.Minute)
			case "equal-end":
				s.Ownership.Captures[0].EndedAt = after.At
			case "duplicate-token":
				s.Ownership.Captures = append(s.Ownership.Captures, s.Ownership.Captures[0])
			case "invalid-token":
				s.Ownership.Captures[0].Token = ""
			case "invalid-scope":
				s.Ownership.Captures[0].Prefixes = []string{"203.0.113.20/32"}
			case "receipt-boot":
				s.Ownership.Captures[0].BootSession = "another-boot"
			case "missing-subnets":
				s.Ownership.Captures[0].TunnelSubnets = nil
			case "invalid-subnets":
				s.Ownership.Captures[0].TunnelSubnets = []string{"198.51.100.0/24"}
			case "baseline-addresses":
				s.Original.InterfaceAddresses = nil
			case "current-addresses":
				delete(after.InterfaceAddresses, "example1")
			case "capture-prefix", "scoped-capture-prefix":
				after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "example0", Index: 42, Scoped: fault == "scoped-capture-prefix"})
			case "owned-name":
				after.InterfaceIndices["utun90"], after.InterfaceAddresses["utun90"] = 91, nil
				after.Interfaces = append(after.Interfaces, api.Interface{Name: "utun90", Kind: api.KindVPN})
			case "owned-index":
				after.InterfaceIndices["utun91"], after.InterfaceAddresses["utun91"] = 90, nil
				after.Interfaces = append(after.Interfaces, api.Interface{Name: "utun91", Kind: api.KindVPN})
			case "baseline-reuse":
				s.Original.InterfaceIndices["utun90"], s.Original.InterfaceAddresses["utun90"] = 91, nil
				s.Original.Interfaces = append(s.Original.Interfaces, api.Interface{Name: "utun90", Kind: api.KindVPN})
			case "tunnel-address":
				after.InterfaceAddresses["example0"] = append(after.InterfaceAddresses["example0"], netip.MustParsePrefix("198.51.100.253/32"))
			case "tunnel-ipv6":
				after.InterfaceAddresses["example0"] = append(after.InterfaceAddresses["example0"], netip.MustParsePrefix("2001:db8:ffff::1/128"))
			case "tunnel-route":
				after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("198.51.100.253/32"), Interface: "example0", Index: 42})
			case "tunnel-gateway":
				after.Routes[0].Gateway = netip.MustParseAddr("198.51.100.253")
			case "helper-present":
				after.Paths[0].Present = true
			case "data-present":
				after.Paths[2].Present = true
			case "path-unreadable":
				after.Paths[3].Error = "permission denied"
			case "missing-path":
				after.Paths = after.Paths[:4]
			case "duplicate-path":
				after.Paths[1] = after.Paths[0]
			case "service-present":
				after.LaunchExit = 0
			case "service-unknown":
				after.Launchd = "permission denied"
			case "other-service":
				after.Launchd = "Could not find service example.unrelated"
			case "similar-service":
				after.Launchd = "Could not find service \"" + helper.Label + ".other\" in domain for system"
			case "wrong-domain":
				after.Launchd = "Could not find service \"" + helper.Label + "\" in domain for user"
			case "boot-changed":
				after.BootSession = "another-boot"
			case "untimed":
				after.At = time.Time{}
			}
			digest := s.baselineDigest()
			ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
			ops.save = func(*session) error { t.Fatal("invalid evidence saved successful removal state"); return nil }
			record, err := verifyUninstalledReport(context.Background(), s, ops, true)
			if err == nil || record.ExplicitAccepted || record.Restored || !s.Installed || s.baselineDigest() != digest {
				t.Fatal("incomplete ownership or residual capture accepted", err)
			}
		})
	}
}

func TestUninstalledExternalAcceptanceRechecksTheSecondSnapshot(t *testing.T) {
	for _, fault := range []string{"routes", "addresses", "interface", "boot", "path", "service", "unreadable", "same-time"} {
		t.Run(fault, func(t *testing.T) {
			s, ops, after := uninstalledExternalFixture(t)
			reads := 0
			ops.snapshot = func(context.Context) (snapshot, error) {
				reads++
				shot := cloneSnapshot(after)
				if reads == 1 {
					return shot, nil
				}
				shot.At = shot.At.Add(time.Second)
				switch fault {
				case "routes":
					shot.Routes = nil
				case "addresses":
					shot.InterfaceAddresses["example0"] = append(shot.InterfaceAddresses["example0"], netip.MustParsePrefix("2001:db8:20::2/64"))
				case "interface":
					shot.Interfaces[0].Up = false
				case "boot":
					shot.BootSession = "another-boot"
				case "path":
					shot.Paths = append([]pathState(nil), shot.Paths...)
					shot.Paths[0].Present = true
				case "service":
					shot.LaunchExit = 0
				case "unreadable":
					return snapshot{}, errors.New("second snapshot failed")
				case "same-time":
					shot.At = after.At
				}
				return shot, nil
			}
			ops.save = func(*session) error { t.Fatal("unstable snapshots saved removal state"); return nil }
			if err := verifyUninstalledWithOps(context.Background(), s, ops, true); err == nil || reads != 2 || !s.Installed {
				t.Fatal("unstable or incomplete post-removal observations accepted", reads, err)
			}
		})
	}
}

func TestUninstalledAcceptancePersistenceFailureRetainsPhase(t *testing.T) {
	for _, fault := range []string{"archive", "save"} {
		t.Run(fault, func(t *testing.T) {
			s, ops, _ := uninstalledExternalFixture(t)
			old, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
			if err != nil {
				t.Fatal(err)
			}
			if fault == "archive" {
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("archive failed") }
				ops.save = func(*session) error { t.Fatal("archive failure changed phase state"); return nil }
			} else {
				ops.save = func(next *session) error {
					if next == s || next.Installed || !s.Installed {
						t.Fatal("uninstall flags changed before durable commit")
					}
					return errors.New("save failed")
				}
			}
			record, err := verifyUninstalledReport(context.Background(), s, ops, true)
			if err == nil || record.ExplicitAccepted || record.Restored || !s.Installed || !s.SmokePassed || s.Cleanup == nil {
				t.Fatal("failed persistence claimed successful removal", err)
			}
			current, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
			if err != nil || string(current) != string(old) {
				t.Fatal("failed persistence changed saved session", err)
			}
		})
	}
}
