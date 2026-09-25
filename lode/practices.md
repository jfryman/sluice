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

## Secrets
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

## Lode
- Lode describes current state; changelog-ish notes go to `lode/tmp/`.
