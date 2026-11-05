package capture

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type liveDriver struct {
	fakeDriver
	indices map[string]int
}

func TestUnreadableEventsRequireProofOfIrrelevance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   Change
		safe    bool
		missing bool
		reason  string
	}{
		{"external scoped link-local", Change{Index: 50, Unparsed: true, Scoped: true}, true, false, "external scoped route"},
		{"external scoped multicast", Change{Index: 50, Unparsed: true, Scoped: true, Added: true}, true, false, "external scoped route"},
		{"external address", Change{Index: 50, Address: true, Unparsed: true}, true, false, "interface notification"},
		{"external interface", Change{Index: 50, Interface: true, Unparsed: true}, true, false, "interface notification"},
		{"unscoped link-local with known key", Change{Index: 50, Unparsed: true, Prefix: prefix("fe80::/64")}, true, false, "external link-local or multicast key"},
		{"unscoped multicast with known key", Change{Index: 50, Unparsed: true, Prefix: prefix("ff00::/8")}, true, false, "external link-local or multicast key"},
		{"non-IP route", Change{Index: 50, Unparsed: true, NonIP: true}, true, false, "non-IP route"},
		{"owned route", Change{Index: 42, Unparsed: true}, false, false, "owned interface"},
		{"owned scoped route", Change{Index: 42, Unparsed: true, Scoped: true}, false, false, "owned interface"},
		{"owned address", Change{Index: 42, Address: true, Unparsed: true}, true, false, "interface notification"},
		{"owned interface", Change{Index: 42, Interface: true, Up: true, Unparsed: true}, true, false, "interface notification"},
		{"unknown notification index", Change{Address: true, Unparsed: true}, true, false, "interface notification"},
		{"owned address with missing capture", Change{Index: 42, Address: true, Unparsed: true}, false, true, "interface notification"},
		{"unknown identity", Change{Unparsed: true, Scoped: true}, false, false, "unknown interface"},
		{"unknown unscoped overlap", Change{Index: 50, Unparsed: true}, false, false, "overlap cannot be excluded"},
		{"external event cannot mask missing capture", Change{Index: 50, Unparsed: true, Scoped: true}, false, true, "external scoped route"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &liveDriver{fakeDriver: fakeDriver{routes: []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1}, {Prefix: prefix("::/0"), Interface: "test0", Index: 1}}}, indices: map[string]int{"test0": 1, "utun50": 50}}
			c, err := Prepare(context.Background(), Options{Driver: d, IPv6: true, Scopes: []netip.Prefix{prefix("2001:db8::9/128")}, StatePath: filepath.Join(t.TempDir(), "capture.json")})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			g, err := NewGuard(c, []api.Interface{{Name: "test0", Up: true}, {Name: "utun50", Up: true}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			old, err := g.Binding("utun50")
			if err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Index == 42 })
			}
			tc.event.Diagnostic = "RTM type=1 flags=0x0 index=50 addrs=[]: synthetic format"
			if diagnostic := g.RouteDiagnostic(tc.event); !strings.Contains(diagnostic, tc.reason) {
				t.Fatal("missing decision reason", diagnostic)
			}
			err = g.Observe(tc.event)
			if tc.safe {
				if err != nil || g.Err() != nil {
					t.Fatal("unrelated event stopped capture", err)
				}
				invalidate := tc.event.Index == 50 && !tc.event.Address
				if (old.Check() != nil) != invalidate {
					t.Fatal("incorrect external binding invalidation")
				}
				if _, err := g.Binding("utun50"); err != nil {
					t.Fatal("fresh external binding did not recover", err)
				}
				if binding, err := g.System(addr("2001:db8::9")); err != nil || binding.Name != "test0" {
					t.Fatal("unrelated event stalled immediate system egress", binding, err)
				}
			} else if api.Code(err) != api.ErrNetworkLost || g.Err() == nil {
				t.Fatal("uncertain capture ownership was accepted", err)
			}
		})
	}
}

type notificationDriver struct {
	liveDriver
	up          bool
	reads       int
	snapshotErr error
}

func (d *notificationDriver) Snapshot() ([]Route, error) {
	d.reads++
	routes, _ := d.fakeDriver.Snapshot()
	return routes, d.snapshotErr
}
func (d *notificationDriver) InterfaceState(name string) (int, bool, error) {
	index, err := d.Index(name)
	return index, d.up, err
}

func TestNotificationsVerifyCurrentOwnedInterface(t *testing.T) {
	for _, tc := range []struct {
		name     string
		event    Change
		down     bool
		readFail bool
	}{
		{"up", Change{Index: 42, Interface: true, Up: true}, false, false},
		{"delayed down while currently up", Change{Index: 42, Interface: true}, false, false},
		{"unreadable owned address", Change{Index: 42, Address: true, Unparsed: true}, false, false},
		{"unreadable owned interface", Change{Index: 42, Interface: true, Unparsed: true}, false, false},
		{"confirmed down", Change{Index: 42, Interface: true}, true, false},
		{"stale up while currently down", Change{Index: 42, Interface: true, Up: true}, true, false},
		{"failed ownership snapshot", Change{Index: 42, Address: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &notificationDriver{liveDriver: liveDriver{fakeDriver: fakeDriver{routes: []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1}}}, indices: map[string]int{"test0": 1}}, up: true}
			c, err := Prepare(context.Background(), Options{Driver: d, Scopes: []netip.Prefix{prefix("192.0.2.9/32")}, StatePath: filepath.Join(t.TempDir(), "capture.json")})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			g, err := NewGuard(c, []api.Interface{{Name: "test0", Up: true}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Activate(context.Background()); err != nil {
				t.Fatal(err)
			}
			d.up = !tc.down
			if tc.readFail {
				d.snapshotErr = errors.New("synthetic observation failure")
			}
			before := d.reads
			err = g.Observe(tc.event)
			if !tc.readFail && d.reads != before+1 || tc.readFail && d.reads <= before+1 {
				t.Fatal("notification did not recheck the complete route table")
			}
			if tc.down || tc.readFail {
				if api.Code(err) != api.ErrNetworkLost || g.Err() == nil {
					t.Fatal("invalid current state was accepted", err)
				}
			} else if err != nil || g.Err() != nil {
				t.Fatal("healthy capture stopped on a notification", err)
			}
			d.snapshotErr = nil
		})
	}
}

func (d *liveDriver) Index(name string) (int, error) {
	if name == "utun42" {
		return 42, nil
	}
	index := d.indices[name]
	if index == 0 {
		return 0, errors.New("absent interface")
	}
	return index, nil
}

func TestOwnershipRetryStopsOnCaptureCancellation(t *testing.T) {
	d := &notificationDriver{liveDriver: liveDriver{fakeDriver: fakeDriver{routes: []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "test0", Index: 1}}}, indices: map[string]int{"test0": 1}}, up: true}
	c, err := Prepare(context.Background(), Options{Driver: d, Scopes: []netip.Prefix{prefix("192.0.2.9/32")}, StatePath: filepath.Join(t.TempDir(), "capture.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, err := NewGuard(c, []api.Interface{{Name: "test0", Up: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.snapshotErr = syscall.ENOMEM
	done := make(chan struct{})
	start := time.Now()
	timer := time.AfterFunc(25*time.Millisecond, func() {
		defer close(done)
		g.Revoke(errors.New("test stop"))
	})
	err = g.Validate(true)
	if !timer.Stop() {
		<-done
	}
	d.snapshotErr = nil
	if err == nil || !strings.Contains(err.Error(), "test stop") || time.Since(start) >= time.Second {
		t.Fatal("capture stop waited for the observation deadline", err)
	}
}

func TestSystemSettlingExpiresWithoutRestartAndIsFamilySpecific(t *testing.T) {
	d := &liveDriver{fakeDriver: fakeDriver{routes: []Route{{Prefix: prefix("0.0.0.0/0"), Interface: "utun50", Index: 50}, {Prefix: prefix("::/0"), Interface: "en0", Index: 1}}}, indices: map[string]int{"en0": 1, "utun50": 50}}
	c, err := Prepare(context.Background(), Options{Driver: d, IPv6: true, StatePath: filepath.Join(t.TempDir(), "capture.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, err := NewGuard(c, []api.Interface{{Name: "en0", Up: true}, {Name: "utun50", Up: true}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	g.now = func() time.Time { return now }
	if _, err := g.System(addr("203.0.113.9")); err == nil || g.Err() != nil {
		t.Fatal("partial activation must fail locally", err)
	}
	if err := c.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.routes = slices.DeleteFunc(d.routes, func(r Route) bool { return r.Interface == "utun50" })
	d.routes = append(d.routes, Route{Prefix: prefix("0.0.0.0/0"), Interface: "en0", Index: 1})
	if _, err := g.System(addr("203.0.113.9")); err == nil {
		t.Fatal("physical fallback during VPN gap")
	}
	if b, err := g.System(addr("2001:db8::9")); err != nil || b.Name != "en0" {
		t.Fatal("IPv4 withdrawal disrupted IPv6", b, err)
	}
	now = now.Add(3 * time.Second)
	if b, err := g.System(addr("203.0.113.9")); err != nil || b.Name != "en0" {
		t.Fatal("stable physical default did not recover", b, err)
	}
	if g.Err() != nil {
		t.Fatal(g.Err())
	}
}
