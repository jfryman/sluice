package sieve

import (
	"os"
	"syscall"
)

// lockLocal serialises publishes that rewrite localPath (TUI, headless
// apply and the sweep service may all publish). Blocks until acquired.
func lockLocal(localPath string) (func(), error) {
	f, err := os.OpenFile(localPath+".lock", os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
