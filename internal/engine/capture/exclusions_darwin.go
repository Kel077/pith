//go:build darwin

package capture

import (
	"context"
	"net/netip"

	"github.com/Kel077/pith/internal/rib"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// AddressExclusions reads kernel cache destinations separately from Snapshot.
// These endpoints may occupy a candidate tunnel subnet even when the only
// configured parent route is a default or /1. They must never enter the saved
// policy snapshot or be used as evidence that a route belongs to Pith.
func (nativeDriver) AddressExclusions() ([]netip.Prefix, error) {
	data, err := rib.Read(context.Background())
	if err != nil {
		return nil, err
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, data)
	if err != nil {
		return nil, err
	}
	return decodeAddressExclusions(messages), nil
}

func decodeAddressExclusions(messages []route.Message) []netip.Prefix {
	var result []netip.Prefix
	seen := make(map[netip.Addr]bool)
	for _, message := range messages {
		r, ok := message.(*route.RouteMessage)
		if !ok || r.Flags&unix.RTF_UP == 0 || !transientRoute(r.Flags) || len(r.Addrs) <= unix.RTAX_DST {
			continue
		}
		dst := address(r.Addrs[unix.RTAX_DST]).Unmap()
		if !dst.IsValid() || dst.IsUnspecified() || seen[dst] {
			continue
		}
		seen[dst] = true
		// A cache entry excludes only its observed endpoint, never its netmask
		// or its parent's subnet, including for scoped neighbor entries.
		result = append(result, netip.PrefixFrom(dst, dst.BitLen()))
	}
	return result
}
