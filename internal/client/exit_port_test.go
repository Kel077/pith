//go:build darwin

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot/hotspottest"
	"github.com/Kel077/pith/internal/proxy"
	"github.com/Kel077/pith/internal/router/routertest"
)

type exitPortHooks struct {
	info, change, changing atomic.Int64
	entered                chan struct{}
}

type exitPortWait struct {
	context.Context
	once    sync.Once
	entered chan<- struct{}
}

func (w *exitPortWait) Done() <-chan struct{} {
	w.once.Do(func() { w.entered <- struct{}{} })
	return w.Context.Done()
}

// installExitPort keeps the actual manager and protocols, with forwarding
// confined to an echo server and shared peers supplied by the fixture.
func installExitPort(t *testing.T, c *Client, peer string) *exitPortHooks {
	t.Helper()
	h := &exitPortHooks{entered: make(chan struct{}, 16)}
	echo := proxyEchoServer(t)
	c.newProxies = func() (*proxy.Manager, error) {
		return proxy.New(proxy.Options{
			Dial: func(ctx context.Context, _ api.InterfaceRef, _ proxy.Target) (net.Conn, error) {
				return dialProxyPeer(ctx, echo)
			},
			Listen: loopbackListen(peer),
			Info: func(ctx context.Context, via api.InterfaceRef, age time.Duration) (api.ProxyInfo, error) {
				h.info.Add(1)
				return c.exitInfo(ctx, via, age)
			},
			ChangeIP: func(ctx context.Context, via api.InterfaceRef) (api.ProxyIPChange, error) {
				h.change.Add(1)
				return c.changeExitIP(&exitPortWait{Context: ctx, entered: h.entered}, via)
			},
			Changing: func(via api.InterfaceRef) bool {
				h.changing.Add(1)
				return c.exitChanging(via)
			},
		})
	}
	return h
}

func exitPortHotspot(t *testing.T, gate <-chan struct{}) (*Client, *exitNetwork, *hotspottest.MiFi, <-chan struct{}) {
	t.Helper()
	return exitPortHotspotOn(t, gate, api.KindCellular)
}

func exitPortHotspotOn(t *testing.T, gate <-chan struct{}, kind api.InterfaceKind) (*Client, *exitNetwork, *hotspottest.MiFi, <-chan struct{}) {
	t.Helper()
	c, n := exitClient(t)
	n.links[0].Kind, n.links[0].Label, n.links[0].Router = api.KindCellular, "M2000", "127.0.0.1"
	n.public = [2]string{"198.51.100.10", "2001:db8::10"}
	f := hotspottest.New()
	enabled := make(chan struct{})
	mark := sync.OnceFunc(func() { close(enabled) })
	var generation int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.ServeHTTP(w, r)
		if r.URL.Path == "/wwan/enablecellulardata/" {
			n.mu.Lock()
			generation++
			n.public = [2]string{fmt.Sprintf("198.51.100.%d", 10*(generation+1)), fmt.Sprintf("2001:db8::%d", 10*(generation+1))}
			n.gate = gate
			n.mu.Unlock()
			mark()
		}
	}))
	t.Cleanup(srv.Close)
	_, c.hotspotPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	if kind == api.KindWiFi {
		useWiFiHotspot(t, c, &n.links[0], srv)
	}
	c.secretGet = func(context.Context, string, string) (string, error) { return hotspottest.Password, nil }
	c.hotspotPace = time.Millisecond
	s, err := c.hotspot(context.Background(), "example0")
	if err != nil {
		t.Fatal(err)
	}
	s.device.Poll = time.Millisecond
	return c, n, f, enabled
}

type exitPortAnswer struct {
	status  int
	body    []byte
	change  api.ProxyIPChange
	problem *api.Error
	err     error
}

func askExitPort(port int, method, path string, origin *string) (answer exitPortAnswer) {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	req, err := http.NewRequest(method, "http://"+net.JoinHostPort("127.0.0.1", portString(port))+path, nil)
	if err != nil {
		answer.err = err
		return
	}
	if origin != nil {
		req.Header.Set("Origin", *origin)
	}
	res, err := client.Do(req)
	if err != nil {
		answer.err = err
		return
	}
	defer res.Body.Close()
	answer.status = res.StatusCode
	answer.body, answer.err = io.ReadAll(res.Body)
	if answer.err == nil {
		if method == http.MethodPost && answer.status == http.StatusOK {
			answer.err = json.Unmarshal(answer.body, &answer.change)
		}
		var envelope struct{ Error *api.Error }
		_ = json.Unmarshal(answer.body, &envelope)
		answer.problem = envelope.Error
	}
	return
}

func readExitPort(t *testing.T, port int, path string) api.ProxyInfo {
	t.Helper()
	answer := askExitPort(port, http.MethodGet, path, nil)
	var info api.ProxyInfo
	if answer.err != nil || answer.status != http.StatusOK {
		t.Fatalf("GET %s: %d %s %v", path, answer.status, answer.body, answer.err)
	}
	if err := json.Unmarshal(answer.body, &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func exitPortStatus(t *testing.T, c *Client, port int) api.ProxyStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := c.Status(ctx)
	for _, p := range status.Proxies {
		if p.Port == port {
			return p
		}
	}
	t.Fatalf("port %d missing from status: %+v %v", port, status, err)
	return api.ProxyStatus{}
}

func TestExitPortInfoAndLiveChangePermission(t *testing.T) {
	c, n, _, _ := exitPortHotspot(t, nil)
	installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, "example0")
	cfg.Aliases = map[string]string{exampleExit.HardwareAddr: "Desk hotspot"}
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	stream := openLocalProxy(t, port)
	proxyExchange(t, stream, "before permission change")
	before := exitPortStatus(t, c, port)
	info := readExitPort(t, port, "/")
	if info.Pith != api.ProxyInfoVersion || info.Port != port || info.Exit != api.ExitInterface || info.Interface != "Desk hotspot" ||
		!info.Online || info.CanChangeIP || info.PublicIPv4 != "198.51.100.10" || info.PublicIPv6 != "2001:db8::10" || info.At.IsZero() {
		t.Fatal("port didn't describe its interface", info)
	}
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	proxyExchange(t, stream, "after permission change")
	info = readExitPort(t, port, "/")
	after := exitPortStatus(t, c, port)
	if !info.CanChangeIP || n.tests() != [2]int{1, 1} || !before.Since.Equal(after.Since) || after.Open != 1 {
		t.Fatal("permission change replaced the listener or repeated the test", info, before, after, n.tests())
	}
	refreshed := readExitPort(t, port, "/?max-age=0")
	if refreshed.PublicIPv4 != info.PublicIPv4 || n.tests() != [2]int{2, 2} {
		t.Fatal("explicit GET didn't make exactly one new test", refreshed, n.tests())
	}
}

func TestExitPortSharesHotspotChanges(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	c, n, f, enabled := exitPortHotspot(t, gate)
	hooks := installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, "example0")
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	denied := askExitPort(port, http.MethodPost, "/change-ip", nil)
	if denied.err != nil || denied.status != http.StatusForbidden || api.Code(denied.problem) != api.ErrNotAllowed || hooks.change.Load() != 0 {
		t.Fatal("disabled permission reached the change hook", denied)
	}
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	answers := make(chan exitPortAnswer, 2)
	go func() { answers <- askExitPort(port, http.MethodPost, "/change-ip", nil) }()
	awaitExit(t, hooks.entered)
	awaitExit(t, enabled)
	if !exitPortStatus(t, c, port).Changing {
		t.Fatal("Status didn't report the hotspot change")
	}
	changing := readExitPort(t, port, "/")
	if !changing.Changing || changing.PublicIPv4 != "" || changing.PublicIPv6 != "" || !changing.At.IsZero() {
		t.Fatal("the hotspot kept reporting its old addresses while renewing", changing)
	}
	go func() { answers <- askExitPort(port, http.MethodPost, "/change-ip", nil) }()
	awaitExit(t, hooks.entered)
	open()
	var result api.ProxyIPChange
	for range 2 {
		answer := <-answers
		result = answer.change
		if answer.err != nil || answer.status != http.StatusOK || result.Port != port || result.BeforeIPv4 != "198.51.100.10" ||
			result.BeforeIPv6 != "2001:db8::10" || result.PublicIPv4 != "198.51.100.20" || result.PublicIPv6 != "2001:db8::20" || result.At.IsZero() {
			t.Fatal("POST didn't return the shared renewal", answer, result)
		}
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !slices.Equal(f.Calls, []string{"off", "on"}) {
			t.Fatal("two POSTs renewed more than once", f.Calls)
		}
	})
	before := n.tests()
	info := readExitPort(t, port, "/")
	if exitPortStatus(t, c, port).Changing || info.PublicIPv4 != result.PublicIPv4 || info.PublicIPv6 != result.PublicIPv6 ||
		!info.At.Equal(result.At) || n.tests() != before {
		t.Fatal("GET lost the after addresses or repeated the test", info, result, n.tests())
	}
}

func TestExitPortRouterSuspendsStreamsAndAnswersItsRequest(t *testing.T) {
	gate, after := make(chan struct{}), make(chan struct{}, 2)
	open := sync.OnceFunc(func() { close(gate) })
	defer open()
	c, f := routerExitClient(t, gate, after)
	installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, "example0")
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	stream := openLocalProxy(t, port)
	proxyExchange(t, stream, "before router release")
	answers := make(chan exitPortAnswer, 1)
	go func() { answers <- askExitPort(port, http.MethodPost, "/change-ip", nil) }()
	awaitExit(t, after)
	info := readExitPort(t, port, "/")
	if info.Online || !info.Changing || !exitPortStatus(t, c, port).Changing {
		t.Fatal("a suspended router wasn't reported offline and changing", info)
	}
	if info.PublicIPv4 != "" || info.PublicIPv6 != "" || !info.At.IsZero() {
		t.Fatal("the router kept reporting its old addresses while renewing", info)
	}
	_, err := stream.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("the router's stream stayed open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("the router's stream timed out instead of being closed", err)
	}
	open()
	answer := <-answers
	result := answer.change
	if answer.err != nil || answer.status != http.StatusOK || result.BeforeIPv4 != "203.0.113.10" || result.PublicIPv4 != "203.0.113.20" ||
		result.BeforeIPv6 != "2001:db8::10" || result.PublicIPv6 != "2001:db8::10" || result.Port != port {
		t.Fatal("the POST was closed by its own suspension", answer, result)
	}
	f.Do(func(f *routertest.Router) {
		if !slices.Equal(f.Calls, []string{"release", "renew"}) {
			t.Fatal(f.Calls)
		}
	})
	info = readExitPort(t, port, "/")
	if !info.Online || info.Changing || exitPortStatus(t, c, port).Changing || info.PublicIPv4 != result.PublicIPv4 {
		t.Fatal("the router's port didn't resume", info)
	}
	proxyExchange(t, openLocalProxy(t, port), "after router renewal")
}

func TestExitPortUpstreamUsesBothPermissions(t *testing.T) {
	far, _, f, _ := exitPortHotspot(t, nil)
	installExitPort(t, far, "192.0.2.10")
	farPort := proxyPort(t)
	farConfig := proxyConfig(farPort, "example0")
	farConfig.Proxies[0].Shared, farConfig.Proxies[0].ChangeIP = true, true
	farConfig.Devices = []api.Device{{Address: "192.0.2.10", Name: "Second client"}}
	if err := far.SaveConfig(context.Background(), farConfig); err != nil {
		t.Fatal(err)
	}
	near := testClient(t)
	installExitPort(t, near, "192.0.2.20")
	nearPort := proxyPort(t)
	for nearPort == farPort {
		nearPort = proxyPort(t)
	}
	nearConfig := api.DefaultConfig()
	nearConfig.Proxies = []api.Proxy{{Port: nearPort, Enabled: true, Upstream: net.JoinHostPort("127.0.0.1", portString(farPort))}}
	if err := near.SaveConfig(context.Background(), nearConfig); err != nil {
		t.Fatal(err)
	}
	// Status starts the manager's upstream check, independently of a GET.
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := exitPortStatus(t, near, nearPort)
		if state.Upstream != nil {
			if state.Upstream.Port != farPort || state.Upstream.PublicIPv4 != "198.51.100.10" {
				t.Fatal("status lost the upstream answer", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status didn't learn the upstream answer", state)
		}
		time.Sleep(time.Millisecond)
	}
	info := readExitPort(t, nearPort, "/")
	if info.Exit != api.ExitUpstream || info.Upstream == nil || info.Upstream.Port != farPort || info.PublicIPv4 != "198.51.100.10" ||
		info.PublicIPv6 != "2001:db8::10" || !info.At.Equal(info.Upstream.At) || info.CanChangeIP || !info.Upstream.CanChangeIP {
		t.Fatal("upstream information wasn't nested and copied", info)
	}
	// The local service bypasses its own port's permission, but not the upstream's.
	result, err := near.ChangeProxyIP(context.Background(), nearPort)
	if err != nil || result.Port != nearPort || result.PublicIPv4 != "198.51.100.20" {
		t.Fatal("the local service required its own port's permission", result, err)
	}
	for _, flags := range [][2]bool{{true, false}, {false, false}, {false, true}} {
		farConfig.Proxies[0].ChangeIP, nearConfig.Proxies[0].ChangeIP = flags[0], flags[1]
		if err := far.SaveConfig(context.Background(), farConfig); err != nil {
			t.Fatal(err)
		}
		if err := near.SaveConfig(context.Background(), nearConfig); err != nil {
			t.Fatal(err)
		}
		answer := askExitPort(nearPort, http.MethodPost, "/change-ip", nil)
		if answer.err != nil || answer.status != http.StatusForbidden || api.Code(answer.problem) != api.ErrNotAllowed {
			t.Fatal("POST didn't require both permissions", flags, answer)
		}
		if !flags[0] {
			if _, err := near.ChangeProxyIP(context.Background(), nearPort); api.Code(err) != api.ErrNotAllowed {
				t.Fatal("local service bypassed the upstream permission", flags, err)
			}
		}
	}
	farConfig.Proxies[0].ChangeIP, nearConfig.Proxies[0].ChangeIP = true, true
	if err := far.SaveConfig(context.Background(), farConfig); err != nil {
		t.Fatal(err)
	}
	if err := near.SaveConfig(context.Background(), nearConfig); err != nil {
		t.Fatal(err)
	}
	answer := askExitPort(nearPort, http.MethodPost, "/change-ip", nil)
	if answer.err != nil || answer.status != http.StatusOK || answer.change.Port != nearPort || answer.change.BeforeIPv4 != "198.51.100.20" ||
		answer.change.PublicIPv4 != "198.51.100.30" {
		t.Fatal("two allowed ports didn't forward the change", answer, answer.change)
	}
	f.Do(func(f *hotspottest.MiFi) {
		if !slices.Equal(f.Calls, []string{"off", "on", "off", "on"}) {
			t.Fatal("refusals renewed the hotspot", f.Calls)
		}
	})
}

func TestExitPortRejectsOriginsBeforeHooks(t *testing.T) {
	c, n, _, _ := exitPortHotspot(t, nil)
	hooks := installExitPort(t, c, "192.0.2.10")
	port := proxyPort(t)
	cfg := proxyConfig(port, "example0")
	cfg.Proxies[0].ChangeIP = true
	if err := c.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://example.com", ""} {
		for _, request := range [][2]string{{http.MethodGet, "/"}, {http.MethodPost, "/change-ip"}} {
			answer := askExitPort(port, request[0], request[1], &origin)
			if answer.err != nil || answer.status != http.StatusForbidden || api.Code(answer.problem) != api.ErrNotAllowed {
				t.Fatal("a browser request wasn't refused", request, answer)
			}
		}
	}
	if hooks.info.Load() != 0 || hooks.change.Load() != 0 || hooks.changing.Load() != 0 || n.tests() != [2]int{} {
		t.Fatal("a refused browser request reached a hook", hooks.info.Load(), hooks.change.Load(), hooks.changing.Load(), n.tests())
	}
}
