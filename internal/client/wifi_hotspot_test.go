//go:build darwin

package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/keychain"
	"github.com/Kel077/pith/internal/netif"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

const wifiMAC = "02:00:00:00:00:51"
const hotspotMAC = "02:00:00:00:00:52"
const otherHotspotMAC = "02:00:00:00:00:53"

func useWiFiHotspot(t *testing.T, c *Client, link *api.Interface, srv *httptest.Server) {
	t.Helper()
	link.Kind, link.Label, link.Router = api.KindWiFi, "Wi-Fi", "127.0.0.1"
	_, c.hotspotPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	c.wifiGatewayMAC = func(context.Context, int, netip.Addr) string { return hotspotMAC }
}

func TestWiFiHotspotCachesAnonymousNegativeResult(t *testing.T) {
	c, n := exitClient(t)
	clock := newExitClock(c)
	var requests, bound atomic.Int32
	opened, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/srv/status" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected discovery request %s", r.URL.Path)
		}
		if requests.Add(1) == 1 {
			close(opened)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	defer unblock()
	useWiFiHotspot(t, c, &n.links[0], server)
	c.dialer = func(name string, _ time.Duration, _ []string) (*net.Dialer, error) {
		if name != exampleExit.Name {
			t.Errorf("discovery dialer = %s", name)
		}
		return &net.Dialer{ControlContext: func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
			bound.Add(1)
			return netif.ValidateBinding(ctx, 7)
		}}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	detached := make(chan error, 1)
	go func() { _, err := c.Hotspot(ctx, exampleExit.Name); detached <- err }()
	awaitExit(t, opened)
	cancel()
	if err := <-detached; err != context.Canceled {
		t.Fatalf("first caller = %v", err)
	}
	// Closing this one-shot gate lets the same detached discovery complete.
	unblock()
	for range 4 {
		if _, err := c.Hotspot(t.Context(), exampleExit.Name); api.Code(err) != api.ErrUnsupported {
			t.Fatalf("ordinary Wi-Fi = %v", err)
		}
		clock.advance(time.Hour)
	}
	n.mu.Lock()
	n.links[0].IPv6 = []string{"2001:db8:2::4"}
	n.mu.Unlock()
	if _, err := c.Hotspot(t.Context(), exampleExit.Name); api.Code(err) != api.ErrUnsupported || requests.Load() != 1 || bound.Load() != 1 {
		t.Fatalf("negative cache after IPv6 change = %v, requests %d, bound %d", err, requests.Load(), bound.Load())
	}
	n.mu.Lock()
	n.links[0].IPv4 = []string{"192.0.2.20"}
	n.mu.Unlock()
	if _, err := c.Hotspot(t.Context(), exampleExit.Name); api.Code(err) != api.ErrUnsupported || requests.Load() != 2 {
		t.Fatalf("changed LAN snapshot = %v, requests %d", err, requests.Load())
	}
}

func TestWiFiHotspotRecognizesAndRemembersEachGateway(t *testing.T) {
	c, n := exitClient(t)
	first, second := hotspottest.New(), hotspottest.New()
	var neighbor atomic.Pointer[string]
	var switchDevice atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if switchDevice.Load() {
			second.ServeHTTP(w, r)
		} else {
			first.ServeHTTP(w, r)
			neighbor.Store(new(hotspotMAC))
		}
	}))
	defer server.Close()
	useWiFiHotspot(t, c, &n.links[0], server)
	n.links[0].HardwareAddr = wifiMAC
	c.wifiGatewayMAC = func(context.Context, int, netip.Addr) string {
		if mac := neighbor.Load(); mac != nil {
			return *mac
		}
		return ""
	}
	secrets := map[string]string{}
	c.secretGet = func(_ context.Context, service, account string) (string, error) {
		if p := secrets[service+"/"+account]; p != "" {
			return p, nil
		}
		return "", keychain.ErrNotFound
	}
	c.secretSet = func(_ context.Context, service, account, password string) error {
		secrets[service+"/"+account] = password
		return nil
	}
	ctx := t.Context()
	name := exampleExit.Name
	if h, err := c.Hotspot(ctx, name); err != nil || h.Model != "M2000" || !h.Connected || !h.Locked || h.Bars != 4 {
		t.Fatalf("locked Wi-Fi M2000 = %+v, %v", h, err)
	}
	if err := c.SetHotspotPassword(ctx, name, hotspottest.Password); err != nil {
		t.Fatal(err)
	}
	firstAccount := hotspotKeychain + "/wifi:" + wifiMAC + "/" + hotspotMAC
	if len(secrets) != 1 || secrets[firstAccount] != hotspottest.Password {
		t.Fatal("password was not saved under the gateway identity")
	}
	// Losing the active session leaves the saved password for the same device.
	n.mu.Lock()
	n.links[0].Up = false
	n.mu.Unlock()
	if _, err := c.Hotspot(ctx, name); api.Code(err) != api.ErrUnsupported {
		t.Fatalf("down Wi-Fi = %v", err)
	}
	n.mu.Lock()
	n.links[0].Up = true
	n.mu.Unlock()
	if h, err := c.Hotspot(ctx, name); err != nil || h.Locked || h.Cell == nil || h.Cell.Operator != "Example Mobile" {
		t.Fatalf("known Wi-Fi M2000 = %+v, %v", h, err)
	}
	old, err := c.hotspot(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	// Roam to another device with the same gateway IPv4 and local IP.
	switchDevice.Store(true)
	neighbor.Store(new(otherHotspotMAC))
	if err := old.device.TryPassword(ctx, hotspottest.Password); api.Code(err) != api.ErrInterfaceUnavailable {
		t.Fatalf("old web session reached a replacement gateway: %v", err)
	}
	if h, err := c.Hotspot(ctx, name); err != nil || !h.Locked || h.Model != "M2000" {
		t.Fatalf("second Wi-Fi M2000 = %+v, %v", h, err)
	}
	second.Do(func(f *hotspottest.MiFi) {
		if f.Logins != 0 {
			t.Fatal("the second hotspot received a password from the first")
		}
	})
	if err := c.SetHotspotPassword(ctx, name, hotspottest.Password); err != nil || len(secrets) != 2 {
		t.Fatalf("second hotspot password: %v", err)
	}
}

func TestWiFiHotspotProxyCanChangeAndRenews(t *testing.T) {
	c, n, f, _ := exitPortHotspotOn(t, nil, api.KindWiFi)
	installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, exampleExit.Name)
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if info := readExitPort(t, port, "/"); !info.CanChangeIP || !info.Online {
		t.Fatalf("Wi-Fi hotspot port = %+v", info)
	}
	got := askExitPort(port, http.MethodPost, "/change-ip", nil)
	if got.status != http.StatusOK || got.err != nil || got.change.BeforeIPv4 != "198.51.100.10" ||
		got.change.PublicIPv4 != "198.51.100.20" || got.change.PublicIPv6 != "2001:db8::20" || got.change.WAN != nil {
		t.Fatalf("Wi-Fi hotspot POST = %d %s, %v", got.status, got.body, got.err)
	}
	calls := n.tests()
	if after := readExitPort(t, port, "/"); !after.CanChangeIP || after.PublicIPv4 != got.change.PublicIPv4 || calls != n.tests() {
		t.Fatalf("Wi-Fi hotspot after change = %+v", after)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if len(f.Calls) != 2 || f.Calls[0] != "off" || f.Calls[1] != "on" || !f.DataOn {
			t.Fatalf("hotspot calls = %+v", f.Calls)
		}
	})
}

func TestWiFiGatewayMACUsesSelectedInterface(t *testing.T) {
	gateway := netip.MustParseAddr("192.0.2.1")
	neighbor := func(index int, raw string) *route.RouteMessage {
		mac, err := net.ParseMAC(raw)
		if err != nil {
			t.Fatal(err)
		}
		addrs := make([]route.Addr, unix.RTAX_GATEWAY+1)
		addrs[unix.RTAX_DST] = &route.Inet4Addr{IP: gateway.As4()}
		addrs[unix.RTAX_GATEWAY] = &route.LinkAddr{Index: index, Addr: mac}
		return &route.RouteMessage{Index: index, Flags: unix.RTF_LLINFO, Addrs: addrs}
	}
	messages := []route.Message{neighbor(2, otherHotspotMAC), neighbor(7, hotspotMAC)}
	if got := findGatewayMAC(messages, 7, gateway); got != hotspotMAC {
		t.Fatalf("scoped ARP = %q", got)
	}
	messages = append(messages, neighbor(7, otherHotspotMAC))
	if got := findGatewayMAC(messages, 7, gateway); got != "" {
		t.Fatalf("ambiguous scoped ARP = %q", got)
	}
}
