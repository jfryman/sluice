# Roadmap

## Built
Index, candidates, search, plan-first staging, full reflink sandbox, apply (sandbox/real, TUI and
headless), trash + undo — implemented and tested (`go test ./...`, tmux-driven sandbox runs).
Not yet exercised against the live server: a real apply (sieve-connect publish + real trash batch).

## Later
- SaneBox migration: bulk-propose rules for everything in `+SaneBlackHole`.
- Rule hit stats: count how many new messages each managed rule caught (index `Deleted Messages` by rule).
- `List-Unsubscribe` one-click (RFC 8058) for candidates instead of/in addition to trashing.
- Pattern rules (subject contains, header regex) beyond exact list/domain/sender.
- Browse the sandbox from real mode without a second process (swap Deps in-app).
- Plan export/import to named files beyond `-plan PATH`.
- notmuch integration as an alternative index backend.
- Use KolabNow's spam verdict (`***SPAM***` subject prefix / X-Spam headers) as a score signal and a
  one-key "trash everything the server already flagged" search preset.
- Collapse Emarsys/Mailchimp-style list ids (`<hash>.<n>.list-id.mcsv.net`) to their provider when
  a sender rotates list ids.

## Open issues found during discovery
- `kolab.sieve` files into `Newsletters`, but no such folder exists locally — either the folder is
  missing server-side (rule silently failing) or it's excluded from sync. Needs James to confirm.
