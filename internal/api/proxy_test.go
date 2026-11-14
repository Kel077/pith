package api

import (
	"encoding/json"
	"net/netip"
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

func TestProxyGoesOutThroughAnInterfaceOrAnUpstream(t *testing.T) {
	via := InterfaceRef{Name: "example0"}
	for _, tc := range []struct {
		name  string
		proxy Proxy
		valid bool
	}{
		{"name", Proxy{Port: 1080, Upstream: "studio.example.com:1080"}, true},
		{"single label", Proxy{Port: 1080, Upstream: "studio:1080"}, true},
		{"local name", Proxy{Port: 1080, Upstream: "Studio-Mac.local:1081"}, true},
		{"ipv4", Proxy{Port: 1080, Upstream: "192.0.2.7:1080"}, true},
		{"ipv6", Proxy{Port: 1080, Upstream: "[2001:db8::7]:1080"}, true},
		{"another port of this Mac", Proxy{Port: 1080, Upstream: "127.0.0.1:1081"}, true},
		{"shared", Proxy{Port: 1080, Upstream: "192.0.2.7:1080", Shared: true}, true},
		{"both", Proxy{Port: 1080, Via: via, Upstream: "192.0.2.7:1080"}, false},
		{"no port", Proxy{Port: 1080, Upstream: "studio.example.com"}, false},
		{"port zero", Proxy{Port: 1080, Upstream: "studio.example.com:0"}, false},
		{"port overflow", Proxy{Port: 1080, Upstream: "studio.example.com:65536"}, false},
		{"padded port", Proxy{Port: 1080, Upstream: "studio.example.com:01080"}, false},
		{"no host", Proxy{Port: 1080, Upstream: ":1080"}, false},
		{"url", Proxy{Port: 1080, Upstream: "socks5://192.0.2.7:1080"}, false},
		{"space", Proxy{Port: 1080, Upstream: "studio example:1080"}, false},
		{"unspecified", Proxy{Port: 1080, Upstream: "0.0.0.0:1080"}, false},
		{"itself", Proxy{Port: 1080, Upstream: "127.0.0.1:1080"}, false},
		{"itself by name", Proxy{Port: 1080, Upstream: "localhost:1080"}, false},
		{"itself over IPv6", Proxy{Port: 1080, Upstream: "[::1]:1080"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Proxies = []Proxy{tc.proxy}
			err := cfg.Validate()
			if (err == nil) != tc.valid || err != nil && Code(err) != ErrInvalidConfig {
				t.Fatal("unexpected upstream result", err)
			}
		})
	}
	cfg := DefaultConfig()
	cfg.Proxies = []Proxy{{Port: 1080, Upstream: "studio.example.com:1080", Enabled: true, Shared: true}}
	data, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(data), `"via"`) {
		t.Fatal("an upstream proxy was saved with an interface", string(data), err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Proxies) != 1 || decoded.Proxies[0] != cfg.Proxies[0] {
		t.Fatal("upstream proxy did not round trip", err)
	}
	// A configuration saved before proxies could be shared reads as before.
	var legacy Proxy
	if err := json.Unmarshal([]byte(`{"port":1080,"via":{"name":"example0"},"enabled":true}`), &legacy); err != nil || legacy != (Proxy{Port: 1080, Via: via, Enabled: true}) {
		t.Fatal("legacy proxy changed", legacy, err)
	}
}

func TestDevicesAreOtherIPv4DevicesOrRanges(t *testing.T) {
	for _, tc := range []struct{ address, want string }{
		{"192.0.2.10", "192.0.2.10/32"},
		{" 192.0.2.10 ", "192.0.2.10/32"},
		{"192.0.2.0/24", "192.0.2.0/24"},
		{"192.0.2.77/24", "192.0.2.0/24"},
		{"100.64.0.9", "100.64.0.9/32"},
		{"10.0.0.0/8", "10.0.0.0/8"},
	} {
		prefix, err := (Device{Address: tc.address}).Prefix()
		if err != nil || prefix.String() != tc.want {
			t.Errorf("Device(%q).Prefix() = %v, %v; want %s", tc.address, prefix, err, tc.want)
		}
	}
	for _, address := range []string{"", "studio.example.com", "192.0.2.10:1080", "127.0.0.1", "127.0.0.0/8", "0.0.0.0", "0.0.0.0/0", "192.0.2.0/7", "2001:db8::1", "2001:db8::/32", "::ffff:192.0.2.10", "192.0.2.10/33"} {
		if prefix, err := (Device{Address: address}).Prefix(); err == nil || Code(err) != ErrInvalidConfig {
			t.Errorf("Device(%q).Prefix() = %v, want an error", address, prefix)
		}
	}
	cfg := DefaultConfig()
	cfg.Devices = []Device{{Address: "192.0.2.10", Name: "Laptop"}, {Address: "198.51.100.0/24"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Devices = append(cfg.Devices, Device{Address: "198.51.100.9/24", Name: "the same range"})
	if Code(cfg.Validate()) != ErrInvalidConfig {
		t.Fatal("a range listed twice was accepted")
	}
	cfg.Devices = []Device{{Address: "2001:db8::1"}}
	if Code(cfg.Validate()) != ErrInvalidConfig {
		t.Fatal("an IPv6 device was accepted though shared proxies listen on IPv4")
	}
	cfg.Devices = nil
	for i := range MaxDevices + 1 {
		cfg.Devices = append(cfg.Devices, Device{Address: netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}).String()})
	}
	if Code(cfg.Validate()) != ErrInvalidConfig {
		t.Fatal("oversized device list was accepted")
	}
	cfg.Devices = cfg.Devices[:MaxDevices]
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(DefaultConfig())
	if err != nil || strings.Contains(string(before), `"devices"`) {
		t.Fatal("empty device list changed legacy JSON", string(before), err)
	}
}
