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

## Local SOCKS5 listeners

`internal/proxy` runs inside the unprivileged App process. Each configured port
binds TCP on `127.0.0.1` and selects one physical interface; it needs neither an
installed/trusted helper nor active TUN capture. `Config.Proxies` is independent
of scenes. Empty configuration opens no ports. The service implements the
no-auth CONNECT subset of [RFC 1928](https://www.rfc-editor.org/rfc/rfc1928.html),
with IPv4, IPv6 and ASCII/punycode domain requests; BIND and UDP ASSOCIATE return
`0x07`. A successful reply contains the real outbound socket's local address and
port. The parser bounds every field, validates versions/reserved bytes and closes
malformed requests. There is no LAN listener or authentication credential store.

At most 32 unique configured ports, each in 1024--65535, are accepted, including
disabled entries. `Interface.CanProxy` is shared by the interface and backend:
supported physical link categories remain selectable while down, but loopback,
VPN/TUN, bridge and peer interfaces are excluded. `SaveConfig` rejects a currently
resolved unsupported exit; a missing or down physical adapter remains valid saved
intent and reports `interface_unavailable`. A usable DNS configuration is required
for domain requests; literal addresses can still work without it.

`internal/proxy/egress` resolves references with `InterfaceRef.Resolve`, pins the
current name/MAC/index and uses `IP_BOUND_IF` or `IPV6_BOUND_IF`. It validates that
identity before and after dialing and each stream I/O. An index reused by another
interface cannot carry the old connection. New requests can resolve a renamed USB
adapter by its unique MAC; established streams do not migrate. Address/prefix or
DNS changes do not shut down unrelated listeners. Interface metadata is cached
for at most one second, while kernel identity is checked directly. Canceling one
metadata reader does not poison the cache for healthy callers.

DNS uses explicit A/AAAA packets sent only to the selected interface's configured
servers. There is no system resolver, hosts-file/search-list lookup, environment
proxy or other-interface fallback. Truncated UDP answers retry TCP on the same
server and binding. IDs, question, opcode and response code are checked; replies
are limited to 16 KiB and 128 records, CNAME chains to eight steps and candidates
to eight addresses. Resolution has a four-second bound, at most three servers
and 750 ms per server attempt. DNS and target sockets use the same pinned binding.

Physical bound sockets use macOS's scoped routing and the full-capture split that
preserves zero-address lookup, described below. Excluding tunnel interfaces also
prevents selecting Pith's own utun as a proxy exit. A VPN policy can still refuse
physical egress; that fails the request rather than selecting another interface.
TUN scene rules, connection history and traffic totals do not include this
independent forwarding path. A client that resolves names itself or bypasses its
proxy is outside the service's DNS/routing guarantee.

The initial handshake has five seconds; CONNECT setup has ten seconds total
from accept. There are at most 128 sessions per port and 512 overall, including
handshakes. Relay preserves TCP half-close and records only successfully written
stream bytes, excluding SOCKS negotiation and DNS. `Status.Proxies` includes
disabled entries in configuration order; its `Since` and counters belong to one
listener instance and reset on replacement. Listening means the local bind
succeeded, not that the internet is reachable. Target-specific errors do not
become listener errors.

`client.New` is idle. The first successful `LoadConfig` activates enabled local
listeners; a canceled first activation retracts partial listeners. Valid saved
changes take effect after atomic rename, even if the caller then cancels. Removing,
disabling or changing a port's exit closes its pending/established streams before
the replacement can forward; unrelated listeners and label-only edits stay open.
A failed bind remains visible as `port_in_use` and is retryable by saving again.
Proxy-only configuration changes do not reload TUN rules. A previously failed
routing apply remains pending, so disk equality cannot turn its retry into a
false success. Explicit identical saves still retry pending backend work.

Subscriptions combine authenticated helper events with local proxy snapshots;
missing or unverified helper status keeps its existing failure meaning. Local
counter updates continue without a helper stream. `client.Close` cancels setup,
closes all local listeners/streams and joins subscriptions without stopping TUN.
App exit closes every proxy socket regardless of KeepRouting; no proxy daemon is
installed. Protocol, DNS, lifecycle, cancellation and interface-reuse tests use
loopback or injected state. Hardware acceptance at `b8b3628` verified separate
wired/cellular exits, disconnect failure without fallback and automatic recovery,
with TUN disabled and no running helper. Concurrent TUN operation and remapping
an open stream were not covered by that run. A later pre-release check at
`2f9d14b` verified both exits while a normal scene captured traffic, with no
captured connection attributed to the App; Stop restored the prior default
route and tunnel count. Remapping an open proxy stream remains unverified; see the
[recorded scope](ROOT-TEST.md#recorded-local-socks5-result).

## Capturing and forwarding

### iCloud Private Relay

With Private Relay active for Safari, the device's observable connections go to
Apple's ingress relay; the origin is reached by the separate egress relay.
Origin DNS and browsing information are encrypted, as described in
[Apple's overview](https://support.apple.com/102602). Pith cannot recover the
website's true IP/domain from that relay connection, so origin IP/domain rules
cannot select it. A cached origin DNS name must not be treated as the relay's
identity. App matching does not depend on visibility of the inner destination:
for a captured socket that XNU attributes/delegates to Safari, ordinary app-only
rules still choose its exit. A shared/system-owned relay socket without Safari
delegation cannot be relabeled heuristically; it follows its actual attribution.
The separate Safari hardware observation used direct connectivity with Private
Relay disabled; it does not verify this relay case.

The scoped browser harness captures only the frozen origin A records, not Apple
relay endpoints. Active Private Relay therefore does not meet that test's
direct-connect prerequisite. No documented reliable read-only detector of the
effective per-navigation Safari relay state was established, so status is
explicitly unknown; private service preferences are not treated as proof.
Network/site exceptions and availability make a global preference insufficient.
The harness never changes Private Relay, account, browser or network settings.

The optional single-open Safari acceptance mode owns its process lifecycle. It
refuses a pre-existing Safari, records kernel process identities before one
LaunchServices request, and always attempts bounded cleanup. Safari gets SIGTERM;
after its exit and a grace period, only new system WebKit services independently
associated with its new resource/jetsam coalitions can receive SIGKILL. A name,
parent PID or start time alone is insufficient. Unknown ownership is reported,
never guessed. No pre-existing process, unrelated app service or process group is
signaled, and no AppleScript or Automation API is used. Manual mode stays manual.

The pure-Go harness reads `kern.proc.all` and process identity/coalition flavors
from XNU's [process ABI](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info_private.h).
Signal targeting uses `PROC_INFO_CALL_SIGNAL_AUDITTOKEN`, whose
[kernel implementation](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/proc_info.c)
validates the PID/exec generation while holding the target identity; unsupported
or denied calls fail without a numeric-PID fallback. This is confined to the
explicit acceptance option, not the app/helper. Its private reports retain normal,
forced and incomplete cleanup, and a separate post-uninstall inventory lists all
still-present processes started in the recorded window. That inventory is read-only
and cannot authorize future signals. The normal automatic-open cleanup path was
verified on hardware at `936c6a7`; independently managed launchd Safari
agents remain outside its ownership, as recorded in the acceptance documents.

### Capture ownership

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

Routing snapshots and events omit `RTF_WASCLONED` hosts and non-static
`RTF_LLINFO` neighbor entries. A non-cloned `RTF_STATIC | RTF_LLINFO` route
remains configured policy: XNU's `nd6_rtrequest` also attaches neighbor metadata
to manually added IPv6 interface hosts, including point-to-point links. Ordinary
traffic can add or expire dynamic caches without changing policy. Cloning parents,
static hosts and VPN routes remain visible for conflicts, system egress and cleanup.

All native route-table readers share a bounded sysctl reader. The queried size
is only a hint: the RIB can grow before copying it. Reserve at least 16 KiB or
25% extra, whichever is greater, and grow again after `ENOMEM`; never decode
its partial buffer. Each read permits at most ten attempts with short cancellable
backoff, a 250 ms deadline and a 64 MiB allocation cap. XNU may report only the
bytes copied on `ENOMEM`, so that returned length alone is not a new capacity.
See [sysctl's copyout and length handling](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_newsysctl.c)
and its read-only [syscall ABI](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/syscalls.master).

A running ownership check distinguishes an unreadable snapshot from a readable
incorrect one. It retries observation failures within a two-second window, holding
new bindings and system-route publication until a complete snapshot is verified.
Existing flows are not revoked solely by an intermediate read failure; guarded
operations may briefly wait. Recovery keeps capture running. A readable missing,
replaced or duplicated owned route still fails immediately, as does unreadability
that lasts through the window. Stop/cancellation interrupts retry backoff. The
bounded address-free diagnostic ring records recovery or exhausted observation.
Preparation, conflict checks, journal recovery, cleanup and interface discovery
also use the padded reader; none accepts partial data or guesses route ownership.

Every full-width prefix uses `route -host` and a literal address for both adding
and deleting it; this includes IPv4 `/32` and IPv6 `/128`, DNS hosts, scoped
profiles and journal cleanup. Subnets use `-net` and a CIDR. In Apple's
[network_cmds route.c](https://github.com/apple-oss-distributions/network_cmds/blob/main/route.tproj/route.c),
`inet6_makenetandmask` returns a host for `/128` without a netmask. Forcing `-net`
then clears `RTF_HOST`, leaving a non-default destination with neither HOST nor
a mask. This reproduced the former IPv6 startup event-decoding failure in a
synthetic Darwin message. The old failed hardware run retained no message
metadata, so its exact event cannot be recovered retroactively.

For a global IPv6 host, `-interface` selects the named interface's `sockaddr_dl`
as gateway; it does not request `RTF_GATEWAY` or `RTF_IFSCOPE`. `-host` sets
`RTF_HOST`, and `route(8)` defaults to `RTF_STATIC`. XNU then sets `RTF_LLINFO`
on that explicit interface host in
[nd6_rtrequest](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet6/nd6.c).
The configured route is still unscoped, with a link gateway and no IP gateway.
It is not a cloned child: `in6_addroute` only makes non-host unicast routes
cloning, and an exact existing key is rejected (with special handling for
replaceable cache entries). See
[in6_rmx.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet6/in6_rmx.c)
and [route.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.c).
Retaining static LLINFO corrects observation without loosening the one-route,
unscoped, no-IP-gateway, matching-name-and-index ownership checks. Stop, journal
recovery and emergency cleanup use that same configured-policy snapshot.

The route observer decodes each frame separately and retains address-free
metadata for unusual events: RTM type, flags, interface index, and each sockaddr
slot's family and original length. It accepts XNU `rt_msg1`'s unpadded final
sockaddr (including an empty default mask), without inventing missing address
payload. IPv6 point-to-point addresses without a peer have a non-NULL,
zero-length AF_UNSPEC destination in
[in6.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet6/in6.c).
`rt_newaddrmsg` sets `RTA_BRD`, but `rt_msg1` copies zero final bytes. The observer
accepts this empty final slot only for NEWADDR/DELADDR, not missing route gateways
or assigned addresses. `x/net/route` normalizes KAME embedded scope and `sin6_scope_id`; scope
bytes never enter the prefix. Synthetic wire-format tests cover scoped defaults,
link-local destinations/gateways, multicast, link gateways, and address events.

Interface notifications use `if_msghdr`/`if_msghdr2` (112/160 bytes, including
96/128 bytes of embedded interface data); address notifications use `ifa_msghdr`
(20 bytes), and multicast notifications use `ifma_msghdr`/`ifma_msghdr2` (16/20
bytes). Only route events use `rt_msghdr`. XNU's `rt_ifmsg` sends a normal IFINFO
with `ifm_addrs=0`: `x/net/route` skips it because it lacks `RTA_IFP`, not because
it is malformed. The observer reads notification indices/flags directly from
their respective headers and validates optional address payload separately.
See Apple's [if.h](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/if.h)
and [rtsock.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/rtsock.c).

An unreadable IP route event on the owned interface, with unknown interface identity,
or with an uncertain unscoped overlap cannot exclude a capture conflict: stop
safely and retain the reason. A proven non-IP route, an external scoped route,
or an independently decoded external link-local/multicast key unequal to every
capture prefix cannot replace Pith's unscoped capture; record the event,
invalidate affected external bindings, and independently verify the complete
owned route set before continuing. A key recovered from an otherwise malformed
message is used only for relevance, never as a routing or deletion record.
Interface and unicast/multicast address notifications only trigger a complete
capture snapshot, even if their address payload or owned interface notification
is unreadable. The check is serialized with activation to avoid inspecting a
partially installed capture. Native ownership checks also read current interface
flags: a genuinely down or missing utun fails, while a delayed DOWN notification
followed by a currently healthy UP does not. Bad notification payloads alone do
not revoke capture; failed ownership observation still does.
Do not guess a host mask or ignore an unknown unscoped route just because its
destination looks link-local. Stream/framing failures remain ownership faults.
`Status.RouteDiagnostics` retains the latest 32 distinct technical summaries,
without raw addresses or names, through Stop and startup failure. A new capture
attempt or helper restart clears it; ordinary Reload does not.
Ownership failures also retain per-capture-prefix summaries from the exact RIB
read used by the failed check, before cleanup. They include flags, gateway family,
scope, interface index and grouped counts, including filtered cache/down candidates.
Prefix ordinals, family and width replace addresses; interface names are omitted.
Each summary contains at most 32 distinct metadata groups and an omitted count.
Raw metadata is separate from policy equality, so reference flags cannot trigger
egress changes. It never authorizes route selection or deletion.

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

Output packet writes reuse one buffer per Forwarder under the existing write
mutex. The reserved native header is reset and only the current packet length
is passed to the synchronous device write. Storage is not reused until that
write returns, including when concurrent TCP, UDP or ICMP writers are active.
This removes repeated output allocations without changing MTU or packet contents.

New TCP SYNs and UDP tuples become userspace endpoints. At the first packet,
process attribution reads `pcblist_n`, prefers the effective/delegated PID, and
resolves the outermost app bundle and identifier. The log keeps the actual socket
owner separately. A delegated helper cannot match its own app by accident.

Rules compile into immutable snapshots. Block rules win; the first other
matching rule supplies an ordered interface list. Every outbound socket uses
`IP_BOUND_IF` or `IPV6_BOUND_IF`; each attempt gets three seconds. System routing
uses the current effective interface with Pith's owned routes excluded, including
another VPN. A failed strict
TCP rule sends RST. UDP has no handshake: route/socket errors can fail over, but
a silent peer expires its session. DNS sessions idle for ten seconds; other UDP
sessions for two minutes. The bridge limits active flows and pending handshakes.

An ICMP port-unreachable is a per-datagram error, not a UDP close. Consuming it
does not expire the session or refresh idle. The application endpoint and native
outbound socket keep their attribution and binding until idle or an explicit
rule change requires reconnection, including final
QUIC datagrams queued before the client closes. A write affected by that error
may drop its datagram; the relay never retries it or switches interfaces. Other
terminal transport errors still end the session and are retained separately from
the paired relay's resulting close error. This avoids hiding the actual failure
when the other direction reports EOF or a closed socket. The adapter handling
follows gVisor's [UDP error consumption](https://github.com/google/gvisor/blob/ee81581eb45f/pkg/tcpip/transport/udp/endpoint.go)
and [gonet error conversion](https://github.com/google/gvisor/blob/ee81581eb45f/pkg/tcpip/adapters/gonet/gonet.go).

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
Synthetic drivers and transition tests cover the guarantees below. The bounded
full IPv4 Tailscale disconnect/reconnect stage was verified on hardware at
`fbb5234`, without an exit node: local MagicDNS and physical/system public controls
passed through reconstruction, followed by clean Stop with `endedAt`, post-Stop
DNS, reviewed external-change rebaseline and uninstall. The rebuilt tunnel reused
its name with a new index, unchanged addresses and the same 13 routes. Independent
inspection found no capture residue; the final run recorded zero unreachable
connections. See the [hardware record](ROOT-TEST.md#manual-vpn-reconstruction-during-full-ipv4-capture).
This does not verify remote peers, daemon self-update/restart or long-lived
connection continuity.

The preceding attempts led to these bounded fixes:

* `348214c`: distinguish DNS-owned deadline closure from unrelated cancellation.
* `64d1214`: let MagicDNS settle within the existing rebuild deadline.
* `7b04e33`: preserve immediate system egress when scoped VPN defaults change.
* `fbb5234`: tolerate transient closure observations while still requiring proven cleanup.

Runtime guarantees:

* Observe configured routes before capture starts, after activation, on kernel
  notifications, and every second. Verify the exact owned utun and capture routes
  before publishing a new system-route generation. Only verified owned routes are
  removed from this lookup. A missing, replaced, duplicated or newly shadowed
  capture route, lost owned utun, or ownership observation unavailable for the
  bounded recovery window stops
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
  Per-I/O checks query the pinned native index and verify both name and index,
  retaining epoch/generation checks without enumerating all interfaces. Fresh
  bindings and rule references still use current name/MAC resolution; no identity
  result is cached to bypass validation.
* System dials and ICMP select the most-specific unscoped route from the verified
  live view. Ambiguous ties, stale indices or a missing route fail that request
  immediately. After an unscoped VPN default is withdrawn, public system fallback is held
  for a three-second settling interval; requests fail during the interval rather
  than waiting without a bound. Explicit physical rules remain available. A new
  VPN default can be used immediately; a stable physical default becomes usable
  after settling. No automatic teardown or SafeStart is needed for these updates.
  Scoped tunnel defaults never supplied system egress, so withdrawing or rebuilding
  them must not start that interval. The event path preserves `RTF_IFSCOPE` and
  distinguishes an explicit `RTM_DELETE` from ADD/CHANGE/REDIRECT. Only a decoded,
  known unscoped default deletion can arm the event-based timer; unknown scope
  defers to the independent snapshot comparison. Interface notifications and
  index changes invalidate affected interface lifetimes, not unrelated physical
  bindings. Capture verification and route-generation checks remain in force.
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
DNS servers and bound sockets. Fake answers are rejected. The selected policy's
block rules are also checked against the real destination candidates.
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

### Reconnecting flows after rule changes

A successful running Reload makes one pass over the bounded set of flows owned
by the forwarder. It rechecks the saved application/process attribution, original
destination, observed domain and protocol against the new policy. A different
route/block/system decision or first-choice interface reference closes the flow;
rule IDs, labels and backup-interface changes alone do not. Interface names and
hardware addresses use the same exact comparison as `InterfaceRef.Resolve`.
Unchanged flows retain their sockets and actual exits. The known real destination
of an established fake-IP flow is also checked against newly blocking rules.

Affected TCP endpoints are aborted with a reset before outbound sockets close;
UDP endpoints are unregistered so subsequent datagrams can create a new session.
The connection's original `Outcome` remains historical fact. Its normal `Ended`
record uses `EndReason: rules_changed` with an empty `Problem`; it does not claim
that the app has already reconnected successfully. A canceled unpublished flow
does not invent an unreachable history entry. Attribution, handshake, sniffing
and dialing in progress are conservatively canceled on Reload. Policy-generation
checks prevent late dial results or old workers from publishing under new rules.

TCP cancellation also covers endpoint registration during the synchronous
handshake: a tuple-specific check and the first observable SYN-ACK event close
the gap, without polling or aborting unrelated endpoints. If the output queue
drops that event, cancellation waits for a later SYN-ACK, handshake timeout or
shutdown; no outbound dial or application data can pass in the meantime. Socket
and telemetry work occur outside the flow-registry lock; all affected sockets
close before terminal log updates can wait for disk-backed telemetry.

UDP/QUIC reconstruction retains a bounded, tuple-specific name context rather
than adding an IP-wide SNI cache. The initial lifetime evidence and reconstruction
checks use fresh PCB snapshots. Only a uniquely matched, nonzero socket generation
and unchanged attribution permit inheriting the old name for a short-header tail.
A proven new socket does not inherit it. Unknown lifetimes are held conservatively.
A canceled first packet whose classification is still pending retains an explicit
unclassified marker: a nameless tail cannot pass simply because fresh ownership
is now known. A fresh complete Initial, or a complete ordinary DNS query to port
53, can classify that pending tuple again. DNS query names are never treated as
the resolver's own name. Valid protection contexts are not evicted under capacity
pressure, and failed fresh checks are rate-limited.
Current-worker, policy-generation and saved-context checks prevent a retired
worker from deleting a successor's protection.

This affects forwarder-owned sessions established after capture starts. Native
pre-capture passthrough, ICMP and the VPN-prefix ingress gate retain their existing
behavior. Scoped IPv4 TCP reset/reconnection and UDP/DNS same-source-port
replacement passed hardware acceptance at `54a2881`. Full capture, IPv6 and QUIC
reload boundaries are not implied by that result. The strict final comparison
was incomplete because an external cellular IPv6 prefix expired; independent
checks found no Pith residue. See the [acceptance record](ROOT-TEST.md#recorded-affected-flow-reload-result).

## Lifecycle and recovery

SafeStart resolves a TLS endpoint before capture (default `api.ipify.org:443`).
After activation it has ten seconds to complete a certificate-verified TLS
handshake with an ordinary native socket and prove that exact socket crossed the
tunnel. Its native leg deliberately has no interface binding. Activity can
therefore show a short TCP connection owned by `pithd` to that endpoint at Start:
it follows normal rules and has outcome `system` when none matches. The separate
forwarder socket binds the chosen egress interface and bypasses capture. This
single captured probe is expected, not evidence of a forwarding loop; binding or
exempting its native leg would defeat the end-to-end capture check. SafeStart
performs TLS verification, not an HTTP public-address query or a periodic probe.

The Network page's `Service.Probe` runs in the unprivileged App process, binds
the requested interface and, for an empty target, sends HTTPS public-address
requests for both address families. It does not invoke the helper. Distinguish
the two paths by the process and startup timing rather than the endpoint alone.

A SafeStart failure returns `ErrNetworkLost`, removes capture and closes every
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

Stop uses an independent cleanup context. A failed pre-close routing snapshot
does not authorize any explicit route deletion: close the owned utun descriptor
instead. Then retry complete post-close snapshots for at most two seconds, with
50 ms spacing, including brief kernel route-retirement lag. Each native RIB read
still has its own 250 ms bound. A transient read deadline is not permanently
cached as cleanup failure when device close and fresh verification succeed.
Exhausted verification, remaining owned routes, device/route command failures,
or journal errors remain failures, leave `EndedAt` unset and retain the journal.
Only matching owned routes are considered; a rebuilt external VPN is preserved.

Stop restores routes before waiting on relays or disk. TCP half-close is retained;
shutdown aborts incomplete handshakes and waits for all workers. Hot rule/scene
changes apply to new connections and close affected existing forwarder sessions
as described above. Changing the IPv6 capture preference restarts
capture, because it changes the actual route set; existing proxied connections
end in that case. Interface availability is refreshed by notifications.

Lifecycle steps are recorded in memory during teardown and atomically persisted
only after capture cleanup, before relay draining and again at completion.
`lifecycle.json` in the private helper data directory keeps at most 128 records
and 64 KiB across helper restarts (mode 0600); uninstall removes it with that
directory. Records contain timestamps, fixed phase names, elapsed milliseconds
and coarse error kinds only: no raw errors, addresses, interface names, process
paths or credentials. They distinguish observer/lease shutdown, capture snapshots,
route deletion ordinals, device/event closure, verification retries, relay/monitor
draining, telemetry, and fault rollback. Recording failures do not prevent routing
cleanup. Capture-close errors also publish their sanitized phase/error kind through
the existing `Status.RouteDiagnostics`, which the test harness already archives;
identifying a failing close step does not require reading a root-owned file.

The helper's private directory is `/Library/Application Support/Pith`. It contains
the capture journal, daily JSONL connection snapshots and atomic `traffic.json`.
`LogDays` controls connection history; zero keeps logs only in memory. Traffic
keeps ten minutes of seconds in memory, 24 hours of minutes, nine days of 15-minute
buckets and daily totals on disk. Samples align to `Since + k*Step`; weekly queries
use sub-hour history so local-calendar and DST grouping stays accurate.

## Helper IPC and development

The Unix socket is mode 0600 and checks `LOCAL_PEERCRED` for the installing UID or
root. Before sending any HTTP bytes, the client verifies the root-owned installed
file against its compiled SHA-256 and authenticates the executing peer's CDHash
using `LOCAL_PEERTOKEN` and `csops_audittoken`. These read-only ABIs exist in macOS
13's [Unix socket implementation](https://github.com/apple-oss-distributions/xnu/blob/xnu-8792.41.9/bsd/kern/uipc_usrreq.c)
and [process implementation](https://github.com/apple-oss-distributions/xnu/blob/xnu-8792.41.9/bsd/kern/kern_proc.c).
The kernel checks the PID generation; the client checks the token again after
querying the executable. There is no numeric-PID, self-reported-version or
unverified-manifest fallback. File hash caching includes inode, device, ownership,
permissions, size, modification and change timestamps; peer checks are never
cached across sockets.

IPC does not depend on a surviving app resource path: a moved or replaced bundle
cannot prevent Stop of an installed, executing helper that still matches the
compiled identity. An installed artifact with no answering peer is a failed
`helper_not_responding` status, never proof that routing stopped. Status retries
do not authorize changes; explicit repair can stage the verified installed bytes
and re-bootstrap them with administrator approval. Removal similarly uses a
verified staged cleanup executable and restores the previous registration if
cleanup fails after bootout, retaining the executable, plist and journal.

App versions do not authorize helper access. A different artifact/protocol requires
replacement, even on an app downgrade. Unverifiable file/peer identity returns a
failed status with `helper_untrusted`, no claimed version/scene, and no normal IPC.
The helper's display version is informational. Ad-hoc identity pins the bundled
artifact; it is not a publisher certificate or protection against compromised root.
HTTP/JSON requests are versioned and
bounded, with NDJSON events. Status/rates and connection updates are coalesced on
a one-second tick only while running. Each publication reaches an event subscriber
as one batch, so a tick's connection updates take one of its 128 slots however
many connections changed; a subscriber that leaves 128 batches unread is closed.
The client reopens a closed stream at once, and reports the helper as not
responding only if that fails or another stream closed in the previous three
seconds. See [helper protocol](../internal/helper/README.md).

For approved development tests the root helper accepts repeated `--capture CIDR`
flags, `--safe-target IP:443`, `--safe-server-name example.com`, `--data-dir` and
`--socket`. Scoped capture never expands to global DNS routes. Passing these flags
does not start routing: an authenticated Start is still required. Each scoped
capture stops automatically after five minutes even if its controller dies;
hot reload never extends that lease. **Root capture tests require administrator
approval and a recorded undo plan.** Unit tests use fake drivers, memory tunnels
and synthetic addresses;
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
The [root-free relay performance investigation](PERFORMANCE.md) measures
interface-query, packet-allocation and MTU candidates without changing production
behavior; its memory-stack ceilings are not native-utun throughput claims.
Scoped TCP acceptance does not establish full capture, IPv6, UDP/QUIC,
delegated WebKit/nsurlsessiond attribution, runtime VPN reconstruction, or the
500 Mbps throughput target. See the [next acceptance plan](NEXT-ACCEPTANCE.md).
Replies from unconnected native UDP listeners are not covered by the existing
socket preservation path; captured replies can lose their original source port.
Do not treat the scoped TCP smoke test as verification of native UDP services.
