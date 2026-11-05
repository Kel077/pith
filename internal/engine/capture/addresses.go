package capture

import (
	"net/netip"

	"github.com/Kel077/pith/internal/api"
)

// TunnelAddresses reserves small point-to-point subnets before creating a utun.
// IPv6 is invalid when IPv6 capture is disabled. Neither family uses a fake-IP
// pool, the shared address space used by Tailscale, or a common home LAN subnet.
type TunnelAddresses struct {
	IPv4 netip.Prefix `json:"ipv4"`
	IPv6 netip.Prefix `json:"ipv6,omitempty"`
}

var ipv4Candidates = [...]netip.Prefix{
	netip.MustParsePrefix("172.31.255.252/30"),
	netip.MustParsePrefix("10.255.255.252/30"),
	netip.MustParsePrefix("172.30.255.252/30"),
	netip.MustParsePrefix("10.254.255.252/30"),
}

var ipv6Candidates = [...]netip.Prefix{
	netip.MustParsePrefix("fd64:7069:7468:ffff::/126"),
	netip.MustParsePrefix("fd68:7069:7468:ffff::/126"),
}

// SelectAddresses is read-only. Specific, scoped, rejecting and local routes
// all reserve their address space. Default and /1 routes do not identify an
// occupied subnet. Assigned interface prefixes, scoped capture destinations and
// observed transient endpoints are checked independently, without treating those
// address exclusions as forwarding routes. Uncached remote addresses behind a
// default or /1 route cannot be inferred from these observations.
func SelectAddresses(routes []Route, assigned []netip.Prefix, ipv6 bool) (TunnelAddresses, error) {
	free := func(pool netip.Prefix) bool {
		for _, r := range routes {
			if (r.Local || r.Prefix.Bits() > 1) && pool.Overlaps(r.Prefix) {
				return false
			}
		}
		for _, p := range assigned {
			if pool.Overlaps(p) {
				return false
			}
		}
		return true
	}
	var selected TunnelAddresses
	for _, pool := range ipv4Candidates {
		if free(pool) {
			selected.IPv4 = pool
			break
		}
	}
	if !selected.IPv4.IsValid() {
		return TunnelAddresses{}, &api.Error{Code: api.ErrRouteConflict, Detail: "no unused IPv4 tunnel subnet"}
	}
	if ipv6 {
		for _, pool := range ipv6Candidates {
			if free(pool) {
				selected.IPv6 = pool
				break
			}
		}
		if !selected.IPv6.IsValid() {
			return TunnelAddresses{}, &api.Error{Code: api.ErrRouteConflict, Detail: "no unused IPv6 tunnel subnet"}
		}
	}
	return selected, nil
}
