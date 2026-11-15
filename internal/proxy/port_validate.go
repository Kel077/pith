package proxy

import (
	"net/netip"
	"unicode/utf8"

	"github.com/Kel077/pith/internal/api"
)

const maxUpstreamInterface = 256 // bytes, without cutting a UTF-8 character

// validateUpstreamInfo bounds the tree and checks identity fields before it
// can be cached or sent on. Display names and addresses are sanitized in place.
func validateUpstreamInfo(info *api.ProxyInfo, depth int) bool {
	if info == nil {
		return true
	}
	if depth >= api.MaxProxyHops || info.Pith < api.ProxyInfoVersion || info.Port < 1 || info.Port > 65535 {
		return false
	}
	switch info.Exit {
	case api.ExitInterface, api.ExitUpstream, api.ExitScene:
	default:
		return false
	}
	if len(info.Interface) > maxUpstreamInterface {
		info.Interface = info.Interface[:maxUpstreamInterface]
		for !utf8.ValidString(info.Interface) {
			info.Interface = info.Interface[:len(info.Interface)-1]
		}
	}
	if info.Exit != api.ExitInterface {
		info.Interface = ""
	}
	info.PublicIPv4 = upstreamIP(info.PublicIPv4, true)
	info.PublicIPv6 = upstreamIP(info.PublicIPv6, false)
	info.At = info.At.UTC()
	return validateUpstreamInfo(info.Upstream, depth+1)
}

func validateUpstreamChange(changed *api.ProxyIPChange) bool {
	if changed.Port < 1 || changed.Port > 65535 {
		return false
	}
	changed.BeforeIPv4 = upstreamIP(changed.BeforeIPv4, true)
	changed.BeforeIPv6 = upstreamIP(changed.BeforeIPv6, false)
	changed.PublicIPv4 = upstreamIP(changed.PublicIPv4, true)
	changed.PublicIPv6 = upstreamIP(changed.PublicIPv6, false)
	changed.At = changed.At.UTC()
	return true
}

func upstreamIP(text string, ipv4 bool) string {
	addr, err := netip.ParseAddr(text)
	if err != nil || addr.Zone() != "" {
		return ""
	}
	addr = addr.Unmap()
	if addr.Is4() != ipv4 {
		return ""
	}
	return addr.String()
}
