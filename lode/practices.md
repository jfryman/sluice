# Practices

## Safety first (mail is irreplaceable)
- Plan-first: the TUI only edits the plan. Mail moves and Sieve publishes happen solely in
  `plan.Apply`, behind an explicit apply dialog (real needs two confirmations). See [apply/plan.md](apply/plan.md).
- Validate in a fresh sandbox (`A` → `s`) before applying to real.
- Never unlink a message file. "Delete" = move to `Deleted Messages`. Server-side expiry is KolabNow's job.
- Every destructive step shows a count/diff and needs an explicit confirm key (`y`).
- All maildir moves happen while holding `$XDG_RUNTIME_DIR/mbsync.lock` (same flock as `mail-sync`),
  so mbsync never observes a half-moved batch. See [mail/maildir-and-mbsync.md](mail/maildir-and-mbsync.md).
- Sieve uploads: drift check → server `--checkscript` → upload → activate. Local file is backed up to
  `kolab.sieve.bak` before rewrite. See [sieve/remote.md](sieve/remote.md).
- Sluice only edits text between its markers; hand-written rules are preserved byte-for-byte.

## Public repo: sanitize what gets committed
`github.com/jfryman/sluice` is **public**. Everything committed (code, tests, README, lode) is
published. Credentials are never involved (see Secrets), but mailbox-derived data is personal.
- In new docs, tests, fixtures and examples use made-up data: `news.example.com`,
  `promo.shop.example`, `<hash>.list-id.example`, generic subjects, `k1:2,S`-style filenames.
- Don't paste real sender domains, List-Ids, subject lines, addresses, maildir filenames (they embed
  the hostname), message counts or screen captures of James's mailbox into committed files. Real
  observations go in `lode/tmp/` (git-ignored) or get paraphrased ("a newsletter platform sending
  ~1.2k msgs / 90d").
- Before committing, scan the diff for real-mail specifics.
- Existing examples already pushed (README screenshot, a few lode lessons) were accepted by James
  on 2026-09-24; don't add more, and prefer sanitized versions whenever those sections are edited.

```sh
# pre-commit scan: flag anything that looks like it came from the real mailbox
git diff --cached | grep -nE '^\+' | grep -viE 'example\.(com|org)|\.example\b|github\.com/jfryman' \
  | grep -nE '[a-z0-9-]+\.(com|net|org|io|me|co)\b|@[a-z0-9.-]+\.[a-z]{2,}'
```

## Secrets
- **Never run `sieve-connect --debug`** (or any protocol trace) where output is captured: the
  AUTHENTICATE line is `base64(user\0user\0password)` — a plaintext credential. This leaked the
  KolabNow password into a session transcript on 2026-09-24 (filtering the output did not catch it).
  To see what the server has, use `sluice doctor -online` or plain `--list`, which never echo auth.
- Never read, store or log credentials. Shell out to configurable commands (`mail-user`, `mail-pass`)
  and hand the password to `sieve-connect` on fd 3 via a pipe (`--passwordfd 3`), newline-terminated.

```go
r, w, _ := os.Pipe()
cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
go func() { w.Write(append(pass, '\n')); w.Close() }()
```

## Go style
- Packages under `internal/`, one responsibility each; TUI depends on everything, nothing depends on TUI.
- Pure logic (query parsing, sieve render/parse, filename munging, scoring) is unit-tested with table tests.
- Long work (index scan, sieve-connect, moves) runs as `tea.Cmd` returning a typed msg; never block `Update`.
- Pure-Go SQLite (`modernc.org/sqlite`) — no cgo.

## Requirements live in doctor
- Any new external dependency, config key with preconditions, or filesystem assumption gets a check
  in `internal/doctor` (with a concrete fix string and the capabilities it gates) and a line in the
  README requirements. See [doctor.md](doctor.md).

## Iteration
- Exercise destructive features in the sandbox (`make dev`), never against `~/Mail/kolab`; for real-mode
  UI testing use `-plan <scratch>` so James's plan isn't touched.
  See [development.md](development.md).

## Working with James
- Discuss design first (he'll pick between concrete options), then execute end to end without
  hand-holding; record decisions in the lode before implementing.
- Vim user: new TUI interactions should follow vim conventions (see [tui/keys.md](tui/keys.md)).
- Values safety (plan-first, sandbox, doctor) but not ceremony: keep confirmations to real applies.
- Commits: branch off `main` for work, fast-forward merge when he asks.

## Lode
- Lode describes current state; changelog-ish notes go to `lode/tmp/`.
