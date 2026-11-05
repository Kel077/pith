// Package captureprefix defines the full-capture prefixes shared by production
// planning and immutable helper test profiles. It has no system side effects.
package captureprefix

import "net/netip"

// Includes reports whether an address is inside the full dual-stack scope.
func Includes(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsValid() && address.AsSlice()[0] != 0
}

// Full covers nonzero first octets in IPv4 and, optionally, IPv6. Darwin's bound
// socket fallback looks up the zero address without an explicit /0 mask. A
// capture route containing zero would hide the original unscoped default there.
// Leaving 0.0.0.0/8 and ::/8 to the system avoids adding physical egress routes
// that could outlive the utun. See docs/ARCHITECTURE.md for the XNU references.
func Full(ipv6 bool) []netip.Prefix {
	result := make([]netip.Prefix, 0, 16)
	for bits := 8; bits >= 1; bits-- {
		result = append(result, netip.PrefixFrom(netip.AddrFrom4([4]byte{1 << (8 - bits)}), bits))
	}
	if ipv6 {
		for bits := 8; bits >= 1; bits-- {
			result = append(result, netip.PrefixFrom(netip.AddrFrom16([16]byte{1 << (8 - bits)}), bits))
		}
	}
	return result
}
