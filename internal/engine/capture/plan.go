// Package capture plans and owns a reversible set of routes into one utun.
package capture

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/captureprefix"
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
	Addresses  TunnelAddresses `json:"addresses"`
	Prefixes   []netip.Prefix  `json:"prefixes"`
	Before     []Route         `json:"before"`
	Warnings   []api.Warning   `json:"warnings,omitempty"`
	SkippedDNS []DNSExclusion  `json:"skippedDNS,omitempty"`
	Scoped     bool            `json:"scoped"`
}

// DNSExclusion explains why an optional DNS host route was not added. Routes
// are existing forwarding policy, never routes owned or removed by Pith.
type DNSExclusion struct {
	Address netip.Addr `json:"address"`
	Reason  string     `json:"reason"`
	Routes  []Route    `json:"routes"`
}

// SystemInterface preserves the effective pre-capture route, including a VPN
// default and a LAN route used by a captured DNS server. Scoped routes are only
// candidates for explicitly bound sockets and must not replace this lookup.
func SystemInterface(routes []Route, address netip.Addr) string {
	best := -1
	name := ""
	var chosen Route
	ambiguous := false
	address = address.WithZone("").Unmap()
	for _, r := range routes {
		if r.Scoped || !r.Prefix.Contains(address) {
			continue
		}
		if r.Prefix.Bits() > best {
			best = r.Prefix.Bits()
			name = r.Interface
			chosen, ambiguous = r, false
			// A more specific rejected/local route must not become permission to
			// send an unmatched connection through the broader default route.
			if r.Reject || r.Local {
				name = ""
			}
		} else if r.Prefix.Bits() == best && r != chosen {
			ambiguous = true
		}
	}
	if ambiguous {
		return ""
	}
	return name
}

// Build is read-only. A scoped plan captures only the supplied prefixes and
// never adds global DNS host routes. Exact pre-existing configured routes conflict
// with required capture prefixes. Optional DNS host capture is skipped instead;
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
		p.Prefixes = captureprefix.Full(ipv6)
		seenDNS := make(map[netip.Addr]bool)
		for _, addr := range dns {
			addr = addr.Unmap().WithZone("")
			if !captureprefix.Includes(addr) || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.Is6() && !ipv6 || seenDNS[addr] {
				continue
			}
			seenDNS[addr] = true
			host := netip.PrefixFrom(addr, addr.BitLen())
			var existing []Route
			for _, r := range routes {
				if r.Prefix == host {
					existing = append(existing, r)
				}
			}
			if len(existing) > 0 {
				// macOS can install a configured host route for a LAN gateway that
				// also provides DNS. Preserve every existing route, including scoped
				// copies, rather than replacing one merely to observe DNS answers.
				p.SkippedDNS = append(p.SkippedDNS, DNSExclusion{
					Address: addr, Routes: existing,
					Reason: "An existing configured host route is preserved. DNS queries using that route are not observed; domain rules rely on other observed names, such as TLS SNI, HTTP Host or QUIC SNI when sniffing is enabled.",
				})
				p.Warnings = append(p.Warnings, api.Warning{Code: api.WarnDNSUnobserved, Detail: addr.String()})
				continue
			}
			iface := SystemInterface(routes, addr)
			// MagicDNS and DNS inside another tunnel must retain their own routes.
			if tunnel(iface) || netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
				continue
			}
			for _, r := range routes {
				if !r.Scoped && !r.Local && !r.Reject && !r.Gateway.IsValid() && r.Interface == iface && r.Prefix.Bits() > 1 && r.Prefix.Contains(addr) {
					p.Prefixes = append(p.Prefixes, host)
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
