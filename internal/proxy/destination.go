package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

type interfaceAddrsKey struct{}

// WithInterfaceAddrs supplies the kernel address source to a proxy connection
// and its dialers. Nil uses net.InterfaceAddrs. The source must be safe for
// concurrent calls and return every interface's addresses, including secondary
// and temporary addresses and those of tunnels, not just eligible proxy exits.
func WithInterfaceAddrs(ctx context.Context, source func() ([]net.Addr, error)) context.Context {
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, interfaceAddrsKey{}, source)
}

func ownAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	return !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		(addr.Is4() && addr.As4()[0] == 0)
}

// CheckDestination refuses addresses a local proxy must never reach. Call it
// on the literal address being dialed, after resolution, never on an independent
// lookup of a name. It reads the kernel's complete unicast address list afresh:
// no subprocess, interface-inventory omissions, cache or background worker.
// Zones do not distinguish own addresses: an address on any interface is refused.
// An unreadable or incomplete address list fails closed.
func CheckDestination(ctx context.Context, addr netip.Addr) error {
	if ownAddress(addr) {
		return errOwnTarget
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, _ := ctx.Value(interfaceAddrsKey{}).(func() ([]net.Addr, error))
	if source == nil {
		source = net.InterfaceAddrs
	}
	addresses, err := source()
	if err != nil {
		return fmt.Errorf("%w: cannot read this Mac's addresses: %v", errOwnTarget, err)
	}
	addr = addr.Unmap().WithZone("")
	for _, address := range addresses {
		var ip net.IP
		switch address := address.(type) {
		case *net.IPNet:
			if address != nil {
				ip = address.IP
			}
		case *net.IPAddr:
			if address != nil {
				ip = address.IP
			}
		}
		own, ok := netip.AddrFromSlice(ip)
		if !ok {
			return fmt.Errorf("%w: cannot read an interface address", errOwnTarget)
		}
		if own.Unmap().WithZone("") == addr {
			return errOwnTarget
		}
	}
	return ctx.Err()
}

// checkRemote runs before any application data or success reply is sent. An
// address may have become this Mac's since the pre-dial check. A fresh kernel
// read, including interfaces other than the exit, closes that gap.
func checkRemote(ctx context.Context, conn net.Conn) error {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || remote == nil {
		return errOwnTarget
	}
	addr, ok := netip.AddrFromSlice(remote.IP)
	if !ok {
		return errOwnTarget
	}
	return CheckDestination(ctx, addr)
}
