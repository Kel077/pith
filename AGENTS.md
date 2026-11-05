# Notes for coding agents

Pith is a macOS menu bar app, for Apple silicon only, that sends each
app's connections out through the network interface the user chooses: by
app, domain, IP or CIDR, with fallback interfaces, block rules, scenes, a
connection log, traffic statistics, connectivity tests and a rule checker.
Everything is Go, in one module, `github.com/Kel077/pith`, under the MIT
license.

## Architecture and design

Before changing the engine, the helper or the client, read the
[architecture](docs/ARCHITECTURE.md). Before changing the interface, read
the [design](docs/DESIGN.md): its look (ambient glows, panes of light
glass, capsules, hairlines) and what each page is for.

## The contract: `internal/api`

The interface depends on `internal/api` alone: `internal/mock` implements
`api.Service` in memory, and `internal/client` for real. Add a field, a
type or a method in a commit of its own, titled `api: …`. Renaming or
removing anything there, or changing what it means, needs the agreement
of both owners.

## Code standards

- Pure Go, without cgo, for Apple silicon. Dependencies must have
  MIT-compatible licenses (MIT, BSD, ISC, Apache-2.0): no GPL or LGPL
  code, and nothing copied from projects under those licenses.
- English everywhere: code, comments, documentation, commit messages. The
  only other languages are the translation catalogs in `locales/` and
  `README.zh-CN.md`.
- The interface's strings live in the catalogs: `locales/en.json`, which
  describes each message, and one catalog per language. After editing
  them, run `go run github.com/egoist/mygo/cmd/mygo@v0.7.4 generate` and
  commit `locales/catalog_gen.go`.
- `go vet ./... && go test ./...` passes before every commit.
- Tests use documentation addresses (192.0.2.0/24, 198.51.100.0/24,
  203.0.113.0/24, 2001:db8::/32) and `example.com`, never a real
  network's.

## Owners

| Area | Owner |
| :-- | :-- |
| `cmd/pith`, `internal/ui`, `internal/mock`, `locales`, `resources`, `mygo.json`, `docs/DESIGN.md`, the READMEs | Claude: the interface, its design, integration |
| `cmd/pithd`, `internal/client`, `internal/engine/…`, `internal/helper`, `internal/rules`, `internal/netif`, `scripts`, `Makefile`, `.github`, `docs/ARCHITECTURE.md` | Astra: the engine, the helper, data, builds, CI |
| `internal/api` | both, by agreement |

## Git

- `main` is the integration branch: the interface's owner works there and
  pushes it. The engine's owner works on `engine`, in a worktree of its
  own, which is merged into `main` at milestones.
- Commit small and often, in English, in the imperative, prefixed by area:
  `engine: …`, `helper: …`, `api: …`, `ui: …`, `docs: …`.
- Never force-push or rewrite history.
- Never commit secrets, logs, packet captures, or a real network's
  addresses, interface names or host names.

## Building and reviewing

- Requires Go 1.27 and macOS on Apple silicon; code is signed ad hoc by mygo build.
- `go run ./cmd/pith -mock` runs the interface on the mock backend;
  `-page`, `-lang` and `-panel` open it where a screenshot needs it.
- `go test ./internal/ui -run 'Snapshots$' -snapshots=DIR -lang=en` (or
  `-lang=zh-Hans`) renders every view from the mock, light and dark: look
  at them after changing the interface.
- The helper ships inside the app as `resources/darwin-arm64/bin/pithd`,
  a build product, not committed, which `mygo build` signs with the app.
- Notes about the maintainer's own Mac for local tests live in
  `AGENTS.local.md`, which is not committed.

## Safety

The maintainer's rules, which are not negotiable:

1. Read before you write: learn an unfamiliar system API from its
   documentation or source before calling it.
2. Nothing that changes the Mac's network or system state (routes, DNS,
   pf, interfaces, launchd daemons, files outside the repository and
   `$TMPDIR`) without the maintainer's go-ahead. Such changes need root,
   and `sudo` asks for a password only the maintainer types: when a test
   needs root, stop and ask. Never run `sudo`.
3. Before changing system state, record how it was and how to undo it.
4. Nothing keeps running: stop every process you start. No daemons, loops
   or watchers left behind; the helper is installed only for an agreed
   test, and removed again unless the maintainer says otherwise.
5. Try one before many: one case first, then the batch; retries are
   bounded.
6. The maintainer's work depends on this Mac's network. Code that takes
   over routing undoes itself when it fails (safe start) and when its
   process dies.
