# Backend architecture

Pith has one pure-Go module and no cgo. `cmd/pith` is the unprivileged MyGo
app; `internal/client` implements `api.Service`. `cmd/pithd` is a root launchd
helper. It always starts idle, even after a crash or reboot. Only an authenticated
request starts routing. The app's rules/configuration are atomically saved with
mode 0600; interface labels remain the System Settings names, with aliases
applied by the UI.

## Capturing and forwarding

`engine/capture` records the routing table, creates a WireGuard-Go utun, and
journals its name, index, random ownership token and intended prefixes before
installing any route. A ready gVisor netstack receives `0.0.0.0/1`, `128.0.0.0/1`
and, when enabled, `::/1` and `8000::/1`. The system default is never replaced.
LAN DNS servers get host routes so their answers can feed the DNS cache. Existing
more-specific VPN/Tailscale routes and MagicDNS are preserved. Exact capture-route
conflicts fail with `ErrRouteConflict`; a VPN default is reported as `WarnVPN`.

New TCP SYNs and UDP tuples become userspace endpoints. At the first packet,
process attribution reads `pcblist_n`, prefers the effective/delegated PID, and
resolves the outermost app bundle and identifier. The log keeps the actual socket
owner separately. A delegated helper cannot match its own app by accident.

Each flow retains an immutable rule snapshot. Block rules win; the first other
matching rule supplies an ordered interface list. Every outbound socket uses
`IP_BOUND_IF` or `IPV6_BOUND_IF`; each attempt gets three seconds. System routing
uses the pre-capture effective interface, including another VPN. A failed strict
TCP rule sends RST. UDP has no handshake: route/socket errors can fail over, but
a silent peer expires its session. DNS sessions idle for ten seconds; other UDP
sessions for two minutes. The bridge limits active flows and pending handshakes.

Native sockets already open before capture keep their existing connection. A PCB
snapshot identifies their outgoing segments, which use a bounded pool of raw,
interface-bound sockets without changing the transport addresses or payload.
Replies continue to reach the original kernel socket. The unknown-TCP handler
also covers the snapshot/activation race; a socket handled or blocked by Pith
is excluded from that path. This native continuity path still requires real
scoped testing. It does not add old native connections to Pith's traffic charts.

TLS ClientHello SNI, HTTP Host and best-effort QUIC Initial SNI supplement a
bounded, TTL-aware DNS cache. TCP sniffing waits at most 200 ms and forwards the
consumed bytes exactly once. DNS responses must match an observed query; ambiguous
shared addresses supply no hostname. ECH, encrypted DNS and multi-datagram QUIC
handshakes can prevent a name match. HTTPS content is not decrypted or logged.

ICMP echo uses a native datagram ICMP socket through the saved system route and
requires a real matching reply. It is outside the API's TCP/UDP rules and traffic
series. Netstack is prevented from fabricating echo replies for remote hosts.

## Lifecycle and recovery

SafeStart resolves a TLS endpoint before capture (default `api.ipify.org:443`).
After activation it has ten seconds to complete a certificate-verified TLS
handshake with an ordinary native socket and prove that exact socket crossed the
tunnel. A failure returns `ErrNetworkLost`, removes capture and closes every
relay. A captured DNS or tunnel fault similarly stops routing. The default routes
remain available throughout teardown. Kernel closure of the utun fd removes its
routes on process death; the next idle helper discards only demonstrably stale
ownership metadata and refuses ambiguous live-looking state.

Stop restores routes before waiting on relays or disk. TCP half-close is retained;
shutdown aborts incomplete handshakes and waits for all workers. Hot rule/scene
changes affect new connections. Changing the IPv6 capture preference restarts
capture, because it changes the actual route set; existing proxied connections
end in that case. Interface availability is refreshed by notifications.

The helper's private directory is `/Library/Application Support/Pith`. It contains
the capture journal, daily JSONL connection snapshots and atomic `traffic.json`.
`LogDays` controls connection history; zero keeps logs only in memory. Traffic
keeps ten minutes of seconds in memory, 24 hours of minutes, nine days of 15-minute
buckets and daily totals on disk. Samples align to `Since + k*Step`; weekly queries
use sub-hour history so local-calendar and DST grouping stays accurate.

## Helper IPC and development

The Unix socket is mode 0600 and checks `LOCAL_PEERCRED` for the installing UID or
root; the client also requires a root peer. HTTP/JSON requests are versioned and
bounded, with NDJSON events. A slow event subscriber is closed and can reconnect.
Status/rates and connection updates are coalesced on a one-second tick only while
running. See [helper protocol](../internal/helper/README.md).

For approved development tests the root helper accepts repeated `--capture CIDR`
flags, `--safe-target IP:443`, `--safe-server-name example.com`, `--data-dir` and
`--socket`. Scoped capture never expands to global DNS routes. Passing these flags
does not start routing: an authenticated Start is still required. **Do not run a
root capture test without the coordinator's explicit go-ahead and the recorded
undo plan.** Unit tests use fake drivers, memory tunnels and synthetic addresses;
unprivileged live tests only read interface/socket metadata or use loopback.

Build/packaging scripts and the consolidated root-test procedure follow in M4.
Actual utun forwarding, native socket continuity, delegated WebKit/nsurlsessiond
attribution and the 500 Mbps throughput target are not yet verified on hardware.
