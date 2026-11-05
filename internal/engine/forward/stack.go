// Package forward bridges an IP tunnel to gVisor and interface-bound sockets.
package forward

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/attribution"
	"github.com/Kel077/pith/internal/engine/domains"
	"github.com/Kel077/pith/internal/engine/egress"
	"github.com/Kel077/pith/internal/engine/telemetry"
	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type LookupFunc func(context.Context, api.Protocol, netip.AddrPort, netip.AddrPort) (attribution.Attribution, error)

type Options struct {
	Device   tun.Device
	Router   *egress.Router
	Store    *telemetry.Store
	Lookup   LookupFunc
	IPv6     bool
	MaxFlows int
	UDPIdle  time.Duration
	DNSIdle  time.Duration
	// Echo handles validated, reassembled ICMP echo requests. It must copy data
	// it retains and return quickly. Nil drops echo instead of fabricating replies.
	Echo func(source, destination netip.Addr, message []byte)
}

type Forwarder struct {
	opts      Options
	stack     *stack.Stack
	link      *channel.Endpoint
	cache     *domains.Cache
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closing   bool
	active    int
	flows     sync.WaitGroup
	io        sync.WaitGroup
	writeMu   sync.Mutex
	closeOnce sync.Once
	faults    chan error
}

// New starts packet workers but does not install routes or change interfaces.
// Close must be called even if activating capture subsequently fails.
func New(opts Options) (*Forwarder, error) {
	if opts.Device == nil || opts.Router == nil || opts.Router.Policy() == nil || opts.Store == nil {
		return nil, errors.New("incomplete forwarding options")
	}
	if opts.MaxFlows <= 0 {
		opts.MaxFlows = 8192
	}
	if opts.UDPIdle <= 0 {
		opts.UDPIdle = 2 * time.Minute
	}
	if opts.DNSIdle <= 0 {
		opts.DNSIdle = 10 * time.Second
	}
	if opts.Lookup == nil {
		opts.Lookup = attribution.New().Lookup
	}
	networks := []stack.NetworkProtocolFactory{ipv4.NewProtocol}
	if opts.IPv6 {
		networks = append(networks, ipv6.NewProtocol)
	}
	s := stack.New(stack.Options{NetworkProtocols: networks, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6}})
	link := channel.New(1024, 1500, "")
	fail := func(err tcpip.Error) (*Forwarder, error) {
		link.Close()
		s.Close()
		s.Wait()
		return nil, errors.New(err.String())
	}
	if err := s.CreateNIC(1, link); err != nil {
		return fail(err)
	}
	if err := s.SetPromiscuousMode(1, true); err != nil {
		return fail(err)
	}
	if err := s.SetSpoofing(1, true); err != nil {
		return fail(err)
	}
	routes := []tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}}
	if opts.IPv6 {
		routes = append(routes, tcpip.Route{Destination: header.IPv6EmptySubnet, NIC: 1})
	}
	s.SetRouteTable(routes)
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{opts: opts, stack: s, link: link, cache: domains.NewCache(0), ctx: ctx, cancel: cancel, faults: make(chan error, 1)}
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, 1024, f.tcp).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s, f.udp).HandlePacket)
	consumeICMP := func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		message := stack.PayloadSince(pkt.TransportHeader())
		defer message.Release()
		data := message.AsSlice()
		if len(data) >= 8 && data[1] == 0 && (data[0] == 8 && pkt.NetworkProtocolNumber == ipv4.ProtocolNumber || data[0] == 128 && pkt.NetworkProtocolNumber == ipv6.ProtocolNumber) && opts.Echo != nil {
			source, _ := netip.AddrFromSlice(id.RemoteAddress.AsSlice())
			destination, _ := netip.AddrFromSlice(id.LocalAddress.AsSlice())
			opts.Echo(source, destination, data)
		}
		return true
	}
	s.SetTransportProtocolHandler(icmp.ProtocolNumber4, consumeICMP)
	s.SetTransportProtocolHandler(icmp.ProtocolNumber6, consumeICMP)
	f.io.Go(f.read)
	f.io.Go(f.write)
	return f, nil
}

func (f *Forwarder) Faults() <-chan error { return f.faults }
func (f *Forwarder) fault(err error) {
	if f.ctx.Err() == nil {
		select {
		case f.faults <- err:
		default:
		}
	}
}

func (f *Forwarder) begin() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing || f.active >= f.opts.MaxFlows {
		return false
	}
	f.active++
	f.flows.Add(1)
	return true
}
func (f *Forwarder) end() { f.mu.Lock(); f.active--; f.mu.Unlock(); f.flows.Done() }

func tuple(id stack.TransportEndpointID) (local, remote netip.AddrPort) {
	a, _ := netip.AddrFromSlice(id.RemoteAddress.AsSlice())
	b, _ := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	return netip.AddrPortFrom(a.Unmap(), id.RemotePort), netip.AddrPortFrom(b.Unmap(), id.LocalPort)
}

func (f *Forwarder) read() {
	data := make([]byte, 65535+4)
	bufs := [][]byte{data}
	sizes := []int{0}
	for f.ctx.Err() == nil {
		n, err := f.opts.Device.Read(bufs, sizes, 4)
		if err != nil {
			f.fault(fmt.Errorf("read tunnel: %w", err))
			return
		}
		if n != 1 || sizes[0] < 1 || sizes[0] > len(data)-4 {
			continue
		}
		p := data[4 : 4+sizes[0]]
		var proto tcpip.NetworkProtocolNumber
		switch p[0] >> 4 {
		case 4:
			proto = ipv4.ProtocolNumber
		case 6:
			if !f.opts.IPv6 {
				continue
			}
			proto = ipv6.ProtocolNumber
		default:
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), p...))})
		f.link.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
}

func (f *Forwarder) write() {
	for {
		pkt := f.link.ReadContext(f.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		pkt.DecRef()
		err := f.WritePacket(view.AsSlice())
		view.Release()
		if err != nil {
			f.fault(fmt.Errorf("write tunnel: %w", err))
			return
		}
	}
}

// WritePacket also lets the ICMP echo relay return an IP packet to the app.
func (f *Forwarder) WritePacket(packet []byte) error {
	data := make([]byte, len(packet)+4)
	copy(data[4:], packet)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, err := f.opts.Device.Write([][]byte{data}, 4)
	return err
}

// Close cancels every socket, releases the tunnel fd and waits for all workers.
// The capture owner should remove its routes before calling this method.
func (f *Forwarder) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closing = true
		f.mu.Unlock()
		f.cancel()
		f.opts.Device.Close()
		f.io.Wait()
		done := make(chan struct{})
		go func() { f.flows.Wait(); close(done) }()
		// Forwarder.CreateEndpoint registers a half-open endpoint before waiting
		// for its handshake. Repeat abort while in-flight creations settle; a
		// single Stack.Close snapshot could miss an endpoint registered after it.
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			for _, ep := range f.stack.RegisteredEndpoints() {
				ep.Abort()
			}
			select {
			case <-done:
				f.link.Close()
				f.stack.Close()
				f.stack.Wait()
				close(f.faults)
				return
			case <-tick.C:
			}
		}
	})
}
