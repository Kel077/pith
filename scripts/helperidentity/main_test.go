package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kel077/pith/internal/helper/identity"
)

func TestGeneratedIdentity(t *testing.T) {
	id := identity.Identity{Schema: 1, Protocol: 1, Version: "0.1.0", SHA256: strings.Repeat("a", 64), CDHash: strings.Repeat("b", 40)}
	data, err := generatedSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "generated.go", data, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(id.SHA256)) || !bytes.Contains(data, []byte(id.CDHash)) {
		t.Fatal("missing frozen digest")
	}
	path := filepath.Join(t.TempDir(), "generated.go")
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatal("unchanged identity unnecessarily rewrote client source", err)
	}
	id.CDHash = ""
	if _, err := generatedSource(id); err == nil {
		t.Fatal("generated an invalid trust anchor")
	}
}

func TestRunRejectsWritableHelper(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native ARM64 Mach-O only")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "pithd")
	if err := os.WriteFile(source, data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-helper", source, "-manifest", filepath.Join(dir, "identity.json")}
	generateArgs := append(append([]string{}, args...), "-generate", filepath.Join(dir, "generated.go"))
	verifyArgs := append(append([]string{}, args...), "-verify")
	if err := run(generateArgs); err != nil {
		t.Fatal(err)
	}
	if err := run(verifyArgs); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0775, 0757, 0777} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			if err := os.Chmod(source, mode); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{generateArgs, verifyArgs} {
				if err := run(args); err == nil || !strings.Contains(err.Error(), "group- or other-writable") {
					t.Fatalf("writable helper was not rejected: %v", err)
				}
			}
		})
	}
	if err := os.Chmod(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := run(verifyArgs); err != nil {
		t.Fatalf("safe permissions did not restore verification: %v", err)
	}
}
