//go:build darwin

package passthrough

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
	"golang.org/x/sys/unix"
)

type socketKey struct {
	iface  string
	source netip.Addr
	proto  api.Protocol
}
type nativeSender struct {
	guard   *capture.Guard
	mu      sync.Mutex
	closed  bool
	sockets map[socketKey]int
}

func newSender(guards ...*capture.Guard) Sender {
	s := &nativeSender{sockets: make(map[socketKey]int)}
	if len(guards) > 0 {
		s.guard = guards[0]
	}
	return s
}

func (s *nativeSender) Send(iface string, p Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.guard != nil {
		if err := s.guard.Check(iface); err != nil {
			return err
		}
	}
	key := socketKey{iface, p.Local.Addr(), p.Protocol}
	fd, exists := s.sockets[key]
	if !exists {
		if os.Geteuid() != 0 {
			return errors.New("native socket preservation requires the root helper")
		}
		if len(s.sockets) >= 256 {
			return errors.New("native socket preservation reached its socket limit")
		}
		var err error
		fd, err = openRaw(key, s.guard)
		if err != nil {
			return err
		}
		s.sockets[key] = fd
	}
	if s.guard != nil {
		if err := s.guard.Check(iface); err != nil {
			return err
		}
	}
	if p.Remote.Addr().Is4() {
		// Darwin IP_HDRINCL uses host-order ip_len/ip_off, unlike utun wire
		// packets. The kernel fills the IPv4 header checksum and packet ID.
		packet := ipv4Packet(p)
		return unix.Sendto(fd, packet, 0, &unix.SockaddrInet4{Addr: p.Remote.Addr().As4()})
	}
	return unix.Sendto(fd, p.Data, 0, &unix.SockaddrInet6{Addr: p.Remote.Addr().As16()})
}

func ipv4Packet(p Packet) []byte {
	packet := make([]byte, 20+len(p.Data))
	packet[0] = 0x45
	packet[8] = 64
	packet[9] = 6
	if p.Protocol == api.UDP {
		packet[9] = 17
	}
	binary.NativeEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], p.Local.Addr().AsSlice())
	copy(packet[16:20], p.Remote.Addr().AsSlice())
	copy(packet[20:], p.Data)
	return packet
}

// Raw IPv4 preserves the complete TCP/UDP segment and its original addresses.
// Raw IPv6 binds the original local address and supplies only the segment; the
// kernel builds its IPv6 header. No socket here is allowed an unbound egress.
func openRaw(key socketKey, guards ...*capture.Guard) (int, error) {
	iface, err := net.InterfaceByName(key.iface)
	if err != nil {
		return -1, err
	}
	if len(guards) > 0 && guards[0] != nil {
		index, err := guards[0].Index(key.iface)
		if err != nil {
			return -1, err
		}
		if iface.Index != index {
			return -1, guards[0].Revoke(errors.New("raw socket interface generation changed"))
		}
	}
	family, protocol := unix.AF_INET, unix.IPPROTO_RAW
	if key.source.Is6() {
		family = unix.AF_INET6
		protocol = unix.IPPROTO_TCP
		if key.proto == api.UDP {
			protocol = unix.IPPROTO_UDP
		}
	}
	fd, err := unix.Socket(family, unix.SOCK_RAW, protocol)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(fd)
	fail := func(err error) (int, error) { unix.Close(fd); return -1, err }
	if err := unix.SetNonblock(fd, true); err != nil {
		return fail(err)
	}
	if key.source.Is4() {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index); err != nil {
			return fail(err)
		}
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_HDRINCL, 1); err != nil {
			return fail(err)
		}
	} else {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, iface.Index); err != nil {
			return fail(err)
		}
		if err := unix.Bind(fd, &unix.SockaddrInet6{Addr: key.source.As16()}); err != nil {
			return fail(err)
		}
	}
	return fd, nil
}
func (s *nativeSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var failures []error
	for k, fd := range s.sockets {
		if err := unix.Close(fd); err != nil {
			failures = append(failures, err)
		}
		delete(s.sockets, k)
	}
	return errors.Join(failures...)
}
