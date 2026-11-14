package client

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
)

// sharedProxyClient is a client whose proxies go out to an echo server. Its
// shared listeners stay on loopback, and every connection they take seems
// to come from the device at from.
func sharedProxyClient(t *testing.T, from string) *Client {
	t.Helper()
	c := testClient(t)
	installProxyFixture(t, c)
	echo := proxyEchoServer(t)
	c.newProxies = func() (*proxy.Manager, error) {
		return proxy.New(proxy.Options{
			Dial: func(ctx context.Context, _ api.InterfaceRef, _ proxy.Target) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp4", echo)
			},
			Listen: loopbackListen(from),
		})
	}
	return c
}

// loopbackListen binds every listener on loopback, and has the connections
// of a shared one seem to come from the device at from.
func loopbackListen(from string) func(context.Context, string, string) (net.Listener, error) {
	return func(ctx context.Context, network, address string) (net.Listener, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, net.JoinHostPort("127.0.0.1", port))
		if err != nil || host != "0.0.0.0" {
			return ln, err
		}
		return deviceListener{Listener: ln, from: &net.TCPAddr{IP: net.ParseIP(from), Port: 50000}}, nil
	}
}

type deviceListener struct {
	net.Listener
	from net.Addr
}

func (l deviceListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return deviceConn{TCPConn: conn.(*net.TCPConn), from: l.from}, nil
}

type deviceConn struct {
	*net.TCPConn
	from net.Addr
}

func (c deviceConn) RemoteAddr() net.Addr { return c.from }

func TestSharedProxyReportsItsDevicesWithoutAHelper(t *testing.T) {
	c := sharedProxyClient(t, "192.0.2.10")
	c.identityErr = errors.New("missing build identity")
	cfg := proxyConfig(proxyPort(t), "example0")
	cfg.Proxies[0].Shared = true
	cfg.Devices = []api.Device{{Address: "192.0.2.10", Name: "Laptop"}}
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first := <-events; first.Status == nil || first.Status.Devices != nil {
		t.Fatal("devices before any connected", first)
	}
	conn := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, conn, "from a device")
	await := func(what string, ok func(api.Status) bool) {
		t.Helper()
		for {
			select {
			case event, open := <-events:
				if !open {
					t.Fatal("subscription ended before", what)
				}
				if event.Status != nil && ok(*event.Status) {
					return
				}
			case <-ctx.Done():
				t.Fatal("no event told", what)
			}
		}
	}
	await("the device and what it moved", func(st api.Status) bool {
		if len(st.Devices) != 1 {
			return false
		}
		d := st.Devices[0]
		return d.Address == "192.0.2.10" && d.Allowed && d.Open == 1 && d.Sent == 13 && d.Received == 13 && !d.Seen.IsZero() &&
			st.State == api.Failed && api.Code(st.Error) == api.ErrHelperUntrusted && st.Sent == 0
	})
	if st, _ := c.Status(ctx); len(st.Devices) != 1 || !st.Devices[0].Allowed {
		t.Fatal("Status left the devices out", st.Devices)
	}
	// A device no longer listed loses its stream, and is refused when it
	// comes back; Status then tells of it, to list it again.
	cfg.Devices = nil
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("an unlisted device kept its stream", err)
	}
	await("that no device is left", func(st api.Status) bool { return st.Devices == nil })
	again, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", portString(cfg.Proxies[0].Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	_ = again.SetDeadline(time.Now().Add(3 * time.Second))
	var reply [2]byte
	if _, err := again.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(again, reply[:]); err != nil || reply != [2]byte{5, 0xff} {
		t.Fatal("an unlisted device was not refused", reply, err)
	}
	await("the refused device", func(st api.Status) bool {
		return len(st.Devices) == 1 && st.Devices[0].Address == "192.0.2.10" && !st.Devices[0].Allowed && st.Devices[0].Open == 0
	})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for range events {
	}
}

func TestDeviceOnlySaveDoesNotReloadRunningTUN(t *testing.T) {
	backend := newFakeEngine()
	c, _, _ := startLocalHelper(t, "test", backend)
	installProxyFixture(t, c)
	if err := c.Start(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	cfg := proxyConfig(proxyPort(t), "offline0")
	cfg.Proxies[0].Enabled = false
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Devices = []api.Device{{Address: "192.0.2.10"}, {Address: "198.51.100.0/24", Name: "Studio"}}
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	reloads, stops := backend.reloads, backend.stops
	backend.mu.Unlock()
	if reloads != 0 || stops != 0 {
		t.Fatal("a change of devices mutated TUN", reloads, stops)
	}
	saved, err := c.LoadConfig(context.Background())
	if err != nil || !slices.Equal(saved.Devices, cfg.Devices) {
		t.Fatal("devices were not saved", saved.Devices, err)
	}
	cfg.Devices = append(cfg.Devices, api.Device{Address: "127.0.0.1"})
	if err := c.SaveConfig(context.Background(), cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal("this Mac was accepted as a device", err)
	}
}

// A port that goes out through an upstream proxy has no interface to
// resolve: here the upstream is another port of the same Pith.
func TestProxyThroughAnUpstreamNeedsNoInterface(t *testing.T) {
	c, calls, mu := localProxyClient(t)
	c.identityErr = errors.New("missing build identity")
	far, near := proxyPort(t), proxyPort(t)
	for near == far {
		near = proxyPort(t)
	}
	cfg := proxyConfig(far, "example1")
	cfg.Proxies = append(cfg.Proxies, api.Proxy{Port: near, Upstream: net.JoinHostPort("127.0.0.1", portString(far)), Enabled: true})
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	conn := openLocalProxy(t, near)
	proxyExchange(t, conn, "through another port")
	mu.Lock()
	went := slices.Clone(*calls)
	mu.Unlock()
	if !slices.Equal(went, []string{"example1"}) {
		t.Fatal("the upstream's interface was not the exit", went)
	}
	st, _ := c.Status(context.Background())
	if len(st.Proxies) != 2 || st.Proxies[0].Open != 1 || st.Proxies[1].Open != 1 || st.Proxies[0].Error != nil || st.Proxies[1].Error != nil {
		t.Fatal("status of the two ports", st.Proxies)
	}
	// An interface and an upstream at once is not a configuration.
	cfg.Proxies[1].Via = api.InterfaceRef{Name: "example0"}
	if err := c.SaveConfig(context.Background(), cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal("a port with both exits was saved", err)
	}
}
