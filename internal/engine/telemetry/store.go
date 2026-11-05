// Package telemetry keeps connection history and traffic without one disk write
// per packet. It owns no goroutines; the engine calls Flush on its event tick.
package telemetry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type Options struct {
	Dir     string // empty: all data remains in memory
	LogDays int
	Limit   int // closed connections retained in memory; default 5000
}

type record struct {
	connection                api.Connection
	dirty, diskDirty, visible bool
	reported, written         time.Time
}

type Store struct {
	mu                     sync.Mutex
	opts                   Options
	now                    func() time.Time
	next                   uint64
	records                map[uint64]*record
	closed                 []uint64
	open                   int
	sent, received         uint64
	seconds, minutes, days map[int64]*bucket
	quarters               map[int64]*bucket
	trafficDirty           bool
	lastTrafficWrite       time.Time
	lastPrune              time.Time
	secondsSince           time.Time
}

func Open(opts Options) (*Store, error) {
	if opts.LogDays < 0 {
		return nil, fmt.Errorf("negative log retention")
	}
	if opts.Limit <= 0 {
		opts.Limit = 5000
	}
	s := &Store{opts: opts, now: time.Now, next: uint64(time.Now().UnixNano()), records: make(map[uint64]*record), seconds: make(map[int64]*bucket), minutes: make(map[int64]*bucket), days: make(map[int64]*bucket)}
	s.quarters = make(map[int64]*bucket)
	s.secondsSince = s.now()
	if opts.Dir != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Add(connection api.Connection) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	connection.ID = s.next
	if connection.Started.IsZero() {
		connection.Started = s.now()
	}
	s.records[connection.ID] = &record{connection: connection, dirty: true, diskDirty: true, visible: true}
	if connection.Ended.IsZero() {
		s.open++
	} else {
		s.closed = append(s.closed, connection.ID)
	}
	s.metric(connection.App, connection.Interface, connection.Sent, connection.Received, 1, s.now())
	s.sent += connection.Sent
	s.received += connection.Received
	s.trim()
	return connection.ID
}

func (s *Store) AddBytes(id uint64, sent, received uint64) {
	if sent == 0 && received == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[id]
	if r == nil || !r.connection.Ended.IsZero() {
		return
	}
	r.connection.Sent += sent
	r.connection.Received += received
	r.dirty = true
	r.diskDirty = true
	s.sent += sent
	s.received += received
	s.metric(r.connection.App, r.connection.Interface, sent, received, 0, s.now())
}

func (s *Store) End(id uint64, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[id]
	if r == nil || !r.connection.Ended.IsZero() {
		return
	}
	r.connection.Ended = s.now()
	if problem != "" {
		r.connection.Problem = problem
	}
	s.open--
	if !r.visible {
		delete(s.records, id)
		return
	}
	r.dirty = true
	s.closed = append(s.closed, id)
	r.diskDirty = true
	s.trim()
}

func (s *Store) trim() {
	for len(s.closed) > s.opts.Limit {
		id := s.closed[0]
		// Allow one extra ring of pending snapshots, bounded even under a flood
		// of short-lived flows. Traffic totals remain complete when it overflows.
		if r := s.records[id]; r != nil && (r.dirty || r.diskDirty) && len(s.closed) <= 2*s.opts.Limit {
			break
		}
		delete(s.records, id)
		s.closed = s.closed[1:]
	}
}

func (s *Store) Totals() (open int, sent, received uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open, s.sent, s.received
}

func (s *Store) Connections(q api.ConnectionQuery) []api.Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.Connection, 0)
	search := strings.ToLower(q.Search)
	for _, r := range s.records {
		c := r.connection
		if !r.visible || !q.Since.IsZero() && c.Started.Before(q.Since) || q.Open && !c.Ended.IsZero() || q.App != "" && c.App.Path != q.App {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(c.App.Name+"\n"+c.App.Path+"\n"+c.Process.Name+"\n"+c.Domain+"\n"+c.Remote+"\n"+c.RuleID), search) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Started.Equal(out[j].Started) {
			return out[i].ID > out[j].ID
		}
		return out[i].Started.After(out[j].Started)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.removeLogs(); err != nil {
		return err
	}
	for id, r := range s.records {
		if r.connection.Ended.IsZero() {
			r.visible = false
			r.dirty = false
			r.diskDirty = false
		} else {
			delete(s.records, id)
		}
	}
	s.closed = nil
	return nil
}

func (s *Store) SetLogDays(days int) error {
	if days < 0 {
		return fmt.Errorf("negative log retention")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.LogDays = days
	s.trafficDirty = true
	return s.pruneLogs(s.now())
}

// Flush coalesces byte updates to at most one per second per connection. Open
// and terminal snapshots are also emitted, including short-lived connections.
// Disk checkpoints for open flows are limited to once per ten seconds.
func (s *Store) Flush(force bool) ([]api.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneBuckets(now)
	var events, writes []api.Connection
	for _, r := range s.records {
		if !r.visible {
			continue
		}
		terminal := !r.connection.Ended.IsZero()
		if r.dirty && (force || r.reported.IsZero() || terminal || now.Sub(r.reported) >= time.Second) {
			events = append(events, r.connection)
			r.reported = now
			r.dirty = false
		}
		if r.diskDirty && (s.opts.Dir == "" || s.opts.LogDays == 0 || force || terminal || r.written.IsZero() || now.Sub(r.written) >= 10*time.Second) {
			writes = append(writes, r.connection)
			r.diskDirty = false
			r.written = now
		}
	}
	if err := s.writeLogs(writes, now, force); err != nil {
		for _, c := range writes {
			if r := s.records[c.ID]; r != nil {
				r.diskDirty = true
				r.written = time.Time{}
			}
		}
		return events, err
	}
	if s.opts.Dir != "" && s.trafficDirty && (force || now.Sub(s.lastTrafficWrite) >= 15*time.Second) {
		if err := s.writeTraffic(); err != nil {
			return events, err
		}
		s.lastTrafficWrite = now
		s.trafficDirty = false
	}
	if force || now.Sub(s.lastPrune) >= time.Hour {
		if err := s.pruneLogs(now); err != nil {
			return events, err
		}
		s.lastPrune = now
	}
	s.trim()
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return events, nil
}
