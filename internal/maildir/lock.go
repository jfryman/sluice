package maildir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockOptions controls how long Lock waits for mbsync.
type LockOptions struct {
	// Timeout is how long to wait without mbsync activity before giving up.
	Timeout time.Duration
	// Root, if set, is the mail root: writes to mbsync's per-folder state
	// files there count as activity, so a long sync that is making
	// progress is waited out (up to MaxWait) instead of failing at Timeout.
	Root string
	// MaxWait caps the total wait even while mbsync is active (default 30m).
	MaxWait time.Duration
	// OnWait, if set, is called about once a second while waiting.
	OnWait func(waited time.Duration)
}

// Lock takes an exclusive flock on the same file mail-sync uses, polling until
// timeout. The returned func releases it.
func Lock(path string, timeout time.Duration) (func(), error) {
	return LockWith(path, LockOptions{Timeout: timeout})
}

// LockWith is Lock with activity-aware waiting and wait reporting.
func LockWith(path string, o LockOptions) (func(), error) {
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.MaxWait == 0 {
		o.MaxWait = 30 * time.Minute
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	start := time.Now()
	idleSince, lastCheck, lastReport := start, time.Time{}, start
	checkEvery := min(5*time.Second, o.Timeout/4)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		now := time.Now()
		if o.Root != "" && now.Sub(lastCheck) >= checkEvery {
			lastCheck = now
			if t := stateActivity(o.Root); t.After(idleSince) {
				idleSince = t
			}
		}
		switch {
		case now.Sub(idleSince) > o.Timeout:
			f.Close()
			if o.Root != "" {
				return nil, fmt.Errorf("mbsync holds %s with no sync progress for %s (waited %s)",
					path, o.Timeout, now.Sub(start).Round(time.Second))
			}
			return nil, fmt.Errorf("mbsync is still running after %s (lock %s)", o.Timeout, path)
		case now.Sub(start) > o.MaxWait:
			f.Close()
			return nil, fmt.Errorf("mbsync is still running after %s (lock %s)", o.MaxWait, path)
		}
		if o.OnWait != nil && now.Sub(lastReport) >= time.Second {
			lastReport = now
			o.OnWait(now.Sub(start))
		}
		time.Sleep(min(500*time.Millisecond, o.Timeout/8))
	}
}

// stateActivity is the newest modification time of mbsync's state files
// (.mbsyncstate*) in the folders under root, two levels deep.
func stateActivity(root string) time.Time {
	var newest time.Time
	for _, pat := range []string{"*/.mbsyncstate*", "*/*/.mbsyncstate*"} {
		ms, _ := filepath.Glob(filepath.Join(root, pat))
		for _, m := range ms {
			if st, err := os.Stat(m); err == nil && st.ModTime().After(newest) {
				newest = st.ModTime()
			}
		}
	}
	return newest
}
