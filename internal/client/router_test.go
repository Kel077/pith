//go:build darwin

package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/router/routertest"
)

var (
	testEth     = api.Interface{Name: "en0", Label: "Ethernet", Kind: api.KindEthernet, HardwareAddr: "02:00:5e:10:00:01", Up: true, Router: "127.0.0.1", Primary: true}
	testHotspot = api.Interface{Name: "en9", Label: "Example Hotspot", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "192.0.2.1"}
)

// selfSigned is a certificate of its own key, as a router's web admin has.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "router.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveRouter serves f over TLS on address, "127.0.0.1:0" for any port.
func serveRouter(t *testing.T, f http.Handler, address string) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f)
	srv.Listener.Close()
	srv.Listener = ln
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // handshakes Pith refuses
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// routerClient is a client whose Ethernet leads to the fake router f and whose
// hotspot has internet; the public IPv4 tests find is the router's.
func routerClient(t *testing.T, c *Client, f *routertest.Router) (*Client, map[string]string) {
	t.Helper()
	srv := serveRouter(t, f, "127.0.0.1:0")
	_, c.routerPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	c.hotspotIndex = func(name string) (int, error) { return map[string]int{"en0": 7, "en9": 9}[name], nil }
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) { return []api.Interface{testEth, testHotspot}, nil }
	c.hotspotPace, c.routerSettle, c.routerPauses = 10*time.Millisecond, time.Millisecond, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	secrets := map[string]string{}
	c.secretGet = func(_ context.Context, service, account string) (string, error) {
		if s, ok := secrets[service+"/"+account]; ok {
			return s, nil
		}
		return "", keychain.ErrNotFound
	}
	c.secretSet = func(_ context.Context, service, account, secret string) error {
		secrets[service+"/"+account] = secret
		return nil
	}
	ipify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var address netip.Addr
		f.Do(func(f *routertest.Router) { address = f.Address })
		if !address.IsValid() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, address)
	}))
	t.Cleanup(ipify.Close)
	c.probeURLs = [2]string{ipify.URL, ipify.URL}
	return c, secrets
}

func TestRouter(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, secrets := routerClient(t, testClient(t), f)
	ctx := context.Background()
	if _, err := c.Router(ctx, "en9"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("a hotspot: %v", err)
	}
	r, err := c.Router(ctx, "en0")
	if err != nil || !r.Locked || r.Model != "CHR30A-R" || r.Interface != "en0" || r.PublicIPv4 != "" {
		t.Fatalf("without a password: %+v %v", r, err)
	}
	if err := c.SetRouterPassword(ctx, "en0", "wrong"); api.Code(err) != api.ErrWrongPassword || len(secrets) != 0 {
		t.Fatalf("wrong password: %v, kept %v", err, secrets)
	}
	if err := c.SetRouterPassword(ctx, "en0", routertest.Password); err != nil || len(secrets) != 1 {
		t.Fatalf("password: %v, kept %v", err, secrets)
	}
	for account := range secrets {
		if !strings.HasPrefix(account, routerKeychain+"/sha256:") {
			t.Fatalf("kept the password as %s", account)
		}
	}
	r, err = c.Router(ctx, "en0")
	if err != nil || r.Locked || !r.Connected || r.PublicIPv4 != "203.0.113.10" {
		t.Fatalf("with the password: %+v %v", r, err)
	}
	f.Do(func(f *routertest.Router) { f.Model = "CR1000A" })
	c.hotspotIndex = func(string) (int, error) { return 8, nil } // another session
	if _, err := c.Router(ctx, "en0"); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("an untested model: %v", err)
	}
}

// A router whose web admin has another key gets no password: it may be another
// device at the same address. It is locked until the password is given again.
func TestRouterPinsItsKey(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, secrets := routerClient(t, testClient(t), f)
	ctx := context.Background()
	if err := c.SetRouterPassword(ctx, "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	// Another web server, with another key, where the router was.
	other := routertest.New("203.0.113.10")
	srv := serveRouter(t, other, "127.0.0.1:0")
	c.mu.Lock()
	for _, s := range c.routers {
		s.transport.CloseIdleConnections()
		s.device.Base = srv.URL
	}
	c.mu.Unlock()
	_, c.routerPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	r, err := c.Router(ctx, "en0")
	if err != nil || !r.Locked {
		t.Fatalf("another key: %+v %v", r, err)
	}
	other.Do(func(o *routertest.Router) {
		if o.Logins != 0 {
			t.Fatal("sent the password to another key")
		}
	})
	if len(secrets) != 1 {
		t.Fatalf("kept %v", secrets)
	}
}

func TestRenewRouter(t *testing.T) {
	f := routertest.New("203.0.113.10")
	// The provider gives the address back once, then another.
	f.Do(func(f *routertest.Router) {
		f.Next = []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.20")}
		f.Reconnect = 2
	})
	engine := newFakeEngine()
	yields := make(chan string, 2)
	released := make(chan struct{}, 2)
	fake := func(ctx context.Context, iface string, ready func(string)) error {
		yields <- iface
		ready("en9")
		<-ctx.Done()
		released <- struct{}{}
		return nil
	}
	c, _, _ := startLocalHelper(t, "test", engine, func(o *helper.ServerOptions) { o.Yield = fake })
	c, _ = routerClient(t, c, f)
	ctx := context.Background()
	if err := c.SetRouterPassword(ctx, "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for _, s := range c.routers {
		s.device.Poll = time.Millisecond
	}
	c.mu.Unlock()
	ethRef := api.InterfaceRef{Name: "en0", Label: "Ethernet", HardwareAddr: testEth.HardwareAddr}
	hotspotRef := api.InterfaceRef{Name: "en9", Label: "Example Hotspot", HardwareAddr: testHotspot.HardwareAddr}
	cfg := api.DefaultConfig()
	cfg.Scenes[0].Rules = []api.Rule{
		{ID: "first", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef, hotspotRef}},
		{ID: "fallback", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef}, Fallback: true},
		{ID: "strict", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef}},
	}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	r, err := c.RenewRouter(ctx, "en0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Before.PublicIPv4 != "203.0.113.10" || r.After.PublicIPv4 != "203.0.113.20" || r.Tries != 2 || r.Standby != "en9" || r.Took <= 0 {
		t.Fatalf("renewal = %+v", r)
	}
	if <-yields != "en0" {
		t.Fatal("yielded another interface")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the yield outlived the renewal")
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew", "release", "renew"}) {
			t.Fatalf("calls = %v", f.Calls)
		}
	})
	engine.mu.Lock()
	scenes := slices.Clone(engine.scenes)
	engine.mu.Unlock()
	if len(scenes) != 3 {
		t.Fatalf("the helper ran %d scenes, want started, drained, restored", len(scenes))
	}
	drained := scenes[1].Rules
	if !slices.Equal(drained[0].Via, []api.InterfaceRef{hotspotRef}) || !slices.Equal(drained[1].Via, []api.InterfaceRef{hotspotRef}) || !slices.Equal(drained[2].Via, []api.InterfaceRef{ethRef}) {
		t.Fatalf("drained rules = %+v", drained)
	}
	if !reflect.DeepEqual(scenes[2], cfg.Scenes[0]) {
		t.Fatalf("restored %+v", scenes[2])
	}
	if _, err := os.Stat(filepath.Join(c.opts.ConfigDir, drainFile)); !os.IsNotExist(err) {
		t.Fatalf("the drain is still marked: %v", err)
	}
}

func TestRenewRouterNeedsAStandby(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, _ := routerClient(t, testClient(t), f)
	ctx := context.Background()
	if err := c.SetRouterPassword(ctx, "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) { return []api.Interface{testEth}, nil }
	if _, err := c.RenewRouter(ctx, "en0"); api.Code(err) != api.ErrNoStandby {
		t.Fatalf("RenewRouter = %v, want no standby", err)
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) { return []api.Interface{testEth, testHotspot}, nil }
	c.probeURLs = [2]string{"http://127.0.0.1:1", "http://127.0.0.1:1"}
	if _, err := c.RenewRouter(ctx, "en0"); api.Code(err) != api.ErrNoStandby {
		t.Fatalf("RenewRouter without internet = %v, want no standby", err)
	}
	f.Do(func(f *routertest.Router) {
		if len(f.Calls) != 0 {
			t.Fatalf("calls = %v", f.Calls)
		}
	})
}

// refusingReload is a helper's engine that can't take new rules.
type refusingReload struct{ *fakeEngine }

func (refusingReload) Reload(context.Context, api.Scene, api.Settings) error {
	return errors.New("reload refused")
}

// yieldingClient is a client of a helper that runs backend and yields en0 to
// en9; released tells when a yield ends.
func yieldingClient(t *testing.T, f *routertest.Router, backend helper.Backend) (c *Client, released <-chan struct{}) {
	t.Helper()
	ended := make(chan struct{}, 2)
	fake := func(ctx context.Context, iface string, ready func(string)) error {
		ready("en9")
		<-ctx.Done()
		ended <- struct{}{}
		return nil
	}
	c, _, _ = startLocalHelper(t, "test", backend, func(o *helper.ServerOptions) { o.Yield = fake })
	c, _ = routerClient(t, c, f)
	if err := c.SetRouterPassword(context.Background(), "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	return c, ended
}

// failedRenewal checks that a renewal that failed left the router, the yield
// and the drain as they were.
func failedRenewal(t *testing.T, c *Client, f *routertest.Router, released <-chan struct{}) {
	t.Helper()
	f.Do(func(f *routertest.Router) {
		if len(f.Calls) != 0 {
			t.Fatalf("calls = %v", f.Calls)
		}
	})
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the yield outlived the renewal")
	}
	c.mu.Lock()
	drained := c.drained
	c.mu.Unlock()
	if drained != nil {
		t.Fatalf("still drained: %+v", drained)
	}
}

// A router keeps its address when the helper can't have rules skip its
// interface: the connections they still sent there would fail meanwhile.
func TestRenewRouterNeedsRulesThatSkipIt(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, released := yieldingClient(t, f, refusingReload{newFakeEngine()})
	ctx := context.Background()
	if err := c.SaveConfig(ctx, api.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RenewRouter(ctx, "en0"); err == nil || !strings.Contains(err.Error(), "reload refused") {
		t.Fatalf("RenewRouter = %v, want the refused reload", err)
	}
	failedRenewal(t, c, f, released)
}

// A router keeps its address when Pith can't mark the renewal: should Pith
// quit meanwhile, nothing would ask the router for an address again.
func TestRenewRouterNeedsItsMark(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, released := yieldingClient(t, f, newFakeEngine())
	// A folder where the mark goes.
	if err := os.MkdirAll(filepath.Join(c.opts.ConfigDir, routerMark), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RenewRouter(context.Background(), "en0"); err == nil {
		t.Fatal("renewed without a mark")
	}
	failedRenewal(t, c, f, released)
	if _, err := os.Stat(filepath.Join(c.opts.ConfigDir, drainFile)); !os.IsNotExist(err) {
		t.Fatalf("the drain is still marked: %v", err)
	}
}

func TestRenewRouterLocked(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, _ := routerClient(t, testClient(t), f)
	if _, err := c.RenewRouter(context.Background(), "en0"); api.Code(err) != api.ErrWrongPassword {
		t.Fatalf("RenewRouter = %v, want a wrong password", err)
	}
}

func TestWithoutInterface(t *testing.T) {
	ethRef := api.InterfaceRef{Name: "en0", HardwareAddr: testEth.HardwareAddr}
	hotspotRef := api.InterfaceRef{Name: "en9", HardwareAddr: testHotspot.HardwareAddr}
	wifi := api.InterfaceRef{Name: "en1", HardwareAddr: "02:00:5e:10:00:02"}
	scene := api.Scene{ID: "s", Rules: []api.Rule{
		{ID: "a", Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef, wifi}},
		{ID: "b", Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{wifi, {Name: "en0"}}},
		{ID: "c", Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef}, Fallback: true},
		{ID: "d", Protocol: api.AnyProtocol, Action: api.Route, Via: []api.InterfaceRef{ethRef}},
		{ID: "e", Protocol: api.AnyProtocol, Action: api.Block},
	}}
	got := withoutInterface(scene, testEth, hotspotRef)
	want := [][]api.InterfaceRef{{wifi}, {wifi}, {hotspotRef}, {ethRef}, nil}
	for i, rule := range got.Rules {
		if !slices.Equal(rule.Via, want[i]) {
			t.Errorf("rule %s: via %v, want %v", rule.ID, rule.Via, want[i])
		}
	}
	if !slices.Equal(scene.Rules[0].Via, []api.InterfaceRef{ethRef, wifi}) {
		t.Fatal("changed the saved scene")
	}
	if err := (api.Config{Version: api.ConfigVersion, Scenes: []api.Scene{got}, Selected: "s"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// Quitting Pith while the router has no address asks it for one again first.
func TestCloseAsksARenewingRouterForAnAddress(t *testing.T) {
	f := routertest.New("203.0.113.10")
	fake := func(ctx context.Context, iface string, ready func(string)) error {
		ready("en9")
		<-ctx.Done()
		return nil
	}
	c, _, _ := startLocalHelper(t, "test", newFakeEngine(), func(o *helper.ServerOptions) { o.Yield = fake })
	c, _ = routerClient(t, c, f)
	if err := c.SetRouterPassword(context.Background(), "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	c.routerPauses = []time.Duration{time.Hour}
	done := make(chan error, 1)
	go func() {
		_, err := c.RenewRouter(context.Background(), "en0")
		done <- err
	}()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting: %s", what)
			}
		}
	}
	calls := func() (calls []string) {
		f.Do(func(f *routertest.Router) { calls = slices.Clone(f.Calls) })
		return calls
	}
	waitFor("the release", func() bool { return len(calls()) == 1 })
	if _, err := os.Stat(filepath.Join(c.opts.ConfigDir, routerMark)); err != nil {
		t.Fatalf("no mark of the released router: %v", err)
	}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls(), []string{"release", "renew"}) || time.Since(start) > 10*time.Second {
		t.Fatalf("closed after %v with calls %v", time.Since(start), calls())
	}
	if err := <-done; err == nil {
		t.Fatal("a renewal cut short succeeded")
	}
}

// A mark of a router a renewal left without an address has the next client
// ask it for one again, and goes once it has one.
func TestRouterLeftWithoutAnAddressIsRestored(t *testing.T) {
	f := routertest.New("203.0.113.10")
	c, _ := routerClient(t, testClient(t), f)
	ctx := context.Background()
	if err := c.SetRouterPassword(ctx, "en0", routertest.Password); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for _, s := range c.routers {
		s.device.Poll = time.Millisecond
	}
	c.mu.Unlock()
	f.Release()
	mark := filepath.Join(c.opts.ConfigDir, routerMark)
	if err := os.MkdirAll(c.opts.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mark, []byte(`{"interface":"en0","hardwareAddr":"`+testEth.HardwareAddr+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadConfig(ctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, err := os.Stat(mark)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mark stayed")
		}
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) || !f.Address.IsValid() {
			t.Fatalf("calls %v, address %v", f.Calls, f.Address)
		}
	})
}
