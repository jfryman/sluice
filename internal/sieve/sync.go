package sieve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The server's script is the source of truth across machines. Each machine
// records the fingerprint of the script it last published or pulled
// (Remote.SyncState); comparing local file, server script and that record
// tells whether the local file is merely behind (safe to update), ahead
// (hand edits, server unchanged) or in conflict. See lode/sieve/remote.md.

// Syncer is a publisher that can bring the local file up to date with the
// server (sieve.Remote); file publishers (the sandbox) don't need it.
type Syncer interface {
	Sync(localPath string) (SyncResult, error)
}

// SyncState classifies the local file against the server script.
type SyncState int

const (
	InSync       SyncState = iota // local == server
	NoRemote                      // the script isn't on the server (first publish creates it)
	LocalMissing                  // no local file (e.g. a new machine): download
	Behind                        // local unchanged since the last sync, server changed: fast-forward
	Ahead                         // local edited, server unchanged since the last sync: publishable
	Conflict                      // both changed (or no record to tell): needs `sluice pull`
)

func (s SyncState) String() string {
	return [...]string{"in sync", "not on server", "no local file", "behind server", "local edits", "conflict"}[s]
}

// ErrConflict: both the local file and the server changed since this
// machine last synced.
var ErrConflict = errors.New("local sieve file and the server script both changed since this machine last synced; " +
	"`sluice pull` (or :pull) adopts the server's and backs up yours")

// ErrBehind: a publish was attempted from a stale local file; Sync first.
var ErrBehind = errors.New("local sieve file is behind the server; sync it first (sluice does this before every apply)")

// SyncResult is what Sync / Pull did.
type SyncResult struct {
	State          SyncState
	Updated        bool   // the local file was replaced with the server's script
	Backup         string // where the previous local file went, if it was replaced
	Added, Removed int    // managed rules gained / lost by the update
}

// Summary is a one-line description for status lines and logs.
func (r SyncResult) Summary() string {
	if !r.Updated {
		return "rules " + r.State.String()
	}
	return fmt.Sprintf("rules updated from server (+%d −%d)", r.Added, r.Removed)
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(normalize(s)))
	return hex.EncodeToString(sum[:])
}

func readRecord(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeRecord(path, text string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // a new machine has no state dir yet
		return err
	}
	return os.WriteFile(path, []byte(fingerprint(text)+"\n"), 0o600)
}

// classify compares local, server and the last-synced record.
func classify(local string, localExists bool, remote string, remoteExists bool, record string) SyncState {
	switch {
	case !remoteExists:
		return NoRemote
	case !localExists:
		return LocalMissing
	case normalize(local) == normalize(remote):
		return InSync
	case record != "" && fingerprint(local) == record:
		return Behind
	case record != "" && fingerprint(remote) == record:
		return Ahead
	}
	return Conflict
}

func readLocal(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// Sync brings the local file up to date with the server when that is safe
// (missing or merely behind), records the synced fingerprint, and returns
// ErrConflict when both sides changed.
func (r Remote) Sync(localPath string) (SyncResult, error) { return r.sync(localPath, false) }

// Pull adopts the server's script regardless, backing up a differing local
// file to <file>.bak-<time>.
func (r Remote) Pull(localPath string) (SyncResult, error) { return r.sync(localPath, true) }

func (r Remote) sync(localPath string, force bool) (SyncResult, error) {
	unlock, err := lockLocal(localPath)
	if err != nil {
		return SyncResult{}, err
	}
	defer unlock()
	local, localExists, err := readLocal(localPath)
	if err != nil {
		return SyncResult{}, err
	}
	var res SyncResult
	err = r.session(func(c *Client) error {
		names, _, err := c.List()
		if err != nil {
			return err
		}
		exists := false
		for _, n := range names {
			exists = exists || n == r.Script
		}
		remote := ""
		if exists {
			if remote, err = c.Get(r.Script); err != nil {
				return err
			}
		}
		res.State = classify(local, localExists, remote, exists, readRecord(r.SyncState))
		switch {
		case res.State == NoRemote:
			return nil
		case res.State == InSync:
			return writeRecord(r.SyncState, remote)
		case res.State == Ahead:
			return nil
		case res.State == Conflict && !force:
			return ErrConflict
		}
		// LocalMissing, Behind, or a forced pull: take the server's script.
		if localExists {
			res.Backup = localPath + ".bak"
			if force && res.State != Behind {
				res.Backup = localPath + ".bak-" + time.Now().Format("20060102T150405")
			}
			if err := os.WriteFile(res.Backup, []byte(local), 0o644); err != nil {
				return err
			}
		}
		if err := os.WriteFile(localPath, []byte(remote), 0o644); err != nil {
			return err
		}
		res.Updated = true
		res.Added, res.Removed = ruleDelta(local, remote)
		return writeRecord(r.SyncState, remote)
	})
	return res, err
}

// ruleDelta counts managed rules in next that aren't in prev, and vice versa.
func ruleDelta(prev, next string) (added, removed int) {
	key := func(r Rule) string {
		return string(r.Kind) + "\x00" + strings.ToLower(r.Value) + "\x00" + string(r.Action)
	}
	set := func(text string) map[string]bool {
		m := map[string]bool{}
		if s, err := Parse(text); err == nil {
			for _, r := range s.Rules {
				m[key(r)] = true
			}
		}
		return m
	}
	a, b := set(prev), set(next)
	for k := range b {
		if !a[k] {
			added++
		}
	}
	for k := range a {
		if !b[k] {
			removed++
		}
	}
	return added, removed
}
