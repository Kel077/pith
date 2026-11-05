package helper

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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
