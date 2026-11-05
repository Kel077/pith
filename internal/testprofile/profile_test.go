package testprofile

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestFullProfileRejectsUnboundedOrAmbiguousCapture(t *testing.T) {
	valid := Profile{Mode: FullIPv4, Lease: 120 * time.Second, Target: "192.0.2.20:443", ServerName: "example.com", DNSHosts: []netip.Addr{netip.MustParseAddr("192.0.2.53")}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Profile){
		func(p *Profile) { p.Mode = "" },
		func(p *Profile) { p.Mode = "full-ipv6" },
		func(p *Profile) { p.Lease = 0 },
		func(p *Profile) { p.Lease = -time.Second },
		func(p *Profile) { p.Lease = 121 * time.Second },
		func(p *Profile) { p.Target = "example.com:443" },
		func(p *Profile) { p.Target = "[2001:db8::1]:443" },
		func(p *Profile) { p.Target = "100.64.0.1:443" },
		func(p *Profile) { p.ServerName = "*.example.com" },
		func(p *Profile) { p.DNSHosts = []netip.Addr{netip.MustParseAddr("127.0.0.1")} },
		func(p *Profile) { p.DNSHosts = append(slices.Clone(p.DNSHosts), p.DNSHosts[0]) },
	} {
		p := valid
		change(&p)
		if _, err := p.Arguments(); err == nil {
			t.Fatal("invalid profile generated installation arguments")
		}
	}
	settings := api.DefaultSettings()
	settings.SafeStart, settings.IPv6 = true, false
	if err := valid.ValidateSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings.IPv6 = true
	if valid.ValidateSettings(settings) == nil {
		t.Fatal("IPv6 expanded the test scope")
	}
	settings.IPv6, settings.SafeStart = false, false
	if valid.ValidateSettings(settings) == nil {
		t.Fatal("SafeStart was bypassed")
	}
}

func TestIPv6ProfileCannotCaptureIPv4OrExpandItsScope(t *testing.T) {
	p := Profile{Mode: HostIPv6, Lease: 60 * time.Second, Target: "[2001:db8::20]:443", ServerName: "example.com"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Prefixes(), []netip.Prefix{netip.MustParsePrefix("2001:db8::20/128")}) {
		t.Fatal("incorrect IPv6 capture scope")
	}
	for _, target := range []string{"192.0.2.20:443", "[::ffff:192.0.2.20]:443", "[2001:db8::20%example0]:443", "[::1]:443", "[fe80::1]:443", "[fd00::1]:443", "[ff02::1]:443", "[::]:443", "[2001:db8::20]:0", "example.com:443"} {
		next := p
		next.Target = target
		if next.Validate() == nil {
			t.Fatal("accepted wrong family, zone or non-public target", target)
		}
	}
	normalized := p
	normalized.Target = "[2001:0DB8:0000:0000:0000:0000:0000:0020]:0443"
	if normalized.Validate() == nil {
		t.Fatal("non-canonical frozen endpoint would differ from connection logs")
	}
	normalized.Target = netip.MustParseAddrPort(normalized.Target).String()
	if err := normalized.Validate(); err != nil || normalized.Target != p.Target {
		t.Fatal("equivalent IPv6 input did not normalize to the reviewed endpoint", err)
	}
	for _, lease := range []time.Duration{0, -time.Second, 61 * time.Second, 120 * time.Second} {
		next := p
		next.Lease = lease
		if next.Validate() == nil {
			t.Fatal("invalid IPv6 lease accepted")
		}
	}
	next := p
	next.DNSHosts = []netip.Addr{netip.MustParseAddr("192.0.2.53")}
	if next.Validate() == nil {
		t.Fatal("IPv6 host capture expanded to DNS")
	}
	settings := api.DefaultSettings()
	settings.SafeStart, settings.IPv6 = true, true
	if err := p.ValidateSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings.IPv6 = false
	if p.ValidateSettings(settings) == nil {
		t.Fatal("IPv4-only SafeStart accepted for IPv6 capture")
	}
}
