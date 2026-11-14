//go:build darwin

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/helper"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/router/routertest"
)

func routerExitClient(t *testing.T, gate <-chan struct{}, testingAfter chan<- struct{}) (*Client, *routertest.Router) {
	t.Helper()
	yield := func(ctx context.Context, iface string, ready func(string)) error {
		if iface != "example0" {
			return fmt.Errorf("unexpected yield of %s", iface)
		}
		ready("example9")
		<-ctx.Done()
		return nil
	}
	c, _, _ := startLocalHelper(t, "test", newFakeEngine(), func(o *helper.ServerOptions) { o.Yield = yield })
	f := routertest.New("203.0.113.10")
	f.Do(func(f *routertest.Router) { f.Next = []netip.Addr{netip.MustParseAddr("203.0.113.20")} })
	srv := serveRouter(t, f, "127.0.0.1:0")
	_, c.routerPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	c.hotspotIndex = func(string) (int, error) { return 7, nil }
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{
			{Name: "example0", Label: "Example Ethernet", Kind: api.KindEthernet, HardwareAddr: exampleExit.HardwareAddr, Up: true, Router: "127.0.0.1"},
			{Name: "example9", Label: "Example Standby", Kind: api.KindThunderbolt, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "192.0.2.1"},
		}, nil
	}
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	c.secretGet = func(context.Context, string, string) (string, error) { return routertest.Password, nil }
	c.secretSet = func(context.Context, string, string, string) error { return errors.New("unexpected password write") }
	c.hotspotPace, c.routerSettle, c.routerPauses = time.Millisecond, time.Millisecond, []time.Duration{time.Millisecond}
	for i := range 2 {
		ln, err := net.Listen([]string{"tcp4", "tcp6"}[i], []string{"127.0.0.1:0", "[::1]:0"}[i])
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var address netip.Addr
			f.Do(func(f *routertest.Router) { address = f.Address })
			if address.String() == "203.0.113.20" && gate != nil {
				select {
				case testingAfter <- struct{}{}:
				default:
				}
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			if i == 1 {
				fmt.Fprint(w, "2001:db8::10")
			} else if address.IsValid() {
				fmt.Fprint(w, address)
			} else {
				http.Error(w, "offline", http.StatusServiceUnavailable)
			}
		}))
		srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		t.Cleanup(srv.Close)
		c.probeURLs[i] = srv.URL
	}
	s, err := c.router(context.Background(), "example0")
	if err != nil {
		t.Fatal(err)
	}
	s.device.Poll = time.Millisecond
	return c, f
}

func TestExitInfoRouterAndLockedRouter(t *testing.T) {
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprint("locked-", locked), func(t *testing.T) {
			c, f := routerExitClient(t, nil, nil)
			if locked {
				c.secretGet = func(context.Context, string, string) (string, error) { return "", keychain.ErrNotFound }
			}
			for range 5 {
				info, err := c.exitInfo(context.Background(), api.InterfaceRef{Name: "example0"}, -1)
				if err != nil || !info.CanChangeIP || !info.Online {
					t.Fatal(info, err)
				}
			}
			f.Do(func(f *routertest.Router) {
				if !slices.Equal(f.Requests, []string{"/CRSP/open/getModelName"}) || f.Logins != 0 {
					t.Fatal("polls contacted the device again", f.Requests, f.Logins)
				}
			})
			if locked {
				if _, err := c.changeExitIP(context.Background(), api.InterfaceRef{Name: "example0"}); api.Code(err) != api.ErrWrongPassword {
					t.Fatal("a locked router didn't return its password error", err)
				}
			}
		})
	}
}

func TestExitChangesShareRouterRenewals(t *testing.T) {
	for _, tt := range []struct{ first, second string }{{"port", "port"}, {"router", "port"}, {"port", "router"}, {"router", "router"}} {
		t.Run(tt.first+"-"+tt.second, func(t *testing.T) {
			gate := make(chan struct{})
			open := sync.OnceFunc(func() { close(gate) })
			defer open()
			after := make(chan struct{}, 2)
			c, f := routerExitClient(t, gate, after)
			ref := api.InterfaceRef{Name: "example0"}
			call := func(ctx context.Context, kind string) (api.RouterRenewal, api.ProxyIPChange, error) {
				if kind == "port" {
					r, err := c.changeExitIP(ctx, ref)
					return api.RouterRenewal{}, r, err
				}
				r, err := c.RenewRouter(ctx, "example0")
				return r, api.ProxyIPChange{BeforeIPv4: r.Before.PublicIPv4, BeforeIPv6: r.Before.PublicIPv6,
					PublicIPv4: r.After.PublicIPv4, PublicIPv6: r.After.PublicIPv6, At: r.After.At, Seconds: r.Took.Seconds()}, err
			}
			done, joined := make(chan struct{}), make(chan struct{})
			var first, second api.ProxyIPChange
			var firstRouter, secondRouter api.RouterRenewal
			var firstErr, secondErr error
			go func() { defer close(done); firstRouter, first, firstErr = call(context.Background(), tt.first) }()
			awaitExit(t, after)
			if !c.exitChanging(ref) {
				t.Fatal("a renewing router wasn't changing")
			}
			// A drained interface is still the same change to join.
			waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
			go func() { defer close(joined); secondRouter, second, secondErr = call(waiting, tt.second) }()
			awaitExit(t, waiting.waiting)
			open()
			awaitExit(t, done)
			awaitExit(t, joined)
			for _, r := range []api.ProxyIPChange{first, second} {
				if r.BeforeIPv4 != "203.0.113.10" || r.PublicIPv4 != "203.0.113.20" || r.BeforeIPv6 != "2001:db8::10" ||
					r.PublicIPv6 != "2001:db8::10" || r.At.IsZero() || r.Seconds <= 0 {
					t.Fatalf("renewal: %+v; %v %v", r, firstErr, secondErr)
				}
			}
			for _, r := range []api.RouterRenewal{firstRouter, secondRouter} {
				if !r.After.At.IsZero() && (r.Standby != "example9" || r.Tries != 1) {
					t.Fatal("joining lost the router's result", r)
				}
			}
			if firstErr != nil || secondErr != nil || first.At != second.At || c.exitChanging(ref) {
				t.Fatal("joined outcomes differ", first, second, firstErr, secondErr)
			}
			f.Do(func(f *routertest.Router) {
				if !slices.Equal(f.Calls, []string{"release", "renew"}) {
					t.Fatal("more than one renewal ran", f.Calls)
				}
			})
			info, err := c.exitInfo(context.Background(), ref, -1)
			if err != nil || info.PublicIPv4 != first.PublicIPv4 || info.PublicIPv6 != first.PublicIPv6 || info.At != first.At {
				t.Fatal("the router's after addresses weren't kept", info, err)
			}
		})
	}
}

func TestExitRouterChangeOutlivesItsCaller(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	after := make(chan struct{}, 2)
	c, f := routerExitClient(t, gate, after)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ref := api.InterfaceRef{Name: "example0"}
	go func() { _, err := c.changeExitIP(ctx, ref); done <- err }()
	awaitExit(t, after)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !c.exitChanging(ref) {
		t.Fatal("the caller ended the router's renewal", err)
	}
	joined := make(chan struct{})
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	var r api.RouterRenewal
	var err error
	go func() { defer close(joined); r, err = c.RenewRouter(waiting, "example0") }()
	awaitExit(t, waiting.waiting)
	open()
	awaitExit(t, joined)
	if err != nil || r.After.PublicIPv4 != "203.0.113.20" || c.exitChanging(ref) {
		t.Fatal(r, err)
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Fatal(f.Calls)
		}
	})
}

func TestExitCloseWaitsForRouterRestoration(t *testing.T) {
	c, f := routerExitClient(t, nil, nil)
	s, err := c.router(context.Background(), "example0")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	released, restoring := make(chan struct{}), make(chan struct{})
	markReleased, markRestoring := sync.OnceFunc(func() { close(released) }), sync.OnceFunc(func() { close(restoring) })
	base := s.device.HTTP.Transport
	s.device.HTTP.Transport = exitRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/updateParameterValues") {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil {
				return nil, err
			}
			if strings.Contains(string(data), ".Renew") {
				markRestoring()
				<-gate
				return base.RoundTrip(r)
			}
			res, err := base.RoundTrip(r)
			markReleased()
			return res, err
		}
		return base.RoundTrip(r)
	})
	c.routerPauses = []time.Duration{time.Hour}
	changed := make(chan error, 1)
	go func() {
		_, err := c.changeExitIP(context.Background(), api.InterfaceRef{Name: "example0"})
		changed <- err
	}()
	awaitExit(t, released)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	awaitExit(t, restoring)
	select {
	case err := <-closed:
		t.Fatal("Close returned before asking the router for an address", err)
	case <-time.After(20 * time.Millisecond):
	}
	open()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-changed; err == nil {
		t.Fatal("a renewal cut short succeeded")
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Fatal("the router wasn't asked for an address", f.Calls)
		}
	})
	if c.exitChanging(api.InterfaceRef{Name: "example0"}) {
		t.Fatal("Close left the router changing")
	}
}

func TestExitRouterRenewalKeepsItsCallersCancellation(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	after := make(chan struct{}, 2)
	c, f := routerExitClient(t, gate, after)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.RenewRouter(ctx, "example0"); done <- err }()
	awaitExit(t, after)
	waiting := &observedWait{Context: context.Background(), waiting: make(chan struct{})}
	joined := make(chan error, 1)
	go func() { _, err := c.changeExitIP(waiting, api.InterfaceRef{Name: "example0"}); joined <- err }()
	awaitExit(t, waiting.waiting)
	cancel()
	for _, err := range []error{<-done, <-joined} {
		if !errors.Is(err, context.Canceled) {
			t.Fatal("a router renewal lost its caller's cancellation", err)
		}
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Fatal(f.Calls)
		}
	})
	if c.exitChanging(api.InterfaceRef{Name: "example0"}) {
		t.Fatal("the canceled router renewal stayed changing")
	}
}
