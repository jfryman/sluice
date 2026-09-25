package maildir

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Lock takes an exclusive flock on the same file mail-sync uses, polling until
// timeout. The returned func releases it.
func Lock(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
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
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("mbsync is still running after %s (lock %s)", timeout, path)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
