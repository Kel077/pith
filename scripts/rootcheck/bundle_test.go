package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/testprofile"
)

func TestBundleChecksOuterIdentityControlAndBlock(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			s, c, cfg := appFixture()
			c.capture = &api.CaptureIdentity{Token: "synthetic"}
			running, _ := c.Status(context.Background())
			f := fixturePaths("/tmp/pith-example/bundle-fixture-synthetic")
			calls := 0
			request := func(path string) proofRequest {
				return func(ctx context.Context) (requestProof, func() error, error) {
					calls++
					app := f.App
					if path == f.Control {
						app = api.AppRef{Path: path}
					}
					return c.requesterWithApp(path, 9000+calls, app)(ctx)
				}
			}
			var report checkReport
			if err := runBundleProofs(context.Background(), s, c, cfg, running, "utun42", &report, batch, f, request); err != nil {
				t.Fatal(err, report)
			}
			want := 1
			if batch {
				want = 6
			}
			if calls != want || len(report.Cases) != want || c.closed != want {
				t.Fatal("missing requests, cases or reaped children", calls, c.closed, report)
			}
			if batch {
				for _, index := range []int{1, 5} {
					if report.Cases[index].Connection.Outcome != api.System || report.Cases[index].Connection.App.BundleID != "" {
						t.Fatal("outside control inherited bundle policy")
					}
				}
				if report.Cases[4].Connection.Outcome != api.Blocked || report.Cases[4].Connection.App.BundleID != bundleIdentifier {
					t.Fatal("nested helper escaped parent block")
				}
			}
		})
	}
}

func TestBundleProofRejectsWrongOuterIdentityAndTuple(t *testing.T) {
	for _, mode := range []string{"nested-id", "nested-path", "missing-tuple", "wrong-tuple"} {
		t.Run(mode, func(t *testing.T) {
			s, c, cfg := appFixture()
			c.capture = &api.CaptureIdentity{Token: "synthetic"}
			status, _ := c.Status(context.Background())
			f := fixturePaths("/tmp/pith-example/bundle-fixture-synthetic")
			c.mutate = func(row *api.Connection, proof *requestProof) {
				switch mode {
				case "nested-id":
					row.App.BundleID += ".helper"
				case "nested-path":
					row.App.Path = filepath.Dir(filepath.Dir(filepath.Dir(f.Helper)))
				case "missing-tuple":
					proof.TLS.Local = ""
				case "wrong-tuple":
					row.Local = "192.0.2.10:50001"
				}
			}
			var report checkReport
			err := runBundleProofs(context.Background(), s, c, cfg, status, "utun42", &report, true, f, func(path string) proofRequest { return c.requesterWithApp(path, 9001, f.App) })
			if err == nil || len(report.Cases) != 1 || c.closed != 1 {
				t.Fatal("incorrect evidence did not fail first and reap child", err, report)
			}
		})
	}
}

func TestBundlePlanAdmissionAndFixtureIntegrity(t *testing.T) {
	s, c, ops := baselineFixture(t)
	if err := s.writeBundlePlan(); err != nil {
		t.Fatal(err)
	}
	if err := s.reviewedBundlePlan(); err != nil {
		t.Fatal(err)
	}
	if err := checkWithOps(context.Background(), s, c, checkOptions{appBundleBatch: true}, ops); err == nil || !strings.Contains(err.Error(), "--app-bundle") {
		t.Fatal("batch missing its smoke gate", err)
	}
	if c.starts != 0 || c.stops != 0 {
		t.Fatal("admission touched helper")
	}
	before := s.cleanupBinding()
	s.BundlePlanSHA256 = strings.Repeat("a", 64)
	if before == s.cleanupBinding() || s.reviewedBundlePlan() == nil {
		t.Fatal("cleanup did not bind bundle plan")
	}
	if err := s.writeBundlePlan(); err != nil {
		t.Fatal(err)
	}
	f, cleanup, err := createBundleFixture(s)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, path := range []string{f.Main, f.Helper, f.Control} {
		app, err := attribution.AppForPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if path == f.Control {
			if app.Path != path || app.BundleID != "" {
				t.Fatal(app)
			}
		} else if app.Path != f.App.Path || app.BundleID != bundleIdentifier {
			t.Fatal("nested metadata overrode outer bundle", app)
		}
	}
	if err := os.WriteFile(f.Helper, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyBundleFixture(s, f.Directory); err == nil {
		t.Fatal("changed helper accepted")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.Directory); !os.IsNotExist(err) {
		t.Fatal("fixture remained", err)
	}
	if _, err := os.Stat(s.Bundle.Tool); err != nil {
		t.Fatal("cleanup removed source")
	}
	s.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4}
	if s.reviewedBundlePlan() == nil {
		t.Fatal("bundle accepted outside reviewed mode")
	}
}

// A copied test executable exercises the real stdin/proof/Wait protocol from
// each fixture location. It performs no networking and never accesses a helper.
func TestBundleProtocolFixture(t *testing.T) {
	if os.Getenv("PITH_BUNDLE_PROTOCOL") != "1" {
		return
	}
	s, err := loadSession(os.Getenv("PITH_BUNDLE_SESSION"))
	if err == nil {
		err = serveRequest(context.Background(), s, os.Stdin, os.Stdout, func(context.Context) (tlsEvidence, error) {
			return tlsEvidence{Local: "192.0.2.10:50000", StatusCode: 200}, nil
		}, os.Getenv("PITH_BUNDLE_DIRECTORY"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestBundleChildrenAreWaitedAndPrivateFixtureRemoved(t *testing.T) {
	s, _, _ := baselineFixture(t)
	if err := s.writeBundlePlan(); err != nil {
		t.Fatal(err)
	}
	f, cleanup, err := createBundleFixture(s)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, path := range []string{f.Main, f.Helper, f.Control} {
		var cmd *exec.Cmd
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		proof, finish, err := requestFromChild(ctx, s, path, func(ctx context.Context, executable, _ string) *exec.Cmd {
			cmd = exec.CommandContext(ctx, executable, "-test.run=^TestBundleProtocolFixture$")
			cmd.Env = []string{"PITH_BUNDLE_PROTOCOL=1", "PITH_BUNDLE_SESSION=" + s.Dir, "PITH_BUNDLE_DIRECTORY=" + f.Directory, "TMPDIR=" + os.TempDir()}
			return cmd
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := finish(); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if cmd.ProcessState == nil || proof.Executable != path || proof.PID != cmd.Process.Pid {
			t.Fatal("child was not correctly identified and reaped")
		}
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.Directory); !os.IsNotExist(err) {
		t.Fatal("fixture remains")
	}
}
