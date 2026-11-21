# Backend architecture

Pith has one pure-Go module. The menu bar app and the routing helper run as
separate processes. The helper starts idle. An authenticated start request
enables capture. The app can also reconnect to a helper that kept routing.

| Component | Responsibility |
| --- | --- |
| `cmd/pith`, `internal/ui` | Menu bar, window and user configuration. The UI calls `api.Service`, implemented by `internal/client` or by `internal/mock` in mock mode. |
| `internal/client` | Interface inventory, rule checks, network tests, hardware sessions, proxy exits and authenticated helper IPC. Saves `config.json` atomically in the user's Pith Application Support directory. |
| `internal/proxy` and `internal/proxy/egress` | App-owned TCP listeners, device access, proxy protocols and interface-bound dialing. These ports work while the helper is stopped or absent. |
| `cmd/pithd`, `internal/helper`, `internal/engine` | Root launchd service, authenticated Unix socket and packet forwarding. The engine owns capture routes, flow attribution and routing telemetry. |
| `internal/hotspot`, `internal/router`, `internal/primary` | The app talks to the two supported gateways. The helper temporarily yields an interface's primary rank for router renewal. |
| `internal/mcp` | The app serves tools on a per-user Unix socket. Another invocation of the same executable, `Pith mcp`, connects a stdio MCP client to it. |

## Network tests

| Operation | Destination and result |
| --- | --- |
| `Service.Probe` with a target | Opens one TCP connection to the supplied `host:port` and reports the peer address and time to connect. **Reach a server** runs this through each connected interface. |
| `Service.Probe` with an empty target | Queries `api.ipify.org` over IPv4 and `api6.ipify.org` over IPv6 in parallel. Either public address makes the probe successful. Renewals, interface-exit lookups and the MCP `test_interface` tool use this path. |
| `Service.SpeedTest` | Measures latency and transfer rates against `speed.cloudflare.com`. The result includes the public IP and data center when Cloudflare supplies them. **Test All** queues one interface at a time. |

These tests run in the app. The dialer binds DNS and TCP sockets to the
specified interface. A probe retries an unanswered UDP DNS query after 400 ms
and gives each socket up to one second. If the interface's resolvers fail
within a two-second lookup budget, it can try eligible fallback resolvers from
the Mac's configuration on the same interface. A definitive name-not-found
answer is respected. The helper's [Safe start](#lifecycle-and-recovery) checks
capture with a separate TLS handshake.

The speed test prefers IPv4 and uses IPv6 for an IPv6-only interface. It sends
five `GET /__down?bytes=0` requests. The first has eight seconds to establish
the connection. The remaining rounds have two seconds each. Latency is the
shortest successful interval from writing the request to the first response
byte, less the reported `Server-Timing` durations. The first response supplies
the data center (`Colo` or the suffix of `CF-Ray`) and address (`CF-Meta-IP`).

A `GET /__down?bytes=25000000` times the download body. A `POST /__up` sends
8,000,000 bytes and times the upload through the response headers. A completed
upload longer than twice the measured latency deducts one round trip. Each
transfer has eight seconds. A deadline-limited transfer can supply a rate after
at least 1 MiB. A failed part has a zero rate and a reason in `Problem`. The
Cloudflare result stays separate from the dual-stack address record used by
proxy ports. [Speed test implementation](../internal/client/speedtest.go) · [Tests](../internal/client/speedtest_test.go)

## Renewing a hotspot's address

The app recognizes the Inseego 5G MiFi M2000 from the system interface label
(case-insensitive `m2000`). The Network page checks connected cellular
interfaces. Public status supplies the signal bars and network type. An
authenticated diagnostics request adds the carrier, band, signal and SNR.
The password is a keychain item under `io.github.kel077.pith.hotspot` keyed
by the adapter's hardware address.

Both gateway drivers reuse an interface-bound HTTP client. The client verifies
the interface name, hardware address, gateway and index before a request,
including requests on pooled connections, and limits redirects to the same
origin. The hotspot uses its local HTTP admin page. The router uses HTTPS.

`RenewHotspot` probes the old public addresses while the device reads its
network settings. It switches cellular data off and on and polls device status
every half second. It allows up to ten seconds to see the old connection drop
and a 90-second reconnect window. When the M2000 reports connection age, a
session older than the data-off event (allowing one second for rounding) is
ignored. Without an age it accepts the device's connected state. If a disable
might have taken effect and re-enabling fails or is canceled, cleanup has an
independent 15-second budget for up to two restore requests.

Both renewal paths then probe public addresses again, with a second between
attempts. They aim to find at least one address and as many address families
as the pre-change probe found. New attempts have a 25-second window, shortened
to at most three more seconds once an address is available. A probe already
underway has its own deadline. The best observed result can be partial or
empty. A provider can also return the same address. [Device tests](../internal/hotspot/hotspot_test.go) · [Client retest](../internal/client/hotspot.go)

## Renewing a router's address

`internal/router` recognizes the Verizon Router CHR30A-R by the model its web
admin returns before login. The client checks eligible connected Ethernet and
Wi-Fi interfaces with a hardware address and a private IPv4 gateway (loopback
is also accepted). It pins the observed TLS public key. The keychain service
`io.github.kel077.pith.router` uses `sha256:` plus the hex hash of that key as
the account. A changed key requires identification and its own credential.
The driver uses the router's Basic login, session cookie and XSRF token. It
reads the broadband WAN lease through the web admin's network-connections API.

1. `RenewRouter` reads the WAN state and probes the current interface and
   eligible standby interfaces in parallel. A standby must be up, support
   physical proxy egress, have a router and pass the internet probe.
2. The helper's `POST /v1/yield` asks `internal/primary` to add
   `PrimaryRank=Never` to the service's `State:/Network/Service/<id>` using a
   temporary `scutil` key. It checks for another service with a router and
   refuses to overwrite an existing state entity. Once macOS selects another
   IPv4 primary, the helper names it on the open response. The app proceeds
   only if that primary passed preflight. configd drops the key with the
   session, on disconnect, helper exit or the ten-minute yield limit.
   Scoped routes and the interface's local addresses still reach the router.
3. The app suspends direct proxies on the renewing interface and attempts to
   reload the running scene. Rules with other exits lose that interface for
   the duration. A rule with only that exit uses the standby if fallback is
   allowed. A strict rule keeps its sole reference. Scene-based proxy ports
   skip suspended interfaces on new dials. System network preferences are
   never written.
4. The driver releases the WAN IPv4 lease and requests renewal after a pause.
   It tries pauses of 5, 20 and 60 seconds while Verizon returns the original
   WAN address. Each release interrupts internet access behind the router.
   IPv6 leases are outside this operation. After a release, cleanup can ask
   for a lease twice within an independent 20-second budget. A normal renewal
   waits for reconnection for up to 90 seconds per attempt. The client can
   wait another two minutes after an error.
5. The client probes the interface again, resumes its direct proxies, tries
   to restore the saved scene and ends the yield. The configuration directory
   holds `.draining` for restoring rules and `.router-released` for a potentially
   missing WAN lease. On a later configuration load, recovery checks the
   recorded adapter and asks a recognized router without a lease to renew.
   Client shutdown allows up to 25 seconds for outstanding renewals.

The rank is read by configd's IPMonitor. Temporary dynamic-store keys belong
to the session that created them. [IPMonitor election](https://github.com/apple-oss-distributions/configd/blob/main/Plugins/IPMonitor/ip_plugin.c) · [Session cleanup](https://github.com/apple-oss-distributions/configd/blob/main/configd.tproj/_configclose.c)

See the [client renewal](../internal/client/router.go), [primary election implementation](../internal/primary/primary.go) and [hardware results](ROOT-TEST.md#router-renewal-and-primary-yield).

## Local proxy listeners

`internal/proxy.Manager` runs in the app. `Config.Proxies` accepts up to 32
unique ports in 1024–65535, including disabled entries. Exactly one of
`Via`, `Scene` and `Upstream` chooses each port's exit. The first successful
`LoadConfig` activates enabled listeners. After a save commits to disk, listener
changes take effect even if that caller cancels. Replacing an exit, port or
sharing mode closes that listener's streams. Toggling IP-change permission or
editing a display label preserves streams, start time and counters. Saving again
retries a failed bind (`port_in_use`). App exit closes all listeners, regardless
of the helper's `KeepRouting` setting.

The app publishes proxy and device counters alongside authenticated helper
status. `listening` reports a successful local bind. Open streams and relayed
bytes belong to the listener. The helper's Activity log and traffic history
describe connections observed by capture. A system-routed leg from the app can
itself go through capture.

### Interface exits

`internal/proxy/egress` resolves a saved `InterfaceRef`, pins the current
name, hardware address and index, and binds sockets with `IP_BOUND_IF` or
`IPV6_BOUND_IF`. Supported physical types are Ethernet, Wi-Fi, cellular and
Thunderbolt, subject to `Interface.CanProxy`. A missing or down adapter stays
configured. New streams can resume when it returns. An established stream
keeps its binding. Identity checks around dialing and stream I/O prevent
index reuse from redirecting it. Interface metadata is cached for at most
one second.

DNS sends A and AAAA queries on the pinned interface. The resolver tries its
own servers, then eligible servers from `/etc/resolv.conf`. It excludes
private, carrier-shared and link-local addresses from the fallback list, as
well as IPv6 servers on a local interface's /64. A definitive negative answer
ends that query. Truncation on UDP retries over TCP on the same server. Limits
are three servers of each kind, four seconds for resolution, 750 ms per attempt
and one retransmission after 400 ms. Replies have limits of 16 KiB, 128 records,
eight CNAME steps and eight candidate addresses. Literal destinations do not
need DNS. [Resolver](../internal/proxy/egress/dns.go) · [DNS tests](../internal/proxy/egress/dns_test.go)

### Ports by a scene's rules

The client compiles each scene named by a port with `internal/rules`.
New proxy connections match that scene as TCP, independently of the scene
routing the Mac. Rules restricted to an app cannot match. A block refuses the
connection. A route tries available interfaces in order for up to three seconds
each. Unmatched connections and permitted fallbacks use an unbound system
dial. Editing rules affects the next connection. Deleting a scene redirects
its configured ports to the selected scene.

With domain-only rules, the selected exit resolves the name supplied by the
client. If an enabled rule without an app filter names an IP address or CIDR,
the client looks up the name once with the system resolver (up to two seconds),
unless already blocked by name. It matches the first eight addresses together
with the name. Rule-selected candidates are tried first, then unmatched ones.
Blocked addresses are excluded. Only those literal candidates are dialed,
staggered by up to 250 ms while earlier attempts are pending. [Route selection](../internal/client/route.go) · [Tests](../internal/client/route_test.go)

### Sharing a port with other devices

Listeners bind IPv4 loopback by default. A shared listener binds all IPv4
addresses. A loopback peer or one whose source address equals the address it
connected to is local. Other peers need a matching `Config.Devices` entry.
The list is global across shared ports and accepts up to 64 IPv4 addresses or
CIDRs with prefix length 8–32. Pith authenticates no device passwords.
An unlisted peer can receive SOCKS5 method `0xFF` or HTTP `403`. Up to eight
such replies run at once with a two-second deadline each.

Device status tracks up to 64 recently seen addresses, their last connection,
open streams and relayed bytes. At capacity it drops an inactive refused
entry first, then the least recently seen inactive allowed entry. Removing
coverage for a device closes its sessions. Turning off the last enabled shared
port clears observed devices. Configured entries persist.

Each port rejects local literal destinations, including loopback and addresses
on any of the Mac's interfaces, and names reserved for loopback. Direct and
scene exits also check resolved addresses against `net.InterfaceAddrs` before
dialing and check the connected peer before relaying. Unspecified, multicast
and IPv4 zero-network destinations are refused. Address literals supplied as
names take the same checks. Other LAN hosts remain reachable. For a name sent
to an upstream, the upstream owns resolution and destination checks.
[Destination checks](../internal/proxy/destination.go) · [Sharing tests](../internal/proxy/share_test.go)

### Upstream proxies

The app opens an ordinary macOS-routed TCP connection to the configured
`host:port`, negotiates SOCKS5 without authentication, then forwards the
original target. The upstream resolves names and its SOCKS reply supplies the
bound address when available. A shared port refuses a local upstream at save
time for loopback hosts or at connect time if the peer is loopback or matches
the socket's own source address. A private port can use another local proxy.

Dial and greeting have a three-second budget. `upstream_unreachable` and
`upstream_refused` describe this Mac's connection to the upstream. Failure to
reach an individual destination does not mark that proxy offline. While status
is being read, the manager polls an enabled upstream at most once every ten
seconds. A Pith upstream can answer with the network and addresses of its own
exit. [SOCKS negotiation](../internal/proxy/upstream.go) · [Upstream tests](../internal/proxy/upstream_test.go)

### HTTP on the same port

The first byte selects SOCKS5 or HTTP. SOCKS5 supports unauthenticated
CONNECT to an IPv4 address, IPv6 address or domain. BIND and UDP ASSOCIATE
return `0x07`. HTTP CONNECT takes `host:port` and opens a byte tunnel.
An absolute `http://` request is forwarded in origin form with
`Connection: close`, one request per connection. The proxy strips standard
hop-by-hop fields, fields named by Connection and request trailers. It preserves
an absent User-Agent and can relay the response while the request body is still
arriving. HTTP headers have a 32 KiB limit. Malformed targets get `400`. A
denied target gets `403`, host-unreachable gets `504`, and other dial failures
get `502`. [HTTP implementation](../internal/proxy/http.go) · [Tests](../internal/proxy/http_test.go)

A handshake has five seconds. The ten-second connect budget starts at accept.
The manager allows 1,024 stream sessions per port and 2,048 overall, including
handshakes. When full, it can replace the least recently active established
stream without a write in progress. If none qualifies, the new connection is
refused. Tunnel relays preserve TCP half-close. Proxy byte counters exclude
negotiation, DNS and requests to the port itself.

### Requests to a port

An origin-form HTTP request goes to the port itself. Both endpoints return
JSON on success and `{"error":{"code":"…","detail":"…"}}` on a handler error.
Times are UTC. Replies set `Cache-Control: no-store` and `Connection: close`.

| Request | Success (`200`) |
| --- | --- |
| `GET /` | `pith` (currently 1), `port`, `exit` (`interface`, `scene` or `upstream`), `online` and `canChangeIP`. Optional fields include `interface`, `publicIPv4`, `publicIPv6`, `at`, `changing` and nested `upstream`. Online describes the exit's availability. A scene is online while its port listens and has no single public address. |
| `POST /change-ip` | `port`, `seconds` and any observed `beforeIPv4`, `beforeIPv6`, `publicIPv4`, `publicIPv6` and `at`. An unchanged or missing address is possible. The optional body is discarded, up to 4 KiB. |

Any admitted client may read status. A POST requires `Proxy.ChangeIP` on each
port in the chain. Scene ports cannot enable it. `canChangeIP` means that a
request would be attempted. Device credentials and a working standby for a
router are checked during the operation. The app's own `ChangeProxyIP` action
calls the manager directly and needs only the upstream's permission, if any.

| HTTP status | Meaning for requests to the port |
| --- | --- |
| `400` | Invalid `max-age`, invalid `Pith-Hops` or an incomplete POST body (`invalid_config`). |
| `403` | A disabled IP-change permission, disallowed Host or browser request headers (`not_allowed`). Unlisted devices can also be refused before endpoint dispatch. |
| `404` / `405` | Unknown path or wrong method (`unsupported`). A method error includes `Allow`. |
| `413` | POST body exceeds 4 KiB (`invalid_config`). |
| `501` | The exit cannot renew its IP (`unsupported`), including an ordinary SOCKS5 upstream. |
| `502` | Renewal, upstream or reply failure. If the caller's wait ended, the change may still be running. |
| `503` | Another interface on this Mac is renewing (`busy`), or the port request limits are full (`internal`). |
| `508` | A request would exceed the limit of four upstream forwards (`internal`). |

The Host can be an IP literal, `localhost`, a single-label name, a name under
`.local`, or empty for HTTP/1.0. Any Origin field or a Sec-Fetch-Site value
other than `none` is refused. The port supplies no CORS grants. These checks
apply to the port's own endpoints. Shared-device admission also applies to
ordinary forwarding.

**Address cache.** The empty-target ipify probe feeds a per-interface record.
The first GET can start the public-address lookup and device identification in
parallel. A plain GET reuses complete results. If an expected family is missing
or the lookup failed, later GETs retry after 30 seconds, doubling up to ten
minutes. A single nonnegative `max-age` in seconds requests a freshness limit.
Changing interface identity, addresses, availability or gateway invalidates the
record. Late results cannot update a different interface snapshot. A lookup
started by a port can finish after its reader leaves. Its next caller benefits
from the result. Unsupported devices are cached, while transient identification
errors can be retried after a minute.

**Concurrent calls.** The manager allows 64 requests to ports across all
listeners. Up to 48 may wait for IP changes, leaving 16 slots available for
other requests. The body must arrive within the connect deadline before a POST
takes a waiting slot. A GET uses the connect deadline. The POST wait can last
ten minutes. Requests to the same interface join one renewal, including
requests from the Network page. A port-initiated change runs under the app's
lifetime (up to three minutes for a hotspot or eight for a router). Disconnecting
ends that caller's wait. A Network-page renewal uses its caller's context.

**Upstream answers.** Requests go directly to the upstream port with its
connected address as Host. Each forward increments `Pith-Hops`. The reader
limits the entire upstream response to 64 KiB and validates the version, port,
exit type and nesting depth. It truncates long interface labels to 256 UTF-8
bytes and discards malformed address fields. A Pith response nests under
`upstream`. A recognized Pith error or a silent timeout after a known response
preserves the last good information. An ordinary SOCKS5 greeting can establish
availability when an upstream does not speak Pith HTTP. Only a recognized Pith
upstream receives the POST. [Wire types](../internal/api/proxy.go) · [Request handling](../internal/proxy/port.go) · [Client integration tests](../internal/client/exit_port_test.go)

## Tools for agents

`Pith mcp` speaks newline-delimited JSON-RPC on standard input and output.
It answers `initialize`, `ping` and `tools/list` locally and forwards
`tools/call` to the open app. It skips the GUI and single-instance lock.
Discovery works with the app closed. A tool call then reports that Pith
isn't running. The server accepts protocol revisions 2024-11-05, 2025-03-26,
2025-06-18 and 2025-11-25 and offers the latest for an unknown revision.

| Task | Tool names |
| --- | --- |
| Inspect Pith and the Mac | `status`, `get_config`, `list_interfaces`, `list_apps` |
| Inspect routing and traffic | `check`, `list_connections`, `traffic` |
| Test an interface or read its gateway | `test_interface`, `speed_test`, `gateway_status` |
| Control routing | `start_routing`, `stop_routing` |
| Edit rules | `add_rule`, `update_rule` (including `position`), `remove_rule` |
| Manage scenes | `add_scene` (optionally with `copy_of`), `rename_scene`, `remove_scene` |
| Configure ports and devices | `set_proxy`, `remove_proxy`, `allow_device`, `remove_device` |
| Renew an exit | `change_ip` with either `interface` or `port` |

`list_interfaces` shows connected links and named inactive non-VPN services.
It reads the backend inventory, which can include Pith's own active tunnel.
The window filters that tunnel using the helper's capture identity.
Tools accept interfaces by BSD name, alias or label, apps by path, bundle ID
or name, and scenes by ID or name. Unknown arguments fail. The tool definitions
supply `readOnlyHint` and `destructiveHint`. Configuration edits go through
`ui.Agent` on the main thread and wait for the UI's saver. Starting and stopping
use the app's routing state. Helper installation, entering device passwords,
changing general settings and clearing the connection log are UI actions.
[Tool definitions](../internal/mcp/tools.go) · [Tool tests](../internal/mcp/tools_test.go)

`change_ip` reports the addresses it observed before and after. Its `changed`
flag compares both pairs of fields, including missing values, when the final
probe found an address. For a partial result, compare the old and new values
of the same family before concluding that its address changed.

The app listens on `$HOME/Library/Application Support/Pith/mcp.sock` with mode
0600 and checks the connecting user's kernel UID. Each call opens its own Unix
connection. The listener accepts at most 32 simultaneous calls. Cancellation,
EOF on stdio or a disconnected call ends its wait. Neither MCP process opens a
TCP listener. `PITH_MCP_SOCKET` overrides the socket for development. A mock app
listens only with this override. [Socket implementation](../internal/mcp/socket.go) · [Running the mock](BUILDING.md#mock-interface-and-mcp)

For clients configured with JSON, the server entry is
`{"command":"/Applications/Pith.app/Contents/MacOS/Pith","args":["mcp"]}`.
For a remote Mac, point the client at `ssh` with that Mac, executable path and
`mcp` as arguments. The app must be running as the SSH user on that Mac.

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
production planning and frozen test profiles. Capture leaves the system default
route in place. Router renewal can separately ask macOS to elect another primary.

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

The app runs separate [network tests](#network-tests) for reachability, speed
and public-address lookup. Those sockets bind to the interface being tested.

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
The interface watcher makes up to three attempts at its initial read. A later
read failure preserves both the stream and its last snapshot. It retries after
a change or on a timer starting at one second and doubling to 30 seconds.
The client reconnects a lost stream immediately and spaces repeated attempts
at least three seconds apart. Stream setup and the separate fallback status
request each have a five-second budget. An answering helper keeps its normal
status. The installed launchd job sets `ProcessType` to `Interactive` for
the traffic it forwards. [Helper protocol](../internal/helper/README.md)

For approved development tests the root helper accepts repeated `--capture CIDR`
flags, `--safe-target IP:443`, `--safe-server-name example.com`, `--data-dir` and
`--socket`. Scoped capture never expands to global DNS routes. Passing these flags
does not start routing: an authenticated Start is still required. Each scoped
capture stops automatically after five minutes even if its controller dies;
hot reload never extends that lease. **Root capture tests require administrator
approval and a recorded undo plan.** Ordinary tests use fake drivers, memory
tunnels, synthetic addresses, loopback sockets or read-only kernel metadata.
Separate opt-in client tests can contact the internet, temporarily change the
primary interface or renew a real router. [Hardware test scope](ROOT-TEST.md#router-renewal-and-primary-yield)

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
