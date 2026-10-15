# Server-side trash moves (IMAP MOVE)

Packages: `internal/imap` (client + `Remote`, `imaptest` fake server), `internal/cleanup`
(`server.go`: split + journal + undo), `internal/maildir` (`FarUIDs`, `NearUID`), `internal/creds`.
Related: [maildir-and-mbsync.md](maildir-and-mbsync.md), [../cleanup/search-and-trash.md](../cleanup/search-and-trash.md),
[../apply/plan.md](../apply/plan.md), [../sweep.md](../sweep.md), [../doctor.md](../doctor.md).

## Why
mbsync has no server-side move: a message moved locally into `Deleted Messages` is **uploaded**
(APPEND) and expunged from its source. Two real applies of 3,947 messages (2026-10-14) kept mbsync
busy 10+ minutes at ~16 msgs/s, holding `mbsync.lock` and blocking the next validate. Moving on the
server instead takes about a second per apply; mbsync then only **downloads** the trash copies.

## Decisions
| | decision |
|---|---|
| local trash copy | keep syncing `Deleted Messages` (mbsync downloads each moved message once). Excluding it from sync would remove even that, at the cost of local trash + local undo — not done |
| client | small native IMAP client (implicit TLS 993, AUTHENTICATE PLAIN with SASL-IR, CAPABILITY, SELECT/EXAMINE, UID MOVE, UID COPY + STORE + UID EXPUNGE fallback, LOGOUT), traced like ManageSieve |
| scope | real trash only: `plan.Apply` (real target), sweep **cleanup**, `U` undo. Sandbox: file moves. Sweep **drops**: file moves (see sweep.md) |
| capability | requires `UIDPLUS` (COPYUID tells exactly what moved and enables undo); `MOVE` preferred, else COPY + `\Deleted` + `UID EXPUNGE` of only the copied UIDs. Without UIDPLUS: `ErrUnsupported` → file moves |
| switch | `server_moves = true` (default); `imap_server`, `imap_port` (993); `sync_cmd` (default `["mail-sync"]`) |

## The far-UID mapping
Local file names carry mbsync's **near** UID (`…,U=10:2,S`). The server UID comes from the folder's
`.mbsyncstate`: header lines (`FarUidValidity 1467142788`, `NearUidValidity …`), a blank line, then
`far near flags` pairs. Near and far differ wildly (+SaneLater near 579 → far 266157).

```go
validity, nearToFar, err := maildir.FarUIDs(root, "Archive") // ErrStatePending if .mbsyncstate.journal exists
far := nearToFar[maildir.NearUID(filepath.Base(path))]       // 0/missing → never synced → file move
```

A folder falls back to file moves when: its state has a pending journal (interrupted/running sync),
its state is unreadable, or the server's UIDVALIDITY ≠ `FarUidValidity` (`Result.Stale`). The state is
read under `mbsync.lock` (the cleaner already holds it for the batch).

## Flow
```mermaid
sequenceDiagram
  participant C as cleanup.Cleaner.Trash
  participant S as .mbsyncstate
  participant I as imap.Remote (one session)
  participant J as journal
  participant F as file moves
  participant M as sync_cmd (mail-sync)
  C->>C: lock mbsync.lock
  C->>S: FarUIDs per source folder; split server / local
  C->>I: LOGIN, CAPABILITY (needs UIDPLUS)
  loop each folder (chunks of 500)
    C->>I: SELECT folder (UIDVALIDITY check) · UID MOVE set "Deleted Messages"
    I-->>C: COPYUID <trash validity> <src> <dst>
    C->>J: {mode:"server", far, dst, dstValid} per confirmed message
  end
  C->>F: the rest: rename into Deleted Messages/cur (journaled)
  C->>C: unlock
  Note over M: TUI / headless apply then run sync_cmd and rescan
```

- **Local files of server-moved messages are not touched.** The next sync expunges them from the
  source (`Expunge Both`) and downloads the trash copy. Moving them locally too would make mbsync
  upload duplicates.
- Until that sync, the index still shows them in the source folder; the report says
  "N of them moved on the server; local copies update after the next sync".
- Requested UIDs absent from COPYUID weren't on the server (already moved): reported as skipped.
- Login/capability failure aborts the trash step **before any move** (local moves included); a failure
  mid-way journals what COPYUID confirmed and reports the rest as skipped.

## Undo
`Cleaner.Undo` sends the batch's server entries back in one session: grouped by
(trash folder, `dstValid`, original folder), `UID MOVE <dst uids>` from the trash to the original
folder; a changed trash UIDVALIDITY skips them with a reason. File entries are undone as before. A
sync follows (TUI), restoring the local copies.

## Badge / progress
Apply: `server: connect · tls · auth` (via `tracedMover`), then `moving on server N/M (Folder)` from
`MoveAll`; after the report, `syncing mail (mail-sync)` then a rescan. `plan.Sync` treats exit 75
(mail-sync's `flock -E 75 -n` found mbsync running) as "already running", not an error.

## Live verification (2026-10-14)
- Probe: KolabNow (Cyrus) advertises `MOVE UIDPLUS` **only after login**; folder UIDVALIDITY matched
  `FarUidValidity` for Deleted Messages, Archive, +SaneLater; 10/10 sampled files mapped to the same
  Message-ID on the server (`UID FETCH … BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)]`, read-only).
- `FarUIDs` on the real maildir: Archive 85,729/85,729 files mapped, +SaneLater 2,992/2,992;
  Deleted Messages had 742 unsynced files (no pair → would be file-moved).
- Round trip with 2 `+SaneBlackHole` messages: trash on the server in 1.3s total
  (`connect 0.14s · tls 0.29s · auth 0.13s · select 0.20s · move 0.16s`), COPYUID 394936/394937,
  Message-IDs confirmed in Deleted Messages and gone from the source.

## Testing
`imap/remote_test.go` (MOVE and COPY fallback, stale validity, no-UIDPLUS refusal, probe, bad login,
password never in recorded commands), `maildir/mbsyncstate_test.go`, `cleanup/server_test.go` (mixed
server/local batch, pending-journal fallback, undo, stale, login failure aborts everything),
`plan` (`TestApplyServerMoves`, `TestSync`), `sweep` (`TestCleanupOnServer`), doctor (`TestOnlineIMAP`).
All against `imaptest.Server` (in-process TLS IMAP with UID-only mailboxes).

## Later
- Exclude `Deleted Messages` from sync (no download at all); undo then only server-side.
- Non-ASCII folder names need modified UTF-7 in `imap.quote` (refused today → file moves).
