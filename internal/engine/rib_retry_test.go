package engine

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
)

// This reader simulates an exhausted native sysctl retry series. The lower
// layer's buffer growth and individual sysctl failures are tested in rib.
type growingRIBDriver struct {
	*testDriver
	failures atomic.Int64 // -1 keeps the table unreadable until teardown.
	reads    atomic.Int64
	failed   chan time.Time
	omitHost bool
}

func (d *growingRIBDriver) SnapshotDetailsContext(ctx context.Context) ([]capture.Route, []capture.RouteDetail, error) {
	routes, err := d.testDriver.Snapshot()
	if err != nil || !slices.ContainsFunc(routes, func(r capture.Route) bool { return r.Index == 42 }) {
		return routes, nil, err
	}
	d.reads.Add(1)
	if n := d.failures.Load(); n != 0 {
		if n > 0 {
			d.failures.Add(-1)
		}
		if d.failed != nil {
			select {
			case d.failed <- time.Now():
			default:
			}
		}
		return nil, nil, os.NewSyscallError("sysctl", syscall.ENOMEM)
	}
	if d.omitHost {
		routes = slices.DeleteFunc(routes, func(r capture.Route) bool { return r.Index == 42 })
	}
	return routes, nil, ctx.Err()
}

func TestStartupOwnershipRetriesGrowingRIB(t *testing.T) {
	for _, badOwnership := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "readable-but-wrong"}[badOwnership], func(t *testing.T) {
			verified := false
			e, base := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error {
				verified = true
				return nil
			})
			d := &growingRIBDriver{testDriver: base, omitHost: badOwnership}
			d.failures.Store(4)
			e.opts.Driver, e.opts.Scopes = d, nil // All eight routes in memory only.
			cfg := api.DefaultConfig()
			start := time.Now()
			err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings)
			status, _ := e.Status(context.Background())
			if badOwnership {
				if api.Code(err) != api.ErrNetworkLost || verified || d.reads.Load() != 5 || status.State != api.Failed {
					t.Fatal("readable wrong ownership was retried or accepted", err, verified, d.reads.Load(), status)
				}
				if time.Since(start) >= 2*time.Second {
					t.Fatal("semantic ownership failure waited for another read window")
				}
			} else {
				if err != nil || !verified || status.State != api.Running || d.reads.Load() < 5 {
					t.Fatal("temporary ENOMEM stopped startup", err, verified, d.reads.Load(), status)
				}
				if !slices.ContainsFunc(status.RouteDiagnostics, func(s string) bool { return strings.Contains(s, "snapshot recovered") }) {
					t.Fatal("recovered observation was not retained", status.RouteDiagnostics)
				}
				select {
				case <-base.devices[0].done:
					t.Fatal("recovered capture device closed")
				default:
				}
			}
			if err := e.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			routes, _ := base.Snapshot()
			if len(routes) != 1 || routes[0].Index != 1 {
				t.Fatal("retry or cleanup changed the external table", routes)
			}
		})
	}
}

func TestPersistentUnreadableRIBStopsOnlyAfterObservationWindow(t *testing.T) {
	e, base := newTestEngine(t, nil)
	d := &growingRIBDriver{testDriver: base, failed: make(chan time.Time, 1)}
	e.opts.Driver = d
	cfg := api.DefaultConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	d.failures.Store(-1)
	base.changes <- capture.Change{Index: 42, Interface: true, Up: true}
	var began time.Time
	select {
	case began = <-d.failed:
	case <-time.After(time.Second):
		t.Fatal("ownership read did not start")
	}
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-base.devices[0].done:
		t.Fatal("temporary unreadability immediately closed capture")
	case <-timer.C:
	}
	status, _ := e.Status(context.Background())
	if status.State != api.Running {
		t.Fatal("capture failed before the bounded recovery window", status)
	}
	waitTopologyStop(t, e, base)
	if elapsed := time.Since(began); elapsed < 1900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatal("ownership retry did not respect its two-second bound", elapsed)
	}
	status, _ = e.Status(context.Background())
	if !slices.ContainsFunc(status.RouteDiagnostics, func(s string) bool { return strings.Contains(s, "snapshot remained unavailable") }) {
		t.Fatal("persistent observation failure lost its diagnosis", status.RouteDiagnostics)
	}
	if !strings.Contains(status.Error.Detail, "cannot allocate memory") {
		t.Fatal("underlying sysctl error was discarded", status.Error)
	}
}
