package engine

import (
	"context"
	"errors"
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

func newIPv6TestEngine(t *testing.T, verify func(context.Context, *capture.Capture, *forward.Forwarder) error) (*Engine, *testDriver) {
	t.Helper()
	if verify == nil {
		verify = func(context.Context, *capture.Capture, *forward.Forwarder) error { return nil }
	}
	base, driver := newTestEngine(t, verify)
	driver.routes[0].Interface = "example0"
	driver.routes = append(driver.routes, capture.Route{Prefix: netip.MustParsePrefix("::/0"), Interface: "example0", Index: 1})
	opts := base.opts
	opts.Interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "example0", Up: true}}, nil
	}
	opts.DataDir, opts.Scopes = filepath.Join(t.TempDir(), "ipv6-profile"), nil
	opts.ProbeAddress, opts.ProbeServerName = "[2001:db8::20]:443", "example.com"
	opts.TestProfile = &testprofile.Profile{Mode: testprofile.HostIPv6, Lease: 60 * time.Second, Target: opts.ProbeAddress, ServerName: opts.ProbeServerName}
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

func TestIPv6ProfileCapturesOnlyItsHostAndPreservesIPv4(t *testing.T) {
	verified := false
	e, d := newIPv6TestEngine(t, func(_ context.Context, c *capture.Capture, _ *forward.Forwarder) error {
		verified = true
		if !c.Plan.Scoped || !slices.Equal(c.Plan.Prefixes, []netip.Prefix{netip.MustParsePrefix("2001:db8::20/128")}) || !c.Plan.Addresses.IPv6.IsValid() {
			t.Error("wrong IPv6 capture plan")
		}
		return nil
	})
	e.opts.Interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "example0", Up: true, DNS: []string{"192.0.2.53", "2001:db8::53"}}}, nil
	}
	before, _ := d.Snapshot()
	cfg := api.DefaultConfig()
	cfg.Settings.IPv6, cfg.Settings.SafeStart = true, true
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if !verified || e.scopedLease != 60*time.Second {
		t.Fatal("IPv6 profile skipped verification or its lease")
	}
	active, _ := d.Snapshot()
	if len(active) != len(before)+1 {
		t.Fatal("IPv6 host added wider routes")
	}
	probe, err := prepareProbe(context.Background(), e.opts, true)
	if err != nil || probe.address.String() != "[2001:db8::20]:443" {
		t.Fatal("SafeStart did not retain exact IPv6 target", err)
	}
	if _, err := prepareProbe(context.Background(), e.opts, false); err == nil {
		t.Fatal("IPv4 fallback satisfied IPv6 SafeStart")
	}
	bad := cfg.Settings
	bad.IPv6 = false
	if api.Code(e.Reload(context.Background(), cfg.Scenes[0], bad)) != api.ErrInvalidConfig {
		t.Fatal("reload disabled IPv6")
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.Snapshot()
	if !slices.Equal(before, after) {
		t.Fatal("Stop changed original IPv4 or IPv6 routes")
	}
}

func TestIPv6ProfileFailureAndExpiryRemoveOnlyOwnedRoutes(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease", true: "SafeStart"}[fail], func(t *testing.T) {
			e, d := newIPv6TestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error {
				if fail {
					return errors.New("synthetic IPv6 failure")
				}
				return nil
			})
			e.scopedLease = 100 * time.Millisecond
			before, _ := d.Snapshot()
			cfg := api.DefaultConfig()
			cfg.Settings.IPv6, cfg.Settings.SafeStart = true, true
			err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings)
			if fail {
				if api.Code(err) != api.ErrNetworkLost {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case <-d.devices[0].done:
			case <-time.After(time.Second):
				t.Fatal("IPv6 capture outlived its lease")
			}
			if err := e.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, _ := d.Snapshot()
			if !slices.Equal(before, after) {
				t.Fatal("IPv6 teardown lost original routes")
			}
		})
	}
}
