# Full sandbox

Package: `internal/sandbox`. Related: [plan.md](plan.md), [../development.md](../development.md).

A complete, isolated copy of the mail environment, cheap because `~/Mail` and `~/.local/share` are on
the same btrfs filesystem: `cp -a --reflink=auto` clones ~105k messages / 6 GB in ~3 s and 0 bytes of
exclusive space (measured 2026-09-24). Writes in the clone copy-on-write; the real files are never
modified.

## Layout (`$XDG_DATA_HOME/sluice/sandbox`, config `sandbox_dir`)
```
sandbox/
  Mail/            reflink clone of mail_root
  kolab.sieve      copy of sieve_file (the sandbox's "local" script)
  remote/kolab     the sandbox's "server" script (file publisher reads/writes it)
  cache/index.db   sandbox index
  state/journal.jsonl  sandbox trash journal (U works inside the sandbox)
  mbsync.lock      sandbox lock (nothing else uses it)
  created          timestamp of the last refresh
```

```go
sb := sandbox.New(cfg)          // Dir from cfg.SandboxDir
sbCfg := sb.Config(cfg)         // same config, every path redirected into Dir
err := sb.Refresh(cfg)          // re-clone Mail + sieve under the REAL mbsync lock
pub := sieve.FilePublisher{Dir: sb.RemoteDir()} // no network, no credentials
```

## Refresh
```mermaid
sequenceDiagram
  participant S as sandbox.Refresh
  participant L as real mbsync.lock
  S->>S: safety checks (Dir not inside/containing mail_root)
  S->>S: rm -rf Dir/Mail.old; mv Dir/Mail Dir/Mail.old
  S->>L: flock (consistent snapshot; mbsync can't rename mid-clone)
  S->>S: cp -a --reflink=auto mail_root Dir/Mail
  S->>L: unlock
  S->>S: copy sieve_file → kolab.sieve and remote/kolab; rm Dir/Mail.old, index, journal
```
Refresh happens on first `-sandbox` launch, on `-reset-sandbox`, and **every time a plan is applied
to the sandbox from real mode**, so validation always runs against current mail.

## Invariants
- Refresh refuses unless `Dir` is absolute, ends in a path element named `sandbox`, and neither
  contains nor is contained by `mail_root`. Deletions only ever target `Dir/Mail*`, `Dir/cache`,
  `Dir/state`.
- The sandbox index and journal are separate from the real ones; the **plan file is shared**.
- Without reflink support `--reflink=auto` silently does a full copy (6 GB); acceptable fallback.
- Sandbox mode shows a `SANDBOX` badge; its apply only ever targets the sandbox.
