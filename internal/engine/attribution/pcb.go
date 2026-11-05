// Package attribution maps TCP/UDP sockets to their owner and effective app.
package attribution

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/Kel077/pith/internal/api"
)

var (
	ErrNotFound  = errors.New("socket owner not found")
	ErrAmbiguous = errors.New("socket ownership is ambiguous")
)

// Socket is the small subset of Darwin's PCB snapshot needed for attribution.
// It contains no kernel pointers. EffectivePID is the delegated owner, if any.
type Socket struct {
	Local, Remote     netip.AddrPort
	PID, EffectivePID int
	Generation        uint64
	families          uint8 // Darwin INP_IPV4/INP_IPV6; preserves dual-stack binds.
}

// ParsePCB decodes pcblist_n's eight-byte-aligned records. The field offsets
// follow the four-byte-packed xinpcb_n/xsocket_n ABIs in Apple XNU headers:
// bsd/netinet/in_pcb.h and bsd/sys/socketvar.h. This is an independent decoder,
// not a Go memory overlay or a copy of Apple's netstat implementation.
func ParsePCB(data []byte, protocol api.Protocol) ([]Socket, error) {
	proto := uint32(6)
	if protocol == api.UDP {
		proto = 17
	} else if protocol != api.TCP {
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
	if len(data) < 24 || binary.LittleEndian.Uint32(data[:4]) != 24 {
		return nil, errors.New("invalid PCB generation header")
	}
	generation := binary.LittleEndian.Uint64(data[8:16])
	var result []Socket
	var socketRecord, pcbRecord []byte
	footer := false
	for offset := 24; offset < len(data); {
		if len(data)-offset < 8 {
			return nil, errors.New("truncated PCB record header")
		}
		size := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		if size < 8 || size > len(data)-offset {
			return nil, errors.New("invalid PCB record length")
		}
		record := data[offset : offset+size]
		aligned := (size + 7) &^ 7
		if aligned > len(data)-offset {
			return nil, errors.New("truncated PCB alignment padding")
		}
		if size == 24 {
			if offset+aligned != len(data) {
				return nil, errors.New("unexpected PCB generation record")
			}
			footer = true
			break
		}
		kind := binary.LittleEndian.Uint32(record[4:8])
		switch kind {
		case 1: // XSO_SOCKET
			if size < 84 {
				return nil, errors.New("short xsocket_n")
			}
			if socketRecord != nil {
				return nil, errors.New("duplicate socket record")
			}
			socketRecord = record
		case 16: // XSO_INPCB
			if size < 80 {
				return nil, errors.New("short xinpcb_n")
			}
			if pcbRecord != nil {
				return nil, errors.New("duplicate PCB record")
			}
			pcbRecord = record
		}
		if socketRecord != nil && pcbRecord != nil {
			so, inp := socketRecord, pcbRecord
			socketRecord, pcbRecord = nil, nil
			if binary.LittleEndian.Uint64(inp[8:16]) != binary.LittleEndian.Uint64(so[28:36]) {
				return nil, errors.New("PCB/socket identity mismatch")
			}
			if binary.LittleEndian.Uint32(so[36:40]) == proto && binary.LittleEndian.Uint64(inp[28:36]) <= generation {
				localPort, remotePort := binary.BigEndian.Uint16(inp[18:20]), binary.BigEndian.Uint16(inp[16:18])
				var local, remote netip.Addr
				switch {
				case inp[44]&1 != 0:
					local = netip.AddrFrom4([4]byte(inp[76:80]))
					remote = netip.AddrFrom4([4]byte(inp[60:64]))
				case inp[44]&2 != 0:
					local = netip.AddrFrom16([16]byte(inp[64:80])).Unmap()
					remote = netip.AddrFrom16([16]byte(inp[48:64])).Unmap()
				}
				if local.IsValid() {
					socket := Socket{PID: int(int32(binary.LittleEndian.Uint32(so[68:72]))), EffectivePID: int(int32(binary.LittleEndian.Uint32(so[72:76]))), Generation: binary.LittleEndian.Uint64(so[76:84]), families: inp[44] & 3}
					socket.Local, socket.Remote = netip.AddrPortFrom(local, localPort), netip.AddrPortFrom(remote, remotePort)
					result = append(result, socket)
				}
			}
		}
		offset += aligned
	}
	// XNU returns just the leading header for an empty list on some versions.
	if !footer && len(data) != 24 {
		return nil, errors.New("missing PCB generation footer")
	}
	if socketRecord != nil || pcbRecord != nil {
		return nil, errors.New("incomplete PCB/socket pair")
	}
	return result, nil
}

func normalized(a netip.AddrPort) netip.AddrPort {
	if !a.IsValid() {
		return a
	}
	return netip.AddrPortFrom(a.Addr().WithZone("").Unmap(), a.Port())
}

// Find prefers an exact connected tuple over a wildcard UDP bind. A shared
// port must never be attributed to an arbitrary process by snapshot order.
func Find(sockets []Socket, local, remote netip.AddrPort) (Socket, error) {
	local, remote = normalized(local), normalized(remote)
	bestScore := -1
	var best Socket
	ambiguous := false
	for _, socket := range sockets {
		if socket.Local.Port() != local.Port() || socket.PID <= 0 && socket.EffectivePID <= 0 {
			continue
		}
		score := 0
		if socket.Local.Addr() == local.Addr() {
			score += 2
		} else if !socket.Local.Addr().IsUnspecified() {
			continue
		} else {
			families := socket.families
			if families == 0 {
				families = 2
				if socket.Local.Addr().Is4() {
					families = 1
				}
			}
			if local.Addr().Is4() && families&1 == 0 || local.Addr().Is6() && families&2 == 0 {
				continue
			}
		}
		if socket.Remote.Port() != 0 && socket.Remote.Port() != remote.Port() {
			continue
		}
		if !socket.Remote.Addr().IsUnspecified() && socket.Remote.Addr() != remote.Addr() {
			continue
		}
		if socket.Remote.Port() != 0 {
			score += 4
		}
		if !socket.Remote.Addr().IsUnspecified() {
			score += 4
		}
		if score > bestScore {
			best, bestScore, ambiguous = socket, score, false
		} else if score == bestScore && (socket.PID != best.PID || socket.EffectivePID != best.EffectivePID) {
			ambiguous = true
		}
	}
	if bestScore < 0 {
		return Socket{}, ErrNotFound
	}
	if ambiguous {
		return Socket{}, ErrAmbiguous
	}
	return best, nil
}
