//go:build darwin

package capture

import (
	"testing"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

func TestDarwinRouteDecoding(t *testing.T) {
	messages := []route.Message{
		&route.RouteMessage{Flags: unix.RTF_UP, Index: 1, Addrs: []route.Addr{unix.RTAX_DST: &route.Inet4Addr{}, unix.RTAX_GATEWAY: &route.Inet4Addr{IP: [4]byte{192, 0, 2, 1}}, unix.RTAX_NETMASK: &route.Inet4Addr{}}},
		&route.RouteMessage{Flags: unix.RTF_UP | unix.RTF_IFSCOPE, Index: 2, Addrs: []route.Addr{unix.RTAX_DST: &route.Inet4Addr{IP: [4]byte{192, 0, 2, 0}}, unix.RTAX_NETMASK: &route.Inet4Addr{IP: [4]byte{255, 255, 255, 0}}}},
	}
	routes := decodeRoutes(messages, map[int]string{1: "en0", 2: "en5"})
	if len(routes) != 2 || routes[0].Prefix != prefix("0.0.0.0/0") || routes[0].Gateway != addr("192.0.2.1") || routes[1].Prefix != prefix("192.0.2.0/24") || !routes[1].Scoped {
		t.Fatal(routes)
	}
}

func TestLiveSnapshotIsReadOnly(t *testing.T) {
	driver := nativeDriver{}
	boot, err := driver.BootSession()
	if err != nil || boot == "" {
		t.Fatal("cannot read the current boot session", err)
	}
	again, err := driver.BootSession()
	if err != nil || again != boot {
		t.Fatal("boot session identity is unstable", err)
	}
	routes, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) == 0 {
		t.Fatal("routing snapshot is empty")
	}
	for _, r := range routes {
		if !r.Prefix.IsValid() {
			t.Fatal("invalid route prefix")
		}
	}
	// Do not print this Mac's addresses, gateways or network inventory.
}
