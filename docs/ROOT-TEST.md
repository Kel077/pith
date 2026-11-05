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
also accepted, but then the actual-interface evidence is the helper's log and
the preflight bound socket. When both interfaces share the same public exit,
an IP echo cannot distinguish them. Record that limitation.

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
