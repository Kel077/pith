# Connections already open before Start

A route change can cause an existing native TCP socket to send its next segment
into the tunnel. It must not meet netstack's unknown-connection TCP reset. Before
capture, this table records connected native TCP/UDP tuples from the PCB lists.
Their outbound segments retain their original addresses, ports and checksum and
are sent on an interface-bound raw socket using the pre-capture system route.
Replies still reach the kernel's original socket. They are not re-attributed,
re-routed or included as newly proxied traffic.

A TCP segment missed in the snapshot/activation gap is adopted only from the
unknown-endpoint handler, after confirming that the exact native PCB exists.
New SYNs are never adopted that way. Snapshot generations protect reused tuples;
one-second refresh removes closed native sockets. New UDP flows are not adopted.
The raw socket pool is limited to 256 source/interface/protocol combinations.
An inability to preserve a native connection stops capture and restores routing.

The native path uses Darwin's documented raw-IP socket format: `IP_HDRINCL`
requires host-order IPv4 length/fragment fields. IPv6 binds the original source
address and passes the TCP/UDP segment to a protocol-specific raw socket. Header
options are not replayed. Fragmented packets pass through netstack reassembly
before the unknown-endpoint callback. No raw socket is opened by unit tests.

Sources: SDK `netinet/in.h`, `netinet6/in6.h`, Go `x/net/ipv4/header.go`, and
Apple's XNU `bsd/netinet/raw_ip.c` / `bsd/netinet6/raw_ip6.c`. The implementation
is original Go; no XNU implementation is copied. Real native-socket continuity
across capture belongs to the authorized scoped root tests.
