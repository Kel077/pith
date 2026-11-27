# Backend architecture

Pith has one pure-Go module and no cgo. `cmd/pith` is the unprivileged MyGo
app; `internal/client` implements `api.Service`. `cmd/pithd` is a root launchd
helper. It always starts idle, even after a crash or reboot. Only an authenticated
request starts routing. The app's rules/configuration are atomically saved with
mode 0600; interface labels remain the System Settings names, with aliases
applied by the UI.

| Component | Responsibility |
| --- | --- |
| `cmd/pith`, `internal/ui` | Menu bar, window and user configuration. The UI calls `api.Service`, implemented by `internal/client` or by `internal/mock` in mock mode. |
| `internal/client` | Interface inventory, rule checks, network tests, hardware sessions, proxy exits and authenticated helper IPC. Saves `config.json` atomically in the user's Pith Application Support directory. |
| `internal/proxy` and `internal/proxy/egress` | App-owned TCP listeners, device access, proxy protocols and interface-bound dialing. These ports work while the helper is stopped or absent. |
| `cmd/pithd`, `internal/helper`, `internal/engine` | Root launchd service, authenticated Unix socket and packet forwarding. The engine owns capture routes, flow attribution and routing telemetry. |
| `internal/hotspot`, `internal/router`, `internal/primary` | The app controls cellular modems and routers. The helper temporarily yields an interface's primary rank for Verizon router renewal. |
| `internal/mcp` | The app serves tools on a per-user Unix socket. Another invocation of the same executable, `Pith mcp`, connects a stdio MCP client to it. |

## Network tests

| Operation | Destination and result |
| --- | --- |
| `Service.Probe` with a target | Opens one TCP connection to the supplied `host:port` and reports the peer address and time to connect. **Reach a server** runs this through each connected interface. |
| `Service.Probe` with an empty target | Queries `api.ipify.org` over IPv4 and `api6.ipify.org` over IPv6 in parallel. Either public address makes the probe successful. Renewals, interface-exit lookups and the MCP `test_interface` tool use this path. |
| `Service.SpeedTest` | Measures latency and transfer rates against `speed.cloudflare.com`. Cloudflare sends the test to an edge near that network and reports the public IP it saw and the data center that answered. **Test All** queues one interface at a time. |

These tests run in the app. The dialer binds DNS and TCP sockets to the
specified interface. A probe retries an unanswered UDP DNS query after 400 ms
and gives each socket up to one second. If the interface's resolvers fail within
a two-second lookup budget, it can try eligible fallback resolvers from the
Mac's configuration on the same interface. Link-local IPv6 servers take the
interface as their zone. Retransmissions use the same socket, so a late reply
still counts. The retry interval doubles but the one-second socket budget
normally permits just one resend. Go's resolver would otherwise wait five
seconds for a reply. The M2000 can drop the first query after a quiet period and
answer the next one at once. A definitive name-not-found answer is respected.
The helper's [Safe start](#lifecycle-and-recovery) checks capture with a
separate TLS handshake.

The speed test prefers IPv4 and uses IPv6 for an IPv6-only interface. It reuses
the connection opened for the first latency request when possible. It sends five
`GET /__down?bytes=0` requests. The first has eight seconds to establish the
connection. The remaining rounds have two seconds each. Latency is the shortest
successful interval from writing the request to the first response byte, less
the reported `Server-Timing` durations. The first response supplies the data
center (`colo` or the suffix of `CF-RAY`) and address (`cf-meta-ip`).

A `GET /__down?bytes=25000000` times the download body. A `POST /__up` sends
8,000,000 bytes of zeros and times the upload through the response headers. A
completed upload longer than twice the measured latency deducts one round trip.
Each transfer has eight seconds. A deadline-limited transfer can supply a rate
after at least 1 MiB. A failed part has a zero rate and a reason in `Problem`.
Trials of the endpoint found that a 100 MB download was refused and a 10 MB
upload went unanswered. The Cloudflare result stays separate from the dual-stack
address record used by proxy ports.
[Speed test implementation](../internal/client/speedtest.go) ·
[Tests](../internal/client/speedtest_test.go)

## Renewing a hotspot's address

The unprivileged client recognizes the USB Inseego 5G MiFi M2000 by its system
interface label (case-insensitive `m2000`). On a connected Wi-Fi interface with
a private IPv4 gateway, it checks for the Inseego status protocol at
`/srv/status` through that interface. Discovery shares one anonymous request
across readers. The cache follows the adapter, kernel index, local IPv4
addresses, gateway and its ARP entry. A definite negative stays cached for
that snapshot, and transient errors can be retried after one minute. The
Network page shows signal bars and the network type before login.
The authenticated diagnostics page adds carrier, band, signal strength and SNR.

Device requests use the selected interface, refresh the binding for every new
socket, recheck the name/MAC/router before requests (including pooled sockets),
and reject redirects outside the original origin. The client also rechecks the
bound interface's kernel index. Cached sessions have at most two idle
connections and a 30-second idle timeout. The hotspot uses its local HTTP admin
page, the router HTTPS. Credentials require a hardware identity and live under
the app's keychain service, `io.github.kel077.pith.hotspot`. USB uses the
adapter's hardware address. On Wi-Fi the account includes both the adapter
and the gateway hardware addresses, and requests recheck the gateway's ARP
entry before using that session. Writing an item uses one quoted command
on `security -i` stdin; no password appears in the process arguments,
diagnostics, or application files. The command-input format is bounded and
rejects embedded line breaks/NULs.

Hotspot renewal restores cellular data with an independent 15-second bound if
cancellation or a lost response follows an attempted disable. At most two
restore requests are made; failures remain errors and are not reported as
successful renewals. This is a device operation, independent of helper capture:
changes to its public addresses, LAN addresses or DNS do not by themselves stop
routing. The public-address test before a renewal runs while the device's
network settings are read; data goes off only once both finish. The device is
polled every half second, with up to ten seconds to see the old connection drop
and a 90-second window for reconnection. Once data is on again, the renewal
waits for the device to show a connection made since data went off, not just
any: a test through the old one would find the old addresses. Its status tells
how long the connection has lasted, in whole seconds; a device that doesn't tell
is taken at its word. The client then probes with a second between attempts. It
looks for at least one address and as many families as it found before. It
starts new probes within a 25-second window, shortened to at most three more
seconds after one family answers. An in-flight probe has its own deadline. The
best result may be partial or empty. A reconnected M2000 refuses DNS queries for
about ten seconds while its DNS server restarts; the probes can then ask the
Mac's public DNS servers through the hotspot.
[Device tests](../internal/hotspot/hotspot_test.go) ·
[Client retest](../internal/client/hotspot.go)

### Quectel RM520N-GL over RMNet

SystemConfiguration keeps the hardware port name alongside the editable
service name. The RM520N-GL port is cellular even when the service has been
renamed or has only IPv6. The unprivileged client invokes
`/Applications/RMNet.app/Contents/MacOS/RMNet`. It reads the QMI data state
from `status --json` and the operator, serving-cell signal and component
carriers from `network --json`. Repeated bands are separate components.
The modem can be registered and idle between packets while its data call is up.

RMNet owns the QMI data calls. Its CLI exposes a radio switch, and the redial
performed during an APN edit uses that same switch. Pith checks that the
radio and data are on, probes the interface, then requests `radio off` and
`radio on`. Status reads and the radio cycle take turns on the AT port.
RMNet selects the first USB device matching `2c7c:0801`, so Pith checks for
a single match and rechecks its identity before each switch. A lost reply
or cancellation after the off request triggers a bounded independent attempt
to restore the radio. The hardware address identifies the interface when it
returns, including under a new BSD name.

Once the driver has a data call again, the client retests both public address
families through that interface. On an IPv6-only carrier with CLAT and NAT64,
the HTTPS IPv4 result is the carrier's translated public address. The Network
page and MCP compare only addresses seen on both sides of a renewal. RMNet
renewal time includes the public-address test after reconnection.
[Modem driver](../internal/hotspot/rmnet.go) · [Renewal tests](../internal/client/rmnet_test.go)

## Renewing a router's address

### Verizon Router CHR30A-R

A router's admin also stays in the client, over the same interface-bound HTTP
client as a hotspot's. The client identifies the router behind an Ethernet or
Wi-Fi interface by the model its web admin names without a login; only Verizon's
CHR30A-R is tested, and other models are left alone. The link must be up, with a
hardware address and a private IPv4 gateway (loopback is also accepted). The web
admin presents a self-signed certificate. The client pins the SHA-256 of the
public key it first saw, and keeps the admin password under that key, as
`sha256:` and its hex, in the keychain service `io.github.kel077.pith.router`. A
device with another key at the router's address is locked, and never sent the
password. Like the router's own page, the client logs in with HTTP Basic, then
sends the session cookie and an XSRF token on authenticated requests. It reads
the broadband connection from `/CRSP/getNetworkConnections` and sets the TR-181
parameters of the lease's Release and Renew buttons,
`Device.DHCPv4.Client.[Alias == 'wan'].X_PRPL-COM_Release` and
`Device.DHCPv4.Client.[Alias == 'wan'].Renew`. A password refused during a
device session is not retried in that session, and a router that refuses logins
for a while gets none.

`RenewRouter` reads the router's WAN state and tests its public addresses
and the possible standby interfaces in parallel. Standbys must be up, have
a router, support physical proxy egress and pass an internet probe. When the
WAN is connected and a standby works, the helper's `POST /v1/yield` calls
`internal/primary.Yield`. This makes the router's interface ineligible to be
macOS's primary for as long as the request stays open, up to ten minutes.

configd's IPMonitor ranks services by the `PrimaryRank` of their `Setup:` and
`State:` entities. The helper adds `State:/Network/Service/<id>` with
`PrimaryRank=Never` as a temporary key of a `scutil` session.
[Primary election in `get_rank_changes`](https://github.com/apple-oss-distributions/configd/blob/main/Plugins/IPMonitor/ip_plugin.c)

Closing the HTTP response cancels the yield. The helper closes the pipe to
`scutil`'s standard input, and EOF makes `scutil` exit. Killing the helper also
closes its end of that pipe. If `scutil` itself exits, its session ends directly.
configd removes the session's temporary keys when it closes, so in every case
the previous rank returns. Nothing is written to system preferences. The
interface keeps its addresses and scoped routes, so the app can still reach
the router and sockets bound to the interface stay on it. [Session cleanup in `_configclose.c`](https://github.com/apple-oss-distributions/configd/blob/main/configd.tproj/_configclose.c)

The yield changes nothing when no other service has a
router or the entity exists already. An NDJSON reply names the IPv4 primary
macOS actually chose. The client proceeds to release the lease only if that
interface passed the internet probe.

Meanwhile the interface counts as down. Its local proxies answer every CONNECT
that the network is unreachable and close their streams, rather than wait for
timeouts or use another interface. Scene-based ports skip the suspended
interface for new dials. The client also reloads the running helper scene with
that interface skipped. A rule tries its remaining interfaces. With none left,
it uses the new primary when fallback is allowed. A strict rule retains its
sole interface. The `.draining` file in the configuration directory lets the
client restore the saved scene on a later load if Pith quit in the meantime.
If that file can't be written or the helper refuses the reload, the client
undoes the drain and ends the renewal before the router is asked anything:
rules that still sent connections to the interface would fail them while it
has no address. The same goes for the router's own mark, below.

The router gives its address up, and is asked for one again after a pause of 5,
20, then 60 seconds, until the address differs or three tries are done: asked
again at once, a provider may give the same address back. Once asked to give it
up, the driver attempts to get a lease back even if the request was canceled. It
can send up to two restore requests within an independent 20-second bound. A
normal try waits up to 90 seconds for reconnection and the client can wait two
more minutes after an error. On quit, the client allows up to 25 seconds for
outstanding renewals. Before the first release the client writes
`.router-released`, and ends the renewal when it can't. Should Pith end before
it asks, the client looks for that file on the next configuration load. It
checks the recorded
adapter before asking a recognized router without a lease to renew: the router
stays without an address until it is asked, and so does everything behind it.
After a completed cycle the client probes the interface again as it does for a
hotspot, resumes the proxies, attempts to restore the saved scene and ends the
yield. Connections through the old address end with it; apps that follow the
network have moved to the new primary by then. Only the IPv4 lease is renewed.
Pith leaves the router's delegated IPv6 prefix alone.

[Client and cleanup](../internal/client/router.go) ·
[Primary election](../internal/primary/primary.go) ·
[Hardware results and timings](ROOT-TEST.md#router-renewal-and-primary-yield)

### Upstream OpenWrt routers

The Network card's terminal button and MCP's `set_openwrt_router` edit the
same saved settings. An explicit configuration selects that upstream router
for the interface, including when the immediate LAN gateway is another device.
The optional `openWrt` entry in `config.json` has this form:

```json
{
  "openWrt": [
    {
      "via": {"name": "eth-demo"},
      "host": "router.example.com",
      "user": "root",
      "identityFile": "~/.ssh/example_openwrt",
      "command": "/usr/sbin/wan-rotate-ip"
    }
  ]
}
```

Here `via.name` is the interface's BSD name. Pith can also save its hardware
address, so it follows the same adapter after a rename. The host can be an
OpenSSH alias. Empty `user` and `identityFile` use OpenSSH's settings and keys.
An empty `command` selects the path shown above. The app uses the system
OpenSSH client in batch mode with a previously trusted host key and key
authentication. Its direct connection uses a source address from the selected
interface. Standard SSH aliases can provide the hostname, port and key file.
[OpenSSH client configuration](https://man.openbsd.org/ssh_config)

The router's script supplies one JSON object on stdout per invocation.

| Argument | Reply fields |
| --- | --- |
| `--status` | `ok`, `router`, `wan_up`, `ipv4`, and optional `ipv6_prefix`, `mwan3_wan`, `mwan3_usbwan`. |
| `--rotate` | `ok`, `old_mac`, `new_mac`, `mac_changed`, `old_ipv4`, `new_ipv4`, and optional `old_ipv6_prefix`, `new_ipv6_prefix`, `mwan3_wan`, `mwan3_usbwan`, `elapsed_sec`. |

A successful rotation confirms a different locally administered unicast MAC
and a WAN IPv4 lease. IPv6 delegation can be reported as the prefix address
alone or in CIDR form. An error reply sets `ok: false` and an `error` message.
The script owns mutual exclusion, changing the WAN MAC, lease release and
reacquisition, and WAN recovery. A configured router can maintain LAN service
through its WAN backup. This path leaves primary-interface selection to macOS
and keeps Pith's proxy listeners active. Traffic continuity depends on the
router's failover and individual connections.

Pith checks for an active WAN lease before the rotation. It runs `--rotate`
once per request with a 90-second SSH deadline. If the reply reports a lease
with the monitored WAN still offline, Pith checks its status for up to ten more
seconds before retesting the Mac's exit. A lost SSH reply can follow a change
that already happened. The next status read and public probe can report the
current state. Pith invalidates old port addresses across the operation, and a
bounded public-address retest fills the cache with the addresses that reply
through the interface. The `wan` result records the router's lease and prefix
changes, MAC change confirmation, failover state and script time. Port and MCP
results also include the public addresses observed from the Mac and overall
elapsed time.
[SSH driver](../internal/router/openwrt.go) · [Client integration](../internal/client/openwrt.go)

## Local proxy listeners

`internal/proxy` runs inside the unprivileged App process. Each configured port
binds TCP on `127.0.0.1`, or on every IPv4 address when it is shared, and goes
out through one physical interface, through an upstream proxy or by a scene's
rules; it needs neither an installed/trusted helper nor active TUN capture.
Exactly one of `Via`, `Scene` and `Upstream` selects the exit. `Config.Proxies`
is independent of the scene that routes, if any. Empty configuration opens no
ports. The service implements the no-auth CONNECT subset of
[RFC 1928](https://www.rfc-editor.org/rfc/rfc1928.html), with IPv4, IPv6 and
ASCII/punycode domain requests; BIND and UDP ASSOCIATE return `0x07`. For a
direct connection a successful reply contains the outbound socket's local
address and port. A chained port uses the bound address the upstream reported.
The parser bounds every field, validates versions/reserved bytes and closes
malformed requests. There is no authentication credential store: a shared port
tells devices by their addresses, as described
[below](#sharing-a-port-with-other-devices).

At most 32 unique configured ports, each in 1024--65535, are accepted, including
disabled entries. `Interface.CanProxy` is shared by the interface and backend:
Ethernet, Wi-Fi, cellular and Thunderbolt remain selectable while down, but loopback,
VPN/TUN, bridge and peer interfaces are excluded. `SaveConfig` rejects a currently
resolved unsupported exit; a missing or down physical adapter remains valid saved
intent and reports `interface_unavailable`. A usable DNS configuration is required
for domain requests; literal addresses can still work without it.

The initial handshake has five seconds; CONNECT setup has ten seconds total from
accept. There are at most 1024 sessions per port and 2048 overall, including
handshakes: a browser opens a stream for every host a page loads from and keeps
idle ones for reuse, and a Chromium-based browser held about 120 through one
port for a single news page. When a port is full, an established stream on that port gives up its slot.
When only the total limit is reached, the manager chooses across all ports.
It closes the quietest stream with no write in progress using an ordinary
TCP close, without forcing a reset. Streams in their handshake or with a write in
progress stay open; when no other stream is left, the new connection is refused.
Relay preserves TCP half-close and records only successfully written stream
bytes, excluding SOCKS negotiation and DNS. `Status.Proxies` includes disabled
entries in configuration order; its `Since` and counters belong to one listener
instance and reset on replacement. Listening means the local bind succeeded, not
that the internet is reachable. Target-specific errors do not become listener
errors. A device that isn't listed takes none of these sessions.

`client.New` is idle. The first successful `LoadConfig` activates enabled local
listeners; a canceled first activation retracts partial listeners. Valid saved
changes take effect after atomic rename, even if the caller then cancels. Removing,
disabling or changing a port's exit closes its pending/established streams before
the replacement can forward; unrelated listeners and label-only edits stay open.
A failed bind remains visible as `port_in_use` and is retryable by saving again.
Proxy-only configuration changes do not reload TUN rules. A previously failed
routing apply remains pending, so disk equality cannot turn its retry into a
false success. Explicit identical saves still retry pending backend work.

Subscriptions combine authenticated helper events with local proxy and device snapshots;
missing or unverified helper status keeps its existing failure meaning. Local
counter updates continue without a helper stream. `client.Close` cancels setup,
closes all local listeners/streams and joins subscriptions without stopping TUN.
App exit closes every proxy socket regardless of KeepRouting; no proxy daemon is
installed. Protocol, DNS, lifecycle, cancellation and interface-reuse tests use
loopback or injected state. For the wired and cellular tests with capture on and off, see the
[hardware record](ROOT-TEST.md#recorded-local-socks5-result).

### Interface exits

`internal/proxy/egress` resolves references with `InterfaceRef.Resolve`, pins the
current name/MAC/index and uses `IP_BOUND_IF` or `IPV6_BOUND_IF`. It validates that
identity before and after dialing and each stream I/O. An index reused by another
interface cannot carry the old connection. New requests can resolve a renamed USB
adapter by its unique MAC; established streams do not migrate. Address/prefix or
DNS changes do not shut down unrelated listeners. Interface metadata is cached
for at most one second, while kernel identity is checked directly. Canceling one
metadata reader does not poison the cache for healthy callers.

DNS uses explicit A/AAAA packets sent through the selected interface: to its
configured servers, then, when none answers, to the Mac's own, from
`/etc/resolv.conf`. The fallback list leaves out private, carrier-shared and
link-local addresses, and IPv6 servers in any interface's local /64. A reconnected
hotspot may restart its DNS server: the M2000's refuses queries for about ten
seconds. A name the interface's servers say doesn't exist isn't asked again.
There is no system resolver, hosts-file/search-list lookup, environment proxy or
other-interface fallback. Truncated UDP answers retry TCP on the same server and
binding. IDs, question, opcode and response code are checked; replies are limited
to 16 KiB and 128 records, CNAME chains to eight steps and candidates to eight
addresses. Resolution has a four-second bound, at most three servers of each
kind and 750 ms per server attempt, whose query goes once more after 400 ms. DNS
and target sockets use the same pinned binding.

Physical bound sockets use macOS's scoped routing and the full-capture split that
preserves zero-address lookup, described below. Excluding tunnel interfaces also
prevents selecting Pith's own utun as a proxy exit. A VPN policy can still refuse
physical egress; that fails the request rather than selecting another interface.
The bound forwarding path has its own counters, outside the helper's
connection history and traffic totals. An unbound system leg from a scene
port can itself enter capture. A client that resolves names itself or bypasses its
proxy is outside the service's DNS/routing guarantee.

[Resolver](../internal/proxy/egress/dns.go) ·
[DNS tests](../internal/proxy/egress/dns_test.go)

### Sharing a port with other devices

A port with `Proxy.Shared` listens on `0.0.0.0` rather than `127.0.0.1`, still
IPv4 only; changing it replaces the listener, which closes its streams. Every
accepted connection is then told apart by its source address. One from a
loopback address, or from the very address it reached, as an app of this Mac
makes to the Mac's own address, is this Mac's. Any other is a device, taken only
when a `Config.Devices` entry, an IPv4 address or a range of at least 8 bits,
covers its address. Up to 64 configured IPv4 addresses or ranges (/8 to /32)
can be shared across all ports. A device that isn't covered holds no session slot and can't
close anyone's stream: at most eight at a time, for two seconds each, are read
far enough to be answered, SOCKS5's method `0xFF` or HTTP's `403`, so that the
other end can tell a refusal from a proxy that is gone; more are reset at once.

The manager remembers at most 64 devices that connected since the app opened,
with when each last did, its established streams and the bytes relayed for it;
`Status.Devices` lists them, the latest first, refused ones included, for the
interface to offer to allow. A full table forgets the device that connected
longest ago and has no connection, a refused one first. When a saved
configuration no longer covers a device, the manager resets its streams and
forgets the device. Adding coverage closes nothing. With no shared port enabled,
no device is remembered.

An address is all that tells a device: there is no password, and a device that
is taken reaches what the Mac reaches through the port's exit, the networks it
is connected to included. Not the Mac itself: every port refuses loopback,
unspecified and multicast destinations, those in 0.0.0.0/8, any address on
one of this Mac's interfaces, tunnels included, and the names reserved for
loopback, before it asks its interface, its upstream or its rules. A port
through an interface or by a scene's rules checks each address it dials,
after a name has resolved, against the kernel's list of this Mac's addresses
read anew (`net.InterfaceAddrs`, about 80 µs), and once connected checks the
address it reached again before it relays anything, for an address the Mac
took meanwhile; a list it can't read refuses the connection. Other hosts of
the local network stay reachable, and this Mac reached back through its
public address, by a router that forwards to it, isn't recognized. An
address a client spells where a name goes is read as the address, and a name
whose last label is a number, which only a lenient resolver reads as an
address, is refused. A port
through an upstream can't see what a name resolves to there, so a shared
port's upstream is on another machine: a loopback one is refused when saved,
and one that turns out to be on this Mac when it is reached, a loopback
address or the very address the connection left from, is refused before it is
told anything, with `upstream_refused`. The listener is IPv4: devices reach it
at the Mac's IPv4 addresses on its local networks, or at the one a VPN to the
user's own devices gives it.

[Destination checks](../internal/proxy/destination.go) ·
[Sharing tests](../internal/proxy/share_test.go)

### Upstream proxies

A port with `Proxy.Upstream`, a host and a port, has no interface. For each
connection the manager opens a TCP connection to the upstream as macOS routes
and resolves it, offers no authentication, and asks for the very target it was
asked for: a name stays a name, resolved where the connection leaves, and what
it reaches there is the upstream's to answer for. A reply
other than success is repeated to the client, and the address the upstream
bound becomes the reply's, or the connection's own when the upstream tells
none, as OpenSSH's dynamic forwarding does. This is how a port another Mac's
Pith shares becomes a port of this Mac.

An upstream has three seconds, of the ten a CONNECT has, to take the connection
and agree: if not, the client is told the network is unreachable, and
`ProxyStatus.Error` is `upstream_unreachable`. An upstream that answers `0xFF`,
as a shared port that doesn't list this Mac does, gives `upstream_refused`, and
the client is told the connection is not allowed. Once the upstream agreed, a
deadline is the target's, and says nothing against the upstream. A target the
upstream couldn't reach clears the error, as a connection does. Every ten
seconds at most, when status is read, a port asks its upstream whether it
answers, as described under [requests to a port](#requests-to-a-port), without
asking for a connection. Suspending an interface during a router's renewal
leaves these ports alone. A private port can use another local proxy. A
configuration pointing a port at itself on loopback is refused when saved. A
longer forwarding loop can run into handshake capacity or the setup deadline.

[SOCKS negotiation](../internal/proxy/upstream.go) ·
[Upstream tests](../internal/proxy/upstream_test.go)

### Ports by a scene's rules

A port with `Proxy.Scene` asks the client's `Route` for each connection. The
client compiles the rules of the scenes that ports name, with `internal/rules`
as `Check` and the engine do, when the configuration is loaded and saved; an
open stream keeps the exit it has. A destination is matched as TCP; a rule that
names apps never matches, since nothing tells the app behind a proxy's
connection, least of all on another device. A block rule's connection is refused
as not allowed. A route rule's interfaces are tried in order through the same
interface-bound dialer as a port through one interface, with three seconds each;
an interface that is suspended is skipped. When none connects, a rule that falls
back, like a connection no rule matches, is dialed unbound, with macOS's routes
and resolver, but never to an address of this Mac, which a name may resolve to:
such a port reaches no more of this Mac than a port through an interface does.
While a scene routes, an unbound connection of the app can enter capture and
follow the running scene's rules as any app's. In a scene whose rules name
domains alone, a name stays a name: the rules see it as the client asked for it,
and each interface's own DNS, or macOS, resolves it. When an enabled rule
without apps names an address, a name is looked up once with macOS's resolver,
for two seconds at most, unless a block rule names it. The rules then see the
name with each of its first eight addresses, as the engine sees a connection an
app makes, and the connection goes to those addresses alone, as literals: first
to the ones a rule sends somewhere, then to the ones no rule names, never to one
a rule blocks. Each address has a quarter of a second's head start on the next,
as [RFC 8305, section 5](https://www.rfc-editor.org/rfc/rfc8305.html#section-5)
has a client try the addresses of a name, and the first to connect is the
connection: an address that never answers doesn't take the others' time. A name
is not looked up a second time, so no socket connects to an address the rules
haven't seen, and a name that can't be looked up fails. Such a port has no
single interface to report. Its counters, like those of other ports, belong to
the proxy manager. Deleting its scene redirects the configured port to the
selected scene.

[Route selection](../internal/client/route.go) ·
[Rule tests](../internal/client/route_test.go) ·
[Shared and chained ports on hardware](ROOT-TEST.md#shared-ports-scene-exits-and-port-responses)

### HTTP on the same port

The first byte of a connection tells its protocol: SOCKS5's version, or a
capital letter, the start of an HTTP method. Such a connection is served as an
HTTP proxy serves it, with `net/http`'s request parser over at most 32 KiB of
request line and header fields, within the handshake's five seconds.
`CONNECT host:port`
([RFC 9110, section 9.3.6](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6))
opens a tunnel, answered `200`, and what the client sent after its request
without waiting is the tunnel's first bytes. A request for an `http://` URL
([RFC 9112, section 3.2.2](https://www.rfc-editor.org/rfc/rfc9112.html#section-3.2.2))
goes to its server in origin form, without the proxy authentication and
connection fields the handler strips, including the headers named by the
client's `Connection` field
([RFC 9110, section 7.6.1](https://www.rfc-editor.org/rfc/rfc9110.html#section-7.6.1)),
with `Connection: close`: one request a connection, whose answer is relayed
while the request's body is still being sent. Its header fields are written
anew, in `net/http`'s order and spelling. Go writes the framing the body
requires, and Pith preserves an absent `User-Agent`. Request trailers are not
sent on. A tunnel's bytes are relayed as they are. A request whose target is a
path asks the port itself, as described below. Other invalid targets are
answered `400`: an `https://` URL outside CONNECT, a CONNECT that names no port,
a port of zero. A connection that can't be opened is answered `403` where SOCKS5
says not allowed, `504` where it says host unreachable, else `502`. Names,
targets, limits, counters and what a port goes out through are those of SOCKS5;
there is no password here either.

[HTTP implementation](../internal/proxy/http.go) · [Tests](../internal/proxy/http_test.go)

### Requests to a port

An origin-form request goes to the port itself. GET returns `api.ProxyInfo` and
POST returns `api.ProxyIPChange`. Both endpoints return JSON on success and
`{"error":{"code":"…","detail":"…"}}` on a handler error. Times are UTC. Every
handler reply includes its content length, `Cache-Control: no-store`,
`X-Content-Type-Options: nosniff` and `Connection: close`. Only one request is
served on the connection.

| Request | Success (`200`) |
| --- | --- |
| `GET /` | `pith` (currently 1), `port`, `exit` (`interface`, `scene` or `upstream`), `online` and `canChangeIP`. Optional fields include `interface`, `publicIPv4`, `publicIPv6`, `at`, `changing` and nested `upstream`. Online describes the exit's availability. A scene is online while its port listens and has no single public address. |
| `POST /change-ip` | `port`, `seconds`, any observed `beforeIPv4`, `beforeIPv6`, `publicIPv4`, `publicIPv6` and `at`, and an optional OpenWrt `wan` result. An unchanged or missing address is possible. The optional body is discarded, up to 4 KiB. |

Any admitted client may read status. A POST requires `Proxy.ChangeIP` on each
port in the chain. Scene ports cannot enable it. `canChangeIP` means that a
request would be attempted. Device credentials and a working standby for a
Verizon router are checked during the operation. The app's own `ChangeProxyIP` action
calls `Manager.ChangeIP` directly and needs only the upstream's permission, if
any.

| HTTP status | Meaning for requests to the port |
| --- | --- |
| `400` | Invalid `max-age`, invalid `Pith-Hops` or an incomplete POST body (`invalid_config`). |
| `403` | A disabled IP-change permission, disallowed Host or browser request headers (`not_allowed`). Unlisted devices can also be refused before endpoint dispatch. |
| `404` / `405` | Unknown path or wrong method (`unsupported`). A method error includes `Allow`, including for OPTIONS. |
| `413` | POST body exceeds 4 KiB (`invalid_config`). |
| `501` | The exit cannot renew its IP (`unsupported`), including an ordinary SOCKS5 upstream. |
| `502` | Renewal, upstream or reply failure. When the wait ends because the caller left or the ten minutes elapsed, `internal` means the change may still be running. |
| `503` | Another interface on this Mac is renewing (`busy`), or the port request limits are full (`internal`). |
| `508` | A request would exceed the limit of four upstream forwards (`internal`). |

An interface's answers come from the client's `Info` hook. Without it the
port reports its configured name and the listener state it knows. If the hook
returns an error, any name, connection and change state it supplies survive,
with no public addresses. A suspended exit is offline while its port still
listens. A single nonnegative `max-age` query parameter bounds the age of
cached public addresses in seconds.

Before dispatch, a request is answered `403` and `not_allowed` when it has
any `Origin` field or a `Sec-Fetch-Site` other than `none`, or when its
`Host` is not an IP address, `localhost`, a name of one label or one under
`.local`; an empty Host passes, as an HTTP/1.0 program sends. No CORS fields
or preflight are provided. A page whose host name rebinds to this Mac makes
same-origin GETs with neither `Origin` nor `Sec-Fetch-*`, which browsers send
only to trustworthy origins, but with its own name as the Host, and that is
what refuses it. Names of one label and `.local` names can still be rebound
by whoever controls the local network's names. Programs and a person opening
the port's address directly may ask; a device not listed is still refused
before dispatch.

#### Upstream answers

An upstream is asked over HTTP directly on its host and port through the same
dialer that reaches it for SOCKS5, with `max-age` passed on and the address
dialed as the Host, which the upstream takes whatever name it was configured by.
A shared port refuses one reached on this Mac before sending anything. A `200`
JSON answer with `pith` of at least one is another Pith's, once its port
(1–65535), exit kind and nesting depth have been checked. `api.MaxProxyHops` is
four. A longer interface name is trimmed to 256 bytes on a UTF-8 boundary.
Malformed, zoned or wrong-family addresses are dropped. Its information is
nested in `Upstream`, its online state, addresses, timestamp and change state
are copied, and both ports' `ChangeIP` flags must allow a remote change. An
error answer in Pith's form, other than `403`, is a Pith that can't answer now:
its last answer stays, and the request is answered `503` or `508` as the
upstream was, else `502` with its error. Silence until the time is up leaves it
undecided: a last answer stays, and the request is answered `502` and
`upstream_unreachable`; without one, a GET tries SOCKS5 as below, and a POST is
answered the same `502` rather than `501`. Anything else, bytes that can't begin
an HTTP answer, an answer without JSON or a closed connection, is another kind
of proxy: a SOCKS5 greeting tells whether it answers and takes this Mac, without
addresses or the ability to change them. HTTP answers are bounded to 64 KiB,
including the headers. A poll's GET has three seconds within the port's connect
timeout, which leaves time for the SOCKS5 check; a change's check before and its
refresh after may take the whole connect timeout, as an upstream's first lookup
may. A POST is passed on only after the upstream answers as Pith, and the change
it answers with is checked too. Each forwarded request adds one to `Pith-Hops`,
initially zero; an upstream request already at `api.MaxProxyHops` is answered
`508` instead of being forwarded, and leaves what the port knows of its upstream
alone. That status returns through the chain.

#### Request limits and cancellation

Requests to ports have 64 slots across the manager, with `503` beyond that. At
most 48 may wait for an address change, leaving 16 for status reads and requests
answered immediately; a 49th waiting POST is answered `503`. A POST's body is
read within the connect timeout, and only then does it count against the 48 and
its wait extend to ten minutes: one that never sends its body holds a slot no
longer than a GET. Immediate method, path, parameter, permission and body
refusals do not count against the 48. Requests count neither as open streams nor
as traffic, and cannot be selected as the quietest stream. Suspending an exit
leaves them and unclassified HTTP handshakes open, so a router renewal can
answer the very POST that caused it. GET uses the connect timeout, with a little
time reserved for the answer; POST waits at most ten minutes. A departed caller
ends only its wait: the client owns an interface's change, and coalesces other
callers onto it. Configuration replacement and manager close still end these
sessions. Turning a port's `ChangeIP` on or off applies to new requests without
replacing its listener or interrupting streams and requests already in progress;
counters and the listening time are preserved.

`ProxyStatus.Upstream` is the upstream's last Pith answer, cleared when it
ceases to answer as Pith or the port is replaced; an error answer or silence
keeps it. `ProxyStatus.Error` for an upstream tells only that this Mac can't
reach it or that it refuses this Mac: one that answered records none,
whatever it said of its own upstream. Status polls it at most
once every ten seconds, even while connections flow, first by GET without
`max-age`, then by SOCKS5 when needed. `ProxyStatus.Changing` comes from the
client's non-blocking `Changing` hook for an interface, or from a change
being passed upstream or its last answer. A completed upstream change
refreshes that answer; a canceled wait makes the next status poll due.

#### Interface address cache

For each interface the client keeps the public addresses and timestamp from its
dual-stack HTTPS test. A renewal's tests before and after, the empty-target
`Probe` and a lookup for a port all fill this record. The Cloudflare speed test
and a TCP test of a named server do not write to it. It forgets them when the
interface goes down or away, or when its hardware address, label, interface
kind, IP addresses or either router change, and a test that began before such a
change can't put its result back. A `GET` without `max-age` is answered from the
record and reaches the internet only when no address is known, or only those of
one family where the interface has addresses of both, as a renewal can end with;
a failed lookup, or one that found one family alone, keeps the next from being
made for 30 seconds, twice as long for each such lookup in a row, ten minutes at
most, until a lookup finds both or the interface changes; a test someone asked
for, as a renewal's, that finds one family alone starts that wait over at 30
seconds. With `max-age`, addresses older than that are looked up again, and a
lookup that fails leaves none. A lookup for a port belongs to no caller: it runs
to its own end while callers wait for it, each for as long as it cares to, so
another Mac's poll that gives up after three seconds finds the addresses the
next time. A caller that joins a test another started doesn't inherit its
cancellation: when the one who started it gives up, a caller still waiting
starts its own. When the record has changed since a caller read the interfaces,
because another reader saw them otherwise meanwhile, an explicit address probe,
including a renewal's, tests the interface it read without filling the new
record. For a port, whether its address can be changed and what its addresses
are are found at once, not in turn. Identification uses the USB hotspot's
interface label, the interface-bound Wi-Fi hotspot discovery cache, the
configured OpenWrt status command or the Verizon web admin's model. A
definitive negative stays cached for its interface snapshot. Transient
identification errors can be retried after a minute. A saved OpenWrt change
invalidates the cached capability.

#### Shared address changes

One change of an interface's address runs at a time, whoever asked: a port's
request, `RenewHotspot` or `RenewRouter`. The others wait for it and get what it
did, each in its own form. One runs at a time across interfaces too: while one
is under way, a change of another interface is refused with `busy`, which a port
answers `503`. A Verizon router's renewal leaves the Mac to another interface, which
macOS picks only once the router yields, so no one interface can be set aside
beforehand; and a renewal must not yield to a hotspot about to reconnect. A
change a port asked for runs under the client's lifetime, three minutes at most
for a hotspot and eight for a router, and the request's end ends only its wait;
one the Network page asked for ends with its caller, as before. `Close` waits up
to 25 seconds for the renewals. The router's restore request has its own
deadline.

[Wire types](../internal/api/proxy.go) ·
[Request handling](../internal/proxy/port.go) ·
[Client integration tests](../internal/client/exit_port_test.go) ·
[Hardware results](ROOT-TEST.md#shared-ports-scene-exits-and-port-responses)

## Tools for agents

`internal/mcp` lets an agent work Pith over the
[Model Context Protocol](https://modelcontextprotocol.io/specification/2025-11-25):
tools alone, over newline-delimited JSON-RPC on standard input and output. Two
processes share the work, because the app alone holds the configuration and the
local proxies: a second client beside it would open the same ports and save over
its configuration.

`Pith mcp`, the app's own executable as an agent's program starts it, speaks the
protocol on its standard input and output (`mcp.Serve`). It answers `initialize`,
`ping` and `tools/list` itself, from the tools compiled into it, and passes each
`tools/call` to the app (`mcp.Remote`). It takes no single-instance lock and
shows no interface. It may start before Pith and outlast it: a call made while
no app listens is answered with a tool error that says so, and the next call
finds the app again. Calls run concurrently, so that a change of address keeps
no other waiting; `notifications/cancelled` cancels a call, and the end of the
input cancels those that run. The accepted protocol revisions are 2024-11-05,
2025-03-26, 2025-06-18 and 2025-11-25. A client that asks for another gets
the latest supported revision.

The app listens (`mcp.Listen`) on
`$HOME/Library/Application Support/Pith/mcp.sock`, beside its configuration. The
path is the same however the stdio program was started. `PITH_MCP_SOCKET` puts
it elsewhere for development, and a Pith on the mock backend listens only there.
The socket is bound under another name, set to mode 0600 and renamed into place:
it is never there with the permissions the umask gave it, and it replaces a
stale one in one step. Each connection's `LOCAL_PEERCRED` user must be the
app's. No TCP port is opened, so that no web page and no other device reaches
the tools. Programs running as the same user can already write the configuration
file and connect to the helper's user-owned socket. A call is a connection of
its own, one line of JSON for the tool and its arguments and one for the answer;
closing it before the answer cancels the call. At most 32 run at once.

The tools run on `ui.Agent`, the backend as the interface holds it. A change of
configuration is made on the main thread to a copy of the interface's
configuration, validated, shown, and saved by the interface's own saver, whose
result the tool waits for. An agent's changes and the user's are thus made one
after another, neither saves over the other's, and a save that fails puts the
interface back to what is saved. Starting selects the scene. Starting and
stopping record whether routing was left on, as the menu bar's switch does.
Everything else goes straight to the `api.Service`.

| Task | Tool names |
| --- | --- |
| Inspect Pith and the Mac | `status`, `get_config`, `list_interfaces`, `list_apps` |
| Inspect routing and traffic | `check`, `list_connections`, `traffic` |
| Test an interface or read its gateway | `test_interface`, `speed_test`, `gateway_status` |
| Control routing | `start_routing`, `stop_routing` |
| Edit rules | `add_rule`, `update_rule` (including `position`), `remove_rule` |
| Manage scenes | `add_scene` (optionally with `copy_of`), `rename_scene`, `remove_scene` |
| Configure ports and devices | `set_proxy`, `remove_proxy`, `allow_device`, `remove_device` |
| Configure an upstream router | `set_openwrt_router` with an interface and SSH host |
| Renew an exit | `change_ip` with either `interface` or `port` |

`list_interfaces` shows connected links and named inactive non-VPN services.
Like the window, it leaves out Pith's own capture tunnel, known by the helper's
capture identity, and so do the tools that take an interface. Tools accept
interfaces by BSD name, alias or label, apps by path, bundle ID or name, and
scenes by ID or name. Unknown arguments fail. The tool definitions supply
`readOnlyHint` and `destructiveHint`. Read-only, editing and disruptive tools
have distinct annotations. Public-address changes call `RenewHotspot`,
`RenewRouter` or `ChangeProxyIP`. The app handles helper installation and
removal, entering gateway passwords, editing general settings and clearing the
connection log. To remove the SSH router configuration, pass an empty `host`
to `set_openwrt_router`. [Tool definitions](../internal/mcp/tools.go) ·
[Tool tests](../internal/mcp/tools_test.go)

`change_ip` reports the addresses it observed before and after. Its `changed`
flag tells that an address found both before and after differs, family by
family, as the window compares them: a family missing on either side isn't
compared. The optional `ipv4Changed` and `ipv6Changed` fields give the
individual comparisons when both measurements exist. A hotspot whose test after the change found its IPv6 address alone,
unchanged, is not reported changed, whatever its IPv4 address was.

For clients configured with JSON, the server entry is
`{"command":"/Applications/Pith.app/Contents/MacOS/Pith","args":["mcp"]}`.
For a remote Mac, point the client at `ssh` with that Mac, executable path and
`mcp` as arguments. The app must be running as the SSH user on that Mac.

[Socket implementation](../internal/mcp/socket.go) ·
[Running the mock](BUILDING.md#mock-interface-and-mcp)

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
On a busy Mac a routing-table read can miss its 250 ms deadline. The interface
watcher makes up to three attempts at its initial read. A later
read failure preserves both the stream and its last snapshot. It retries after
a change or on a timer starting at one second and doubling to 30 seconds.
The client reconnects a lost stream immediately and spaces repeated attempts
at least three seconds apart. Stream setup and the separate fallback status
request each have a five-second budget. An answering helper keeps its normal
status. A verified installation with nothing listening on its socket reports
`helper_not_responding` without waiting through both budgets. The installed launchd job sets `ProcessType` to `Interactive` for
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
