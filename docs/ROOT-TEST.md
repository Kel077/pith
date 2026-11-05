# Scoped root acceptance session

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
link-local, private, multicast, or shared-address-space address. This initial
harness deliberately refuses IPv6 and subnet capture; those need a subsequent
reviewed plan.

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
| `session.json` | User ID, immutable target selection, interface references including MAC, helper checksum, and completed phases |
| `original.json` | Pre-privilege interface inventory, route snapshot, launchd result, and the five managed paths |
| `preflight.json` | Bound/unbound HTTPS status and public-IP echo, when the endpoint provides one |
| `install-plan.json`, `install.sh` | Exact scoped launchd arguments, fixed destinations, helper SHA-256 check, rollback, and authorization prompt |
| `uninstall.sh` | Exact bootout, ownership-checked route cleanup, and managed-path removal |
| `emergency-command.txt` | A standalone, single command that can be pasted into Terminal without IPC or the test executable |

Set `PITH_SESSION` to the **exact canonical directory printed by prepare**.
Do not use a path inside the repository or the normal application config
directory. A changed helper or regenerated installation plan invalidates the
review; `cancel` and `install` recheck the SHA-256 and reviewed script before
authorization. The root installation stage independently verifies the copied
binary checksum after the password dialog.

The coordinator should review these concrete artifacts and obtain the user's
go-ahead now. Keep a second Terminal window open with the emergency command
already copied. Do not start the application or another helper installer
during the session.

## Ordered execution

Stop after the first failed case. Every test failure runs `Stop`; inspect the
private report and original snapshots before deciding on another test. If
cleanup fails, use the emergency command and do not retry capture. Concurrent
commands on the same session are refused by an advisory lock.

| Step | Command after the root-session go-ahead | Dialog and changes | Pass condition and undo |
| --- | --- | --- | --- |
| 1. Cancellation | `"$PITH_TEST_BIN/rootcheck" cancel --session "$PITH_SESSION"` | Administrator dialog requests `Pith needs your password to install its routing helper.` Click **Cancel**; do not enter a password. | Returns `ErrCanceled`; no managed path, service, route, or utun changes. If a password was entered or cached authorization installs the helper, this case fails; uninstall it before preparing a new session. |
| 2. Installation | `"$PITH_TEST_BIN/rootcheck" install --session "$PITH_SESSION"` | Same install prompt; enter the administrator password. Creates the root-owned helper and launchd plist/data directory; the daemon creates its authenticated socket. It starts idle. | Version and executable checksum match; plist arguments contain only the reviewed host captures; engine is stopped; routes and utun inventory match the original snapshot. Undo is step 9 or emergency cleanup. |
| 3. One smoke check | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION"` | No dialog. Saves only a temporary config, starts the scoped utun with SafeStart, requests HTTPS through ordinary system routing, then hot-applies an IP rule for interface A. | SafeStart succeeds; both logged flows belong to this test process; outcome, rule, interface, bidirectional byte counts, and live traffic are correct. `Stop` restores routes and utun inventory before the command returns. Only a successful smoke check unlocks later cases. |
| 4. Rule batch | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --batch` | No dialog. Starts a fresh bounded capture and changes only the test scene. | Verifies interface B, TLS SNI domain matching, the second target of one rule, block priority over an earlier route, missing-interface fallback, explicit system fallback, and strict unreachable. Opens TLS on A, changes the rule to B, completes the same connection on A, then verifies a new connection on B. The capture start timestamp stays unchanged during hot reload. Ends stopped. |
| 4a. Optional app-specific routing | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --app-bypass` | No dialog. Requires a passed smoke check and public-IP preflight evidence. Uses the current standalone `rootcheck` executable path as the app/tool identity, and creates a checksum-verified copy at a different path inside the private session directory. Each controlled child makes exactly one ordinary TLS request to the same frozen target. No helper installation, capture expansion, or VPN setting change occurs. | The parent matches app+IP and then app+domain rules through physical interface A. Under each rule, the different executable remains unmatched and uses the saved system route. A final app block after the route rule wins. Every case checks PID, executable/app path, destination, rule, outcome, interface, and byte counts; successful requests must match the physical or system public-IP preflight. Children are acknowledged and waited for on every outcome, their copy is removed, and routing ends stopped. Identical public exits do not prove a distinct exit-node bypass. |
| 5. Native TCP preservation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --preserve` | No dialog. Establishes an ordinary TLS connection **before** capture, starts capture, then sends HTTP over that same native connection. | The pre-existing kernel connection survives route activation and receives a verified HTTPS response. This flow is not claimed as a new proxied/logged connection. Ends stopped. A server that closes idle TLS before the request makes this case inconclusive; do not count it as a pass. |
| 6. SafeStart rollback | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --safe-failure` | No dialog. Starts with a block rule for the exact SafeStart destination and SafeStart enabled. | `Start` returns `ErrNetworkLost`; status reports the same failure; routes/utun are restored. Cleanup calls `Stop` so the next command sees an idle helper. |
| 7. Optional observation | `"$PITH_TEST_BIN/rootcheck" check --session "$PITH_SESSION" --observe 90s` | No dialog. Runs the smoke checks, then holds the reviewed host capture for at most 90 seconds. See the manual cases below. | The harness records an observation window, not a pass for an unimplemented manual assertion. It polls helper status and stops on interruption, failure, or expiry. |
| 8. Explicit stop | `"$PITH_TEST_BIN/rootcheck" stop --session "$PITH_SESSION"` | No dialog; authenticated IPC only. Safe to repeat while the helper is installed. | Status is stopped; route snapshot and utun inventory match preparation. If IPC is broken, use emergency cleanup instead. |
| 9. Uninstall | `"$PITH_TEST_BIN/rootcheck" uninstall --session "$PITH_SESSION"` | Administrator dialog requests `Pith needs your password to remove its routing helper.` Enter the password. Attempts `Stop`, then independently boots out launchd, checks stale capture ownership, and removes managed files. | Helper, service, socket, journal, and data directory are absent; original routes and utun inventory are restored. Private session artifacts remain for review only. |

macOS supplies the surrounding dialog title, buttons, and any cached
authorization behavior. The quoted prompt strings are the harness's exact
English text. A missing cancellation dialog is not a cancellation pass. No
update dialog is expected because the harness refuses an existing installation;
testing helper updates needs its own reviewed case.

All snapshots and detailed reports, including real addresses and MACs, stay
under `PITH_SESSION`. Do not attach or commit them without redaction. The
harness compares route and utun snapshots strictly. Another VPN, interface
change, or unrelated route-cache change can make the result inconclusive; it
never removes an unrelated route to force a pass. It also checks public-IP
echoes against preflight when available. A rotating public exit can make this
evidence inconclusive and requires review rather than an automatic retry.

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
* **Crash cleanup:** only after the coordinator approves this deliberate fault,
  interrupt the *managed service only* during observation with an administrator
  dialog running `/bin/launchctl kill SIGKILL system/io.github.kel077.pith.helper`.
  Kernel utun ownership must remove capture when the process dies. Launchd may
  restart the daemon, but it must come back idle. The observation command is
  expected to report failure rather than claim a normal pass. Verify original
  routes and no extra utun, then uninstall. Do not use `killall`, a guessed PID,
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
the original snapshot for another mode is not a valid rollback reference.
With the unchanged `rootcheck` harness, finish and uninstall the current
phase's helper **before** the user changes modes, then run `prepare`, review
the new artifacts, and follow `cancel` and `install` for that mode. The harness
refuses an existing installation and intentionally refuses a changed route
baseline. Do not edit `original.json` or bypass those checks. These are several
explicitly approved phases of one coordinated window, not permission to leave
the helper installed for a later unattended session.

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
| No exit node | The user selects no exit node in the existing Tailscale UI, only if this was explicitly approved. No new Pith dialog is required for this selection. Any Tailscale/macOS dialog is version-dependent: record its actual wording and obtain approval before accepting it. A fresh helper phase repeats the exact cancel/install/remove prompts listed above. | The chosen tailnet service and MagicDNS lookup work before capture, during a Pith route/block case for the separate public test host, and after Stop. Unmatched public traffic retains the saved system route for this mode. Existing Tailscale routes and its tunnel are unchanged by Pith; Pith selects an unused tunnel address without conflicting with existing interface addresses or tunnel/subnet routes. A failed preflight makes the connectivity case inconclusive. | Stop and uninstall Pith against this phase's baseline. The user restores the original Tailscale mode, or explicitly selects the next approved mode. Pith does not disconnect Tailscale. |
| Approved exit node | The user selects the already authorized exit node while Pith is stopped. This changes Tailscale's routing behavior; record a new baseline, including its public exit and tailnet/MagicDNS results. Extra VPN/OS prompts need their actual wording reviewed. A fresh helper phase adds the same cancel/install/remove dialogs as above; `--app-bypass` itself adds no dialog. | First verify unmatched traffic to the captured public test host still uses the exit node. Run `--app-bypass`: the main test executable must use the chosen physical interface under app+IP and app+domain rules, while its different-path child remains unmatched and uses the exit node. The final matching block must prevent the parent's new connection even when a route rule also matches. Require captured-flow evidence and the expected physical versus system public-IP echoes; if the echoes cannot distinguish exits, record bypass as inconclusive. Tailnet and MagicDNS behavior remain consistent with the preflight throughout. | Stop and uninstall Pith, verifying that the exit-node routes/tunnel and tailnet service still work. The user restores the initial exit-node choice and DNS preferences recorded before the window. Never remove Tailscale routes as cleanup. |
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

After the last phase, verify Pith's managed paths/service are absent and the
user has restored the original Tailscale settings. Compare each Pith cleanup
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
and compare the original route and utun snapshots. Remove the private session
and build directories only after that review. No helper, watcher, test socket,
or test process should remain running when the session is complete.
