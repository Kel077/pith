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
