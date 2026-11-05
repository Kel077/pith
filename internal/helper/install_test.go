package helper

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func TestInstallPlanIsQuotedAndParseable(t *testing.T) {
	source := filepath.Join(t.TempDir(), "Pith's $(touch unwanted) helper")
	if err := os.WriteFile(source, []byte("test executable; never executed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanInstall(source, 501)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Script, shellQuote(source)) || !strings.Contains(plan.Script, "helper changed during authorization") || !strings.Contains(plan.Script, "rollback") {
		t.Fatal("incomplete install plan")
	}
	if len(plan.Changes) != 4 || plan.Undo == "" {
		t.Fatal("missing review/undo information")
	}
	for _, script := range []string{plan.Script, PlanUninstall().Script} {
		cmd := exec.Command("/bin/sh", "-n") // Parse only; no privileged operation runs.
		cmd.Stdin = strings.NewReader(script)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid installation script: %v: %s", err, output)
		}
	}
	if runtime.GOOS == "darwin" {
		// Compilation validates AppleScript quoting and the prompt clause without
		// executing the shell script or presenting an authorization dialog.
		for _, prompt := range []string{InstallPrompt, UpdatePrompt, UninstallPrompt, "Authorize \"Pith\" to use \\ paths.\nContinue?"} {
			plan.Prompt = prompt
			cmd := exec.Command("/usr/bin/osacompile", "-o", filepath.Join(t.TempDir(), "plan.scpt"), "-e", plan.appleScript())
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid authorization script: %v: %s", err, output)
			}
		}
	}
	if _, err := PlanInstall(source, 0); err == nil {
		t.Fatal("accepted root as installing user")
	}
	if _, err := PlanInstall("relative", 501); err == nil {
		t.Fatal("accepted relative source")
	}
}

func TestLaunchPlistIsIdleAndBoundToInstaller(t *testing.T) {
	data, err := launchPlist(501)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if _, err := plist.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if value["Label"] != Label || value["RunAtLoad"] != true || value["KeepAlive"] != true {
		t.Fatal("incorrect launchd configuration")
	}
	args := value["ProgramArguments"].([]any)
	if len(args) != 3 || args[0] != BinaryPath || args[1] != "--owner-uid" || args[2] != "501" {
		t.Fatal(args)
	}
	if bytes.Contains(data, []byte("--start")) {
		t.Fatal("daemon must start idle")
	}
}

func TestInstallPlansOnlyCreateMissingSharedDirectories(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pithd")
	if err := os.WriteFile(source, []byte("test helper"), 0700); err != nil {
		t.Fatal(err)
	}
	regular, err := PlanInstall(source, 501)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := PlanScopedInstall(source, 501, ScopedOptions{
		Prefixes:     []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")},
		ProbeAddress: "192.0.2.10:443", ProbeServerName: "example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, plan := range map[string]InstallPlan{"regular": regular, "scoped": scoped} {
		t.Run(name, func(t *testing.T) {
			parents := []string{"/Library/PrivilegedHelperTools", "/Library/LaunchDaemons"}
			if !slices.Equal(plan.CreatesIfMissing, parents) {
				t.Fatalf("review plan omits conditional shared directory creation: %v", plan.CreatesIfMissing)
			}
			for i, parent := range parents {
				mode := []string{"1755", "0755"}[i]
				guard := createSharedDirectory(parent, mode)
				if strings.Count(plan.Script, guard) != 1 {
					t.Fatalf("missing unique creation guard for %s", parent)
				}
				if strings.Index(plan.Script, guard)+len(guard) > strings.Index(plan.Script, "/usr/bin/install -d -o root -g wheel -m 0700") {
					t.Fatalf("shared parent validation must precede the file/service transaction: %s", parent)
				}
				remainder := strings.Replace(plan.Script, guard, "", 1)
				for _, line := range strings.Split(remainder, "\n") {
					if strings.Contains(line, parent) && (strings.Contains(line, "install -d") || strings.Contains(line, "mkdir") || strings.Contains(line, "chmod") || strings.Contains(line, "chown")) {
						t.Fatalf("shared directory mutation outside its missing-only guard: %s", line)
					}
				}
			}
		})
	}
}

func TestSharedDirectoryGuardPreservesExistingMetadata(t *testing.T) {
	for _, mode := range []string{"1755", "0755"} {
		for _, kind := range []string{"missing", "directory", "foreign-directory", "group-writable-directory", "other-writable-directory", "file", "symlink", "dangling-symlink", "concurrent-directory", "mkdir-failure"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "shared's $(ignored) directory")
				switch kind {
				case "directory", "foreign-directory", "group-writable-directory", "other-writable-directory":
					if err := os.Mkdir(path, 0711); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0711|os.ModeSticky); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink", "dangling-symlink":
					target := filepath.Join(dir, "target")
					if kind == "symlink" {
						if err := os.Mkdir(target, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := os.Lstat(path)
				log := filepath.Join(dir, "chown-args")
				mkdir := "record_mkdir() { /bin/mkdir \"$@\"; }\n"
				if kind == "concurrent-directory" {
					// Simulate another installer winning the exclusive creation race.
					mkdir = "record_mkdir() { /bin/mkdir -m 1711 " + shellQuote(path) + "; return 1; }\n"
				} else if kind == "mkdir-failure" {
					mkdir = "record_mkdir() { return 1; }\n"
				}
				// Run only the guard in TMPDIR. Replace chown with a recorder and
				// stat with synthetic root metadata; no real ownership is changed.
				owner, permissions := "0", "711"
				if kind == "foreign-directory" {
					owner = "501"
				} else if kind == "group-writable-directory" {
					permissions = "775"
				} else if kind == "other-writable-directory" {
					permissions = "757"
				}
				stat := "record_stat() { case \"$2\" in %u) printf '%s\\n' " + owner + ";; %OLp) printf '%s\\n' " + permissions + ";; *) return 1;; esac; }\n"
				guard := strings.ReplaceAll(createSharedDirectory(path, mode), "/usr/sbin/chown ", "record_chown ")
				guard = strings.ReplaceAll(guard, "/bin/mkdir ", "record_mkdir ")
				guard = strings.ReplaceAll(guard, "/usr/bin/stat ", "record_stat ")
				script := "set -eu\nrecord_chown() { printf '%s\\n' \"$@\" >>" + shellQuote(log) + "; }\n" + mkdir + stat + guard
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "/bin/sh")
				cmd.Stdin = strings.NewReader(script)
				output, err := cmd.CombinedOutput()
				wantError := kind != "missing" && kind != "directory" && kind != "concurrent-directory"
				if (err != nil) != wantError {
					t.Fatalf("guard result = %v, output %q; want error %v", err, output, wantError)
				}
				args, readErr := os.ReadFile(log)
				if kind == "missing" {
					want := "root:wheel\n" + path + "\n"
					if readErr != nil || string(args) != want {
						t.Fatalf("ownership arguments = %q, error %v; want %q", args, readErr, want)
					}
					wantMode := os.ModeDir | 0755
					if mode == "1755" {
						wantMode |= os.ModeSticky
					}
					if info, err := os.Stat(path); err != nil || info.Mode() != wantMode {
						t.Fatalf("created directory mode is incorrect: info=%v error=%v want=%v", info, err, wantMode)
					}
				} else {
					if !os.IsNotExist(readErr) {
						t.Fatalf("non-created path reached chown: %q, error %v", args, readErr)
					}
					after, err := os.Lstat(path)
					if kind == "mkdir-failure" {
						if !os.IsNotExist(err) {
							t.Fatalf("failed mkdir changed path: %v, %v", after, err)
						}
					} else if kind == "concurrent-directory" {
						if err != nil || after.Mode() != os.ModeDir|os.ModeSticky|0711 {
							t.Fatalf("concurrently created directory changed: %v, %v", after, err)
						}
					} else if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
						t.Fatalf("existing metadata changed: before=%v after=%v error=%v", before, after, err)
					}
				}
			})
		}
	}
}

func TestScopedInstallCannotExpandToNetworkCapture(t *testing.T) {
	source := filepath.Join(t.TempDir(), "pithd")
	if err := os.WriteFile(source, []byte("test helper"), 0700); err != nil {
		t.Fatal(err)
	}
	valid := ScopedOptions{Prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}, ProbeAddress: "192.0.2.10:443", ProbeServerName: "example.com"}
	plan, err := PlanScopedInstall(source, 501, valid)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(plan.Script, "<?xml")
	end := strings.Index(plan.Script, "</plist>")
	if start < 0 || end < start {
		t.Fatal("missing launchd plist")
	}
	var launch map[string]any
	if _, err := plist.Unmarshal([]byte(plan.Script[start:end+len("</plist>")]), &launch); err != nil {
		t.Fatal(err)
	}
	args := launch["ProgramArguments"].([]any)
	want := []string{BinaryPath, "--owner-uid", "501", "--safe-target", "192.0.2.10:443", "--safe-server-name", "example.com", "--capture", "192.0.2.10/32"}
	if len(args) != len(want) {
		t.Fatal(args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatal(args)
		}
	}
	for _, p := range []string{"0.0.0.0/1", "192.0.2.0/24", "2001:db8::/32", "127.0.0.1/32", "192.0.2.11/32"} {
		invalid := valid
		invalid.Prefixes = []netip.Prefix{netip.MustParsePrefix(p)}
		if _, err := PlanScopedInstall(source, 501, invalid); err == nil {
			t.Fatalf("accepted unsafe or unrelated scope %s", p)
		}
	}
	invalid := valid
	invalid.Prefixes = nil
	if _, err := PlanScopedInstall(source, 501, invalid); err == nil {
		t.Fatal("accepted empty scope")
	}
	valid.Prefixes = []netip.Prefix{netip.MustParsePrefix("2001:db8::10/128")}
	valid.ProbeAddress = "[2001:db8::10]:443"
	if _, err := PlanScopedInstall(source, 501, valid); err != nil {
		t.Fatal("rejected scoped IPv6 probe", err)
	}
}
