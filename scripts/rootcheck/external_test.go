package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

func externalFixture(t *testing.T) (*session, *baselineService, rootcheckOps, snapshot) {
	t.Helper()
	s, c, ops := baselineFixture(t)
	id := api.CaptureIdentity{BootSession: "test-boot", Token: "observed-capture", Interface: "utun90", Index: 90, Prefixes: []string{"192.0.2.20/32"}}
	s.Ownership.Captures = []api.CaptureIdentity{id}
	c.status.LastCapture = &id
	s.Original.Interfaces = append(s.Original.Interfaces, api.Interface{Name: "utun50", Kind: api.KindVPN, Up: true})
	s.Original.InterfaceIndices["utun50"] = 50
	s.Original.Routes = append(s.Original.Routes, capture.Route{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Interface: "utun50", Index: 50})
	after := cloneSnapshot(s.Original)
	after.Interfaces[2].Name, after.Routes[1].Interface, after.Routes[1].Index = "utun51", "utun51", 51
	delete(after.InterfaceIndices, "utun50")
	after.InterfaceIndices["utun51"] = 51
	ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
	return s, c, ops, after
}

func TestExternalCleanupRequiresExplicitRebaseline(t *testing.T) {
	s, c, ops, after := externalFixture(t)
	ctx := context.Background()
	if err := stopWithOps(ctx, s, c, ops); !errors.Is(err, errExternalChange) {
		t.Fatalf("cleanup: %v", err)
	}
	if s.Cleanup == nil || s.Cleanup.Kind != externalProof {
		t.Fatal("missing separately classified evidence")
	}
	proof := *s.Cleanup
	data, err := os.ReadFile(filepath.Join(s.Dir, proof.File))
	if err != nil {
		t.Fatal(err)
	}
	var record cleanupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Passed || record.Restored || record.External == nil || len(record.External.RoutesAdded) != 1 || len(record.External.RoutesRemoved) != 1 || record.External.IndicesAdded["utun51"] != 51 || record.External.IndicesRemoved["utun50"] != 50 {
		t.Fatal("external differences were lost or declared restored")
	}
	if err := s.requireCleanup(); !errors.Is(err, errExternalChange) {
		t.Fatal("external proof silently authorized rebaseline", err)
	}
	if err := s.requireCleanup(true); err != nil {
		t.Fatal(err)
	}
	before, files := s.baselineDigest(), privateArtifacts(t, s)
	if err := rebaselineWithOps(ctx, s, c, "vpn-rebuilt", ops); !errors.Is(err, errExternalChange) {
		t.Fatal("missing opt-in accepted", err)
	}
	if s.baselineDigest() != before {
		t.Fatal("implicit acceptance changed baseline")
	}
	probes := 0
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
		probes++
		return tlsEvidence{StatusCode: 200}, nil
	}
	if err := rebaselineWithOps(ctx, s, c, "vpn-rebuilt", ops, true); err != nil {
		t.Fatal(err)
	}
	if probes != 3 || c.starts != 0 || c.stops != 1 || s.SmokePassed || s.Cleanup != nil || !sameCleanupBaseline(s.Original, after) || len(s.Ownership.Captures) != 1 {
		t.Fatal("rebaseline changed routing, lost history, or preserved pass state")
	}
	assertArtifactsUnchanged(t, s, files, false)
	data, err = os.ReadFile(filepath.Join(s.Dir, s.BaselineFile))
	if err != nil {
		t.Fatal(err)
	}
	var report rebaselineReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.AcceptedExternalChange || report.Cleanup == nil || *report.Cleanup != proof || report.External == nil {
		t.Fatal("acceptance decision was not auditable")
	}
	if err := checkWithOps(ctx, s, c, checkOptions{batch: true}, ops); err == nil {
		t.Fatal("external acceptance bypassed smoke")
	}
}

func TestExternalClassificationRejectsOwnershipAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*session, *baselineService, *snapshot)
	}{
		{"legacy-history", func(s *session, _ *baselineService, _ *snapshot) { s.Ownership.Complete = false }},
		{"pending-start", func(s *session, _ *baselineService, _ *snapshot) { s.Ownership.Pending = true }},
		{"missing-receipt", func(s *session, _ *baselineService, _ *snapshot) { s.Ownership.Captures = nil }},
		{"foreign-helper-capture", func(_ *session, c *baselineService, _ *snapshot) { c.status.LastCapture.Token = "unknown" }},
		{"artifact", func(_ *session, c *baselineService, _ *snapshot) { c.status.Helper.Outdated = true }},
		{"still-running", func(_ *session, c *baselineService, _ *snapshot) { c.status.State = api.Running }},
		{"new-boot", func(_ *session, _ *baselineService, after *snapshot) { after.BootSession = "another-boot" }},
		{"indices-missing", func(_ *session, _ *baselineService, after *snapshot) { after.InterfaceIndices = nil }},
		{"inconsistent-rib", func(_ *session, _ *baselineService, after *snapshot) { after.Routes[1].Index++ }},
		{"duplicate-interface", func(_ *session, _ *baselineService, after *snapshot) {
			after.Interfaces = append(after.Interfaces, after.Interfaces[0])
		}},
		{"owned-tunnel-remains", func(_ *session, _ *baselineService, after *snapshot) {
			after.Interfaces = append(after.Interfaces, api.Interface{Name: "utun90"})
			after.InterfaceIndices["utun90"] = 90
		}},
		{"index-reused", func(_ *session, _ *baselineService, after *snapshot) {
			after.InterfaceIndices["utun51"], after.Routes[1].Index = 90, 90
		}},
		{"old-owned-name-reused", func(s *session, _ *baselineService, _ *snapshot) {
			id := s.Ownership.Captures[0]
			id.Token, id.Interface, id.Index = "earlier", "utun51", 91
			s.Ownership.Captures = append(s.Ownership.Captures, id)
		}},
		{"capture-host-remains", func(_ *session, _ *baselineService, after *snapshot) {
			after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "example0", Index: 42})
		}},
		{"scoped-capture-host-remains", func(_ *session, _ *baselineService, after *snapshot) {
			after.Routes = append(after.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "utun51", Index: 51, Scoped: true})
		}},
		{"different-capture-scope", func(s *session, _ *baselineService, _ *snapshot) {
			s.Ownership.Captures[0].Prefixes = []string{"198.51.100.20/32"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, c, _, after := externalFixture(t)
			test.change(s, c, &after)
			if diff, err := s.externalDifferences(after, c.status); err == nil || diff != nil {
				t.Fatal("ambiguous ownership accepted", diff, err)
			}
		})
	}
}

func TestExternalEvidenceAfterCheckFailureAndHelperRestart(t *testing.T) {
	s, c, ops, after := externalFixture(t)
	c.startErr = &api.Error{Code: api.ErrNetworkLost}
	calls := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		calls++
		if calls == 1 {
			return s.Original, nil
		}
		return after, nil
	}
	if err := checkWithOps(context.Background(), s, c, checkOptions{safeFailure: true}, ops); !errors.Is(err, errExternalChange) {
		t.Fatal(err)
	}
	if s.Cleanup == nil || s.Cleanup.Kind != externalProof || len(s.Ownership.Captures) != 2 {
		t.Fatal("deferred cleanup lost identity or external proof")
	}
	if err := s.requireCleanup(true); err != nil {
		t.Fatal(err)
	}
	// launchd can restart the same installation idle. Saved controller receipts
	// survive that restart; an unknown non-nil receipt remains forbidden.
	c.status.LastCapture = nil
	if _, err := s.externalDifferences(after, c.status); err != nil {
		t.Fatal("idle restart discarded recorded ownership", err)
	}
	withdrawn := cloneSnapshot(after)
	withdrawn.Interfaces = withdrawn.Interfaces[:2]
	withdrawn.Routes = withdrawn.Routes[:1]
	delete(withdrawn.InterfaceIndices, "utun51")
	if diff, err := s.externalDifferences(withdrawn, c.status); err != nil || len(diff.RoutesAdded) != 0 || len(diff.RoutesRemoved) != 1 {
		t.Fatal("external withdrawal could not be classified", err)
	}
}

func TestExternalRebaselineRetainsFullIPv6ObservationsWithoutAddressChurnFailure(t *testing.T) {
	s, c, ops, after := externalFixture(t)
	after.Interfaces[2].IPv6 = []string{"2001:db8::1"}
	ops.snapshot = func(context.Context) (snapshot, error) { return after, nil }
	if err := stopWithOps(context.Background(), s, c, ops); !errors.Is(err, errExternalChange) {
		t.Fatal(err)
	}
	calls := 0
	ops.snapshot = func(context.Context) (snapshot, error) {
		calls++
		next := cloneSnapshot(after)
		if calls == 2 {
			next.Interfaces[2].IPv6 = []string{"2001:db8::2", "2001:db8::3"}
		}
		return next, nil
	}
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) { return tlsEvidence{StatusCode: 200}, nil }
	if err := rebaselineWithOps(context.Background(), s, c, "vpn-rebuilt", ops, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Original.Interfaces[2].IPv6, []string{"2001:db8::2", "2001:db8::3"}) {
		t.Fatal("final IPv6 observations were discarded")
	}
}

func TestExternalProofNeverHidesCleanupFailures(t *testing.T) {
	for _, fault := range []string{"stop", "status", "identity", "snapshot", "status-after-snapshot", "archive", "save"} {
		t.Run(fault, func(t *testing.T) {
			s, c, ops, _ := externalFixture(t)
			s.Cleanup = nil
			save := ops.save
			saves := 0
			switch fault {
			case "stop":
				c.stopErr = errors.New("stop failed")
			case "status":
				c.statusErr = errors.New("status failed")
			case "identity":
				ops.verifyInstalled = func(*session) error { return errors.New("installation changed") }
			case "snapshot":
				ops.snapshot = func(context.Context) (snapshot, error) { return snapshot{}, errors.New("snapshot failed") }
			case "status-after-snapshot":
				c.onStatus = func(n int, st *api.Status) error {
					if n >= 2 {
						st.State = api.Running
					}
					return nil
				}
			case "archive":
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("archive failed") }
			case "save":
				ops.save = func(s *session) error {
					saves++
					if saves > 1 {
						return errors.New("save failed")
					}
					return save(s)
				}
			}
			if err := stopWithOps(context.Background(), s, c, ops); err == nil {
				t.Fatal("cleanup failure passed")
			}
			if s.Cleanup != nil {
				t.Fatal("failure produced usable cleanup evidence")
			}
		})
	}
}

func TestExternalRebaselineRechecksCurrentStateAndKeepsOldProofOnFailure(t *testing.T) {
	for _, fault := range []string{"current-residue", "current-owned-tunnel", "down-adapter", "preflight-change", "new-capture", "probe"} {
		t.Run(fault, func(t *testing.T) {
			s, c, ops, after := externalFixture(t)
			if err := stopWithOps(context.Background(), s, c, ops); !errors.Is(err, errExternalChange) {
				t.Fatal(err)
			}
			old, proof := s.baselineDigest(), *s.Cleanup
			calls, probes := 0, 0
			ops.snapshot = func(context.Context) (snapshot, error) {
				calls++
				next := cloneSnapshot(after)
				switch fault {
				case "current-residue":
					next.Routes = append(next.Routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "example0", Index: 42})
				case "current-owned-tunnel":
					next.Interfaces = append(next.Interfaces, api.Interface{Name: "utun90"})
					next.InterfaceIndices["utun90"] = 90
				case "down-adapter":
					next.Interfaces[0].Up = false
				case "preflight-change":
					if calls == 2 {
						next.Routes[0].Gateway = netip.MustParseAddr("192.0.2.254")
					}
				}
				return next, nil
			}
			ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
				probes++
				if fault == "probe" {
					return tlsEvidence{}, errors.New("preflight failed")
				}
				if fault == "new-capture" {
					copy := *c.status.LastCapture
					copy.Token = "unreviewed"
					c.status.LastCapture = &copy
				}
				return tlsEvidence{StatusCode: 200}, nil
			}
			if err := rebaselineWithOps(context.Background(), s, c, "vpn-rebuilt", ops, true); err == nil {
				t.Fatal("unsafe baseline was accepted")
			}
			if old != s.baselineDigest() || s.Cleanup == nil || *s.Cleanup != proof || !s.SmokePassed || probes > 3 || c.starts != 0 {
				t.Fatal("failure changed accepted state or started capture")
			}
		})
	}
}

func TestCaptureHistoryPersistsFailuresAndControllerGaps(t *testing.T) {
	for _, missing := range []bool{false, true} {
		s, c, ops := baselineFixture(t)
		c.startErr = &api.Error{Code: api.ErrNetworkLost}
		c.onStart = func() {
			loaded, err := loadSession(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if !loaded.Ownership.Pending || loaded.Cleanup != nil {
				t.Fatal("Start was not durably marked pending")
			}
		}
		if missing {
			c.onStatus = func(n int, st *api.Status) error {
				if n > 1 {
					st.LastCapture = nil
				}
				return nil
			}
		}
		if err := s.startCapture(context.Background(), c, "test", ops); api.Code(err) != api.ErrNetworkLost {
			t.Fatal(err)
		}
		loaded, err := loadSession(s.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Ownership.Pending || loaded.Ownership.Complete == missing || len(loaded.Ownership.Captures) != map[bool]int{false: 1, true: 0}[missing] {
			t.Fatal("capture failure lost identity or claimed complete history")
		}
	}
	s, c, ops := baselineFixture(t)
	s.Ownership.Pending = true
	if err := s.startCapture(context.Background(), c, "test", ops); err != nil {
		t.Fatal(err)
	}
	if s.Ownership.Complete || s.Ownership.Pending {
		t.Fatal("a new Start erased an unobserved earlier attempt")
	}
}

func TestExternalProofTamperingAndTunnelIndexReuse(t *testing.T) {
	s, c, ops, after := externalFixture(t)
	if err := stopWithOps(context.Background(), s, c, ops); !errors.Is(err, errExternalChange) {
		t.Fatal(err)
	}
	file := filepath.Join(s.Dir, s.Cleanup.File)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var record cleanupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.External.RoutesRemoved = nil
	if err := writeJSON(file, record); err != nil {
		t.Fatal(err)
	}
	s.Cleanup.SHA256, _ = fileHash(file)
	if err := s.requireCleanup(true); err == nil {
		t.Fatal("accepted mismatched difference details")
	}
	next := cloneSnapshot(s.Original)
	next.InterfaceIndices["utun50"]++
	if sameCleanupBaseline(s.Original, next) {
		t.Fatal("same tunnel name concealed a new interface index")
	}
	diff, err := s.externalDifferences(after, c.status)
	if err != nil {
		t.Fatal(err)
	}
	reordered := cloneSnapshot(after)
	slices.Reverse(reordered.Routes)
	slices.Reverse(reordered.Interfaces)
	other, err := s.externalDifferences(reordered, c.status)
	if err != nil || !reflect.DeepEqual(diff, other) {
		t.Fatal("difference ordering is unstable")
	}
}
