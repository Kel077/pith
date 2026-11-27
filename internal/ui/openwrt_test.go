package ui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/mock"
	"github.com/Kel077/pith/locales"
)

func TestOpenWrtNetworkSettings(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true})
	a.page = "network"
	tt := ui.NewTester(a.WindowView, 1040, 1020)
	tt.Frame()
	if err := tt.Click(locales.OpenWrtRouterSettings()); err != nil {
		t.Fatal(err)
	}
	d := &a.router.openWrt
	if !d.open || d.settings.Via.Name != "en0" {
		t.Fatalf("OpenWrt editor = %+v", d)
	}
	d.settings.Host = "-oProxyCommand"
	tt.Frame()
	if err := tt.Click(locales.Save()); err != nil {
		t.Fatal(err)
	}
	if !d.open || d.problem == "" || len(a.cfg.OpenWrt) != 0 {
		t.Fatalf("invalid router edited the configuration: %+v", d)
	}
	d.settings.Host, d.settings.User = " router.example.com ", "operator"
	d.settings.IdentityFile, d.settings.Command = "~/.ssh/example", "/opt/wan/rotate-ip"
	tt.Frame()
	if err := tt.Click(locales.Save()); err != nil {
		t.Fatal(err)
	}
	saved, err := svc.LoadConfig(t.Context())
	if d.open || err != nil || len(saved.OpenWrt) != 1 || saved.OpenWrt[0].Via.Name != "en0" ||
		saved.OpenWrt[0].Host != "router.example.com" || saved.OpenWrt[0].IdentityFile != "~/.ssh/example" || a.openWrtPending("en0") {
		t.Fatalf("saved OpenWrt = %+v, %v", saved.OpenWrt, err)
	}
	a.readRouters(t.Context())
	tt.Frame()
	if !tt.HasText(locales.OpenWrtBackupOnline()) || !tt.HasText(locales.WanIpv6Prefix()) {
		t.Fatalf("missing OpenWrt status in %q", tt.Texts())
	}
	if err := tt.Click(locales.ChangeIp()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if got := a.router.renewals["en0"]; got.WAN == nil || !got.WAN.MACChanged || got.Standby != "" ||
		!tt.HasText(locales.OpenWrtWanMacChanged("12.0")) {
		t.Fatalf("OpenWrt renewal = %+v in %q", got, tt.Texts())
	}
	if err := tt.Click(locales.OpenWrtRouterSettings()); err != nil {
		t.Fatal(err)
	}
	if !d.open || d.settings.Host != saved.OpenWrt[0].Host {
		t.Fatalf("editor did not reopen with saved values: %+v", d)
	}
	if err := tt.Click(locales.Remove()); err != nil {
		t.Fatal(err)
	}
	a.readRouters(t.Context())
	if a.openWrtConfig("en0").Host != "" || a.router.info["en0"].Model != "CHR30A-R" {
		t.Fatalf("the local Verizon router did not return: %+v", a.router.info["en0"])
	}
}

func TestWiFiHotspotOnNetworkPage(t *testing.T) {
	a, svc := testApp(t, mock.Options{Sample: true, WiFiHotspot: true})
	svc.SetInterfaceUp("en9", false)
	svc.SetInterfaceUp("cell-demo", false)
	svc.LockHotspot()
	a.Sync(t.Context())
	a.page = "network"
	i := a.ifaceByName("en1")
	if h := a.hotspot.info["en1"]; i.Kind != api.KindWiFi || i.Label != "Wi-Fi" || h.Model != "M2000" || !h.Locked || h.Bars != 4 {
		t.Fatalf("Wi-Fi hotspot card = %+v", h)
	}
	tt := ui.NewTester(a.WindowView, 1040, 1150)
	tt.Frame()
	if err := tt.Click(locales.HotspotPassword()); err != nil {
		t.Fatal(err)
	}
	if a.hotspot.unlock.iface != "en1" || !a.hotspot.unlock.open {
		t.Fatalf("Wi-Fi hotspot unlock = %+v", a.hotspot.unlock)
	}
	a.hotspot.unlock.password = "example-password"
	tt.Frame()
	if err := tt.Click(locales.Save()); err != nil {
		t.Fatal(err)
	}
	a.readHotspots(t.Context())
	tt.Frame()
	if h := a.hotspot.info["en1"]; h.Locked || h.Cell == nil || h.Cell.Operator == "" || !tt.HasText(h.Cell.Operator) {
		t.Fatalf("unlocked hotspot = %+v", h)
	}
	if err := tt.Click(locales.ChangeHotspotIp()); err != nil {
		t.Fatal(err)
	}
	if r := a.hotspot.renewals["en1"]; !r.After.OK || r.Before.PublicIPv4 == r.After.PublicIPv4 || r.Before.PublicIPv6 == r.After.PublicIPv6 {
		t.Fatalf("Wi-Fi hotspot result = %+v", r)
	}
}
