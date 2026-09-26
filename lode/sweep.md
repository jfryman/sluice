# Sweep: the training folder (`+Sluice`) and `sluice sweep`

Package: `internal/sweep` (`sweep.go` one pass, `watch.go` service loop). CLI: `sluice sweep
[-watch] [-sandbox] [-plan PATH]`; TUI `:sweep`; unit `contrib/systemd/sluice-sweep.service`.
Related: [apply/plan.md](apply/plan.md), [sieve/remote.md](sieve/remote.md),
[mail/index.md](mail/index.md), [doctor.md](doctor.md).

## Why
The TUI is a review tool; day to day James triages where he reads mail (phone, webmail, aerc).
Dragging an unwanted message into one IMAP folder should (1) stop future mail like it and
(2) queue cleanup of existing mail like it. Server-side folder triggers need the **imapsieve**
extension; KolabNow's Cyrus doesn't advertise it, so a sluice service watches the synced maildir.

## Decisions
| question | decision |
|----------|----------|
| automation | **rule auto-published**; retro cleanup **queued into the plan** (reviewed/validated as usual) |
| trigger | **long-running service** `sluice sweep -watch` (systemd user unit shipped in `contrib/`), not a hook in `mail-sync` — the dev machine isn't where it will live |
| folders | **one** folder, config `training_folder` (default `+Sluice`); smart match |
| smart match | `List-Id` if present → `list` rule; else exact From address → `sender` rule. **Never domain** automatically |

```mermaid
sequenceDiagram
  participant U as James (any client)
  participant S as server
  participant M as mbsync (timer / mail-sync)
  participant W as sluice sweep -watch
  U->>S: move message into +Sluice
  M->>S: sync downloads it into maildir +Sluice
  W->>W: inotify on +Sluice/{new,cur} (+ periodic rescan), debounce, take mbsync.lock
  W->>W: derive match; guards
  alt passes guards
    W->>S: publish: base sieve + new rule (only), activate
    W->>W: plan: add trash item "sweep: list X" (existing matches)
  else held
    W->>W: plan: add pending rule op + trash item, marked held
  end
  W->>W: move training message to trash (journaled)
  M-->>S: next sync propagates moves
```

## Outcomes per dropped message (`sweep.jsonl`)
| result | when | rule | cleanup | drop moved to trash |
|--------|------|------|---------|---------------------|
| `published` | new match, guards pass, publish OK | published now (`"source":"sweep"`) | queued plan item `sweep: <kind> <value>` | yes |
| `ruled` | a managed rule for that (kind, value) already exists | — | queued | yes |
| `held` | From is a correspondent or own address (`index.Known`) | pending `add` op in the plan | queued, label `sweep (held: <why>): …` | yes |
| `error` | publish failed (drift, other active script, auth…) | — | — | **no**, retried next sweep |
| `skipped` | no List-Id and no From | — | — | **no**; logged once per key |

Correspondents/identities come from the index's `sent` table: To/Cc/Bcc and From of messages in
`sent_folders` (default `Sent Messages`, `Sent Items`). See [mail/index.md](mail/index.md).

## Behaviour details
- Sweep publishes **only its own new rules** on top of the current sieve file; pending plan rule
  ops stay pending. A pending op for the same (kind, value) is dropped (the sweep's add wins).
- Publishing uses the normal path (drift check, active-script guard, checkscript). On failure the
  training message **stays in `+Sluice`** and is retried next sweep; the error is recorded.
- Several drops of the same sender/list in one sweep → one rule, one trash item.
- Training messages are moved to the trash folder (journaled like any trash batch), so an empty
  `+Sluice` means "processed".
- Credentials: publishing runs `pass_cmd` — the same keyring-cached `mail-pass` mbsync already
  uses every sync, so no extra prompts in practice. Nothing to publish → no credential call.
- State: `$XDG_STATE_HOME/sluice/sweep.jsonl` records each processed message (key, derived rule,
  outcome: published / held / error). TUI Plan tab labels sweep items; `:sweep` runs one manually.
- One-shot `sluice sweep` (same logic, exits; code 0 done/nothing, 3 mail changed, 1 error) for
  manual runs and tests; `--watch` loops on inotify events with a debounce and a periodic rescan
  (`sweep_interval`, default 5m) as a fallback.
- `post_sweep_cmd` (default `["mail-sync"]`) runs after a sweep that moved local mail (locks already
  released) so the server — and the phone's view of `+Sluice` — catch up immediately instead of on the
  sync timer. The follow-up sync's writes may re-trigger fsnotify; that sweep finds nothing and
  doesn't run the command again, so there's no loop. Disabled for `-sandbox`. Doctor warns if the
  command isn't on PATH. It's the only coupling to the sync setup.

## Setup
- `sweep -watch` creates the local maildir `+Sluice` if missing; mbsync `Create Both`/`Far` creates
  it on the server. Config: `training_folder` (default `+Sluice`), `sent_folders`, `sweep_interval`
  (default `5m`), `post_sweep_cmd` (default empty).
- Doctor's **sweep** capability: training folder, mbsync create, sluice on PATH, sweep service
  (`systemctl --user show` LoadState/ActiveState), last sweep outcome.
- `contrib/systemd/sluice-sweep.service` (user unit, `ExecStart=%h/.local/bin/sluice sweep -watch`,
  `Restart=on-failure`); `make install-service` copies + enables it on the machine it's run on.
  Nothing edits `mail-sync` or other dotfiles.

## Concurrency
- `sweep.lock` (state dir) serialises the service, `:sweep` and one-shot runs.
- Publishing takes `<sieve_file>.lock`, shared with TUI/headless applies.
- Moving drops uses the cleanup journal under `mbsync.lock` (so `U` can undo a sweep's moves — which
  puts the drops back in `+Sluice`, where the next sweep processes them again).
- Moves in `+Sluice/{new,cur}` trigger fsnotify again; the follow-up sweep finds nothing and returns
  before scanning.

## Testing
`sweep_test.go`: temp maildir + `FilePublisher`; published/grouped, held (correspondent and own
address), skipped logged once, failed publish leaves drops, existing rule, and `Watch` reacting to a
drop. Verified 2026-09-25 on a full sandbox clone (`sluice sweep -sandbox -plan <scratch>`): list and
sender drops published to the sandbox remote, cleanup queued, real maildir + sieve untouched.

## Later
- Undo from the phone (`+Sluice/Undo`); desktop notification for held items.
