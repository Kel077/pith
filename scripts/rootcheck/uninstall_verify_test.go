package main

import (
	"context"
	"errors"
	"testing"
)

func TestVerifyUninstalledAfterExternalNetworkRestoration(t *testing.T) {
	s, _, ops := baselineFixture(t)
	s.DNSPassed, s.QUICPassed, s.BundlePassed = true, true, true
	clean := cloneSnapshot(s.Original)
	clean.Paths, clean.LaunchExit = absentPathStates(), 113
	down := cloneSnapshot(clean)
	down.Routes = nil
	ops.snapshot = func(context.Context) (snapshot, error) { return down, nil }
	before := privateArtifacts(t, s)
	if err := verifyUninstalledWithOps(context.Background(), s, ops); err == nil || !s.Installed {
		t.Fatal("changed routes accepted as restored", err, s.Installed)
	}
	ops.snapshot = func(context.Context) (snapshot, error) { return clean, nil }
	if err := verifyUninstalledWithOps(context.Background(), s, ops); err != nil {
		t.Fatal(err)
	}
	if s.Installed || s.SmokePassed || s.DNSPassed || s.QUICPassed || s.BundlePassed || s.Cleanup != nil {
		t.Fatal("stale installed state or cleanup proof retained")
	}
	if len(privateArtifacts(t, s)) != len(before)+2 {
		t.Fatal("both failed and successful read-only verification must be archived")
	}
}

func TestVerifyUninstalledRequiresAbsentPathsAndService(t *testing.T) {
	for _, kind := range []string{"path", "service", "snapshot-error", "unattempted"} {
		t.Run(kind, func(t *testing.T) {
			s, _, ops := baselineFixture(t)
			after := cloneSnapshot(s.Original)
			after.Paths, after.LaunchExit = absentPathStates(), 113
			var readErr error
			switch kind {
			case "path":
				after.Paths[0].Present = true
			case "service":
				after.LaunchExit = 0
			case "snapshot-error":
				readErr = errors.New("snapshot unavailable")
			case "unattempted":
				s.AuthorizationAttempted = false
			}
			ops.snapshot = func(context.Context) (snapshot, error) { return after, readErr }
			ops.save = func(*session) error { t.Fatal("failed verification changed phase state"); return nil }
			if err := verifyUninstalledWithOps(context.Background(), s, ops); err == nil || !s.Installed {
				t.Fatal("incomplete removal accepted", kind, err)
			}
		})
	}
}
