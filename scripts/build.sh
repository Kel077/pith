#!/bin/bash
# Build only. Installing or starting the helper is a separate operation.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo"

fail() {
    printf 'pith build: %s\n' "$*" >&2
    exit 1
}

mode=${1:-app}
[[ $# -le 1 ]] || fail 'expected one target: check, helper, licenses, app, verify, archive, dmg or clean'
case "$mode" in
    check|helper|licenses|app|verify|archive|dmg|clean) ;;
    *) fail "unknown target: $mode" ;;
esac

if [[ "$mode" == clean ]]; then
    rm -rf "$repo/build"
    rm -f "$repo/resources/darwin-arm64/bin/pithd"
    rm -f "$repo/resources/darwin-arm64/bin/pithd.identity.json"
    rm -f "$repo/internal/client/helper_identity_generated.go"
    rm -rf "$repo/resources/darwin-arm64/licenses"
    exit 0
fi

[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || fail 'build on macOS with Apple Silicon'
command -v go >/dev/null || fail 'Go is required; install the version specified in go.mod'

# Keep caches out of the source tree and the user configuration directories.
# CI can supply its own temporary cache locations before invoking this script.
pith_tmp=${TMPDIR:-/tmp}
pith_tmp=${pith_tmp%/}
export GOCACHE=${GOCACHE:-"$pith_tmp/pith-go-build"}
export GOMODCACHE=${GOMODCACHE:-"$pith_tmp/pith-go-mod"}
export GOPATH=${GOPATH:-"$pith_tmp/pith-go-path"}
export GOENV=off
export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-mod=readonly -trimpath -buildvcs=false"

if [[ "$mode" == check ]]; then
    go vet ./...
    go test ./...
    exit 0
fi

helper="$repo/resources/darwin-arm64/bin/pithd"
helper_identity="$helper.identity.json"

build_helper() {
    mkdir -p "$(dirname "$helper")"
    GOOS=darwin GOARCH=arm64 go build -ldflags '-s -w' -o "$helper" ./cmd/pithd
    chmod 0755 "$helper"
    /usr/bin/codesign --force --sign - --identifier io.github.kel077.pith.helper --timestamp=none "$helper"
    /usr/bin/codesign --verify --strict "$helper"
    go run ./scripts/helperidentity -helper "$helper" -manifest "$helper_identity" \
        -generate "$repo/internal/client/helper_identity_generated.go"
    printf 'Built pithd %s for darwin/arm64\n' "$("$helper" --version)"
}

# The helper build never reads the app version or links it into the artifact.
if [[ "$mode" == helper ]]; then
    build_helper
    exit 0
fi

version=$(/usr/bin/plutil -extract version raw -expect string mygo.json)
name=$(/usr/bin/plutil -extract name raw -expect string mygo.json)
identifier=$(/usr/bin/plutil -extract identifier raw -expect string mygo.json)
[[ "$version" =~ ^[0-9][0-9A-Za-z.+-]*$ ]] || fail 'mygo.json version must be a nonempty version without spaces'
[[ -n "$name" && "$name" != */* && "$name" != . && "$name" != .. ]] || fail 'mygo.json name must be a single path component'
app="$repo/build/darwin-arm64/$name.app"

verify_app() {
    local plist="$app/Contents/Info.plist"
    local bundled_helper="$app/Contents/Resources/bin/pithd"
    [[ -f "$plist" && -x "$bundled_helper" ]] || fail 'app bundle or embedded helper is missing; run make build'
    [[ $(/usr/bin/plutil -extract CFBundleShortVersionString raw "$plist") == "$version" ]] || fail 'app version differs from mygo.json'
    [[ $(/usr/bin/plutil -extract CFBundleIdentifier raw "$plist") == "$identifier" ]] || fail 'app identifier differs from mygo.json'
    local executable
    executable=$(/usr/bin/plutil -extract CFBundleExecutable raw "$plist")
    [[ $(/usr/bin/lipo -archs "$app/Contents/MacOS/$executable") == arm64 ]] || fail 'app executable is not ARM64 only'
    [[ $(/usr/bin/lipo -archs "$bundled_helper") == arm64 ]] || fail 'embedded helper is not ARM64 only'
    # Reject group- or other-writable helpers as well as changed artifact bytes.
    go run ./scripts/helperidentity -helper "$bundled_helper" -manifest "$helper_identity" -verify
    go run ./scripts/licenses -verify "$app/Contents/Resources/licenses"
    /usr/bin/codesign --verify --strict "$bundled_helper"
    /usr/bin/codesign --verify --deep --strict "$app"
    printf 'Verified %s %s and its embedded helper\n' "$name" "$version"
}

build_app() {
    build_helper
    go run ./scripts/licenses
    local flags=(-platform darwin/arm64 -sign - -skip-notarize -o build)
    if [[ "$mode" != dmg ]]; then
        flags+=(-skip-dmg)
    fi
    # The app version is independent of the already frozen helper artifact.
    MYGO_LDFLAGS="${MYGO_LDFLAGS:+$MYGO_LDFLAGS }-X main.version=$version" \
        go run github.com/egoist/mygo/cmd/mygo@v0.7.4 build "${flags[@]}"
    verify_app
}

archive_app() (
    local archive="$repo/build/pith-$version-arm64.zip"
    local stage extracted='' entry entry_count=0
    stage=$(mktemp -d "$repo/build/.pith-archive.XXXXXX")
    trap 'rm -rf "$stage"; [[ -z "$extracted" ]] || rm -rf "$extracted"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    extracted=$(mktemp -d "$pith_tmp/pith-archive.XXXXXX")

    # A fresh archive cannot retain stale entries from an earlier build.
    # Store symlinks as links and omit macOS metadata without changing the app.
    (
        cd "$(dirname "$app")"
        /usr/bin/zip -qry -X "$stage/app.zip" -- "$name.app"
    )
    /usr/bin/unzip -Z1 "$stage/app.zip" > "$stage/entries.txt"
    while IFS= read -r entry; do
        case "/$entry/" in
            */._*|*/__MACOSX/*) fail "archive contains macOS metadata: $entry" ;;
        esac
        entry_count=$((entry_count + 1))
    done < "$stage/entries.txt"

    # Plain unzip leaves AppleDouble files in place, unlike Finder or ditto.
    /usr/bin/unzip -q "$stage/app.zip" -d "$extracted"
    /usr/bin/codesign --verify --deep --strict "$extracted/$name.app"
    printf 'Verified archive: %d entries, no macOS metadata, valid extracted signature\n' "$entry_count"
    mv -f "$stage/app.zip" "$archive"
    printf 'Created %s\n' "${archive#"$repo/"}"
)

case "$mode" in
    licenses) go run ./scripts/licenses ;;
    verify) verify_app ;;
    app|dmg) build_app ;;
    archive)
        build_app
        archive_app
        ;;
esac
