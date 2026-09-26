# Roadmap

## Built
Index, candidates, search, plan-first staging, full reflink sandbox, apply (sandbox/real, TUI and
headless), trash + undo — implemented and tested (`go test ./...`, tmux-driven sandbox runs).
Not yet exercised against the live server: a real plan apply to real (trash batch).
The sweep service **has** published to KolabNow (2026-09-25, from a phone drop): `kolab` is active.
Server state (2026-09-24): KolabNow has **no Sieve scripts**, so the first real apply creates and
activates `kolab` from the local file plus the plan (the hand-written `Newsletters` rule goes live
too). First real apply checklist: `sluice doctor -online` shows no ✗ (expect a 1Password prompt from
`mail-pass`) → plan one rule + a small trash batch → `A`→`s` validate → `A`→`r`→`y` → after the
next `mail-sync`, confirm the server's trash and active script.

Sweep (training folder service) is built and sandbox-verified; not yet installed on the desktop
(`make install-service` there, then create `+Sluice` via the doctor fix or let `sweep -watch` do it).

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
