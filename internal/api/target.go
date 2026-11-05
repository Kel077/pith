package api

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// Target is a parsed rule target: a domain, or a range of IP addresses.
type Target struct {
	// Domain is the host name of a domain target, in lower case without a
	// trailing dot; empty for an address target.
	Domain string
	// Subdomains: every subdomain of Domain matches too.
	Subdomains bool
	// Prefix holds the addresses of an address target: a single address is
	// a /32 or a /128.
	Prefix netip.Prefix
}

// ErrBadTarget is the error of a target ParseTarget does not understand.
var ErrBadTarget = errors.New("not a domain, an IP address or a CIDR range")

// ParseTarget parses a rule target:
//
//	example.com     the host example.com alone
//	*.example.com   example.com and every subdomain of it (also .example.com)
//	192.168.50.10   one IPv4 address
//	2001:db8::1     one IPv6 address
//	10.0.0.0/8      a CIDR range, as fd00::/8
//
// It accepts what users paste: spaces around it, upper case, a trailing dot,
// a port, and a URL, whose host counts. Domains are ASCII: an
// internationalized one is written in Punycode (xn--).
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, ErrBadTarget
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		return Target{Prefix: p.Masked()}, nil
	}
	if a, ok := parseAddr(s); ok {
		return Target{Prefix: netip.PrefixFrom(a, a.BitLen())}, nil
	}
	host := s
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Hostname() == "" {
			return Target{}, ErrBadTarget
		}
		host = u.Hostname()
	} else {
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			if _, ok := parseAddr(host[:i]); ok && host[i] == '/' {
				return Target{}, ErrBadTarget // a CIDR range ParsePrefix refused
			}
			host = host[:i]
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	if a, ok := parseAddr(host); ok {
		return Target{Prefix: netip.PrefixFrom(a, a.BitLen())}, nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	t := Target{}
	switch {
	case strings.HasPrefix(host, "*."):
		t.Subdomains, host = true, host[2:]
	case strings.HasPrefix(host, "."):
		t.Subdomains, host = true, host[1:]
	}
	if !validDomain(host) {
		return Target{}, ErrBadTarget
	}
	t.Domain = host
	return t, nil
}

// parseAddr parses an IP address, unmapping IPv4-mapped IPv6 addresses and
// dropping an IPv6 zone.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

func validDomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// IsDomain tells a domain target from an address target.
func (t Target) IsDomain() bool { return t.Domain != "" }

// String returns the target in the canonical form of ParseTarget.
func (t Target) String() string {
	switch {
	case t.Domain != "" && t.Subdomains:
		return "*." + t.Domain
	case t.Domain != "":
		return t.Domain
	case t.Prefix.IsValid() && t.Prefix.IsSingleIP():
		return t.Prefix.Addr().String()
	case t.Prefix.IsValid():
		return t.Prefix.String()
	}
	return ""
}

// MatchHost reports whether the host name host is the target's domain, or
// a subdomain of it when the target includes subdomains. Host is compared
// without case and without a trailing dot.
func (t Target) MatchHost(host string) bool {
	if t.Domain == "" || host == "" {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == t.Domain {
		return true
	}
	return t.Subdomains && strings.HasSuffix(host, "."+t.Domain)
}

// MatchAddr reports whether an address target holds a.
func (t Target) MatchAddr(a netip.Addr) bool {
	return t.Domain == "" && t.Prefix.IsValid() && t.Prefix.Contains(a.WithZone("").Unmap())
}
