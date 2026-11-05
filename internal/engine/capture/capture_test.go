package capture

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Kel077/pith/internal/api"
	"golang.zx2c4.com/wireguard/tun"
)

func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr     { return netip.MustParseAddr(s) }
func baseRoutes() []Route {
	return []Route{
		{Prefix: prefix("0.0.0.0/0"), Interface: "en0", Index: 1, Gateway: addr("192.0.2.1")},
		{Prefix: prefix("0.0.0.0/0"), Interface: "en5", Index: 2, Gateway: addr("192.0.2.1"), Scoped: true},
		{Prefix: prefix("192.0.2.0/24"), Interface: "en0", Index: 1},
		{Prefix: prefix("::/0"), Interface: "en0", Index: 1, Gateway: addr("2001:db8::1")},
	}
}

func TestPlansPreserveSpecificRoutesAndSystemEgress(t *testing.T) {
	routes := baseRoutes()
	// This is the well-known Tailscale range, not a captured user address.
	routes = append(routes, Route{Prefix: prefix("100.64.0.0/10"), Interface: "utun2", Index: 8})
	p, err := Build(routes, []netip.Addr{addr("192.0.2.53"), addr("100.100.100.100"), addr("198.51.100.53")}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{prefix("0.0.0.0/1"), prefix("128.0.0.0/1"), prefix("::/1"), prefix("8000::/1"), prefix("192.0.2.53/32")}
	if !reflect.DeepEqual(p.Prefixes, want) {
		t.Fatal(p.Prefixes)
	}
	if !reflect.DeepEqual(p.Before, routes) {
		t.Fatal("changed pre-capture routes")
	}
	for _, tc := range []struct{ ip, iface string }{{"198.51.100.1", "en0"}, {"192.0.2.53", "en0"}, {"100.100.100.100", "utun2"}} {
		if got := SystemInterface(p.Before, addr(tc.ip)); got != tc.iface {
			t.Fatalf("system egress = %q, want %q", got, tc.iface)
		}
	}
	p, err = Build(routes, []netip.Addr{addr("192.0.2.53")}, false, []netip.Prefix{prefix("198.51.100.10/32")})
	if err != nil || len(p.Prefixes) != 1 || !p.Scoped {
		t.Fatal("scoped plan expanded capture", p, err)
	}
}

func TestRouteConflictsAndVPNDefault(t *testing.T) {
	routes := baseRoutes()
	routes[0].Interface = "utun5"
	p, err := Build(routes, nil, false, nil)
	if err != nil || len(p.Warnings) != 1 || p.Warnings[0].Code != api.WarnVPN {
		t.Fatal(p, err)
	}
	if got := SystemInterface(p.Before, addr("198.51.100.10")); got != "utun5" {
		t.Fatal("lost VPN system route", got)
	}
	routes = append(routes, Route{Prefix: prefix("0.0.0.0/1"), Interface: "utun5", Index: 5})
	_, err = Build(routes, nil, false, nil)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrRouteConflict {
		t.Fatal("route conflict not detected", err)
	}
	for _, scope := range []netip.Prefix{{}, prefix("0.0.0.0/0"), prefix("2001:db8::/32")} {
		if _, err := Build(baseRoutes(), nil, false, []netip.Prefix{scope}); err == nil {
			t.Fatal("accepted invalid scope", scope)
		}
	}
}

func TestSystemRoutePreservesRejectAndLocalDestinations(t *testing.T) {
	for _, terminal := range []Route{
		{Prefix: prefix("198.51.100.0/24"), Interface: "lo0", Reject: true},
		{Prefix: prefix("198.51.100.10/32"), Interface: "lo0", Local: true},
		{Prefix: prefix("198.51.100.0/24")},
	} {
		for _, routes := range [][]Route{append(baseRoutes(), terminal), append([]Route{terminal}, baseRoutes()...)} {
			if got := SystemInterface(routes, addr("198.51.100.10")); got != "" {
				t.Fatalf("terminal route escaped through %s", got)
			}
			if got := SystemInterface(routes, addr("203.0.113.10")); got != "en0" {
				t.Fatalf("unrelated route changed to %s", got)
			}
		}
	}
}

func TestEmergencyRecoveryRefusesLiveOwnerAndPreservesReusedName(t *testing.T) {
	driver := &fakeDriver{routes: baseRoutes()}
	path := filepath.Join(t.TempDir(), "capture.json")
	c, err := Prepare(context.Background(), Options{StatePath: path, Driver: driver, Scopes: []netip.Prefix{prefix("198.51.100.10/32")}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStale(context.Background(), path, driver, func(int) bool { return true }); err == nil {
		t.Fatal("removed a live owner's routes")
	}
	// Simulate a route still referring to the dead owner's index, plus a route
	// using the same BSD name with a new index that must remain untouched.
	driver.routes = append(driver.routes, Route{Prefix: prefix("203.0.113.1/32"), Interface: "utun42", Index: 99})
	if err := cleanupStale(context.Background(), path, driver, func(int) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if ownsRoutes(c.state, driver.routes) {
		t.Fatal("left journaled route")
	}
	if !slices.ContainsFunc(driver.routes, func(r Route) bool { return r.Index == 99 }) {
		t.Fatal("changed reused interface")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

type fakeDriver struct {
	routes                    []Route
	device                    *fakeDevice
	operations                []string
	failConfigure, failDelete bool
	failAdd, adds             int
}
type fakeDevice struct {
	owner  *fakeDriver
	events chan tun.Event
	closed bool
}

func (d *fakeDevice) File() *os.File                         { return nil }
func (d *fakeDevice) Name() (string, error)                  { return "utun42", nil }
func (d *fakeDevice) MTU() (int, error)                      { return MTU, nil }
func (d *fakeDevice) Events() <-chan tun.Event               { return d.events }
func (d *fakeDevice) BatchSize() int                         { return 1 }
func (d *fakeDevice) Read([][]byte, []int, int) (int, error) { return 0, fmt.Errorf("not used") }
func (d *fakeDevice) Write([][]byte, int) (int, error)       { return 0, fmt.Errorf("not used") }
func (d *fakeDevice) Close() error {
	if !d.closed {
		d.closed = true
		d.owner.operations = append(d.owner.operations, "close")
		d.owner.routes = slices.DeleteFunc(d.owner.routes, func(r Route) bool { return r.Index == 42 })
		close(d.events)
	}
	return nil
}
func (d *fakeDriver) Snapshot() ([]Route, error) { return slices.Clone(d.routes), nil }
func (d *fakeDriver) Index(string) (int, error)  { return 42, nil }
func (d *fakeDriver) Create(int) (tun.Device, error) {
	d.operations = append(d.operations, "create")
	d.device = &fakeDevice{owner: d, events: make(chan tun.Event, 8)}
	return d.device, nil
}
func (d *fakeDriver) Configure(context.Context, string, bool) error {
	d.operations = append(d.operations, "configure")
	if d.failConfigure {
		return fmt.Errorf("configure failed")
	}
	return nil
}
func (d *fakeDriver) Add(ctx context.Context, p netip.Prefix, name string) error {
	d.adds++
	d.operations = append(d.operations, "add "+p.String())
	if d.adds == d.failAdd {
		return fmt.Errorf("add failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.routes = append(d.routes, Route{Prefix: p, Interface: name, Index: 42})
	return nil
}
func (d *fakeDriver) Delete(_ context.Context, p netip.Prefix, name string) error {
	d.operations = append(d.operations, "delete "+p.String())
	if d.failDelete {
		return fmt.Errorf("delete failed")
	}
	d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Interface == name && r.Prefix == p && r.Index == 42 })
	return nil
}
func fakeOptions(t *testing.T, d *fakeDriver) Options {
	return Options{StatePath: filepath.Join(t.TempDir(), "capture.json"), Driver: d}
}

func TestPrepareActivateAndStopAreReversible(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	before := slices.Clone(d.routes)
	opts := fakeOptions(t, d)
	c, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if d.adds != 0 {
		t.Fatal("Prepare took over routing")
	}
	state, err := readState(opts.StatePath)
	if err != nil || state.Index != 42 || state.Interface != "utun42" {
		t.Fatal(state, err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.adds != 2 {
		t.Fatal("incorrect capture routes", d.adds)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal("non-idempotent close", err)
	}
	if !d.device.closed || !reflect.DeepEqual(d.routes, before) {
		t.Fatal("Stop failed to restore original routes", d.routes)
	}
	if _, err := os.Stat(opts.StatePath); !os.IsNotExist(err) {
		t.Fatal("journal remained", err)
	}
	want := []string{"create", "configure", "add 0.0.0.0/1", "add 128.0.0.0/1", "delete 128.0.0.0/1", "delete 0.0.0.0/1", "close"}
	if !reflect.DeepEqual(d.operations, want) {
		t.Fatal(d.operations)
	}
}

func TestEveryPartialActivationFailureClosesTunnel(t *testing.T) {
	for _, fail := range []int{1, 2} {
		d := &fakeDriver{routes: baseRoutes(), failAdd: fail}
		before := slices.Clone(d.routes)
		opts := fakeOptions(t, d)
		c, err := Prepare(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Activate(context.Background()); err == nil {
			t.Fatal("missing failure")
		}
		if !d.device.closed || !reflect.DeepEqual(d.routes, before) {
			t.Fatal("partial activation leaked routes")
		}
		if _, err := os.Stat(opts.StatePath); !os.IsNotExist(err) {
			t.Fatal("failed activation journal remained", err)
		}
	}
	d := &fakeDriver{routes: baseRoutes(), failConfigure: true}
	if _, err := Prepare(context.Background(), fakeOptions(t, d)); err == nil || !d.device.closed {
		t.Fatal("configure failure leaked tunnel", err)
	}
}

func TestConflictRaceNeverDeletesForeignRoutes(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	c, err := Prepare(context.Background(), fakeOptions(t, d))
	if err != nil {
		t.Fatal(err)
	}
	foreign := Route{Prefix: prefix("0.0.0.0/1"), Interface: "utun7", Index: 7}
	d.routes = append(d.routes, foreign)
	if err := c.Activate(context.Background()); err == nil {
		t.Fatal("concurrent VPN route ignored")
	}
	if !slices.Contains(d.routes, foreign) || d.adds != 0 {
		t.Fatal("foreign route changed")
	}
}

func TestDeviceCloseRestoresRoutesAfterDeleteFailure(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes(), failDelete: true}
	before := slices.Clone(d.routes)
	c, err := Prepare(context.Background(), fakeOptions(t, d))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	if !d.device.closed || !reflect.DeepEqual(d.routes, before) {
		t.Fatal("device not closed on delete failure")
	}
}

func TestRecoveryDoesNotGuessOwnershipOfRoutes(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	opts := fakeOptions(t, d)
	c, err := Prepare(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := Recover(opts.StatePath, d); err == nil {
		t.Fatal("live-looking state silently discarded")
	}
	// Simulate process death: closing utun removes its routes, leaving metadata.
	if err := d.device.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Recover(opts.StatePath, d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opts.StatePath); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledActivationNeverLeavesCapture(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	c, err := Prepare(context.Background(), fakeOptions(t, d))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Activate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if d.adds != 0 || !d.device.closed {
		t.Fatal("canceled capture left resources")
	}
}

func TestReusedInterfaceNameIsNotDeleted(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	c, err := Prepare(context.Background(), fakeOptions(t, d))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range d.routes {
		if d.routes[i].Index == 42 {
			d.routes[i].Index = 99
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range d.operations {
		if len(operation) > 7 && operation[:7] == "delete " {
			t.Fatal("deleted route on reused utun name")
		}
	}
	if !slices.ContainsFunc(d.routes, func(r Route) bool { return r.Index == 99 && r.Prefix == prefix("0.0.0.0/1") }) {
		t.Fatal("foreign interface route removed")
	}
}

func TestJournalFailureClosesBeforeConfiguration(t *testing.T) {
	d := &fakeDriver{routes: baseRoutes()}
	opts := fakeOptions(t, d)
	opts.StatePath = filepath.Join(t.TempDir(), "missing", "capture.json")
	if _, err := Prepare(context.Background(), opts); err == nil {
		t.Fatal("missing journal failure")
	}
	if !d.device.closed || slices.Contains(d.operations, "configure") || d.adds != 0 {
		t.Fatal("journal failure leaked configuration")
	}
}
