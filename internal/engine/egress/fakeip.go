package egress

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/netif"
)

// The benchmark block includes the common 198.18.0.0/16 fake-IP pool. Treating
// this fixed block as synthetic is deliberately conservative: a tunnel route
// alone does not distinguish another proxy's custom pool from a real private
// network. In particular, Tailscale's 100.64.0.0/10 and arbitrary IPv6 ULA routes
// are not evidence of a fake-IP mapping.
var fakeIPv4 = netip.MustParsePrefix("198.18.0.0/15")

func fakeIP(address netip.Addr) bool { return fakeIPv4.Contains(address.Unmap()) }

func domainName(raw string) string {
	target, err := api.ParseTarget(raw)
	if err != nil || !target.IsDomain() || target.Subdomains {
		return ""
	}
	return target.Domain
}

func vpnInterface(iface api.Interface) bool {
	return iface.Kind == api.KindVPN || strings.HasPrefix(iface.Name, "utun") || strings.HasPrefix(iface.Name, "ppp") || strings.HasPrefix(iface.Name, "ipsec")
}

func resolveDomain(ctx context.Context, iface api.Interface, domain string) ([]netip.Addr, error) {
	// An empty list must not fall through to /etc/resolv.conf: a different VPN
	// may own that resolver. Do not send queries to its fake/loopback DNS either.
	servers := physicalDNS(iface)
	if len(servers) == 0 {
		return nil, errors.New("selected interface has no usable physical DNS servers")
	}
	dialer, err := netif.Dialer(iface.Name, 0, servers)
	if err != nil {
		return nil, err
	}
	if dialer.Resolver == nil {
		return nil, errors.New("refusing unbound fake-IP DNS resolution")
	}
	// Observed wire names are absolute; do not append another VPN's search list.
	return dialer.Resolver.LookupNetIP(ctx, "ip", domain+".")
}

func physicalDNS(iface api.Interface) []string {
	var servers []string
	seen := make(map[netip.Addr]bool)
	for _, raw := range iface.DNS {
		addr, err := netip.ParseAddr(raw)
		addr = addr.Unmap()
		if err != nil || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || fakeIP(addr) {
			continue
		}
		if addr.Is6() && addr.IsLinkLocalUnicast() {
			if addr.Zone() != "" && addr.Zone() != iface.Name {
				continue
			}
			addr = addr.WithZone(iface.Name)
		} else if addr.Zone() != "" {
			continue
		}
		if !seen[addr] {
			servers = append(servers, addr.String())
			seen[addr] = true
		}
	}
	return servers
}

func realTargets(addresses []netip.Addr, port uint16) []netip.AddrPort {
	var targets []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, addr := range addresses {
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.Zone() != "" || fakeIP(addr) || seen[addr] {
			continue
		}
		seen[addr] = true
		targets = append(targets, netip.AddrPortFrom(addr, port))
		if len(targets) == 32 {
			break
		}
	}
	return targets
}
