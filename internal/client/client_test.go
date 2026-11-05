package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{ConfigDir: filepath.Join(t.TempDir(), "config")})
	if err != nil {
		t.Fatal(err)
	}
	c.interfaces = func(context.Context) ([]api.Interface, error) {
		return []api.Interface{{Name: "en0", Label: "Example Ethernet", HardwareAddr: "02:00:00:00:00:01", Up: true, Primary: true}, {Name: "en2", Label: "Example USB", Up: true}}, nil
	}
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.4"), netip.MustParseAddr("2001:db8::4")}, nil
	}
	return c
}

func TestConfigPersistenceIsolationAndValidation(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if _, err := os.Stat(c.opts.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("New wrote a directory")
	}
	cfg, err := c.LoadConfig(ctx)
	if err != nil || !reflect.DeepEqual(cfg, api.DefaultConfig()) {
		t.Fatal(cfg, err)
	}
	cfg.Scenes[0].Name = "Example"
	cfg.Aliases = map[string]string{"02:00:00:00:00:01": "Desk"}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(c.configPath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("configuration permissions: %v", err)
	}
	loaded, err := c.LoadConfig(ctx)
	if err != nil || !reflect.DeepEqual(loaded, cfg) {
		t.Fatal(loaded, err)
	}
	loaded.Scenes[0].Name = "Changed in caller"
	loaded.Aliases["02:00:00:00:00:01"] = "Changed in caller"
	again, _ := c.LoadConfig(ctx)
	if !reflect.DeepEqual(again, cfg) {
		t.Fatal("load aliased state")
	}
	interfaces, err := c.Interfaces(ctx)
	if err != nil || interfaces[0].Label != "Desk" {
		t.Fatal(interfaces, err)
	}
	invalid := cfg
	invalid.Selected = "missing"
	if err := c.SaveConfig(ctx, invalid); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal(err)
	}
	again, _ = c.LoadConfig(ctx)
	if !reflect.DeepEqual(again, cfg) {
		t.Fatal("invalid save changed disk")
	}
	entries, _ := os.ReadDir(c.opts.ConfigDir)
	if len(entries) != 1 {
		t.Fatal("temporary files left behind")
	}
	if err := os.WriteFile(c.configPath, []byte(`{"version":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadConfig(ctx); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.configPath, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadConfig(ctx); api.Code(err) != api.ErrInvalidConfig {
		t.Fatal(err)
	}
}

func TestConcurrentAndCanceledConfig(t *testing.T) {
	c := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SaveConfig(ctx, api.DefaultConfig()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.opts.ConfigDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled save wrote a directory")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			cfg := api.DefaultConfig()
			cfg.Scenes[0].Name = fmt.Sprint(i)
			if err := c.SaveConfig(context.Background(), cfg); err != nil {
				t.Error(err)
			}
			if _, err := c.LoadConfig(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestCheckAndExplicitAddress(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	cfg := api.DefaultConfig()
	cfg.Scenes[0].Rules = []api.Rule{
		{ID: "route", Enabled: true, Protocol: api.AnyProtocol, Action: api.Route, Targets: []string{"*.example.com", "2001:db8::/32"}, Via: []api.InterfaceRef{{Name: "missing"}, {Name: "en2"}}},
		{ID: "block", Enabled: true, Protocol: api.TCP, Action: api.Block, Targets: []string{"192.0.2.8"}},
	}
	if err := c.SaveConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	result, err := c.Check(ctx, api.CheckInput{Target: "https://WWW.EXAMPLE.COM/path", Protocol: api.AnyProtocol})
	if err != nil || result.RuleID != "route" || result.Outcome != api.Fallback || result.Interface != "en2" || len(result.Addresses) != 2 {
		t.Fatal(result, err)
	}
	c.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("unexpected DNS lookup")
		return nil, errors.New("unexpected lookup")
	}
	result, err = c.Check(ctx, api.CheckInput{Target: "example.com", Address: "192.0.2.8"})
	if err != nil || result.RuleID != "block" || result.Outcome != api.Blocked || result.Rules[0].Result != api.CheckShadowed {
		t.Fatal(result, err)
	}
	result, err = c.Check(ctx, api.CheckInput{Target: "[2001:db8::8]:443", Protocol: api.UDP})
	if err != nil || result.RuleID != "route" {
		t.Fatal(result, err)
	}
	result, err = c.Check(ctx, api.CheckInput{Target: "198.51.100.1"})
	if err != nil || result.Outcome != api.System || result.Interface != "en0" {
		t.Fatal(result, err)
	}
	if _, err := c.Check(ctx, api.CheckInput{SceneID: "missing", Target: "example.com"}); api.Code(err) != api.ErrNoScene {
		t.Fatal(err)
	}
	for _, target := range []string{"", "*.example.com", "192.0.2.0/24"} {
		if _, err := c.Check(ctx, api.CheckInput{Target: target}); err == nil {
			t.Error("accepted", target)
		}
	}
}

func TestProbeLocalTCP(t *testing.T) {
	c := testClient(t)
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	result, err := c.Probe(context.Background(), "en0", address)
	listener.Close()
	if err != nil || !result.OK || result.Address != address || result.Latency <= 0 {
		t.Fatal(result, err)
	}
	result, err = c.Probe(context.Background(), "en0", address)
	if err != nil || result.OK || result.Problem == "" {
		t.Fatal(result, err)
	}
	if _, err := c.Probe(context.Background(), "en0", "example.com"); err == nil {
		t.Fatal("accepted missing port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Probe(ctx, "en0", address); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestProbePublicAddressesAndBadResponse(t *testing.T) {
	c := testClient(t)
	c.dialer = func(string, time.Duration, []string) (*net.Dialer, error) {
		return &net.Dialer{Timeout: time.Second}, nil
	}
	v4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "192.0.2.9\n") }))
	defer v4.Close()
	listener6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	v6 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "2001:db8::9") }))
	v6.Listener.Close()
	v6.Listener = listener6
	v6.Start()
	defer v6.Close()
	c.probeURLs = [2]string{v4.URL, v6.URL}
	result, err := c.Probe(context.Background(), "en0", "")
	if err != nil || !result.OK || result.PublicIPv4 != "192.0.2.9" || result.PublicIPv6 != "2001:db8::9" {
		t.Fatal(result, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer bad.Close()
	c.probeURLs = [2]string{bad.URL, bad.URL}
	result, err = c.Probe(context.Background(), "en0", "")
	if err != nil || result.OK || result.Problem == "" {
		t.Fatal(result, err)
	}
}

func TestMissingHelperIsExplicit(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	status, err := c.Status(ctx)
	if err != nil || status.Helper.Installed || status.State != api.Stopped {
		t.Fatal(status, err)
	}
	if err := c.Start(ctx, "default"); api.Code(err) != api.ErrHelperMissing {
		t.Fatal(err)
	}
	if _, err := c.Subscribe(ctx); api.Code(err) != api.ErrHelperMissing {
		t.Fatal(err)
	}
}
