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
	for _, f := range []string{"INBOX", "Archive", "Deleted Messages", "Drafts", "Sent Messages", "Sent Items", "+SaneBlackHole", "+SaneNews", "+Sluice"} {
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
	cfg.User = "me@home.example"
	cfg.PlanPath = filepath.Join(home, "state", "plan.json")
	cfg.SweepLog = filepath.Join(home, "state", "sweep.jsonl")
	rc := filepath.Join(home, "mbsyncrc")
	os.WriteFile(rc, []byte("MaildirStore x\nPath "+cfg.MailRoot+"/\nChannel c\nSync All\nExpunge Both\nCreate Both\n"), 0o600)
	return env{cfg: cfg, home: home, opts: Options{
		ConfigPath:   filepath.Join(home, "config.toml"),
		MbsyncRC:     rc,
		LookPath:     func(s string) (string, error) { return "/usr/bin/" + s, nil },
		ServiceState: func() (string, error) { return "active", nil },
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
		{"user via user_cmd", func(e *env) { e.cfg.User = "" }, "credentials", Warn, "set `user", ""},
		{"no user at all", func(e *env) { e.cfg.User, e.cfg.UserCmd = "", nil }, "credentials", Fail, "set user", CapReal},
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
		{"no training folder", func(e *env) { os.RemoveAll(filepath.Join(e.cfg.MailRoot, "+Sluice")) }, "training folder", Warn, "+Sluice'/{cur,new,tmp}", ""},
		{"custom training folder missing", func(e *env) { e.cfg.TrainingFolder = "+Drop" }, "training folder", Warn, "+Drop'/{cur,new,tmp}", ""},
		{"empty training folder", func(e *env) { e.cfg.TrainingFolder = "" }, "training folder", Warn, "training_folder", ""},
		{"mbsync no create", func(e *env) {
			os.WriteFile(e.opts.MbsyncRC, []byte("Path "+e.cfg.MailRoot+"\nExpunge Both\nCreate Near\n"), 0o600)
		}, "mbsync create", Warn, "Create Both", ""},
		{"sluice not installed", func(e *env) {
			look := e.opts.LookPath
			e.opts.LookPath = func(s string) (string, error) {
				if s == "sluice" {
					return "", errors.New("not found")
				}
				return look(s)
			}
		}, "sluice on PATH", Warn, "make install", ""},
		{"post-sweep cmd missing", func(e *env) {
			look := e.opts.LookPath
			e.opts.LookPath = func(s string) (string, error) {
				if s == "mail-sync" {
					return "", errors.New("not found")
				}
				return look(s)
			}
		}, "post-sweep cmd", Warn, "post_sweep_cmd", ""},
		{"service not installed", func(e *env) { e.opts.ServiceState = func() (string, error) { return "not-found", nil } }, "sweep service", Warn, "make install-service", ""},
		{"service failed", func(e *env) { e.opts.ServiceState = func() (string, error) { return "failed", errors.New("exit 3") } }, "sweep service", Warn, "service-logs", ""},
		{"last sweep failed", func(e *env) {
			os.MkdirAll(filepath.Dir(e.cfg.SweepLog), 0o700)
			os.WriteFile(e.cfg.SweepLog, []byte(`{"time":"t","key":"k","kind":"sender","value":"a@spam.example","result":"error","reason":"publish: drift"}`+"\n"), 0o600)
		}, "last sweep", Warn, "retried", ""},
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
		fix  string
	}{
		{sieve.ServerStatus{Exists: true, Active: "kolab"}, nil, OK, ""},
		{sieve.ServerStatus{Exists: true, Active: "kolab", Drift: true}, nil, Warn, "re-download"},
		{sieve.ServerStatus{Exists: true}, nil, Warn, "activates"},
		{sieve.ServerStatus{Exists: true, Active: "roundcube"}, nil, Warn, "confirm the switch"},
		{sieve.ServerStatus{}, nil, Warn, "first apply"},                                              // no scripts at all
		{sieve.ServerStatus{Scripts: []string{"old"}}, nil, Warn, "first apply"},                      // only inactive others
		{sieve.ServerStatus{Scripts: []string{"rc"}, Active: "rc"}, nil, Fail, `sieve_script = "rc"`}, // never switch silently
		{sieve.ServerStatus{}, errors.New("auth failed"), Fail, "credentials"},
	}
	for i, c := range cases {
		e := healthy(t)
		e.opts.Online = true
		e.opts.RemoteStat = func() (sieve.ServerStatus, error) { return c.st, c.err }
		got := find(t, Run(e.cfg, e.opts), "server")
		if got.Status != c.want || !strings.Contains(got.Fix, c.fix) {
			t.Errorf("case %d: %s (%s; fix %q), want %s with fix containing %q", i, got.Status.Symbol(), got.Detail, got.Fix, c.want.Symbol(), c.fix)
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

func TestCapabilitySummary(t *testing.T) {
	e := healthy(t)
	e.cfg.TrainingFolder = "+Missing"
	r := Run(e.cfg, e.opts)
	if !r.Available(CapSweep) || !r.Degraded(CapSweep) || r.Degraded(CapBrowse) {
		t.Fatalf("available=%v degraded=%v", r.Available(CapSweep), r.Degraded(CapSweep))
	}
	last := r.Lines()[len(r.Lines())-1]
	if !strings.Contains(last, "! sweep") || !strings.Contains(last, "✓ browse & plan") {
		t.Errorf("summary = %q", last)
	}
	// A custom training folder that exists is accepted.
	os.MkdirAll(filepath.Join(e.cfg.MailRoot, "+Missing", "cur"), 0o700)
	if c := find(t, Run(e.cfg, e.opts), "training folder"); c.Status != OK || c.Detail != "+Missing" {
		t.Errorf("custom folder: %+v", c)
	}
}
