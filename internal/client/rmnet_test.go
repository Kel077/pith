package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

type testRadio struct{ cycle func() }

func (testRadio) Status(context.Context) (api.Hotspot, error) {
	return api.Hotspot{Model: api.HotspotRM520N, Connected: true}, nil
}

func (r testRadio) Renew(ctx context.Context, before func(context.Context) error) error {
	if err := before(ctx); err != nil {
		return err
	}
	r.cycle()
	return nil
}

func TestRMNetFollowsItsHardwareThroughRenewal(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	c.hotspotPace = 3 * time.Millisecond
	var mu sync.Mutex
	old := api.Interface{Name: "cell-old", Label: "Travel", HardwarePort: api.HotspotRM520N, Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:ab", Up: true}
	newLink := old
	newLink.Name = "cell-new"
	other := api.Interface{Name: old.Name, Label: api.HotspotRM520N, HardwarePort: "Ethernet", Kind: api.KindEthernet, HardwareAddr: "02:00:5e:10:00:cd", Up: true}
	changed := false
	reads := 0
	var dials []string
	c.rmnet = testRadio{cycle: func() { mu.Lock(); changed = true; mu.Unlock() }}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if !changed {
			return []api.Interface{old}, nil
		}
		reads++
		if reads <= 2 {
			return []api.Interface{other}, nil
		}
		return []api.Interface{other, newLink}, nil
	}
	c.hotspotIndex = func(name string) (int, error) {
		if name == newLink.Name {
			return 2, nil
		}
		return 1, nil
	}
	c.dialer = func(name string, timeout time.Duration, _ []string) (*net.Dialer, error) {
		mu.Lock()
		defer mu.Unlock()
		if changed && name != newLink.Name || !changed && name != old.Name {
			t.Errorf("probe used %q", name)
		}
		dials = append(dials, name)
		return &net.Dialer{Timeout: timeout}, nil
	}
	v4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if changed {
			fmt.Fprint(w, "192.0.2.2")
		} else {
			fmt.Fprint(w, "192.0.2.1")
		}
	}))
	defer v4.Close()
	v6 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "2001:db8::1") }))
	v6.Listener.Close()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	v6.Listener = l
	v6.Start()
	defer v6.Close()
	c.probeURLs = [2]string{v4.URL, v6.URL}
	h, err := c.Hotspot(ctx, old.Name)
	if err != nil || h.Model != api.HotspotRM520N || h.Locked {
		t.Fatal(h, err)
	}
	c.secretSet = func(context.Context, string, string, string) error {
		t.Fatal("RMNet accessed the keychain")
		return nil
	}
	if err := c.SetHotspotPassword(ctx, old.Name, "unused"); api.Code(err) != api.ErrUnsupported {
		t.Fatal(err)
	}
	// The local port uses automatic device detection and the same renewal as
	// the Network page. A newly attached Ethernet adapter reuses the old name.
	r, err := c.changeExitIP(ctx, api.InterfaceRef{Name: old.Name, HardwareAddr: old.HardwareAddr})
	if err != nil || r.BeforeIPv4 != "192.0.2.1" || r.PublicIPv4 != "192.0.2.2" || r.PublicIPv6 != "2001:db8::1" || r.PublicIPv6 != r.BeforeIPv6 || r.Seconds <= 0 {
		t.Fatal(r, err)
	}
	if !slices.Equal(dials, []string{old.Name, newLink.Name}) {
		t.Fatal(dials)
	}
	if _, err := c.Hotspot(ctx, old.Name); api.Code(err) != api.ErrUnsupported {
		t.Fatal("service label alone authorized RMNet", err)
	}
}
