package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/proxy"
)

func proxyPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func proxyEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	closing := false
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closing {
				mu.Unlock()
				conn.Close()
				return
			}
			connections[conn] = true
			workers.Add(1)
			mu.Unlock()
			go func() {
				defer workers.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
				mu.Lock()
				delete(connections, conn)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		closing = true
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return listener.Addr().String()
}

func localProxyClient(t *testing.T) (*Client, *[]string, *sync.Mutex) {
	t.Helper()
	c := testClient(t)
	installProxyFixture(t, c)
	var calls []string
	var mu sync.Mutex
	echo := proxyEchoServer(t)
	c.newProxies = func() (*proxy.Manager, error) {
		return proxy.New(proxy.Options{Dial: func(ctx context.Context, via api.InterfaceRef, target proxy.Target) (net.Conn, error) {
			if target.Host != "192.0.2.80" || target.Port != 443 || target.IsDomain {
				return nil, errors.New("unexpected synthetic target")
			}
			if via.Name == "missing0" {
				return nil, &api.Error{Code: api.ErrInterfaceUnavailable}
			}
			mu.Lock()
			calls = append(calls, via.Name)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp4", echo)
		}, Health: func(_ context.Context, via api.InterfaceRef) error {
			if via.Name == "missing0" {
				return &api.Error{Code: api.ErrInterfaceUnavailable}
			}
			return nil
		}})
	}
	return c, &calls, &mu
}

func installProxyFixture(t *testing.T, c *Client) {
	t.Helper()
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "example0", Kind: api.KindEthernet, Up: true}, {Name: "example1", Kind: api.KindCellular, Up: true}, {Name: "offline0", Kind: api.KindCellular}, {Name: "utun90", Kind: api.KindVPN, Up: true}}, nil
	}
}

func proxyConfig(port int, name string) api.Config {
	cfg := api.DefaultConfig()
	cfg.Proxies = []api.Proxy{{Port: port, Enabled: true, Via: api.InterfaceRef{Name: name}}}
	return cfg
}

func openLocalProxy(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", portString(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0, 5, 1, 0, 1, 192, 0, 2, 80, 1, 187}); err != nil {
		t.Fatal(err)
	}
	var reply [12]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 5 || reply[1] != 0 || reply[2] != 5 || reply[3] != 0 || reply[5] != 1 || binary.BigEndian.Uint16(reply[10:]) == 0 {
		t.Fatal("SOCKS CONNECT failed", reply, err)
	}
	return conn
}

func portString(port int) string { return fmt.Sprint(port) }

func proxyExchange(t *testing.T, conn net.Conn, text string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, text); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len(text))
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != text {
		t.Fatal("proxy payload mismatch", string(data), err)
	}
}

func TestLocalProxyRunsWithUntrustedHelperAndClosesOnChange(t *testing.T) {
	c, calls, mu := localProxyClient(t)
	c.identityErr = errors.New("missing build identity")
	cfg := proxyConfig(proxyPort(t), "example0")
	ctx := context.Background()
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	first := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, first, "first interface")
	if err := c.Stop(ctx); api.Code(err) != api.ErrHelperUntrusted {
		t.Fatal("local proxy changed helper trust checks", err)
	}
	proxyExchange(t, first, "still independent")
	status, err := c.Status(ctx)
	if err != nil || api.Code(status.Error) != api.ErrHelperUntrusted || status.State != api.Failed || len(status.Proxies) != 1 || !status.Proxies[0].Listening || status.Proxies[0].Open != 1 || status.Sent != 0 {
		t.Fatal("local proxy depended on helper readiness or polluted TUN traffic", status, err)
	}
	cfg.Proxies[0].Via.Name = "example1"
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	first.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := first.Read(make([]byte, 1)); err == nil {
		t.Fatal("old mapped stream stayed open")
	}
	second := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, second, "second interface")
	mu.Lock()
	got := append([]string(nil), (*calls)...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "example0" || got[1] != "example1" {
		t.Fatal("incorrect selected interfaces", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", portString(cfg.Proxies[0].Port)), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("Close left a listener")
	}
	if err := c.SaveConfig(ctx, cfg); !errors.Is(err, errClientClosed) {
		t.Fatal("closed client reopened proxies", err)
	}
}

func TestProxySaveKeepsOfflineRefsAndRejectsKnownVirtualExit(t *testing.T) {
	c, _, _ := localProxyClient(t)
	cfg := proxyConfig(proxyPort(t), "missing0")
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	status, _ := c.Status(context.Background())
	if len(status.Proxies) != 1 || !status.Proxies[0].Listening || api.Code(status.Proxies[0].Error) != api.ErrInterfaceUnavailable {
		t.Fatal("unresolved saved intent was lost", status.Proxies)
	}
	before, _ := os.ReadFile(c.configPath)
	cfg.Proxies[0].Via.Name = "utun90"
	if err := c.SaveConfig(context.Background(), cfg); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal("known VPN was accepted", err)
	}
	after, _ := os.ReadFile(c.configPath)
	if string(before) != string(after) {
		t.Fatal("rejected proxy exit changed stored configuration")
	}
}

func TestProxyBindFailureIsSavedAndRetryable(t *testing.T) {
	c, _, _ := localProxyClient(t)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := proxyConfig(busy.Addr().(*net.TCPAddr).Port, "example0")
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal("bind failure discarded valid configuration", err)
	}
	status, _ := c.Status(context.Background())
	if len(status.Proxies) != 1 || status.Proxies[0].Listening || api.Code(status.Proxies[0].Error) != api.ErrPortInUse {
		t.Fatal(status.Proxies)
	}
	busy.Close()
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	proxyExchange(t, openLocalProxy(t, cfg.Proxies[0].Port), "retry")
}

func TestLoadConfigActivatesOnlyOnSuccessfulUncanceledLoad(t *testing.T) {
	for _, action := range []string{"load", "cancel", "cancel-partial", "close"} {
		t.Run(action, func(t *testing.T) {
			c, _, _ := localProxyClient(t)
			cfg := proxyConfig(proxyPort(t), "example0")
			if action == "cancel-partial" {
				second := cfg.Proxies[0].Port + 1
				if second > 65535 {
					second -= 2
				}
				cfg.Proxies = append(cfg.Proxies, api.Proxy{Port: second, Via: api.InterfaceRef{Name: "example1"}, Enabled: true})
			}
			data, _ := json.Marshal(cfg)
			if err := os.MkdirAll(filepath.Dir(c.configPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if action == "load" {
				if states := c.proxyStatus(context.Background()); len(states) != 0 {
					t.Fatal("New started listeners")
				}
				if _, err := c.LoadConfig(context.Background()); err != nil {
					t.Fatal(err)
				}
				proxyExchange(t, openLocalProxy(t, cfg.Proxies[0].Port), "loaded")
				return
			}
			entered := make(chan struct{})
			listens := 0
			c.newProxies = func() (*proxy.Manager, error) {
				return proxy.New(proxy.Options{Dial: func(context.Context, api.InterfaceRef, proxy.Target) (net.Conn, error) {
					return nil, errors.New("unexpected dial")
				}, Listen: func(ctx context.Context, network, address string) (net.Listener, error) {
					listens++
					if action == "cancel-partial" && listens == 1 {
						return (&net.ListenConfig{}).Listen(ctx, network, address)
					}
					close(entered)
					<-ctx.Done()
					return nil, ctx.Err()
				}})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.LoadConfig(ctx); done <- err }()
			<-entered
			if action == "cancel" || action == "cancel-partial" {
				cancel()
			} else {
				closed := make(chan struct{})
				go func() { _ = c.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("Close waited for setup's full timeout")
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled initialization claimed success")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled initialization did not finish")
			}
			if action == "cancel-partial" {
				conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", portString(cfg.Proxies[0].Port)), 100*time.Millisecond)
				if err == nil {
					conn.Close()
					t.Fatal("failed first Load retained a partially activated listener")
				}
			}
		})
	}
}

func TestLocalSubscriptionWithoutHelperStreamsProxyCountersAndCloses(t *testing.T) {
	c, _, _ := localProxyClient(t)
	c.identityErr = errors.New("missing build identity")
	cfg := proxyConfig(proxyPort(t), "example0")
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, second := <-events, <-events
	if first.Status == nil || first.Status.State != api.Failed || api.Code(first.Status.Error) != api.ErrHelperUntrusted || len(first.Status.Proxies) != 1 || second.Interfaces == nil {
		t.Fatal("independent initial snapshots were lost", first, second)
	}
	conn := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, conn, "proxy counters")
	for {
		select {
		case event := <-events:
			if event.Status == nil || len(event.Status.Proxies) != 1 || event.Status.Proxies[0].Sent == 0 || event.Status.Proxies[0].Received == 0 {
				continue
			}
			if event.Status.State != api.Failed || event.Status.Helper.Running || api.Code(event.Status.Error) != api.ErrHelperUntrusted || event.Status.Sent != 0 || event.Status.Received != 0 {
				t.Fatal("proxy updates claimed helper readiness or changed TUN counters", event.Status)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			return
		case <-ctx.Done():
			t.Fatal("proxy counters required a helper", ctx.Err())
		}
	}
}

func TestHelperStreamLossDoesNotEndLocalProxySubscription(t *testing.T) {
	c, stopHelper, _ := startLocalHelper(t, "test", newFakeEngine())
	installProxyFixture(t, c)
	echo := proxyEchoServer(t)
	c.newProxies = func() (*proxy.Manager, error) {
		return proxy.New(proxy.Options{Dial: func(ctx context.Context, _ api.InterfaceRef, _ proxy.Target) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", echo)
		}})
	}
	cfg := proxyConfig(proxyPort(t), "example0")
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-events
	<-events
	stopHelper()
	conn := openLocalProxy(t, cfg.Proxies[0].Port)
	proxyExchange(t, conn, "after helper exit")
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("helper exit closed the local subscription")
			}
			if event.Status == nil || api.Code(event.Status.Error) != api.ErrHelperNotResponding || len(event.Status.Proxies) != 1 || event.Status.Proxies[0].Received == 0 {
				continue
			}
			if event.Status.State != api.Failed || event.Status.Helper.Running || !event.Status.Proxies[0].Listening {
				t.Fatal(event.Status)
			}
			return
		case <-ctx.Done():
			t.Fatal("helper loss hid active proxy counters", ctx.Err())
		}
	}
}

type failReloadTransport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	fail  bool
	calls int
}

func (r *failReloadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/v1/reload" {
		r.mu.Lock()
		r.calls++
		fail := r.fail
		r.fail = false
		r.mu.Unlock()
		if fail {
			return nil, errors.New("synthetic reload failure")
		}
	}
	return r.base.RoundTrip(req)
}

func TestSavedRoutingFailureCannotBeHiddenByDiskEquality(t *testing.T) {
	for _, changeProxy := range []bool{false, true} {
		t.Run(fmt.Sprint(changeProxy), func(t *testing.T) {
			backend := newFakeEngine()
			c, _, _ := startLocalHelper(t, "test", backend)
			installProxyFixture(t, c)
			if err := c.Start(context.Background(), "default"); err != nil {
				t.Fatal(err)
			}
			transport := &failReloadTransport{base: c.http.Transport, fail: true}
			c.http.Transport = transport
			cfg := api.DefaultConfig()
			cfg.Scenes[0].Rules = []api.Rule{{ID: "block", Enabled: true, Action: api.Block, Protocol: api.TCP, Targets: []string{"example.com"}}}
			if err := c.SaveConfig(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "hot reload") {
				t.Fatal("failed helper apply claimed success", err)
			}
			if changeProxy {
				cfg.Proxies = []api.Proxy{{Port: proxyPort(t), Via: api.InterfaceRef{Name: "example0"}}}
			}
			if err := c.SaveConfig(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			transport.mu.Lock()
			calls := transport.calls
			transport.mu.Unlock()
			backend.mu.Lock()
			applied := backend.reloads
			backend.mu.Unlock()
			if calls != 2 || applied != 1 || c.routingPending {
				t.Fatal("saved but unapplied rules were silently skipped", calls, applied)
			}
		})
	}
}

func TestProxyOnlySaveDoesNotReloadRunningTUN(t *testing.T) {
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
	backend.mu.Lock()
	reloads, stops := backend.reloads, backend.stops
	backend.mu.Unlock()
	if reloads != 0 || stops != 0 {
		t.Fatal("local proxy change mutated TUN", reloads, stops)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	stops = backend.stops
	backend.mu.Unlock()
	if stops != 0 {
		t.Fatal("Close stopped the independent helper", stops)
	}
}

func TestSavedRoutingRetryWaitsForMatchingHelper(t *testing.T) {
	for _, code := range []api.ErrorCode{api.ErrHelperOutdated, api.ErrHelperUntrusted} {
		t.Run(string(code), func(t *testing.T) {
			backend := newFakeEngine()
			c, _, _ := startLocalHelper(t, "test", backend)
			if err := c.Start(context.Background(), "default"); err != nil {
				t.Fatal(err)
			}
			checkFile := c.checkFile
			binaryPath := c.binaryPath
			if code == api.ErrHelperOutdated {
				c.checkFile = func(string, bool) error { return errArtifactMismatch }
			} else {
				// The authenticated artifact disappeared, but an executing helper could
				// still own this socket. Installed=false cannot turn unknown into stopped.
				c.binaryPath = filepath.Join(t.TempDir(), "removed-helper")
				c.checkFile = func(string, bool) error { return os.ErrNotExist }
			}
			cfg := api.DefaultConfig()
			cfg.Scenes[0].Rules = []api.Rule{{ID: "block", Enabled: true, Action: api.Block, Protocol: api.TCP, Targets: []string{"example.com"}}}
			if err := c.SaveConfig(context.Background(), cfg); api.Code(err) != code || !c.routingPending {
				t.Fatal("an unavailable helper hid unapplied routing rules", err)
			}
			c.checkFile = checkFile
			c.binaryPath = binaryPath
			if err := c.SaveConfig(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			backend.mu.Lock()
			reloads, rules := backend.reloads, len(backend.lastScene.Rules)
			backend.mu.Unlock()
			if reloads != 1 || rules != 1 || c.routingPending {
				t.Fatal("matching helper did not receive the saved retry", reloads, rules)
			}
		})
	}
}
