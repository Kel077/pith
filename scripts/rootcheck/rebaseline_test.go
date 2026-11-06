package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/helper"
	"howett.net/plist"
)

type baselineService struct {
	api.Service
	status    api.Status
	statusErr error
	stopErr   error
	startErr  error
	configErr error
	starts    int
	stops     int
	statuses  int
	onStart   func()
	onStop    func()
	onStatus  func(int, *api.Status) error
	logRows   []api.Connection
	logErr    error
	logQuery  api.ConnectionQuery
}

func (c *baselineService) Status(ctx context.Context) (api.Status, error) {
	c.statuses++
	if c.onStatus != nil {
		if err := c.onStatus(c.statuses, &c.status); err != nil {
			return c.status, err
		}
	}
	return c.status, c.statusErr
}

func (c *baselineService) Start(ctx context.Context, scene string) error {
	c.starts++
	if c.onStart != nil {
		c.onStart()
	}
	c.status.State = api.Running
	c.status.LastCapture = &api.CaptureIdentity{BootSession: "test-boot", Token: fmt.Sprintf("test-capture-%d", c.starts), Interface: "utun90", Index: 90, Prefixes: []string{"192.0.2.20/32"}}
	if c.startErr != nil {
		c.status.State = api.Failed
		c.status.Error = &api.Error{Code: api.Code(c.startErr)}
	}
	return c.startErr
}

func (c *baselineService) Stop(ctx context.Context) error {
	c.stops++
	if c.onStop != nil {
		c.onStop()
	}
	if c.stopErr == nil {
		c.status.State, c.status.Error = api.Stopped, nil
	}
	return c.stopErr
}

func (c *baselineService) SaveConfig(context.Context, api.Config) error {
	return c.configErr
}

func (c *baselineService) Connections(ctx context.Context, query api.ConnectionQuery) ([]api.Connection, error) {
	c.logQuery = query
	return c.logRows, errors.Join(c.logErr, ctx.Err())
}

func baselineFixture(t *testing.T) (*session, *baselineService, rootcheckOps) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := &session{
		Dir: dir, UID: os.Getuid(), Helper: filepath.Join(dir, "pithd"),
		HelperSHA256: strings.Repeat("a", 64), InstallScriptSHA256: strings.Repeat("b", 64),
		Version: "test", Target: "192.0.2.20:443", ServerName: "example.com",
		Interfaces: [2]api.InterfaceRef{{Name: "example0", HardwareAddr: "02:00:00:00:00:01"}, {Name: "example1", HardwareAddr: "02:00:00:00:00:02"}},
		Original: snapshot{
			BootSession: "test-boot", InterfaceIndices: map[string]int{"example0": 42, "example1": 43},
			At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Interfaces: []api.Interface{
				{Name: "example0", Kind: api.KindEthernet, Up: true, HardwareAddr: "02:00:00:00:00:01", IPv4: []string{"192.0.2.1"}},
				{Name: "example1", Kind: api.KindWiFi, Up: true, HardwareAddr: "02:00:00:00:00:02", IPv4: []string{"198.51.100.1"}},
			},
			Routes: []capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "example0", Index: 42}},
		},
		BaselineLabel: "initial", BaselineFile: "original.json",
		Installation: &installationID{Device: 1, Inode: 2, Modified: 3, Size: 4},
		CancelPassed: true, Installed: true, SmokePassed: true, AuthorizationAttempted: true,
		Ownership: captureHistory{Complete: true},
		Preflight: []preflight{
			{Interface: "", TLS: tlsEvidence{StatusCode: 200, BodyBytes: 12, PublicIP: "192.0.2.1"}},
			{Interface: "example0", TLS: tlsEvidence{StatusCode: 200, BodyBytes: 12, PublicIP: "192.0.2.1"}},
			{Interface: "example1", TLS: tlsEvidence{StatusCode: 200, BodyBytes: 12, PublicIP: "198.51.100.1"}},
		},
	}
	c := &baselineService{status: api.Status{State: api.Stopped, Helper: api.HelperStatus{Installed: true, Running: true, Version: s.Version}}}
	ops := liveOps()
	ops.verifyInstalled = func(*session) error { return nil }
	ops.snapshot = func(context.Context) (snapshot, error) { return s.Original, nil }
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
		t.Fatal("unexpected probe: every test must install its own synthetic probe")
		return tlsEvidence{}, errors.New("unexpected probe")
	}
	for name, data := range map[string]any{"original.json": s.Original, "preflight.json": s.Preflight, "older-check.json": "preserve this report"} {
		if err := writeJSON(filepath.Join(s.Dir, name), data); err != nil {
			t.Fatal(err)
		}
	}
	proof := cleanupRecord{At: time.Now(), Operation: "stop", Label: s.BaselineLabel, Binding: s.cleanupBinding(), Status: c.status, After: s.Original, Restored: true}
	if err := s.recordCleanup(proof, nil, ops); err != nil {
		t.Fatal(err)
	}
	return s, c, ops
}

func cloneSnapshot(in snapshot) snapshot {
	out := in
	out.Routes = slices.Clone(in.Routes)
	out.Interfaces = slices.Clone(in.Interfaces)
	if in.InterfaceIndices != nil {
		out.InterfaceIndices = make(map[string]int, len(in.InterfaceIndices))
		for name, index := range in.InterfaceIndices {
			out.InterfaceIndices[name] = index
		}
	}
	if in.InterfaceAddresses != nil {
		out.InterfaceAddresses = make(map[string][]netip.Prefix, len(in.InterfaceAddresses))
		for name, addresses := range in.InterfaceAddresses {
			out.InterfaceAddresses[name] = slices.Clone(addresses)
		}
	}
	return out
}

func nextMode(s *session) snapshot {
	next := cloneSnapshot(s.Original)
	next.At = next.At.Add(time.Hour)
	next.Routes[0].Interface, next.Routes[0].Index = "utun42", 84
	next.Interfaces = append(next.Interfaces, api.Interface{Name: "utun42", Up: true, Kind: api.KindVPN, IPv4: []string{"203.0.113.1"}})
	next.InterfaceIndices["utun42"] = 84
	return next
}

func privateArtifacts(t *testing.T, s *session) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = string(data)
	}
	return result
}

func assertArtifactsUnchanged(t *testing.T, s *session, old map[string]string, includeSession bool) {
	t.Helper()
	current := privateArtifacts(t, s)
	for name, before := range old {
		if name == "session.json" && !includeSession {
			continue
		}
		if current[name] != before {
			t.Errorf("previous artifact was changed or removed: %s", name)
		}
	}
}

func assertNoCleanup(t *testing.T, s *session) {
	t.Helper()
	loaded, err := loadSession(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Cleanup != nil || s.Cleanup != nil {
		t.Fatal("failed or active operation retained successful cleanup evidence")
	}
}

func TestRebaselineAdoptsStableNewModeWithoutCapture(t *testing.T) {
	s, c, ops := baselineFixture(t)
	s.DNSPassed = true
	s.QUICPassed = true
	s.BundlePassed = true
	old := privateArtifacts(t, s)
	previous := s.baselineDigest()
	next := nextMode(s)
	// Renamed adapters still resolve by their unique reviewed hardware address.
	next.Interfaces[0].Name = "renamed0"
	var calls []string
	verifyCalls, snapshotCalls := 0, 0
	ops.verifyInstalled = func(*session) error { verifyCalls++; return nil }
	ops.snapshot = func(ctx context.Context) (snapshot, error) {
		snapshotCalls++
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("snapshot has no deadline")
		}
		return next, nil
	}
	ops.probe = func(ctx context.Context, current *session, iface string) (tlsEvidence, error) {
		calls = append(calls, iface)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second || time.Until(deadline) <= 0 {
			t.Fatal("probe is not bounded to eight seconds")
		}
		if current.Target != "192.0.2.20:443" || current.ServerName != "example.com" || current.baselineDigest() != previous {
			t.Fatal("probe changed frozen target or prematurely adopted the new baseline")
		}
		return tlsEvidence{StatusCode: 200, BodyBytes: 12, PublicIP: "203.0.113.2"}, nil
	}
	if err := rebaselineWithOps(context.Background(), s, c, "exit-node", ops); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"", "renamed0", "example1"}) || snapshotCalls != 2 || verifyCalls != 2 || c.statuses != 2 || c.starts != 0 || c.stops != 0 {
		t.Fatalf("unexpected operations: probes=%v snapshots=%d verify=%d status=%d start=%d stop=%d", calls, snapshotCalls, verifyCalls, c.statuses, c.starts, c.stops)
	}
	loaded, err := loadSession(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaselineLabel != "exit-node" || loaded.BaselineFile == "original.json" || !sameRoutes(loaded.Original.Routes, next.Routes) || loaded.Interfaces[0].Name != "renamed0" || loaded.SmokePassed || loaded.DNSPassed || loaded.QUICPassed || loaded.BundlePassed || loaded.Cleanup != nil || loaded.baselineIP("") != "203.0.113.2" {
		t.Fatalf("new phase was not adopted or pass flags survived: %#v", loaded)
	}
	assertArtifactsUnchanged(t, s, old, false)
	if err := checkWithOps(context.Background(), s, nil, checkOptions{batch: true}, ops); err == nil || !strings.Contains(err.Error(), "smoke") {
		t.Fatalf("new phase bypassed its smoke prerequisite: %v", err)
	}
	if err := rebaselineWithOps(context.Background(), s, c, "tailnet", ops); err == nil || !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("new phase reused previous cleanup evidence: %v", err)
	}
}

func TestRebaselineRejectsInvalidPrerequisitesBeforeProbe(t *testing.T) {
	tests := []struct {
		name   string
		change func(*session, *baselineService, *rootcheckOps)
	}{
		{"not-installed", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Installed = false }},
		{"no-cancel", func(s *session, _ *baselineService, _ *rootcheckOps) { s.CancelPassed = false }},
		{"no-installation-identity", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Installation = nil }},
		{"different-helper", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			ops.verifyInstalled = func(*session) error { return errors.New("different helper") }
		}},
		{"outdated", func(_ *session, c *baselineService, _ *rootcheckOps) { c.status.Helper.Outdated = true }},
		{"not-responding", func(_ *session, c *baselineService, _ *rootcheckOps) { c.status.Helper.Running = false }},
		{"not-stopped", func(_ *session, c *baselineService, _ *rootcheckOps) { c.status.State = api.Running }},
		{"ipc-error", func(_ *session, c *baselineService, _ *rootcheckOps) { c.statusErr = errors.New("IPC failed") }},
		{"no-cleanup", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Cleanup = nil }},
		{"different-uid", func(s *session, _ *baselineService, _ *rootcheckOps) { s.UID++ }},
		{"different-plan", func(s *session, _ *baselineService, _ *rootcheckOps) { s.InstallScriptSHA256 = strings.Repeat("c", 64) }},
		{"different-installation", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Installation.Inode++ }},
		{"different-baseline", func(s *session, _ *baselineService, _ *rootcheckOps) { s.BaselineLabel = "other" }},
		{"changed-evidence", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Cleanup.SHA256 = strings.Repeat("0", 64) }},
		{"evidence-path-escape", func(s *session, _ *baselineService, _ *rootcheckOps) { s.Cleanup.File = "../cleanup-other.json" }},
		{"interface-down", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			original := ops.snapshot
			ops.snapshot = func(ctx context.Context) (snapshot, error) {
				shot, err := original(ctx)
				shot = cloneSnapshot(shot)
				shot.Interfaces[1].Up = false
				return shot, err
			}
		}},
		{"interface-missing", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			original := ops.snapshot
			ops.snapshot = func(ctx context.Context) (snapshot, error) {
				shot, err := original(ctx)
				shot.Interfaces = shot.Interfaces[:1]
				return shot, err
			}
		}},
		{"interface-mac-replaced", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			original := ops.snapshot
			ops.snapshot = func(ctx context.Context) (snapshot, error) {
				shot, err := original(ctx)
				shot = cloneSnapshot(shot)
				shot.Interfaces[1].HardwareAddr = "02:00:00:00:00:03"
				return shot, err
			}
		}},
		{"no-system-route", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			original := ops.snapshot
			ops.snapshot = func(ctx context.Context) (snapshot, error) {
				shot, err := original(ctx)
				shot.Routes = nil
				return shot, err
			}
		}},
		{"host-route-conflict", func(_ *session, _ *baselineService, ops *rootcheckOps) {
			original := ops.snapshot
			ops.snapshot = func(ctx context.Context) (snapshot, error) {
				shot, err := original(ctx)
				shot.Routes = append(slices.Clone(shot.Routes), capture.Route{Prefix: netip.MustParsePrefix("192.0.2.20/32"), Interface: "utun42"})
				return shot, err
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			old := privateArtifacts(t, s)
			test.change(s, c, &ops)
			if err := rebaselineWithOps(context.Background(), s, c, "tailnet", ops); err == nil {
				t.Fatal("accepted invalid rebaseline prerequisites")
			}
			if c.starts != 0 || c.stops != 0 {
				t.Fatal("rebaseline changed helper routing")
			}
			assertArtifactsUnchanged(t, s, old, true)
		})
	}
	for _, label := range []string{"", "../exit-node", "VPN", "mode name", "a\nmode", "1mode", strings.Repeat("x", 41)} {
		t.Run("label-"+label, func(t *testing.T) {
			if err := rebaselineWithOps(context.Background(), nil, nil, label, rootcheckOps{}); err == nil {
				t.Fatal("accepted invalid label")
			}
		})
	}
}

func TestRebaselineFailureKeepsPreviousBaselineAndHistory(t *testing.T) {
	for _, failure := range []string{"before-snapshot", "probe-system", "probe-a", "probe-b", "after-snapshot", "changed-route", "changed-interface", "changed-helper", "changed-artifact", "not-stopped-after", "archive", "save", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			old := privateArtifacts(t, s)
			beforeDigest := s.baselineDigest()
			next := nextMode(s)
			snapshots, probes, verifies := 0, 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ops.snapshot = func(context.Context) (snapshot, error) {
				snapshots++
				if failure == "before-snapshot" || snapshots == 2 && failure == "after-snapshot" {
					return snapshot{}, errors.New("snapshot failed")
				}
				shot := cloneSnapshot(next)
				if snapshots == 2 && failure == "changed-route" {
					shot.Routes[0].Index++
				}
				if snapshots == 2 && failure == "changed-interface" {
					shot.Interfaces[0].Up = false
				}
				return shot, nil
			}
			ops.probe = func(context.Context, *session, string) (tlsEvidence, error) {
				probes++
				if failure == "probe-system" && probes == 1 || failure == "probe-a" && probes == 2 || failure == "probe-b" && probes == 3 {
					return tlsEvidence{}, errors.New("probe failed")
				}
				if failure == "canceled" {
					cancel()
				}
				return tlsEvidence{StatusCode: 200, BodyBytes: 12}, nil
			}
			ops.verifyInstalled = func(*session) error {
				verifies++
				if verifies == 2 && failure == "changed-helper" {
					return errors.New("installation changed")
				}
				return nil
			}
			c.onStatus = func(call int, st *api.Status) error {
				if call == 2 && failure == "changed-artifact" {
					st.Helper.Outdated = true
				}
				if call == 2 && failure == "not-stopped-after" {
					st.State = api.Running
				}
				return nil
			}
			if failure == "archive" {
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("archive failed") }
			}
			if failure == "save" {
				ops.save = func(*session) error { return errors.New("session write failed") }
			}
			if err := rebaselineWithOps(ctx, s, c, "exit-node", ops); err == nil {
				t.Fatal("injected failure was accepted")
			}
			if s.baselineDigest() != beforeDigest || !s.SmokePassed || s.Cleanup == nil || c.starts != 0 || c.stops != 0 || probes > 3 {
				t.Fatal("failure changed the active baseline, pass flags, or routing")
			}
			assertArtifactsUnchanged(t, s, old, true)
		})
	}
}

func TestCaptureInvalidatesCleanupBeforeEveryStart(t *testing.T) {
	for _, startFails := range []bool{false, true} {
		s, c, ops := baselineFixture(t)
		if startFails {
			c.startErr = errors.New("Start failed")
		}
		c.onStart = func() { assertNoCleanup(t, s) }
		old := privateArtifacts(t, s)
		for range 2 {
			c.status.State = api.Stopped
			err := s.startCapture(context.Background(), c, "test", ops)
			if (err != nil) != startFails {
				t.Fatalf("Start error = %v", err)
			}
			assertNoCleanup(t, s)
		}
		assertArtifactsUnchanged(t, s, old, false)
	}
	s, c, ops := baselineFixture(t)
	ops.save = func(*session) error { return errors.New("cannot persist") }
	if err := s.startCapture(context.Background(), c, "test", ops); err == nil || c.starts != 0 {
		t.Fatal("Start ran without durable invalidation")
	}
}

func TestStopUsesCurrentBaselineAndInvalidatesFailedProof(t *testing.T) {
	for _, failure := range []string{"", "verify", "stop", "status", "artifact", "not-stopped", "snapshot", "routes", "tunnels", "archive", "save"} {
		t.Run(failure, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			// Simulate an already committed newer mode. The initial file is retained
			// but must not be used to judge cleanup in the current phase.
			s.Original, s.BaselineLabel = nextMode(s), "exit-node"
			if err := s.save(); err != nil {
				t.Fatal(err)
			}
			c.onStop = func() { assertNoCleanup(t, s) }
			saveCalls := 0
			ops.save = func(s *session) error {
				saveCalls++
				if failure == "save" && saveCalls > 1 {
					return errors.New("save failed")
				}
				return s.save()
			}
			if failure == "verify" {
				ops.verifyInstalled = func(*session) error { return errors.New("different installation") }
			}
			if failure == "stop" {
				c.stopErr = errors.New("stop failed")
			}
			if failure == "status" {
				c.statusErr = errors.New("status failed")
			}
			if failure == "artifact" {
				c.status.Helper.Outdated = true
			}
			if failure == "not-stopped" {
				c.onStatus = func(_ int, st *api.Status) error { st.State = api.Running; return nil }
			}
			if failure == "archive" {
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("archive failed") }
			}
			ops.snapshot = func(context.Context) (snapshot, error) {
				if failure == "snapshot" {
					return snapshot{}, errors.New("snapshot failed")
				}
				after := cloneSnapshot(s.Original)
				if failure == "routes" {
					after.Routes[0].Index++
				}
				if failure == "tunnels" {
					after.Interfaces = append(after.Interfaces, api.Interface{Name: "utun43"})
				}
				return after, nil
			}
			err := stopWithOps(context.Background(), s, c, ops)
			if (err != nil) != (failure != "") {
				t.Fatalf("Stop error = %v", err)
			}
			if failure != "" {
				assertNoCleanup(t, s)
				if failure == "verify" && c.stops != 0 {
					t.Fatal("stopped an unverified helper")
				}
			} else {
				if err := s.requireCleanup(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCheckTracksRollbackSeparatelyFromAssertions(t *testing.T) {
	for _, failure := range []string{"", "start", "config", "verify", "before-snapshot", "before-routes", "stop", "after-snapshot", "after-routes", "archive", "save"} {
		t.Run(failure, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			c.startErr = &api.Error{Code: api.ErrNetworkLost}
			c.onStart, c.onStop = func() { assertNoCleanup(t, s) }, func() { assertNoCleanup(t, s) }
			if failure == "start" {
				c.startErr = errors.New("unexpected Start failure")
			}
			if failure == "config" {
				c.configErr = errors.New("config save failed")
			}
			if failure == "stop" {
				c.stopErr = errors.New("stop failed")
			}
			if failure == "verify" {
				ops.verifyInstalled = func(*session) error { return errors.New("different installation") }
			}
			snapshots := 0
			ops.snapshot = func(context.Context) (snapshot, error) {
				snapshots++
				if snapshots == 1 && failure == "before-snapshot" || snapshots == 2 && failure == "after-snapshot" {
					return snapshot{}, errors.New("snapshot failed")
				}
				shot := cloneSnapshot(s.Original)
				if snapshots == 1 && failure == "before-routes" || snapshots == 2 && failure == "after-routes" {
					shot.Routes[0].Index++
				}
				return shot, nil
			}
			if failure == "archive" {
				ops.archive = func(*session, string, any) (string, error) { return "", errors.New("report failed") }
			}
			if failure == "save" {
				ops.save = func(s *session) error {
					if s.Cleanup != nil {
						return errors.New("proof save failed")
					}
					return s.save()
				}
			}
			err := checkWithOps(context.Background(), s, c, checkOptions{safeFailure: true}, ops)
			if (err != nil) != (failure != "") {
				t.Fatalf("check error = %v", err)
			}
			cleanupFailed := slices.Contains([]string{"stop", "after-snapshot", "after-routes", "archive", "save"}, failure)
			if cleanupFailed {
				assertNoCleanup(t, s)
			} else if err := s.requireCleanup(); err != nil {
				t.Fatal(err)
			}
			if c.starts > 0 && c.stops != 1 {
				t.Fatalf("Start was not followed by exactly one unconditional Stop: %d", c.stops)
			}
			if failure == "config" && (c.starts != 0 || c.stops != 1) {
				t.Fatal("setup failure skipped bounded cleanup")
			}
		})
	}
}

func TestStartRechecksInstallationAndIdleIdentityBeforeIPC(t *testing.T) {
	for _, failure := range []string{"installation", "running", "artifact", "status"} {
		t.Run(failure, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			if failure == "installation" {
				ops.verifyInstalled = func(*session) error { return errors.New("installation replaced") }
			}
			if failure == "running" {
				c.status.State = api.Running
			}
			if failure == "artifact" {
				c.status.Helper.Outdated = true
			}
			if failure == "status" {
				c.statusErr = errors.New("IPC failed")
			}
			if err := s.startCapture(context.Background(), c, "test", ops); err == nil || c.starts != 0 {
				t.Fatal("Start reached an unverified or non-idle helper")
			}
			assertNoCleanup(t, s)
		})
	}
}

func TestCheckDoesNotStartOrStopAReplacedInstallation(t *testing.T) {
	for _, replacedAt := range []string{"before-start", "before-cleanup"} {
		t.Run(replacedAt, func(t *testing.T) {
			s, c, ops := baselineFixture(t)
			c.startErr = &api.Error{Code: api.ErrNetworkLost}
			verifies := 0
			ops.verifyInstalled = func(*session) error {
				verifies++
				if verifies >= 2 && replacedAt == "before-start" || verifies >= 3 && replacedAt == "before-cleanup" {
					return errors.New("installed helper was replaced")
				}
				return nil
			}
			if err := checkWithOps(context.Background(), s, c, checkOptions{safeFailure: true}, ops); err == nil {
				t.Fatal("check accepted a replaced helper")
			}
			if c.stops != 0 || replacedAt == "before-start" && c.starts != 0 || replacedAt == "before-cleanup" && c.starts != 1 {
				t.Fatalf("replaced helper received a mutation: start=%d stop=%d", c.starts, c.stops)
			}
			assertNoCleanup(t, s)
		})
	}
}

func TestCheckUsesCurrentBaselineAndPreservesProofForReadOnlyRejection(t *testing.T) {
	s, c, ops := baselineFixture(t)
	oldProof := *s.Cleanup
	next := nextMode(s)
	ops.snapshot = func(context.Context) (snapshot, error) { return next, nil }
	if err := checkWithOps(context.Background(), s, c, checkOptions{}, ops); err == nil {
		t.Fatal("check accepted a changed mode without rebaseline")
	}
	if c.starts != 0 || c.stops != 0 || s.Cleanup == nil || *s.Cleanup != oldProof {
		t.Fatal("read-only rejection invalidated the previous cleanup proof")
	}
	if err := s.requireCleanup(); err != nil {
		t.Fatal(err)
	}
	// Rebaseline is permitted after that rejection and requires a fresh smoke.
	ops.probe = func(context.Context, *session, string) (tlsEvidence, error) { return tlsEvidence{StatusCode: 200}, nil }
	if err := rebaselineWithOps(context.Background(), s, c, "tailnet", ops); err != nil {
		t.Fatal(err)
	}
	// Set only the smoke gate to exercise the safe-failure branch without TLS.
	s.SmokePassed = true
	c.startErr = &api.Error{Code: api.ErrNetworkLost}
	if err := checkWithOps(context.Background(), s, c, checkOptions{safeFailure: true}, ops); err != nil {
		t.Fatal(err)
	}
	if err := s.requireCleanup(); err != nil {
		t.Fatal(err)
	}
	if sameRoutes(s.Original.Routes, []capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "example0", Index: 42}}) {
		t.Fatal("check fell back to the initial baseline")
	}
}

func TestInstallationIdentityRejectsReplacementAndAllowsPartialRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "helper.plist")
	if err := os.WriteFile(path, []byte("reviewed plist"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := readInstallationID(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{Installation: &id}
	if err := s.verifyInstallationID(path, false); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.plist")
	if err := os.WriteFile(replacement, []byte("reviewed plist"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyInstallationID(path, true); err == nil {
		t.Fatal("identical replacement plist reused another installation's evidence")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyInstallationID(path, false); err == nil {
		t.Fatal("missing installation accepted for a test")
	}
	if err := s.verifyInstallationID(path, true); err != nil {
		t.Fatalf("partial uninstall cannot recover: %v", err)
	}
	if err := os.WriteFile(replacement, []byte("reviewed plist"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyInstallationID(path, true); err == nil {
		t.Fatal("symlink accepted as owned installation")
	}
}

func TestCleanupProofRejectsCopiedSessionAndChangedEvidence(t *testing.T) {
	s, _, _ := baselineFixture(t)
	if err := s.requireCleanup(); err != nil {
		t.Fatal(err)
	}
	other, _, _ := baselineFixture(t)
	data, err := os.ReadFile(filepath.Join(s.Dir, s.Cleanup.File))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other.Dir, s.Cleanup.File), data, 0600); err != nil {
		t.Fatal(err)
	}
	copyProof := *s.Cleanup
	other.Cleanup = &copyProof
	if err := other.requireCleanup(); err == nil {
		t.Fatal("copied proof accepted in another session directory")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, s.Cleanup.File), append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.requireCleanup(); err == nil {
		t.Fatal("modified cleanup artifact accepted")
	}
}

func TestRemovalOwnershipRetainsOfflinePartialRecovery(t *testing.T) {
	for _, variant := range []string{"matching", "missing-binary", "missing-plist", "missing-both", "wrong-binary", "wrong-plist", "symlink", "different-instance"} {
		t.Run(variant, func(t *testing.T) {
			s, _, _ := baselineFixture(t)
			s.Installation = nil
			binary, plistPath := filepath.Join(s.Dir, "installed-helper"), filepath.Join(s.Dir, "installed.plist")
			data := []byte("reviewed executable bytes")
			s.HelperSHA256 = hashBytes(data)
			if variant == "wrong-binary" {
				data = []byte("another executable")
			}
			if !slices.Contains([]string{"missing-binary", "missing-both"}, variant) {
				if err := os.WriteFile(binary, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			launch := struct {
				Label            string
				ProgramArguments []string
			}{
				Label:            helper.Label,
				ProgramArguments: []string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID), "--safe-target", s.Target, "--safe-server-name", s.ServerName, "--capture", "192.0.2.20/32"},
			}
			if variant == "wrong-plist" {
				launch.ProgramArguments = []string{helper.BinaryPath, "--owner-uid", strconv.Itoa(s.UID)}
			}
			encoded, err := plist.Marshal(launch, plist.XMLFormat)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains([]string{"missing-plist", "missing-both"}, variant) {
				if err := os.WriteFile(plistPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "symlink" {
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(plistPath, binary); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "different-instance" {
				s.Installation = &installationID{}
			}
			// Removal deliberately needs no IPC or matching app/helper version.
			s.Version = "different-client-version"
			err = s.verifyRemovalOwnership(binary, plistPath)
			wantErr := slices.Contains([]string{"wrong-binary", "wrong-plist", "symlink", "different-instance"}, variant)
			if (err != nil) != wantErr {
				t.Fatalf("removal ownership: %v", err)
			}
		})
	}
}

func TestPostUninstallComparisonUsesLatestBaselineAndArchivesEveryAttempt(t *testing.T) {
	s, _, ops := baselineFixture(t)
	initial := s.Original
	s.Original, s.BaselineLabel = nextMode(s), "exit-node"
	clean := cloneSnapshot(s.Original)
	clean.LaunchExit = 113
	clean.Paths = absentPathStates()
	ops.snapshot = func(context.Context) (snapshot, error) { return clean, nil }
	old := privateArtifacts(t, s)
	for range 2 {
		if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err != nil {
			t.Fatal(err)
		}
	}
	assertArtifactsUnchanged(t, s, old, true)
	files, err := filepath.Glob(filepath.Join(s.Dir, "after-uninstall-*.json"))
	if err != nil || len(files) != 2 {
		t.Fatalf("repeated snapshot overwrote history: %v, %v", files, err)
	}
	initial.LaunchExit = 113
	initial.Paths = absentPathStates()
	ops.snapshot = func(context.Context) (snapshot, error) { return initial, nil }
	if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err == nil {
		t.Fatal("uninstall accepted initial routes instead of latest mode")
	}
	clean.Paths[0].Present = true
	ops.snapshot = func(context.Context) (snapshot, error) { return clean, nil }
	if err := s.expectOriginalWithOps(context.Background(), "after-uninstall", ops); err == nil {
		t.Fatal("uninstall accepted a surviving managed path")
	}
}
