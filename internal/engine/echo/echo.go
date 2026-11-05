// Package echo relays ICMP echo requests through the pre-capture system route.
package echo

import (
	"context"
	"encoding/binary"
	"net/netip"
	"sync"
	"time"
)

type ExchangeFunc func(context.Context, string, netip.Addr, []byte) ([]byte, error)

type Options struct {
	System   func(netip.Addr) string
	Write    func([]byte) error
	Exchange ExchangeFunc // nil: an interface-bound native ICMP socket
	Limit    int
	Timeout  time.Duration
}

type Relay struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	active int
	wg     sync.WaitGroup
}

func New(opts Options) *Relay {
	if opts.Exchange == nil {
		opts.Exchange = exchange
	}
	if opts.Limit <= 0 {
		opts.Limit = 64
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{opts: opts, ctx: ctx, cancel: cancel}
}

// Handle copies a validated echo request and returns immediately. Echo bypasses
// TCP/UDP rules, which have no ICMP protocol; it always uses the saved system
// route. A timeout produces no synthetic reply.
func (r *Relay) Handle(source, destination netip.Addr, message []byte) {
	if r.opts.System == nil || r.opts.Write == nil || !source.IsValid() || !destination.IsValid() || source.BitLen() != destination.BitLen() || !destination.IsGlobalUnicast() || len(message) < 8 || len(message) > 65515 || message[1] != 0 {
		return
	}
	if source.Is4() {
		if message[0] != 8 || checksum(message) != 0 {
			return
		}
	} else {
		if message[0] != 128 || checksum(pseudo(source, destination, message)) != 0 {
			return
		}
	}
	iface := r.opts.System(destination)
	if iface == "" {
		return
	}
	r.mu.Lock()
	if r.closed || r.active >= r.opts.Limit {
		r.mu.Unlock()
		return
	}
	r.active++
	r.wg.Add(1)
	r.mu.Unlock()
	message = append([]byte(nil), message...)
	go func() {
		defer func() { r.mu.Lock(); r.active--; r.mu.Unlock(); r.wg.Done() }()
		ctx, cancel := context.WithTimeout(r.ctx, r.opts.Timeout)
		defer cancel()
		reply, err := r.opts.Exchange(ctx, iface, destination, message)
		if err != nil || ctx.Err() != nil || len(reply) != len(message) || reply[1] != 0 {
			return
		}
		if source.Is4() && reply[0] != 0 || source.Is6() && reply[0] != 129 {
			return
		}
		copy(reply[4:8], message[4:8])
		packet := replyPacket(destination, source, reply)
		_ = r.opts.Write(packet)
	}()
}

func (r *Relay) Close() { r.mu.Lock(); r.closed = true; r.cancel(); r.mu.Unlock(); r.wg.Wait() }

func checksum(data []byte) uint16 {
	var sum uint32
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) > 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}

func pseudo(source, destination netip.Addr, message []byte) []byte {
	data := make([]byte, 40+len(message))
	copy(data, source.AsSlice())
	copy(data[16:], destination.AsSlice())
	binary.BigEndian.PutUint32(data[32:36], uint32(len(message)))
	data[39] = 58
	copy(data[40:], message)
	return data
}

func replyPacket(source, destination netip.Addr, message []byte) []byte {
	message = append([]byte(nil), message...)
	message[2], message[3] = 0, 0
	if source.Is4() {
		binary.BigEndian.PutUint16(message[2:4], checksum(message))
		packet := make([]byte, 20+len(message))
		packet[0] = 0x45
		packet[8] = 64
		packet[9] = 1
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
		copy(packet[12:16], source.AsSlice())
		copy(packet[16:20], destination.AsSlice())
		binary.BigEndian.PutUint16(packet[10:12], checksum(packet[:20]))
		copy(packet[20:], message)
		return packet
	}
	binary.BigEndian.PutUint16(message[2:4], checksum(pseudo(source, destination, message)))
	packet := make([]byte, 40+len(message))
	packet[0] = 0x60
	packet[6] = 58
	packet[7] = 64
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(message)))
	copy(packet[8:24], source.AsSlice())
	copy(packet[24:40], destination.AsSlice())
	copy(packet[40:], message)
	return packet
}
