package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
