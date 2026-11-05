package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewRejectsMissingChangedAndAdditionalNotices(t *testing.T) {
	dir := t.TempDir()
	m := &module{Path: "example.com/dependency", Dir: dir}
	text := []byte("Example license\r\nCopyright Example Contributors\r\n")
	a := approval{Files: map[string]string{"LICENSE": digest(text)}}
	dirs := map[string]bool{dir: true}
	if _, err := reviewedFiles(m, dirs, a); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing license: %v", err)
	}
	path := filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := reviewedFiles(m, dirs, a)
	if err != nil || !bytes.Equal(files["LICENSE"], text) {
		t.Fatalf("license bytes were not retained: %v", err)
	}
	if err := os.WriteFile(path, []byte("Changed license\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewedFiles(m, dirs, a); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed license: %v", err)
	}
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "NOTICE.txt"), []byte("Additional attribution\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewedFiles(m, dirs, a); err == nil || !strings.Contains(err.Error(), "unreviewed") {
		t.Fatalf("additional notice: %v", err)
	}
}

func TestLeadingNoticesKeepFullComments(t *testing.T) {
	block := "/* SPDX-License-Identifier: MIT\nCopyright Example Contributors\n*/"
	lines := "// Copyright Example Contributors\r\n// Use is subject to LICENSE.\r\n"
	got := leadingNotices([]byte(block + "\n\n" + lines + "\n// Package documentation.\npackage example\n"))
	if len(got) != 2 || got[0] != block || got[1] != lines {
		t.Fatalf("source notices were changed or truncated: %q", got)
	}
}
