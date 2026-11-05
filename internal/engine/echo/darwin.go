//go:build darwin

package echo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"github.com/Kel077/pith/internal/netif"
	"net"
	"net/netip"
	"os"

	"golang.org/x/sys/unix"
)

// Darwin's SOCK_DGRAM ICMP sockets preserve echo IDs, accept IP_BOUND_IF /
// IPV6_BOUND_IF, and compute the ICMPv6 checksum. IP_STRIPHDR is documented in
// the SDK's netinet/in.h; net.FilePacketConn gives this socket poller/deadlines.
// References: x/net/icmp/listen_posix.go and XNU ip_icmp.c/raw_ip6.c.
func exchange(ctx context.Context, iface string, destination netip.Addr, message []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if iface == "" {
		return nil, fmt.Errorf("ICMP requires a bound interface")
	}
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	if err := netif.ValidateBinding(ctx, link.Index); err != nil {
		return nil, err
	}
	family, protocol, level, option := unix.AF_INET, unix.IPPROTO_ICMP, unix.IPPROTO_IP, unix.IP_BOUND_IF
	var bind unix.Sockaddr = &unix.SockaddrInet4{}
	if destination.Is6() {
		family, protocol, level, option = unix.AF_INET6, unix.IPPROTO_ICMPV6, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF
		bind = &unix.SockaddrInet6{}
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM, protocol)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), "pith-icmp")
	defer file.Close()
	if err := unix.SetsockoptInt(fd, level, option, link.Index); err != nil {
		return nil, err
	}
	if destination.Is4() {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_STRIPHDR, 1); err != nil {
			return nil, err
		}
	}
	if err := unix.Bind(fd, bind); err != nil {
		return nil, err
	}
	conn, err := net.FilePacketConn(file)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	wire := append([]byte(nil), message...)
	rand.Read(wire[4:8])
	wire[2], wire[3] = 0, 0
	if destination.Is4() {
		binary.BigEndian.PutUint16(wire[2:4], checksum(wire))
	}
	if err := netif.ValidateBinding(ctx, link.Index); err != nil {
		return nil, err
	}
	if _, err := conn.WriteTo(wire, &net.UDPAddr{IP: destination.AsSlice(), Zone: destination.Zone()}); err != nil {
		return nil, err
	}
	data := make([]byte, 65535)
	for {
		n, peer, err := conn.ReadFrom(data)
		if err != nil {
			return nil, err
		}
		addr, ok := peer.(*net.UDPAddr)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(addr.IP)
		if !ok || ip.Unmap() != destination.Unmap() {
			continue
		}
		wantType := byte(0)
		if destination.Is6() {
			wantType = 129
		}
		if n != len(wire) || n < 8 || data[0] != wantType || data[1] != 0 || !bytes.Equal(data[4:n], wire[4:]) {
			continue
		}
		if destination.Is4() && checksum(data[:n]) != 0 {
			continue
		}
		return append([]byte(nil), data[:n]...), nil
	}
}
