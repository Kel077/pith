# Tunnel forwarding

The bridge uses the Apache-2.0 gVisor netstack and its public TCP/UDP forwarders.
Packet tests connect two in-memory netstacks; they create no network interfaces
and install no routes. Tests cover both IP families, HTTP domain blocks with TCP
RST, ordered interface fallback, policy snapshots across reload, datagram session
reuse/expiry, and stopping a half-open TCP handshake. Race tests cover teardown.

TCP waits at most 200 ms for a recognizable ClientHello or HTTP request before
dialing. Bytes consumed while sniffing are forwarded exactly once. DNS queries
and replies on port 53 feed the correlated name cache; QUIC Initial inspection is
best effort. Attribution uses the effective app while retaining the socket owner
in the connection log. A delegated helper cannot match its own app's rule.

Every outbound TCP/UDP socket is bound to a named interface. System fallback uses
the saved pre-capture route, including an existing VPN's default. UDP has no
connection handshake: fallback can observe socket/route errors, but a peer that
silently discards datagrams expires the session rather than proving another
interface works. General UDP is idle for at most two minutes, DNS ten seconds.
ICMP echo is handed to the engine's native echo relay; the stack never fabricates
a successful ping to an unreachable destination.

The bridge bounds active TCP/UDP flows at 8192 and SYN handshakes at 1024. It owns
all workers and must be closed after capture routes are removed. Half-open
endpoints are aborted while their creation settles, then the stack is drained.
Real utun capture, interface binding under capture, and throughput still need
the separately authorized root test session.
