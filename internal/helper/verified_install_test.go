package helper

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper/identity"
)

func verifiedPlanSource(t *testing.T) (string, identity.Identity) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "Pith's $(ignored) helper")
	contents := []byte("verified helper fixture; never executed\n")
	if err := os.WriteFile(source, contents, 0700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return source, identity.Identity{Schema: identity.Schema, Protocol: ProtocolVersion, Version: "test-helper", SHA256: fmt.Sprintf("%x", digest), CDHash: strings.Repeat("1", 40)}
}

func TestVerifiedPlansRejectUntrustedIdentity(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	planners := map[string]func(string, identity.Identity) (InstallPlan, error){
		"install": func(source string, id identity.Identity) (InstallPlan, error) {
			return PlanVerifiedInstall(source, 501, id)
		},
		"uninstall": PlanVerifiedUninstall,
	}
	for operation, plan := range planners {
		t.Run(operation, func(t *testing.T) {
			for name, change := range map[string]func(*identity.Identity){
				"schema":          func(id *identity.Identity) { id.Schema++ },
				"protocol":        func(id *identity.Identity) { id.Protocol++ },
				"missing-version": func(id *identity.Identity) { id.Version = "" },
				"invalid-version": func(id *identity.Identity) { id.Version = "test\nhelper" },
				"short-sha256":    func(id *identity.Identity) { id.SHA256 = id.SHA256[:63] },
				"uppercase-sha256": func(id *identity.Identity) {
					id.SHA256 = strings.ToUpper(id.SHA256)
				},
				"zero-cdhash": func(id *identity.Identity) { id.CDHash = strings.Repeat("0", 40) },
				"different-final-sha256-digit": func(id *identity.Identity) {
					last := "0"
					if strings.HasSuffix(id.SHA256, last) {
						last = "1"
					}
					id.SHA256 = id.SHA256[:63] + last
				},
			} {
				t.Run(name, func(t *testing.T) {
					invalid := expected
					change(&invalid)
					got, err := plan(source, invalid)
					if api.Code(err) != api.ErrHelperUntrusted || got.Script != "" {
						t.Fatalf("untrusted identity produced a usable plan: error=%v script=%q", err, got.Script)
					}
				})
			}
		})
	}
}

func TestVerifiedPlansRejectChangedSource(t *testing.T) {
	for operation, plan := range map[string]func(string, identity.Identity) (InstallPlan, error){
		"install": func(source string, id identity.Identity) (InstallPlan, error) {
			return PlanVerifiedInstall(source, 501, id)
		},
		"uninstall": PlanVerifiedUninstall,
	} {
		t.Run(operation, func(t *testing.T) {
			source, expected := verifiedPlanSource(t)
			first, err := plan(source, expected)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, []byte("different helper bytes; never executed\n"), 0700); err != nil {
				t.Fatal(err)
			}
			got, err := plan(source, expected)
			if api.Code(err) != api.ErrHelperUntrusted || got.Script != "" {
				t.Fatalf("changed source produced a usable plan: error=%v script=%q", err, got.Script)
			}
			if !strings.Contains(first.Script, "[ \"${actual%% *}\" = "+shellQuote(expected.SHA256)+" ]") {
				t.Fatal("previously reviewed plan did not retain the complete compiled digest")
			}
		})
	}
}

func TestVerifiedPlansRejectNonExecutableSource(t *testing.T) {
	for _, kind := range []string{"non-executable", "directory", "missing", "relative"} {
		t.Run(kind, func(t *testing.T) {
			source, expected := verifiedPlanSource(t)
			switch kind {
			case "non-executable":
				if err := os.Chmod(source, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				source = filepath.Dir(source)
			case "missing":
				source += ".missing"
			case "relative":
				source = "relative-helper"
			}
			for operation, plan := range map[string]func() (InstallPlan, error){
				"install":   func() (InstallPlan, error) { return PlanVerifiedInstall(source, 501, expected) },
				"uninstall": func() (InstallPlan, error) { return PlanVerifiedUninstall(source, expected) },
			} {
				got, err := plan()
				if err == nil || got.Script != "" {
					t.Fatalf("%s accepted %s source: error=%v script=%q", operation, kind, err, got.Script)
				}
			}
		})
	}
}

func TestVerifiedPlansCheckStagedBytesBeforeServiceChanges(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	install, err := PlanVerifiedInstall(source, 501, expected)
	if err != nil {
		t.Fatal(err)
	}
	uninstall, err := PlanVerifiedUninstall(source, expected)
	if err != nil {
		t.Fatal(err)
	}
	for operation, plan := range map[string]InstallPlan{"install": install, "uninstall": uninstall} {
		t.Run(operation, func(t *testing.T) {
			stageName := "new-helper"
			if operation == "uninstall" {
				stageName = "cleanup-helper"
			}
			script := plan.Script
			stagePath := "\"$stage/" + stageName + "\""
			copyAt := strings.Index(script, "/usr/bin/install -o root -g wheel -m 0755 "+shellQuote(source)+" "+stagePath)
			hashAt := strings.Index(script, "actual=$(/usr/bin/shasum -a 256 "+stagePath+")")
			checkAt := strings.Index(script, "[ \"${actual%% *}\" = "+shellQuote(expected.SHA256)+" ] ||")
			cleanupAt := strings.Index(script, stagePath+" --cleanup")
			bootoutAt := strings.LastIndex(script, "/bin/launchctl bootout \"$service\"")
			if copyAt < 0 || hashAt <= copyAt || checkAt <= hashAt || bootoutAt <= checkAt || cleanupAt <= bootoutAt {
				t.Fatalf("unsafe staging order: copy=%d hash=%d check=%d bootout=%d cleanup=%d", copyAt, hashAt, checkAt, bootoutAt, cleanupAt)
			}
			if operation == "install" {
				changedAt := strings.Index(script, "\nchanged=1\n")
				if changedAt <= checkAt || changedAt >= bootoutAt || !strings.Contains(script[:checkAt], "[ \"$changed\" -eq 1 ]") {
					t.Fatal("rollback can change service state before the staged hash is verified")
				}
			}
			if strings.Count(script, " --cleanup") != 1 || strings.Contains(script, shellQuote(BinaryPath)+" --cleanup") || strings.Contains(script, "\"$binary\" --cleanup") {
				t.Fatal("cleanup can execute an installed or unverified helper")
			}
			cmd := exec.Command("/bin/sh", "-n") // Parse only; never execute the installation plan.
			cmd.Stdin = strings.NewReader(script)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid verified installation script: %v: %s", err, output)
			}
		})
	}
	parents := []string{filepath.Dir(BinaryPath), filepath.Dir(PlistPath)}
	if !slices.Equal(install.CreatesIfMissing, parents) {
		t.Fatalf("missing conditional shared-directory review: %v", install.CreatesIfMissing)
	}
	for i, parent := range parents {
		guard := createSharedDirectory(parent, []string{"1755", "0755"}[i])
		if strings.Count(install.Script, guard) != 1 {
			t.Fatalf("verified install omitted the missing-only directory guard for %s", parent)
		}
		for operation, script := range map[string]string{"install": strings.Replace(install.Script, guard, "", 1), "uninstall": uninstall.Script} {
			for _, line := range strings.Split(script, "\n") {
				if strings.Contains(line, shellQuote(parent)) && (strings.Contains(line, "install -d") || strings.Contains(line, "mkdir") || strings.Contains(line, "chmod") || strings.Contains(line, "chown")) {
					t.Fatalf("%s changes shared-directory metadata outside its creation guard: %s", operation, line)
				}
			}
		}
	}
}
