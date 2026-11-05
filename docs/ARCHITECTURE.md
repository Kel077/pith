# Backend architecture

Pith has one pure-Go module and no cgo. `cmd/pith` is the unprivileged MyGo
app; `internal/client` implements `api.Service`. `cmd/pithd` is a root launchd
helper. It always starts idle, even after a crash or reboot. Only an authenticated
request starts routing. The app's rules/configuration are atomically saved with
mode 0600; interface labels remain the System Settings names, with aliases
applied by the UI.

Mobile-hotspot management stays in the unprivileged client. Device requests use
the selected interface, refresh the binding for every new socket, recheck the
name/MAC/router before requests (including pooled sockets), and reject redirects
outside the original origin. Cached sessions have bounded idle connections.
Credentials require a hardware identity and live under the app's keychain
service. Writing an item uses one quoted command on `security -i` stdin; no
password appears in the process arguments, diagnostics, or application files.
The command-input format is bounded and rejects embedded line breaks/NULs.

Hotspot renewal restores cellular data with an independent 15-second bound if
cancellation or a lost response follows an attempted disable. At most two restore
requests are made; failures remain errors and are not reported as successful
renewals. This is a device operation, independent of helper capture: changes to
its public addresses, LAN addresses or DNS do not by themselves stop routing.

## Capturing and forwarding

`engine/capture` records the routing table, creates a WireGuard-Go utun, and
journals its name, index, boot session, random ownership token and intended prefixes before
installing any route. A ready gVisor netstack receives eight IPv4 prefixes:
`1.0.0.0/8`, `2.0.0.0/7`, `4.0.0.0/6`, `8.0.0.0/5`, `16.0.0.0/4`,
`32.0.0.0/3`, `64.0.0.0/2`, and `128.0.0.0/1`. When IPv6 is enabled, it also
receives `100::/8`, `200::/7`, `400::/6`, `800::/5`, `1000::/4`, `2000::/3`,
`4000::/2`, and `8000::/1`. `internal/captureprefix` supplies the same set to
production planning and frozen test profiles. The system default is never replaced.

The omitted `0.0.0.0/8` and `::/8` preserve Darwin's bound-socket default lookup.
When no scoped route matches, XNU's `rt_lookup_common` falls back through
`node_lookup_default`, which looks up the zero address without an explicit `/0`
mask. `rn_lookup_args` performs longest-prefix matching: a capture route such as
`0.0.0.0/1` or `::/1` hides the physical default in that lookup, and its utun
index fails the socket's interface constraint. See Apple's
[route.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.c)
and [radix.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/radix.c).
The split avoids adding physical-interface egress routes. All capture routes
remain attached to the owned utun and disappear with it on process death.
The omitted ranges stay under system routing, including special translation
prefixes inside `::/8`; full capture does not apply rules to those destinations.

LAN DNS servers get optional host routes so their answers can feed the DNS cache.
If a DNS address already has a configured host route, including a gateway's
macOS host route or a scoped copy, Pith preserves it and skips that DNS capture
route. `WarnDNSUnobserved` identifies the server; the plan and private reports
retain the existing routes and reason. Queries using that route are not observed,
so domain rules need another observed name, such as TLS SNI, HTTP Host or QUIC
SNI when sniffing is enabled. No gateway route is deleted or replaced to observe
DNS. Existing more-specific VPN/Tailscale routes and MagicDNS are preserved.
Every split prefix and explicitly requested scoped prefix still fails with
`ErrRouteConflict` on an exact unscoped conflict; a VPN default is reported as
`WarnVPN`.

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
uses the current effective interface with Pith's owned routes excluded, including
another VPN. A failed strict
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
An existing unscoped route equal to a planned capture prefix is an `ErrRouteConflict`, including when
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

The runtime separates capture ownership from replaceable egress observations.
Its implementation is covered by synthetic drivers and transition tests;
full-capture VPN reconstruction on hardware remains outstanding:

* Observe configured routes before capture starts, after activation, on kernel
  notifications, and every second. Verify the exact owned utun and capture routes
  before publishing a new system-route generation. Only verified owned routes are
  removed from this lookup. A missing, replaced, duplicated or newly shadowed
  capture route, lost owned utun, or unavailable ownership observation stops
  capture. Existing bypass prefixes may move between external VPNs; connected
  and local routes may follow address changes. A new unrelated forwarding route
  that shadows capture still requires a fresh reviewed Start.
* Rule references resolve by name and MAC against current interfaces for each
  attempt, and bind the current index. Missing/down adapters fall through the
  ordered list. Address, IPv6 privacy/prefix, DNS and availability changes do not
  stop capture. Each bind gets a lifetime token; removal, reindexing or MAC
  replacement invalidates that token without revoking other interfaces. Check
  it before binding and after dialing; established sockets keep their binding,
  never migrate, and fail locally if that interface lifetime ends.
* System dials and ICMP select the most-specific unscoped route from the verified
  live view. Ambiguous ties, stale indices or a missing route fail that request
  immediately. After a VPN default is withdrawn, public system fallback is held
  for a three-second settling interval; requests fail during the interval rather
  than waiting without a bound. Explicit physical rules remain available. A new
  VPN default can be used immediately; a stable physical default becomes usable
  after settling. No automatic teardown or SafeStart is needed for these updates.
* Independently of notification timing, packets for a pre-existing, more-specific
  unscoped VPN route must not enter this capture. If one does, drop it before
  TCP/UDP rules, native passthrough or ICMP, without stopping other traffic. This
  covers the withdrawal gap without ever sending tailnet or VPN DNS destinations
  through a physical fallback. Deliberate narrower scoped capture remains valid.
* A capture ownership fault removes Pith's capture and closes its relays, reporting
  `ErrNetworkLost`. No automatic restart or route repair is attempted. The journal
  remains an ownership ledger, never a baseline replay: only Pith's own matching
  routes are deleted, and another VPN's rebuilt routes are left intact. Ordinary
  native-passthrough send failures drop that flow's packets, not the whole capture.

These guarantees cover Pith's forwarding while capture exists. After a safe stop,
ordinary macOS routing and the VPN's own withdrawal/kill-switch policy apply.
Routing sockets are asynchronous; userspace cannot make an external privileged
route mutation atomic with a socket send. The immutable VPN-prefix ingress gate
specifically closes the tailnet withdrawal hole while notifications catch up.

The acceptance tool distinguishes an external-only change from exact baseline
restoration using complete helper capture identities and independent snapshots.
Its explicit `rebaseline --accept-external-change` operates only while stopped;
it does not refresh the running engine or establish runtime VPN compatibility.
Reused names/indices require confirmed capture closure, absence of every recorded
tunnel subnet, and a unique whole-VPN address/route migration from a disappeared
baseline interface. The proof and independent address inventories stay in the
private report; missing or contradictory evidence still forbids acceptance.

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
relay. Startup failure publishes completed connection records even before a
Running monitor exists; outbound errors remain in the log. The test harness
also reads a bounded post-Stop log into its private report, independently of
event delivery. A captured DNS or tunnel fault similarly stops routing. The default routes
remain available throughout teardown. Kernel closure of the utun fd removes its
routes on process death; the next idle helper discards only demonstrably stale
ownership metadata and refuses ambiguous live-looking state. Metadata from a
previous boot can never authorize deleting current routes, even if an interface
name and index were reused. Explicit emergency cleanup also verifies the boot
identity and refuses legacy journals that cannot establish ownership.
Authenticated status also exposes `LastCapture`, a copy of the most recently
prepared journal identity and selected subnets. It remains after Stop or SafeStart failure and is
cleared on helper process restart. Controllers can retain these receipts to
distinguish their captures from external changes; the field alone proves neither
active routing nor full baseline restoration and cannot authorize route deletion.
Its additive `EndedAt` is set only after a successful device close and a subsequent
snapshot with no owned capture routes. Failed or unobservable cleanup leaves it
zero. Controllers retain this confirmation separately from immutable identity.

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

Full IPv4 acceptance uses a separate `--test-profile full-ipv4` installation,
never an omitted `--capture` flag on the production installer. Its immutable
launch arguments freeze a literal TLS endpoint, IPv4-only capture, the exact
LAN-DNS host routes and a lease of at most 120 seconds. DNS hosts skipped because
of existing configured host routes are excluded from that frozen set, with the
same diagnostics as production. The engine rejects mixed
scoped/profile arguments, disabled SafeStart, IPv6 enablement, and changed DNS
capture scope before creating a tunnel. Ordinary production capture planning is
used, including all eight IPv4 split prefixes and LAN-DNS routes, then checked against that
frozen route set. Changing the test mode or DNS capture requires a new reviewed
installation rather than rebaseline or edited session data.

The helper starts the lease clock before startup work and bounds preparation and
SafeStart by that deadline. After startup, its independent monitor stops capture
when the same deadline expires, even if the controller dies. Repeated Start and
hot Reload cannot extend it or bypass the profile settings. Explicit Stop/crash
cleanup still removes only journal-owned routes. The completed full IPv4 TCP
and cleanup hardware cases, and the remaining acceptance limits, are recorded
in [root tests](ROOT-TEST.md#recorded-full-ipv4-result).

The separate `host-ipv6` acceptance profile freezes exactly one public IPv6
`/128`, a literal bracketed TLS endpoint, and a lease of at most 60 seconds.
It uses the same independent deadline and teardown as the full IPv4 test profile,
but supplies only that host prefix to scoped capture planning: no full-capture, IPv4 or
DNS capture routes are added. IPv6 and SafeStart must stay enabled. The native
SafeStart socket targets that exact IPv6 literal and must appear in the tunnel;
IPv4 fallback cannot satisfy it. Controller preflights and cases also force
`tcp6`, require global IPv6 on each reviewed adapter, and match the exact IPv6
tuple in the connection record. IPv4 policy is compared before and after; this
alone is not an independent IPv4 connectivity test or full dual-stack acceptance.

See [building and packaging](BUILDING.md) and the consolidated
[authorized root-test procedure](ROOT-TEST.md).
Scoped TCP acceptance does not establish full capture, IPv6, UDP/QUIC,
delegated WebKit/nsurlsessiond attribution, runtime VPN reconstruction, or the
500 Mbps throughput target. See the [next acceptance plan](NEXT-ACCEPTANCE.md).
Replies from unconnected native UDP listeners are not covered by the existing
socket preservation path; captured replies can lose their original source port.
Do not treat the scoped TCP smoke test as verification of native UDP services.
