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

1. Read the version from `mygo.json`.
2. Build `cmd/pithd` for `darwin/arm64` with `CGO_ENABLED=0`, writing
   `resources/darwin-arm64/bin/pithd`.
3. Collect the reviewed licenses and notices for both binaries.
4. Run `go run github.com/egoist/mygo/cmd/mygo@v0.7.4 build` for
   `darwin/arm64`, with ad hoc signing and without notarization or DMG creation.
5. Verify both executables' architecture, the helper and bundle versions,
   the bundle identifier, the bundled license texts, and both signatures.

The app is `build/darwin-arm64/Pith.app`. MyGo merges the ARM64 resource
directory into the bundle, so the helper ships at
`Pith.app/Contents/Resources/bin/pithd` and is signed with the app. The build
sets both programs' `main.version` from the same configuration value; the
app receives it through `MYGO_LDFLAGS`.

Neither the app nor the helper service is started by these commands. The only
helper invocation is `pithd --version`, which exits before privileged setup.
`make verify` repeats the bundle checks without rebuilding.

For development with `go run ./cmd/pith`, first run `make helper`. The client
also searches `resources/darwin-arm64/bin/pithd` when the resources directory
has not been packaged. A development app's version must match `mygo.json`;
the build always sets this explicitly for the packaged app.

## Archives and disk images

```sh
make archive
```

This builds the app and creates `build/pith-<version>-arm64.zip` with `ditto`,
preserving executable permissions and the app bundle layout. Use this archive
when uploading an app as a GitHub Actions artifact.

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

```sh
make clean
```

This removes `build/`, `resources/darwin-arm64/bin/pithd`, and the generated
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
the ad hoc signed app, then stores its ZIP for seven days. Actions are pinned
to full commit IDs. The job has read-only repository permissions, performs no
root tests, and does not mount a DMG or publish a release.

The job initializes its temporary Go cache paths from `RUNNER_TEMP` in a step
before `setup-go`; the `runner` context is not available in job-level `env`.
After editing the workflow, validate its expressions and structure locally:

```sh
GOCACHE="${TMPDIR%/}/pith-go-build" GOMODCACHE="${TMPDIR%/}/pith-go-mod" GOPATH="${TMPDIR%/}/pith-go-path" GOENV=off go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck= -pyflakes= .github/workflows/ci.yml
```

This pinned MIT-licensed validation tool is not part of the app's dependencies.
Passing local validation does not replace a successful GitHub Actions run.

See [MyGo's distribution guide](https://github.com/egoist/mygo/blob/v0.7.4/docs/distribution.md)
for bundle resources, signing and disk images, and
[GitHub's runner reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
for the ARM64 runner labels.
