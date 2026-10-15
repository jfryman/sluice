# Plan: server-side trash moves (IMAP MOVE)

Status: **designed, probe passed, not built** (decisions agreed and server probed 2026-10-14). Related:
[../mail/maildir-and-mbsync.md](../mail/maildir-and-mbsync.md), [../apply/plan.md](../apply/plan.md),
[../cleanup/search-and-trash.md](../cleanup/search-and-trash.md), [../sieve/remote.md](../sieve/remote.md),
[../sweep.md](../sweep.md).

## Problem
A local move into `Deleted Messages` costs a full **upload**: mbsync has no server-side move, so it
APPENDs every moved message to the server's trash and expunges it from the source. On 2026-10-14 two
real applies (3,947 messages) kept mbsync busy 10+ minutes at ~16 msgs/s, holding `mbsync.lock`
throughout — blocking the next validate (see the lock-wait behaviour in maildir-and-mbsync.md).

## Decisions
| | decision | why |
|---|---|---|
| A. local trash copy | keep syncing `Deleted Messages`: mbsync **downloads** each moved message once | least surprising; dropping trash from sync (no transfer at all, server-only undo) can come later |
| B. IMAP client | small native client in a new `internal/imap` package, modeled on `sieve/managesieve.go` (TLS dial, LOGIN, CAPABILITY, SELECT, UID MOVE / fallback, LOGOUT) with `timing.Recorder` tracing | the subset is tiny; matches the ManageSieve client, its credentials and its badge steps |
| C. scope | every **real** trash path: `plan.Apply` (real), sweep cleanup + drops, `:cleanup`'s eventual apply. Sandbox keeps local moves | one code path; the sandbox can't rehearse a server anyway |
| D. capability | require `MOVE` (RFC 6851) + `UIDPLUS` (COPYUID); else fall back to `UID COPY` + `UID STORE +FLAGS (\Deleted)` + `UID EXPUNGE`; `doctor -online` reports which | **confirmed**: KolabNow advertises both (see Probe results); the fallback stays for other servers |

## The key fact: local `U=` is not the server UID
mbsync's maildir names carry the **near** (local) UID: `…,U=10:2,S`. The server (far) UID comes from
the folder's `.mbsyncstate`, whose lines pair them after a blank line:

```
FarUidValidity 1467142788      # must equal the server's UIDVALIDITY on SELECT
NearUidValidity 1788208369     # = <folder>/.uidvalidity
...
5 4 S                          # far UID 5  <->  near U=4, flags
```

Rules:
- Read the state **under `mbsync.lock`** (mbsync rewrites it at the end of a run).
- If `<folder>/.mbsyncstate.journal` exists, the state isn't compacted (interrupted run): that
  folder falls back to local moves.
- If SELECT's UIDVALIDITY ≠ `FarUidValidity`, the folder falls back (mapping is stale).
- A file with no `U=`, or no pair in the state, was never synced: local move.

```go
// internal/maildir (new): far UIDs for near UIDs in one folder.
func FarUIDs(root, folder string) (validity uint32, nearToFar map[uint32]uint32, err error)
```

## Flow (real apply)
```mermaid
sequenceDiagram
  participant A as plan.Apply (real)
  participant L as mbsync.lock
  participant S as maildir state
  participant I as IMAP (imap.kolabnow.com:993)
  participant J as journal
  participant M as mail-sync (after)
  A->>L: LockWith (activity-aware)
  A->>S: per source folder: FarUIDs; split refs into server / local
  A->>I: LOGIN (user + pass_cmd, as sieve.Remote)
  loop each source folder
    A->>I: SELECT folder (check UIDVALIDITY)
    A->>I: UID MOVE <far uids> "Deleted Messages"  (chunks of ~500)
    I-->>A: COPYUID <trash validity> <src uids> <dst uids>
    A->>J: entry per message {batch, key, from, to, far, dstFar, mode:"server"}
  end
  A->>A: local-mode refs: today's file move (journaled)
  A->>L: unlock
  A->>M: run post_sweep_cmd-style sync so the server change lands locally now
```

- **Local files are not touched** for server-moved messages. mbsync then sees them gone from the
  source (expunges locally, `Expunge Both`) and new in `Deleted Messages` (downloads). Moving them
  locally too would make mbsync upload duplicates; editing mbsync's state to avoid that is too
  fragile.
- Progress steps for the badge: `server: connect/auth`, `moving on server N/M (Archive)`,
  `syncing (mbsync)`.
- The index keeps the messages in their old folder until the follow-up scan; the apply report says
  "moved on the server; local copies update after the sync".

## Undo (`U`)
Journal entries with `mode:"server"` carry the destination far UID from COPYUID. Undo SELECTs
`Deleted Messages`, checks its UIDVALIDITY against the recorded one, and `UID MOVE`s them back to
their source folders; then syncs. Entries without a COPYUID mapping (server lacked UIDPLUS) can't be
undone server-side: the report says so (they can be found in Deleted Messages by hand).

```go
type Entry struct {
	Batch, Key, From, To, Undo string // as today
	Mode     string `json:"mode,omitempty"`      // "" (local file move) | "server"
	FarUID   uint32 `json:"far,omitempty"`       // in From, before the move
	DstUID   uint32 `json:"dst,omitempty"`       // in To, from COPYUID
	DstValid uint32 `json:"dstValid,omitempty"`  // To's UIDVALIDITY at move time
}
```

## Interfaces
`plan.Target` gains a mover; the cleaner keeps local moves:
```go
type Mover interface { // implemented by imap.Remote (real); nil for the sandbox
	Move(byFolder map[string][]uint32, dst string, progress func(string)) (MoveResult, error)
}
```
`cleanup.Cleaner.Trash` takes the split: server refs → Mover, the rest → file moves, one journal
batch for both so `U` undoes the whole apply.

## Failure handling
- Login/capability failure before any MOVE: abort the apply's trash step with the error (rules may
  already be published — same as a local-move failure today); nothing moved.
- A MOVE chunk fails mid-way: journal what COPYUID confirmed, report the rest as skipped; the next
  apply of the same plan item finds them still in place.
- Connection drop: MOVE is atomic per command; re-run is safe (already-moved UIDs are gone from the
  source and are skipped as "not found").

## Probe results (2026-10-14, read-only)
- Pre-login CAPABILITY is minimal (`IMAP4REV1 LITERAL+ ID ENABLE … AUTH=PLAIN AUTH=LOGIN SASL-IR`);
  **ask again after LOGIN**: `MOVE UIDPLUS CONDSTORE QRESYNC ESEARCH SPECIAL-USE IDLE COMPRESS=DEFLATE …`.
  Login works with `user` + `pass_cmd` (`mail-pass`), same as ManageSieve.
- Server UIDVALIDITY equals mbsync's `FarUidValidity` for `Deleted Messages` (1508512700), `Archive`
  (1467142788) and `+SaneLater` (1615864900).
- Mapping verified: 10 random local files (5 Archive, 5 +SaneLater) → near `U=` → far UID via
  `.mbsyncstate` → `UID FETCH BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)]` returned the same Message-ID
  10/10. Far UIDs differ a lot from near ones (e.g. +SaneLater near 579 → far 266157), so using
  `U=` directly would hit the wrong messages.
- No `.mbsyncstate.journal` left behind after a completed sync.

```python
M.select('"Archive"', readonly=True)   # EXAMINE: read-only
M.uid("FETCH", far, "(BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)])")  # PEEK: doesn't set \Seen
```

## Build order
1. ~~Probe~~ done by hand (above). Still build the same check into `sluice doctor -online` (post-login
   CAPABILITY, UIDVALIDITY vs `FarUidValidity` per trash/source folder) as part of step 3.
2. `maildir.FarUIDs` + tests against fixture `.mbsyncstate` files (incl. journal present, validity
   mismatch).
3. `internal/imap` client + a fake server test (like the ManageSieve tests).
4. Cleaner split + journal fields + undo; sandbox unaffected.
5. Wire into `plan.Apply` (real), sweep, TUI progress; post-apply sync.
6. First live use: a 2–3 message trash item, confirm on the server and after the sync, `U` it back.

## Open questions
- Should a real apply wait for the follow-up sync (badge `syncing`) or return and let the timer /
  sweep service pick it up? Lean: run it, but don't block the TUI's report on it.
- Option A(b) later: excluding `Deleted Messages` from sync removes the download too.
