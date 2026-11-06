# Helper identity independent of app versions

Helper compatibility depends on the bundled artifact and protocol, independently
of the app's version. The client authenticates both the installed file and the
executing peer before IPC. Replacement and recovery preserve the same identity
and capture-ownership checks.

The idle-only hardware acceptance at `54a2881` passed fresh installation, an
app-only version change with no authorization and the same helper PID, explicit
changed-artifact replacement, an unreadable installed-file rejection and removal.
The unreadable-file case returned `helper_untrusted` for Start and Stop and
restored mode 0755. It does not establish a substituted live peer or every
rollback case; see the [recorded scope](ROOT-TEST.md#recorded-helper-identity-result).

## Build identity

The helper has its own display version, advanced only for helper releases. The
app version, repository revision and build time are not embedded in `pithd`.
The exact artifact, rather than that display version, determines compatibility:

- protocol version;
- SHA-256 of the complete, final signed helper file;
- the executing Mach-O CodeDirectory hash (CDHash).

Generate this identity after deterministic ad-hoc signing, with a fixed signing
identifier and no signing timestamp. Embed the expected identity in the app at
build time; a neighboring editable manifest is not the trust anchor. Verify that
the final app contains exactly those helper bytes after MyGo packaging. If MyGo
resigns the helper differently, fail the build rather than accepting a changed
identity. Development builds must generate the same identity through `make helper`.

An app-only version, UI or documentation change must produce the same helper
identity with the same toolchain and dependencies. An actual helper, compiler or
linked dependency change can require replacement even with the same display
version. Compare two builds with different app versions in the build tests.

## Verification before IPC

The installed file must be regular, root-owned, not writable by group/others, and
reached without following a substituted symlink. Read/hash the same opened file;
check its metadata again after reading. Its complete hash must match the embedded
identity. A cache may avoid rehashing unchanged file identities, but must include
device, inode, size, modification/change times and permissions, and be invalidated
by installation or inconsistent metadata.

On every newly connected Unix socket, before writing an HTTP request:

1. Preserve the existing root peer-credential requirement.
2. Obtain the kernel peer audit token and validate the executing peer's CDHash
   against the embedded expected identity, tying the query to the token's PID
   generation. A matching on-disk pathname alone cannot prove that the running
   process uses the current file.
3. Require the protocol version on the authenticated response. A version string
   or a hash reported by the peer is never its own authentication.

XNU exposes `LOCAL_PEERTOKEN` in
[un.h](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/un.h).
Its read-only `CS_OPS_CDHASH` query accepts a peer audit token and checks the PID
generation in
[kern_proc.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_proc.c).
The pure-Go implementation uses these documented read-only ABIs. An unavailable
or failed identity check rejects IPC; there is no version-only fallback.

No Developer ID, keychain access, signing entitlement or new privileged operation
is needed for this design. Ad-hoc identity binds the peer to the app's bundled
artifact; it does not establish a publisher identity or defend against a
compromised root account/kernel or replacement of the entire app.

## Compatibility and status

`HelperStatus.Outdated` means **the installed helper must be replaced to
match this app's expected artifact or protocol**, not "older than the app."
`ErrHelperOutdated` reports that actionable condition, including an app downgrade
whose helper differs. `Version` is the helper's own informational version; it is
never compared with the app version to authorize IPC.

`ErrHelperUntrusted` (`helper_untrusted`) reports an unverifiable
socket, process, installation metadata, or bundled identity. It must not be
silently converted to ready, stopped or missing. No hashes need to enter the UI
contract.

| Observation | Client behavior | UI and administrator approval |
| :-- | :-- | :-- |
| No installed helper | `Installed=false` | Installation requires administrator approval |
| Expected artifact and protocol, app version changed | Ready; `Outdated=false` | Show helper version; no replacement or administrator approval required |
| Secure installed file differs, or authenticated protocol differs | `Outdated=true`; no ordinary commands to the differing peer | Replacement requires administrator approval |
| Expected installed artifact, no answering peer | `Failed` with `helper_not_responding`; routing state is unknown | Status retries request no approval; explicit repair can re-bootstrap verified installed bytes with administrator approval |
| Peer, path or file metadata cannot be verified | `ErrHelperUntrusted`; no IPC | Cannot verify helper; offer documented recovery without automatically requesting administrator approval |
| Bundled artifact differs from the app's embedded identity | Refuse that installation payload; existing verified IPC remains usable | Rebuild or reinstall the app; never elevate an invalid payload |

For an unverified/different running process, do not obtain its version or engine
state over IPC and do not display a claimed "routing stopped" state. `Running`
continues to mean an answering verified helper; unavailable engine state needs
the error/replacement presentation. Local configuration, interface listing,
probing and rule checking remain usable without a verified helper.

An unverifiable helper is returned by `Status` as `State: Failed` with
`Error.Code: helper_untrusted`, not a transport error. In its helper status,
`Installed` reflects the actual installed file; `Running` and `Outdated` are false,
with an empty version and no scene. Helper operations return `ErrHelperUntrusted`
without sending commands. Missing build identity alone never invents an installation.

Ordinary IPC does not read the bundle. Moving or replacing the running app on disk
cannot revoke Status, Subscribe or Stop when its compiled identity still matches
the installed file and executing peer. A bundled payload is checked only when
it is to be used for installation or recovery.

## Replacement, recovery and migration

Replacement uses administrator-approved staging, checksum verification, launchd
bootout, verified recovery and restart, with rollback on failure. Revalidate the
new peer before declaring success. An authorization
cancel leaves the existing installation intact. Never ask solely because the app
version increased.

When the old peer cannot be authenticated or speaks another protocol, do not send
it commands. Replacement/removal requires administrator approval and may boot out
the fixed launchd job; any journal cleanup must use a verified staged helper, not execute
the unverified installed binary. Preserve ambiguous journals and fail rather
than guess ownership or broaden route deletion.

Uninstall remains available with an unverifiable peer when the bundled cleanup
artifact is valid. A damaged bundle may instead use a root-owned installed file
whose complete bytes match the compiled identity, copied and verified in the
same root-owned stage. If neither artifact can be verified, reinstall the app
before cleanup; no unverified executable is run or journal discarded.
If cleanup fails after bootout, removal preserves the executable, plist and journal
and attempts to restore the previously loaded registration. An explicit repair
can use the verified installed bytes even when the original app is gone.

Existing installations may require one replacement at migration. Do not add a
trust-on-first-use exception or accept an arbitrary old binary's claimed version.
An exact matching existing artifact may be accepted after the same kernel/file
checks; migration itself does not require administrator approval.

Tests must cover app-only bumps, changed artifacts with unchanged display
versions, protocol mismatch, missing/offline helpers, mismatching file and running
process identities, PID reuse, symlink/writable metadata, cancellation/rollback,
and invalid bundle manifests. Use injected file/peer/signing fixtures for unit
tests; any native install acceptance remains a separately authorized session.
