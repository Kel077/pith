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
