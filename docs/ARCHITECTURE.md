# Backend architecture

Pith has one pure-Go module and no cgo. `cmd/pith` is the unprivileged MyGo
app; `internal/client` implements `api.Service`. `cmd/pithd` is a root launchd
helper. It always starts idle, even after a crash or reboot. Only an authenticated
request starts routing. The app's rules/configuration are atomically saved with
mode 0600; interface labels remain the System Settings names, with aliases
applied by the UI.

## Capturing and forwarding

`engine/capture` records the routing table, creates a WireGuard-Go utun, and
journals its name, index, boot session, random ownership token and intended prefixes before
installing any route. A ready gVisor netstack receives `0.0.0.0/1`, `128.0.0.0/1`
and, when enabled, `::/1` and `8000::/1`. The system default is never replaced.
LAN DNS servers get host routes so their answers can feed the DNS cache. Existing
more-specific VPN/Tailscale routes and MagicDNS are preserved. Exact capture-route
conflicts fail with `ErrRouteConflict`; a VPN default is reported as `WarnVPN`.

Routing snapshots omit `RTF_WASCLONED` host routes and `RTF_LLINFO` neighbor
cache entries. Ordinary traffic can add or expire those entries without changing
the configured forwarding policy. Cloning parent routes, static host routes and
VPN routes remain visible for conflict checks, saved system egress and cleanup.

The tunnel uses the first available IPv4 `/30` and IPv6 ULA `/126` from bounded
candidate lists. Both the routing table and assigned interface prefixes must be
free of overlaps; scoped, local and rejecting routes all count as occupied.
These candidates exclude the standard fake-IP and Tailscale ranges. If no subnet
is available, startup fails before creating the utun. Its selected subnets are
included in the capture journal.
Currently cached remote addresses are read separately as additional address
selection exclusions, so filtering the policy snapshot cannot make an observed
remote address become a local tunnel address. They never enter the policy
snapshot or authorize route deletion; uncached remote addresses behind a default
route cannot be inferred from this observation.

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
is excluded from that path. A host-scoped TCP continuity check does not establish
continuity for every protocol or for a changing VPN. This path does not add old
native connections to Pith's traffic charts.

TLS ClientHello SNI, HTTP Host and best-effort QUIC Initial SNI supplement a
bounded, TTL-aware DNS cache. TCP sniffing waits at most 200 ms and forwards the
consumed bytes exactly once. DNS responses must match an observed query; ambiguous
shared addresses supply no hostname. ECH, encrypted DNS and multi-datagram QUIC
handshakes can prevent a name match. HTTPS content is not decrypted or logged.

ICMP echo uses a native datagram ICMP socket through the saved system route and
requires a real matching reply. It is outside the API's TCP/UDP rules and traffic
series. Netstack is prevented from fabricating echo replies for remote hosts.

## VPN and proxy coexistence

Pith leaves more-specific routes in place. Tailscale's tailnet and MagicDNS
therefore keep their existing path; those uncaptured connections are outside
Pith's rules and connection log. With an exit node or another full-tunnel VPN,
unmatched captured connections use the effective route saved before capture.
An explicitly selected physical interface can bypass that route if the VPN
permits bound sockets. VPN policy or a kill switch can still refuse that access.
An existing unscoped `/1` capture prefix is an `ErrRouteConflict`, including when
an exit-node implementation owns it. Pith refuses to replace it. Host-scoped
development capture can test selected destinations without replacing the VPN's
default or split routes, provided no exact host-route conflict exists.

In the reviewed Tailscale layout without an exit node, its default entries are
scoped `0.0.0.0/0` and `::/0`; it adds no `/1` capture routes. Those defaults do
not conflict with Pith's full-capture prefixes and do not become the unscoped
system egress selected by `SystemInterface`. Tailnet and MagicDNS routes are
more specific. Always inspect the actual table: an exit-node mode or another
VPN can supply a different route layout.

### Rebuilding a VPN while capture is running

**Runtime VPN reconstruction is not yet handled reliably.** Startup coexistence
and ownership-safe teardown are separate from keeping an original egress valid
after another VPN replaces its tunnel. The current implementation has these
limits:

* `capture.Plan.Before` is a fixed pre-capture route snapshot. `egress.Router`
  and native-socket passthrough each keep a copy; ICMP uses that router's system
  lookup. `SystemInterface` returns the saved interface **name**, not a validated
  live name/index generation. Interface notifications refresh availability and
  metadata for rule targets, but do not refresh these saved routes. A changed
  interface list is not an atomic routing-table observation.
* If a saved VPN egress disappears, new system/fallback dials still try its old
  name and can fail. If that name is reused, lookup can bind to an unrelated
  interface. Existing bound TCP/UDP sockets are not migrated. A native passthrough
  send error stops capture, but ordinary outbound failures alone do not establish
  a global failure or guarantee an automatic Stop. A closed interface watcher is
  not currently a routing fault either.
* Without an exit node, the saved public default can remain physical while
  Tailscale recreates its more-specific routes. Once those routes return, the
  kernel can bypass Pith again. During withdrawal, full `/1` capture can receive
  tailnet traffic that used to bypass it; the saved lookup may still name the old
  tunnel. This gap is not covered by successful public-host `/32` checks.
* `Build` and `Activate` reject exact unscoped capture-prefix conflicts during
  startup. They are not a continuous conflict detector. Another privileged
  service can later remove, replace, or shadow a capture route. The engine does
  not yet revalidate capture ownership or rerun SafeStart on such changes.
* The journal is an immutable ownership ledger for Pith's own prefixes and utun,
  not a saved system configuration to replay. Stop/recovery compare intended
  prefixes with the recorded name, index and boot identity, then close Pith's
  device. They do not restore an old VPN interface or delete a rebuilt VPN's
  routes merely because the baseline changed. This protects normal teardown
  across a VPN rebuild, but does not repair stale forwarding decisions. A journal
  token identifies the application record; it is not a kernel route-generation
  tag and must not justify guessing ownership of a reused interface.

Before claiming unattended full-capture compatibility with VPN reconstruction,
add generation-aware route observation. The conservative first implementation
should stop capture with a reported network-change failure when the effective
external egress disappears/reindexes, capture ownership changes, or observation
fails. Recompute the external baseline only during an explicit fresh Start.
A future implementation that updates new-flow egress in place must exclude only
verified Pith-owned routes, never learn Pith's own `/1` or DNS host routes as its
upstream, validate live name/index identity, and publish the route view atomically.
Existing connections must not silently migrate. Both designs need synthetic
route-transition tests before an attended, separately approved full-capture test.
These mechanisms are design work, not implemented behavior in this revision.

The acceptance tool distinguishes an external-only change from exact baseline
restoration using complete helper capture identities and independent snapshots.
Its explicit `rebaseline --accept-external-change` operates only while stopped;
it does not refresh the running engine or establish runtime VPN compatibility.

### Application proxies and fake addresses

System proxy settings are not changed. If an app connects to a loopback HTTP or
SOCKS proxy, that local socket normally stays outside capture; the proxy's own
outbound connection belongs to its process. An app-specific rule does not turn
off that app's proxy settings or recover the original app behind an arbitrary
proxy. Direct app connections and the proxy's captured outbound connections
remain subject to their respective rules.

For a **captured** destination in `198.18.0.0/15`, a matched physical-interface
route with an observed domain resolves that domain using only that interface's
DNS servers and bound sockets. Fake answers are rejected. The original flow's
immutable block rules are also checked against the real destination candidates.
An allowed candidate may connect; if known blocked candidates remain after the
allowed candidates fail, the flow is blocked instead of returning its fake IP
to the proxy through fallback. System/VPN egress otherwise retains the original
fake address, including unmatched flows. Without an observed domain, the flow
uses its saved system path even when the route's normal fallback is disabled;
the connection log explains that decision. A matching block still wins.

These fake-IP rules do not add overriding capture routes. A proxy's more-specific
fake-pool route can keep traffic entirely outside Pith. Custom fake-IP pools are
not inferred from arbitrary tunnel routes: those routes can describe real private
networks, including a tailnet. Compatibility tests cover synthetic route/flow
cases; no particular proxy's TUN mode has been verified on hardware.

## Lifecycle and recovery

SafeStart resolves a TLS endpoint before capture (default `api.ipify.org:443`).
After activation it has ten seconds to complete a certificate-verified TLS
handshake with an ordinary native socket and prove that exact socket crossed the
tunnel. A failure returns `ErrNetworkLost`, removes capture and closes every
relay. A captured DNS or tunnel fault similarly stops routing. The default routes
remain available throughout teardown. Kernel closure of the utun fd removes its
routes on process death; the next idle helper discards only demonstrably stale
ownership metadata and refuses ambiguous live-looking state. Metadata from a
previous boot can never authorize deleting current routes, even if an interface
name and index were reused. Explicit emergency cleanup also verifies the boot
identity and refuses legacy journals that cannot establish ownership.
Authenticated status also exposes `LastCapture`, a copy of the most recently
prepared journal identity. It remains after Stop or SafeStart failure and is
cleared on helper process restart. Controllers can retain these receipts to
distinguish their captures from external changes; the field alone proves neither
active routing nor successful cleanup and cannot authorize route deletion.

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
does not start routing: an authenticated Start is still required. Each scoped
capture stops automatically after five minutes even if its controller dies;
hot reload never extends that lease. **Do not run a
root capture test without the coordinator's explicit go-ahead and the recorded
undo plan.** Unit tests use fake drivers, memory tunnels and synthetic addresses;
unprivileged live tests only read interface/socket metadata or use loopback.

See [building and packaging](BUILDING.md) and the consolidated
[authorized root-test procedure](ROOT-TEST.md).
Scoped TCP acceptance does not establish full `/1` capture, IPv6, UDP/QUIC,
delegated WebKit/nsurlsessiond attribution, runtime VPN reconstruction, or the
500 Mbps throughput target. See the [next acceptance plan](NEXT-ACCEPTANCE.md).
Replies from unconnected native UDP listeners are not covered by the existing
socket preservation path; captured replies can lose their original source port.
Do not treat the scoped TCP smoke test as verification of native UDP services.
