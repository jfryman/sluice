# Development workflow

Related: [architecture.md](architecture.md), [practices.md](practices.md), [apply/sandbox.md](apply/sandbox.md),
[tui/summary.md](tui/summary.md).

## Make targets
| target | does |
|--------|------|
| `make` | `fmt-check` + `vet` + `test` + build `bin/sluice` |
| `make build` / `test` / `vet` / `fmt` / `check` | the obvious |
| `make watch` | `scripts/watch.sh`: re-runs `check build` on every `.go`/`go.mod`/`go.sum` save (inotifywait) |
| `make dev` | `sluice -sandbox` — full reflink clone; clones on first use |
| `make dev-scan` | `sluice -sandbox -scan` |
| `make dev-reset` | `sluice -reset-sandbox -scan` — re-clone from current real mail (~3 s) |
| `make run` / `make scan` | real mail (still plan-first: nothing changes until an apply) |
| `make install` | `check`, then install to `~/.local/bin/sluice` |
| `make install-service` | install + `make install` the binary, copy `contrib/systemd/sluice-sweep.service` to `~/.config/systemd/user`, enable and (re)start it — **run on the machine that should host sweep, not the dev box** |
| `make uninstall-service` / `make service-logs` | disable + remove the unit / follow its journal |
| `make version` | print the version the next build will be stamped with |

## Versioning
`internal/version` holds `Version`, `Commit`, `Branch`, `Date`, stamped by the Makefile with
`-ldflags -X` from git:

```make
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)   # v0.1.0-3-gabc1234-dirty
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null)
BRANCH  := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
```
Unstamped builds (`go build`, `go run`) fall back to `runtime/debug.ReadBuildInfo` (`vcs.revision`,
`vcs.modified`, `vcs.time`); branch is then unknown. The binary target always relinks so a new
commit/tag is picked up even when no `.go` file changed. Releases are tags `vMAJOR.MINOR.PATCH`.
Shown by `sluice version` / `-version`, `:version`, and the first line of `sluice doctor`.

User-facing docs live in `README.md` (install, quick start, keys, config, file locations); keep it in
sync when flags, keys, config keys or paths change.

Typical loop: terminal 1 `make watch`; terminal 2 `make dev` (restart after green).

```mermaid
flowchart LR
  E[edit .go] --> W[make watch: fmt-check vet test build]
  W -->|green| D[make dev: TUI on sandbox]
  D --> P[(plan.json)]
  P -->|A s from make run| V[validated]
  V -->|A r y| R[real]
```

## Packaging
`contrib/arch/PKGBUILD` builds **`sluice-git`** (`x86_64`, `aarch64`) from GitHub `main`
(`git+https://github.com/jfryman/sluice.git#branch=main`), so only pushed commits are packaged.

- `pkgver`: `git describe --long --tags` → `0.1.0.r14.gca17861` (last tag `v0.1.0`, 14 commits
  later, commit `ca17861`). It rises with every commit, so `makepkg -si` always upgrades; **tags are
  optional**, only needed to name a release (`git tag v0.2.0 && git push --tags` → `0.2.0.r0.g…`).
  Tags must only move forward (a lower tag would make pkgver go backwards). The static `pkgver=`
  line in the file is just a placeholder: makepkg rewrites it in place on every build, so building
  inside the repo dirties `contrib/arch/PKGBUILD` — `git checkout contrib/arch/PKGBUILD` (or build
  from a copy). Bump `pkgrel` only for packaging-only changes.
- `build()`: Arch Go guidelines — `GOFLAGS=-buildmode=pie -trimpath -mod=readonly -modcacherw`,
  distro `CGO_*FLAGS`, `-linkmode=external`; the same `-X` version stamps as the Makefile, with
  `Branch=main` (makepkg's checkout is a local `makepkg` branch) and `Date` from
  `SOURCE_DATE_EPOCH` (reproducible).
- `prepare()` first runs `_check_dev_install`: if the **building user** has the dev install —
  `~/.local/bin/sluice` (shadows `/usr/bin` on PATH) or `~/.config/systemd/user/sluice-sweep.service`
  (overrides the packaged unit) — makepkg stops with an `error` listing each and its fix, before
  anything is built or installed. `SLUICE_ALLOW_DEV=1` downgrades it to a warning. Then
  `go mod download` with `GOFLAGS=-modcacherw` (without it the module cache is read-only and
  makepkg can't delete `src/`).
- `check()`: `go test ./...`.
- `package()`: `/usr/bin/sluice`, `/usr/lib/systemd/user/sluice-sweep.service` (the repo unit with
  `ExecStart` rewritten from `%h/.local/bin/sluice` to `/usr/bin/sluice`; a `grep` fails the build if
  that rewrite stops matching), README in `/usr/share/doc/sluice`. `sluice.install` prints the
  per-user steps (`doctor -online`, `systemctl --user enable --now`) and, on upgrade, the restart;
  `pre_install`/`pre_upgrade` scan `/root` and `/home/*` for the dev install and **warn** — pacman
  runs scriptlets as root and ignores their exit status, so only `prepare()` can actually refuse
  (it covers `makepkg -si`; a prebuilt package installed with `pacman -U` only gets the warning).
- Deps: `glibc`; make: `git`, `go>=2:1.27.1`; optional: `isync`, `libnotify`, `keyutils`.
  `license=('MIT')` (repo `LICENSE`), installed to `/usr/share/licenses/sluice-git/LICENSE` as Arch
  requires for MIT.
- makepkg output under `contrib/arch/` is git-ignored.

```sh
cd contrib/arch && makepkg -si        # build + test + install
makepkg -fd                           # build without the pacman dep check (Go from mise, as on the dev box)
```
Verified 2026-10-15 on the aarch64 dev box: `makepkg -fd` built `0.1.0.r14.gca17861`, all tests
passed in `check()`, `sluice version` showed the stamps, the binary is PIE, and `systemd-analyze
--user verify` accepted the unit (apart from `/usr/bin/sluice` not being installed there). The dev
box has the dev install, so `makepkg` refuses there (verified); `SLUICE_ALLOW_DEV=1 makepkg -fdc`
built and cleaned up; the license lands in `/usr/share/licenses/sluice-git/LICENSE`. Building on the
dev box therefore needs `SLUICE_ALLOW_DEV=1`, or build from a copy of the PKGBUILD elsewhere.

## Testing without touching real data
- Everything destructive happens only in `plan.Apply`; unit tests cover it against temp maildirs
  (`internal/plan`, `internal/cleanup`, `internal/sandbox`).
- For manual TUI runs against real mail, pass `-plan <scratch>.json` so the real plan file isn't
  edited; apply with `s` (sandbox) only.
- Sweep: `bin/sluice sweep -sandbox -plan X.json` after copying a message into the sandbox's
  `Mail/+Sluice/cur` (use `find … | xargs grep -il '^List-Id:'`; List-Id header case varies).
- Headless: `bin/sluice -sandbox -plan X.json -apply -yes` applies to the sandbox and prints the report.
- Drive the TUI in tmux (see [tui/summary.md](tui/summary.md#testing)). The first scan of a fresh
  sandbox index takes ~5 s; keys pressed while busy are ignored — wait for "indexed N messages".
- Verification used on 2026-09-24: per-folder file counts of `~/Mail/kolab` + `sha1sum kolab.sieve`
  before/after a real-mode session with a sandbox apply were identical.

## Lessons
- zsh (James's shell): `echo =====` fails (`=word` expansion) and an unmatched glob errors
  (`ls foo*`) — quote them in scripted checks.
- `set -o pipefail` + `ls | head` kills a script with exit 141 (SIGPIPE); use
  `mapfile -t xs < <(… | head)` instead.
- The earlier partial-copy `.dev/` sandbox script was replaced by the built-in full sandbox: a reflink
  clone of everything is faster to build than copying 300 messages per folder.
