package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Process inventories contain identities, never command arguments or environment.
// They stay in the private session and are not helper API or connection-log data.
type processRecord struct {
	PID      int       `json:"pid"`
	Parent   int       `json:"parent"`
	UID      int       `json:"uid"`
	Started  time.Time `json:"started"`
	Name     string    `json:"name"`
	Path     string    `json:"path,omitempty"`
	UniqueID uint64    `json:"uniqueID,omitempty"`
	Version  uint32    `json:"pidVersion,omitempty"`
	Resource uint64    `json:"resourceCoalition,omitempty"`
	Jetsam   uint64    `json:"jetsamCoalition,omitempty"`
	Problem  string    `json:"identityProblem,omitempty"`
}

func sameProcess(a, b processRecord) bool {
	return a.PID > 1 && a.PID == b.PID && a.UID == b.UID && a.Started.Equal(b.Started) &&
		a.UniqueID != 0 && a.UniqueID == b.UniqueID && a.Version != 0 && a.Version == b.Version && a.Path == b.Path
}

type processInventory struct {
	At        time.Time       `json:"at"`
	Processes []processRecord `json:"processes"`
}

type processWindow struct {
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
}

type processAudit struct {
	At        time.Time       `json:"at"`
	Windows   []processWindow `json:"observationWindows"`
	Remaining []processRecord `json:"startedDuringObservationAndStillPresent"`
	Notes     string          `json:"notes"`
}

func auditProcesses(windows []processWindow, inventory processInventory) processAudit {
	r := processAudit{At: inventory.At, Windows: windows, Notes: "Read-only independent inventory of all process owners, compared by kernel start time. Presence does not establish test ownership and never authorizes a signal. An unfinished window ends at this audit."}
	for _, p := range inventory.Processes {
		for _, w := range windows {
			end := w.Ended
			if end.IsZero() {
				end = inventory.At
			}
			if !w.Started.IsZero() && !p.Started.Before(w.Started) && !p.Started.After(end) {
				r.Remaining = append(r.Remaining, p)
				break
			}
		}
	}
	sort.Slice(r.Remaining, func(i, j int) bool { return r.Remaining[i].PID < r.Remaining[j].PID })
	return r
}

func (s *session) auditProcesses(ctx context.Context, snapshot func(context.Context) (processInventory, error)) error {
	if len(s.ProcessWindows) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	inv, err := snapshot(ctx)
	if err != nil {
		return fmt.Errorf("independent process audit unavailable: %w", err)
	}
	report := auditProcesses(s.ProcessWindows, inv)
	path, err := archiveJSON(s, "process-audit", report)
	if err != nil {
		return err
	}
	fmt.Printf("Process audit: %d processes started during observation still present; private report %s\n", len(report.Remaining), path)
	for _, p := range report.Remaining {
		fmt.Printf("  PID %d UID %d started %s name %q path %q\n", p.PID, p.UID, p.Started.Format(time.RFC3339Nano), p.Name, p.Path)
	}
	return nil
}

func processPause(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errProcessChanged = errors.New("process identity changed; no signal sent")
