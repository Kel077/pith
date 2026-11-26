package api

import (
	"encoding/json"
	"testing"
)

func TestOpenWrtConfig(t *testing.T) {
	via := InterfaceRef{Name: "eth-demo"}
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, router := range []OpenWrtRouter{
		{Via: via, Host: "router.example.com", User: "operator", IdentityFile: "~/.ssh/example key", Command: "/opt/wan/rotate-ip"},
		{Via: via, Host: "192.0.2.1"},
		{Via: via, Host: "2001:db8::1"},
		{Via: via, Host: "openwrt-demo"},
	} {
		cfg.OpenWrt = []OpenWrtRouter{router}
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var loaded Config
		if err := json.Unmarshal(b, &loaded); err != nil {
			t.Fatal(err)
		}
		if err := loaded.Validate(); err != nil || len(loaded.OpenWrt) != 1 || loaded.OpenWrt[0] != router {
			t.Fatalf("round trip = %+v, %v", loaded.OpenWrt, err)
		}
	}
	for _, router := range []OpenWrtRouter{
		{Host: "example.com"},
		{Via: via},
		{Via: via, Host: "-oProxyCommand"},
		{Via: via, Host: "user@example.com"},
		{Via: via, Host: "example.com:22"},
		{Via: via, Host: "example.com", User: "-F"},
		{Via: via, Host: "example.com", IdentityFile: ".ssh/key"},
		{Via: via, Host: "example.com", IdentityFile: "/tmp/key\n"},
		{Via: via, Host: "example.com", Command: "wan-rotate-ip"},
		{Via: via, Host: "example.com", Command: "/usr/sbin/rotate\n--rotate"},
	} {
		cfg.OpenWrt = []OpenWrtRouter{router}
		if err := cfg.Validate(); Code(err) != ErrInvalidConfig {
			t.Errorf("%+v: got %v, want invalid_config", router, err)
		}
	}
	cfg.OpenWrt = []OpenWrtRouter{{Via: via, Host: "router.example.com"}, {Via: via, Host: "standby.example.com"}}
	if err := cfg.Validate(); Code(err) != ErrInvalidConfig {
		t.Errorf("duplicate router: %v", err)
	}
}

func TestOpenWrtIndex(t *testing.T) {
	links := []Interface{{Name: "eth-demo", HardwareAddr: "02:00:00:00:00:41"}}
	routers := []OpenWrtRouter{
		{Via: InterfaceRef{Name: "old-demo", HardwareAddr: links[0].HardwareAddr}},
		{Via: InterfaceRef{Name: links[0].Name}},
	}
	if i := OpenWrtIndex(routers, links, links[0].Name); i != 1 {
		t.Fatalf("exact name = %d", i)
	}
	if i := OpenWrtIndex(routers[:1], links, links[0].Name); i != 0 {
		t.Fatalf("renamed adapter = %d", i)
	}
	links = append(links, Interface{Name: "other-demo", HardwareAddr: links[0].HardwareAddr})
	if i := OpenWrtIndex(routers[:1], links, links[0].Name); i != -1 {
		t.Fatalf("shared hardware address = %d", i)
	}
}
