package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/sieve"
)

type env struct {
	cfg  config.Config
	opts Options
	home string
}

// healthy builds a temp environment where every offline check passes.
func healthy(t *testing.T) env {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.MailRoot = filepath.Join(home, "Mail")
	for _, f := range []string{"INBOX", "Archive", "Deleted Messages", "Drafts", "Sent Messages", "Sent Items", "+SaneBlackHole", "+SaneNews"} {
		for _, s := range []string{"cur", "new", "tmp"} {
			os.MkdirAll(filepath.Join(cfg.MailRoot, f, s), 0o700)
		}
	}
	os.WriteFile(filepath.Join(cfg.MailRoot, "Archive", "cur", "k:2,S"), []byte("From: a@b\r\n\r\n"), 0o600)
	cfg.SieveFile = filepath.Join(home, "kolab.sieve")
	os.WriteFile(cfg.SieveFile, []byte("require [\"fileinto\"];\nkeep;\n"), 0o644)
	cfg.LockFile = filepath.Join(home, "mbsync.lock")
	os.WriteFile(cfg.LockFile, nil, 0o600)
	cfg.SandboxDir = filepath.Join(home, "data", "sluice", "sandbox")
	os.MkdirAll(filepath.Dir(cfg.SandboxDir), 0o700)
	cfg.IndexPath = filepath.Join(home, "cache", "index.db")
	cfg.PlanPath = filepath.Join(home, "state", "plan.json")
	rc := filepath.Join(home, "mbsyncrc")
	os.WriteFile(rc, []byte("MaildirStore x\nPath "+cfg.MailRoot+"/\nChannel c\nSync All\nExpunge Both\n"), 0o600)
	return env{cfg: cfg, home: home, opts: Options{
		ConfigPath: filepath.Join(home, "config.toml"),
		MbsyncRC:   rc,
		LookPath:   func(s string) (string, error) { return "/usr/bin/" + s, nil },
	}}
}

func find(t *testing.T, r Report, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, r.Checks)
	return Check{}
}

func TestHealthy(t *testing.T) {
	e := healthy(t)
	r := Run(e.cfg, e.opts)
	for _, c := range r.Checks {
		// reflink depends on the test filesystem; tolerate a warning there.
		if c.Status != OK && !(c.Name == "reflink" && c.Status == Warn) {
			t.Errorf("%s: %s %s (fix: %s)", c.Name, c.Status.Symbol(), c.Detail, c.Fix)
		}
	}
	for _, cap := range Capabilities {
		if !r.Available(cap) {
			t.Errorf("%s unavailable", cap)
		}
	}
	if r.Failed() {
		t.Error("healthy env reported failure")
	}
}

func TestBrokenEnvironments(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(e *env)
		check  string
		want   Status
		fix    string     // substring expected in Fix
		lost   Capability // capability that must become unavailable ("" = none)
	}{
		{"no mail root", func(e *env) { e.cfg.MailRoot = filepath.Join(e.home, "nope") }, "mail root", Fail, "mail_root", CapBrowse},
		{"no trash", func(e *env) { e.cfg.TrashFolder = "Trash" }, "trash folder", Fail, "trash_folder", CapReal},
		{"typo folder", func(e *env) { e.cfg.NewsFolders = []string{"+SaneNewz"} }, "folders", Warn, "folder names", ""},
		{"no sieve-connect", func(e *env) {
			look := e.opts.LookPath
			e.opts.LookPath = func(s string) (string, error) {
				if s == "sieve-connect" {
					return "", errors.New("not found")
				}
				return look(s)
			}
		}, "sieve-connect", Fail, "install sieve-connect", CapReal},
		{"no pass cmd", func(e *env) { e.cfg.PassCmd = nil }, "credentials", Fail, "pass_cmd", CapReal},
		{"bad sieve", func(e *env) {
			os.WriteFile(e.cfg.SieveFile, []byte("# >>> sluice managed block — edit via sluice >>>\n"), 0o644)
		}, "sieve file", Fail, "markers", CapReal},
		{"missing sieve", func(e *env) { os.Remove(e.cfg.SieveFile) }, "sieve file", Warn, "download", ""},
		{"mbsync pull only", func(e *env) {
			os.WriteFile(e.opts.MbsyncRC, []byte("Path "+e.cfg.MailRoot+"\nSync Pull\nExpunge Both\n"), 0o600)
		}, "mbsync config", Warn, "Sync All", ""},
		{"mbsync no expunge", func(e *env) {
			os.WriteFile(e.opts.MbsyncRC, []byte("Path "+e.cfg.MailRoot+"\n"), 0o600)
		}, "mbsync config", Warn, "Expunge Both", ""},
		{"mbsync other path", func(e *env) {
			os.WriteFile(e.opts.MbsyncRC, []byte("Path ~/Elsewhere\nExpunge Both\n"), 0o600)
		}, "mbsync config", Warn, "MaildirStore Path", ""},
		{"no lock", func(e *env) { os.Remove(e.cfg.LockFile) }, "sync lock", Warn, "flock", ""},
		{"bad sandbox", func(e *env) { e.cfg.SandboxDir = filepath.Join(e.cfg.MailRoot, "sandbox") }, "sandbox dir", Fail, "sandbox_dir", CapSandbox},
		{"corrupt plan", func(e *env) {
			os.MkdirAll(filepath.Dir(e.cfg.PlanPath), 0o700)
			os.WriteFile(e.cfg.PlanPath, []byte("{nope"), 0o600)
		}, "state", Fail, "corrupt", CapBrowse},
		{"config error", func(e *env) { e.opts.ConfigErr = errors.New("toml: line 3") }, "config", Fail, "TOML", CapBrowse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := healthy(t)
			tt.break_(&e)
			r := Run(e.cfg, e.opts)
			c := find(t, r, tt.check)
			if c.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", c.Status.Symbol(), c.Detail, tt.want.Symbol())
			}
			if !strings.Contains(c.Fix, tt.fix) {
				t.Errorf("fix %q lacks %q", c.Fix, tt.fix)
			}
			if tt.lost != "" && r.Available(tt.lost) {
				t.Errorf("%s still available", tt.lost)
			}
			if tt.want == Warn && r.Failed() {
				t.Errorf("a warning made the report fail")
			}
		})
	}
}

func TestOnline(t *testing.T) {
	cases := []struct {
		st   sieve.ServerStatus
		err  error
		want Status
	}{
		{sieve.ServerStatus{Exists: true, Active: true}, nil, OK},
		{sieve.ServerStatus{Exists: true, Active: true, Drift: true}, nil, Warn},
		{sieve.ServerStatus{Exists: true}, nil, Warn},
		{sieve.ServerStatus{Scripts: []string{"other"}}, nil, Fail},
		{sieve.ServerStatus{}, errors.New("auth failed"), Fail},
	}
	for i, c := range cases {
		e := healthy(t)
		e.opts.Online = true
		e.opts.RemoteStat = func() (sieve.ServerStatus, error) { return c.st, c.err }
		if got := find(t, Run(e.cfg, e.opts), "server"); got.Status != c.want {
			t.Errorf("case %d: %s (%s), want %s", i, got.Status.Symbol(), got.Detail, c.want.Symbol())
		}
	}
	// Offline never runs the server check.
	e := healthy(t)
	e.opts.RemoteStat = func() (sieve.ServerStatus, error) {
		t.Fatal("server contacted offline")
		return sieve.ServerStatus{}, nil
	}
	for _, c := range Run(e.cfg, e.opts).Checks {
		if c.Name == "server" {
			t.Fatal("server check ran offline")
		}
	}
}

func TestNeverWritesInMailRoot(t *testing.T) {
	e := healthy(t)
	e.cfg.SandboxDir = filepath.Join(e.cfg.MailRoot, "x", "sandbox") // invalid: inside mail root
	before, _ := os.ReadDir(e.cfg.MailRoot)
	Run(e.cfg, e.opts)
	e.cfg.SandboxDir = filepath.Join(e.cfg.MailRoot, "sandbox")
	Run(e.cfg, e.opts)
	after, _ := os.ReadDir(e.cfg.MailRoot)
	if len(after) != len(before) {
		t.Fatalf("mail root changed: %d → %d entries", len(before), len(after))
	}
}
