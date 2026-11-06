# Building Pith

Build on macOS with Apple Silicon and the Go version specified in `go.mod`.
The macOS Command Line Tools provide the build and signing tools. Xcode, cgo,
an Apple Developer account and a Developer ID certificate are not required.
The MyGo CLI is pinned to v0.7.4.

## App and helper

```sh
make check
make build
```

`make check` runs `go vet ./...` followed by `go test ./...`. It does not install
the helper or activate routing. Privileged routing tests require a separate,
explicitly approved test session.

`make build` performs these steps:

1. Read the app version from `mygo.json`.
2. Build `cmd/pithd` for `darwin/arm64` with `CGO_ENABLED=0`, writing
   `resources/darwin-arm64/bin/pithd`. Its informational version comes from
   `helper.Version`, independently of the app. Sign it ad hoc with a fixed
   identifier, 4096-byte code pages, an 18000-byte CMS reservation and no timestamp,
   then freeze its identity in generated client code.
   Normalize the helper to mode `0755` before signing, independently of umask;
   identity generation and final verification reject group/other write access.
3. Collect the reviewed licenses and notices for both binaries.
4. Run `go run github.com/egoist/mygo/cmd/mygo@v0.7.4 build` for
   `darwin/arm64`, with ad hoc signing and without notarization or DMG creation.
5. Verify both executables' architecture, the frozen helper identity and app version,
   the bundle identifier, the bundled license texts, and both signatures.

The app is `build/darwin-arm64/Pith.app`. MyGo merges the ARM64 resource
directory into the bundle, so the helper ships at
`Pith.app/Contents/Resources/bin/pithd`. MyGo preserves its existing valid ad hoc
signature; final verification rejects any byte change during packaging. Only the
app receives `mygo.json`'s version through `MYGO_LDFLAGS`.

`make helper` does not read the app version. It generates
`internal/client/helper_identity_generated.go`, which embeds the expected complete
SHA-256, CDHash, protocol and helper version into the client at compile time. The
adjacent `pithd.identity.json` is a build-verification record, never a runtime trust
anchor. Both generated files are ignored by Git. The helper does not depend on
the client, so generating a new client identity cannot change the helper's bytes.

Neither the app nor the helper service is started by these commands. The only
helper invocation is `pithd --version`, which exits before privileged setup.
`make verify` repeats the bundle checks without rebuilding.

For development with `go run ./cmd/pith`, first run `make helper`. The client
also searches `resources/darwin-arm64/bin/pithd` when the resources directory
has not been packaged. A development app uses the identity compiled by
`make helper`; an absent identity must not enable a version-only verification
fallback. Changing the helper requires rebuilding that app/controller too.

From a fresh checkout, `make helper` followed by `go run ./cmd/pith` enables the
real backend's explicit helper installation and verification. `make build`
instead produces the complete app bundle. Running `go run ./cmd/pith` alone
without the generated identity still opens the interface, but helper status is
`helper_untrusted` and installation is refused before administrator approval.

## Archives and disk images

```sh
make archive
```

This builds the app and creates `build/pith-<version>-arm64.zip` with
`zip -qry -X`, preserving executable permissions, symbolic links and the app
bundle layout without macOS extended metadata. Each build uses a fresh archive;
no stale entries can survive from an earlier ZIP. It rejects every `._*` or
`__MACOSX` path component, extracts with plain `unzip` into a private temporary
directory, and requires `codesign --verify --deep --strict` on that extracted app
before replacing the final archive. Temporary files are removed on success or
failure. Finder/ditto extraction alone cannot prove this: they consume AppleDouble
entries that plain `unzip` would leave inside the signed bundle.

```sh
make dmg
```

This explicitly enables MyGo's DMG step and produces
`build/darwin-arm64/pith-<version>-arm64.dmg`. Unlike `make build`, it temporarily
attaches a writable disk image under the temporary directory to prepare its
Finder layout, detaches it, and compresses the result. MyGo attempts to detach
the image on errors too. Do not run this target during a session that forbids
temporary disk mounts. It does not install Pith or the helper.

The app and disk image use ad hoc signing and are not notarized. These are
development artifacts; building them does not grant Gatekeeper approval on
another Mac. The build does not alter Gatekeeper or remove quarantine flags.

## Caches and cleanup

The build script defaults `GOCACHE`, `GOMODCACHE` and `GOPATH` to separate
`pith-go-*` directories under `TMPDIR` (or `/tmp` if unset), and sets `GOENV=off`.
Explicit environment values for those cache directories are respected. Keep
them inside the repository or a temporary directory for local test sessions.
No `go env -w` command is needed. Go builds use `-trimpath` and
`-buildvcs=false` to omit local source paths and working-tree metadata.

Helper signing also pins `codesign --pagesize 4096 --signature-size 18000`.
The latter fixes the CMS reservation, not the complete embedded signature size.
Different macOS signing defaults otherwise change the CodeDirectory, Mach-O
load commands and padding even when Go emitted identical code. The fixed values
reproduced the released v0.1.0 helper byte for byte on a newer signing host;
`codesign --verify --strict` also passed. This does not promise identical helpers
across different Go toolchains, dependencies, target settings or custom build
flags, nor reproducible complete app archives. `--signature-size` is an advanced
codesign option; retain strict signature and frozen-identity checks when changing
the signing tools.

```sh
make clean
```

This removes `build/`, the helper, its generated identity files, and the generated
`resources/darwin-arm64/licenses/`. It leaves source assets and caches in place.
None of the generated binaries, archives, disk images or license copies belong
in Git.

## Dependency licenses

`make licenses` generates `resources/darwin-arm64/licenses/`; normal app builds
run it automatically. MyGo packages that directory at
`Pith.app/Contents/Resources/licenses/` before signing. The directory contains:

- The project's MIT license and the Go runtime's BSD license and patent grant.
- Complete, unmodified license, notice, copying and patent texts from the
  modules used by the two shipped binaries.
- `NOTICE.txt`, listing versions and selected licenses and retaining relevant
  leading copyright and license comments from compiled dependency source files.
- `manifest.json`, recording the packaged license files and their SHA-256 hashes.

The collector runs `go list -deps` for `./cmd/pith` and `./cmd/pithd` with
`GOOS=darwin`, `GOARCH=arm64` and `CGO_ENABLED=0`. It matches MyGo's production
inspector tag for the app. It does not use `go list -m all`, download unrelated
modules, or include build-tool and test-only dependencies. It checks each
compiled package and embedded resource's parent directories for additional
notices, up to that module's root.

`scripts/licenses/approvals.json` records the reviewed dependency versions,
chosen licenses and exact notice hashes. A missing file, changed text, new
notice, unreviewed module version or module replacement fails the build. After
updating a dependency or Go, review its license and relevant source notices
before updating this file. `typesetting` offers a license choice; this build
selects BSD-3-Clause while preserving the complete upstream dual-license text.
All selected third-party terms are MIT, BSD or Apache-2.0. Module-wide notices
may describe optional code that is not included in Pith; they are preserved
rather than edited.

`make verify` checks the packaged texts against the current reviewed dependency
graph as well as checking the signatures. Generated license files should not
be edited or committed.

## Continuous integration

`.github/workflows/ci.yml` runs on GitHub's standard `macos-15` ARM64 runner.
It reads the Go version from `go.mod`, runs vet and tests, builds and verifies
the ad hoc signed app, and stores its ZIP for seven days on manual runs only.
Pushes/PRs that change only Markdown, `docs/`, `LICENSE` or issue templates skip
CI; a mixed change still runs. Actions are pinned
to full commit IDs. The job has read-only repository permissions, performs no
root tests, and does not mount a DMG or publish a release.

The job initializes its temporary Go cache paths from `RUNNER_TEMP` in a step
before `setup-go`; the `runner` context is not available in job-level `env`.
After editing the workflow, validate its expressions and structure locally:

```sh
GOCACHE="${TMPDIR%/}/pith-go-build" GOMODCACHE="${TMPDIR%/}/pith-go-mod" GOPATH="${TMPDIR%/}/pith-go-path" GOENV=off go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck= -pyflakes= .github/workflows/ci.yml .github/workflows/release.yml
```

This pinned MIT-licensed validation tool is not part of the app's dependencies.
Passing local validation does not replace a successful GitHub Actions run.

## Draft releases

`.github/workflows/release.yml` runs on a pushed `v*` tag, or manually **on that
existing tag**. For example:

```sh
gh workflow run release.yml --ref v0.1.0
```

The tag must be a full semantic version prefixed by `v`, match `mygo.json`, and
have exactly one nonempty `CHANGELOG.md` section headed
`## [<version>] - YYYY-MM-DD`. Set a valid release date before tagging. A manual
run on `main` is rejected so provenance identifies the same source ref/commit
that was built. No workflow creates or moves tags.

The ARM64 build job runs `make check`, `make archive`, then attempts `make dmg`.
Successful assets have fixed names, `pith-<version>-arm64.zip` and
`pith-<version>-arm64.dmg`. If the runner's DMG packaging fails or times out, a
visible warning identifies a ZIP-only draft; no stale or partial DMG is attached.
Review any packaging failure before publishing.

`SHA256SUMS` lists exactly the attached ZIP and, when successful, DMG, with
relative filenames. `actions/attest`, pinned to a full commit SHA like every
action, records SLSA build provenance for those archive digests using GitHub's
short-lived OIDC identity. It needs no signing secret or Developer ID, and does
not notarize the app. The build job has repository read, attestation write and
OIDC permissions, but no repository write permission.

A separate bounded job verifies that the remote tag has not moved, downloads
only this build's immutable artifact, checks the exact file set and hashes, and
compares the notes with the tagged changelog again. Its only repository permission
is `contents: write`; the generated job token is supplied only to
`gh release create --draft --verify-tag --notes-file ...`. It attaches the ZIP,
optional DMG and `SHA256SUMS`, marks prerelease versions appropriately, and requires
separate manual publication. It never overwrites an existing release. If draft
creation/upload is interrupted, review the partial draft before deleting it and
rerunning; no automatic cleanup can remove a published release.

For downloaded artifacts, from the directory containing the release files:

```sh
shasum -a 256 --check SHA256SUMS
gh attestation verify pith-0.1.0-arm64.zip --repo Kel077/pith
```

Download every archive listed in `SHA256SUMS` to check the complete file. The
checksum detects changed download bytes; GitHub's provenance separately binds
the archive to the workflow/source. Neither replaces macOS code-signing trust.
The pure-Go `scripts/release` tests use temporary fixtures only; validating this
workflow locally does not run DMG mounts, GitHub release writes or root tests.

See [MyGo's distribution guide](https://github.com/egoist/mygo/blob/v0.7.4/docs/distribution.md)
for bundle resources, signing and disk images, and
[GitHub's runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
for the ARM64 runner labels.
