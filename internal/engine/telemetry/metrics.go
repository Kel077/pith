package telemetry

import (
	"sort"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	secondRetention = 10 * time.Minute
	minuteRetention = 24 * time.Hour
	dayRetention    = 5 * 365 * 24 * time.Hour
)

type bucket struct {
	At         int64                           `json:"at"`
	Apps       map[string]api.AppTraffic       `json:"apps"`
	Interfaces map[string]api.InterfaceTraffic `json:"interfaces"`
}

func appKey(app api.AppRef) string {
	if app.Path != "" {
		return app.Path
	}
	return "bundle:" + app.BundleID
}

func (s *Store) metric(app api.AppRef, iface string, sent, received uint64, connections int, now time.Time) {
	for _, level := range []struct {
		buckets map[int64]*bucket
		step    time.Duration
	}{{s.seconds, time.Second}, {s.minutes, time.Minute}, {s.days, 24 * time.Hour}} {
		at := now.UTC().Truncate(level.step).Unix()
		b := level.buckets[at]
		if b == nil {
			b = &bucket{At: at, Apps: make(map[string]api.AppTraffic), Interfaces: make(map[string]api.InterfaceTraffic)}
			level.buckets[at] = b
		}
		key := appKey(app)
		a := b.Apps[key]
		a.App = app
		a.Sent += sent
		a.Received += received
		a.Connections += connections
		b.Apps[key] = a
		if iface != "" {
			i := b.Interfaces[iface]
			i.Interface = iface
			i.Sent += sent
			i.Received += received
			b.Interfaces[iface] = i
		}
	}
	s.trafficDirty = true
}

func (s *Store) pruneBuckets(now time.Time) {
	for _, level := range []struct {
		buckets map[int64]*bucket
		keep    time.Duration
	}{{s.seconds, secondRetention}, {s.minutes, minuteRetention}, {s.days, dayRetention}} {
		cut := now.Add(-level.keep).Unix()
		for at := range level.buckets {
			if at < cut {
				delete(level.buckets, at)
			}
		}
	}
}

func copyBucket(b *bucket) *bucket {
	out := &bucket{At: b.At, Apps: make(map[string]api.AppTraffic), Interfaces: make(map[string]api.InterfaceTraffic)}
	for k, v := range b.Apps {
		out.Apps[k] = v
	}
	for k, v := range b.Interfaces {
		out.Interfaces[k] = v
	}
	return out
}

func subtract(whole, part *bucket) {
	for k, v := range part.Apps {
		a := whole.Apps[k]
		a.Sent -= min(a.Sent, v.Sent)
		a.Received -= min(a.Received, v.Received)
		a.Connections -= min(a.Connections, v.Connections)
		whole.Apps[k] = a
	}
	for k, v := range part.Interfaces {
		i := whole.Interfaces[k]
		i.Sent -= min(i.Sent, v.Sent)
		i.Received -= min(i.Received, v.Received)
		whole.Interfaces[k] = i
	}
}

// Traffic uses disjoint residual buckets: days minus retained minutes, minutes
// minus retained seconds, and seconds. This avoids double-counting at retention
// boundaries and after a helper restart. Since reflects the actual coarser
// boundary when historical data can no longer answer at second resolution.
func (s *Store) Traffic(q api.TrafficQuery) api.Traffic {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneBuckets(s.now())
	type atom struct {
		b   *bucket
		end int64
	}
	days, minutes := make(map[int64]atom), make(map[int64]atom)
	// The UI's one-second polling never needs to clone historical buckets.
	recent := !q.Since.IsZero() && !q.Since.Before(s.secondsSince) && !q.Since.Before(s.now().Add(-secondRetention))
	if !recent {
		for at, b := range s.days {
			days[at] = atom{copyBucket(b), at + 86400}
		}
		for at, b := range s.minutes {
			minutes[at] = atom{copyBucket(b), at + 60}
			day := time.Unix(at, 0).UTC().Truncate(24 * time.Hour).Unix()
			if a, ok := days[day]; ok {
				subtract(a.b, b)
				a.end = min(a.end, at)
				days[day] = a
			}
		}
		for at, b := range s.seconds {
			minute := time.Unix(at, 0).Truncate(time.Minute).Unix()
			if a, ok := minutes[minute]; ok {
				subtract(a.b, b)
				a.end = min(a.end, at)
				minutes[minute] = a
			}
		}
	}
	out := api.Traffic{Since: q.Since, Apps: []api.AppTraffic{}, Interfaces: []api.InterfaceTraffic{}}
	apps := make(map[string]api.AppTraffic)
	interfaces := make(map[string]api.InterfaceTraffic)
	type sampleKey struct {
		at    int64
		iface string
	}
	series := make(map[sampleKey]api.TrafficSample)
	step := q.Step
	if step > 0 && step < time.Second {
		step = time.Second
	}
	add := func(a atom) {
		at := time.Unix(a.b.At, 0).UTC()
		if !q.Since.IsZero() && !time.Unix(a.end, 0).After(q.Since) {
			return
		}
		hasData := false
		for key, v := range a.b.Apps {
			if v.Sent == 0 && v.Received == 0 && v.Connections == 0 {
				continue
			}
			hasData = true
			p := apps[key]
			p.App = v.App
			p.Sent += v.Sent
			p.Received += v.Received
			p.Connections += v.Connections
			apps[key] = p
		}
		for key, v := range a.b.Interfaces {
			if v.Sent == 0 && v.Received == 0 {
				continue
			}
			hasData = true
			p := interfaces[key]
			p.Interface = key
			p.Sent += v.Sent
			p.Received += v.Received
			interfaces[key] = p
			if step > 0 {
				k := sampleKey{at.Truncate(step).UnixNano(), key}
				sample := series[k]
				sample.At = time.Unix(0, k.at).UTC()
				sample.Interface = key
				sample.Sent += v.Sent
				sample.Received += v.Received
				series[k] = sample
			}
		}
		if hasData && (out.Since.IsZero() || at.Before(out.Since)) {
			out.Since = at
		}
	}
	for _, a := range days {
		add(a)
	}
	for _, a := range minutes {
		add(a)
	}
	for at, b := range s.seconds {
		add(atom{b, at + 1})
	}
	for _, a := range apps {
		out.Apps = append(out.Apps, a)
	}
	for _, i := range interfaces {
		out.Interfaces = append(out.Interfaces, i)
	}
	for _, v := range series {
		out.Series = append(out.Series, v)
	}
	sort.Slice(out.Apps, func(i, j int) bool {
		a, b := out.Apps[i], out.Apps[j]
		if a.Sent+a.Received == b.Sent+b.Received {
			return appKey(a.App) < appKey(b.App)
		}
		return a.Sent+a.Received > b.Sent+b.Received
	})
	sort.Slice(out.Interfaces, func(i, j int) bool {
		a, b := out.Interfaces[i], out.Interfaces[j]
		if a.Sent+a.Received == b.Sent+b.Received {
			return a.Interface < b.Interface
		}
		return a.Sent+a.Received > b.Sent+b.Received
	})
	sort.Slice(out.Series, func(i, j int) bool {
		if out.Series[i].At.Equal(out.Series[j].At) {
			return out.Series[i].Interface < out.Series[j].Interface
		}
		return out.Series[i].At.Before(out.Series[j].At)
	})
	return out
}
