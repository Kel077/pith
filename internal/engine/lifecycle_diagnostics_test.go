package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLifecycleDiagnosticsAreBoundedPrivateAndAddressFree(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	path := filepath.Join(e.opts.DataDir, lifecycleFile)
	for i := 0; i < lifecycleLimit+4; i++ {
		e.traceLifecycle("capture.close.verify-1", time.Millisecond, fmt.Errorf("private target 192.0.2.9 via example0: %w", context.DeadlineExceeded))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("trace performed disk I/O before cleanup", err)
	}
	e.saveLifecycle()
	data, err := os.ReadFile(path)
	if err != nil || len(data) > lifecycleMaxBytes {
		t.Fatal(err, len(data))
	}
	for _, secret := range []string{"192.0.2.9", "example0", "private target"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("raw error payload entered lifecycle record")
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	var records []lifecycleRecord
	if err := json.Unmarshal(data, &records); err != nil || len(records) != lifecycleLimit || records[0].ErrorKind != "deadline" {
		t.Fatal(err, len(records))
	}
	loaded := &Engine{opts: e.opts}
	loaded.loadLifecycle()
	if !slices.Equal(records, loaded.diagnostics) {
		t.Fatal("helper restart discarded the bounded diagnostic record")
	}
	status, _ := e.Status(context.Background())
	if !slices.Contains(status.RouteDiagnostics, "cleanup phase=capture.close.verify error=deadline") || len(status.RouteDiagnostics) != 1 {
		t.Fatal("authenticated status cannot expose the failing phase", status.RouteDiagnostics)
	}
}

func TestLifecycleDiagnosticsDoNotFollowSymlinksOrFailCleanup(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	outside := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(outside, []byte("leave unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.opts.DataDir, lifecycleFile)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	e.loadLifecycle()
	e.traceLifecycle("stop.complete", time.Millisecond, nil)
	e.saveLifecycle()
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "leave unchanged" {
		t.Fatal("diagnostics followed a symlink", err)
	}
	bad := &Engine{opts: Options{DataDir: filepath.Join(t.TempDir(), "absent")}}
	bad.saveLifecycle()
	if len(bad.status.RouteDiagnostics) != 1 {
		t.Fatal("unavailable diagnostics were not reported")
	}
}
