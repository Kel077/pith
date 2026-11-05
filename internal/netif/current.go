package netif

import (
	"net"
	"net/netip"

	"github.com/Kel077/pith/internal/api"
)

// Current refreshes cheap kernel identities for dialing, without executing
// SystemConfiguration queries per socket. DNS/service metadata comes from Watch.
func Current(metadata []api.Interface) ([]api.Interface, error) {
	links, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	return currentLinks(metadata, links, func(link net.Interface) ([]net.Addr, error) { return link.Addrs() }), nil
}

func currentLinks(metadata []api.Interface, links []net.Interface, addrs func(net.Interface) ([]net.Addr, error)) []api.Interface {
	var result []api.Interface
	for _, link := range links {
		if link.Flags&net.FlagLoopback != 0 {
			continue
		}
		iface := api.Interface{Name: link.Name, HardwareAddr: link.HardwareAddr.String()}
		if old := (api.InterfaceRef{Name: iface.Name, HardwareAddr: iface.HardwareAddr}).Resolve(metadata); old != nil {
			iface = *old
		}
		iface.Name, iface.HardwareAddr = link.Name, link.HardwareAddr.String()
		iface.IPv4, iface.IPv6 = nil, nil
		addresses, err := addrs(link)
		if err != nil {
			continue
		}
		for _, raw := range addresses {
			p, err := netip.ParsePrefix(raw.String())
			if err != nil || !p.Addr().IsGlobalUnicast() {
				continue
			}
			if p.Addr().Is4() {
				iface.IPv4 = append(iface.IPv4, p.Addr().String())
			} else {
				iface.IPv6 = append(iface.IPv6, p.Addr().String())
			}
		}
		iface.Up = link.Flags&net.FlagUp != 0 && link.Flags&net.FlagRunning != 0 && len(iface.IPv4)+len(iface.IPv6) > 0
		result = append(result, iface)
	}
	return result
}
