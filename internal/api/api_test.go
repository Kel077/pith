package api

import (
	"net/netip"
	"testing"
)

func TestParseTarget(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"example.com", "example.com"},
		{"  Example.COM. ", "example.com"},
		{"*.example.com", "*.example.com"},
		{".example.com", "*.example.com"},
		{"https://www.example.com/path?q=1", "www.example.com"},
		{"example.com:443", "example.com"},
		{"example.com/path", "example.com"},
		{"xn--fiqs8s.cn", "xn--fiqs8s.cn"},
		{"nas", "nas"},
		{"192.168.50.10", "192.168.50.10"},
		{"10.1.2.3/8", "10.0.0.0/8"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"http://[2001:db8::1]:8080/", "2001:db8::1"},
		{"fd00::/8", "fd00::/8"},
		{"::ffff:1.2.3.4", "1.2.3.4"},
		{"fe80::1%en0", "fe80::1"},
	} {
		got, err := ParseTarget(tt.in)
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tt.in, err)
			continue
		}
		if got.String() != tt.want {
			t.Errorf("ParseTarget(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	for _, in := range []string{"", "  ", "exa mple.com", "-bad.com", "a..b", "\u4e2d\u56fd.cn", "*.", "1.2.3.4/33", "http://"} {
		if got, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %q, want an error", in, got)
		}
	}
}

func TestTargetMatch(t *testing.T) {
	exact, _ := ParseTarget("example.com")
	sub, _ := ParseTarget("*.example.com")
	for _, tt := range []struct {
		t    Target
		host string
		want bool
	}{
		{exact, "example.com", true},
		{exact, "EXAMPLE.com.", true},
		{exact, "www.example.com", false},
		{sub, "example.com", true},
		{sub, "a.b.example.com", true},
		{sub, "badexample.com", false},
		{sub, "", false},
	} {
		if got := tt.t.MatchHost(tt.host); got != tt.want {
			t.Errorf("%v.MatchHost(%q) = %v, want %v", tt.t, tt.host, got, tt.want)
		}
	}
	lan, _ := ParseTarget("192.168.0.0/16")
	if !lan.MatchAddr(netip.MustParseAddr("192.168.50.10")) || !lan.MatchAddr(netip.MustParseAddr("::ffff:192.168.1.1")) {
		t.Error("192.168.0.0/16 does not match its addresses")
	}
	if lan.MatchAddr(netip.MustParseAddr("10.0.0.1")) || exact.MatchAddr(netip.MustParseAddr("192.168.1.1")) {
		t.Error("a target matches an address outside it")
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	c := DefaultConfig()
	c.Scenes[0].Rules = []Rule{{
		ID: "r1", Enabled: true, Protocol: AnyProtocol, Action: Route,
		Targets: []string{"example.com", "EXAMPLE.com", "nope nope"},
	}}
	ps := c.Scenes[0].Rules[0].Problems()
	want := []Problem{
		{"targets", 1, ProblemDuplicate},
		{"targets", 2, ProblemBadTarget},
		{"via", -1, ProblemNoVia},
	}
	if len(ps) != len(want) {
		t.Fatalf("Problems() = %v, want %v", ps, want)
	}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("Problems()[%d] = %v, want %v", i, ps[i], want[i])
		}
	}
	if Code(c.Validate()) != ErrInvalidConfig {
		t.Errorf("Validate() of an invalid rule: %v", c.Validate())
	}
}

func TestResolve(t *testing.T) {
	ifs := []Interface{
		{Name: "bridge0", HardwareAddr: "02:00:00:00:00:0b"},
		{Name: "en2", HardwareAddr: "02:00:00:00:00:0b"},
		{Name: "en3", HardwareAddr: "02:00:00:00:00:0b"},
		{Name: "en8", HardwareAddr: "02:00:00:00:00:08"}, // a USB adapter, once en7
		{Name: "utun4"},
	}
	for _, tt := range []struct {
		ref  InterfaceRef
		want string
	}{
		{InterfaceRef{Name: "en3", HardwareAddr: "02:00:00:00:00:0b"}, "en3"},
		{InterfaceRef{Name: "en7", HardwareAddr: "02:00:00:00:00:08"}, "en8"},
		{InterfaceRef{Name: "en4", HardwareAddr: "02:00:00:00:00:0b"}, ""}, // shared address, gone name: not a guess
		{InterfaceRef{Name: "en8", HardwareAddr: "02:00:00:00:00:99"}, ""}, // another adapter took the name
		{InterfaceRef{Name: "utun4"}, "utun4"},
	} {
		got := ""
		if i := tt.ref.Resolve(ifs); i != nil {
			got = i.Name
		}
		if got != tt.want {
			t.Errorf("%+v.Resolve = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestParseFile(t *testing.T) {
	if _, err := ParseFile([]byte(`{"pith":"rules","version":1,"rules":[]}`)); err != nil {
		t.Errorf("rules file: %v", err)
	}
	for _, in := range []string{`{}`, `{"pith":"rules","version":99}`, `{"pith":"backup","version":1}`, `not json`} {
		if _, err := ParseFile([]byte(in)); Code(err) != ErrInvalidConfig {
			t.Errorf("ParseFile(%s) = %v, want ErrInvalidConfig", in, err)
		}
	}
}
