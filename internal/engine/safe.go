package engine

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/Kel077/pith/internal/engine/capture"
	"github.com/Kel077/pith/internal/engine/forward"
)

type probeTarget struct {
	address netip.AddrPort
	server  string
}

// Resolve before adding capture routes. The verification itself deliberately
// uses an ordinary native socket so it must traverse the new tunnel.
func prepareProbe(ctx context.Context, opts Options, ipv6 bool) (probeTarget, error) {
	target := opts.ProbeAddress
	if target == "" {
		target = "api.ipify.org:443"
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return probeTarget{}, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return probeTarget{}, fmt.Errorf("invalid SafeStart port")
	}
	server := opts.ProbeServerName
	if server == "" {
		server = host
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return probeTarget{}, err
	}
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.Is6() && !ipv6 {
			continue
		}
		if len(opts.Scopes) > 0 {
			inside := false
			for _, prefix := range opts.Scopes {
				if prefix.Contains(addr) {
					inside = true
					break
				}
			}
			if !inside {
				continue
			}
		}
		return probeTarget{netip.AddrPortFrom(addr, uint16(port)), server}, nil
	}
	return probeTarget{}, fmt.Errorf("SafeStart target has no address inside the capture scope")
}

func (p probeTarget) verify(ctx context.Context, plan capture.Plan, f *forward.Forwarder) error {
	bits := -1
	for _, prefix := range plan.Prefixes {
		if prefix.Contains(p.address.Addr()) {
			bits = max(bits, prefix.Bits())
		}
	}
	if bits < 0 {
		return fmt.Errorf("SafeStart target is outside capture")
	}
	for _, route := range plan.Before {
		if !route.Scoped && route.Prefix.Contains(p.address.Addr()) && route.Prefix.Bits() > bits {
			return fmt.Errorf("SafeStart target bypasses capture through a more specific route")
		}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.address.String())
	if err != nil {
		return err
	}
	defer conn.Close()
	local, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return err
	}
	secure := tls.Client(conn, &tls.Config{ServerName: p.server, MinVersion: tls.VersionTLS12})
	if err := secure.HandshakeContext(ctx); err != nil {
		return err
	}
	if !f.SawTCP(netip.AddrPortFrom(local.Addr().Unmap(), local.Port()), p.address) {
		return fmt.Errorf("SafeStart socket did not cross the Pith tunnel")
	}
	return nil
}
