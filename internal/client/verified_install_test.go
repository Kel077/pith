//go:build darwin && arm64

package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/helper/identity"
)

type verifiedInstallFixture struct {
	client    *Client
	bundle    string
	installed string
	plans     *[]helper.InstallPlan
	canceled  *api.Error
}

func recordVerifiedAuthorization(c *Client) (*[]helper.InstallPlan, *api.Error) {
	plans := new([]helper.InstallPlan)
	canceled := &api.Error{Code: api.ErrCanceled, Detail: "test authorization canceled"}
	// Every installer test replaces execution before calling either public method.
	// The recorder cannot open a dialog or execute an installation script.
	c.executePlan = func(_ context.Context, plan helper.InstallPlan) error {
		*plans = append(*plans, plan)
		return canceled
	}
	return plans, canceled
}

func newVerifiedInstallFixture(t *testing.T) verifiedInstallFixture {
	t.Helper()
	// Keep the nonexistent socket path below Darwin's Unix-domain length limit.
	dir, err := os.MkdirTemp("", "pith-install-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	bundle := filepath.Join(dir, "resources", "bin", "pithd")
	installed := filepath.Join(dir, "installed", "pithd")
	contents := []byte("verified installer fixture; never executed\n")
	for _, path := range []string{bundle, installed} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0700); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(contents)
	expected := identity.Identity{Schema: identity.Schema, Protocol: helper.ProtocolVersion, Version: "fixture-helper", SHA256: fmt.Sprintf("%x", digest), CDHash: strings.Repeat("1", 40)}
	c, err := New(Options{ConfigDir: filepath.Join(dir, "config"), ResourcesDir: filepath.Join(dir, "resources"), SocketPath: filepath.Join(dir, "s"), Version: "fixture-app", ExpectedHelper: &expected})
	if err != nil {
		t.Fatal(err)
	}
	plans, canceled := recordVerifiedAuthorization(c)
	c.binaryPath = installed
	// The installed-path trust boundary is simulated for temporary files.
	// Complete byte, regular-file, file-permission and cache checks remain active.
	c.checkFile = func(path string, _ bool) error {
		return c.artifacts.check(path, false, c.expectedHelper, c.artifactOpen)
	}
	return verifiedInstallFixture{client: c, bundle: bundle, installed: installed, plans: plans, canceled: canceled}
}

func TestVerifiedInstallIgnoresInformationalVersions(t *testing.T) {
	c, _, _ := startLocalHelper(t, "unrelated-peer-display-version", newFakeEngine())
	plans, _ := recordVerifiedAuthorization(c)
	for _, version := range []string{"0.0.1", "999.0.0", "dev-other"} {
		c.opts.Version = version
		if err := c.InstallHelper(context.Background()); err != nil {
			t.Fatalf("app version %q requested replacement: %v", version, err)
		}
	}
	if len(*plans) != 0 {
		t.Fatal("matching helper artifact reached authorization after an app version change")
	}
}

func TestVerifiedInstallOfflineMatchingArtifactOnlyRetries(t *testing.T) {
	f := newVerifiedInstallFixture(t)
	status, err := f.client.Status(context.Background())
	if err != nil || !status.Helper.Installed || status.Helper.Running || status.Helper.Outdated || status.Error != nil {
		t.Fatalf("matching offline artifact status = %+v, %v", status, err)
	}
	for range 2 {
		err := f.client.InstallHelper(context.Background())
		if api.Code(err) != api.ErrInternal || !strings.Contains(err.Error(), "retry") {
			t.Fatalf("offline matching helper should request a retry: %v", err)
		}
	}
	if len(*f.plans) != 0 {
		t.Fatal("offline matching helper reached authorization")
	}
}

func TestVerifiedInstallRejectsInvalidBundleBeforeAuthorization(t *testing.T) {
	for _, failure := range []string{"identity", "changed-bytes", "writable", "symlink", "missing"} {
		t.Run(failure, func(t *testing.T) {
			f := newVerifiedInstallFixture(t)
			switch failure {
			case "identity":
				f.client.identityErr = errors.New("missing compiled helper identity")
			case "changed-bytes":
				if err := os.WriteFile(f.bundle, []byte("different bundle bytes"), 0700); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(f.bundle, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(f.bundle); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.installed, f.bundle); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(f.bundle); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.InstallHelper(context.Background()); api.Code(err) != api.ErrHelperUntrusted {
				t.Fatalf("invalid bundle was not rejected: %v", err)
			}
			if len(*f.plans) != 0 {
				t.Fatal("invalid bundle reached authorization")
			}
		})
	}
}

func TestVerifiedInstallAllowsExplicitReplacementOfRejectedHelper(t *testing.T) {
	for _, failure := range []string{"outdated-artifact", "untrusted-metadata", "untrusted-peer"} {
		t.Run(failure, func(t *testing.T) {
			var c *Client
			if failure == "untrusted-peer" {
				c, _, _ = startLocalHelper(t, "old-helper", newFakeEngine())
				c.expectedHelper.CDHash = strings.Repeat("1", 40)
			} else {
				f := newVerifiedInstallFixture(t)
				c = f.client
				checkFile := c.checkFile
				c.checkFile = func(path string, installed bool) error {
					if installed {
						if failure == "outdated-artifact" {
							return errArtifactMismatch
						}
						return errors.New("installed helper is not root-owned")
					}
					return checkFile(path, installed)
				}
			}
			plans, canceled := recordVerifiedAuthorization(c)
			c.opts.Prompts.Update = "Review the verified helper replacement."
			status, err := c.Status(context.Background())
			wantCode := api.ErrHelperUntrusted
			if failure == "outdated-artifact" {
				wantCode = api.ErrHelperOutdated
			}
			if err != nil || status.Helper.Running || status.Helper.Version != "" || status.Error == nil || api.Code(status.Error) != wantCode {
				t.Fatalf("rejected helper status = %+v, %v", status, err)
			}
			if err := c.InstallHelper(context.Background()); err != canceled {
				t.Fatalf("explicit replacement did not preserve authorization cancellation: %v", err)
			}
			if len(*plans) != 1 || (*plans)[0].Prompt != c.opts.Prompts.Update || !strings.Contains((*plans)[0].Script, "\"$stage/new-helper\" --cleanup") {
				t.Fatalf("expected one verified update plan, got %+v", *plans)
			}
		})
	}
}

func TestVerifiedUninstallUsesTrustedBundleDespiteRejectedPeer(t *testing.T) {
	c, _, _ := startLocalHelper(t, "untrusted-helper", newFakeEngine())
	plans, canceled := recordVerifiedAuthorization(c)
	c.expectedHelper.CDHash = strings.Repeat("1", 40)
	c.opts.Prompts.Uninstall = "Review verified helper removal."
	status, err := c.Status(context.Background())
	if err != nil || status.Error == nil || api.Code(status.Error) != api.ErrHelperUntrusted {
		t.Fatalf("expected rejected peer before removal: %+v, %v", status, err)
	}
	if err := c.UninstallHelper(context.Background()); err != canceled {
		t.Fatalf("removal did not preserve authorization cancellation: %v", err)
	}
	if len(*plans) != 1 || (*plans)[0].Prompt != c.opts.Prompts.Uninstall || !strings.Contains((*plans)[0].Script, "\"$stage/cleanup-helper\" --cleanup") {
		t.Fatalf("expected one staged cleanup plan, got %+v", *plans)
	}
}

func TestVerifiedUninstallFallsBackOnlyToVerifiedInstalledBytes(t *testing.T) {
	for _, installed := range []string{"verified", "different-bytes", "writable", "symlink", "missing", "changed-after-check"} {
		t.Run(installed, func(t *testing.T) {
			f := newVerifiedInstallFixture(t)
			if err := os.WriteFile(f.bundle, []byte("damaged bundle"), 0700); err != nil {
				t.Fatal(err)
			}
			switch installed {
			case "different-bytes":
				if err := os.WriteFile(f.installed, []byte("different installed helper"), 0700); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(f.installed, 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := f.installed + ".original"
				if err := os.Rename(f.installed, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, f.installed); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(f.installed); err != nil {
					t.Fatal(err)
				}
			}
			var installedChecks []string
			checkFile := f.client.checkFile
			f.client.checkFile = func(path string, isInstalled bool) error {
				if isInstalled {
					installedChecks = append(installedChecks, path)
				}
				err := checkFile(path, isInstalled)
				if err == nil && isInstalled && installed == "changed-after-check" {
					return os.WriteFile(path, []byte("changed after verification"), 0700)
				}
				return err
			}
			err := f.client.UninstallHelper(context.Background())
			if !slices.Equal(installedChecks, []string{f.installed}) {
				t.Fatalf("fallback did not verify the installed file: %v", installedChecks)
			}
			if installed != "verified" {
				if api.Code(err) != api.ErrHelperUntrusted || len(*f.plans) != 0 {
					t.Fatalf("untrusted cleanup source reached authorization: error=%v plans=%d", err, len(*f.plans))
				}
				return
			}
			if err != f.canceled || len(*f.plans) != 1 {
				t.Fatalf("verified fallback did not reach one canceled authorization: error=%v plans=%d", err, len(*f.plans))
			}
			script := (*f.plans)[0].Script
			if !strings.Contains(script, "'"+f.installed+"' \"$stage/cleanup-helper\"") || strings.Contains(script, "'"+f.bundle+"' \"$stage/cleanup-helper\"") || !strings.Contains(script, "\"$stage/cleanup-helper\" --cleanup") {
				t.Fatal("verified fallback was not staged from the installed artifact")
			}
		})
	}
}

func TestVerifiedInstallerCancellationNeverAuthorizes(t *testing.T) {
	for _, operation := range []string{"install", "uninstall"} {
		t.Run(operation, func(t *testing.T) {
			f := newVerifiedInstallFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			call := f.client.InstallHelper
			if operation == "uninstall" {
				call = f.client.UninstallHelper
			}
			if err := call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was not preserved: %v", err)
			}
			if len(*f.plans) != 0 {
				t.Fatal("canceled operation reached authorization")
			}
		})
	}
}

func TestVerifiedInstallRequiresAuthenticatedReadinessAfterAuthorization(t *testing.T) {
	for _, becomesReady := range []bool{true, false} {
		t.Run(fmt.Sprint(becomesReady), func(t *testing.T) {
			c, _, _ := startLocalHelper(t, "test", newFakeEngine())
			replacementNeeded := true
			checkFile := c.checkFile
			c.checkFile = func(path string, installed bool) error {
				if installed && replacementNeeded {
					return errArtifactMismatch
				}
				return checkFile(path, installed)
			}
			calls := 0
			c.executePlan = func(context.Context, helper.InstallPlan) error {
				calls++
				replacementNeeded = !becomesReady
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := c.InstallHelper(ctx)
			if calls != 1 || (err == nil) != becomesReady {
				t.Fatalf("readiness=%v calls=%d err=%v", becomesReady, calls, err)
			}
		})
	}
}

func TestVerifiedFreshInstallKeepsCancellation(t *testing.T) {
	f := newVerifiedInstallFixture(t)
	if err := os.Remove(f.installed); err != nil {
		t.Fatal(err)
	}
	f.client.opts.Prompts.Install = "Review first helper installation."
	if err := f.client.InstallHelper(context.Background()); err != f.canceled {
		t.Fatal(err)
	}
	if len(*f.plans) != 1 || (*f.plans)[0].Prompt != f.client.opts.Prompts.Install {
		t.Fatal("fresh install did not use the explicit installation plan")
	}
	if _, err := os.Stat(f.installed); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled installation created an installed file", err)
	}
}
