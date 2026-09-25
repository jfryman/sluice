package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfryman/sluice/internal/config"
)

func TestRefresh(t *testing.T) {
	home := t.TempDir()
	real := config.Default()
	real.MailRoot = filepath.Join(home, "Mail")
	real.SieveFile = filepath.Join(home, "kolab.sieve")
	real.LockFile = filepath.Join(home, "lock")
	real.SandboxDir = filepath.Join(home, "data", "sandbox")
	os.MkdirAll(filepath.Join(real.MailRoot, "Archive", "cur"), 0o700)
	os.WriteFile(filepath.Join(real.MailRoot, "Archive", "cur", "k:2,S"), []byte("From: a@b\r\n\r\n"), 0o600)
	os.WriteFile(real.SieveFile, []byte("keep;\n"), 0o644)

	sb := New(real)
	if sb.Exists() {
		t.Fatal("exists before refresh")
	}
	for i := 0; i < 2; i++ { // second refresh replaces the first
		if err := sb.Refresh(real); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(sb.MailDir(), "Archive", "cur", "k:2,S")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(sb.RemoteDir(), "kolab")); string(b) != "keep;\n" {
		t.Fatalf("remote = %q", b)
	}
	// Changing the clone must not touch the original.
	os.Remove(filepath.Join(sb.MailDir(), "Archive", "cur", "k:2,S"))
	if _, err := os.Stat(filepath.Join(real.MailRoot, "Archive", "cur", "k:2,S")); err != nil {
		t.Fatal("original affected")
	}
	c := sb.Config(real)
	if c.MailRoot != sb.MailDir() || c.PlanPath != real.PlanPath || filepath.Dir(c.IndexPath) != filepath.Join(sb.Dir, "cache") {
		t.Fatalf("config = %+v", c)
	}
}

func TestRefreshSafety(t *testing.T) {
	home := t.TempDir()
	real := config.Default()
	real.MailRoot = filepath.Join(home, "Mail")
	for _, dir := range []string{"relative/sandbox", filepath.Join(home, "notsandbox"), filepath.Join(home, "Mail", "sandbox"), home + "/sandbox/../Mail/sandbox"} {
		if err := (Sandbox{Dir: dir}).Refresh(real); err == nil {
			t.Errorf("%s: expected refusal", dir)
		}
	}
	real.MailRoot = filepath.Join(home, "sandbox", "Mail")
	if err := (Sandbox{Dir: filepath.Join(home, "sandbox")}).Refresh(real); err == nil {
		t.Error("containing mail root: expected refusal")
	}
}
