package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const lifecycleLimit = 128
const lifecycleMaxBytes = 64 << 10
const lifecycleFile = "lifecycle.json"

// Lifecycle evidence is metadata only. Never serialize error strings, interface
// names, destinations, process paths, requests, or credentials into this file.
type lifecycleRecord struct {
	At        time.Time `json:"at"`
	Phase     string    `json:"phase"`
	ElapsedMS int64     `json:"elapsedMS"`
	ErrorKind string    `json:"errorKind,omitempty"`
}

func lifecycleErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ENOMEM):
		return "no-memory"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	default:
		return "other"
	}
}

func (e *Engine) traceLifecycle(phase string, elapsed time.Duration, err error) {
	record := lifecycleRecord{At: time.Now().UTC(), Phase: phase, ElapsedMS: elapsed.Milliseconds(), ErrorKind: lifecycleErrorKind(err)}
	e.diagnosticsMu.Lock()
	if len(e.diagnostics) == lifecycleLimit {
		e.diagnostics = e.diagnostics[1:]
	}
	e.diagnostics = append(e.diagnostics, record)
	e.diagnosticsMu.Unlock()
	// Capture-close failures also reach authenticated Status, so the acceptance
	// controller can archive the failed phase before removal without root file access.
	if err != nil && strings.HasPrefix(phase, "capture.close.") {
		// Repeated observations retain every attempt on disk, but must not evict
		// the original route-fault metadata from the small authenticated status ring.
		if strings.HasPrefix(phase, "capture.close.verify-") {
			phase = "capture.close.verify"
		}
		e.routeDiagnostic("cleanup phase=" + phase + " error=" + record.ErrorKind)
	}
}

func (e *Engine) lifecycleStep(phase string, action func() error) error {
	started := time.Now()
	e.traceLifecycle(phase+".begin", 0, nil)
	err := action()
	e.traceLifecycle(phase, time.Since(started), err)
	return err
}

// Tracing itself is memory-only. Persist only after capture cleanup, so disk I/O
// cannot postpone closing the utun. Failure to record diagnostics never blocks Stop.
func (e *Engine) saveLifecycle() {
	if err := e.writeLifecycle(); err != nil {
		e.routeDiagnostic("lifecycle diagnostics could not be saved: " + lifecycleErrorKind(err))
	}
}

func (e *Engine) writeLifecycle() error {
	e.diagnosticsMu.Lock()
	defer e.diagnosticsMu.Unlock()
	data, err := json.Marshal(e.diagnostics)
	if err != nil {
		return err
	}
	if len(data) > lifecycleMaxBytes {
		return errors.New("lifecycle diagnostic budget exceeded")
	}
	f, err := os.CreateTemp(e.opts.DataDir, ".lifecycle-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(e.opts.DataDir, lifecycleFile))
}

func (e *Engine) loadLifecycle() {
	f, err := os.OpenFile(filepath.Join(e.opts.DataDir, lifecycleFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > lifecycleMaxBytes {
		return
	}
	var records []lifecycleRecord
	if json.NewDecoder(io.LimitReader(f, lifecycleMaxBytes)).Decode(&records) != nil || len(records) > lifecycleLimit {
		return
	}
	e.diagnostics = slices.Clone(records)
}
