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

## Publish flow
```mermaid
sequenceDiagram
  participant T as TUI
  participant R as remote
  participant K as KolabNow
  T->>T: render new text, show diff, wait for "y"
  T->>R: Publish(new)
  R->>K: --download kolab (to temp file)
  R->>R: compare remote vs local kolab.sieve (before edit)
  alt drift
    R-->>T: error "remote differs from local; run sieve-edit to reconcile"
  end
  R->>R: write kolab.sieve.bak, write new kolab.sieve
  R->>K: --checkscript --localsieve kolab.sieve
  R->>K: --upload kolab.sieve --remotesieve kolab
  R->>K: --activate kolab
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
