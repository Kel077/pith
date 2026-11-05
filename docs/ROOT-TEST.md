# Bounded root acceptance sessions

This is a plan and an opt-in test harness, not a record of completed root tests.
Do not run a command that opens an administrator dialog or starts capture until
the coordinator has obtained the user's approval for this session. Run the
harness as the installing user, never with `sudo` or as root.

The first session captures only one explicitly selected public IPv4 `/32`
HTTPS destination, plus an optional public IPv4 `/32` DNS destination. It does
not install `/1` routes, alter DNS settings or pf, or replace the default route.
The helper starts idle. It creates one utun only when `check` calls `Start`.
Every check has a three-minute deadline and an independent fifteen-second
cleanup deadline. SIGINT and SIGTERM cancel the check and invoke cleanup.
The helper also limits each scoped routing run to five minutes, so a killed
test client cannot leave routing active indefinitely. The helper remains
installed and idle until the explicit uninstall step.

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
reviewed full IPv4 installation is described below; IPv6 remains a subsequent
extension.

Build both binaries into a private temporary directory, without changing the
application's configuration or packaged resources:

```sh
PITH_TEST_BIN=$(/usr/bin/mktemp -d "${TMPDIR%/}/pith-rootcheck-bin.XXXXXXXX")
export GOCACHE="${TMPDIR%/}/pith-go-build"
export GOMODCACHE="${TMPDIR%/}/pith-go-mod"
CGO_ENABLED=0 go build -trimpath -ldflags '-X main.version=acceptance' -o "$PITH_TEST_BIN/pithd" ./cmd/pithd
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

For manual UDP/DNS checks, add `--dns-target '<reviewed public IPv4:53>'` to
that one preparation command. Do not add extra hosts later: prepare and review
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
binary checksum after the password dialog.

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

The coordinator should review these concrete artifacts and obtain the user's
go-ahead now. Keep a second Terminal window open with the emergency command
already copied. Do not start the application or another helper installer
during the session.

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
planning. Review both IPv4 `/1` routes and every DNS `/32` before authorization.
The helper receives explicit `--test-profile full-ipv4`, `--test-lease`, and
repeated `--test-dns-host` arguments; no `--capture` argument is used. Tailscale
DNS and DNS routed through another tunnel are excluded by the production plan.
`--dns-target` cannot be combined with this mode. IPv6 capture is forbidden and
SafeStart cannot be disabled, including by hot Reload or repeated Start.

The exact install/cancellation prompt is:
`Pith needs your password to install its full IPv4 test helper (at most 120 seconds per capture).`
The uninstall and optional crash prompts, managed paths, ownership checks and
emergency-command construction are unchanged. Dialog mode uses one canceled
installation dialog and two password authorizations; optional crash adds one.
Managed authorization, only when separately permitted, marks cancellation as
skipped as described below. These are separate session counts, not permission
to reuse a previous installation or credential authorization.

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
a fresh reviewed installation. Stop removes only the two owned `/1`s, reviewed
DNS host routes and owned utun. Defaults and external routes are never replayed
or removed. The standalone emergency command uses the same journal ownership
checks and needs no network or IPC.

The first approved full-mode check must still be smoke. A later TCP batch,
SafeStart failure, preservation, app/tool bypass, observation, or separately
authorized crash uses a fresh bounded capture. Ordinary full capture includes
other apps' new IPv4 traffic, which keeps system routing unless a test rule
matches; reviewing only the test endpoint is therefore insufficient approval.
DNS responses, tailnet controls, VPN self-restart, UDP/QUIC, app bundles and
throughput require the additional evidence in [next acceptance](NEXT-ACCEPTANCE.md).
The automated hotspot-renewal flag currently remains restricted to host-scoped
sessions. No full-mode hardware acceptance is claimed by these unit tests.

## Ordered execution

Stop after the first failed case. Once a check enters its active test phase,
every outcome invokes bounded cleanup; inspect the private report and current
baseline before deciding on another test. Cleanup rechecks installation identity
before sending `Stop`, and refuses to stop a replacement helper. If Pith cleanup
fails or its ownership cannot be established, use the reviewed removal procedure
and do not retry capture. A separately proven external-only change can instead
use the explicit acceptance procedure below; it is not a successful check.
Concurrent commands on the same session are refused by an advisory lock.

### Optional delegated authorization

Only the coordinator may opt into a password that the user has explicitly
delegated for this session. Use `--auth keychain:REVIEWED_SERVICE` on **each**
privileged invocation. The default remains the ordinary administrator dialog;
the session never remembers permission to read a keychain item on a later
invocation. Other agents must not read or use the delegated item, the hotspot's
administrator item, or the app's stored hotspot credentials.

In this mode, `cancel --auth keychain:REVIEWED_SERVICE` performs the original-state
checks and records the manual cancel case as **skipped**, without opening a
dialog or reading a password. `install` with the same explicit option may record
that skip itself if the cancel step was omitted. `cancelPassed` stays false;
session/check reports retain `cancelSkipped` and an explanatory note. Managed
installation and uninstall each use the delegated credential once. An optional
`check --crash --auth keychain:REVIEWED_SERVICE` uses it once more. No manual
cancel click or administrator-password dialog is expected; Keychain access may
still be denied or require user authorization under the user's macOS settings.

The harness reads `/usr/bin/security find-generic-password -s SERVICE -w` into a
bounded memory buffer. Hex-looking output requires a matching private `-g` read:
only an explicit `password: 0x...` representation permits decoding non-ASCII
bytes; a literal hexadecimal password stays literal. Failed or inconsistent
reads abort authorization. The password goes only on stdin to `/usr/bin/sudo -S -k -p ''`.
Only the frozen reviewed script goes in the command arguments. Passwords never
enter arguments, reports, files or forwarded child output, and temporary buffers
are cleared afterward. Each use records its operation, mode, script checksum,
attempt and result in private authorization reports; subprocess diagnostic text
is discarded to prevent it echoing credentials. A command success is not a test
pass: the existing independent identity/state/cleanup checks still decide that.

Fresh prepare output includes `install.sh`, `uninstall.sh`, and optional
`crash.sh` with frozen checksums. A missing or changed script refuses managed
execution. Announce the root session beforehand and report every credential use
afterward. A skipped cancellation case does not establish cancellation behavior.
The emergency command remains an independently reviewed, manual recovery path;
this option does not broaden it or automatically authorize retries.

### Dialog mode

The complete session requests **one cancellation dialog and two password
authorizations**, for installation and final uninstall. There is no additional
authorization for any `check`, `stop`, or `rebaseline`, regardless of the number
of approved Tailscale modes. One optional deliberate crash test adds exactly
one separately approved administrator dialog. Emergency recovery is an
alternative removal authorization, not an automatic extra step.

| Step | Command after the root-session go-ahead | Dialog and changes | Pass condition and undo |
| --- | --- | --- | --- |
| 1. Cancellation | `"$PITH_TEST_BIN/rootcheck" cancel --session "$PITH_SESSION"` | Administrator dialog requests `Pith needs your password to install its routing helper.` Click **Cancel**; do not enter a password. | Returns `ErrCanceled`; no managed path, service, route, or utun changes. If a password was entered or cached authorization installs the helper, this case fails; uninstall it before preparing a new session. |
| 2. Installation | `"$PITH_TEST_BIN/rootcheck" install --session "$PITH_SESSION"` | Same install prompt; enter the administrator password. Creates the root-owned helper and launchd plist/data directory, plus a listed shared parent only if absent; the daemon creates its authenticated socket. It starts idle. | Version and executable checksum match; plist arguments contain only the reviewed host captures; plist inode/mtime identify this installation; engine is stopped; routes and utun inventory match the initial snapshot. Undo is step 9 or emergency cleanup. |
| 3. One smoke check | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION"` | No dialog. Saves only a temporary config, starts the scoped utun with SafeStart, requests HTTPS through ordinary system routing, then hot-applies an IP rule for interface A. | SafeStart succeeds; both logged flows belong to this test process; outcome, rule, interface, bidirectional byte counts, and live traffic are correct. `Stop` restores routes and utun inventory before the command returns. Only a successful smoke check unlocks later cases. |
| 4. Rule batch | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --batch` | No dialog. Starts a fresh bounded capture and changes only the test scene. | Verifies interface B, TLS SNI domain matching, the second target of one rule, block priority over an earlier route, missing-interface fallback, explicit system fallback, and strict unreachable. Opens TLS on A, changes the rule to B, completes the same connection on A, then verifies a new connection on B. The capture start timestamp stays unchanged during hot reload. Ends stopped. |
| 4a. Optional app-specific routing | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bypass` | No dialog. Requires a passed smoke check and public-IP preflight evidence. Uses the current standalone `rootcheck` executable path as the app/tool identity, and creates a checksum-verified copy at a different path inside the private session directory. Each controlled child makes exactly one ordinary TLS request to the same frozen target. No helper installation, capture expansion, or VPN setting change occurs. | The parent matches app+IP and then app+domain rules through physical interface A. Under each rule, the different executable remains unmatched and uses the saved system route. A final app block after the route rule wins. Every case checks PID, executable/app path, destination, rule, outcome, interface, and byte counts; successful requests use the public-IP drift policy below. Children are acknowledged and waited for on every outcome, their copy is removed, and routing ends stopped. Identical public exits do not prove a distinct exit-node bypass. |
| 5. Native TCP preservation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --preserve` | No dialog. Establishes an ordinary TLS connection **before** capture, starts capture, then sends HTTP over that same native connection. | The pre-existing kernel connection survives route activation and receives a verified HTTPS response. This flow is not claimed as a new proxied/logged connection. Ends stopped. A server that closes idle TLS before the request makes this case inconclusive; do not count it as a pass. |
| 6. SafeStart rollback | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --safe-failure` | No dialog. Starts with a block rule for the exact SafeStart destination and SafeStart enabled. | `Start` returns `ErrNetworkLost`; status reports the same failure; routes/utun are restored. Cleanup calls `Stop` so the next command sees an idle helper. |
| 7. Optional observation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --observe 90s` | No dialog. Runs the smoke checks, then holds the reviewed host capture for at most 90 seconds. See the manual cases below. | The harness records an observation window, not a pass for an unimplemented manual assertion. It polls helper status and stops on interruption, failure, or expiry. |
| 8. Explicit stop | `"$PITH_TEST_BIN/rootcheck" stop --session "$PITH_SESSION"` | No dialog; authenticated IPC only after verifying the installation belongs to this session. Safe to repeat while this helper is installed. | Status is stopped at the reviewed version; route snapshot and utun inventory match the latest active baseline. A successful comparison is persisted as cleanup proof. If IPC is broken, use emergency cleanup instead. |
| 8a. Optional next mode | While stopped, the user selects the approved Tailscale mode; then `"$PITH_TEST_BIN/rootcheck" rebaseline --session "$PITH_SESSION" --label tailnet` (or `vpn-off` / `exit-node`) | No dialog. Verifies the same idle installation and the preceding cleanup proof; takes new snapshots and makes three bounded HTTPS requests to the frozen target through system/A/B. Writes private review files only; it does not change VPN settings, capture scope, or routing. | Both reviewed adapters resolve and are up; helper identity/version remain unchanged; snapshots are stable across preflight. Only then does the new baseline become active, with smoke/pass state cleared. Repeat step 3 before any further case. A failed rebaseline leaves the previous active baseline and all history intact. |
| 8b. Reviewed external change | `"$PITH_TEST_BIN/rootcheck" rebaseline --session "$PITH_SESSION" --label vpn-rebuilt --accept-external-change` | No dialog or routing change. Use only after reviewing an `external-change` cleanup report; this is not a general mismatch override. | Pith's exact capture prefixes are absent; every reused name/index has independent whole-VPN migration evidence; the same idle installation and stable preflight checks pass. Archive both the prior proof and current differences, then repeat smoke. The original failed check remains failed. |
| 9. Final uninstall, once | `"$PITH_TEST_BIN/rootcheck" uninstall --session "$PITH_SESSION"` | Administrator dialog requests `Pith needs your password to remove its routing helper.` Enter the password. Attempts `Stop` on the verified installation, then independently boots out launchd, checks stale capture ownership, and removes managed files. It does not require working IPC or a matching IPC version. | Helper, service, socket, journal, and data directory are absent; the latest baseline's routes and utun inventory are restored. Shared parent directories and private session artifacts remain. A clearly different installation is refused before authorization; review ownership and the standalone emergency command instead. |

macOS supplies the surrounding dialog title, buttons, and any cached
authorization behavior. The quoted prompt strings are the harness's exact
English text. A missing cancellation dialog is not a cancellation pass. No
update dialog is expected because the harness refuses an existing installation;
testing helper updates needs its own reviewed case.

All snapshots and detailed reports, including real addresses and MACs, stay
under `PITH_SESSION`. Do not attach or commit them without redaction. The
harness compares route and utun snapshots strictly against the active baseline.
Snapshots exclude `RTF_WASCLONED` host routes and `RTF_LLINFO` ARP/NDP entries;
ordinary traffic refreshing those caches does not change the baseline. Static
host routes, cloning parents and VPN routes remain in the comparison. Another
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
running capture, delete a route, restart a VPN, widen scope, or add a password
dialog. If external state has already returned exactly to the old baseline,
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

* **UDP and DNS:** if a DNS target was included at preparation, perform one
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
  the frozen `crash.sh` with the separate crash prompt. Add the explicit managed
  auth option above to use the stdin mechanism instead. It allows ten seconds
  for capture disappearance and an authenticated idle restart, and always runs
  independent Stop/cleanup afterward. Its report is a distinct crash check, not
  a smoke pass. For a manually observed crash, only after the coordinator approves
  this deliberate fault,
  interrupt the *managed service only* during observation with an administrator
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
* **IPv6, QUIC, existing UDP, loss-driven failover, throughput, and full capture:**
  these remain separate acceptance items. The automatic fallback uses a missing
  synthetic interface; it does not prove timeout behavior on a live interface
  that drops packets. Do not infer IPv6, UDP/QUIC correctness or a throughput
  target from the TCP smoke result. A later IPv6 test must use only a reviewed
  `/128`. Testing `/1` capture requires another explicit go-ahead after scoped
  capture and cleanup have passed.

The [next acceptance plan](NEXT-ACCEPTANCE.md) separates the broader capture,
IPv6, UDP/QUIC, throughput, and app-bundle cases. It authorizes no execution and
does not add those capabilities to this host-only harness.

## Optional Tailscale compatibility phases

These phases belong to the same coordinated acceptance window, but they are
opt-in. They do not authorize installing, updating, reconnecting, or changing
Tailscale automatically. Test only an already available Tailscale configuration
and an exit node the user is permitted to use. The user selects **no exit node**
or the approved **exit node** in Tailscale after the coordinator has explicitly
included that change in the session approval. Keep Pith stopped while changing
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
installed, idle scoped helper throughout this coordinated session. After the
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

For this IPv4-only harness, interface comparison normalizes interface and IPv4
address ordering. It preserves interface identity, kind, up/down state, IPv4
address values, both routers, DNS values and priority order, and primary status.
`api.Interface` exposes neither IPv6 prefix lengths nor address lifetimes or
temporary flags, so the harness does not guess which address is temporary.
It compares whether global IPv6 addresses are available, while tolerating changes
to their individual values or count; full addresses remain in the private
snapshots. This does not establish IPv6 address stability or replace a future
reviewed `/128` acceptance plan. Route comparison stays strict: if IPv6 address
rotation also changes a non-cache local `/128` or another semantic route, the
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

The whole check retains the three-minute limit and unconditional Stop cleanup.
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
captured IPv4 host**, while preserving the TLS server name. It does not edit
`/etc/hosts`, change DNS, follow redirects, or add another destination. The
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
