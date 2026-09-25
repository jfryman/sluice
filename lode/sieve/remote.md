# Sieve upload (ManageSieve via sieve-connect)

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

## Credential pattern (mirrors `~/.local/bin/sieve-edit`)
- Username: output of config `user_cmd` (default `mail-user`, reads 1Password).
- Password: output of config `pass_cmd` (default `mail-pass`, 1Password cached in kernel keyring 8h).
- Password is written, newline-terminated, to a pipe passed as fd 3; sieve-connect gets `--passwordfd 3`.
  It is never stored on disk or kept beyond the call.

```go
cmd := exec.Command("sieve-connect", "--server", srv, "--user", user, "--passwordfd", "3", args...)
r, w, _ := os.Pipe()
cmd.ExtraFiles = []*os.File{r}
```

Server: config `sieve_server` (default `imap.kolabnow.com`), script name `sieve_script` (default `kolab`).

## Server model
ManageSieve has no partial edits: a publish uploads the **whole** script (hand-written text preserved
byte-for-byte, managed block regenerated) and activates it. Exactly one script can be active.
`--list` prints the server's raw lines: `"name"` or `"name" ACTIVE`; nothing when there are no scripts.

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
  R->>K: --list (+ --download ours if present)
  T->>T: REAL dialog shows server facts; y → Apply with AllowSwitchFrom
  T->>R: Publish(new, opts)
  R->>K: --list again (state may have changed)
  alt another active and not allowed / ours missing but other active
    R-->>T: ErrOtherActive
  end
  opt ours exists
    R->>K: --download ours; compare with local → ErrDrift on mismatch
  end
  R->>R: write kolab.sieve.bak, write new kolab.sieve
  R->>K: --checkscript, --upload, --activate
```

`checkscript` needs the server's VERSION capability; if sieve-connect's error mentions VERSION the
step is skipped (PUTSCRIPT validates anyway). Any other checkscript failure aborts and restores.

## Testing
`remote_test.go` puts a bash `sieve-connect` stub on `PATH` that logs argv + the fd-3 password,
serves a canned remote script on `--download`, and rejects scripts containing `BAD`. It asserts the
password never appears in argv, drift aborts, and a rejected script restores the local file.

## Invariants
- A failed checkscript leaves the server untouched; the local file is restored from `.bak`.
- A remote script that is missing (first run) counts as "no drift" only when local is empty.
- Temp downloads go to `os.MkdirTemp` and are removed.
- Trailing-whitespace/newline differences are ignored in the drift comparison.
