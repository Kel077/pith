package ui

import (
	"context"
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
	"github.com/Kel077/pith/locales"
)

// A new proxy takes the next free port and a connected interface no proxy
// uses yet; a port that is out of range or another proxy's is refused.
func TestProxyAddAndPort(t *testing.T) {
	a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	exits, _ := a.proxyExits(a.cfg.Proxies[0].Via)
	a.addProxy(exits)
	if n := len(a.cfg.Proxies); n != 4 {
		t.Fatalf("%d proxies", n)
	}
	if p := a.cfg.Proxies[3]; p.Port != 1083 || p.Via.Name != "en7" || !p.Enabled || p.Shared {
		t.Errorf("added %+v, want 1083 through the iPhone, on, not shared", p)
	}
	for _, bad := range []string{"1081", "80", "70000", "x"} {
		a.setProxyPort(0, bad)
		if a.cfg.Proxies[0].Port != 1080 {
			t.Errorf("port %q taken: %d", bad, a.cfg.Proxies[0].Port)
		}
	}
	a.setProxyPort(0, "2080")
	if a.cfg.Proxies[0].Port != 2080 || a.settings.editingPort != 0 {
		t.Errorf("port 2080: %+v, editing %d", a.cfg.Proxies[0], a.settings.editingPort)
	}
}

// A proxy goes out through another proxy by its host and port, typed or
// pasted as a URL; what isn't one is refused, as the proxy's own port is.
func TestProxyUpstream(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	before := a.cfg.Proxies[1]
	for _, bad := range []string{"", "  ", "studio.example.com", "studio.example.com:0", "127.0.0.1:1081", "http://studio.example.com:1080", "studio example:1080"} {
		if a.setUpstream(1081, bad) || a.cfg.Proxies[1] != before {
			t.Errorf("upstream %q taken: %+v", bad, a.cfg.Proxies[1])
		}
	}
	for _, typed := range []string{"studio.example.com:1080", " socks5://studio.example.com:1080/ ", "SOCKS5H://studio.example.com:1080"} {
		if !a.setUpstream(1081, typed) {
			t.Fatalf("upstream %q refused", typed)
		}
		if p := a.cfg.Proxies[1]; p.Upstream != "studio.example.com:1080" || p.Via != (api.InterfaceRef{}) || !p.Enabled || p.Port != 1081 {
			t.Fatalf("upstream %q: %+v", typed, p)
		}
	}
	saved, _ := svc.LoadConfig(context.Background())
	if saved.Proxies[1].Upstream != "studio.example.com:1080" {
		t.Errorf("saved %+v", saved.Proxies[1])
	}
	// A proxy that is gone asks for nothing again.
	if !a.setUpstream(4444, "studio.example.com:1080") || len(a.cfg.Proxies) != 3 {
		t.Errorf("a proxy that is gone: %+v", a.cfg.Proxies)
	}
	// The interface of a proxy that goes out through another proxy is free
	// for the next proxy.
	exits, _ := a.proxyExits(api.InterfaceRef{})
	a.addProxy(exits)
	if p := a.cfg.Proxies[3]; p.Via.Name != "en7" {
		t.Errorf("added %+v, want through the iPhone", p)
	}
}

// A proxy goes out by the rules of a scene; when the scene is deleted, by
// those of the scene then selected. Another exit takes the scene's place.
func TestProxyByRules(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	a.page = "settings"
	a.setProxyScene(1, "office")
	if p := a.cfg.Proxies[1]; p.Scene != "office" || p.Via != (api.InterfaceRef{}) || p.Upstream != "" {
		t.Fatalf("by rules: %+v", p)
	}
	saved, _ := svc.LoadConfig(context.Background())
	if saved.Proxies[1].Scene != "office" {
		t.Fatalf("saved %+v", saved.Proxies[1])
	}
	tt := ui.NewTester(a.WindowView, 1040, 1500)
	tt.Frame()
	if want := locales.RulesOfScene("Office"); !tt.HasText(want) {
		t.Errorf("no %q among %q", want, tt.Texts())
	}
	// Its interface is free for the next proxy.
	exits, _ := a.proxyExits(api.InterfaceRef{})
	a.addProxy(exits)
	if p := a.cfg.Proxies[3]; p.Via.Name != "en7" {
		t.Errorf("added %+v, want through the iPhone", p)
	}
	a.deleteScene("office")
	if p := a.cfg.Proxies[1]; p.Scene != "home" || len(a.cfg.Scenes) != 1 {
		t.Fatalf("after its scene was deleted: %+v", p)
	}
	saved, _ = svc.LoadConfig(context.Background())
	if len(saved.Scenes) != 1 || saved.Proxies[1].Scene != "home" {
		t.Fatalf("the deletion was not saved: %+v", saved.Proxies[1])
	}
	if !a.setUpstream(1081, "studio.example.com:1080") || a.cfg.Proxies[1].Scene != "" {
		t.Fatalf("another proxy kept the scene: %+v", a.cfg.Proxies[1])
	}
}

// A device is listed by its IPv4 address or a range of them, as Pith
// compares them; anything else, or one listed already, is refused.
func TestDeviceAdd(t *testing.T) {
	a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	for _, bad := range []string{"", "studio.example.com", "127.0.0.1", "2001:db8::1", "192.0.2.21", "192.0.2.21/32", "0.0.0.0/0"} {
		if a.addDevice(bad, "") || len(a.cfg.Devices) != 1 {
			t.Errorf("device %q taken: %+v", bad, a.cfg.Devices)
		}
	}
	if !a.addDevice(" 192.0.2.44 ", " Phone ") || !a.addDevice("198.51.100.77/24", "") {
		t.Fatal("a device was refused")
	}
	want := []api.Device{{Address: "192.0.2.21", Name: "MacBook Pro"}, {Address: "192.0.2.44", Name: "Phone"}, {Address: "198.51.100.0/24"}}
	if !slices.Equal(a.cfg.Devices, want) {
		t.Errorf("devices %+v, want %+v", a.cfg.Devices, want)
	}
	if got := a.ownAddresses(); !slices.Equal(got, []string{"10.0.1.20", "172.20.10.2", "192.0.2.20"}) {
		t.Errorf("this Mac's addresses %v", got)
	}
	if shared, local := a.proxyAddress(a.cfg.Proxies[0]), a.proxyAddress(a.cfg.Proxies[1]); shared != "10.0.1.20:1080" || local != "127.0.0.1:1081" {
		t.Errorf("addresses %q and %q", shared, local)
	}
}

// The devices are there while a proxy is shared: the laptop with what it
// moves, and a device that was refused, which a click allows. A proxy no
// longer shared keeps the devices listed, with nothing to tell of them.
func TestDevicesGroup(t *testing.T) {
	ctx := context.Background()
	a, svc := testApp(t, mock.Options{Sample: true, HelperInstalled: true, Running: true})
	a.page = "settings"
	tt := ui.NewTester(a.WindowView, 1040, 1500)
	tt.Frame()
	for _, s := range []string{locales.Devices(), "MacBook Pro", "192.0.2.44", locales.DeviceRefused(), "10.0.1.20:1080", "127.0.0.1:1081", locales.ThisMac()} {
		if !tt.HasText(s) {
			t.Errorf("no %q among %q", s, tt.Texts())
		}
	}
	if err := tt.Click(locales.Allow()); err != nil {
		t.Fatal(err)
	}
	if n := len(a.cfg.Devices); n != 2 || a.cfg.Devices[1] != (api.Device{Address: "192.0.2.44"}) {
		t.Fatalf("devices after Allow: %+v", a.cfg.Devices)
	}
	svc.Simulate(2e9)
	a.Sync(ctx)
	tt.Frame()
	if tt.HasText(locales.DeviceRefused()) || tt.HasText(locales.Allow()) {
		t.Errorf("the device allowed is still refused: %q", tt.Texts())
	}

	if err := tt.Click(locales.ShareWithDevices()); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Proxies[0].Shared {
		t.Fatal("the first proxy is still shared")
	}
	a.Sync(ctx)
	tt.Frame()
	if len(a.status.Devices) != 0 || !tt.HasText(locales.Devices()) || !tt.HasText("127.0.0.1:1080") || tt.HasText("10.0.1.20:1080") {
		t.Errorf("with nothing shared: status %+v, %q", a.status.Devices, tt.Texts())
	}

	a.change(func(cfg *api.Config) { cfg.Devices = nil })
	tt.Frame()
	if tt.HasText(locales.Devices()) {
		t.Errorf("devices shown with nothing shared and none listed: %q", tt.Texts())
	}
}

// The sheets that ask for another proxy and for a device keep asking after
// an answer they can't take, and close on one they can.
func TestProxySheets(t *testing.T) {
	a, _ := testApp(t, mock.Options{Sample: true, HelperInstalled: true})
	a.page = "settings"
	tt := ui.NewTester(a.WindowView, 1040, 900)
	a.settings.upstream = upstreamDialog{open: true, port: 1082, address: "nowhere"}
	tt.Frame()
	if err := tt.Click(locales.Done()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.upstream.open || a.cfg.Proxies[2].Upstream != "" {
		t.Fatalf("a bad proxy closed the sheet: %+v", a.cfg.Proxies[2])
	}
	a.settings.upstream.address = "studio.example.com:1080"
	tt.Frame()
	if err := tt.Click(locales.Done()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.upstream.open || a.cfg.Proxies[2].Upstream != "studio.example.com:1080" {
		t.Fatalf("the proxy was not taken: %+v", a.cfg.Proxies[2])
	}

	a.settings.device = deviceDialog{open: true, address: "not an address"}
	tt.Frame()
	if err := tt.Click(locales.Done()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.settings.device.open || len(a.cfg.Devices) != 1 {
		t.Fatalf("a bad device closed the sheet: %+v", a.cfg.Devices)
	}
	a.settings.device.address, a.settings.device.name = "198.51.100.9", "Tablet"
	tt.Frame()
	if err := tt.Click(locales.Done()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.settings.device.open || len(a.cfg.Devices) != 2 || a.cfg.Devices[1] != (api.Device{Address: "198.51.100.9", Name: "Tablet"}) {
		t.Fatalf("the device was not taken: %+v", a.cfg.Devices)
	}
}
