# Privileged helper and IPC

The helper is a pure-Go arm64 executable. `pithd --version` is unprivileged;
normal startup requires root and `--owner-uid <installing user's UID>`. launchd
starts it idle, with `ProcessType` set to `Interactive` so its forwarded
sockets avoid the background traffic class. A Background job has the lowest
CPU priority and its sockets yield to other traffic on a busy Mac. The routing
engine is linked into `cmd/pithd`. Importing or constructing it leaves capture
off. An authenticated request starts it. Local proxy ports and
the MCP socket belong to the menu bar app.

HTTP/JSON runs only over a Unix socket, with protocol version 1 in
`X-Pith-Protocol`. The status response also announces its protocol/build
version. The socket is mode 0600, owned by the installing user. The server
checks Darwin `LOCAL_PEERCRED` on every accepted connection. Before sending HTTP,
the client verifies the installed artifact's full SHA-256 and the root peer's
kernel audit token/CDHash. The expected identity is compiled into the app, not
learned from the peer or an adjacent manifest. App versions are informational;
only a differing helper artifact/protocol requires replacement. There is no TCP
listener or trusted UID in JSON.

| Method | Path | Result |
| --- | --- | --- |
| GET | `/v1/status` | Helper and routing state. |
| GET | `/v1/events` | NDJSON stream. Status and the interface list come first, followed by updates. |
| POST | `/v1/start`, `/v1/reload`, `/v1/stop` | Start, update or stop capture. |
| POST | `/v1/connections`, `/v1/connections/clear`, `/v1/traffic` | Read or clear connection history, or query routing totals. |
| POST | `/v1/yield` | Hold a temporary change of primary network for a router renewal. |

Except for `GET /v1/status`, requests must carry the matching protocol header.
The status response announces its own protocol. JSON request bodies that the
helper decodes have a 4 MiB limit. The initial `GET /v1/events` interface
snapshot is an explicit JSON array, even when empty. Cancellation, EOF or
helper exit ends the stream. During shutdown the helper stops its backend and
removes its socket.

The helper retains its last interface snapshot across a failed read and retries.
If the event stream ends, the client reconnects. It allows five seconds to
open a stream and read the initial snapshots. If setup fails and the cause
requires a status check, that check has its own five-second budget.

`internal/primary` keeps one interface from being macOS's primary while its router
renews its address, for as long as the client keeps the request open and at
most ten minutes. The helper adds a `PrimaryRank` of `Never` to the State:
entity of the interface's network service, as a temporary key of a `scutil`
session, and answers one NDJSON line naming the interface macOS made primary
instead. configd removes the key when that session closes: when the client
closes the request, and when the helper or `scutil` exits for any reason. It
changes nothing when no other service has a router or the entity exists
already, and one interface yields at a time.

The installer changes only these managed paths and launchd service:

- `/Library/PrivilegedHelperTools/io.github.kel077.pith.helper`
- `/Library/LaunchDaemons/io.github.kel077.pith.helper.plist`
- `/Library/Application Support/Pith` (0700)
- `/var/run/io.github.kel077.pith.helper.sock`
- launchd `system/io.github.kel077.pith.helper`

An update records the previous executable, plist and loaded state before
booting out the service. It stages a root-owned executable, checks the SHA-256
computed before authorization, and validates the plist. Failure restores the
old files and registration. The app's installation plan pins the expected SHA
before authorization and executes only the verified root-owned staged helper for
stale-journal cleanup. Successful installation removes the temporary backup.
Uninstallation uses the same staged-copy verification, never an unverified
installed executable. If the bundle is damaged, a matching root-owned installed
artifact can supply the staged cleanup copy; if neither copy is verifiable,
reinstall the app first. Ambiguous cleanup preserves the ownership journal and
fails instead of deleting unproven routes. No launchd command is called by tests: installer tests parse the shell
with `sh -n` and decode the plist. Peer, request, subscription, cancellation,
hot-reload and shutdown tests use a temporary socket and no elevated process.

## Privileged operations

Installation, real routing acceptance and uninstall require an administrator's
approval. No unit test installs the daemon, creates a real utun or changes routing.
Uninstall removes helper logs/traffic;
reinstalling restores the service, not deleted history. The consolidated procedure
must record original routes/managed paths and include emergency undo before any
authorization is requested. See [backend architecture](../../docs/ARCHITECTURE.md).

System references: Apple's [launchd guide](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html),
[AppleScript command invocation](https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/CallCommandLineUtilities.html),
the local `launchctl help bootstrap/bootout`, and SDK `sys/ucred.h`.
