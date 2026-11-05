package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
	"golang.zx2c4.com/wireguard/tun"
)

type testDriver struct {
	mu          sync.Mutex
	routes      []capture.Route
	devices     []*testDevice
	changes     chan capture.Change
	snapshotErr error
	indices     map[string]int
	assigned    []netip.Prefix
}
type testDevice struct {
	driver  *testDriver
	done    chan struct{}
	events  chan tun.Event
	fault   chan error
	packets chan []byte
	once    sync.Once
}

func (d *testDevice) File() *os.File           { return nil }
func (d *testDevice) Name() (string, error)    { return "utun42", nil }
func (d *testDevice) MTU() (int, error)        { return 1500, nil }
func (d *testDevice) BatchSize() int           { return 1 }
func (d *testDevice) Events() <-chan tun.Event { return d.events }
func (d *testDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-d.done:
		return 0, net.ErrClosed
	case err := <-d.fault:
		return 0, err
	case packet := <-d.packets:
		sizes[0] = copy(bufs[0][offset:], packet)
		return 1, nil
	}
}
func (d *testDevice) Write(bufs [][]byte, _ int) (int, error) { return len(bufs), nil }
func (d *testDevice) Close() error {
	d.once.Do(func() {
		d.driver.mu.Lock()
		d.driver.routes = slices.DeleteFunc(d.driver.routes, func(r capture.Route) bool { return r.Index == 42 })
		d.driver.mu.Unlock()
		close(d.done)
		close(d.events)
	})
	return nil
}
func (d *testDriver) Snapshot() ([]capture.Route, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.routes), d.snapshotErr
}
func (d *testDriver) BootSession() (string, error) { return "test-boot-one", nil }
func (d *testDriver) InterfacePrefixes() ([]netip.Prefix, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.assigned), nil
}
func (d *testDriver) Index(name string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if index, ok := d.indices[name]; ok {
		return index, nil
	}
	if name == "utun42" {
		return 42, nil
	}
	for _, route := range d.routes {
		if route.Interface == name {
			return route.Index, nil
		}
	}
	return 0, errors.New("test interface absent")
}
func (d *testDriver) Create(int) (tun.Device, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	device := &testDevice{driver: d, done: make(chan struct{}), events: make(chan tun.Event), fault: make(chan error, 1), packets: make(chan []byte, 1)}
	d.devices = append(d.devices, device)
	return device, nil
}
func (d *testDriver) Configure(context.Context, string, capture.TunnelAddresses) error { return nil }
func (d *testDriver) Add(_ context.Context, p netip.Prefix, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes = append(d.routes, capture.Route{Prefix: p, Interface: name, Index: 42})
	return nil
}
func (d *testDriver) Delete(_ context.Context, p netip.Prefix, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes = slices.DeleteFunc(d.routes, func(r capture.Route) bool { return r.Index == 42 && r.Prefix == p })
	return nil
}

func newTestEngine(t *testing.T, verify func(context.Context, *capture.Capture, *forward.Forwarder) error) (*Engine, *testDriver) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	driver := &testDriver{routes: []capture.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0", Index: 1}}}
	if verify == nil {
		verify = func(ctx context.Context, c *capture.Capture, _ *forward.Forwarder) error {
			routes, _ := driver.Snapshot()
			if len(routes) != 2 || !c.Plan.Scoped {
				t.Error("verification ran before scoped capture")
			}
			return ctx.Err()
		}
	}
	e, err := New(Options{DataDir: dir, Scopes: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}, Driver: driver, Verify: verify,
		ExistingSockets: func(api.Protocol) ([]attribution.Socket, error) { return nil, nil },
		WatchRoutes: func(ctx context.Context) (<-chan capture.Change, error) {
			driver.changes = make(chan capture.Change, 64)
			ch := driver.changes
			go func() { <-ctx.Done(); close(ch) }()
			return ch, nil
		},
		Interfaces: func(context.Context) ([]api.Interface, error) {
			return []api.Interface{{Name: "en0", Label: "Example Ethernet", Up: true}}, nil
		},
		WatchInterfaces: func(ctx context.Context) (<-chan []api.Interface, error) {
			ch := make(chan []api.Interface)
			go func() { <-ctx.Done(); close(ch) }()
			return ch, nil
		},
	})
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

func TestIdleStartReloadAndStop(t *testing.T) {
	e, d := newTestEngine(t, nil)
	ctx := context.Background()
	cfg := api.DefaultConfig()
	if len(d.devices) != 0 {
		t.Fatal("construction changed the network")
	}
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := e.Subscribe(subCtx)
	if err != nil {
		t.Fatal(err)
	}
	if status := <-events; status.Status == nil || status.Status.State != api.Stopped {
		t.Fatal(status)
	}
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	old := e.run.router.Policy()
	cfg.Scenes[0].Rules = []api.Rule{{ID: "block", Enabled: true, Action: api.Block, Protocol: api.AnyProtocol}}
	if err := e.Reload(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	if old == e.run.router.Policy() || len(d.devices) != 1 {
		t.Fatal("reload restarted capture or reused the policy")
	}
	status, _ := e.Status(ctx)
	if status.State != api.Running || status.SceneID != "default" {
		t.Fatal(status)
	}
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.opts.DataDir, "capture.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal survived clean stop", err)
	}
	routes, _ := d.Snapshot()
	if len(routes) != 1 || routes[0].Index != 1 {
		t.Fatal("original routes not restored", routes)
	}
	select {
	case <-d.devices[0].done:
	default:
		t.Fatal("utun not closed")
	}
	for _, want := range []api.State{api.Starting, api.Running, api.Running, api.Stopping, api.Stopped} {
		select {
		case event := <-events:
			if event.Status == nil || event.Status.State != want {
				t.Fatal(want, event)
			}
		case <-time.After(time.Second):
			t.Fatal("missing state", want)
		}
	}
}

func TestSafeStartFailureRestoresCapture(t *testing.T) {
	e, d := newTestEngine(t, func(context.Context, *capture.Capture, *forward.Forwarder) error {
		return errors.New("simulated internet failure")
	})
	cfg := api.DefaultConfig()
	if err := e.Start(context.Background(), cfg.Scenes[0], cfg.Settings); api.Code(err) != api.ErrNetworkLost {
		t.Fatal(err)
	}
	status, _ := e.Status(context.Background())
	if status.State != api.Failed || status.Error.Code != api.ErrNetworkLost {
		t.Fatal(status)
	}
	routes, _ := d.Snapshot()
	if len(routes) != 1 {
		t.Fatal("SafeStart left capture routes", routes)
	}
	select {
	case <-d.devices[0].done:
	default:
		t.Fatal("SafeStart left utun open")
	}
	if e.run != nil {
		t.Fatal("failed start left a session")
	}
}

func TestTunnelFailureStopsAutomatically(t *testing.T) {
	e, d := newTestEngine(t, nil)
	cfg := api.DefaultConfig()
	ctx := context.Background()
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	d.devices[0].fault <- errors.New("simulated tunnel read failure")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := e.Status(ctx)
		if status.State == api.Failed {
			routes, _ := d.Snapshot()
			if len(routes) != 1 || status.Error.Code != api.ErrNetworkLost {
				t.Fatal(status, routes)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fault did not restore the network")
}

func TestScopedCaptureLeaseStopsAfterControllerDisappears(t *testing.T) {
	e, d := newTestEngine(t, nil)
	if e.scopedLease != 5*time.Minute {
		t.Fatal("scoped capture lacks its bounded default lifetime")
	}
	e.scopedLease = 250 * time.Millisecond
	cfg := api.DefaultConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := e.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	run := e.run
	deadline := run.leaseUntil
	e.mu.Unlock()
	if err := e.Reload(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	extended := run.leaseUntil != deadline
	e.mu.Unlock()
	if extended {
		t.Fatal("hot reload extended a development capture lease")
	}
	for {
		select {
		case event := <-events:
			if event.Status == nil || event.Status.State != api.Failed {
				continue
			}
			routes, _ := d.Snapshot()
			if len(routes) != 1 || event.Status.Error == nil || event.Status.Error.Code != api.ErrNetworkLost {
				t.Fatal("expired test left capture active", event.Status)
			}
			return
		case <-ctx.Done():
			t.Fatal("scoped capture outlived its lease")
		}
	}
}

func TestTickPublishesTrafficAndStopsItsWatcher(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	cfg := api.DefaultConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.Start(ctx, cfg.Scenes[0], cfg.Settings); err != nil {
		t.Fatal(err)
	}
	events, err := e.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-events
	id := e.store.Add(api.Connection{App: api.AppRef{Path: "/usr/bin/example"}, Protocol: api.TCP, Remote: "192.0.2.10:443", Outcome: api.System, Interface: "en0"})
	e.store.AddBytes(id, 100, 200)
	gotConnection, gotRate := false, false
	for !gotConnection || !gotRate {
		select {
		case event := <-events:
			if event.Connection != nil {
				gotConnection = event.Connection.ID == id && event.Connection.Sent == 100
			}
			if event.Status != nil {
				gotRate = event.Status.Sent == 100 && event.Status.Received == 200 && event.Status.SendRate > 0
			}
		case <-ctx.Done():
			t.Fatal("missing coalesced telemetry events")
		}
	}
	e.store.End(id, "")
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProbeScopeSelectionNeedsNoNetwork(t *testing.T) {
	p, err := prepareProbe(context.Background(), Options{Scopes: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}, ProbeAddress: "192.0.2.10:443", ProbeServerName: "example.com"}, false)
	if err != nil || p.address.String() != "192.0.2.10:443" || p.server != "example.com" {
		t.Fatal(p, err)
	}
	if _, err := prepareProbe(context.Background(), Options{Scopes: []netip.Prefix{netip.MustParsePrefix("192.0.2.11/32")}, ProbeAddress: "192.0.2.10:443"}, false); err == nil {
		t.Fatal("probe escaped scoped capture")
	}
}
