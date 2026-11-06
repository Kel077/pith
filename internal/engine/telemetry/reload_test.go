package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestRulesChangedEndsNormallyAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, _ := testStore(t, Options{Dir: dir, LogDays: 2})
	c := fixture()
	c.Problem = "previous informational note"
	id := s.Add(c)
	s.AddBytes(id, 17, 23)
	s.EndWithReason(id, "ignored transport cancellation", api.EndRulesChanged)
	s.End(id, "paired relay close must not overwrite normal termination")
	rows := flush(t, s, true)
	if len(rows) != 1 || rows[0].Ended.IsZero() || rows[0].EndReason != api.EndRulesChanged || rows[0].Problem != "" || rows[0].Outcome != c.Outcome || rows[0].Sent != 17 || rows[0].Received != 23 {
		t.Fatal("rules change was recorded as a failure or changed routing history", rows)
	}
	reopened, err := Open(Options{Dir: dir, LogDays: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows = reopened.Connections(api.ConnectionQuery{})
	if len(rows) != 1 || rows[0].EndReason != api.EndRulesChanged || rows[0].Problem != "" || rows[0].Outcome != c.Outcome {
		t.Fatal("normal termination reason did not survive storage", rows)
	}
}

func TestCanceledPublicationWaitsForStoreWithoutCreatingRecord(t *testing.T) {
	s, _ := testStore(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	started := make(chan struct{})
	done := make(chan uint64, 1)
	go func() { close(started); done <- s.AddContext(ctx, fixture()) }()
	<-started
	cancel()
	s.mu.Unlock()
	select {
	case id := <-done:
		if id != 0 || len(s.Connections(api.ConnectionQuery{})) != 0 {
			t.Fatal("canceled unpublished flow invented a connection record", id)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled telemetry publication did not finish")
	}
}

func TestReloadRetainsAccountingFromCompletedWrites(t *testing.T) {
	for _, reason := range []api.EndReason{"", api.EndRulesChanged} {
		t.Run(string(reason), func(t *testing.T) {
			s, _ := testStore(t, Options{})
			id := s.Add(fixture())
			s.AddBytes(id, 17, 23)
			s.EndWithReason(id, "", reason)
			before := s.Connections(api.ConnectionQuery{})[0]
			flush(t, s, true)
			// The relay can finish Write before Reload, then lose the scheduler
			// until after Reload has published the end record. Its successful
			// bytes are still part of the original connection, not a new flow.
			s.AddBytes(id, 7, 11)
			after := s.Connections(api.ConnectionQuery{})[0]
			wantSent, wantReceived := uint64(17), uint64(23)
			if reason == api.EndRulesChanged {
				wantSent, wantReceived = 24, 34
			}
			open, sent, received := s.Totals()
			if after.Sent != wantSent || after.Received != wantReceived || sent != wantSent || received != wantReceived || open != 0 {
				t.Fatal("late completion changed closure or lost committed bytes", after, open, sent, received)
			}
			if !after.Ended.Equal(before.Ended) || after.EndReason != reason || after.Outcome != before.Outcome || after.Problem != "" {
				t.Fatal("late accounting reopened or rewrote the connection", after)
			}
			rows := flush(t, s, true)
			if reason == api.EndRulesChanged && (len(rows) != 1 || rows[0].Sent != wantSent || rows[0].Received != wantReceived) {
				t.Fatal("late bytes did not publish a final update", rows)
			}
		})
	}
}
