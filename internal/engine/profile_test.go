package engine

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
	"github.com/Kel077/pith/internal/testprofile"
)

func newFullTestEngine(t *testing.T, verify func(context.Context, *capture.Capture, *forward.Forwarder) error) (*Engine, *testDriver) {
	t.Helper()
	if verify == nil {
		verify = func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil }
	}
	base, driver := newTestEngine(t, verify)
	opts := base.opts
	opts.DataDir = filepath.Join(t.TempDir(), "profile")
	opts.Scopes = nil
	opts.ProbeAddress, opts.ProbeServerName = "192.0.2.20:443", "example.com"
	opts.TestProfile = &testprofile.Profile{Mode: testprofile.FullIPv4, Lease: 120 * time.Second, Target: opts.ProbeAddress, ServerName: opts.ProbeServerName}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return e, driver
}

func fullTestConfig() api.Config {
	cfg := api.DefaultConfig()
	cfg.Settings.IPv6, cfg.Settings.SafeStart = false, true
	return cfg
}

func TestFullProfileStartsWithGatewayDNSAndReportsSkippedObservation(t *testing.T) {
	e, d := newFullTestEngine(t, nil)
	d.routes = []capture.Route{
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Gateway: netip.MustParseAddr("192.0.2.1"), Interface: "example0", Index: 1},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 1},
		{Prefix: netip.MustParsePrefix("192.0.2.1/32"), Interface: "example0", Index: 1},
	}
	e.opts.Interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "example0", Up: true, DNS: []string{"192.0.2.1"}}}, nil
	}
	before, _ := d.Snapshot()
	cfg := fullTestConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal("full profile failed on configured gateway DNS", err)
	}
	want := api.Warning{Code: api.WarnDNSUnobserved, Detail: "192.0.2.1"}
	status, err := e.Status(context.Background())
	if err != nil || !slices.Contains(status.Warnings, want) {
		t.Fatal("running status omitted DNS observation warning", status, err)
	}
	if err := e.Reload(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	status, err = e.Status(context.Background())
	if err != nil || !slices.Contains(status.Warnings, want) {
		t.Fatal("reload lost DNS warning", status, err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.Snapshot()
	if !slices.Equal(before, after) {
		t.Fatal("full capture cleanup altered the gateway route")
	}
}

func TestFullProfileLeaseClosesCaptureWithoutControllerOrLifecycleLock(t *testing.T) {
	e, d := newFullTestEngine(t, nil)
	if e.scopedLease != 120*time.Second {
		t.Fatal("profile lease was not installed")
	}
	e.scopedLease = 100 * time.Millisecond // Accelerate only this synthetic driver.
	cfg := fullTestConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if e.run.capture.Plan.Scoped || len(e.run.capture.Plan.Prefixes) != 2 {
		t.Fatal("test profile did not use full capture planning")
	}
	deadline := e.run.leaseUntil
	if err := e.Reload(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if e.run.leaseUntil != deadline {
		t.Fatal("reload or repeated Start extended the lease")
	}
	e.lifecycle.Lock()
	select {
	case <-d.devices[0].done:
	case <-time.After(time.Second):
		e.lifecycle.Unlock()
		t.Fatal("lease waited for lifecycle lock/controller")
	}
	routes, _ := d.Snapshot()
	e.lifecycle.Unlock()
	if len(routes) != 1 {
		t.Fatal("watchdog left full capture routes")
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFullProfileRejectsUnsafeSettingsWithoutRestart(t *testing.T) {
	e, d := newFullTestEngine(t, nil)
	cfg := fullTestConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	for _, ipv6 := range []bool{false, true} {
		bad := cfg.Settings
		bad.IPv6, bad.SafeStart = ipv6, ipv6
		for _, call := range []func(context.Context, api.Scene, api.Settings) error{e.Start, e.Reload} {
			if api.Code(call(context.Background(), cfg.Scenes[0], bad)) != api.ErrInvalidConfig {
				t.Fatal("profile settings bypassed")
			}
		}
	}
	if len(d.devices) != 1 || e.run == nil {
		t.Fatal("rejected settings restarted/stopped capture")
	}
	opts := e.opts
	opts.Scopes = []netip.Prefix{netip.MustParsePrefix("192.0.2.20/32")}
	if _, err := New(opts); err == nil {
		t.Fatal("mixed full/scoped mode accepted")
	}
}

func TestFullProfileCoversStartupAndRejectsChangedDNSBeforeUtun(t *testing.T) {
	t.Run("startup-deadline", func(t *testing.T) {
		e, d := newFullTestEngine(t, func(ctx context.Context, _ *capture.Capture, _ *forward.Forwarder) error {
			<-ctx.Done()
			return ctx.Err()
		})
		e.scopedLease = 30 * time.Millisecond
		cfg := fullTestConfig()
		if api.Code(e.Start(context.Background(), cfg.Scenes[0], cfg.Settings)) != api.ErrNetworkLost {
			t.Fatal("startup outlived its lease")
		}
		routes, _ := d.Snapshot()
		if len(routes) != 1 {
			t.Fatal("expired SafeStart left routes")
		}
	})
	t.Run("new-dns-host", func(t *testing.T) {
		e, d := newFullTestEngine(t, nil)
		d.routes = append(d.routes, capture.Route{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Interface: "example0", Index: 2})
		e.opts.Interfaces = func(context.Context) ([]api.Interface, error) {
			return []api.Interface{{Name: "example0", DNS: []string{"192.0.2.53"}}}, nil
		}
		cfg := fullTestConfig()
		if e.Start(context.Background(), cfg.Scenes[0], cfg.Settings) == nil || len(d.devices) != 0 {
			t.Fatal("unreviewed DNS route created a tunnel")
		}
	})
}
