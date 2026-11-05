package netif

import (
	"net"
	"testing"

	"github.com/Kel077/pith/internal/api"
)

func TestCurrentLinksRetainsDNSButRefreshesIdentityAndAvailability(t *testing.T) {
	mac, _ := net.ParseMAC("02:00:00:00:00:05")
	metadata := []api.Interface{{Name: "en5", HardwareAddr: mac.String(), DNS: []string{"192.0.2.53"}, IPv6: []string{"2001:db8:1::1"}, Up: true}}
	links := []net.Interface{{Name: "en6", Index: 6, HardwareAddr: mac, Flags: net.FlagUp | net.FlagRunning}}
	addrs := func(net.Interface) ([]net.Addr, error) {
		_, p, _ := net.ParseCIDR("2001:db8:2::/64")
		return []net.Addr{p}, nil
	}
	got := currentLinks(metadata, links, addrs)
	if len(got) != 1 || got[0].Name != "en6" || !got[0].Up || got[0].IPv6[0] != "2001:db8:2::" || got[0].DNS[0] != "192.0.2.53" {
		t.Fatal(got)
	}
	links[0].Flags = 0
	got = currentLinks(metadata, links, addrs)
	if got[0].Up {
		t.Fatal("down link retained stale availability")
	}
	if metadata[0].IPv6[0] != "2001:db8:1::1" {
		t.Fatal("mutated metadata")
	}
}
