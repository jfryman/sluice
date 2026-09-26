# Sieve upload (native ManageSieve)

Package: `internal/sieve` (`remote.go`). Related: [managed-block.md](managed-block.md),
[../practices.md](../practices.md).

## Publisher interface
```go
type Publisher interface {
  Publish(localPath, newText string) (log []string, err error)
  Describe() string
}
```
`Remote` (below) is the real server. `FilePublisher{Dir, Script}` is the sandbox stand-in: same drift
check (remote file vs local file), validates by `sieve.Parse`, writes `local.bak`, local, then the
remote file. Only `plan.Apply` calls `Publish`. See [../apply/plan.md](../apply/plan.md).

## Native ManageSieve client (`managesieve.go`)
sluice speaks ManageSieve (RFC 5804) itself; `sieve-connect` is no longer used. **One session per
operation** — connect → greeting → STARTTLS → greeting → `AUTHENTICATE "PLAIN"` → commands → LOGOUT —
so a publish is one TLS handshake and one login (it used to be five sieve-connect runs).

```go
c, err := sieve.Dial(ctx, "imap.kolabnow.com:4190", tlsCfg) // reads caps, STARTTLS, re-reads caps
c.AuthPlain(user, pass)                                     // base64("\0"+user+"\0"+pass)
scripts, active, _ := c.List()                              // LISTSCRIPTS
text, _ := c.Get("kolab")                                   // GETSCRIPT → literal
c.Check(text); c.Put("kolab", text); c.SetActive("kolab")   // {n+} non-synchronising literals
```
- Responses are parsed as ManageSieve tokens (atoms, quoted strings, `{n}` literals); `NO`/`BYE`
  carry an optional response code and message, surfaced in errors.
- `CHECKSCRIPT` is only sent when the server advertises `VERSION`; otherwise `PUTSCRIPT` validates.
- **Never logs protocol traffic.** Timings only (stage names + durations) — see Timing below.
- Config: `sieve_server`, `sieve_port` (default 4190). `Remote.TLS` overrides the TLS config (tests).

## Credentials
- Username: config `user` if set (no command run — preferred: a username isn't secret and `op read`
  may need a 1Password authorisation per caller); else output of `user_cmd` (default `mail-user`).
- Password: output of `pass_cmd` (default `mail-pass`: 1Password, cached 8h in the kernel keyring).
  Held as `[]byte`, wiped after the session. Never stored, never on argv.

## Timing
`internal/timing.Recorder` collects `(stage, duration)`; `Remote.Trace` records `user`, `pass`,
`connect`, `starttls`, `auth`, and each command. `sluice sweep` logs one line per sweep:
`timing: scan 0.5s · user 0.0s · pass 0.0s · connect 0.2s · starttls 0.1s · auth 0.3s · list … · total …`.
`doctor -online` includes the server round-trip time in its detail.

## Server model
ManageSieve has no partial edits: a publish uploads the **whole** script (hand-written text preserved
byte-for-byte, managed block regenerated) and activates it. Exactly one script can be active.
`LISTSCRIPTS` returns one line per script: `"name"` or `"name" ACTIVE`; just `OK` when there are none.

```go
type ServerStatus struct {
  Scripts []string
  Active  string // name of the active script, "" if none
  Exists  bool   // sieve_script is on the server
  Drift   bool   // exists and differs from the local file
}
type PublishOptions struct{ AllowSwitchFrom string } // active script the user agreed to replace
```

| server state | Status / doctor `-online` | Publish |
|---|---|---|
| ours exists, active, matches local | ✓ | normal |
| ours exists, differs from local | ! drift | stop (`ErrDrift`) |
| no scripts / ours missing, nothing active | ! "first apply will create + activate" | **first publish**: no drift check; upload local+plan, activate |
| ours missing, **another script active** | ✗ "set `sieve_script` to it or deactivate it" | stop (`ErrOtherActive`), never switch it off |
| ours exists, not active, nothing active | ! not active | activates ours |
| ours exists, **another active** | ! "applying makes ours active instead of X" | stop unless `AllowSwitchFrom == X` (confirmed in the REAL dialog) |

## Publish flow
```mermaid
sequenceDiagram
  participant T as TUI
  participant R as remote
  participant K as server
  T->>R: Status() (REAL confirm precheck, only when rules change)
  R->>K: one session: LISTSCRIPTS (+ GETSCRIPT ours if present)
  T->>T: REAL dialog shows server facts; y → Apply with AllowSwitchFrom
  T->>R: Publish(new, opts)
  R->>K: one session: LISTSCRIPTS again (state may have changed)
  alt another active and not allowed / ours missing but other active
    R-->>T: ErrOtherActive
  end
  opt ours exists
    R->>K: GETSCRIPT ours; compare with local → ErrDrift on mismatch
  end
  R->>R: write kolab.sieve.bak, write new kolab.sieve
  R->>K: CHECKSCRIPT (if VERSION), PUTSCRIPT, SETACTIVE, LOGOUT
```

## Testing
`remote_test.go` runs an in-process fake ManageSieve server (real STARTTLS with a throwaway
self-signed cert, SASL PLAIN, script store; `BAD` scripts rejected by CHECKSCRIPT/PUTSCRIPT). It
asserts one session + one login per publish and the stage order, first publish, active-script guards,
drift, rejection restoring the local file (with and without `VERSION`), auth failure, `user_cmd`
fallback, and that credentials are never sent without STARTTLS (`ErrNoTLS`). `go test -race` clean.

Measured against KolabNow (2026-09-25): a full session is ~1.2 s (connect 0.12 · starttls 0.55 ·
auth 0.30 · list/get 0.12 each). `user_cmd` (`op read`) varied 0.6 s → 27 s depending on 1Password
authorisation — hence the `user` config key.

## Invariants
- A failed checkscript leaves the server untouched; the local file is restored from `.bak`.
- A missing remote script is the first-publish case (see Server model), not drift.
- Trailing-whitespace/newline differences are ignored in the drift comparison.
