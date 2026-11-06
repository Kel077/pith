package engine

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
)

type stopSnapshotDriver struct {
	*testDriver
	stopping atomic.Bool
	fail     atomic.Int32
	after    bool
}

func (d *stopSnapshotDriver) Snapshot() ([]capture.Route, error) {
	if d.stopping.Load() {
		d.mu.Lock()
		device := d.devices[len(d.devices)-1]
		d.mu.Unlock()
		closed := false
		select {
		case <-device.done:
			closed = true
		default:
		}
		if closed == d.after && d.fail.Add(-1) >= 0 {
			return nil, context.DeadlineExceeded
		}
	}
	return d.testDriver.Snapshot()
}

// The running observer has a separate successful observation. Fault injection
// is limited to the Close snapshots so its 250 ms read deadline is reproducible.
func (d *stopSnapshotDriver) SnapshotDetailsContext(context.Context) ([]capture.Route, []capture.RouteDetail, error) {
	routes, err := d.testDriver.Snapshot()
	return routes, nil, err
}

func TestStopAfterVPNRebuildRecoversTransientSnapshotDeadline(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "before-device-close"
		if after {
			name = "after-device-close"
		}
		t.Run(name, func(t *testing.T) {
			e, base := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil })
			d := &stopSnapshotDriver{testDriver: base, after: after}
			e.opts.Driver, e.opts.Scopes = d, nil
			e.opts.Interfaces = func(context.Context) ([]api.Interface, error) {
				return []api.Interface{{Name: "example0", Up: true}, {Name: "utun50", Up: true}}, nil
			}
			base.routes = []capture.Route{
				{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "example0", Index: 1},
				{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun50", Index: 50, Scoped: true},
				{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "utun50", Index: 50},
			}
			cfg := api.DefaultConfig()
			if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
				t.Fatal(err)
			}
			g := e.run.guard
			base.mu.Lock()
			base.routes = slices.DeleteFunc(base.routes, func(r capture.Route) bool { return r.Index == 50 })
			base.mu.Unlock()
			if err := g.Observe(capture.Change{Index: 50, Prefix: netip.MustParsePrefix("0.0.0.0/0"), Deleted: true, ScopeKnown: true, Scoped: true}); err != nil {
				t.Fatal(err)
			}
			base.mu.Lock()
			base.routes = append(base.routes,
				capture.Route{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "utun51", Index: 51, Scoped: true},
				capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "utun51", Index: 51})
			base.mu.Unlock()
			if err := g.Observe(capture.Change{Index: 51, Prefix: netip.MustParsePrefix("0.0.0.0/0"), Added: true, ScopeKnown: true, Scoped: true}); err != nil {
				t.Fatal(err)
			}
			before, _ := e.Status(context.Background())
			d.fail.Store(1)
			d.stopping.Store(true)
			if err := e.Stop(context.Background()); err != nil {
				t.Fatal("transient cleanup observation poisoned Stop", err)
			}
			st, _ := e.Status(context.Background())
			if st.State != api.Stopped || st.LastCapture == nil || st.LastCapture.EndedAt.IsZero() || st.LastCapture.Token != before.LastCapture.Token {
				t.Fatal("confirmed capture closure was not retained", st)
			}
			routes, _ := base.Snapshot()
			if len(routes) != 3 || slices.ContainsFunc(routes, func(r capture.Route) bool { return r.Index == 42 || r.Index == 50 }) {
				t.Fatal("Stop changed rebuilt VPN routes or left capture", routes)
			}
			data, err := os.ReadFile(filepath.Join(e.opts.DataDir, lifecycleFile))
			var records []lifecycleRecord
			if err != nil || json.Unmarshal(data, &records) != nil || !slices.ContainsFunc(records, func(r lifecycleRecord) bool { return r.Phase == "stop.complete" && r.ErrorKind == "" }) || !slices.ContainsFunc(records, func(r lifecycleRecord) bool { return r.ErrorKind == "deadline" }) {
				t.Fatal("Stop did not persist both the transient failure and successful completion", err)
			}
		})
	}
}
