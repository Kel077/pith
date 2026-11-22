package ui

import (
	"testing"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/locales"
)

// A renewal is told by the addresses of one family: IPv4, or IPv6 when the
// hotspot has no IPv4 after it.
func TestRenewalText(t *testing.T) {
	both := func(v4, v6 string) api.Probe { return api.Probe{OK: true, PublicIPv4: v4, PublicIPv6: v6} }
	for _, tt := range []struct {
		before, after api.Probe
		want          string
	}{
		{both("198.51.100.1", "2001:db8:1::1"), both("198.51.100.2", "2001:db8:2::1"), "198.51.100.1 → 198.51.100.2"},
		{both("198.51.100.1", "2001:db8:1::1"), both("", "2001:db8:2::1"), "2001:db8:1::1 → 2001:db8:2::1"},
		{both("", "2001:db8:1::1"), both("198.51.100.2", "2001:db8:2::1"), "198.51.100.2"},
		{both("198.51.100.1", ""), both("198.51.100.1", ""), locales.IpUnchanged("198.51.100.1")},
		{both("", "2001:db8:1::1"), both("", "2001:db8:1::1"), locales.IpUnchanged("2001:db8:1::1")},
		// Without its IPv4 after, the same IPv6 doesn't make it unchanged.
		{both("198.51.100.1", "2001:db8:1::1"), both("", "2001:db8:1::1"), "2001:db8:1::1"},
		{both("198.51.100.1", ""), api.Probe{}, ""},
	} {
		if got := renewalText(tt.before, tt.after); got != tt.want {
			t.Errorf("%+v → %+v: %q, want %q", tt.before, tt.after, got, tt.want)
		}
	}
}

func TestRMNetBands(t *testing.T) {
	c := api.HotspotCell{Carriers: []api.HotspotCarrier{
		{Role: "PCC", RAT: "LTE", Band: "B1", BandwidthMHz: 10},
		{Role: "SCC", RAT: "NR5G", Band: "n78", BandwidthMHz: 80},
	}}
	if bands, ca := rmnetBands(&c); ca || bands != "B1 (10 MHz) + n78 (80 MHz)" {
		t.Fatal("dual connectivity", bands, ca)
	}
	c.Carriers = append(c.Carriers, api.HotspotCarrier{Role: "SCC", RAT: "NR5G", Band: "n78"})
	if bands, ca := rmnetBands(&c); !ca || bands != "B1 (10 MHz) + n78 (80 MHz) + n78" {
		t.Fatal("aggregation", bands, ca)
	}
}
