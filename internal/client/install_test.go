package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHelperSourceForDevelopmentAndBundle(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "darwin-arm64", "bin", "pithd")
	bundle := filepath.Join(dir, "bin", "pithd")
	for _, path := range []string{dev, bundle} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test helper"), 0700); err != nil {
			t.Fatal(err)
		}
		got, err := helperSource(dir)
		if err != nil || got != path {
			t.Fatalf("source = %q, %v; want %q", got, err, path)
		}
	}
}
