package telemetry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func testStore(t *testing.T, opts Options) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	s.secondsSince = now
	return s, &now
}

func fixture() api.Connection {
	return api.Connection{App: api.AppRef{Path: "/Applications/Example.app", Name: "Example", BundleID: "com.example.app"}, Process: api.Process{PID: 42, Name: "Example"}, Protocol: api.TCP, Remote: "192.0.2.10:443", Domain: "example.com", Outcome: api.Routed, Interface: "en0", RuleID: "example"}
}

func flush(t *testing.T, s *Store, force bool) []api.Connection {
	t.Helper()
	events, err := s.Flush(force)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestEventCoalescingAndConnectionQueries(t *testing.T) {
	s, now := testStore(t, Options{})
	id := s.Add(fixture())
	if got := flush(t, s, false); len(got) != 1 || got[0].ID != id {
		t.Fatal(got)
	}
	*now = now.Add(100 * time.Millisecond)
	s.AddBytes(id, 10, 20)
	if got := flush(t, s, false); len(got) != 0 {
		t.Fatal("unthrottled update", got)
	}
	*now = now.Add(time.Second)
	if got := flush(t, s, false); len(got) != 1 || got[0].Sent != 10 || got[0].Received != 20 {
		t.Fatal(got)
	}
	*now = now.Add(time.Second)
	if got := flush(t, s, false); len(got) != 0 {
		t.Fatal("unchanged flow emitted again")
	}
	if got := s.Connections(api.ConnectionQuery{Open: true, Search: "EXAMPLE.COM"}); len(got) != 1 {
		t.Fatal(got)
	}
	s.End(id, "")
	if got := flush(t, s, false); len(got) != 1 || got[0].Ended.IsZero() {
		t.Fatal("missing terminal event", got)
	}
	s.End(id, "duplicate close")
	if got := s.Connections(api.ConnectionQuery{Open: true}); len(got) != 0 {
		t.Fatal(got)
	}
	open, sent, received := s.Totals()
	if open != 0 || sent != 10 || received != 20 {
		t.Fatalf("totals %d %d %d", open, sent, received)
	}
}

func TestRingIsBoundedAndClearPreservesTraffic(t *testing.T) {
	s, now := testStore(t, Options{Limit: 2})
	for range 20 {
		id := s.Add(fixture())
		s.AddBytes(id, 1, 2)
		s.End(id, "")
	}
	if len(s.records) > 4 {
		t.Fatal("unbounded pending log ring")
	}
	flush(t, s, false)
	if len(s.Connections(api.ConnectionQuery{})) != 2 {
		t.Fatal("ring limit ignored")
	}
	id := s.Add(fixture())
	s.AddBytes(id, 3, 4)
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	s.AddBytes(id, 5, 6)
	if len(s.Connections(api.ConnectionQuery{})) != 0 {
		t.Fatal("cleared active connection reappeared")
	}
	s.End(id, "")
	flush(t, s, false)
	traffic := s.Traffic(api.TrafficQuery{Since: now.Add(-time.Second), Step: time.Second})
	if len(traffic.Apps) != 1 || traffic.Apps[0].Sent != 28 || traffic.Apps[0].Received != 50 || traffic.Apps[0].Connections != 21 {
		t.Fatalf("lost traffic after clear: %+v", traffic)
	}
}

func TestHistoryDoesNotDoubleCountRetentionLevels(t *testing.T) {
	s, now := testStore(t, Options{})
	base := now.UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	for _, sample := range []struct {
		age   time.Duration
		bytes uint64
	}{{49 * time.Hour, 1}, {25 * time.Hour, 2}, {23 * time.Hour, 4}, {11 * time.Minute, 8}, {2 * time.Minute, 16}, {time.Second, 32}} {
		*now = base.Add(-sample.age)
		id := s.Add(fixture())
		s.AddBytes(id, sample.bytes, 0)
		s.End(id, "")
	}
	*now = base
	for _, tc := range []struct {
		since time.Time
		want  uint64
	}{{time.Time{}, 63}, {base.Truncate(24 * time.Hour), 56}, {base.Add(-3 * time.Minute), 48}, {base.Add(-30 * time.Second), 32}} {
		traffic := s.Traffic(api.TrafficQuery{Since: tc.since, Step: time.Second})
		if len(traffic.Apps) != 1 || traffic.Apps[0].Sent != tc.want {
			t.Fatalf("since %v: %+v, want %d", tc.since, traffic, tc.want)
		}
		var series uint64
		for _, p := range traffic.Series {
			series += p.Sent
		}
		if series != tc.want {
			t.Fatalf("series double count: %d, want %d", series, tc.want)
		}
	}
}

func TestPersistenceRetentionAndMemoryOnlyLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, now := testStore(t, Options{Dir: dir, LogDays: 2})
	id := s.Add(fixture())
	s.AddBytes(id, 123, 456)
	s.End(id, "")
	flush(t, s, true)
	log := filepath.Join(dir, "connections-"+now.Format("2006-01-02")+".jsonl")
	for _, path := range []string{log, filepath.Join(dir, "traffic.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions: %v %v", info, err)
		}
	}
	reopened, err := Open(Options{Dir: dir, LogDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	connections := reopened.Connections(api.ConnectionQuery{})
	if len(connections) != 1 || connections[0].Sent != 123 || connections[0].Ended.IsZero() {
		t.Fatal(connections)
	}
	traffic := reopened.Traffic(api.TrafficQuery{Step: time.Second})
	if len(traffic.Apps) != 1 || traffic.Apps[0].Sent != 123 || traffic.Apps[0].Received != 456 {
		t.Fatal(traffic)
	}
	old := filepath.Join(dir, "connections-"+now.AddDate(0, 0, -3).Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLogDays(2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old log retained", err)
	}
	if err := s.SetLogDays(0); err != nil {
		t.Fatal(err)
	}
	id = s.Add(fixture())
	s.AddBytes(id, 1, 2)
	s.End(id, "")
	flush(t, s, true)
	if files, err := s.logFiles(); err != nil || len(files) != 0 {
		t.Fatal("memory-only setting wrote logs", files, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "traffic.json")); err != nil {
		t.Fatal("traffic history should survive log setting", err)
	}
}

func TestRefusesSymlinkHistoryAndPublicDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "traffic.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Fatal("followed a telemetry symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "untouched" {
		t.Fatal("changed symlink target")
	}
	dir = t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Fatal("accepted public telemetry directory")
	}
}

func TestConcurrentCountersAndQueries(t *testing.T) {
	s, _ := testStore(t, Options{})
	id := s.Add(fixture())
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				s.AddBytes(id, 10, 20)
				s.Traffic(api.TrafficQuery{Step: time.Second})
				s.Connections(api.ConnectionQuery{Limit: 10})
			}
		})
	}
	wg.Wait()
	s.End(id, "")
	_, sent, received := s.Totals()
	if sent != 4000 || received != 8000 {
		t.Fatal(sent, received)
	}
}

func BenchmarkRecentTraffic(b *testing.B) {
	s, err := Open(Options{})
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	s.secondsSince = now.Add(-24 * time.Hour)
	for i := 0; i < 24*60; i++ {
		for range 10 {
			s.metric(fixture().App, "en0", 1000, 2000, 1, now.Add(-time.Duration(i)*time.Minute))
		}
	}
	s.pruneBuckets(now)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Traffic(api.TrafficQuery{Since: now.Add(-2 * time.Minute), Step: time.Second})
	}
}
