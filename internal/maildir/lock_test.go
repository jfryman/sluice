package maildir

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// hold takes the lock the way mbsync's flock would, until release.
func hold(t *testing.T, path string) (release func()) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

func TestLockWithGivesUpWhenIdle(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "lock")
	release := hold(t, lock)
	defer release()
	waits := 0
	start := time.Now()
	_, err := LockWith(lock, LockOptions{Timeout: 400 * time.Millisecond, Root: dir, OnWait: func(time.Duration) { waits++ }})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestLockWithWaitsWhileMbsyncIsActive(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Deleted Messages"), 0o700)
	state := filepath.Join(root, "Deleted Messages", ".mbsyncstate.journal")
	lock := filepath.Join(t.TempDir(), "lock")
	release := hold(t, lock)

	// "mbsync" writes its state for 1.2s (3x the idle timeout), then exits.
	go func() {
		for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			os.WriteFile(state, []byte(time.Now().String()), 0o600)
		}
		release()
	}()
	var waited time.Duration
	unlock, err := LockWith(lock, LockOptions{Timeout: 400 * time.Millisecond, Root: root, OnWait: func(w time.Duration) { waited = w }})
	if err != nil {
		t.Fatalf("gave up on an active sync: %v", err)
	}
	unlock()
	if waited < time.Second {
		t.Errorf("OnWait last reported %s", waited)
	}
}
