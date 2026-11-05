# Next acceptance plan

This document plans future attended tests. It authorizes no administrator
operation, network change, benchmark, VPN restart, or application launch. The
current `rootcheck` accepts only frozen public IPv4 host captures. Its external-
change acceptance flag does not enable IPv6, subnet capture, or a wider scope.
Implement and review the missing harness support before approving these stages.

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

Installation/removal use the standard reviewed prompts from the scoped plan.
Rebaseline itself has no administrator dialog. Changed frozen scopes require a
new installation plan and explicit approval, not hand-edited session JSON. A
full-capture test must be visibly distinct from a host-scoped installation. Test
its cancellation first. Count the exact dialogs in the concrete session plan;
do not promise that the present host-only installer can span all stages.

Every privileged test needs a helper-enforced deadline, independent Stop cleanup,
and a copied Pith-only emergency undo command that works without IPC or network.
The existing five-minute lease applies to scoped capture only. **Do not remove
`--capture` to start an unbounded full-capture helper.** Add and test a short
explicit full-capture test lease before the `/1` stage. Leave no helper, fixture,
server, child process, socket, watcher, or benchmark running afterward.

## Stages

Run one request before each bounded batch. Stop at the first unexplained result;
do not broaden capture, change DNS/pf, remove a VPN route, or retry indefinitely
to obtain a pass. Preserve failures and independent cleanup evidence separately.

| Stage | Scope and execution budget | Required observations | Pass and cleanup |
| --- | --- | --- | --- |
| IPv6 first | One explicitly selected global IPv6 `/128`, at most 60 seconds of capture; one system and one bound TLS request per adapter with an 8-second deadline. Use a reviewed dual-stack SafeStart design that proves the intended IPv6 flow crossed capture. | Before capture, verify both the ordinary and selected adapter paths can actually reach the endpoint. During capture, verify process, IPv6 destination, rule/outcome, actual interface and bytes. Test block, a missing-interface fallback, and a new connection after hot reload. | A verified remote response and exact captured tuple are required; a working IPv4 fallback cannot count as IPv6 success. No public IPv6 on an adapter is a prerequisite failure/inconclusive case. Stop removes only the `/128` and owned tunnel; IPv4 routing and existing VPN routes stay unchanged. |
| UDP and DNS | Reviewed public host routes only, one controlled UDP exchange first, then at most 10 datagrams per case; 2-second response deadlines. Include a separately approved DNS host only if required. | Match replies to nonces, check source/destination tuples, route selection, logs and counters. Exercise DNS query/answer matching and TTL/domain-cache behavior with controlled names. Observe reuse before and replacement after the 10-second DNS idle timeout; ordinary UDP idle expiry may use one bounded 130-second stage. | Verify real replies through A/B, block/drop without fabricated success, and source-port stability. Test connected pre-existing UDP and an unconnected UDP listener separately; the latter is a known limitation, not covered by a TCP pass. Silent-peer timeout does not prove automatic interface failover. |
| QUIC | One reviewed QUIC endpoint on frozen host routes; one handshake/request before at most 3 sequential cases, each with a 10-second deadline. | Require a server-confirmed QUIC exchange, UDP flow attribution, expected interface, rule and bytes. Compare an ordinary Initial carrying recoverable SNI with a fixture whose name cannot be sniffed. Verify IP rules in both cases. | Domain routing requires actual observed SNI or a valid DNS mapping; encrypted/ECH or unsupported fragmented Initials must not be described as successful sniffing. No TLS interception or logging application plaintext. Stop and verify routes/tunnel. |
| App bundle and helpers | One test bundle with a main executable, a nested helper/XPC service, and a separate control executable; each makes one bounded request to the frozen host. Then a separately reviewed WebKit and background URL-session fixture. | Correlate spawned PID, executable, outer bundle path/identifier, socket-owner PID and effective/delegated app with connection records. Apply one parent-app rule through A, then a block; repeat a control request outside the bundle. Ensure no preexisting pooled socket is mistaken for a new flow. | Main and nested helpers match the parent automatically. WebKit/URL-session traffic is attributed to the requesting app only when the kernel delegation evidence supports it. The control remains unmatched. End/wait for every fixture process and remove only the temporary fixtures. |
| Throughput | A permitted dedicated endpoint and finite transfer budget; direct bound baseline first, then Pith on the same adapter. Use 5-second warmup and at most three 15-second samples per direction; cap total transferred bytes in the reviewed plan. | Measure server/client payload bytes and elapsed time independently of Pith counters; record CPU, memory, log volume, connection count and idle CPU after completion. Compare a rule-heavy scene separately from the minimal scene. | Claim the 500 Mbps target only if the direct link/server baseline can support it and sustained observed payload throughput meets it. Otherwise record a bottleneck or inconclusive target. Do not use an unrelated public speed service without permission or create an unlimited generator. |
| Full IPv4 `/1` capture last | Separate approval and helper-enforced lease of at most 120 seconds; one ordinary TCP request first, then a small bounded mixed-protocol batch. Capture `0.0.0.0/1` and `128.0.0.0/1`; do not enable IPv6 `/1` in this stage. | Review all intended LAN-DNS host routes as well as both `/1`s. Verify original default routes remain installed, real DNS answers, ordinary system egress, explicit A/B rules, block, SafeStart rollback, and native TCP continuity. Check selected existing VPN/tailnet/MagicDNS controls without scanning peers. | Prove each asserted flow crossed Pith and reached the expected external path. Stop on lost ordinary connectivity, routing loops, unexpected destination scope, conflicting ownership, or changed VPN state. After Stop and optional separately approved helper crash, verify no Pith `/1`, DNS host route or utun remains, and unrelated paths still work. |

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

## Required full-capture VPN restart acceptance

The live-egress implementation in [architecture](ARCHITECTURE.md) has
synthetic coverage for withdrawal, name/index reuse, replacement/shadowing of
capture routes, observer failure, pending and established sockets, and VPN
packets arriving before notifications. This is not hardware acceptance.

**The next round must include a Tailscale self-restart during full IPv4 `/1`
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

Full IPv6 capture (`::/1` and `8000::/1`) is a later, separately approved extension
after `/128`, UDP/QUIC and full IPv4 stages. Verify both native IPv6 and dual-stack
fallback explicitly; a functioning IPv4 path cannot conceal IPv6 loss.

## Completion evidence and undo

After each stage: Stop, verify helper idle/version/installation identity, compare
the accepted route and tunnel baseline, and save the separate test and cleanup
results. External-only differences require manual review and explicit acceptance;
ambiguous Pith ownership or any capture residue forbids rebaseline. Use only the
reviewed bootout plus ownership-checked journal cleanup when IPC cleanup fails.

After the last stage: uninstall once against the latest accepted baseline,
independently verify all five managed paths and the launchd service are absent,
verify shared-directory permissions unchanged, and confirm every process started
for testing has exited. The user then restores any explicitly changed VPN mode.
Review private artifacts before deleting them; publish only a deliberately
sanitized acceptance summary with limitations, never raw captures or snapshots.
