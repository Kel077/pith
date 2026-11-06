package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProxyConfigurationValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proxies []Proxy
		valid   bool
	}{
		{"empty", nil, true},
		{"minimum", []Proxy{{Port: 1024, Via: InterfaceRef{Name: "example0"}, Enabled: true}}, true},
		{"maximum", []Proxy{{Port: 65535, Via: InterfaceRef{Name: "missing0"}}}, true},
		{"privileged", []Proxy{{Port: 1023, Via: InterfaceRef{Name: "example0"}}}, false},
		{"negative", []Proxy{{Port: -1, Via: InterfaceRef{Name: "example0"}}}, false},
		{"overflow", []Proxy{{Port: 65536, Via: InterfaceRef{Name: "example0"}}}, false},
		{"no exit", []Proxy{{Port: 1080}}, false},
		{"duplicate disabled", []Proxy{{Port: 1080, Via: InterfaceRef{Name: "example0"}, Enabled: true}, {Port: 1080, Via: InterfaceRef{Name: "example1"}}}, false},
		{"shared exit", []Proxy{{Port: 1080, Via: InterfaceRef{Name: "example0"}}, {Port: 1081, Via: InterfaceRef{Name: "example0"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Proxies = tc.proxies
			err := cfg.Validate()
			if (err == nil) != tc.valid || err != nil && Code(err) != ErrInvalidConfig {
				t.Fatal("unexpected proxy configuration result", err)
			}
		})
	}
	cfg := DefaultConfig()
	for i := range MaxProxies + 1 {
		cfg.Proxies = append(cfg.Proxies, Proxy{Port: 1024 + i, Via: InterfaceRef{Name: "offline0"}})
	}
	if Code(cfg.Validate()) != ErrInvalidConfig {
		t.Fatal("oversized proxy list was accepted")
	}
	cfg.Proxies = cfg.Proxies[:MaxProxies]
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyEligibilityDoesNotRequireAnOnlineInterface(t *testing.T) {
	for _, kind := range []InterfaceKind{KindWiFi, KindEthernet, KindCellular, KindThunderbolt} {
		for _, up := range []bool{false, true} {
			if !(Interface{Name: "example0", Kind: kind, Up: up}).CanProxy() {
				t.Fatal("supported physical interface rejected", kind, up)
			}
		}
	}
	for _, iface := range []Interface{{Name: "example0", Kind: KindVPN}, {Name: "example0", Kind: KindOther}, {Kind: KindEthernet}, {Name: "example0"}} {
		if iface.CanProxy() {
			t.Fatal("unknown or virtual interface accepted", iface)
		}
	}
	for _, name := range []string{"lo0", "utun42", "UTUN42", "ipsec0", "ppp0", "bridge0", "gif0", "stf0", "awdl0", "llw0", "anpi0", "tun0", "tap0", "vmenet0"} {
		if (Interface{Name: name, Kind: KindEthernet, Up: true}).CanProxy() {
			t.Fatal("virtual interface passed through a physical-looking label", name)
		}
	}
}

func TestProxyJSONAndIndependentStatus(t *testing.T) {
	cfg := DefaultConfig()
	before, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(before), `"proxies"`) {
		t.Fatal("empty proxy configuration changed legacy JSON", string(before), err)
	}
	cfg.Proxies = []Proxy{{Port: 1080, Via: InterfaceRef{Name: "example0"}, Enabled: true}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Proxies) != 1 || decoded.Proxies[0] != cfg.Proxies[0] {
		t.Fatal("proxy configuration did not round trip", err)
	}
	status := Status{State: Failed, Error: &Error{Code: ErrHelperUntrusted}, Proxies: []ProxyStatus{{Port: 1080, Listening: true, Since: time.Unix(1000, 0).UTC(), Open: 1, Sent: 17, Received: 23}}}
	data, err = json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var actual Status
	if err := json.Unmarshal(data, &actual); err != nil || actual.Error.Code != ErrHelperUntrusted || len(actual.Proxies) != 1 || actual.Proxies[0] != status.Proxies[0] || actual.Open != 0 || actual.Sent != 0 {
		t.Fatal("helper error or local proxy status was lost or merged into TUN counters", string(data), err)
	}
}
