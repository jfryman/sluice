// Package sandbox maintains a full, isolated reflink clone of the mail
// environment for validating plans. See lode/apply/sandbox.md.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/maildir"
	"github.com/jfryman/sluice/internal/sieve"
)

type Sandbox struct{ Dir string }

func New(cfg config.Config) Sandbox { return Sandbox{Dir: cfg.SandboxDir} }

func (s Sandbox) MailDir() string   { return filepath.Join(s.Dir, "Mail") }
func (s Sandbox) RemoteDir() string { return filepath.Join(s.Dir, "remote") }
func (s Sandbox) SieveFile() string { return filepath.Join(s.Dir, "kolab.sieve") }

// Exists reports whether a clone has been made.
func (s Sandbox) Exists() bool {
	_, err := os.Stat(filepath.Join(s.Dir, "created"))
	return err == nil
}

// Created is the time of the last refresh (zero if none).
func (s Sandbox) Created() time.Time {
	b, err := os.ReadFile(filepath.Join(s.Dir, "created"))
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	return t
}

// Config redirects every path of the real config into the sandbox. The plan
// path and sandbox dir stay shared.
func (s Sandbox) Config(real config.Config) config.Config {
	c := real
	c.MailRoot = s.MailDir()
	c.LockFile = filepath.Join(s.Dir, "mbsync.lock")
	c.SieveFile = s.SieveFile()
	c.IndexPath = filepath.Join(s.Dir, "cache", "index.db")
	c.JournalPath = filepath.Join(s.Dir, "state", "journal.jsonl")
	c.SweepLog = filepath.Join(s.Dir, "state", "sweep.jsonl")
	return c
}

// Publisher is the sandbox stand-in for the Sieve server.
func (s Sandbox) Publisher(real config.Config) sieve.FilePublisher {
	return sieve.FilePublisher{Dir: s.RemoteDir(), Script: real.SieveScript}
}

func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Validate reports whether Dir is safe to use: absolute, ends in /sandbox,
// and neither contains nor is contained by the mail root.
func (s Sandbox) Validate(real config.Config) error {
	d := filepath.Clean(s.Dir)
	m := filepath.Clean(real.MailRoot)
	switch {
	case !filepath.IsAbs(d):
		return fmt.Errorf("sandbox dir %q must be absolute", s.Dir)
	case filepath.Base(d) != "sandbox":
		return fmt.Errorf("sandbox dir %q must end in /sandbox", s.Dir)
	case within(d, m) || within(m, d):
		return fmt.Errorf("sandbox dir %q overlaps mail root %q", s.Dir, real.MailRoot)
	}
	return nil
}

// Refresh re-clones mail and the sieve script from the real environment and
// clears the sandbox index and journal. The clone is taken under the real
// mbsync lock so it is a consistent snapshot.
func (s Sandbox) Refresh(real config.Config) error {
	if err := s.Validate(real); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	old := s.MailDir() + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(s.MailDir(), old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	unlock, err := maildir.Lock(real.LockFile, 2*time.Minute)
	if err != nil {
		return err
	}
	out, err := exec.Command("cp", "-a", "--reflink=auto", filepath.Clean(real.MailRoot), s.MailDir()).CombinedOutput()
	unlock()
	if err != nil {
		return fmt.Errorf("clone mail: %w: %s", err, strings.TrimSpace(string(out)))
	}

	sv, err := os.ReadFile(real.SieveFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.RemoteDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.SieveFile(), sv, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.RemoteDir(), real.SieveScript), sv, 0o644); err != nil {
		return err
	}
	for _, p := range []string{old, filepath.Join(s.Dir, "cache"), filepath.Join(s.Dir, "state")} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(s.Dir, "created"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}
