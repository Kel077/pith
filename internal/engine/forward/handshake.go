package forward

import (
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func handshakeID(fl *flow) stack.TransportEndpointID {
	return stack.TransportEndpointID{
		LocalAddress: tcpip.AddrFromSlice(fl.remote.Addr().AsSlice()), LocalPort: fl.remote.Port(),
		RemoteAddress: tcpip.AddrFromSlice(fl.local.Addr().AsSlice()), RemotePort: fl.local.Port(),
	}
}

// CreateEndpoint registers a half-open endpoint before waiting for the ACK.
// Abort only this exact tuple; FindTransportEndpoint's wildcard match alone
// cannot authorize resetting another endpoint.
func (f *Forwarder) abortHandshake(fl *flow) {
	key := [2]netip.AddrPort{fl.local, fl.remote}
	f.mu.Lock()
	current := f.pendingTCP[key] == fl
	f.mu.Unlock()
	if !current {
		return
	}
	proto := ipv4.ProtocolNumber
	if fl.remote.Addr().Is6() {
		proto = ipv6.ProtocolNumber
	}
	id := handshakeID(fl)
	ep, ok := f.stack.FindTransportEndpoint(proto, tcp.ProtocolNumber, id, 1).(*tcp.Endpoint)
	if !ok {
		return
	}
	if info, ok := ep.Info().(*stack.TransportEndpointInfo); ok && info.ID == id {
		f.mu.Lock()
		current = f.pendingTCP[key] == fl
		f.mu.Unlock()
		if current {
			ep.Abort()
		}
	}
}

// A cancellation can precede gVisor's endpoint registration. In its
// startHandshake, registration precedes the first SYN-ACK, so inspecting that
// packet closes the window without polling or scanning unrelated endpoints.
// Subsequent SYN-ACKs for a revoked pending flow are discarded as well.
func (f *Forwarder) dropRevokedSYNACK(pkt *stack.PacketBuffer) bool {
	if pkt.TransportProtocolNumber != tcp.ProtocolNumber {
		return false
	}
	h := header.TCP(pkt.TransportHeader().Slice())
	if len(h) < header.TCPMinimumSize || !h.Flags().Contains(header.TCPFlagSyn|header.TCPFlagAck) {
		return false
	}
	network := pkt.Network()
	id := stack.TransportEndpointID{
		LocalAddress: network.SourceAddress(), LocalPort: h.SourcePort(),
		RemoteAddress: network.DestinationAddress(), RemotePort: h.DestinationPort(),
	}
	local, remote := tuple(id)
	f.mu.Lock()
	fl := f.pendingTCP[[2]netip.AddrPort{local, remote}]
	revoked := fl != nil && (fl.revoked.Load() || fl.ctx.Err() != nil)
	f.mu.Unlock()
	if revoked {
		f.abortHandshake(fl)
	}
	return revoked
}
