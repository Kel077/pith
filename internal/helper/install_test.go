package helper

import (
	"bytes"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
