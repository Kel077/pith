package telemetry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type persisted struct {
	Version int               `json:"version"`
	LogDays int               `json:"logDays"`
	Minutes map[int64]*bucket `json:"minutes"`
	Days    map[int64]*bucket `json:"days"`
}

func (s *Store) openFile(name string, flags int) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(s.opts.Dir, name), flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("telemetry path is not a regular file: %s", name)
	}
	return f, nil
}

func (s *Store) load() error {
	if err := os.MkdirAll(s.opts.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.opts.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("telemetry directory must be a private directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("telemetry directory has an unexpected owner")
	}
	f, err := s.openFile("traffic.json", os.O_RDONLY)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var state persisted
		decoder := json.NewDecoder(io.LimitReader(f, 128<<20))
		decodeErr := decoder.Decode(&state)
		f.Close()
		if decodeErr != nil {
			return fmt.Errorf("read traffic history: %w", decodeErr)
		}
		if state.Version != 1 || state.LogDays < 0 {
			return fmt.Errorf("unsupported traffic history")
		}
		s.opts.LogDays = state.LogDays
		if state.Minutes != nil {
			s.minutes = state.Minutes
		}
		if state.Days != nil {
			s.days = state.Days
		}
		for _, level := range []map[int64]*bucket{s.minutes, s.days} {
			for at, b := range level {
				if b == nil || b.At != at || b.Apps == nil || b.Interfaces == nil {
					return fmt.Errorf("invalid traffic bucket")
				}
			}
		}
	}
	now := s.now()
	s.pruneBuckets(now)
	if err := s.pruneLogs(now); err != nil {
		return err
	}
	files, err := s.logFiles()
	if err != nil {
		return err
	}
	// Load newest files first. The disk retains the configured history; the UI
	// receives the bounded recent ring, including latest snapshots by ID.
	for i := len(files) - 1; i >= 0; i-- {
		if len(s.records) >= s.opts.Limit {
			break
		}
		f, err := s.openFile(files[i], os.O_RDONLY)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		start := max(int64(0), info.Size()-(8<<20))
		if _, err = f.Seek(start, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		if start > 0 {
			scanner.Scan()
		} // discard a possibly partial first line
		day := make(map[uint64]api.Connection)
		for scanner.Scan() {
			var c api.Connection
			if json.Unmarshal(scanner.Bytes(), &c) == nil && c.ID != 0 {
				day[c.ID] = c
			}
		}
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return scanErr
		}
		ids := make([]uint64, 0, len(day))
		for id := range day {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
		for _, id := range ids {
			if _, exists := s.records[id]; exists {
				continue
			}
			if len(s.records) >= s.opts.Limit {
				break
			}
			c := day[id]
			if c.Ended.IsZero() {
				c.Ended = now
				c.Problem = "helper exited before the connection closed"
			}
			s.records[id] = &record{connection: c, visible: true}
			s.closed = append(s.closed, id)
			s.next = max(s.next, id)
		}
	}
	sort.Slice(s.closed, func(i, j int) bool { return s.closed[i] < s.closed[j] })
	return nil
}

func (s *Store) logFiles() ([]string, error) {
	if s.opts.Dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if _, ok := logDate(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func logDate(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, "connections-") || !strings.HasSuffix(name, ".jsonl") {
		return time.Time{}, false
	}
	date, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(name, "connections-"), ".jsonl"))
	return date, err == nil
}

func (s *Store) pruneLogs(now time.Time) error {
	files, err := s.logFiles()
	if err != nil {
		return err
	}
	cut := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -min(s.opts.LogDays, 36500)+1)
	for _, name := range files {
		date, _ := logDate(name)
		if s.opts.LogDays == 0 || date.Before(cut) {
			if err := os.Remove(filepath.Join(s.opts.Dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) removeLogs() error {
	files, err := s.logFiles()
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := os.Remove(filepath.Join(s.opts.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeLogs(connections []api.Connection, now time.Time, sync bool) error {
	if len(connections) == 0 || s.opts.Dir == "" || s.opts.LogDays == 0 {
		return nil
	}
	name := "connections-" + now.UTC().Format("2006-01-02") + ".jsonl"
	f, err := s.openFile(name, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return err
	}
	defer f.Close()
	// An interrupted append can leave an incomplete JSON line. Separate it
	// before the next snapshot so recovery loses only that incomplete record.
	if info, err := f.Stat(); err != nil {
		return err
	} else if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	w := bufio.NewWriter(f)
	encoder := json.NewEncoder(w)
	for _, c := range connections {
		if err := encoder.Encode(c); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if sync {
		return f.Sync()
	}
	return nil
}

func (s *Store) writeTraffic() error {
	f, err := os.CreateTemp(s.opts.Dir, ".traffic-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	state := persisted{Version: 1, LogDays: s.opts.LogDays, Minutes: s.minutes, Days: s.days}
	if err := json.NewEncoder(f).Encode(state); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.opts.Dir, "traffic.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.opts.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
