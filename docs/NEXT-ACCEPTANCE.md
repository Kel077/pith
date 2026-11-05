# Next acceptance plan

This document plans future attended tests. It authorizes no administrator
operation, network change, benchmark, VPN restart, or application launch. The
current `rootcheck` defaults to frozen public IPv4 host captures. It also has a
separate `prepare --capture-mode full-ipv4 --test-lease 120s` installation with
an engine-enforced deadline and frozen LAN-DNS routes. A separate
`prepare --capture-mode host-ipv6 --test-lease 60s` freezes one public IPv6
`/128`, requires global IPv6 on both adapters, and uses IPv6-only preflights and
SafeStart. All modes have synthetic coverage; the completed hardware coverage
is recorded below. The external-change flag cannot widen an installation.
The scoped UDP/DNS extension has passed hardware acceptance with its separate
one-query smoke gate and bounded batch, as recorded below. The scoped QUIC/HTTP/3
harness has also passed its reviewed hardware subset after the UDP lifetime fix.
The disposable app-bundle subset and separate delegated Safari observation have
also passed hardware acceptance. Safari used one explicit LaunchServices open
to a stable IPv4-only endpoint, with Private Relay disabled by the user. This
proves fresh kernel delegation and the selected exit, not automatic page-body
verification or Private Relay support. Throughput still needs the bounded
extension described below.

## Recorded Safari hardware acceptance

The coordinator reported a successful scoped Safari/WebKit observation using
`5a60671`: TCP smoke and independent delegated socket/exit evidence passed,
without an authorization dialog; uninstall restored routing and helper state. The actual frozen target
was identified by ordinal. The page body was not automatically verified.

Later review found browser processes left by the former automatic-open harness.
The coordinator removed those test-owned processes. The new lifecycle refuses
an already running Safari, uses bounded identity-checked cleanup on every return,
and independently audits all processes started in the test window after uninstall.
The coordinator subsequently verified `936c6a7` on hardware: managed cancellation
was explicitly skipped; installation, TCP smoke, delegated observation, automatic
browser cleanup and uninstall passed. Independent coalition inspection found no
remaining Safari or WebKit process in any coalition created by that Safari launch.
Two managed authorizations covered installation and uninstall. The earlier routing
pass alone did not establish process cleanup. Synthetic tests cover existing browsers,
other applications' WebKit services, PID reuse, cancellation and stubborn children.

The time-window audit intentionally included unrelated applications and system
services. Launchd's on-demand Safari agents may occupy pre-existing job coalitions
and run or restart with Safari closed; these are expected services, not test-owned
leftovers. Older WebKit processes from other apps were left untouched. Review
identity and coalition ownership, not just the audit's total process count.

Earlier incomplete observations showed why endpoint selection matters: freezing
all addresses in one DNS answer cannot cover a CDN that returns rotating subsets
of a larger address pool. Choose a small stable A-record set and **no AAAA
records**; Safari may select IPv6, which is outside the IPv4 host capture. The
bounded prepare preflight now compares three A answers and queries AAAA once,
retaining warnings without widening capture. Short or cached DNS samples cannot
guarantee later stability; the coordinator still reviews this prerequisite.

The multi-host startup also exposed an `ENOMEM` RIB size/copy race. Padded,
bounded read retries and a separate ownership-observation window fixed it;
the subsequent hardware TCP smoke activated all reviewed hosts successfully.
This result does not establish background URLSession, Private Relay, full-capture
VPN rebuilding, or throughput performance.

## Recorded application bundle hardware acceptance

The coordinator reported successful hardware acceptance of `fb94688`: TCP smoke,
the main executable's `--app-bundle` IP rule through A, and all six
`--app-bundle-batch` cases. The nested helper inherited the parent's IP rule; the
outside control retained system routing. Main and nested executables matched the
outermost Bundle ID with a TLS domain target. The parent's block applied to its
helper while leaving the outside control untouched. Process, tuple, application,
rule, exit and byte evidence passed. Uninstall restored the baseline, all temporary
apps were deleted, and independent system checks were clean. Reports were reviewed
privately and deleted. This establishes bundle containment and identity matching,
not XPC activation or delegated Safari/WebKit attribution.

The first QUIC hardware smoke passed. Its batch stopped after the first verified
HTTP/3 response because the same UDP tuple was split into a second unattributed
system flow. A synthetic IPv4/IPv6 regression reproduces premature teardown on
client ICMP port-unreachable after socket close; the original binding now lasts
until normal idle, preserving queued tail datagrams. The failed run's triggering
error was not retained, so the triggering hardware packet was not directly
observed. Evidence failures now distinguish early closure, duplicate tuples
(including missing owners) and pending byte counts from HTTP/3 request failure.
The coordinator verified clean uninstall. That failed attempt remains distinct
from the successful repeat recorded below.

## Recorded QUIC hardware acceptance

The coordinator reported successful hardware acceptance of `8dd603e`: TCP smoke,
the single `--quic` HTTP/3 IP request through A, and all three `--quic-batch` cases
(observable Initial SNI domain rule through A, IP rule through B, and IP rule
through A with a fragmented Initial ClientHello). Verified server HTTP/3 responses
and exact helper process/tuple/rule/interface/name/counter evidence passed.
Uninstall restored the baseline and independent system-state checks were clean.
Reports were reviewed privately and deleted. This covers the reviewed IPv4 QUIC
v1/HTTP/3 subset, not ECH, migration, 0-RTT, HTTP retry or general multi-packet SNI
reassembly. The repeat also exercises the corrected UDP binding lifetime; the
synthetic regression separately covers IPv4 and IPv6 ICMP errors.

## Recorded full IPv4 hardware acceptance

The coordinator reported a successful hardware run of `9ceff5c`, with Tailscale
disconnected. The following full IPv4 cases passed, each using bounded capture:

* `smoke`: immediate SafeStart, system egress and a rule through adapter A.
* `batch`: all 11 TCP cases, including block priority, interface/system fallback
  and established versus new connections across hot reload.
* `app-bypass`: standalone tool identity and an unmatched different-path child.
* `preserve`: an existing native TCP connection survived capture activation.
* `safe-failure`: a deliberately blocked SafeStart rolled back.
* `crash`: helper death removed capture and the replacement helper started idle.
* Final `uninstall`: baseline restoration passed. Independent inspection found
  none of the eight capture prefixes, retained normal default routing and
  connectivity, and found no remaining test processes or sudo credential cache.

The initial full-capture SafeStart failed because XNU's bound-socket fallback
looked up the zero address when no scoped route matched. The former lower `/1`
captured that lookup, selecting the utun instead of the bound system adapter;
forwarding failed and reset the probe. Eight IPv4 prefixes excluding
`0.0.0.0/8` preserve that lookup. Production IPv6 has the corresponding split
excluding `::/8`, with synthetic regression coverage only so far. See
[the XNU source references and exact prefixes](ARCHITECTURE.md#capturing-and-forwarding).

A gateway also used as DNS had an existing `/32 UCS` host route. Its route was
preserved and its optional DNS capture was skipped, with the exclusion retained
in the private review. Queries on that path are not observed; domain rules need
another observed name, such as SNI or HTTP Host. This is not a DNS-capture pass.

This record does not cover full capture with Tailscale connected (including VPN
restart), full IPv6 capture, UDP/QUIC, app bundles/delegated processes, or throughput.
Tool attribution is not bundle acceptance. Raw reports were reviewed privately
and deleted; no network identifiers or raw observations are reproduced here.

## Recorded UDP/DNS hardware acceptance

The coordinator also reported successful scoped UDP/DNS hardware acceptance of
`c342de7`: TCP smoke, the single A-query `--udp-dns` smoke through adapter A,
and the complete two-resolver `--udp-dns-batch`. The first resolver passed ten
cases, including idle expiry and a new session through B on the same application
source port; the second passed eight cases. The batch checked A/AAAA replies,
nonce/tuple attribution, rule/interface/outcome, payload counters, reuse, hot
reload, block priority and deterministic fallback. Uninstall restored the
baseline; independent inspection confirmed ordinary resolver routing, working
system DNS, no owned residue and no sudo credential cache. Reports were reviewed
privately and deleted. This establishes the reviewed IPv4 UDP/DNS subset, not
general UDP services, pre-existing UDP, cache expiry, QUIC or public NAT ports.

## Review and boundaries

Select two available adapters, controlled test destinations, a permitted UDP/QUIC
server, a bandwidth budget, and an app fixture with the user. Keep all names,
addresses, public exits, adapter identifiers, application paths and raw results
in a new private session below `TMPDIR`, never in this repository. Freeze helper
and fixture checksums, allowed endpoints, routes, installed paths, limits, and
undo commands in the concrete review material before any authorization.

Preserve the initial routes, interface names/indices, DNS configuration, launchd
registration, system-directory permissions and existing long-lived connections.
Each stage gets its own accepted baseline; an approved external VPN change may
be accepted only while Pith is stopped and with the documented ownership proof.
Keep the app and other helper installers closed. The user controls any approved
VPN mode change; neither the harness nor an agent starts or updates a VPN.

Installation/removal use the reviewed prompts in [root tests](ROOT-TEST.md).
Rebaseline itself has no administrator dialog. Changed frozen scopes require a
new installation plan and explicit approval, not hand-edited session JSON. A
full-capture test must be visibly distinct from a host-scoped installation. Test
its cancellation first. Count the exact dialogs in the concrete session plan;
do not reuse a host-scoped installation for full capture.

Every privileged test needs a helper-enforced deadline, independent Stop cleanup,
and a copied Pith-only emergency undo command that works without IPC or network.
The existing five-minute lease applies to legacy `host-ipv4` capture only.
**Do not remove `--capture` to start an unbounded full-capture helper.** Use the distinct
`full-ipv4` profile, whose maximum lease is 120 seconds; `host-ipv6` is limited to
60 seconds. Leave no helper, fixture, server, child process, socket, watcher,
or benchmark running afterward.

## Stages

The full IPv4 harness has passed the TCP subset recorded above. Its observation
window checks uninterrupted capture identity,
but does not implement the manual DNS, VPN-control or mixed-protocol assertions
below. Passing that TCP subset is not a pass for the entire full-capture stage.
The dedicated hotspot renewal flag remains `host-ipv4` only pending review of its
full-capture/device timing. New modes are implemented separately and reviewed
before use; their implementation order does not change the attended test order.

Full IPv4 now freezes eight prefixes: `1.0.0.0/8`, `2.0.0.0/7`, `4.0.0.0/6`,
`8.0.0.0/5`, `16.0.0.0/4`, `32.0.0.0/3`, `64.0.0.0/2`, and `128.0.0.0/1`.
They leave `0.0.0.0/8` to the system. The corresponding production IPv6 split
leaves `::/8` to the system, including translation prefixes inside that range.
This preserves Darwin's bound-socket fallback through its zero-address route
lookup; see [architecture and XNU sources](ARCHITECTURE.md#capturing-and-forwarding).
Rebuild and prepare a new session after this change. Require an immediate
SafeStart through the system adapter when it has an unscoped default but no
scoped default: a successful bound request on a second adapter alone misses this
regression. Repeat that condition in the later full-IPv6 stage. Fake-driver
tests reproduce the old split's failure and the new split's success in both
families; full IPv6 hardware acceptance remains pending.

Run one request before each bounded batch. Stop at the first unexplained result;
do not broaden capture, change DNS/pf, remove a VPN route, or retry indefinitely
to obtain a pass. Preserve failures and independent cleanup evidence separately.

| Stage | Scope and execution budget | Required observations | Pass and cleanup |
| --- | --- | --- | --- |
| IPv6 first | Use `host-ipv6`: one explicitly selected global IPv6 `/128`, at most 60 seconds of capture; one system and one bound TLS request per adapter with an 8-second deadline. Its mandatory SafeStart proves the exact native IPv6 socket crossed capture. | Before capture, verify both the ordinary and selected adapter paths can actually reach the endpoint. During capture, verify process, IPv6 destination, rule/outcome, actual interface and bytes. Test block, a missing-interface fallback, and a new connection after hot reload. | A verified remote response and exact captured tuple are required; a working IPv4 fallback cannot count as IPv6 success. No public IPv6 on an adapter is a prerequisite failure/inconclusive case. Stop removes only the `/128` and owned tunnel; IPv4 routing and existing VPN routes stay unchanged. Any dual-stack connectivity claim also needs a separately reviewed IPv4 control endpoint and observation. |
| UDP and DNS | Passed the reviewed two-resolver IPv4 hardware subset. One-query smoke, then at most 11/20 queries for one/two resolvers; two-second response deadlines, no retries or TCP fallback. | Match DNS IDs/questions and exact application tuple/PID/path against helper records, including rule/interface/outcome and payload counters. Check A/AAAA, stable pre-NAT source tuples, hot reload, block/fallback, reuse and replacement after the ten-second DNS idle timeout. Record TTLs without dialing answer addresses or widening capture. | Cache expiry, ordinary UDP's two-minute idle timeout, pre-existing connected UDP and unconnected listeners remain separate gaps. The latter has a known source-port limitation. Silent-peer timeout does not prove automatic interface failover; DNS replies cannot prove public NAT ports. |
| QUIC | Passed the reviewed IPv4 hardware subset: one frozen `/32`, one HTTP/3 smoke request through A, then three sequential cases; ten seconds per exchange, bounded socket payload and packet counts. | Require verified TLS/ALPN and a real HTTP/3 response, exact UDP process/tuple/rule/interface/name records and counters. Test domain routing with observable SNI, IP routing through B, then IP routing through A with a fragmented ClientHello that lacks recoverable first-datagram SNI. | The fragmented control is not ECH or multi-packet name recovery. No redirects, application retries, TCP fallback, unreviewed destination or plaintext/packet logging. Stop and verify routes/tunnel. See the [QUIC procedure](ROOT-TEST.md#bounded-quichttp3-stage). |
| App bundle and helpers | Passed the reviewed hardware subset: one main-executable smoke request, then six nested/helper/control cases to the existing host, each with an eight-second deadline. | Correlate spawned PID, executable, exact application tuple, outer bundle path/identifier, rule/outcome/interface and bytes. Exercise path and identifier matching independently, then block priority and the outside control. | Children were reaped, private fixtures removed and uninstall restored the baseline. See the [bundle procedure](ROOT-TEST.md#disposable-application-bundle-stage). This does not establish XPC activation or delegated sockets. |
| Safari/WebKit delegation | Scoped hardware subset passed; repeat with a stable IPv4-only endpoint: `prepare --webkit-app` freezes Safari metadata/executable, the existing endpoint URL and all A records in one bounded resolver answer (at most 16 scoped hosts). After TCP smoke, `check --webkit 45s` waits for one manual navigation; explicit `--webkit-open` opens that URL once after READY via LaunchServices. At most 60 seconds. | Independently correlate a fresh exact PCB tuple, owner and delegated PID, effective Safari executable/bundle, helper process/app/rule/interface, outbound tuple and bidirectional bytes. No new capture prefixes or DNS overrides. | Missing/new-address/reused/short-lived socket evidence is incomplete, with no retry or capture widening. Automatic pass proves delegation and exit only, not page content. No AppleScript or window/tab control. Automatic open refuses an existing Safari and owns bounded cleanup of the newly launched browser and independently attributed WebKit services; manual mode leaves cleanup to the coordinator. See the [Safari procedure](ROOT-TEST.md#bounded-safariwebkit-observation); background URLSession is skipped. |
| Throughput | A permitted dedicated endpoint and finite transfer budget; direct bound baseline first, then Pith on the same adapter. Use 5-second warmup and at most three 15-second samples per direction; cap total transferred bytes in the reviewed plan. | Measure server/client payload bytes and elapsed time independently of Pith counters; record CPU, memory, log volume, connection count and idle CPU after completion. Compare a rule-heavy scene separately from the minimal scene. | Claim the 500 Mbps target only if the direct link/server baseline can support it and sustained observed payload throughput meets it. Otherwise record a bottleneck or inconclusive target. Do not use an unrelated public speed service without permission or create an unlimited generator. |
| Full IPv4 extensions after scoped stages | The core TCP and cleanup cases above have passed. DNS, mixed-protocol and VPN extensions still need separate approval and a helper-enforced lease of at most 120 seconds; one ordinary TCP request first, then a small bounded batch. Use the eight IPv4 split prefixes above; do not enable IPv6 capture in this stage. | Review all intended LAN-DNS host routes as well as every split prefix. Retain the TCP regression checks, and separately verify real observed DNS answers, mixed protocols and the selected VPN/tailnet/MagicDNS controls without scanning peers. | Prove each asserted flow crossed Pith and reached the expected external path. Stop on lost ordinary connectivity, routing loops, unexpected destination scope, conflicting ownership, or changed VPN state. After Stop and optional separately approved helper crash, verify no Pith split prefix, DNS host route or utun remains, and unrelated paths still work. |

A proxy helper with a direct delegated kernel socket and an arbitrary loopback
HTTP/SOCKS proxy are different cases. Add a controlled loopback-proxy negative
control: Pith should attribute the proxy's outbound socket to the proxy when no
delegation exists. Do not claim it recovered the original app or bypassed system
proxy settings. Record unresolved delegation as an acceptance gap, not by adding
broad app-name heuristics.

Do not substitute ordinary HTTPS success for path evidence. Keep server-side
nonces/addresses, caller identity, and matching Pith connection records together.
The root helper's identity receipt establishes which tunnel Pith created; it
does not itself prove that a test request traversed it.

## Recorded IPv6 host hardware acceptance

The coordinator reported successful hardware acceptance of `d359287`: `host-ipv6`
passed smoke, all 11 batch cases, app-bypass, preservation, forced SafeStart
failure and crash recovery. A scoped IPv4 regression passed the same TCP cases
without a crash test, and full IPv4 passed the same cases including crash
recovery. Every uninstall restored its accepted baseline. Independent inspection
confirmed ordinary IPv4/IPv6 routing and connectivity, no remaining owned routes,
tunnel or test processes, and no sudo credential cache. Raw session reports were
reviewed privately and deleted. These results cover host IPv6 and the full IPv4
TCP subset, not full IPv6, UDP/QUIC, bundles or throughput.

The `host-ipv6` harness freezes one `/128`,
requires global IPv6 on both adapters, forces `tcp6` for all three preflights and
test requests, and requires the native SafeStart socket to cross capture. IPv4
fallback and an IPv4 echo cannot satisfy the IPv6 checks. The helper's lease is
at most 60 seconds and the controller stops its work ten seconds before that
deadline. Preparation, installation identity, family/scope rejection and cleanup
also have synthetic coverage.

The first hardware smoke attempt stopped during SafeStart with
`unreadable configured route event`; the coordinator verified clean rollback and
uninstall. Source analysis and a synthetic wire-format reproduction found that
`-net` with IPv6 `/128` removes HOST while omitting the mask. Host additions and
deletions now explicitly use `-host` in both families. The former report had no
event metadata, so the exact offending hardware message is unconfirmed. Inspect
the `routeDiagnostics` field even on failed startup: it
records address formats and the ownership decision without address payloads.

A subsequent retry and scoped IPv4 control both exposed a notification regression:
normal IFINFO with `ifm_addrs=0` was skipped by `x/net/route` and misclassified as
unreadable. Header decoding and snapshot-based notification handling are now
corrected. Synthetic IPv4/IPv6 startup sequences cover immediate and queued
IFINFO/NEWADDR/route-ADD delivery without stopping capture, plus actual link-down
and failed ownership checks. The corrected scoped IPv4 hardware regression passed
smoke, the 11-case batch, app-bypass, preservation and forced SafeStart failure;
explicit IPv4 host add/delete is therefore verified again.

The next IPv6 retry reached ownership validation but reported the `/128` missing.
Source analysis and a raw-RIB fixture reproduce the cause: XNU adds LLINFO to
explicit static IPv6 interface hosts, and the previous cache filter discarded
them. Static non-cloned LLINFO entries now remain visible to validation and
cleanup; unscoped, link-gateway and exact interface checks remain strict. The same
retry's non-fatal NEWADDR diagnostic was reproduced as an empty final RTA_BRD
for a point-to-point address without a peer, which is now accepted. Both fixes
have synthetic coverage, including teardown and retained pre-cleanup diagnostics.
The subsequent hardware passes above verify the corrected startup, host-route
ownership and cleanup paths. A read-only comparison around ordinary IPv4/IPv6
HTTPS traffic also retained identical configured-route snapshots.

### Repeating the IPv6 host stage

1. Confirm the two adapters and one permitted IPv6 TLS endpoint. It must support
   certificate-verified HTTP/1.1 `GET /`; a plain-text IPv6 exit echo also enables
   the optional tool-bypass check. Freeze a literal `[IPv6]:port` and certificate
   name, without choosing a default server or adding another target.
2. Follow [the separate IPv6 installation](ROOT-TEST.md#separate-ipv6-host-installation)
   with `--capture-mode host-ipv6 --test-lease 60s`. Review the new private scope,
   preflights, installation and emergency undo before authorization. A missing
   system/A/B IPv6 path ends preparation; it must not fall back to IPv4.
3. Run `check` first. Only after smoke passes, run `check --batch`, then
   `check --safe-failure`, each with the same `--session` and a fresh bounded
   capture. Verify reports and cleanup after each command. A slow batch that
   exceeds its deadline remains failed; do not extend the lease to finish it.
4. Uninstall against the current baseline and independently check owned routes,
   tunnel, service and files. An IPv4 route comparison is not an independent
   IPv4 connectivity control; that would require another reviewed endpoint.

Additional preservation, tool-bypass or crash cases need to be included in this
session's explicit scope. No UDP/QUIC, bundle, throughput or full IPv6 test is
implicitly enabled by the IPv6 host profile.

## Confirmed scope for later harness extensions

The coordinator has confirmed the choices below. Keep actual endpoint names,
adapter identities and byte budgets in private session notes, not in this table.
They define the fixtures and bounded requests; no harness is run by the engine
developer. The coordinator reviews each completed implementation before use.

| Stage | Coordinator/user choices | Harness preparation after confirmation |
| --- | --- | --- |
| UDP and DNS | Two permitted public IPv4 DNS resolvers on port 53; small A/AAAA queries for one reviewed name, using DNS IDs as nonces. No echo server. | Implemented: freeze up to two `/32` DNS hosts and the question, run `--udp-dns` before `--udp-dns-batch`, validate replies/tuples/policy/counters and session reuse/expiry. At most 11/20 batch queries for one/two resolvers. TTLs are evidence only; do not dial answer addresses or infer public NAT ports. See the [bounded DNS procedure](ROOT-TEST.md#bounded-udpdns-stage). |
| QUIC | One permitted HTTP/3 endpoint; one request per case, with a real HTTP/3 response as application evidence. | Implemented: freeze endpoint/certificate and request/packet budgets in `quic-plan.json`, run `--quic` before `--quic-batch`. The third batch request is an explicit fragmented-Initial negative SNI control with a positive IP-route response. |
| App bundles and delegation | A temporary pure-Go `.app` with main executable, nested helper, and an outside-bundle control; one Safari request to the frozen URL. Background URLSession is explicitly skipped this round. | Bundle portion implemented: `prepare --app-bundle` freezes checksums/metadata and request limits; `--app-bundle` gates `--app-bundle-batch`. Children exit and fixtures are removed. WebKit requires a separate observation; bundle containment does not establish delegation. |
| Throughput | A permitted upload/download service, direct baseline before Pith, separate wired and cellular total byte budgets. | Freeze endpoint/protocol and private per-adapter caps. Count baseline, warmup, retries and captured upload/download against the same cap; enforce time and byte limits independently. No automatic replacement endpoint or unlimited generator. |
| Full capture with Tailscale | The user enables the VPN and manually disconnects/reconnects at the agreed checkpoint. MagicDNS checks use only this machine's name; no peer scan. | Add explicit transition evidence and private own-name DNS controls to the reviewed full-mode stage; do not automatically toggle/restart the VPN or claim remote tailnet reachability from self-name resolution. |

## Required full-capture VPN restart acceptance

The live-egress implementation in [architecture](ARCHITECTURE.md) has
synthetic coverage for withdrawal, name/index reuse, replacement/shadowing of
capture routes, observer failure, pending and established sockets, and VPN
packets arriving before notifications. This is not hardware acceptance.

**The next round must include a Tailscale self-restart during full IPv4
capture, without an exit node.** Review and authorize this as a separate fault
stage with a helper lease of at most 120 seconds. Freeze one permitted tailnet
endpoint, its MagicDNS name, the public control endpoint, the restart action and
undo. The user performs the approved VPN restart; no agent silently updates it.

1. Record the external tunnel name/index, scoped defaults, more-specific routes,
   addresses and DNS. Verify one public, tailnet and MagicDNS request first.
2. Start full capture, repeat the controls, then perform exactly one approved
   restart. Issue only a bounded set of requests during withdrawal and rebuilding;
   retain independent path evidence and the helper's state/generation timestamps.
3. Require Pith to remain Running with the same owned utun and capture routes.
   Tailnet/MagicDNS packets caught during withdrawal must never reach a physical
   rule or fallback. Unavailable or ambiguous system requests fail locally; a VPN
   default withdrawal permits at most a three-second settling interval before a
   stable physical default becomes usable. Existing physical connections survive.
   A rebuilt VPN is selected by its current name/index lifetime for new requests;
   old sockets never migrate onto a reused index. Verify public and private
   controls again after reconstruction, without Stop/Start.
4. Stop explicitly, confirm owned-route cleanup and restored tailnet/MagicDNS
   reachability. Inspect external changes before rebaseline. A damaged Pith route
   or utun must still trigger automatic `ErrNetworkLost` and safe cleanup; this
   distinct fault test must not be confused with ordinary VPN reconstruction.

If self-restart cannot be reproduced safely, record this required item as pending,
not passed. An unplanned VPN change in a throughput stage still aborts that stage.

## Required adapter renewal and fallback acceptance

During approved capture, renew the selected cellular hotspot's public connection
once through the app. Record an IPv6 prefix/privacy-address and DNS change when
the device supplies them. Require Pith to remain Running, keep its owned routes,
and send subsequent rule-matched connections through that adapter using current
addresses and DNS. Existing sockets keep their bindings and may naturally fail
when the provider invalidates their addresses; they must not migrate to another
interface. Include DHCP/address renewal on the system-default adapter as well.

Then, in a separately approved bounded case, unplug the preferred hotspot while
its rule lists a second available adapter. A new connection must use that next
adapter with outcome Fallback. Reconnect the first adapter (including a changed
index), verify new connections use it again without Start/Stop, and keep an
unrelated control connection alive throughout. Restore the approved physical
setup and verify cleanup against the accepted baseline. Record actual adapter
names, public addresses and device actions only in the private session.

Full IPv6 capture uses `100::/8`, `200::/7`, `400::/6`, `800::/5`, `1000::/4`,
`2000::/3`, `4000::/2`, and `8000::/1`. It is a later, separately approved extension
after `/128`, UDP/QUIC and full IPv4 stages. Verify both native IPv6 and dual-stack
fallback explicitly; a functioning IPv4 path cannot conceal IPv6 loss.

## Completion evidence and undo

After each stage: Stop, verify helper idle/version/installation identity, compare
the accepted route and tunnel baseline, and save the separate test and cleanup
results. External-only differences require manual review and explicit acceptance;
failed-start reports retain a bounded connection log with outbound errors even
when no Running event occurred. Inspect that evidence before another attempt;
ambiguous Pith ownership or any capture residue forbids rebaseline. Use only the
reviewed bootout plus ownership-checked journal cleanup when IPC cleanup fails.

After the last stage: uninstall once against the latest accepted baseline,
independently verify all five managed paths and the launchd service are absent,
verify shared-directory permissions unchanged, and confirm every process started
for testing has exited. The user then restores any explicitly changed VPN mode.
Review private artifacts before deleting them; publish only a deliberately
sanitized acceptance summary with limitations, never raw captures or snapshots.
