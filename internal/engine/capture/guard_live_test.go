package capture

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type liveDriver struct {
	fakeDriver
	indices map[string]int
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
