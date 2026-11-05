// Package capture plans and owns a reversible set of routes into one utun.
package capture

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/Kel077/pith/internal/api"
)

type Route struct {
	Prefix    netip.Prefix `json:"prefix"`
	Interface string       `json:"interface"`
	Index     int          `json:"index"`
	Gateway   netip.Addr   `json:"gateway,omitempty"`
	Scoped    bool         `json:"scoped"`
	Local     bool         `json:"local"`
	Reject    bool         `json:"reject"`
}

type Plan struct {
	Addresses TunnelAddresses `json:"addresses"`
	Prefixes  []netip.Prefix  `json:"prefixes"`
	Before    []Route         `json:"before"`
	Warnings  []api.Warning   `json:"warnings,omitempty"`
	Scoped    bool            `json:"scoped"`
}

// SystemInterface preserves the effective pre-capture route, including a VPN
// default and a LAN route used by a captured DNS server. Scoped routes are only
// candidates for explicitly bound sockets and must not replace this lookup.
func SystemInterface(routes []Route, address netip.Addr) string {
	best := -1
	name := ""
	address = address.WithZone("").Unmap()
	for _, r := range routes {
		if r.Scoped || !r.Prefix.Contains(address) {
			continue
		}
		if r.Prefix.Bits() > best {
			best = r.Prefix.Bits()
			name = r.Interface
			// A more specific rejected/local route must not become permission to
			// send an unmatched connection through the broader default route.
			if r.Reject || r.Local {
				name = ""
			}
		}
	}
	return name
}

// Build is read-only. A scoped plan captures only the supplied prefixes and
// never adds global DNS host routes. Exact pre-existing routes are conflicts;
// more specific routes (including Tailscale's routes) remain in place.
func Build(routes []Route, dns []netip.Addr, ipv6 bool, scopes []netip.Prefix) (Plan, error) {
	p := Plan{Before: slices.Clone(routes), Scoped: len(scopes) > 0}
	if p.Scoped {
		for _, prefix := range scopes {
			if !prefix.IsValid() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().Is6() && !ipv6 {
				return Plan{}, fmt.Errorf("invalid scoped capture prefix %q", prefix)
			}
			p.Prefixes = append(p.Prefixes, prefix.Masked())
		}
	} else {
		p.Prefixes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}
		if ipv6 {
			p.Prefixes = append(p.Prefixes, netip.MustParsePrefix("::/1"), netip.MustParsePrefix("8000::/1"))
		}
		for _, addr := range dns {
			addr = addr.Unmap().WithZone("")
			if !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.Is6() && !ipv6 {
				continue
			}
			iface := SystemInterface(routes, addr)
			// MagicDNS and DNS inside another tunnel must retain their own routes.
			if tunnel(iface) || netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
				continue
			}
			for _, r := range routes {
				if !r.Scoped && !r.Local && !r.Reject && !r.Gateway.IsValid() && r.Interface == iface && r.Prefix.Bits() > 1 && r.Prefix.Contains(addr) {
					p.Prefixes = append(p.Prefixes, netip.PrefixFrom(addr, addr.BitLen()))
					break
				}
			}
		}
	}
	unique := make(map[netip.Prefix]bool)
	p.Prefixes = slices.DeleteFunc(p.Prefixes, func(prefix netip.Prefix) bool {
		if unique[prefix] {
			return true
		}
		unique[prefix] = true
		return false
	})
	for _, prefix := range p.Prefixes {
		for _, r := range routes {
			if !r.Scoped && r.Prefix == prefix {
				return Plan{}, &api.Error{Code: api.ErrRouteConflict, Detail: prefix.String() + " via " + r.Interface}
			}
		}
	}
	warned := make(map[string]bool)
	for _, r := range routes {
		if !r.Scoped && r.Prefix.Bits() == 0 && tunnel(r.Interface) && !warned[r.Interface] {
			p.Warnings = append(p.Warnings, api.Warning{Code: api.WarnVPN, Detail: r.Interface})
			warned[r.Interface] = true
		}
	}
	return p, nil
}

func tunnel(name string) bool {
	return strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "ppp") || strings.HasPrefix(name, "ipsec")
}
