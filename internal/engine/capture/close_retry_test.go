package capture

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type cleanupSnapshotDriver struct {
	fakeDriver
	closing      bool
	postFailures int
	postReads    int
	remain       bool
	stale        bool
}

func (d *cleanupSnapshotDriver) Snapshot() ([]Route, error) {
	if d.closing && d.device.closed {
		d.postReads++
		if d.postReads <= d.postFailures || d.postFailures < 0 {
			return nil, context.DeadlineExceeded
		}
		if d.remain {
			return []Route{{Prefix: prefix("1.0.0.0/8"), Interface: "utun42", Index: 42}}, nil
		}
		if d.stale && d.postReads == 1 {
			return []Route{{Prefix: prefix("1.0.0.0/8"), Index: 42}}, nil
		}
	}
	return d.fakeDriver.Snapshot()
}

func TestCleanupVerificationRecoversAndPreservesFailureEvidence(t *testing.T) {
	for _, kind := range []string{"temporary-read", "persistent-read", "residue", "disappearing-interface"} {
		t.Run(kind, func(t *testing.T) {
			d := &cleanupSnapshotDriver{fakeDriver: fakeDriver{routes: baseRoutes()}, postFailures: 2}
			if kind == "persistent-read" {
				d.postFailures = -1
			}
			d.remain = kind == "residue"
			if kind == "disappearing-interface" {
				d.postFailures, d.stale = 0, true
			}
			opts := fakeOptions(t, &d.fakeDriver)
			opts.Driver = d
			var phases []string
			opts.Trace = func(phase string, _ time.Duration, err error) {
				if err != nil {
					phases = append(phases, phase)
				}
			}
			c, err := Prepare(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			d.closing = true
			started := time.Now()
			err = c.Close()
			if !d.device.closed || len(phases) < 1 || time.Since(started) > 3*time.Second {
				t.Fatal("cleanup was not bounded or device/evidence missing", err, phases)
			}
			_, journalErr := os.Stat(opts.StatePath)
			if kind == "temporary-read" || kind == "disappearing-interface" {
				want := 3
				if d.stale {
					want = 2
				}
				if err != nil || c.Identity().EndedAt.IsZero() || d.postReads != want || !errors.Is(journalErr, os.ErrNotExist) {
					t.Fatal("transient verification did not recover", err, d.postReads, journalErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), "verify capture closure") || !c.Identity().EndedAt.IsZero() || journalErr != nil {
				t.Fatal("unproven cleanup was accepted or lost its journal", err, journalErr)
			}
		})
	}
}
