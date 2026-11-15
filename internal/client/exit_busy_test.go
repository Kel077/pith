//go:build darwin

package client

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/router/routertest"
)

func routerWithHotspotStandby(t *testing.T) (*Client, *routertest.Router, *hotspottest.MiFi) {
	t.Helper()
	c, f := routerExitClient(t, nil, nil)
	h := hotspottest.New()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, c.hotspotPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{
			{Name: "example0", Label: "Example Ethernet", Kind: api.KindEthernet, HardwareAddr: exampleExit.HardwareAddr, Up: true, Router: "127.0.0.1"},
			{Name: "example9", Label: "M2000", Kind: api.KindCellular, HardwareAddr: "02:00:5e:10:00:09", Up: true, Router: "127.0.0.1"},
		}, nil
	}
	s, err := c.hotspot(context.Background(), "example9")
	if err != nil {
		t.Fatal(err)
	}
	s.device.Poll = time.Millisecond
	return c, f, h
}

func callExitChange(ctx context.Context, c *Client, kind, name string) error {
	switch kind {
	case "hotspot":
		_, err := c.RenewHotspot(ctx, name)
		return err
	case "router":
		_, err := c.RenewRouter(ctx, name)
		return err
	default:
		_, err := c.changeExitIP(ctx, api.InterfaceRef{Name: name})
		return err
	}
}

func assertExitBusy(t *testing.T, c *Client, name string) {
	t.Helper()
	for _, kind := range []string{"port", "hotspot", "router"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := callExitChange(ctx, c, kind, name)
		cancel()
		if api.Code(err) != api.ErrBusy {
			t.Fatalf("%s changed %s during another interface's renewal: %v", kind, name, err)
		}
	}
}

func TestExitChangePreservesTheRoutersStandby(t *testing.T) {
	for _, owner := range []string{"port", "router"} {
		t.Run(owner, func(t *testing.T) {
			c, f, h := routerWithHotspotStandby(t)
			defer c.Close()
			s, err := c.router(context.Background(), "example0")
			if err != nil {
				t.Fatal(err)
			}
			released := make(chan struct{})
			mark := sync.OnceFunc(func() { close(released) })
			base := s.device.HTTP.Transport
			s.device.HTTP.Transport = exitRoundTrip(func(r *http.Request) (*http.Response, error) {
				res, err := base.RoundTrip(r)
				if strings.HasSuffix(r.URL.Path, "/updateParameterValues") && r.GetBody != nil {
					body, bodyErr := r.GetBody()
					if bodyErr == nil {
						data, _ := io.ReadAll(body)
						body.Close()
						if strings.Contains(string(data), "_Release") {
							mark()
						}
					}
				}
				return res, err
			})
			c.routerPauses = []time.Duration{time.Hour}
			done := make(chan struct{})
			go func() { defer close(done); _ = callExitChange(context.Background(), c, owner, "example0") }()
			awaitExit(t, released)
			c.mu.Lock()
			standby := c.drained.standby.Name
			c.mu.Unlock()
			if standby != "example9" {
				t.Fatal("the wrong interface carries the Mac", standby)
			}
			assertExitBusy(t, c, "example9")
			h.Do(func(h *hotspottest.MiFi) {
				if len(h.Calls) != 0 || !h.DataOn {
					t.Fatal("the standby's cellular data was changed", h.Calls)
				}
			})
			f.Do(func(f *routertest.Router) {
				if f.Address.IsValid() || !slices.Equal(f.Calls, []string{"release"}) {
					t.Fatal("the router wasn't held without its address", f.Address, f.Calls)
				}
			})
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			awaitExit(t, done)
		})
	}
}

func TestExitRouterWaitsUntilTheHotspotChangeEnds(t *testing.T) {
	for _, owner := range []string{"port", "hotspot"} {
		t.Run(owner, func(t *testing.T) {
			c, f, h := routerWithHotspotStandby(t)
			probes, n := exitClient(t)
			c.probeURLs = probes.probeURLs
			gate := make(chan struct{})
			open := sync.OnceFunc(func() { close(gate) })
			defer func() { open(); _ = c.Close() }()
			n.gate = gate
			done := make(chan struct{})
			var changeErr error
			go func() { defer close(done); changeErr = callExitChange(context.Background(), c, owner, "example9") }()
			awaitExit(t, n.started)
			awaitExit(t, n.started)
			assertExitBusy(t, c, "example0")
			if n.tests() != [2]int{1, 1} {
				t.Fatal("a router tested its standby during the hotspot's before-test", n.tests())
			}
			f.Do(func(f *routertest.Router) {
				if len(f.Calls) != 0 {
					t.Fatal("the router gave its address up", f.Calls)
				}
			})
			open()
			awaitExit(t, done)
			if changeErr != nil {
				t.Fatal(changeErr)
			}
			h.Do(func(h *hotspottest.MiFi) {
				if !slices.Equal(h.Calls, []string{"off", "on"}) {
					t.Fatal("the original hotspot change didn't finish", h.Calls)
				}
			})
		})
	}
}

func TestExitRouterReportsBusyWhileAnotherRouterRenews(t *testing.T) {
	c, _ := routerExitClient(t, nil, nil)
	c.renewing.Lock()
	defer c.renewing.Unlock()
	if _, err := c.RenewRouter(context.Background(), "example0"); api.Code(err) != api.ErrBusy {
		t.Fatal("the router renewal lock didn't report busy", err)
	}
}
