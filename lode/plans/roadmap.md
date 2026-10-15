# Roadmap

## Built
Index, candidates, SaneBox tab, search (`name:`, `t` rules from a query), plan-first staging, full
reflink sandbox, apply (sandbox/real, TUI and headless), trash + undo, sweep service — all in daily
use against KolabNow since 2026-10-14 (real applies of thousands of messages; `kolab` is the active
script; `sluice-sweep.service` runs on the desktop).

## Next
- **Server-side trash moves** — [imap-move.md](imap-move.md): stop mbsync re-uploading every trashed
  message. The server probe passed (MOVE + UIDPLUS, far-UID mapping verified); next is
  `maildir.FarUIDs`.

## Later
- SaneBox migration: bulk-propose rules for everything in `+SaneBlackHole`.
- Rule hit stats: count how many new messages each managed rule caught (index `Deleted Messages` by rule).
- `List-Unsubscribe` one-click (RFC 8058) for candidates instead of/in addition to trashing.
- Pattern rules (subject contains, header regex) beyond exact list/domain/sender.
- Browse the sandbox from real mode without a second process (swap Deps in-app).
- Plan export/import to named files beyond `-plan PATH`.
- `sluice pull`: adopt the server script as local after drift (with backup).
- Warn when a hand-written rule outside the managed block mentions the same sender as a managed rule.
- notmuch integration as an alternative index backend.
- Use KolabNow's spam verdict (`***SPAM***` subject prefix / X-Spam headers) as a score signal and a
  one-key "trash everything the server already flagged" search preset.
- Collapse Emarsys/Mailchimp-style list ids (`<hash>.<n>.list-id.mcsv.net`) to their provider when
  a sender rotates list ids.

## Open issues found during discovery
- `kolab.sieve` files into `Newsletters`, but no such folder exists locally — either the folder is
  missing server-side (rule silently failing) or it's excluded from sync. Needs James to confirm
  (raised twice, unanswered; substack/nytimes/mcsv mail still lands in Archive and +SaneLater).
