//go:build darwin

package capture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// Decode individual frames so a malformed external event cannot hide a later
// capture withdrawal in the same read. Summaries contain ABI metadata only.
// Header layouts: XNU bsd/net/{route.h,if.h}. rt_msg1 in rtsock.c omits the last
// sockaddr's alignment bytes; the RIB's rt_msg2 includes them. x/net expects
// aligned sockaddrs, so supply only missing alignment, never missing payload.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/rtsock.c
func parseRouteEvents(data []byte) []Change {
	var result []Change
	for len(data) != 0 {
		if len(data) < 4 {
			return append(result, routeFrameFault("RTM truncated header"))
		}
		n := int(binary.NativeEndian.Uint16(data[:2]))
		if n < 4 || n > len(data) {
			return append(result, routeFrameFault(fmt.Sprintf("RTM type=%d length=%d: invalid frame boundary", data[3], n)))
		}
		frame := data[:n]
		data = data[n:]
		meta, known := routeHeader(frame)
		if frame[2] != unix.RTM_VERSION {
			result = append(result, routeFrameFault(meta.summary()+": unsupported message version"))
			continue
		}
		if !known {
			continue // GET replies and other non-policy notifications.
		}
		if !meta.complete {
			if meta.route {
				result = append(result, routeFrameFault(meta.summary()+": truncated policy header"))
			} else {
				result = append(result, meta.notification("truncated notification header"))
			}
			continue
		}
		if meta.route && (meta.flags&(unix.RTF_WASCLONED|unix.RTF_LLINFO) != 0 || binary.NativeEndian.Uint32(frame[28:32]) != 0) {
			continue // Neighbor/cache churn and unsuccessful requests.
		}
		aligned, problem := meta.sockaddrs(frame)
		if !meta.route {
			// XNU rt_ifmsg sends ifm_addrs=0. x/net's RIB parser intentionally
			// skips IFINFO without RTA_IFP, which is not a malformed notification.
			// Read the type-specific header ourselves; parse optional sockaddrs
			// only to diagnose their encoding, never to infer a route mutation.
			if problem == "" {
				if _, err := route.ParseRIB(route.RIBTypeRoute, aligned); err != nil {
					problem = "invalid notification sockaddr encoding"
				}
			}
			result = append(result, meta.notification(problem))
			continue
		}
		if problem == "" {
			messages, err := route.ParseRIB(route.RIBTypeRoute, aligned)
			if err == nil && len(messages) == 1 {
				for _, event := range changes(messages) {
					if event.Diagnostic != "" {
						event.Diagnostic = meta.summary() + ": " + event.Diagnostic
					}
					result = append(result, event)
				}
				continue
			}
			problem = "invalid routing sockaddr encoding"
		}
		diagnostic := meta.summary() + ": " + problem
		result = append(result, Change{Index: meta.index, Prefix: meta.prefixHint(frame), Added: meta.typ == unix.RTM_ADD, Scoped: meta.flags&unix.RTF_IFSCOPE != 0, Unparsed: true, NonIP: meta.nonIP, Diagnostic: diagnostic})
	}
	return result
}

func routeFrameFault(summary string) Change {
	return Change{Diagnostic: summary, Err: errors.New(summary)}
}

type routeEventHeader struct {
	typ, index, flags, size int
	attrs                   uint32
	route, complete, nonIP  bool
	formats                 []string
	prefixAddrs             map[int][]byte
}

func routeHeader(b []byte) (m routeEventHeader, known bool) {
	m.typ = int(b[3])
	switch m.typ {
	case unix.RTM_ADD, unix.RTM_DELETE, unix.RTM_CHANGE, unix.RTM_REDIRECT:
		m.size, m.route = unix.SizeofRtMsghdr, true
	case unix.RTM_NEWADDR, unix.RTM_DELADDR:
		m.size = unix.SizeofIfaMsghdr
	case unix.RTM_NEWMADDR, unix.RTM_DELMADDR:
		m.size = unix.SizeofIfmaMsghdr
	case unix.RTM_NEWMADDR2:
		m.size = unix.SizeofIfmaMsghdr2
	case unix.RTM_IFINFO:
		m.size = unix.SizeofIfMsghdr
	case unix.RTM_IFINFO2:
		m.size = unix.SizeofIfMsghdr2
	default:
		return m, false
	}
	if len(b) < 16 {
		return m, true
	}
	m.complete = len(b) >= m.size
	m.flags = int(binary.NativeEndian.Uint32(b[8:12]))
	if m.route {
		m.index = int(binary.NativeEndian.Uint16(b[4:6]))
		m.attrs = binary.NativeEndian.Uint32(b[12:16])
	} else {
		m.index = int(binary.NativeEndian.Uint16(b[12:14]))
		m.attrs = binary.NativeEndian.Uint32(b[4:8])
	}
	return m, true
}

func (m *routeEventHeader) notification(problem string) Change {
	iface := m.typ == unix.RTM_IFINFO || m.typ == unix.RTM_IFINFO2
	event := Change{Index: m.index, Interface: iface, Address: !iface,
		Added: m.typ == unix.RTM_NEWADDR || m.typ == unix.RTM_NEWMADDR || m.typ == unix.RTM_NEWMADDR2,
		Up:    iface && m.complete && m.flags&unix.IFF_UP != 0}
	if problem != "" {
		event.Unparsed, event.Diagnostic = true, m.summary()+": "+problem
	}
	return event
}

func (m *routeEventHeader) summary() string {
	return fmt.Sprintf("RTM type=%d flags=0x%x index=%d addrs=[%s]", m.typ, m.flags, m.index, strings.Join(m.formats, ","))
}

func (m *routeEventHeader) sockaddrs(frame []byte) ([]byte, string) {
	if m.attrs>>unix.RTAX_MAX != 0 {
		return nil, "unknown sockaddr slots"
	}
	offset := m.size
	m.prefixAddrs = make(map[int][]byte)
	for slot := 0; slot < unix.RTAX_MAX; slot++ {
		if m.attrs&(1<<slot) == 0 {
			continue
		}
		last := m.attrs>>uint(slot+1) == 0
		// rt_msg1 can omit the entire final, zero-length default mask.
		if offset == len(frame) && last && (slot == unix.RTAX_NETMASK || slot == unix.RTAX_GENMASK) {
			m.formats = append(m.formats, fmt.Sprintf("%d:family=0/len=0", slot))
			m.prefixAddrs[slot] = nil
			return padRouteFrame(frame, 4)
		}
		if len(frame)-offset < 2 {
			m.formats = append(m.formats, fmt.Sprintf("%d:truncated", slot))
			return nil, "truncated sockaddr header"
		}
		length, family := int(frame[offset]), int(frame[offset+1])
		m.formats = append(m.formats, fmt.Sprintf("%d:family=%d/len=%d", slot, family, length))
		if length > len(frame)-offset || length == 1 {
			return nil, "truncated sockaddr payload"
		}
		if slot == unix.RTAX_DST || slot == unix.RTAX_NETMASK {
			m.prefixAddrs[slot] = frame[offset : offset+length]
		}
		if family == unix.AF_LINK && length >= 8 {
			if m.index == 0 {
				m.index = int(binary.NativeEndian.Uint16(frame[offset+2 : offset+4]))
			}
			if slot == unix.RTAX_DST {
				m.nonIP = true
			}
		}
		space := (max(length, 1) + 3) &^ 3
		if offset+space > len(frame) {
			if !last {
				return nil, "missing later sockaddr"
			}
			return padRouteFrame(frame, offset+space-len(frame))
		}
		offset += space
	}
	return frame, ""
}

// A malformed gateway can still leave an unambiguous IP key. Decode that key
// separately for relevance classification only; never publish it as a Route or
// use it as ownership/deletion evidence. Keep x/net's KAME/mask normalization.
func (m *routeEventHeader) prefixHint(frame []byte) netip.Prefix {
	dst := m.prefixAddrs[unix.RTAX_DST]
	if len(dst) < 2 || !(dst[1] == unix.AF_INET && len(dst) == unix.SizeofSockaddrInet4 || dst[1] == unix.AF_INET6 && len(dst) == unix.SizeofSockaddrInet6) {
		return netip.Prefix{}
	}
	b := append([]byte(nil), frame[:m.size]...)
	attrs := m.attrs & (unix.RTA_DST | unix.RTA_NETMASK)
	for _, slot := range []int{unix.RTAX_DST, unix.RTAX_NETMASK} {
		if attrs&(1<<slot) == 0 {
			continue
		}
		a, ok := m.prefixAddrs[slot]
		if !ok {
			return netip.Prefix{}
		}
		b = append(b, a...)
		b = append(b, make([]byte, ((max(len(a), 1)+3)&^3)-len(a))...)
	}
	binary.NativeEndian.PutUint32(b[12:16], attrs)
	binary.NativeEndian.PutUint16(b[:2], uint16(len(b)))
	messages, err := route.ParseRIB(route.RIBTypeRoute, b)
	if err != nil || len(messages) != 1 {
		return netip.Prefix{}
	}
	r, ok := messages[0].(*route.RouteMessage)
	if !ok {
		return netip.Prefix{}
	}
	p, _ := routePrefix(r)
	return p
}

func padRouteFrame(frame []byte, padding int) ([]byte, string) {
	if len(frame)+padding > 65535 {
		return nil, "aligned message exceeds frame length limit"
	}
	b := make([]byte, len(frame)+padding)
	copy(b, frame)
	binary.NativeEndian.PutUint16(b[:2], uint16(len(b)))
	return b, ""
}
