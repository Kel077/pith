# Bounded root acceptance sessions

This document describes an opt-in test harness and records the limited completed
hardware coverage below. A past result does not authorize another session.
Privileged operations require administrator approval for the specific session.
Review the capture scope and undo plan before starting. Run the harness as the
installing user, never with `sudo` or as root.

The default host-scoped session captures only one explicitly selected public IPv4 `/32`
HTTPS destination, plus an optional public IPv4 `/32` DNS destination. It does
not install full-capture routes, alter DNS settings or pf, or replace the default route.
The helper starts idle. It creates one utun only when `check` calls `Start`.
Every check has a three-minute deadline and an independent fifteen-second
cleanup deadline. SIGINT and SIGTERM cancel the check and invoke cleanup.
The helper also limits each scoped routing run to five minutes, so a killed
test client cannot leave routing active indefinitely. The helper remains
installed and idle until the explicit uninstall step.

## Recorded full IPv4 result

Full IPv4 was verified on hardware at `9ceff5c`, with Tailscale disconnected:
`smoke`, all 11 `batch` cases, `app-bypass`, `preserve`, `safe-failure` and `crash`
passed. Final `uninstall` restored the accepted
baseline. Independent inspection found none of Pith's eight split prefixes,
normal default routing and connectivity, and no remaining test processes.
The app-bypass result covers standalone executables and their different-path
child, not app bundles or delegated networking services.

The initial full-capture SafeStart failed at outbound forwarding. When no scoped
route matched a bound socket, XNU fell back to a longest-prefix lookup of the
zero address. The former lower `/1` selected Pith's utun instead of the required
system interface, so the forwarded probe was reset. The fix splits IPv4 into
eight prefixes excluding `0.0.0.0/8`, and production IPv6 likewise excludes
`::/8`. The system default remains available to that lookup. See
[the exact prefixes and XNU sources](ARCHITECTURE.md#capturing-and-forwarding).
Both families have synthetic old-failure/new-success coverage; the hardware
pass recorded here applies only to IPv4.

A gateway serving DNS with an existing `/32 UCS` route is preserved. Its
optional DNS capture is skipped and recorded in the private plan/report, so
queries using it are not observed. Domain rules then need another observed name
such as TLS SNI, HTTP Host or QUIC SNI. This behavior allowed full capture to
start without replacing the gateway route; it does not prove DNS interception.

That first run did not cover connected VPN operation. The later bounded
[Tailscale reconstruction stage](#manual-vpn-reconstruction-during-full-ipv4-capture)
has now passed on hardware. Full IPv6, broader UDP/QUIC coverage, background
delegation and sustained throughput remain separate acceptance items.
The later scoped IPv6, UDP/QUIC, bundle, Safari and short throughput results are
recorded separately here and in [the acceptance plan](NEXT-ACCEPTANCE.md).
These results do not establish cancellation-dialog behavior.

## Recorded IPv6 host result and IPv4 regressions

Host IPv6 was verified on hardware at `d359287`: smoke, all 11 batch cases,
app-bypass, preservation, forced SafeStart failure and crash recovery passed.
The scoped IPv4 regression passed smoke, batch, app-bypass, preservation and
forced SafeStart failure; full IPv4 passed those cases and crash recovery.
Each uninstall restored its baseline. Independent inspection confirmed ordinary
IPv4/IPv6 routing and connectivity, no remaining owned routes, tunnel or test
processes. The IPv6 result covers one frozen `/128`, not full IPv6 capture.

Three issues were corrected on the way to that pass: explicit `-host` preserves
HOST and host-route semantics where IPv6 `-net /128` omitted both HOST and the
netmask; a normal IFINFO skipped by `x/net/route` is decoded as an interface
notification rather than a failure; and static non-cloned IPv6 LLINFO host
routes remain in ownership snapshots while dynamic neighbors stay excluded.
The first issue was reproduced from source and wire fixtures, since its original
hardware event was not recorded. The latter two were identified by retained
hardware diagnostics. See [the detailed history](#separate-ipv6-host-installation).

## Prepare a concrete review

Before preparing, have the user choose two currently up interfaces and a public
HTTPS endpoint. Use the interface names discovered on this machine; do not
copy them into repository files. The endpoint must support certificate-verified
TLS and HTTP/1.1 `GET /`, respond within eight seconds, and tolerate several
short requests. A small plain-text public-IP echo is preferable: it supplies
independent evidence of the observed public exit. A generic HTTPS endpoint is
also accepted for the ordinary smoke checks, but then the actual-interface
evidence is the helper's log and the preflight bound socket. The optional
`--app-bypass` check requires public-IP echoes from both the ordinary system
preflight and interface A. Missing echoes make that check inconclusive. When
both interfaces share the same public exit, an IP echo cannot distinguish
them; the report records this limitation even when routing assertions pass.

Do not use a production host whose existing connections must remain entirely
untouched. Select a dedicated test destination. Do not select a LAN, loopback,
link-local, private, multicast, or shared-address-space address. The default
host-scoped mode deliberately refuses IPv6 and subnet capture. The separately
reviewed full IPv4 and IPv6 host installations are described below.

Build both binaries into a private temporary directory, without changing the
application's configuration or packaged resources:

```sh
PITH_TEST_BIN=$(/usr/bin/mktemp -d "${TMPDIR%/}/pith-rootcheck-bin.XXXXXXXX")
export GOCACHE="${TMPDIR%/}/pith-go-build"
export GOMODCACHE="${TMPDIR%/}/pith-go-mod"
CGO_ENABLED=0 go build -trimpath -ldflags '-X main.version=acceptance' -o "$PITH_TEST_BIN/pithd" ./cmd/pithd
chmod 0755 "$PITH_TEST_BIN/pithd"
CGO_ENABLED=0 go build -trimpath -o "$PITH_TEST_BIN/rootcheck" ./scripts/rootcheck
```

Set the following values from the user's choices. They stay in the terminal;
the real interface names, addresses, and certificates must not enter a commit:

```sh
PITH_IF_A='<first reviewed BSD interface name>'
PITH_IF_B='<second reviewed BSD interface name>'
PITH_TLS_TARGET='<reviewed literal IPv4:port>'
PITH_TLS_NAME='<reviewed TLS certificate DNS name>'
"$PITH_TEST_BIN/rootcheck" prepare --helper "$PITH_TEST_BIN/pithd" --version acceptance --interface-a "$PITH_IF_A" --interface-b "$PITH_IF_B" --target "$PITH_TLS_TARGET" --server-name "$PITH_TLS_NAME"
```

For UDP/DNS checks, add up to two `--dns-target '<reviewed public IPv4:53>'`
arguments and an explicit `--dns-name '<reviewed question name>'` to that one
preparation command; see the separate bounded DNS stage below. A legacy session
with only a DNS target permits manual checks, not the new automatic batch.
Do not add extra hosts or change the question later: prepare and review
a new session instead. There is no default target or automatic capture expansion.

Preparation opens no administrator dialog. It refuses an existing helper,
service, socket, data directory, or capture journal rather than replacing it.
It reads interfaces, routes, and the managed launchd registration. It checks
the same HTTPS destination using one ordinary socket, then one bound socket
per chosen interface, closing each before the next. These are three bounded
network requests, not network configuration changes.

The result is a mode-0700 session directory under the current `TMPDIR`. Its
mode-0600 files include:

| File | Review purpose |
| --- | --- |
| `session.json` | User ID, immutable target selection, reviewed adapter references, helper checksum and installation identity, capture ownership history, active baseline label/file, current smoke result, and a checksum-bound cleanup proof |
| `original.json` | Permanent initial pre-privilege interface inventory, route snapshot, launchd result, and the five managed paths |
| `preflight.json` | Bound/unbound HTTPS status and public-IP echo, when the endpoint provides one |
| `dns-plan.json` | Optional frozen resolver endpoints, question name, A/AAAA types, request counts, response/idle bounds and evidence limits; its checksum binds the automatic DNS stage |
| `quic-plan.json` | Optional frozen HTTP/3 host/certificate name, request/packet/payload budgets and observable/fragmented Initial controls; its checksum binds the QUIC stage |
| `bundle-plan.json` | Optional standalone tool checksum/size, exact bundle metadata/layout, TLS target and one/six-request smoke/batch limits for disposable bundle fixtures |
| `webkit-plan.json` | Optional frozen Safari app/executable checksums, current A-record set and lookup time, nonce URL, manual/single-open trigger and a 60-second maximum delegation observation |
| `process-audit-*.json` | Independent read-only inventory after browser observation/uninstall: all still-present processes whose kernel start time falls in the recorded observation/cleanup windows, regardless of owner or executable |
| `throughput-plan.json` | Optional frozen IPv4 throughput endpoint/TLS name, two adapter budgets, payload/socket caps and exact direct/minimal/many-rule request counts |
| `throughput-a-attempt.json`, `throughput-b-attempt.json` | Exclusive durable reservations created before any measurement request; interruption, failure, rebaseline and reinstall never refund these attempts |
| `vpn-plan.json`, `vpn-restart-attempt.json` | Optional frozen own-name/device-local resolver and original VPN identity, human timing/request limits, and one durable non-repeatable restart attempt |
| `install-plan.json`, `install.sh` | Exact scoped launchd arguments, fixed destinations, helper SHA-256 check, rollback, and authorization prompt |
| `uninstall.sh` | Exact bootout, ownership-checked route cleanup, and managed-path removal |
| `emergency-command.txt` | A standalone, single command that can be pasted into Terminal without IPC or the test executable |
| `cleanup-*.json`, `check-*.json` | Immutable cleanup observations and separate test results, tied to the current baseline, installation, and ownership history; external-only differences have a separate classification |
| `rebaseline-*.json` | Immutable mode-change candidates, before/after snapshots, and system/A/B preflights; failed attempts remain archived |

Set `PITH_SESSION` to the **exact canonical directory printed by prepare**.
Do not use a path inside the repository or the normal application config
directory. A changed helper or regenerated installation plan invalidates the
review; `cancel` and `install` recheck the SHA-256 and reviewed script before
authorization. The root installation stage independently verifies the copied
binary checksum after administrator approval.

The installation plan also lists two shared parent directories under
`CreatesIfMissing`. `/Library/PrivilegedHelperTools` is created as root:wheel
mode `1755` only if missing, and `/Library/LaunchDaemons` as root:wheel mode
`0755` only if missing. Existing directory ownership and permissions remain
unchanged; symlinks, non-directories, non-root owners, and group/other-writable
existing parents are refused instead of repaired. Uninstall retains these
shared parents and removes only Pith's five owned paths.

Use a freshly prepared session with this harness version. Older session files
without the recorded installation identity cannot acquire cleanup proof or
rebaseline; do not edit JSON to upgrade them. The initial snapshot file is never
rewritten. For compatibility, `session.json.original` holds the **active**
rollback snapshot, selected by `baselineLabel` and `baselineFile`.
Sessions prepared by an older snapshot decoder may contain kernel route-cache
entries without their original flags. Prepare a new session rather than deleting
host routes from saved JSON or comparing that old baseline with filtered data.
External-change acceptance also requires complete capture identities and boot/
interface-index observations from this version. An old session cannot infer
that history from utun names or be upgraded by editing its JSON.

Review these artifacts before administrator approval. Keep a second Terminal
window open with the emergency command already copied. Do not start the
application or another helper installer during the session.

## Separate full IPv4 installation

This mode requires its own explicit review and installation after the previous
session has stopped and uninstalled. Never edit a scoped session into full
capture, remove `--capture` from its plist, or invoke the application's ordinary
installer for a test. Preparation still refuses any existing installation.

With a newly built, reviewed helper and the same privately chosen variables:

```sh
"$PITH_TEST_BIN/rootcheck" prepare --helper "$PITH_TEST_BIN/pithd" --version acceptance --interface-a "$PITH_IF_A" --interface-b "$PITH_IF_B" --target "$PITH_TLS_TARGET" --server-name "$PITH_TLS_NAME" --capture-mode full-ipv4 --test-lease 120s
```

The session records `testProfile`, including mode, lease, literal TLS endpoint,
certificate name, and the exact LAN-DNS addresses selected by production capture
planning. Review all eight IPv4 prefixes (`1.0.0.0/8`, `2.0.0.0/7`, `4.0.0.0/6`,
`8.0.0.0/5`, `16.0.0.0/4`, `32.0.0.0/3`, `64.0.0.0/2`, `128.0.0.0/1`)
and every DNS `/32` before authorization. They exclude `0.0.0.0/8` so Darwin's
zero-address fallback still finds the original default for interface-bound
sockets. Production IPv6 uses the corresponding eight prefixes excluding `::/8`;
this profile still forbids IPv6. See the exact set and XNU `route.c`/`radix.c`
references in [capture architecture](ARCHITECTURE.md#capturing-and-forwarding).
Every prefix gets its own conflict check and belongs only to Pith's utun. Sessions
from the former two-half plan require fresh preparation and reviewed installation;
never edit an older session or reuse its helper checksum.
An existing configured DNS host route, including a gateway's host route or a
scoped copy, is preserved and excluded from the frozen DNS capture set. Review
`dns-skips.json` and `session.json` for each skipped address, its reason and the
existing routes; prepare also prints this warning. Check and rebaseline reports
record the current exclusions. DNS queries retaining those routes are not
observed, so domain rules need other observed names (TLS SNI, HTTP Host or QUIC
SNI when sniffing is enabled). This does not relax split-prefix conflicts or permit
deleting, replacing or replaying gateway routes. If later DNS/route changes alter
the frozen capture set, prepare and review a new installation.
The helper receives explicit `--test-profile full-ipv4`, `--test-lease`, and
repeated `--test-dns-host` arguments; no `--capture` argument is used. Tailscale
DNS and DNS routed through another tunnel are excluded by the production plan.
`--dns-target` cannot be combined with this mode. IPv6 capture is forbidden and
SafeStart cannot be disabled, including by hot Reload or repeated Start.

The exact install/cancellation prompt is:
`Pith needs your password to install its full IPv4 test helper (at most 120 seconds per capture).`
The uninstall and optional crash prompts, managed paths, ownership checks and
emergency-command construction are unchanged. Installation, uninstall and the
optional deliberate crash each require administrator approval. Cancellation
must be tested separately; a previous session does not authorize another one.

The harness accepts a 30-to-120-second lease, default 120 seconds. The immutable
helper limit starts before startup work, includes SafeStart, and never extends
on reload. A separate timer closes the owned capture at expiry even if the
engine's lifecycle lock is busy; bounded ownership-checked teardown can then
finish. Expiry is a failure, not a passing test. The controller's total check
deadline is ten seconds shorter than the lease, plus its independent cleanup
deadline. Choose an observation duration that fits the remaining time after
smoke requests; the outer deadline always wins.

Only an authenticated `check` starts routing. Its smoke and existing TCP cases
require actual captured process/target/rule/interface records. Production
planning must still produce exactly the frozen prefixes before any utun is
created. A changed LAN-DNS capture set refuses Start/check/rebaseline and needs
a fresh reviewed installation. Stop removes only the eight owned split prefixes, reviewed
DNS host routes and owned utun. Defaults and external routes are never replayed
or removed. The standalone emergency command uses the same journal ownership
checks and needs no network or IPC. Journal recovery and emergency cleanup iterate
the exact recorded prefix set; they neither assume two routes nor recreate a
default route. Kernel utun closure removes every owned prefix after a crash.

The first approved full-mode check must still be smoke. A later TCP batch,
SafeStart failure, preservation, app/tool bypass, observation, or separately
authorized crash uses a fresh bounded capture. Ordinary full capture includes
other apps' new IPv4 traffic, which keeps system routing unless a test rule
matches; reviewing only the test endpoint is therefore insufficient approval.
DNS responses, tailnet controls, VPN self-restart, UDP/QUIC, app bundles and
throughput require the additional evidence in [next acceptance](NEXT-ACCEPTANCE.md).
The automated hotspot-renewal flag currently remains restricted to `host-ipv4`
sessions. The recorded hardware pass above covers the TCP and cleanup cases;
unit tests and that pass do not establish the additional acceptance items.

## Separate IPv6 host installation

The first hardware smoke attempt failed during SafeStart with
`unreadable configured route event`; rollback and uninstall were independently
verified clean. The former `-net /128` route command produced a destination with
neither HOST nor netmask in a synthetic Darwin reproduction. Both IPv4 `/32` and
IPv6 `/128` now use explicit `-host` for add and delete. See
[the source analysis and observer policy](ARCHITECTURE.md#capturing-and-forwarding).
The original failed report contained no event metadata, so the exact event is
not retrospectively proven. The corrected IPv6 host hardware pass is recorded
above. Repeated tests still require a new reviewed session and authorization.

The subsequent IPv6 smoke and a scoped IPv4 regression exposed another startup
fault: a normal utun IFINFO with no sockaddrs was mistaken for a decoder failure.
`x/net/route` deliberately skips IFINFO without `RTA_IFP`; the observer must read
its interface header directly. That classification is corrected, with synthetic
IPv4/IPv6 IFINFO, NEWADDR and route-ADD startup sequences, both immediate and
delayed delivery. Notifications now verify the complete capture and current link
state instead of treating the owned interface's notification as a route fault.
The corrected scoped IPv4 hardware regression passed smoke, the 11-case batch,
app-bypass, preservation and forced SafeStart failure, including explicit IPv4
host-route add/delete.

The subsequent IPv6 smoke reached ownership validation but reported its `/128`
missing. Source analysis and a synthetic RIB reproduce a filter error: XNU marks
manually added IPv6 interface hosts with LLINFO as well as STATIC. Only dynamic
neighbor entries and cloned children should be omitted; explicit static hosts
must remain visible to ownership and cleanup. The same attempt's non-fatal
NEWADDR diagnostic matches a legitimate empty final RTA_BRD on an IPv6
point-to-point address without a peer. Both formats are now covered by wire
fixtures. Failed ownership checks retain address-free candidate summaries from
the failed RIB read in `routeDiagnostics`, including flags, gateway family,
scope, index and counts, even after Stop. See the
[source analysis](ARCHITECTURE.md#capturing-and-forwarding). The subsequent IPv6
hardware run passed, including crash cleanup; this record does not authorize
another test.

IPv6 `/128` acceptance is another new installation and private session, selected
explicitly by `--capture-mode host-ipv6`. Both reviewed adapters must be up and
have a global, non-private IPv6 address. A missing IPv6 path is a prerequisite
failure, never permission to use IPv4. Choose one public IPv6 test endpoint that
supports the same certificate-verified HTTP/1.1 request as the IPv4 endpoint:

```sh
PITH_TLS6_TARGET='<reviewed [IPv6]:port>'
PITH_TLS6_NAME='<reviewed TLS certificate DNS name>'
"$PITH_TEST_BIN/rootcheck" prepare --helper "$PITH_TEST_BIN/pithd" --version acceptance --interface-a "$PITH_IF_A" --interface-b "$PITH_IF_B" --target "$PITH_TLS6_TARGET" --server-name "$PITH_TLS6_NAME" --capture-mode host-ipv6 --test-lease 60s
```

Preparation performs exactly three IPv6 TLS requests to that frozen literal:
ordinary system routing, bound A, and bound B. It first normalizes the literal's
spelling to match connection logs. The helper receives the distinct
`host-ipv6` profile and a lease of at most 60 seconds (the harness accepts 30 to
60 seconds). No additional DNS target, LAN-DNS capture, IPv4 capture or full IPv6 capture
is permitted. Mapped IPv4 addresses, scoped literals, private/link-local addresses
and domain-name endpoints are rejected. The tunnel still has its own selected
local addresses; closing it removes those along with the single owned `/128`.

The exact install/cancellation prompt is:
`Pith needs your password to install its IPv6 host test helper (at most 60 seconds per capture).`
Use the same separately reviewed cancel/install/check/stop/uninstall sequence and
ownership-checked emergency command as above. Installation, uninstall and the
optional deliberate crash each require administrator approval. The helper starts
idle and the controller leaves ten seconds of margin before the helper deadline.

SafeStart is mandatory and proves the exact IPv6 socket crossed the tunnel.
Every preflight and ordinary test request uses `tcp6`, with no name resolution
or IPv4 connection fallback. Smoke verifies system and A; after smoke, the TCP
batch includes B, domain rules, block, missing-interface fallback, strict failure,
and new versus established connections across hot reload. Every positive case
must have the exact IPv6 destination, process, rule, outcome, actual interface
and bidirectional bytes in the connection log. Public echoes use the same
family-checked drift policy; an IPv4 echo cannot satisfy an IPv6 preflight.

The short outer deadline may end a slow batch; retain that failure rather than
extending the lease or claiming all cases passed. Run observation only with a
duration that fits its remaining budget. Hotspot renewal remains unavailable in
this profile. Stop or SafeStart failure must restore the complete IPv4/IPv6 route
and tunnel baseline. Rebaseline cannot change the frozen IPv6 host or family.
Privacy addresses within one IPv6 prefix may rotate; a changed assigned prefix
during preflight must be reviewed rather than silently accepted as stable.

This establishes IPv6 host-capture support, not full IPv6 capture, UDP/QUIC, or full
dual-stack acceptance. IPv4 policy equality is recorded, but an independent IPv4
connectivity control requires its own reviewed endpoint and observation.

## Bounded UDP/DNS stage

UDP/DNS was verified on hardware at `c342de7`: TCP smoke, the one-query A-path DNS
smoke and the complete two-resolver batch passed. The first
resolver passed ten cases, including expiry and a new B-path session on the same
application source port; the second passed eight. Real A/AAAA replies, DNS IDs,
application/pre-NAT tuples, policy and payload counters supported those results.
Uninstall restored the baseline. Independent inspection confirmed ordinary
resolver routing, working system DNS and no owned residue. Cache expiry, general
UDP services, pre-existing UDP, QUIC and public NAT ports remain outside this pass.

This is an explicit extension of `host-ipv4` only. During preparation,
freeze one or two distinct public IPv4 resolver literals on port 53 and one
plain lowercase question name. There is no default resolver, echo fixture, or
system DNS change. The reviewed capture consists of the TLS host plus those
DNS hosts, all `/32`; the immutable helper lease remains five minutes. Full IPv4
and `host-ipv6` profiles reject additional DNS targets in this stage.

```sh
"$PITH_TEST_BIN/rootcheck" prepare --helper "$PITH_TEST_BIN/pithd" --version acceptance --interface-a "$PITH_IF_A" --interface-b "$PITH_IF_B" --target "$PITH_TLS_TARGET" --server-name "$PITH_TLS_NAME" --dns-target '<first reviewed IPv4:53>' --dns-target '<second reviewed IPv4:53>' --dns-name '<reviewed question name>'
```

Omit the second resolver if only one is approved. Preparation still performs
exactly the three existing TLS preflights; it sends no DNS test queries. Review
`dns-plan.json`, the added host routes, installation and emergency undo before
authorizing this new session. After the normal cancel/install sequence and a
successful default TCP smoke, run one DNS query before any batch:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION"
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --udp-dns
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --udp-dns-batch
```

Each command starts a fresh bounded capture, verifies the ordinary TLS system
path, and always stops and compares the baseline afterward. The DNS smoke sends
one A query through adapter A to the first resolver. Only a passed DNS smoke and
successful cleanup enable the batch; default TCP smoke and rebaseline clear that
pass flag. Each command is a separate case: stop on failure and review its
private `check-udp-dns*.json` and cleanup report before continuing.

The batch sends at most 11 queries for one resolver or 20 for two. Each exchange
sends one classic UDP query with a unique DNS ID and a two-second response
deadline, without retransmission, EDNS, resolver fallback or TCP fallback. The
512-byte reply limit, matching ID/question/class/type, successful response code,
and address answer for the question or its bounded CNAME chain are required.
Truncation or a missing A/AAAA answer is a failed prerequisite/result, never a
synthetic success or permission to choose another name. At most three queries
belong to one case, and the existing three-minute controller deadline still
applies independently of the helper's lease.

For each resolver the batch checks unmatched system egress, A and AAAA through
A, reuse of that engine session and its source tuples, normal closure of A after
hot reload to B, a replacement session through B from the same native socket,
another new session through B, missing-interface fallback to A,
explicit system fallback, block priority, and strict missing-interface failure.
On the first resolver it also waits at most 14 seconds for the ten-second DNS
idle timeout, then uses the same native UDP socket/source port to require a new
engine connection ID and B's new rule. No packet loss or silent peer is used to
claim automatic failover.

Positive evidence requires the exact PID, executable, UDP application tuple,
destination, rule, outcome, interface and cumulative payload byte counts from
the helper, plus the actual nonce-matched DNS response. Application and outbound
pre-NAT source ports must stay stable within one engine session. A new outbound
socket after expiry may legitimately reuse its old ephemeral port. Public
resolver replies do not reveal carrier/public NAT ports; no such claim is made.
Negative cases require no response and a matching terminal block/unreachable
record with no outbound tuple or forwarded payload. Capture identity must stay
Running throughout the stage.

The private report retains A/AAAA/CNAME answers and their TTLs. It does not dial
those answer addresses, add routes for them, or prove IP-to-domain cache expiry.
Those cache semantics retain synthetic tests. General UDP echo, a two-minute
non-DNS idle timeout, pre-existing connected UDP, and unconnected UDP listeners
remain separate acceptance gaps. Uninstall and independently inspect all owned
routes, tunnel, service and paths when this session ends. The DNS checks themselves
require no additional administrator approval.

## Bounded QUIC/HTTP/3 stage

QUIC/HTTP/3 was verified on hardware at `8dd603e`: TCP smoke, the single `--quic`
request through A, and all three `--quic-batch` cases passed:
observable Initial SNI domain routing through A, IP routing through B, and IP
routing through A with a fragmented ClientHello. Verified HTTP/3 responses and
helper tuple/process/rule/interface/name/counter evidence all passed. Uninstall
restored the baseline; independent system checks were clean. This establishes
the reviewed IPv4 QUIC v1/HTTP/3 subset, not ECH, migration, 0-RTT or arbitrary
multi-packet SNI recovery.

The first hardware attempt passed TCP and QUIC smoke, but failed the domain-rule batch
case's post-response evidence check despite a verified HTTP/3 response. One UDP
tuple had ended early and been recreated without owner/name on the system path.
Cleanup restored the baseline. A synthetic IPv4/IPv6 reproduction identified the
UDP ICMP teardown path: late server replies after client close must not release
the original binding before its queued tail is forwarded. The relay now consumes
port-unreachable as a datagram error and keeps the session until normal idle or
an explicit policy change requires reconnection;
other terminal errors remain visible instead of being hidden by paired close.
The old hardware report did not retain the triggering ICMP/error, so that exact
packet was not directly observed. The successful hardware repeat above exercises
the fix, alongside the separate synthetic IPv4/IPv6 ICMP regression.

Add an explicit `--quic-target '<reviewed IPv4:443>'`
and `--quic-server-name '<reviewed certificate name>'` when preparing a new
`host-ipv4` session. It freezes one additional `/32` if that IP differs from the
TLS/DNS targets. No name resolution or QUIC request occurs during preparation;
the existing three TLS preflights and mandatory SafeStart still use the original
frozen TLS target. Review `quic-plan.json`, every host route and the ordinary
install/undo plan before authorization. Other installation profiles reject this
extension. Rebaseline cannot change the endpoint or protocol budget.

After installation and the default TCP smoke, first run one request:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --quic
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --quic-batch
```

The first command makes one HTTP/3 `GET /` with an IP rule through A. Its
successful independent cleanup enables the second command, which makes exactly
three requests: a domain rule through A with observable first-datagram SNI, an IP
rule through B, and an IP rule through A with ClientHello split across Initial
packets. Each request uses a fresh connected IPv4 UDP socket to the literal
target, QUIC v1, verified TLS 1.3 and ALPN `h3`. The fragmented control uses a
hybrid key share while retaining normal certificate validation and SNI; the
recorded first datagram must actually lack a recoverable name. It is not an ECH
test or a claim of multi-packet sniffing support. If a cached name or a changed
packet layout defeats that control, the case fails rather than claiming it ran.

There is one direct HTTP/3 request per case: no application retry, redirect
following, HTTP proxy, DNS lookup, TCP fallback, migration or saved 0-RTT state.
QUIC's own protocol retransmissions remain enabled within each ten-second
deadline. Every exchange is capped at 64 KiB of sent UDP payload, 256 KiB of read
UDP payload, 128 writes and 256 received datagrams. These limits count client
socket payload, not network headers or packets queued/dropped by the kernel.
Headers are limited to 16 KiB; only the first 16 KiB of the response body is read
and hashed. The private report stores its size/hash, status and protocol, not
the body, packet captures or TLS keys. A successful 2xx/3xx HTTP/3 response is
required; a redirect response is recorded without following it.

Every positive case also requires a new helper UDP record with the exact PID,
executable, application tuple, target, rule, interface, outcome and expected
observed name. The record must have an outbound pre-NAT tuple and at least the
client-observed encrypted payload counts in both directions. The helper may
count additional peer datagrams after the client closes; these are not exposed
as a public NAT port or application payload measurement. Capture identity must
remain Running. The report and independent Stop/baseline cleanup use the same
failure path as the TCP and DNS stages; default smoke and rebaseline clear the
QUIC smoke gate. Uninstall and independently inspect the owned routes and paths
when the reviewed session ends.

The acceptance tool uses MIT-licensed
[quic-go v0.63.0](https://github.com/quic-go/quic-go/tree/v0.63.0) and
[qpack v0.6.0](https://github.com/quic-go/qpack/tree/v0.6.0), with Go's BSD-licensed
`x/crypto` dependency. They are not imported by the shipped app/helper. Direct
`ClientConn.RoundTrip` avoids the general transport's retry branch; see the
[HTTP/3 client API](https://quic-go.net/docs/http3/client/) and
[pinned transport implementation](https://github.com/quic-go/quic-go/blob/v0.63.0/http3/transport.go).

## Disposable application bundle stage

Application bundles were verified on hardware at `fb94688`: TCP smoke, the main
executable's IP-rule smoke through A, and all six batch cases listed below passed.
Nested helper inheritance, outside controls, parent Bundle ID/domain matching
and parent block priority all
passed their process/tuple/app/rule/exit/byte checks. Uninstall restored the
baseline; temporary apps were deleted and independent system checks were clean.
This result does not establish XPC launch or delegated WebKit attribution.

Add `--app-bundle` to a new `host-ipv4` preparation. It freezes this standalone
rootcheck executable's checksum and size, the existing TLS endpoint, both
Info.plists and the relative fixture layout in `bundle-plan.json`. No app is
launched during preparation and no capture prefix is added. The ordinary TLS
preflights must provide public-IP echoes for A and the system path, as for
app-bypass; drift is supplementary evidence under the same policy.

After reviewing the private plan, installing and passing default TCP smoke:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bundle
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bundle-batch
```

The first command makes one main-executable request through A. Only a successful
smoke with verified cleanup enables the six-case batch: nested helper under the
parent IP rule, outside control under that rule, main and helper under the parent
identifier/domain rule, helper under block priority, and the outside control
under that block. The path-only and identifier-only controls are distinct; the
latter names a nonexistent relocated app path but the real parent's identifier.

Each invocation copies the reviewed pure-Go executable three times beneath a
fresh mode-0700 `bundle-fixture-*` directory inside the private session: the
outer app's `Contents/MacOS`, a nested helper app with a deliberately different
identifier, and a control outside either bundle. Each child makes exactly one
unbound, certificate-verified TLS request to the frozen literal address, with
an eight-second total deadline and at most 4 KiB of response data. There is no
Launch Services registration, XPC activation, shell, authorization dialog or
browser launch. Both ends verify the source/fixture checksums and metadata.

Proof includes the actual spawned PID/path, application socket tuple (including
TLS rejection), outermost bundle path/identifier, rule, outcome, interface and
bytes. Successful routes also require an outbound tuple; blocks require none.
Each child stays alive for attribution verification until its stdin is closed,
has a 25-second lifetime, and is waited for on success, rejection and timeout.
The parent removes only its newly created fixture directory on every ordinary
return; reports and the source executable remain. If the controller itself is
killed, EOF/child lifetime still release its processes, but the private fixture
files may remain and require removal with the session.

Default smoke, rebaseline and uninstall clear the bundle smoke gate. The shared
Stop/cleanup/report path still applies, including after a fixture integrity
failure. This is bundle containment acceptance, not evidence of XPC launch or
delegated WebKit sockets. Safari requires its separate reviewed observation;
background URLSession is outside this stage. Synthetic policy tests
and real copied Go protocol children cover preparation, tampering, attribution,
block priority and process cleanup without accessing a helper or network.

## Bounded Safari/WebKit observation

TCP smoke and delegated Safari/WebKit socket and exit routing were verified on
hardware at `5a60671`, with one explicit LaunchServices open to a stable IPv4-only
endpoint and Private Relay disabled. The destination was recorded by its frozen
ordinal; page-body content
was not automatically verified. Uninstall restored routing and helper state.
Subsequent process review found Safari and a WebKit service left running by the
former single-open implementation; the
earlier result therefore did not establish process cleanup. Installation, TCP
smoke, delegated socket/exit observation, automatic browser cleanup and uninstall
were subsequently verified on hardware at `936c6a7`. Cancellation was not tested.
Independent process review confirmed Safari and every WebKit process in its newly
created coalitions had exited.

The independent audit also listed unrelated application renderers, system services
and launchd-managed Safari agents. On-demand bookmark, safe-browsing and other
Safari agents can run in their own launchd-job coalitions, allocated before this
observation, and launchd may restart them while Safari is closed. These are expected
system/app services, not leftovers owned by this test. Other applications' older
WebKit services were preserved. Cleanup ownership depends on process identity and
coalitions; the audit's total count is not a failure or an instruction to kill.
Earlier runs remained incomplete: even freezing every A record in an
answer missed later answers from a larger rotating CDN pool.

The multi-host startup also exposed a RIB `ENOMEM` size/copy race. Extra buffer
headroom and bounded sysctl retries now tolerate growth; ownership observation
retries for up to two seconds, while a readable ownership fault still stops
immediately. Hardware TCP smoke then activated all frozen hosts successfully.

Add `--webkit-app '<absolute Safari .app path>'` to a new `host-ipv4`
preparation. It resolves the app's canonical path, requires the Safari identifier,
and freezes its Info.plist/executable hashes and one nonce-bearing HTTPS URL in
`webkit-plan.json`. The first system-resolver IPv4 answer freezes every distinct
A record as a `/32`, sorted numerically. Two additional A queries compare sets,
and one AAAA query checks for an IPv6 alternative, all within the same five-second
lookup deadline. Warnings are printed and retained in the private review plan.
These advisory queries never expand the frozen first answer.
All addresses must be public, and the separately reviewed TLS target must be in
the set. Empty, unsafe or oversized results fail preparation. The whole scoped
installation, including any other optional stage, may contain at most 16 hosts.
Review every frozen host before installation. No browser runs during preparation.
Later commands never resolve again or add new records automatically; preparing a
different answer requires a new reviewed installation. This is the current
resolver answer, not a promise to enumerate every address a CDN may return later.

Select a small stable A-record set and **no AAAA records**. Safari may prefer an
available IPv6 destination, bypassing this IPv4-only capture. Rotating subsets of
a large CDN address pool are unsuitable. Any set-change, comparison failure,
AAAA result or unknown AAAA status needs review before running; choose a suitable
endpoint rather than retrying navigation or widening scope. Short or cached DNS
samples cannot establish future stability even when no warning appears.

Private Relay is a direct-connect prerequisite, not a tested capability of this
origin-only scope. No documented, reliable read-only per-Safari-navigation status
detector was established; the preflight and report explicitly say `unknown`.
Undocumented `networkserviceproxy` preference/status values cannot establish
whether this particular site/connection uses the relay, so the harness does not
read or interpret them. Establish that the test navigation will connect directly
before starting; if it is known to use Private Relay, stop
at prerequisite review instead of running the timed observation. Apple describes
device/network/site exceptions in its [Private Relay guidance](https://support.apple.com/102022).
The tool never changes that setting, queries an account/keychain, or opens an
automation authorization dialog. IPv6 preference, DNS changes, native connection
reuse and other proxies can still produce an incomplete observation.

After private plan review, installation and default TCP smoke, choose exactly one
trigger for an observation of at most 60 seconds:

```sh
# Manual: navigate once to the printed URL after READY.
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --webkit 45s
# Explicit alternative: one LaunchServices navigation after READY.
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --webkit 45s --webkit-open
```

The second form first records the process inventory and **refuses an already
running Safari**. Use manual mode, or confirm that Safari has exited before
retrying this command. After READY it rechecks that prerequisite,
then executes `/usr/bin/open -a <frozen app> <frozen URL>` exactly once. The
short-lived `open` child is waited for with a five-second limit. There is no
AppleScript/osascript, Automation control, private-window creation, tab inspection
or tab closing. Do not independently launch Safari during this automatic stage.
Manual mode never starts or terminates a browser; its test tab remains the
user's responsibility.

On every automatic-open return, including incomplete evidence, errors and
cancellation, a separate fifteen-second cleanup deadline applies. Identify the
new Safari by the frozen executable, installing UID, kernel start time, unique
process ID and exec generation. Send only that Safari SIGTERM and wait at most
five seconds. Safari is never force-killed. Then observe a full three-second grace
for its new WebKit WebContent/Networking/GPU services, including queued launches;
only still-present, independently owned services may receive SIGKILL. Wait at most
two more seconds to confirm their exit. A failed open can still have launched
Safari; discovery during cleanup is bounded to two seconds.

Services must have launchd as parent, the same UID, a trusted system WebKit path,
a start time after the open attempt, and both coalitions matching that Safari.
Its jetsam coalition must not have existed in the initial inventory. Name and
time alone cannot authorize termination: another application's new WebKit process
must remain untouched. Before every signal revalidate identity; the kernel's
audit-token signal checks PID plus exec generation atomically. There is no fallback
to a numeric-PID kill or process-group signal. Missing metadata, ambiguous ownership,
a stubborn Safari or surviving owned service makes cleanup incomplete and retains
details for review. Previously existing processes are never cleanup targets.

The private check report records whether Safari already ran, newly owned processes,
signals, normal/forced exits and unowned/uncertain survivors. Observation and cleanup
start/end times are saved in the session. Successful uninstall independently lists
**every** still-present process started in those windows, including unrelated and
system-owned ones, rather than checking only helper names. Repeat that read-only
inspection separately after uninstall:

```sh
"$PITH_TEST_BIN/rootcheck" audit-processes --session "$PITH_SESSION"
```

Review its private report before deleting the session. A listed process is evidence
to investigate, never authorization to terminate it. Missing metadata is retained;
an unfinished recorded window extends through the audit time. This command sends
no signals and does not contact or reinstall the helper.
On-demand Safari agents in pre-existing launchd-job coalitions are expected even
after Safari exits. Do not terminate them solely because their name includes Safari.

The app/IP rule covers every frozen A record and directs fresh Safari TCP/UDP
sockets to A. The observation summary records the actual destination's one-based
`frozenTargetIndex`, not its address; its Connection/PCB remote fields are cleared.
The usual private route snapshots and full helper logs retain their existing
audit format. A successful automatic
observation requires a new post-READY helper record and an independent unprivileged
PCB sample for the exact connected local/remote tuple. Its actual socket owner
must be the WebKit networking process, with a different delegated PID resolving
to the frozen Safari executable and app. Kernel owner/effective PID and PCB
generation, helper process/app/rule/exit, outbound tuple and bidirectional byte
counts are retained privately. Process name alone is never attribution proof.
The [WebKit launcher](https://github.com/WebKit/WebKit/blob/main/Source/WebKit/UIProcess/Launcher/cocoa/ProcessLauncherCocoa.mm)
identifies the networking service; policy continues to use kernel delegation.

Automatic `passed` means **delegated socket ownership and exit routing only**.
The encrypted page body, HTTP response and navigation nonce are not automatically
verified; `pageBodyVerified` remains false. If Safari resolves an uncaptured
address, reuses an existing native connection, or the PCB/delegation cannot be
sampled, the private report records `incomplete` and the command exits unsuccessfully.
It does not retry navigation or expand capture. Independent positive delegation
paired with an incorrect helper route/owner is a failure. A changed Safari binary
invalidates the frozen plan. Stop and baseline verification run on every active
return, including incomplete observations.

One navigation can cause multiple or background browser requests. The observation
bounds capture duration, not Safari's total network bytes, and does not override
DNS, proxies, certificates or ECH. Use the approved small endpoint only. Background
URLSession remains skipped. Assess any manual page evidence separately from the
automatic socket result.

## Bounded throughput comparison

This extension has passed the bounded hardware comparison below, in addition to
fake service/route/usage tests and loopback TLS transfers. Keep the actual endpoint and separate
adapter byte caps in the private session. Privileged steps require administrator
approval. Preparation does not transfer performance payloads.

In a new `host-ipv4` session, add a canonical public IPv4:443 and certificate name
for the permitted performance service, plus explicit total byte caps for A and B:

```sh
# Add to the ordinary reviewed prepare invocation:
--throughput-target "$PITH_SPEED_TARGET" \
--throughput-server-name "$PITH_SPEED_NAME" \
--throughput-budget-a "$PITH_SPEED_BUDGET_A" \
--throughput-budget-b "$PITH_SPEED_BUDGET_B"
```

The target contributes one frozen `/32`, deduplicated if already in the scope;
no other target, DNS capture or full takeover is added. Review the complete scope
and `throughput-plan.json` before installation. The service must implement verified
HTTPS HTTP/1.1 `GET /__down?bytes=N` and `POST /__up?bytes=N`, following the
[service's open client protocol](https://github.com/cloudflare/speedtest/blob/main/src/engines/BandwidthEngine/BandwidthEngine.ts).
Uploads contain generated zeros, never a user file. Replies are discarded, not
saved. Redirects, proxies, DNS resolution during transfer, compression, connection
reuse, automatic retries and replacement endpoints are disabled.

Optional `--throughput-download-a`, `--throughput-upload-a` and corresponding
`-b` flags specify payload bytes per sample. Defaults are the smaller of 32 MiB or
one twelfth of the adapter cap for download, and 4 MiB or one ninety-sixth for
upload. Each phase also downloads a 64 KiB warmup. Download samples are limited
to 64 KiB..256 MiB and upload samples to 64 KiB..64 MiB. Each measured request has
a 15-second deadline; each warmup has a five-second deadline. These are finite
single-stream comparisons, not a duration-based unlimited generator.

After installation and a successful ordinary TCP smoke, run **one adapter first**,
review its report and cleanup, then run the other adapter:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --throughput a
# Only after reviewing the first adapter's completed result:
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --throughput b
```

Each adapter invocation reserves nine requests before the first measurement:
direct bound baseline while the helper is idle, then captured minimal rules and
captured 1001 rules. Every phase has one warmup, one download and one upload with
identical payload sizes. The larger scene has 1000 nonmatching domain blocks before
the same final explicit route; it does not send requests to those domains. Capture
starts only after the direct baseline succeeds. The shared three-minute controller
deadline, helper's five-minute scoped lease, unconditional Stop and baseline checks
still apply. Requests/capture are sequential, never concurrent speed streams.

Reservation files use exclusive creation, private permissions and file/directory
sync. They survive failed requests, interrupted commands, rebaseline and reinstall;
do not delete a claim to rerun. A new attempt requires a newly reviewed session
and accounting for the **remaining** budget. Each socket enforces a combined TLS
read/write cap of payload + 12.5% + 128 KiB, including handshake, HTTP headers,
warmups and failure traffic. The full reservation includes a 1 MiB control allowance
and must fit in half the adapter's reviewed cap. The unused half is overhead
headroom: TCP retransmissions, kernel queues and carrier billing are not measurable
at this application boundary. Reported byte totals must not be described as exact
on-wire or billable bytes. Other applications' background traffic is outside this
test's budget and measurement.

Captured samples require matching helper process, exact local/remote tuple, TCP,
rule, outcome, actual interface, outbound local tuple and byte counters. A verified
HTTP success alone is insufficient. Requests bind the current reviewed adapter for
the direct baseline; captured requests are unbound locally and must follow the
explicit rule. Missing/down adapters fail this stage instead of silently choosing
another link. A changed helper identity, capture token, external topology or VPN
invalidates the comparison and stops further requests. No stage retries failure.

The private report retains payload/TLS bytes, transfer and total elapsed time,
Mbps, exact captured records, cumulative helper CPU and RSS before/after phases,
and a separate two-second idle CPU sample while still running. CPU uses bounded
read-only `ps` for the independently identified helper; no privilege or process
signal is involved. Percentages use one core as 100%, RSS is a point sample, and
serialized connection JSON size is **not** on-disk log volume. Current samples do
not prove sustained capacity or the 500 Mbps target; short transfers, endpoint
limits and link conditions can dominate. Uninstall and independently verify the
usual service, path, route, tunnel and process cleanup after reviewing both results.

### Recorded throughput result

The throughput comparison was verified on hardware at `8a9094f` in a `host-ipv4`
session: install, TCP smoke, the hotspot's three phases, the subsequently gated
wired phases and uninstall all passed. Cancellation was not tested. Independent
checks found no helper or rootcheck process and no change to the default route.
A separately bounded 64 KiB GET and POST had first confirmed HTTP/1.1 success at
the permitted service.

With 32 MiB download / 4 MiB upload samples, a cellular hotspot measured
138 / 23 Mbps direct, 139 / 22 Mbps captured with one rule, and 132 / 23 Mbps
with 1001 rules. Wired Ethernet measured 672 / 154, 680 / 145 and 740 / 160 Mbps,
respectively. The captured phases used about 0.35--0.42 of one CPU core on the
hotspot and 0.90--1.00 wired, with helper RSS of 27 MiB and 30--31 MiB.
Two-second idle CPU was 0.5% / 1.0%. TLS usage was 108.4 / 108.5 MiB, below
the roughly 123.8 MiB reservations; each captured phase recorded three connections
in about 1.9 KB of serialized JSON. Budgets, addresses and interface identifiers
remain in the private session.

No visible capture overhead appeared around 700 Mbps, but this is **not a
sustained 500 Mbps proof**: the wired download samples lasted only about
0.4 seconds. Phase CPU includes setup, download and upload and is not an isolated
steady transfer cost. See the [comparison and limitations](NEXT-ACCEPTANCE.md#recorded-throughput-hardware-comparison).

### Recorded optimization rerun

The optimization comparison was verified on hardware at `20ae914` with the same
privately frozen endpoint and per-adapter budgets as `8a9094f`, retaining the
32 MiB / 4 MiB sample sizes. Both adapters' throughput stages passed. Independent
cleanup checks passed and the default route was unchanged. MTU remained 1500.
This run combines indexed binding checks and output
buffer reuse; it does not attribute the hardware improvement to either alone.

Application rates in Mbps (download / upload):

| Interface class | Phase | Before | After |
| --- | --- | ---: | ---: |
| Cellular hotspot | Direct | 138 / 23 | 152 / 19 |
| Cellular hotspot | One rule | 139 / 22 | 145 / 14 |
| Cellular hotspot | 1001 rules | 132 / 23 | 152 / 15 |
| Wired Ethernet | Direct | 672 / 154 | 710 / 45 |
| Wired Ethernet | One rule | 680 / 145 | 773 / 81 |
| Wired Ethernet | 1001 rules | 740 / 160 | 335 / 78 |

Helper CPU for the one-rule / 1001-rule phases fell from 42% / 35% to
23.6% / 19.6% on the hotspot, and from 90% / 100% to 49.5% / 48.4% on wired
Ethernet, where 100% means one core. RSS was about 27--28 MiB and 29 MiB,
respectively. Idle CPU was 2.0% on the hotspot and 1.0% wired, versus 0.5% and
1.0% previously; this run does not establish an idle improvement.

The **one-rule** CPU% per download-Mbps ratio improved by approximately 46% on
the hotspot and 52% on wired Ethernet. Phase CPU also includes setup and upload,
so this is not isolated steady-state per-byte cost. Upload was noisy: wired direct
upload fell to 45 Mbps. The wired 1001-rule download was a single 0.80-second
sample at 335 Mbps; its normalized CPU ratio did not improve, and it is not
discarded from the record. Short-sample and network variation are plausible,
not proven explanations. This remains no proof of sustained 500 Mbps.

Source inspection confirms that TCP rule matching occurs during connection
establishment, including the block precheck and post-sniff route selection;
the payload-copy loop does not scan the rule list. UDP matches per new session.
Rule compilation, validation and new-connection matching can still cost more
with 1001 rules. See the [rule-count analysis](PERFORMANCE.md#rule-count-cost-is-at-connection-setup)
and complete comparison in PERFORMANCE.md.

## Manual VPN reconstruction during full IPv4 capture

This stage was verified on hardware at `fbb5234`: full IPv4 capture
stayed running through a user-controlled Tailscale disconnect/reconnect without
an exit node, and final Stop and cleanup passed. It requires the separate `full-ipv4` profile
with exactly a 120-second helper lease. VPN changes are manual.
Neither prepare nor check runs a VPN CLI, starts, stops, updates or reconfigures
the VPN, changes an exit node, queries peers, or connects to a DNS answer address.

The first hardware attempt with `b8f8dee` passed installation, TCP smoke and the
connected controls, but was incomplete during withdrawal. The DNS datagram was
sent and unanswered for its two-second bound; a context-triggered socket close
returned `net.ErrClosed` instead of an I/O timeout. This was a harness
classification error, not evidence that Pith forwarded the query. Only a close
after the DNS query's own deadline is now classified as a timeout; parent
cancellation and unrelated closes are not. Real unanswered loopback UDP tests
cover both deadline/close orders, preserving the raw error in the report.
Stop was immediate and independent inspection found no owned capture residue;
post-uninstall comparison remained incomplete because the VPN was still off.

The final successful run observed withdrawal 3.3 seconds after READY. The own-name
DNS datagram received no reply and timed out after 2.00 seconds, without entering
a Pith forwarding session; the system public control immediately used wired
Ethernet. RECONNECT appeared at 13.4 seconds, routes returned at 18.2 seconds, and
the first rebuilt MagicDNS query succeeded 0.02 seconds later. Both the explicit
physical rule and the system public control passed after reconstruction.
RESTORED was followed by clean Stop: state Stopped, `endedAt` set, no route
diagnostics, a successful post-Stop own-name query, and both `controlsPassed` and
`postStopPassed` true. There were zero unreachable connections in the final run.

The only external change was the VPN retaining its tunnel name and addresses
with a new interface index and the same 13 routes. After review of that difference,
explicit `rebaseline vpn-rebuilt` and uninstall passed.
Independent inspection confirmed no helper, split capture routes or Pith tunnel,
ordinary wired default routing, working public HTTPS and local MagicDNS.

Changes leading to the verified result:

* `348214c`: classify a socket close at the DNS query's own deadline as timeout.
* `64d1214`: allow bounded MagicDNS settling inside the original rebuild window.
* `7b04e33`: exclude scoped VPN defaults from system-egress settling.
* `fbb5234`: retry bounded closure verification instead of permanently retaining one transient snapshot error.

This result covers only the local own-name/Quad100 and public controls. It does
not establish remote peer reachability, daemon self-update/restart, or continuity
of long-lived connections across reconstruction. No peer scan or VPN CLI was used.

Before the session, the user enables Tailscale **without an exit node** and keeps
it connected through prepare, install and ordinary TCP smoke. Obtain this Mac's
full MagicDNS name independently and supply it privately, along with the
device-local Quad100 resolver and an ordinary public HTTPS control.
The actual name, addresses and adapter identifiers never belong in repository
examples. Add these options to the reviewed prepare invocation:

```sh
--capture-mode full-ipv4 --test-lease 120s \
--vpn-dns-target "$PITH_VPN_DNS" \
--vpn-own-name "$PITH_OWN_MAGICDNS_NAME"
```

Prepare requires a more-specific unscoped VPN route to that resolver and a
physical system route to the public control; an unscoped tunnel default is
refused. One A query must return only addresses actually assigned to this Mac's
VPN interface. Freeze that answer, name, interface identity, routes and DNS in
`vpn-plan.json`. These options do not add capture prefixes, override the
production DNS plan, query another node or change any DNS settings.

After plan review, cancellation testing, administrator-approved installation and
default `check` smoke, run one explicit attempt:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --vpn-restart
```

The command prints these prompts for the manual transition:

1. `READY:` means the pre-capture and running controls passed:
   **turn Tailscale off, keep it off about 15 seconds, then turn it on once.**
   Observe route withdrawal for at most 45 seconds; advance immediately on actual
   withdrawal, not a fixed sleep. A down flag with retained VPN routes is not
   withdrawal evidence. The printed window can be shorter if setup consumed
   lease time.
2. While disconnected, send one bounded own-name DNS query and one public system
   request; continue polling throughout these requests. The DNS datagram must
   time out with no exact helper-forwarded tuple, while the public request must
   use the expected system exit. Require at least ten seconds of observed
   withdrawal. An early reconnect or missing evidence is **incomplete**, not a
   Pith network failure; Stop and ordinary cleanup still run.
3. `RECONNECT:` means the disconnected controls/minimum interval completed.
   Reconnection has at most 45 seconds, shortened to leave 15 seconds for final
   controls before the controller deadline. The prompt reports the actual bound.
   Detect recovery from current routes/interfaces even if the name/index changed.
   Routes can return before MagicDNS is ready: allow up to three own-name queries,
   each bounded by two seconds, with one second between attempts. Route recovery
   and all attempts share the original rebuild deadline; do not start a new
   window when the route appears. Check every attempted tuple for Pith forwarding,
   retaining failed attempts and rechecking the final log after Stop. A wrong
   own-address answer, forwarding evidence or capture fault ends the stage without
   retry. Repeat the physical rule and system public request after DNS succeeds,
   requiring the original Pith capture token, interface, prefixes and Running
   timestamp. Exhausted DNS attempts are incomplete, followed by immediate Stop.
4. `RESTORED:` means those active controls passed. The harness immediately stops
   capture, checks cleanup and performs one final own-name query. This prompt is
   not a cleanup pass; inspect the completed private check/cleanup reports.

The command uses the existing 110-second controller deadline and the independent
120-second helper lease; it never extends either for a human delay. The two
45-second maxima are not additive guarantees. Any timeout/cancellation is
incomplete and ends capture.
The durable attempt file prevents automatic or accidental repetition in this
session, even after failure/rebaseline. If aborted while disconnected, restore the
VPN manually after confirming Pith is stopped. Any
failure or incomplete result after READY prints exactly one terminal reminder:
`RECONNECT: test ended; turn the VPN back on now`. This also covers errors in
final verification. The reminder never waits for user action or delays Stop.

There are at most seven own-name A queries in check (one before capture, one running
before, one withdrawn, up to three rebuilt, one after Stop), plus the single prepare
query. Each sends one classic DNS datagram with a two-second deadline and no
retransmission/fallback. Only the rebuilt phase permits another separate query,
spaced by one second; even its maximum eight-second query/spacing budget must fit
inside the remaining rebuild window. Four extra
public HTTPS requests use the frozen endpoint and three-second bounds, in addition
to the existing SafeStart/common system control. No polling request is sent to a
tailnet peer: route/status polls are local reads and capped at 512 observations.
All request workers are canceled and joined before returning.

The response deadline is also applied to the UDP socket. If context cancellation
wins the read race, `net.ErrClosed` counts as negative evidence only when this
DNS operation's own deadline expired and its parent is still live. A sent query,
zero response bytes and no matching helper-forwarded tuple remain mandatory.

Private reports retain the prompts' times and actual windows, independent
snapshots, local VPN name/index and routes, exact DNS tuples/replies, public
connection records and the unchanged capture identity. Route recovery and DNS-ready
times are separate, with every rebuilt DNS attempt retained. The API does not expose
the internal egress generation; it is not inferred from timestamps. If Stop
reports only a verified external VPN change, active controls and cleanup remain
separate results: inspect the differences, then explicitly
`rebaseline --label vpn-rebuilt --accept-external-change` while stopped before
uninstall. No automatic baseline adoption or route repair is allowed.

The intended final order is **review the private differences, rebaseline, then
uninstall**. Rebaseline requires this session's helper to remain installed,
identity/version-matched and idle; it needs no root or authorization and never
starts capture. It rechecks cleanup evidence, both reviewed adapters, stable
snapshots and three bounded system/A/B TLS preflights. No additional smoke is
needed if the next action is uninstall; a new smoke is required only before
additional capture checks. A changed frozen capture plan or ambiguous ownership
still blocks rebaseline. If VPN state changed again after check, first run `stop`
to refresh its cleanup evidence before reviewing/rebaselining.

If the test ends with the VPN disconnected, keep Pith stopped and ask the user
to restore the VPN before the final baseline comparison. With the idle helper
still installed, run `stop` again after restoration to archive fresh cleanup
evidence. If only independently verified external reconstruction differences
remain, review them and explicitly rebaseline as above, then uninstall once.
Do not rerun capture to repair or conceal a failed test.

If uninstall already removed the helper while the VPN was still down, do not
repeat authorization. After the user restores the VPN, repeat only verification:

```sh
"$PITH_TEST_BIN/rootcheck" verify-uninstalled --session "$PITH_SESSION"
```

This reads system state, archives the snapshot privately, and verifies all five
managed paths and the launchd service are absent and the accepted routes/tunnel
inventory match. It contacts no helper, runs no removal script, and reads no
credential. Success clears the stale installed/test flags and runs the independent
process audit. If a rebuilt VPN has a different identity and strict comparison
still fails after removal, keep the snapshots for independent review;
this command does not silently adopt external changes or claim a cleanup pass.

Quad100 is a [device-local service](https://tailscale.com/docs/reference/quad100);
an [own-name MagicDNS answer](https://tailscale.com/docs/features/magicdns) does
not prove remote peer reachability. This stage establishes local DNS and route
withdrawal/reconstruction continuity only. Absence of a forwarded connection
record plus a query timeout is not a wire capture proving every packet was
contained. Existing long-lived connections, remote tailnet destinations, a
daemon self-update/restart, and independently damaged capture routes remain
separate acceptance items. Do not broaden the test to obtain any of those claims.

## Ordered execution

Stop after the first failed case. Once a check enters its active test phase,
every outcome invokes bounded cleanup; inspect the private report and current
baseline before deciding on another test. Cleanup rechecks installation identity
before sending `Stop`, and refuses to stop a replacement helper. If Pith cleanup
fails or its ownership cannot be established, use the reviewed removal procedure
and do not retry capture. A separately proven external-only change can instead
use the explicit acceptance procedure below; it is not a successful check.
Concurrent commands on the same session are refused by an advisory lock.

### Administrator approval

Installation, final uninstall and an optional deliberate crash require
administrator approval. Cancellation is a separate test. Ordinary `check`, `stop`
and `rebaseline` do not require additional approval. Emergency recovery is an
alternative removal operation, not an automatic extra step.

| Step | Command | Dialog and changes | Pass condition and undo |
| --- | --- | --- | --- |
| 1. Cancellation | `"$PITH_TEST_BIN/rootcheck" cancel --session "$PITH_SESSION"` | Cancel the administrator dialog. | Returns `ErrCanceled`; no managed path, service, route, or utun changes. If the helper is installed, this case fails; uninstall it before preparing a new session. |
| 2. Installation | `"$PITH_TEST_BIN/rootcheck" install --session "$PITH_SESSION"` | Requires administrator approval. Creates the root-owned helper and launchd plist/data directory, plus a listed shared parent only if absent; the daemon creates its authenticated socket. It starts idle. | Version and executable checksum match; plist arguments contain only the reviewed host captures; plist inode/mtime identify this installation; engine is stopped; routes and utun inventory match the initial snapshot. Undo is step 9 or emergency cleanup. |
| 3. One smoke check | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION"` | No dialog. Saves only a temporary config, starts the scoped utun with SafeStart, requests HTTPS through ordinary system routing, then hot-applies an IP rule for interface A. | SafeStart succeeds; both logged flows belong to this test process; outcome, rule, interface, bidirectional byte counts, and live traffic are correct. `Stop` restores routes and utun inventory before the command returns. Only a successful smoke check unlocks later cases. |
| 4. Rule batch | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --batch` | No dialog. Starts a fresh bounded capture and changes only the test scene. | Verifies interface B, TLS SNI domain matching, the second target of one rule, block priority over an earlier route, missing-interface fallback, explicit system fallback, and strict unreachable. Opens TLS on A, records its identity, changes the rule to B, verifies a TCP reset and normal `Ended` record with `EndReason=rules_changed` and empty `Problem`, then verifies a new connection on B. The capture start timestamp stays unchanged during hot reload. Ends stopped. |
| 4a. Optional app-specific routing | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bypass` | No dialog. Requires a passed smoke check and public-IP preflight evidence. Uses the current standalone `rootcheck` executable path as the app/tool identity, and creates a checksum-verified copy at a different path inside the private session directory. Each controlled child makes exactly one ordinary TLS request to the same frozen target. No helper installation, capture expansion, or VPN setting change occurs. | The parent matches app+IP and then app+domain rules through physical interface A. Under each rule, the different executable remains unmatched and uses the saved system route. A final app block after the route rule wins. Every case checks PID, executable/app path, destination, rule, outcome, interface, and byte counts; successful requests use the public-IP drift policy below. Children are acknowledged and waited for on every outcome, their copy is removed, and routing ends stopped. Identical public exits do not prove a distinct exit-node bypass. |
| 5. Native TCP preservation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --preserve` | No dialog. Establishes an ordinary TLS connection **before** capture, starts capture, then sends HTTP over that same native connection. | The pre-existing kernel connection survives route activation and receives a verified HTTPS response. This flow is not claimed as a new proxied/logged connection. Ends stopped. A server that closes idle TLS before the request makes this case inconclusive; do not count it as a pass. |
| 6. SafeStart rollback | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --safe-failure` | No dialog. Starts with a block rule for the exact SafeStart destination and SafeStart enabled. | `Start` returns `ErrNetworkLost`; status reports the same failure; routes/utun are restored. Cleanup calls `Stop` so the next command sees an idle helper. |
| 7. Optional observation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --observe 90s` | No dialog. Runs the smoke checks, then holds the reviewed host capture for at most 90 seconds. See the manual cases below. | The harness records an observation window, not a pass for an unimplemented manual assertion. It polls helper status and stops on interruption, failure, or expiry. |
| 8. Explicit stop | `"$PITH_TEST_BIN/rootcheck" stop --session "$PITH_SESSION"` | No dialog; authenticated IPC only after verifying the installation belongs to this session. Safe to repeat while this helper is installed. | Status is stopped at the reviewed version; route snapshot and utun inventory match the latest active baseline. A successful comparison is persisted as cleanup proof. If IPC is broken, use emergency cleanup instead. |
| 8a. Optional next mode | While stopped, the user selects the approved Tailscale mode; then `"$PITH_TEST_BIN/rootcheck" rebaseline --session "$PITH_SESSION" --label tailnet` (or `vpn-off` / `exit-node`) | No dialog. Verifies the same idle installation and the preceding cleanup proof; takes new snapshots and makes three bounded HTTPS requests to the frozen target through system/A/B. Writes private review files only; it does not change VPN settings, capture scope, or routing. | Both reviewed adapters resolve and are up; helper identity/version remain unchanged; snapshots are stable across preflight. Only then does the new baseline become active, with smoke/pass state cleared. Repeat step 3 before any further case. A failed rebaseline leaves the previous active baseline and all history intact. |
| 8b. Reviewed external change | `"$PITH_TEST_BIN/rootcheck" rebaseline --session "$PITH_SESSION" --label vpn-rebuilt --accept-external-change` | No dialog or routing change. Use only after reviewing an `external-change` cleanup report; this is not a general mismatch override. | Pith's exact capture prefixes are absent; every reused name/index has independent whole-VPN migration evidence; the same idle installation and stable preflight checks pass. Archive both the prior proof and current differences, then repeat smoke. The original failed check remains failed. |
| 9. Final uninstall, once | `"$PITH_TEST_BIN/rootcheck" uninstall --session "$PITH_SESSION"` | Requires administrator approval. Attempts `Stop` on the verified installation, then independently boots out launchd, checks stale capture ownership, and removes managed files. It does not require working IPC or a matching IPC version. | Helper, service, socket, journal, and data directory are absent; the latest baseline's routes and utun inventory are restored. Shared parent directories and private session artifacts remain. A clearly different installation is refused before authorization; review ownership and the standalone emergency command instead. |

macOS supplies the dialog title, buttons, and authorization behavior.
A missing cancellation dialog is not a cancellation pass. No
update dialog is expected because the harness refuses an existing installation;
testing helper updates needs its own reviewed case.

All snapshots and detailed reports, including real addresses and MACs, stay
under `PITH_SESSION`. Do not attach or commit them without redaction. The
harness compares route and utun snapshots strictly against the active baseline.
Snapshots exclude `RTF_WASCLONED` hosts and non-static `RTF_LLINFO` ARP/NDP entries;
ordinary traffic refreshing those caches does not change the baseline. Static
non-cloned hosts remain even with LLINFO, as do cloning parents and VPN routes. Another
VPN, interface change, or configured-route change can make the result
inconclusive; the harness never removes an unrelated route to force a pass.
Connection records (process, target, rule, outcome, interface and bytes) are the
primary path evidence. Public-IP echoes supplement them: an unchanged expected
exit passes; a changed echo that matches no other system/adapter preflight passes
with `exitIPDrift` and an explanatory note. An echo matching another preflight
exit fails. Missing/invalid echoes fail when a preflight supplied one. A shared
expected exit still cannot distinguish two paths by itself. The same policy
applies to app-bypass requests; carrier NAT rotation does not by itself fail a
correctly attributed connection.

Every check also archives up to 500 connection records started during that check,
queried after cleanup with an independent three-second bound. These retain
outbound errors from SafeStart even when startup never reaches Running or its
final event was not delivered. `connectionLogError` records an unavailable log;
missing diagnostics never turn a failed probe or failed cleanup into a pass.
The report also copies `Status.RouteDiagnostics` after Stop, independently of
subscription delivery. Up to 32 distinct entries retain RTM type, flags, index,
sockaddr families/lengths and the decision for unusual messages. They contain
no raw addresses, interface names or packet contents. Startup failures retain
them; the next capture attempt or helper restart clears them. An external scoped
event can be recorded while capture continues only if independent ownership
verification succeeds; an ambiguous capture-affecting event still stops routing.

### Optional hotspot renewal during observation

Only an explicit `check --observe 90s --renew-hotspot` renews interface A. It
requires prior smoke, a connected unlocked M2000, and public-IP preflights.
Ordinary `--observe` never contacts the hotspot. Review this separate device
action first: it briefly interrupts the hotspot's cellular connection and uses
the app's existing stored credential through `client.RenewHotspot`. It requires
no additional root authorization and does not set or replace any password.
Do not run it while keychain/device operations are suspended by the user.

After the current capture's system/A checks, the harness archives intent, calls
renewal once, and polls the same capture identity/start time every 250 ms. A
stopped, failed, restarted or replaced capture fails the case. After renewal it
requires the client's successful A probe, a fresh bound request to the frozen
endpoint, and a new ordinary request with the expected A rule/connection record.
The latter uses the fresh bound echo under the same drift policy. The private
report retains all evidence, including the client's independent probe addresses.
An unchanged public IP is reported as observed; renewal cannot promise a different
carrier NAT allocation. This automated case returns as soon as verification
finishes; `--observe` is its upper time bound, not a minimum wait.

Cancellation waits for the client's bounded cellular-data restoration before
Stop; no renewal worker remains running after return. Stop still checks owned
capture removal. A's changed IPv4 address or IPv6 prefix/connected routes produces
external-change evidence, never a Pith-residue claim or an automatic baseline
rewrite. IPv6 privacy rotation within the same prefix alone remains harmless.
Inspect the private changes and explicitly rebaseline with
`--accept-external-change` before the next smoke or final baseline comparison.
There is no automatic retry, second renewal, or device reconfiguration to undo
the new carrier-assigned addresses.

Every capture start first clears and saves the previous cleanup proof, then
durably records a pending attempt before calling Start. Failed Stop, non-stopped
status, identity/version mismatch during cleanup, or a missing snapshot cannot
produce usable proof. Unequal routes/tunnels cannot produce a restored-baseline
proof; only the separately classified external-change exception below is eligible
for explicit acceptance. A test assertion
failure still fails the check, even when independent cleanup succeeds and records
proof; review that failed test before continuing. A read-only check rejection
before capture or cleanup begins preserves the preceding proof, so accidentally
running `check` after an approved VPN switch does not prevent `rebaseline`.
The proof is bound to the session directory/UID, reviewed binary/plan/targets,
installation identity, capture history, and current baseline. An older or copied proof is refused.
File identity is rechecked immediately before Start/Stop, but it is not an atomic
IPC installation token: keep the app and all other installers closed throughout.

Tunnel cleanup compares the exact sorted utun inventory and its indices within
the same boot, not its changing addresses or display metadata. An additional,
missing, renamed, or reindexed utun still fails the restored-baseline comparison.
Removal checks require all five managed paths to have
been inspected and found absent, with no inspection error; a missing or repeated
path entry cannot pass. The managed launchd service must also be absent, but
its diagnostic output text is archived rather than compared for equality.

## External VPN changes during a check

A VPN can rebuild its own tunnel while Pith is running. A mismatch alone does
not prove that Pith left something behind, but it must never be silently ignored.
The helper's authenticated `Status.LastCapture` reports its most recently
prepared capture's boot identity, journal token, utun name/index, exact capture
prefixes and selected tunnel subnets. `EndedAt` becomes nonzero only after its
device closed successfully and a subsequent snapshot found no owned capture
routes. It survives Stop and SafeStart failure within that helper process.
The harness records each receipt, including failed Start attempts, in the private
session history. A helper restart can clear its in-memory receipt; receipts
already saved by the controller remain available. They never authorize deletion.

After successful Stop, the harness may archive **external-change** evidence only
when all of the following hold:

1. The reviewed binary/plist installation still matches, and the helper responds
   at the reviewed version in `Stopped` both before and after the snapshot.
2. Capture ownership history is complete, with no pending/unobserved Start. Every
   receipt matches the frozen host prefixes and current boot. Any current helper
   receipt is already in this session's history.
3. No exact capture prefix exists on any interface, including scoped routes.
   A reused Pith name or index requires the independent whole-VPN migration
   evidence below; an unused or unexplained leftover interface still fails.
4. Complete, consistent interface-name/index inventories accompany both
   snapshots. Every added/removed route and changed interface belongs solely to
   interfaces outside Pith's ownership or to a proven foreign migration.

A new name/index reuse needs all of these additional observations:

* The current helper's latest known capture has a nonzero `EndedAt` preceding
  the independent snapshot. Every recorded capture that used the reused name or
  index has a confirmed end. A cleared receipt after helper restart cannot prove
  a new reuse, even though ordinary non-reuse cleanup remains possible.
* Complete assigned-address inventories cover every interface, including loopback
  and link-local addresses. No address or specific route uses any recorded Pith
  tunnel subnet, and no route uses an exact capture prefix. Missing subnet or
  address evidence cannot be reconstructed from a guessed tunnel name.
* Exactly one disappeared baseline VPN matches the reused interface's complete
  global assigned prefixes and routes after normalizing only interface name/index
  and display label. It must be up and have at least one global address and route.
  Different addresses, an additional route, a surviving source VPN, duplicate
  candidate identities, or an unconfirmed earlier capture make reuse ambiguous.
  Link-local addresses are retained and checked for Pith residue, but are not
  stable migration identity. There is no broad IPv6-address-churn exception for
  the global addresses used as migration evidence.

The private report includes the matched source/destination name/index pairs,
global prefixes, normalized route set, and complete original/after snapshots.
After explicit acceptance, the new stopped baseline may retain that foreign
interface: captures confirmed ended before this baseline do not taint it forever.
A later capture using the same name/index must establish new evidence.

This report has `restored: false`, `passed: false`, a separate external-change
classification, and full route/interface/index differences. The command still
returns a failure so a test interrupted by a VPN change cannot become a pass.
Review the private report, then use step 8b explicitly. Without that flag,
rebaseline refuses this kind of proof. The flag cannot override missing evidence,
a Pith residue, an unknown capture, a different installation/version, a down
reviewed adapter, a scope conflict, or a failed/unstable preflight.

Rebaseline independently classifies the current snapshot again, makes the same
three bounded requests, and requires stable routes, interface indices and
topology throughout. It archives the accepted proof reference and current
differences before atomically selecting the new baseline. Previous reports and
failed assertions are preserved; smoke must pass again. It does not adopt a
running capture, delete a route, restart a VPN, widen scope, or require additional
administrator approval. If external state has already returned exactly to the old baseline,
run `stop` to obtain an ordinary restored-baseline proof instead.

An interrupted controller or a failed preparation without a readable capture
receipt permanently disables this exception for that session. Ordinary exact
cleanup comparisons still work. Use the reviewed uninstall path if ownership
remains ambiguous; do not reconstruct missing history from guessed utun names.

## Manual cases and remaining acceptance limits

Run only explicitly approved manual cases during `--observe`, in a second
Terminal. Use the previously selected literal destinations, bounded counts,
and deadlines. No manual command may broaden capture. The harness ends its
observation automatically and treats an unexpected helper stop as a failure.

* **UDP and DNS:** prefer the reviewed [bounded stage](#bounded-udpdns-stage)
  above. If only a legacy manual DNS target was included at preparation, perform one
  ordinary UDP DNS request to it, with a two-second timeout and one attempt;
  repeat once before the session idle timeout, then after it. Confirm responses
  and the expected UDP session creation/byte counts in the app or an authenticated
  client. Check an IP-to-domain result against the same DNS answer. A TCP-only
  route in the smoke scene does not test UDP interface selection; review a UDP
  rule before claiming that separate result. No DNS system setting is changed.
  Source-port preservation for replies from an unconnected UDP listener remains
  unimplemented or unverified; the existing mechanism cannot simply exempt
  wildcard-bound sockets because that would bypass rules. A passing TCP smoke
  test does not establish compatibility with general UDP server applications.
* **ICMP:** first confirm the selected captured destination responds to two
  ordinary pings while routing is stopped. During observation, repeat
  `/sbin/ping -n -c 2 -t 5 '<reviewed captured IPv4>'`. Receiving genuine replies
  in both cases is the pass; a server that filters echo is inconclusive. The
  current engine sends ICMP through the saved system route; TCP/UDP rule
  outcomes and traffic counters do not represent ICMP routing.
* **Crash cleanup:** prefer `check --session "$PITH_SESSION" --crash` after a
  passed smoke check. It holds the session lock, starts the reviewed host capture,
  verifies one system request and the active helper/capture identity, then runs
  the frozen `crash.sh`, which requires administrator approval. It allows ten
  seconds for capture disappearance and an authenticated idle restart, and always runs
  independent Stop/cleanup afterward. Its report is a distinct crash check, not
  a smoke pass. A manually observed crash also requires administrator approval.
  Interrupt the *managed service only* during observation with an administrator
  dialog running `/bin/launchctl kill SIGKILL system/io.github.kel077.pith.helper`.
  Kernel utun ownership must remove capture when the process dies. Launchd may
  restart the daemon, but it must come back idle. The observation command is
  expected to report failure rather than claim a normal pass. Verify the latest
  baseline's routes and no extra utun, then uninstall. Do not use `killall`, a guessed PID,
  or commands that act on other VPN processes. This adds one explicitly
  approved crash-test dialog; it is not part of the automatic smoke sequence.
  Its exact optional command and prompt are:
  `/usr/bin/osascript -e 'do shell script "/bin/launchctl kill SIGKILL system/io.github.kel077.pith.helper" with administrator privileges with prompt "Pith needs your password to test recovery from a helper crash."'`.
* **Scoped lease:** a separately approved five-minute observation by a dedicated
  authenticated caller can verify the helper's hard lease. This three-minute
  harness does not intentionally abandon a running client to test the lease.
* **App bundles and delegation:** use an app that can connect to the selected
  host without adding any capture route, and verify its own process, nested
  helper, and delegated networking process attribution. The automatic tests
  prove attribution to this command-line process only; they do not validate
  WebKit, `nsurlsessiond`, bundle matching, or delegated UUID behavior.
* **Full IPv6, QUIC, existing UDP, loss-driven failover, throughput, and full-capture
  extensions:** these remain separate acceptance items beyond the recorded IPv4/IPv6
  TCP result. The automatic fallback uses a missing
  synthetic interface; it does not prove timeout behavior on a live interface
  that drops packets. Do not infer full IPv6, UDP/QUIC correctness or a throughput
  target from the TCP results. Repeating host IPv6 requires a reviewed
  `/128`. Every new full-capture session still requires administrator approval;
  the completed IPv4 run does not authorize VPN or mixed-protocol extensions.

The [next acceptance plan](NEXT-ACCEPTANCE.md) separates the broader capture,
IPv6, UDP/QUIC, throughput, and app-bundle cases. It authorizes no execution and
does not add the pending protocol or fixture capabilities to this harness.

## Optional Tailscale compatibility phases

These phases belong to the same bounded acceptance session, but they are
opt-in. They do not authorize installing, updating, reconnecting, or changing
Tailscale automatically. Test only an already available Tailscale configuration
and an exit node the user is permitted to use. The user selects **no exit node**
or the approved **exit node** in Tailscale only when that change is included in
the session scope. Keep Pith stopped while changing
the mode. Do not change routes, DNS preferences, or Tailscale settings to make
a failing Pith test pass.

Before the first mode change, record the original Tailscale connection state,
exit-node selection, and DNS preferences in the private temporary session
notes. Also select one already authorized tailnet TCP/TLS service and its
MagicDNS name. Use the same bounded service request and one system-resolver
lookup before, during, and after each Pith run. Do not enumerate the tailnet or
probe additional peers. Real names, exit-node identifiers, addresses, route
snapshots, and public-IP echoes stay outside the repository.

A changed Tailscale mode requires a fresh reviewed route/interface baseline;
a different mode's snapshot is not a valid rollback reference. Keep the same
installed, idle scoped helper throughout this session. After the
initial smoke check, use the following sequence for each approved mode:

```sh
"$PITH_TEST_BIN/rootcheck" stop --session "$PITH_SESSION"
# The user now selects the approved mode in Tailscale while Pith is stopped.
"$PITH_TEST_BIN/rootcheck" rebaseline --session "$PITH_SESSION" --label tailnet
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION"
```

Use `--label exit-node` for the approved exit-node phase, or `--label vpn-off`
for an explicitly approved disconnected phase. Labels are generic bookkeeping,
not commands to Tailscale. `rebaseline` never switches VPN modes or prompts for
authorization. It requires a recorded successful stop/check cleanup **under the
previous active baseline**, the same binary/plist installation, a responding
version-matched helper in `Stopped`, and both reviewed adapters still up. Do not
call `stop` after switching modes merely to establish proof against the old
baseline: establish that proof before the switch, then call `rebaseline`.

The command permits the expected route changes between approved modes. It
compares its new before/after routes and interface topology for stability, and
rechecks helper identity and stopped status after the probes.
Success requires three sequential TLS preflights (system, A, B), each bounded
to eight seconds, with a forty-second command deadline. A unique candidate
file is archived first; only a fully successful operation atomically selects
it in `session.json`. Earlier snapshots, cleanup proofs, reports, and preflights
remain untouched. Failure leaves the previous active baseline in place; inspect
the archived failure and do not run capture against an unaccepted mode.

Adopting a baseline clears `SmokePassed` and the previous cleanup proof. Run a
fresh ordinary smoke check before `--batch`, `--app-bypass`, or any other case.
Do not edit JSON or rerun prepare/cancel/install between modes. All approved
phases share the original frozen targets and the same installation; remove it
once at the end of this attended window.

Interface comparison normalizes interface and IPv4 address ordering. It
preserves interface identity, kind, up/down state, IPv4
address values, both routers, DNS values and priority order, and primary status.
`api.Interface` exposes neither IPv6 prefix lengths nor address lifetimes or
temporary flags, so the harness does not guess which address is temporary.
It compares whether global IPv6 addresses are available, while tolerating changes
to their individual values or count; full addresses remain in the private
snapshots. Separate complete prefix inventories permit privacy-address rotation
within one prefix, but reject a changed assigned IPv6 prefix during preflight
and classify it as external evidence during cleanup. These checks apply to all
capture modes; they do not prove IPv6 connectivity. Route comparison stays
strict: if IPv6 address rotation also changes a non-cache local `/128` or another semantic route, the
command conservatively refuses the new baseline. Thus this normalization does
not eliminate every possible IPv6 churn failure or weaken route ownership checks.

The smoke and batch checks can be repeated within each mode. After the smoke
check passes, the app-specific caller is built into the same harness:

```sh
"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bypass
```

This command uses `api.Service` and the existing temporary config location.
It sets an app filter for the current standalone test executable, routes its
new connections through physical interface A, and runs an unmatched control
from a different executable path under the session directory. The checksum
and copied path are recorded in the private report. The internal `_request`
subcommand only reads the frozen session and makes one TLS request; it does
not construct a client, call IPC, acquire the parent's session lock, or change
configuration. Its independent deadline is 25 seconds. The parent gives each
child at most 30 seconds, verifies its actual spawned PID and reported target,
keeps it alive while checking attribution, then closes its input and waits for
exit on success or failure. The copied executable is removed after use.

The whole check retains its mode's deadline and unconditional Stop cleanup
(three minutes for `host-ipv4`, ten seconds less than the helper lease for the
explicit test profiles).
No normal application config is used, no target or capture scope is added,
and no Tailscale setting is changed. It proves command-line tool identity;
bundle/helper/delegated-process attribution remains a separate manual case.
Tailnet reachability and MagicDNS before/during/after comparisons still need
the bounded manual control requests described above.

| Phase | Additional dialogs and changes | Pass, fail, or inconclusive | Undo |
| --- | --- | --- | --- |
| Initial approved mode | Keep the mode recorded by preparation, including a disconnected mode if selected. Run cancellation and installation once, then the initial smoke and approved cases. | Public-target flows match the initial system/A/B preflight. When Tailscale is disconnected, do not claim tailnet or MagicDNS connectivity checks passed; those apply to the connected phases below. | Finish with verified Stop and retain the idle helper for the next approved mode. Uninstall once after the last phase. |
| No exit node | The user selects no exit node in the existing Tailscale UI while Pith is stopped, only if explicitly approved. Any Tailscale/macOS dialog is version-dependent: record its actual wording and obtain approval before accepting it. `rebaseline --label tailnet` and the fresh smoke check add zero Pith dialogs and retain the same installation. | The chosen tailnet service and MagicDNS lookup work before capture, during a Pith route/block case for the separate public test host, and after Stop. Unmatched public traffic retains the saved system route for this mode. Existing Tailscale routes and its tunnel are unchanged by Pith; Pith selects an unused tunnel address without conflicting with existing interface addresses or tunnel/subnet routes. A failed preflight makes the connectivity case inconclusive. | Stop against this phase's baseline. Only then may the user select the next approved mode and rebaseline. If finished, uninstall once before restoring the initial Tailscale settings. Pith does not disconnect Tailscale. |
| Approved exit node | The user selects the already authorized exit node while Pith is stopped, then runs `rebaseline --label exit-node` and a fresh smoke check. This records the new system and physical public exits without reinstalling. Extra VPN/OS prompts need their actual wording reviewed; rebaseline and `--app-bypass` add zero Pith dialogs. | First verify unmatched traffic to the captured public test host still uses the exit node. Run `--app-bypass`: the main test executable must use the chosen physical interface under app+IP and app+domain rules, while its different-path child remains unmatched and uses the exit node. The final matching block must prevent the parent's new connection even when a route rule also matches. Require captured-flow evidence and the expected physical versus system public-IP echoes; if the echoes cannot distinguish exits, record bypass as inconclusive. Tailnet and MagicDNS behavior remain consistent with the preflight throughout. | Stop against the exit-node baseline, verifying that its routes/tunnel and tailnet service still work. At the end, uninstall once in this mode, then the user restores the initial exit-node choice and DNS preferences. Never remove Tailscale routes as cleanup. |
| Stop and optional deliberate helper crash in either mode | Normal Stop adds no dialog. A crash test is optional and uses exactly the separate crash authorization command and prompt documented above; it must be explicitly included in that mode's approval. | Pith's capture routes and utun disappear, the pre-existing Tailscale tunnel and route ownership remain intact, and the selected tailnet service, MagicDNS lookup, and public-route behavior still match that mode's baseline. A launchd restart of Pith must be idle. An observation failure caused by the intentional crash is expected; record the crash assertions separately rather than calling the normal smoke check a pass. | Use the reviewed Pith-only emergency command if necessary, then uninstall. Do not restart, kill, or reconfigure Tailscale to hide a cleanup failure. |

Every `--app-bypass` request connects directly to the **already reviewed
captured literal host**, while preserving the TLS server name and address
family. It does not edit `/etc/hosts`, change DNS, follow redirects, or add another destination. The
temporary app-specific IP rule is tested first with the parent and then its
unmatched child; the corresponding domain rule is hot-applied and tested with
both identities. The final rule set contains the domain route followed by a
matching app/domain block, and requires a blocked log entry and failed new
parent connection. The capture's start timestamp must remain unchanged.
Reports retain the request PID/path/target alongside the observed connection
and public-IP echo, so a different process or uncaptured destination cannot
masquerade as successful system fallback. Rotating exit addresses or changing
VPN state require review of the failed comparison, not an automatic retry.

For every public-target assertion, first establish that the request actually
entered Pith: its new connection record must identify the intended process,
literal destination, and protocol. A VPN's more-specific or scoped route can
keep a request out of the Pith capture. An exact route conflict must be refused
without changing that VPN route. Missing capture evidence makes the per-app
route, fallback, or block result **inconclusive**, not a compatibility pass or
proof that the matcher failed. Stop and review the route snapshot; do not
delete the competing route, enlarge the capture, or toggle the VPN
automatically. Tailnet control traffic is intentionally outside these public
host captures and does not need a Pith connection record.

After the last phase, stop and uninstall against the latest baseline **before**
the user restores the initial Tailscale settings. Verify Pith's managed
paths/service are absent, then have the user restore the recorded settings.
Compare each Pith cleanup
with its own phase baseline; a user-approved Tailscale mode switch can itself
recreate its interface, so do not invent route repairs to force identity with
an earlier mode's interface index.

## Emergency undo

During preparation, `emergency-command.txt` contains the exact standalone
one-command undo generated by `helper.PlanUninstall`. Copy it before the root
session starts. It does not require working IPC, the test executable, or a
network connection. The following equivalent command is included for review;
it opens the standard removal authorization dialog:

```sh
/usr/bin/osascript -e 'do shell script "set -eu\nservice=system/io.github.kel077.pith.helper\nbinary=/Library/PrivilegedHelperTools/io.github.kel077.pith.helper\nplist=/Library/LaunchDaemons/io.github.kel077.pith.helper.plist\ndata=\"/Library/Application Support/Pith\"\nsocket=/var/run/io.github.kel077.pith.helper.sock\nfor path in \"$binary\" \"$plist\" \"$data\"; do [ ! -L \"$path\" ] || exit 1; done\nif /bin/launchctl print \"$service\" >/dev/null 2>&1; then /bin/launchctl bootout \"$service\"; fi\nattempt=0; while [ -S \"$socket\" ] && [ \"$attempt\" -lt 60 ]; do /bin/sleep 0.1; attempt=$((attempt+1)); done\nif [ -f \"$data/capture.json\" ]; then \"$binary\" --cleanup; fi\n/bin/rm -f \"$binary\" \"$plist\"\nif [ -S \"$socket\" ]; then /bin/rm -f \"$socket\"; fi\n/bin/rm -rf \"$data\"" with administrator privileges with prompt "Pith needs your password to remove its routing helper."'
```

The five paths are the helper binary, launchd plist, data directory, capture
journal inside that directory, and Unix socket. The cleanup sequence boots out
the service first and waits a bounded grace period. If a capture journal
remains, `pithd --cleanup` first checks its boot-session identity. A journal from
a previous boot authorizes metadata removal only, never current route deletion.
For the same boot, cleanup refuses to proceed while the owner PID is alive,
then deletes only journaled routes whose prefix, utun name, and interface index
still match. Legacy journals without a boot identity cannot authorize route
deletion. Closing the owning helper removes its utun. It never runs
a global route flush, changes the system default route, or destroys another
utun. If ownership cannot be established or the helper binary is missing,
cleanup fails and preserves the remaining journal for review; do not replace
it with broad route deletion.

Finally verify that the managed launchd service and all five paths are gone,
and compare the latest accepted route and utun baseline. Remove the private session
and build directories only after that review. No helper, watcher, test socket,
or test process should remain running when the session is complete.
