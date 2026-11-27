//go:build darwin

package client

import (
	"context"
	"net"
	"net/netip"

	"github.com/Kel077/pith/internal/rib"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// gatewayHardwareAddr only reads the existing scoped or unscoped ARP entry
// on index. The interface-bound HTTP probe can populate a missing entry.
func gatewayHardwareAddr(ctx context.Context, index int, gateway netip.Addr) string {
	raw, err := rib.Read(ctx)
	if err != nil {
		return ""
	}
	messages, err := route.ParseRIB(route.RIBTypeRoute, raw)
	if err != nil {
		return ""
	}
	return findGatewayMAC(messages, index, gateway)
}

func findGatewayMAC(messages []route.Message, index int, gateway netip.Addr) string {
	var found string
	for _, message := range messages {
		r, ok := message.(*route.RouteMessage)
		if !ok || r.Index != index || r.Flags&unix.RTF_LLINFO == 0 || len(r.Addrs) <= unix.RTAX_GATEWAY {
			continue
		}
		dst, ok := r.Addrs[unix.RTAX_DST].(*route.Inet4Addr)
		if !ok || netip.AddrFrom4(dst.IP) != gateway {
			continue
		}
		addr, ok := r.Addrs[unix.RTAX_GATEWAY].(*route.LinkAddr)
		if !ok || len(addr.Addr) != 6 || addr.Addr[0]&1 != 0 || [6]byte(addr.Addr) == [6]byte{} {
			continue
		}
		mac := net.HardwareAddr(addr.Addr).String()
		if found != "" && found != mac {
			return ""
		}
		found = mac
	}
	return found
}
