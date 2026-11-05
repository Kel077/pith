# Privileged helper and IPC

The helper is a pure-Go arm64 executable. `pithd --version` is unprivileged;
normal startup requires root and `--owner-uid <installing user's UID>`. Launchd
starts it idle. The engine backend is linked into `cmd/pithd`; importing or
constructing it never starts capture. The helper owns its complete shutdown.

HTTP/JSON runs only over a Unix socket, with protocol version 1 in
`X-Pith-Protocol`. The status response also announces its protocol/build
version. The socket is mode 0600, owned by the installing user. The server
checks Darwin `LOCAL_PEERCRED` on every accepted connection, and the client
requires a root peer. There is no TCP listener or trusted UID in JSON.

Endpoints: `GET /v1/status`, `GET /v1/events` (NDJSON), and POST requests for
`/v1/start`, `/v1/reload`, `/v1/stop`, `/v1/connections`,
`/v1/connections/clear`, `/v1/traffic`. Requests are size limited and version
checked. A subscription first emits Status and Interfaces; empty interface
snapshots remain explicit JSON arrays. Cancellation, EOF and helper exit
close streams. The process stops its backend and removes its socket on exit.

The installer changes only these managed paths and launchd service:

- `/Library/PrivilegedHelperTools/io.github.kel077.pith.helper`
- `/Library/LaunchDaemons/io.github.kel077.pith.helper.plist`
- `/Library/Application Support/Pith` (0700)
- `/var/run/io.github.kel077.pith.helper.sock`
- launchd `system/io.github.kel077.pith.helper`

An update records the previous executable, plist and loaded state before
booting out the service. It stages a root-owned executable, checks the SHA-256
computed before authorization, and validates the plist. Failure restores the
old files and registration. Successful installation removes the temporary
backup. No launchd command is called by tests: installer tests parse the shell
with `sh -n` and decode the plist. Peer, request, subscription, cancellation,
hot-reload and shutdown tests use a temporary socket and no elevated process.

## Pending authorized test

Installation (including cancellation), real scoped routing and uninstall are
reserved for one coordinated session after M3. No unit test installs the daemon,
creates a real utun or changes routing. Uninstall removes helper logs/traffic;
reinstalling restores the service, not deleted history. The consolidated procedure
must record original routes/managed paths and include emergency undo before any
authorization is requested. See [backend architecture](../../docs/ARCHITECTURE.md).

System references: Apple's [launchd guide](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html),
[AppleScript command invocation](https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/CallCommandLineUtilities.html),
the local `launchctl help bootstrap/bootout`, and SDK `sys/ucred.h`.
