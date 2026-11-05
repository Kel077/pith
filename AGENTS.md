# 橘核 Pith — notes for agents

Pith (橘核) is an open-source macOS menu bar app, for Apple Silicon only,
that sends each app's connections out through the network interface the
user chooses: by app, domain, IP or CIDR, with fallback interfaces, block
rules, scenes, a connection log, traffic statistics, connectivity tests and
a rule checker. MIT licensed.

## Team and ownership

| Area | Owner |
|---|---|
| `cmd/pith`, `internal/ui`, `internal/mock`, `locales`, `resources` (icons, images), `mygo.json`, `docs/DESIGN.md` | Claude (front end, design, integration) |
| `cmd/pithd`, `internal/client`, `internal/engine/...`, `internal/helper`, `internal/rules`, `internal/netif`, `scripts`, `Makefile`, `.github` | Astra (engine, helper, data, build, CI) |
| `internal/api` — the contract | shared: see below |
| `README.md`, `docs/ARCHITECTURE.md` | both, each for their part |

The interface uses [MyGo](https://github.com/egoist/mygo) (v0.7.4, pure Go,
no cgo) with its native UI toolkit `github.com/egoist/mygo/ui`. Everything
is Go: one module, `github.com/Kel077/pith`. Dependencies must have
MIT-compatible licenses (MIT, BSD, ISC, Apache-2.0); no GPL or LGPL code.

## The contract: `internal/api`

The interface depends on `internal/api` alone; `internal/mock` implements
`api.Service` in memory for UI work, `internal/client` for real. Adding a
field, type or method is fine in its own commit titled `api: …`, reported
to Claude. Renaming, removing or changing the meaning of anything there
needs Claude's agreement first.

## Git

- `main` is the integration branch; Claude works there and pushes to
  GitHub. Astra works on the `engine` branch in its own worktree.
- Commit small and often, with messages in English, imperative mood,
  prefixed by area: `engine: …`, `helper: …`, `api: …`, `ui: …`.
- Never push, force-push or rewrite history: Claude merges `engine` into
  `main` at milestones and pushes.
- Never commit secrets, logs, captures or anything with the user's IP
  addresses, MAC addresses or hostnames: tests use documentation ranges
  (192.0.2.0/24, 2001:db8::/32) and `example.com`.

## Building

- Requires Go 1.27 and macOS on Apple silicon; code is signed ad hoc by mygo build.
- `go vet ./... && go test ./...` must pass before every commit.
- The helper ships inside the app as `resources/darwin-arm64/bin/pithd`
  (a build product, not committed), which `mygo build` signs with the app.

## Safety rules (the user's, non-negotiable)

1. Read before you write: learn an unfamiliar system API from its
   documentation or source before calling it.
2. Nothing that changes the Mac's network or system state — routes, DNS,
   pf, interfaces, launchd daemons, files outside the repository and
   `$TMPDIR` — without Claude's go-ahead. Changes need root anyway, and
   `sudo` asks for a password nobody but the user can type: when a test
   needs root, stop and ask Claude, who asks the user. Never run `sudo`.
3. Before changing system state, record how it was and how to undo it.
4. Nothing keeps running: every process you start, stop it when done. No
   daemons, loops or watchers left behind; the helper is installed only
   for an agreed test, and removed again unless the user says otherwise.
5. Try one before many: one case first, then the batch; retries are
   bounded.
6. The agents and the user's work depend on this Mac's network. Code that
   takes over routing must undo itself when it fails (SafeStart) and when
   its process dies.
