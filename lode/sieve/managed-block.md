# Managed Sieve block

Package: `internal/sieve`. File: config `sieve_file` (default `~/.config/sieve/kolab.sieve`).
Related: [remote.md](remote.md), [../mail/index.md](../mail/index.md).

## Contract
- Sluice owns only the text between the markers; everything else is preserved byte-for-byte.
- If no block exists it is inserted **immediately after the first `require` statement**, so managed
  trash rules run before hand-written rules (e.g. the existing Newsletters rule).
- The `require [...]` list is merged to include `fileinto` and `imap4flags` if missing.
- Each rule is preceded by a metadata comment; parsing reads only these comments (the Sieve
  text is regenerated from them), so the block round-trips deterministically.
- KolabNow Sieve cannot create folders (no `mailbox` extension): `file` targets are picked from
  existing maildir folders only.

## Rendered example
```sieve
require ["fileinto", "imap4flags"];

# >>> sluice managed block — edit via sluice >>>
# sluice: {"kind":"list","value":"news.example.com","action":"trash","added":"2026-09-24"}
if header :contains "List-Id" "news.example.com" {
    addflag "\\Seen";
    fileinto "Deleted Messages";
    stop;
}
# sluice: {"kind":"domain","value":"promo.shop.com","action":"file","folder":"+SaneNews","added":"2026-09-24"}
if address :domain :is "from" "promo.shop.com" {
    fileinto "+SaneNews";
    stop;
}
# <<< sluice managed block <<<
```

## Match kinds → tests
| kind   | Sieve test                                   |
|--------|----------------------------------------------|
| list   | `header :contains "List-Id" "<value>"`       |
| domain | `address :domain :is "from" "<value>"`       |
| sender | `address :all :is "from" "<value>"`          |

## Actions
| action | body                                                        |
|--------|-------------------------------------------------------------|
| trash  | `addflag "\\Seen"; fileinto "Deleted Messages"; stop;`      |
| file   | `fileinto "<folder>"; stop;`                                |
| read   | `addflag "\\Seen";` (no stop: continues to later rules/INBOX) |

Strings are escaped per RFC 5228 (`\` → `\\`, `"` → `\"`). Duplicate (kind,value) rules are replaced
(case-insensitive). The `require` regex is anchored to line start so prose in comments is ignored.
Rendering is idempotent: `Parse(Render(s)).Render() == Render(s)` (tested).

```mermaid
flowchart LR
  F[kolab.sieve text] --> S[Split: before / block / after]
  S --> P[Parse metadata comments -> []Rule]
  P --> E[add / remove rules]
  E --> R[Render block] --> J[Join + merge require] --> O[new text]
```
