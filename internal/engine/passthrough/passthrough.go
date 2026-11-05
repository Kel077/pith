// Package passthrough preserves native sockets that predate tunnel capture.
// Only their outbound packets are replayed; replies still reach the native PCB.
package passthrough

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/capture"
)

type Packet struct {
	Local, Remote netip.AddrPort
	Protocol      api.Protocol
	Data          []byte
}
type Sender interface {
	Send(string, Packet) error
	Close() error
}
type key struct {
	local, remote netip.AddrPort
	proto         api.Protocol
}
type SnapshotFunc func(api.Protocol) ([]attribution.Socket, error)
type Options struct {
	Guard    *capture.Guard
	Routes   []capture.Route
	Snapshot SnapshotFunc
	Sender   Sender
}
type Table struct {
	mu       sync.Mutex
	opts     Options
	existing map[key]uint64
	proxied  map[key]struct{}
	faults   chan error
}

func New(opts Options) (*Table, error) {
	if opts.Snapshot == nil {
		opts.Snapshot = attribution.Snapshot
	}
	if opts.Sender == nil {
		opts.Sender = newSender(opts.Guard)
	}
	t := &Table{opts: opts, existing: make(map[key]uint64), proxied: make(map[key]struct{}), faults: make(chan error, 1)}
	for _, proto := range []api.Protocol{api.TCP, api.UDP} {
		sockets, err := opts.Snapshot(proto)
		if err != nil {
			opts.Sender.Close()
			return nil, err
		}
		for _, s := range sockets {
			if connected(s) {
				t.existing[key{s.Local, s.Remote, proto}] = s.Generation
			}
		}
	}
	return t, nil
}
func connected(s attribution.Socket) bool {
	return s.Local.IsValid() && s.Remote.IsValid() && s.Local.Port() != 0 && s.Remote.Port() != 0 && !s.Local.Addr().IsUnspecified() && !s.Remote.Addr().IsUnspecified()
}

func (t *Table) Faults() <-chan error { return t.faults }
func (t *Table) Close() error         { return t.opts.Sender.Close() }

// Intercepted permanently excludes this native socket from late adoption until
// it disappears from the PCB table. Queued data after a domain block/RST must
// not become a raw-packet escape from that block.
func (t *Table) Intercepted(local, remote netip.AddrPort) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key{local, remote, api.TCP}
	delete(t.existing, k)
	if len(t.proxied) >= 65536 {
		t.fault(errors.New("native socket exclusion table is full"))
		return
	}
	t.proxied[k] = struct{}{}
}

// Refresh removes closed native sockets. It never adds newly created sockets;
// those belong to the forwarding engine. SYN checks also guard tuple reuse.
func (t *Table) Refresh() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.existing) == 0 && len(t.proxied) == 0 {
		return nil
	}
	for _, proto := range []api.Protocol{api.TCP, api.UDP} {
		sockets, err := t.opts.Snapshot(proto)
		if err != nil {
			return err
		}
		live := make(map[key]uint64)
		for _, s := range sockets {
			if connected(s) {
				live[key{s.Local, s.Remote, proto}] = s.Generation
			}
		}
		for k, generation := range t.existing {
			if k.proto == proto {
				if current, ok := live[k]; !ok || current != generation {
					delete(t.existing, k)
				}
			}
		}
		if proto == api.TCP {
			for k := range t.proxied {
				if _, ok := live[k]; !ok {
					delete(t.proxied, k)
				}
			}
		}
	}
	return nil
}

// Packet handles an unfragmented kernel packet before it enters netstack.
func (t *Table) Packet(data []byte) bool { p, ok := parse(data); return ok && t.process(p, false) }

// Unknown handles a reassembled transport packet with no netstack endpoint.
// A native TCP socket missed in the short snapshot/activation window may be
// adopted for a non-SYN segment. A SYN-ACK may also belong to a new inbound
// native connection; pure SYNs and new UDP flows are never adopted here.
func (t *Table) Unknown(local, remote netip.AddrPort, proto api.Protocol, data []byte) bool {
	adopt := proto == api.TCP && len(data) >= 20 && (data[13]&2 == 0 || data[13]&16 != 0)
	return t.process(Packet{local, remote, proto, data}, adopt)
}

func (t *Table) process(p Packet, adopt bool) bool {
	if len(p.Data) < 8 || p.Protocol == api.TCP && len(p.Data) < 20 {
		return false
	}
	k := key{p.Local, p.Remote, p.Protocol}
	t.mu.Lock()
	if _, intercepted := t.proxied[k]; intercepted {
		t.mu.Unlock()
		return false
	}
	generation, found := t.existing[k]
	// Unlike TCP's SYN, a UDP datagram cannot identify tuple reuse. Validate every
	// preserved UDP packet against its live socket generation: a periodic refresh
	// alone could let a new socket inherit the old flow's exemption from rules.
	if adopt && !found || found && (p.Protocol == api.UDP || p.Protocol == api.TCP && p.Data[13]&2 != 0) {
		sockets, err := t.opts.Snapshot(p.Protocol)
		if err != nil {
			t.mu.Unlock()
			t.fault(err)
			return true
		}
		valid := false
		for _, s := range sockets {
			if s.Local == p.Local && s.Remote == p.Remote && connected(s) {
				valid = !found || s.Generation == generation
				if valid {
					t.existing[k] = s.Generation
				}
				break
			}
		}
		if !valid {
			delete(t.existing, k)
		}
		found = valid
	}
	t.mu.Unlock()
	if !found {
		return false
	}
	iface := capture.SystemInterface(t.opts.Routes, p.Remote.Addr())
	if t.opts.Guard != nil {
		if err := t.opts.Guard.Admit(p.Remote.Addr()); err != nil {
			t.fault(err)
			return true
		}
		if err := t.opts.Guard.Check(iface); err != nil {
			t.fault(err)
			return true
		}
	}
	if iface == "" {
		t.fault(errors.New("original native socket route is unavailable"))
		return true
	}
	if err := t.opts.Sender.Send(iface, p); err != nil {
		t.fault(err)
	}
	return true
}
func (t *Table) fault(err error) {
	select {
	case t.faults <- err:
	default:
	}
}

func parse(data []byte) (Packet, bool) {
	var p Packet
	if len(data) < 20 {
		return p, false
	}
	var source, destination netip.Addr
	offset, protocol := 0, byte(0)
	switch data[0] >> 4 {
	case 4:
		offset = int(data[0]&15) * 4
		length := int(binary.BigEndian.Uint16(data[2:4]))
		if offset < 20 || length > len(data) || length < offset || binary.BigEndian.Uint16(data[6:8])&0x3fff != 0 {
			return p, false
		}
		data = data[:length]
		protocol = data[9]
		source = netip.AddrFrom4([4]byte(data[12:16]))
		destination = netip.AddrFrom4([4]byte(data[16:20]))
	case 6:
		if len(data) < 40 {
			return p, false
		}
		length := 40 + int(binary.BigEndian.Uint16(data[4:6]))
		if length > len(data) {
			return p, false
		}
		data = data[:length]
		offset = 40
		protocol = data[6]
		source = netip.AddrFrom16([16]byte(data[8:24]))
		destination = netip.AddrFrom16([16]byte(data[24:40]))
		for range 8 {
			if protocol != 0 && protocol != 60 {
				break
			}
			if len(data) < offset+2 {
				return p, false
			}
			next := data[offset]
			offset += (int(data[offset+1]) + 1) * 8
			protocol = next
			if offset > len(data) {
				return p, false
			}
		}
	default:
		return p, false
	}
	if protocol == 6 {
		p.Protocol = api.TCP
	} else if protocol == 17 {
		p.Protocol = api.UDP
	} else {
		return p, false
	}
	if len(data) < offset+8 || protocol == 6 && len(data) < offset+20 {
		return p, false
	}
	p.Local = netip.AddrPortFrom(source, binary.BigEndian.Uint16(data[offset:offset+2]))
	p.Remote = netip.AddrPortFrom(destination, binary.BigEndian.Uint16(data[offset+2:offset+4]))
	p.Data = data[offset:]
	return p, true
}
