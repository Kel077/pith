# Helper identity independent of app versions

Status: proposal for agreement; this document does not change the current API or
implementation. Today the build embeds the app version in both executables, and
the client compares those version strings. Both parts must change together.

## Build identity

Give the helper its own display version, advanced only for helper releases. Stop
embedding the app version, repository revision or build time in `pithd`. The exact
artifact, rather than that display version, determines compatibility:

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
Implement these documented ABIs in pure Go, without copying source. Verify their
availability on the minimum supported macOS before rollout. An unavailable or
failed identity check rejects IPC; there is no version-only fallback.

No Developer ID, keychain access, signing entitlement or new privileged operation
is needed for this design. Ad-hoc identity binds the peer to the app's bundled
artifact; it does not establish a publisher identity or defend against a
compromised root account/kernel or replacement of the entire app.

## API and UI agreement requested

Change `HelperStatus.Outdated` to mean **the installed helper must be replaced to
match this app's expected artifact or protocol**, not "older than the app."
Retain `ErrHelperOutdated` for that actionable condition, including an app
downgrade whose helper differs. Keep `Version` as the helper's own informational
version; never compare it with the app version to authorize IPC.

Propose additive `ErrHelperUntrusted` (`helper_untrusted`) for an unverifiable
socket, process, installation metadata, or bundled identity. It must not be
silently converted to ready, stopped or missing. No hashes need to enter the UI
contract. Any API edit is a separate `api:` commit after agreement.

| Observation | Client behavior | UI and administrator authorization |
| :-- | :-- | :-- |
| No installed helper | `Installed=false` | Install; one authorization on explicit action |
| Expected artifact and protocol, app version changed | Ready; `Outdated=false` | Show helper version; no update or password |
| Secure installed file differs, or authenticated protocol differs | `Outdated=true`; no ordinary commands to the differing peer | Replacement required; explicit Update/Replace needs authorization |
| Expected installed artifact, no answering peer | Not responding; do not infer that routing is stopped | Retry without authorization; explicit repair may require it |
| Peer, path or file metadata cannot be verified | `ErrHelperUntrusted`; no IPC | Cannot verify helper; offer documented recovery, not an automatic password prompt |
| Bundled artifact differs from the app's embedded identity | `ErrHelperUntrusted`; no installation | Reinstall the app; never elevate an invalid payload |

For an unverified/different running process, do not obtain its version or engine
state over IPC and do not display a claimed "routing stopped" state. `Running`
continues to mean an answering verified helper; unavailable engine state needs
the error/replacement presentation. Local configuration, interface listing,
probing and rule checking remain usable without a verified helper.

## Replacement, recovery and migration

Normal replacement first stops a verified compatible helper, then uses the
existing explicit authorization, staged checksum verification, launchd restart
and rollback. Revalidate the new peer before declaring success. An authorization
cancel leaves the existing installation intact. Never ask solely because the app
version increased.

When the old peer cannot be authenticated or speaks another protocol, do not send
it commands. Explicit authorized replacement/removal may boot out the fixed
launchd job; any journal cleanup must use a verified staged helper, not execute
the unverified installed binary. Preserve ambiguous journals and fail rather
than guess ownership or broaden route deletion. This also needs installer tests.

Existing installations may require one replacement at migration. Do not add a
trust-on-first-use exception or accept an arbitrary old binary's claimed version.
An exact matching existing artifact may be accepted after the same kernel/file
checks; migration itself is not a reason to show a password dialog.

Tests must cover app-only bumps, changed artifacts with unchanged display
versions, protocol mismatch, missing/offline helpers, mismatching file and running
process identities, PID reuse, symlink/writable metadata, cancellation/rollback,
and invalid bundle manifests. Use injected file/peer/signing fixtures for unit
tests; any native install acceptance remains a separately authorized session.
