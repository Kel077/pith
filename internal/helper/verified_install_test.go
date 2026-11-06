package helper

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper/identity"
	"howett.net/plist"
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
				"protocol":        func(id *identity.Identity) { id.Protocol = 0 },
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

func TestVerifiedPlansRejectUntrustedSource(t *testing.T) {
	for _, kind := range []string{"non-executable", "group-writable", "other-writable", "symlink", "directory", "empty", "missing", "relative"} {
		t.Run(kind, func(t *testing.T) {
			source, expected := verifiedPlanSource(t)
			switch kind {
			case "non-executable":
				if err := os.Chmod(source, 0600); err != nil {
					t.Fatal(err)
				}
			case "group-writable":
				if err := os.Chmod(source, 0770); err != nil {
					t.Fatal(err)
				}
			case "other-writable":
				if err := os.Chmod(source, 0702); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := source + ".link"
				if err := os.Symlink(source, link); err != nil {
					t.Fatal(err)
				}
				source = link
			case "directory":
				source = filepath.Dir(source)
			case "empty":
				if err := os.Truncate(source, 0); err != nil {
					t.Fatal(err)
				}
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

func TestVerifiedUninstallAcceptsFrozenProtocolWithoutIPC(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	expected.Protocol--
	if expected.Protocol == 0 {
		// Protocol 1 has no earlier valid value; any different valid protocol
		// establishes that CLI cleanup is independent of the current IPC version.
		expected.Protocol = ProtocolVersion + 1
	}
	plan, err := PlanVerifiedUninstall(source, expected)
	if err != nil {
		t.Fatal("frozen cleanup identity was incorrectly tied to the current IPC protocol", err)
	}
	if !strings.Contains(plan.Script, "[ \"${actual%% *}\" = "+shellQuote(expected.SHA256)+" ]") || !strings.Contains(plan.Script, "\"$stage/cleanup-helper\" --cleanup") {
		t.Fatal("frozen protocol bypassed the staged cleanup identity check")
	}
	if plan, err := PlanVerifiedInstall(source, 501, expected); api.Code(err) != api.ErrHelperUntrusted || plan.Script != "" {
		t.Fatal("installation accepted an incompatible IPC protocol", err)
	}
}

func TestVerifiedInstallRepairsRegistrationFromVerifiedSource(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	plan, err := PlanVerifiedInstall(source, 501, expected)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(plan.Script, "<?xml")
	end := strings.Index(plan.Script, "</plist>")
	if start < 0 || end < start {
		t.Fatal("repair omitted the rebuilt launchd registration")
	}
	var launch map[string]any
	if _, err := plist.Unmarshal([]byte(plan.Script[start:end+len("</plist>")]), &launch); err != nil {
		t.Fatal(err)
	}
	args, ok := launch["ProgramArguments"].([]any)
	if !ok || len(args) != 3 || args[0] != BinaryPath || args[1] != "--owner-uid" || args[2] != "501" {
		t.Fatalf("repair did not rebuild the fixed installed executable and owner: %v", launch)
	}
	if launch["Label"] != Label || launch["RunAtLoad"] != true || launch["KeepAlive"] != true {
		t.Fatalf("repair did not rebuild the fixed launchd job: %v", launch)
	}
	if !strings.Contains(plan.Script, shellQuote(source)+" \"$stage/new-helper\"") || !strings.Contains(plan.Script, "[ \"${actual%% *}\" = "+shellQuote(expected.SHA256)+" ]") || !strings.Contains(plan.Script, "/bin/launchctl bootstrap system \"$plist\"") {
		t.Fatal("repair bypassed verified staging or registration")
	}
	if plan, err := PlanVerifiedInstall(source, 0, expected); api.Code(err) != api.ErrHelperUntrusted || plan.Script != "" {
		t.Fatal("repair accepted a root installation owner", err)
	}
}

func TestVerifiedUninstallPreservesInstallationUntilCleanupSucceeds(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	plan, err := PlanVerifiedUninstall(source, expected)
	if err != nil {
		t.Fatal(err)
	}
	script := plan.Script
	checkAt := strings.Index(script, "[ \"${actual%% *}\" = "+shellQuote(expected.SHA256)+" ]")
	loadedAt := strings.Index(script, "  was_loaded=1\n")
	changedAt := strings.Index(script, "\nchanged=1\n")
	bootoutAt := strings.Index(script, "/bin/launchctl bootout \"$service\"")
	cleanupLine := "if [ -f \"$data/capture.json\" ]; then \"$stage/cleanup-helper\" --cleanup; fi\n"
	cleanupAt := strings.Index(script, cleanupLine)
	completeAt := strings.Index(script, "\ncleanup_done=1\n")
	removeAt := strings.Index(script, "/bin/rm -f "+shellQuote(BinaryPath)+" "+shellQuote(PlistPath))
	if checkAt < 0 || loadedAt <= checkAt || changedAt <= loadedAt || bootoutAt <= changedAt || cleanupAt <= bootoutAt || completeAt < cleanupAt+len(cleanupLine)-1 || removeAt <= completeAt {
		t.Fatalf("unsafe uninstall order: hash=%d loaded=%d changed=%d bootout=%d cleanup=%d complete=%d remove=%d", checkAt, loadedAt, changedAt, bootoutAt, cleanupAt, completeAt, removeAt)
	}
	beforeCleanup := script[:cleanupAt]
	if strings.Contains(beforeCleanup, "/bin/rm -f ") || strings.Contains(beforeCleanup, "/bin/rm -rf \"$data\"") || strings.Contains(beforeCleanup, "/bin/mv ") {
		t.Fatal("uninstall can delete or replace installed files before cleanup succeeds")
	}
	if !strings.Contains(script[:loadedAt], "[ -f \"$plist\" ] && [ \"$(/usr/bin/stat -f %u \"$plist\")\" = 0 ]") || !strings.Contains(script[:loadedAt], "$((0$pith_plist_mode & 022))") {
		t.Fatal("rollback can bootstrap an unverified registration file")
	}
}

func TestVerifiedUninstallRollbackRestoresOnlyInterruptedLoadedJob(t *testing.T) {
	source, expected := verifiedPlanSource(t)
	plan, err := PlanVerifiedUninstall(source, expected)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(plan.Script, "rollback() {\n")
	end := strings.Index(plan.Script, "\n}\ntrap rollback EXIT\n")
	if start < 0 || end <= start {
		t.Fatal("uninstall omitted its rollback handler")
	}
	rollback := plan.Script[start : end+2]
	// Execute only the rollback function, with every external command replaced
	// by a shell builtin recorder. No installation plan or system command runs.
	rollback = strings.ReplaceAll(rollback, "/bin/launchctl ", "record_launchctl ")
	rollback = strings.ReplaceAll(rollback, "/bin/rm ", "record_remove ")
	if strings.Contains(rollback, "/bin/") || strings.Contains(rollback, "$data") || strings.Contains(rollback, "$binary") {
		t.Fatal("rollback contains an unmocked system command or installation deletion")
	}
	for _, tc := range []struct {
		name                              string
		loaded, changed, cleaned, present int
		status                            int
		want                              string
	}{
		{name: "cleanup-failed-loaded", loaded: 1, changed: 1, status: 17, want: "print\nbootstrap\nremove-stage\n"},
		{name: "cleanup-failed-unloaded", changed: 1, status: 17, want: "remove-stage\n"},
		{name: "failed-before-bootout", loaded: 1, status: 17, want: "remove-stage\n"},
		{name: "bootout-failed-still-loaded", loaded: 1, changed: 1, present: 1, status: 17, want: "print\nremove-stage\n"},
		{name: "cleanup-succeeded", loaded: 1, changed: 1, cleaned: 1, want: "remove-stage\n"},
		{name: "removal-failed-after-cleanup", loaded: 1, changed: 1, cleaned: 1, status: 17, want: "remove-stage\n"},
		{name: "interrupted-after-bootout", loaded: 1, changed: 1, status: 1, want: "print\nbootstrap\nremove-stage\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "commands")
			script := "set +e\nlog=" + shellQuote(log) + "\nservice='system/test-helper'\nplist=" + shellQuote(filepath.Join(dir, "helper.plist")) + "\nstage=" + shellQuote(filepath.Join(dir, "stage")) + "\n" +
				fmt.Sprintf("was_loaded=%d\nchanged=%d\ncleanup_done=%d\npresent=%d\n", tc.loaded, tc.changed, tc.cleaned, tc.present) +
				"record_launchctl() {\n  case \"$1\" in\n    print) [ \"$2\" = \"$service\" ] || return 97; printf 'print\\n' >>\"$log\"; [ \"$present\" -eq 1 ];;\n    bootstrap) [ \"$2\" = system ] && [ \"$3\" = \"$plist\" ] || return 97; printf 'bootstrap\\n' >>\"$log\";;\n    *) return 97;;\n  esac\n}\n" +
				"record_remove() { [ \"$1\" = -rf ] && [ \"$2\" = \"$stage\" ] || return 97; printf 'remove-stage\\n' >>\"$log\"; }\n" +
				rollback + fmt.Sprintf("\n(exit %d)\nrollback\n", tc.status)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh")
			cmd.Stdin = strings.NewReader(script)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if tc.status == 0 && err != nil || tc.status != 0 && (!errors.As(err, &exit) || exit.ExitCode() != tc.status) {
				t.Fatalf("rollback did not preserve exit status %d: %v: %s", tc.status, err, output)
			}
			got, err := os.ReadFile(log)
			if err != nil || string(got) != tc.want {
				t.Fatalf("rollback commands = %q, %v; want %q", got, err, tc.want)
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
