package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/proxy"
)

var exampleExit = api.Interface{Name: "example0", Label: "Example Ethernet", Kind: api.KindThunderbolt,
	HardwareAddr: "02:00:5e:10:00:01", Up: true, IPv4: []string{"192.0.2.2"}, IPv6: []string{"2001:db8::2"}, Router: "192.0.2.1"}

// exitNetwork supplies both families over loopback, counting each HTTPS
// test as one request per family. gate can hold tests in flight.
type exitNetwork struct {
	mu      sync.Mutex
	links   []api.Interface
	public  [2]string
	calls   [2]int
	gate    <-chan struct{}
	started chan struct{}
}

func exitClient(t *testing.T) (*Client, *exitNetwork) {
	t.Helper()
	c := testClient(t)
	n := &exitNetwork{links: []api.Interface{exampleExit}, public: [2]string{"198.51.100.9", "2001:db8::9"}, started: make(chan struct{}, 64)}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return slices.Clone(n.links), nil
	}
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.hotspotIndex = func(string) (int, error) { return 7, nil }
	c.secretGet = func(context.Context, string, string) (string, error) { return "", keychain.ErrNotFound }
	c.secretSet = func(context.Context, string, string, string) error { return errors.New("unexpected password write") }
	for i := range 2 {
		ln, err := net.Listen([]string{"tcp4", "tcp6"}[i], []string{"127.0.0.1:0", "[::1]:0"}[i])
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.mu.Lock()
			n.calls[i]++
			gate, address := n.gate, n.public[i]
			n.mu.Unlock()
			select {
			case n.started <- struct{}{}:
			default:
			}
			if gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			if address == "" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			} else {
				fmt.Fprint(w, address)
			}
		}))
		srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		t.Cleanup(srv.Close)
		c.probeURLs[i] = srv.URL
	}
	return c, n
}

func (n *exitNetwork) tests() [2]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func awaitExit(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the operation")
	}
}

func TestExitInfoLabelsAndSavedAliases(t *testing.T) {
	c, n := exitClient(t)
	ctx := context.Background()
	cfg := api.DefaultConfig()
	cfg.Aliases = map[string]string{exampleExit.HardwareAddr: "Desk", "example0": "Cable", "missing0": "Unplugged"}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Aliases[exampleExit.HardwareAddr] = "Changed by caller"
	if err := os.Remove(c.configPath); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		via  api.InterfaceRef
		want string
	}{
		{api.InterfaceRef{Name: "example0"}, "Cable"},
		{api.InterfaceRef{Name: "old0", HardwareAddr: exampleExit.HardwareAddr}, "Desk"},
		{api.InterfaceRef{Name: "missing0", Label: "Old label"}, "Unplugged"},
		{api.InterfaceRef{Name: "missing1", Label: "Saved adapter"}, "Saved adapter"},
		{api.InterfaceRef{Name: "missing2"}, "missing2"},
	} {
		got, err := c.exitInfo(ctx, tt.via, -1)
		if err != nil || got.Interface != tt.want {
			t.Fatalf("%+v: %+v, %v; want %q", tt.via, got, err, tt.want)
		}
	}
	// Loading and saving both publish private alias copies.
	if _, err := c.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1)
	if err != nil || got.Interface != exampleExit.Label {
		t.Fatalf("label: %+v %v", got, err)
	}
	n.mu.Lock()
	n.links[0].Label = ""
	n.mu.Unlock()
	got, err = c.exitInfo(ctx, api.InterfaceRef{Name: "example0", Label: "Stale label"}, -1)
	if err != nil || got.Interface != "example0" {
		t.Fatalf("BSD name: %+v %v", got, err)
	}
}

func TestExitInfoOnlineAndSuspended(t *testing.T) {
	c, n := exitClient(t)
	manager, err := proxy.New(proxy.Options{Dial: func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
		return nil, errors.New("unexpected proxy connection")
	}})
	if err != nil {
		t.Fatal(err)
	}
	c.proxies = manager
	ref := api.InterfaceRef{Name: "example0"}
	for _, tt := range []struct {
		up, suspended, online bool
	}{{false, false, false}, {true, true, false}, {true, false, true}} {
		n.mu.Lock()
		n.links[0].Up = tt.up
		n.mu.Unlock()
		if tt.suspended {
			manager.Suspend(func(api.InterfaceRef) bool { return true })
		} else {
			manager.Resume()
		}
		before := n.tests()
		got, err := c.exitInfo(context.Background(), ref, -1)
		if err != nil || got.Online != tt.online {
			t.Fatalf("%+v: %+v %v", tt, got, err)
		}
		if !tt.online && before != n.tests() {
			t.Fatal("tested an offline interface")
		}
	}
	if got, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "missing0"}, 0); err != nil || got.Online {
		t.Fatalf("absent interface: %+v %v", got, err)
	}
	before := n.tests()
	manager.Suspend(func(api.InterfaceRef) bool { return true })
	got, err := c.exitInfo(context.Background(), ref, 0)
	if err != nil || got.Online || got.PublicIPv4 != "198.51.100.9" || n.tests() != before {
		t.Fatal("suspension lost known addresses or started a test", got, err, n.tests())
	}
}

func TestExitInfoKeepsPublicProbesAndHonorsMaxAge(t *testing.T) {
	c, n := exitClient(t)
	ctx := context.Background()
	ref := api.InterfaceRef{Name: "example0"}
	p, err := c.Probe(ctx, "example0", "")
	if err != nil || !p.OK {
		t.Fatal(p, err)
	}
	for _, age := range []time.Duration{-1, time.Hour, -time.Hour} {
		got, err := c.exitInfo(ctx, ref, age)
		if err != nil || got.PublicIPv4 != p.PublicIPv4 || got.PublicIPv6 != p.PublicIPv6 || got.At != p.At || got.CanChangeIP {
			t.Fatalf("age %v: %+v %v", age, got, err)
		}
	}
	if n.tests() != [2]int{1, 1} {
		t.Fatal("polls repeated the public-address test", n.tests())
	}
	if _, err := c.exitInfo(ctx, ref, 0); err != nil || n.tests() != [2]int{2, 2} {
		t.Fatal("zero maxAge didn't refresh", n.tests(), err)
	}
	c.exitMu.Lock()
	c.exits["example0"].public.At = time.Now().Add(-2 * time.Hour)
	c.exitMu.Unlock()
	if _, err := c.exitInfo(ctx, ref, time.Hour); err != nil || n.tests() != [2]int{3, 3} {
		t.Fatal("positive maxAge didn't refresh", n.tests(), err)
	}
	n.mu.Lock()
	n.public = [2]string{}
	n.mu.Unlock()
	for _, age := range []time.Duration{0, -1} {
		got, err := c.exitInfo(ctx, ref, age)
		if err != nil || got.PublicIPv4 != "" || got.PublicIPv6 != "" || !got.At.IsZero() || n.tests() != [2]int{4, 4} {
			t.Fatal("a failed refresh retained stale addresses or wasn't remembered", got, err, n.tests())
		}
	}
}

func TestExitInfoForgetsChangedInterfaces(t *testing.T) {
	for _, change := range []string{"hardware", "IPv4", "IPv6", "router", "router6", "down", "absent"} {
		t.Run(change, func(t *testing.T) {
			c, n := exitClient(t)
			ctx := context.Background()
			if p, err := c.Probe(ctx, "example0", ""); err != nil || !p.OK {
				t.Fatal(p, err)
			}
			n.mu.Lock()
			switch change {
			case "hardware":
				n.links[0].HardwareAddr = "02:00:5e:10:00:02"
			case "IPv4":
				n.links[0].IPv4 = []string{"192.0.2.3"}
			case "IPv6":
				n.links[0].IPv6 = []string{"2001:db8::3"}
			case "router":
				n.links[0].Router = "192.0.2.254"
			case "router6":
				n.links[0].Router6 = "2001:db8::1"
			case "down":
				n.links[0].Up = false
			case "absent":
				n.links = nil
			}
			n.public = [2]string{}
			n.mu.Unlock()
			got, err := c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1)
			if err != nil || got.PublicIPv4 != "" || got.PublicIPv6 != "" || !got.At.IsZero() {
				t.Fatalf("kept stale addresses: %+v %v", got, err)
			}
			if change == "down" || change == "absent" {
				if n.tests() != [2]int{1, 1} {
					t.Fatal("tested a missing or down interface", n.tests())
				}
				n.mu.Lock()
				n.links = []api.Interface{exampleExit}
				n.mu.Unlock()
				got, err = c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1)
				if err != nil || got.PublicIPv4 != "" || n.tests() != [2]int{2, 2} {
					t.Fatal("reused addresses after the interface returned", got, err, n.tests())
				}
			} else if n.tests() != [2]int{2, 2} {
				t.Fatal("didn't refresh the changed interface", n.tests())
			}
		})
	}
}

func TestExitInfoSharesLookupsAndRemembersFailures(t *testing.T) {
	c, n := exitClient(t)
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	n.gate = gate
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1)
			if err != nil || got.PublicIPv4 != "198.51.100.9" || got.PublicIPv6 != "2001:db8::9" {
				t.Errorf("shared lookup: %+v %v", got, err)
			}
		})
	}
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	open()
	wg.Wait()
	if n.tests() != [2]int{1, 1} {
		t.Fatal("concurrent polls tested more than once", n.tests())
	}
	c.forgetExitAddresses("example0")
	n.mu.Lock()
	n.gate, n.public = nil, [2]string{}
	n.mu.Unlock()
	for range 5 {
		got, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1)
		if err != nil || got.PublicIPv4 != "" || got.PublicIPv6 != "" {
			t.Fatal(got, err)
		}
	}
	if n.tests() != [2]int{2, 2} {
		t.Fatal("polls didn't remember a failed test", n.tests())
	}
	if _, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, time.Minute); err != nil || n.tests() != [2]int{3, 3} {
		t.Fatal("explicit maxAge didn't retry a failure", n.tests(), err)
	}
	c.exitMu.Lock()
	c.exits["example0"].failed = time.Now().Add(-c.exits["example0"].backoff - time.Second)
	c.exitMu.Unlock()
	if _, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1); err != nil || n.tests() != [2]int{4, 4} {
		t.Fatal("failure didn't expire", n.tests(), err)
	}
}

func TestExitInfoDiscardsLookupForAReplacedInterface(t *testing.T) {
	c, n := exitClient(t)
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	n.gate = gate
	done := make(chan struct{})
	var info api.ProxyInfo
	go func() {
		defer close(done)
		info, _ = c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1)
	}()
	awaitExit(t, n.started)
	awaitExit(t, n.started)
	n.mu.Lock()
	n.links[0].HardwareAddr = "02:00:5e:10:00:02"
	n.mu.Unlock()
	open()
	awaitExit(t, done)
	c.exitMu.Lock()
	public := c.exits["example0"].public
	c.exitMu.Unlock()
	if publicIPs(public) != 0 || !info.Online || info.PublicIPv4 != "" || info.PublicIPv6 != "" {
		t.Fatal("the previous adapter's test repopulated the cache or hid the new link", public, info)
	}
}

func TestExitInfoSharesExplicitRefreshes(t *testing.T) {
	for _, age := range []time.Duration{0, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			c, n := exitClient(t)
			ref := api.InterfaceRef{Name: "example0"}
			if _, err := c.exitInfo(context.Background(), ref, -1); err != nil {
				t.Fatal(err)
			}
			c.exitMu.Lock()
			c.exits["example0"].public.At = time.Now().Add(-2 * time.Hour)
			c.exitMu.Unlock()
			gate := make(chan struct{})
			open := sync.OnceFunc(func() { close(gate) })
			defer open()
			n.mu.Lock()
			n.gate = gate
			n.mu.Unlock()
			var wg sync.WaitGroup
			for range 20 {
				waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
				wg.Go(func() {
					got, err := c.exitInfo(waiting, ref, age)
					if err != nil || got.PublicIPv4 != "198.51.100.9" || got.PublicIPv6 != "2001:db8::9" {
						t.Errorf("explicit refresh: %+v %v", got, err)
					}
				})
				awaitExit(t, waiting.waiting)
			}
			open()
			wg.Wait()
			if n.tests() != [2]int{2, 2} {
				t.Fatal("concurrent explicit refreshes didn't share one test", n.tests())
			}
		})
	}
}

func TestExitChangingResolvesSharedHardwareAddresses(t *testing.T) {
	c := testClient(t)
	first, other := exampleExit, exampleExit
	other.Name = "example1"
	c.observeExits([]api.Interface{first, other})
	c.exitChanges = map[string]*exitChange{"example0": {link: first, done: make(chan struct{})}}
	if !c.exitChanging(api.InterfaceRef{Name: first.Name, HardwareAddr: first.HardwareAddr}) ||
		c.exitChanging(api.InterfaceRef{Name: other.Name, HardwareAddr: other.HardwareAddr}) ||
		c.exitChanging(api.InterfaceRef{Name: "missing0", HardwareAddr: first.HardwareAddr}) {
		t.Fatal("a shared hardware address matched the wrong interface")
	}
	c.observeExits([]api.Interface{other})
	if !c.exitChanging(api.InterfaceRef{Name: first.Name}) {
		t.Fatal("a disappeared interface lost its change")
	}
	delete(c.exitChanges, "example0")
}

// observedWait marks the point a joining caller waits for the operation,
// so tests can hold the device until the join has happened.
type observedWait struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *observedWait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func hotspotExitClient(t *testing.T, gate <-chan struct{}, testingAfter chan<- struct{}) (*Client, *hotspottest.MiFi) {
	t.Helper()
	f := hotspottest.New()
	address := func(family int) func(int) string {
		return func(after int) string {
			if after >= 0 && gate != nil {
				select {
				case testingAfter <- struct{}{}:
				default:
				}
				<-gate
			}
			if family == 4 {
				if after < 0 {
					return "198.51.100.10"
				}
				return "198.51.100.20"
			}
			if after < 0 {
				return "2001:db8::10"
			}
			return "2001:db8::20"
		}
	}
	c := renewalClient(t, f, address(4), address(6))
	old := c.interfaces
	c.interfaces = func(ctx context.Context) ([]api.Interface, error) {
		links, err := old(ctx)
		links[0].Name = "example0"
		return links, err
	}
	s, err := c.hotspot(context.Background(), "example0")
	if err != nil {
		t.Fatal(err)
	}
	s.device.Poll = time.Millisecond
	return c, f
}

func TestExitInfoHotspotAndLockedHotspot(t *testing.T) {
	c, f := hotspotExitClient(t, nil, nil)
	c.secretGet = func(context.Context, string, string) (string, error) { return "", keychain.ErrNotFound }
	ref := api.InterfaceRef{Name: "example0"}
	for range 3 {
		info, err := c.exitInfo(context.Background(), ref, -1)
		if err != nil || !info.CanChangeIP {
			t.Fatal(info, err)
		}
	}
	f.Do(func(f *hotspottest.MiFi) {
		if f.Logins != 0 || len(f.Calls) != 0 {
			t.Fatal("status tried a password or a renewal", f.Logins, f.Calls)
		}
	})
	if _, err := c.changeExitIP(context.Background(), ref); api.Code(err) != api.ErrWrongPassword {
		t.Fatal("a locked hotspot didn't return its password error", err)
	}
}

func TestExitChangesShareHotspotRenewals(t *testing.T) {
	for _, tt := range []struct{ first, second string }{{"port", "port"}, {"hotspot", "port"}, {"port", "hotspot"}, {"hotspot", "hotspot"}} {
		t.Run(tt.first+"-"+tt.second, func(t *testing.T) {
			gate := make(chan struct{})
			open := sync.OnceFunc(func() { close(gate) })
			defer open()
			after := make(chan struct{}, 2)
			c, f := hotspotExitClient(t, gate, after)
			ref := api.InterfaceRef{Name: "example0"}
			call := func(ctx context.Context, kind string) (api.ProxyIPChange, error) {
				if kind == "port" {
					return c.changeExitIP(ctx, ref)
				}
				r, err := c.RenewHotspot(ctx, "example0")
				return api.ProxyIPChange{BeforeIPv4: r.Before.PublicIPv4, BeforeIPv6: r.Before.PublicIPv6,
					PublicIPv4: r.After.PublicIPv4, PublicIPv6: r.After.PublicIPv6, At: r.After.At, Seconds: r.Took.Seconds()}, err
			}
			done := make(chan struct{})
			var first, second api.ProxyIPChange
			var firstErr, secondErr error
			go func() { defer close(done); first, firstErr = call(context.Background(), tt.first) }()
			awaitExit(t, after)
			if !c.exitChanging(ref) || c.exitChanging(api.InterfaceRef{Name: "other0"}) {
				t.Fatal("changing didn't describe the active interface")
			}
			waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
			joined := make(chan struct{})
			go func() { defer close(joined); second, secondErr = call(waiting, tt.second) }()
			awaitExit(t, waiting.waiting)
			open()
			awaitExit(t, done)
			awaitExit(t, joined)
			for _, r := range []api.ProxyIPChange{first, second} {
				if r.BeforeIPv4 != "198.51.100.10" || r.BeforeIPv6 != "2001:db8::10" || r.PublicIPv4 != "198.51.100.20" ||
					r.PublicIPv6 != "2001:db8::20" || r.At.IsZero() || r.Seconds <= 0 {
					t.Fatalf("renewal: %+v; %v %v", r, firstErr, secondErr)
				}
			}
			if firstErr != nil || secondErr != nil || c.exitChanging(ref) || first.At != second.At {
				t.Fatal("joined outcomes differ", first, second, firstErr, secondErr)
			}
			f.Do(func(f *hotspottest.MiFi) {
				if !slices.Equal(f.Calls, []string{"off", "on"}) {
					t.Fatal("more than one renewal ran", f.Calls)
				}
			})
			info, err := c.exitInfo(context.Background(), ref, -1)
			if err != nil || info.PublicIPv4 != first.PublicIPv4 || info.PublicIPv6 != first.PublicIPv6 || info.At != first.At {
				t.Fatal("after addresses weren't kept", info, err)
			}
		})
	}
}

func TestExitChangeOutlivesItsCallerAndJoinCancellation(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	after := make(chan struct{}, 2)
	c, f := hotspotExitClient(t, gate, after)
	ref := api.InterfaceRef{Name: "example0"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.changeExitIP(ctx, ref); done <- err }()
	awaitExit(t, after)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !c.exitChanging(ref) {
		t.Fatal("the caller ended the renewal", err)
	}
	if _, err := c.RenewHotspot(ctx, "example0"); !errors.Is(err, context.Canceled) || !c.exitChanging(ref) {
		t.Fatal("a canceled join ended the renewal", err)
	}
	joined := make(chan struct{})
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	var r api.HotspotRenewal
	var err error
	go func() { defer close(joined); r, err = c.RenewHotspot(waiting, "example0") }()
	awaitExit(t, waiting.waiting)
	open()
	awaitExit(t, joined)
	if err != nil || r.After.PublicIPv4 != "198.51.100.20" || c.exitChanging(ref) {
		t.Fatal(r, err)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !slices.Equal(f.Calls, []string{"off", "on"}) {
			t.Fatal(f.Calls)
		}
	})
}

func TestExitChangeUnsupportedAndClosed(t *testing.T) {
	c, _ := exitClient(t)
	ref := api.InterfaceRef{Name: "example0"}
	if _, err := c.changeExitIP(context.Background(), ref); api.Code(err) != api.ErrUnsupported {
		t.Fatal("changed a plain interface", err)
	}
	if _, err := c.changeExitIP(context.Background(), api.InterfaceRef{Name: "missing0"}); api.Code(err) != api.ErrUnsupported {
		t.Fatal("changed an absent interface", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.changeExitIP(context.Background(), ref); !errors.Is(err, errClientClosed) {
		t.Fatal("changed an interface after Close", err)
	}
}

type exitRoundTrip func(*http.Request) (*http.Response, error)

func (f exitRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExitHotspotRenewalKeepsItsCallersCancellation(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	after := make(chan struct{}, 2)
	c, f := hotspotExitClient(t, gate, after)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.RenewHotspot(ctx, "example0"); done <- err }()
	awaitExit(t, after)
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	joined := make(chan error, 1)
	go func() { _, err := c.changeExitIP(waiting, api.InterfaceRef{Name: "example0"}); joined <- err }()
	awaitExit(t, waiting.waiting)
	cancel()
	for _, err := range []error{<-done, <-joined} {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("an interface renewal lost its caller's cancellation", err)
		}
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !slices.Equal(f.Calls, []string{"off", "on"}) {
			t.Fatal(f.Calls)
		}
	})
	if c.exitChanging(api.InterfaceRef{Name: "example0"}) {
		t.Fatal("the canceled renewal stayed changing")
	}
}

func TestExitCloseWaitsForHotspotRenewal(t *testing.T) {
	c, f := hotspotExitClient(t, nil, nil)
	s, err := c.hotspot(context.Background(), "example0")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	enabling := make(chan struct{})
	mark := sync.OnceFunc(func() { close(enabling) })
	base := s.device.HTTP.Transport
	s.device.HTTP.Transport = exitRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/wwan/enablecellulardata/" {
			mark()
			<-gate
		}
		return base.RoundTrip(r)
	})
	changed := make(chan error, 1)
	go func() {
		_, err := c.changeExitIP(context.Background(), api.InterfaceRef{Name: "example0"})
		changed <- err
	}()
	awaitExit(t, enabling)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close returned before the hotspot restored cellular data", err)
	case <-time.After(20 * time.Millisecond):
	}
	open()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-changed; err == nil {
		t.Fatal("a renewal cut short succeeded")
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !f.DataOn || !slices.Equal(f.Calls, []string{"off", "on"}) {
			t.Fatal("cellular data wasn't restored", f.Calls, f.DataOn)
		}
	})
	if c.exitChanging(api.InterfaceRef{Name: "example0"}) {
		t.Fatal("Close left the hotspot changing")
	}
}

// Addresses of one family where the interface has both, as a renewal can
// end with while the other family is a moment away, are looked up again
// in turn rather than kept until the interface changes.
func TestExitInfoLooksUpAMissingFamilyAgain(t *testing.T) {
	c, n := exitClient(t)
	ctx := context.Background()
	ref := api.InterfaceRef{Name: "example0"}
	n.mu.Lock()
	n.public[0] = ""
	n.mu.Unlock()
	if p, err := c.Probe(ctx, "example0", ""); err != nil || p.PublicIPv4 != "" || p.PublicIPv6 != "2001:db8::9" {
		t.Fatal(p, err)
	}
	n.mu.Lock()
	n.public[0] = "198.51.100.9"
	n.mu.Unlock()
	got, err := c.exitInfo(ctx, ref, -1)
	if err != nil || got.PublicIPv4 != "" || got.PublicIPv6 != "2001:db8::9" || n.tests() != [2]int{1, 1} {
		t.Fatal("didn't keep the addresses found for a while", got, err, n.tests())
	}
	c.exitMu.Lock()
	c.exits["example0"].failed = time.Now().Add(-exitFailureAge - time.Second)
	c.exitMu.Unlock()
	for range 2 {
		got, err = c.exitInfo(ctx, ref, -1)
		if err != nil || got.PublicIPv4 != "198.51.100.9" || got.PublicIPv6 != "2001:db8::9" || n.tests() != [2]int{2, 2} {
			t.Fatal("didn't look the missing family up again once", got, err, n.tests())
		}
	}
}

// An interface without IPv4 addresses misses no family without a public one.
func TestExitInfoKeepsTheOnlyFamily(t *testing.T) {
	c, n := exitClient(t)
	ctx := context.Background()
	n.mu.Lock()
	n.links[0].IPv4, n.links[0].Router = nil, ""
	n.public[0] = ""
	n.mu.Unlock()
	for range 3 {
		got, err := c.exitInfo(ctx, api.InterfaceRef{Name: "example0"}, -1)
		if err != nil || got.PublicIPv6 != "2001:db8::9" || n.tests() != [2]int{1, 1} {
			t.Fatal(got, err, n.tests())
		}
	}
}
