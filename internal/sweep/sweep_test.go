package sweep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/timing"
)

type env struct {
	t    *testing.T
	cfg  config.Config
	d    Deps
	root string
	n    int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.MailRoot = filepath.Join(home, "Mail")
	for _, f := range []string{"INBOX", "Archive", "Deleted Messages", "Sent Messages", "+Sluice"} {
		for _, s := range []string{"cur", "new", "tmp"} {
			os.MkdirAll(filepath.Join(cfg.MailRoot, f, s), 0o700)
		}
	}
	cfg.SieveFile = filepath.Join(home, "kolab.sieve")
	os.WriteFile(cfg.SieveFile, []byte("require [\"fileinto\"];\nkeep;\n"), 0o644)
	remote := filepath.Join(home, "remote")
	os.MkdirAll(remote, 0o700)
	os.WriteFile(filepath.Join(remote, "kolab"), []byte("require [\"fileinto\"];\nkeep;\n"), 0o644)
	cfg.LockFile = filepath.Join(home, "mbsync.lock")
	cfg.JournalPath = filepath.Join(home, "state", "journal.jsonl")
	cfg.PlanPath = filepath.Join(home, "state", "plan.json")
	cfg.SweepLog = filepath.Join(home, "state", "sweep.jsonl")
	cfg.IndexPath = filepath.Join(home, "cache", "index.db")
	ix, err := index.Open(cfg.IndexPath, cfg.MailRoot, index.Folders{Trash: cfg.TrashFolder,
		Exclude: []string{"Sent Messages"}, Sent: cfg.SentFolders})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	e := &env{t: t, cfg: cfg, root: cfg.MailRoot}
	e.d = Deps{Cfg: cfg, Index: ix, Publisher: sieve.FilePublisher{Dir: remote, Script: "kolab"},
		Now: func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) }}
	return e
}

// msg writes a message into folder and returns its key.
func (e *env) msg(folder, from, listID, to string) string {
	e.n++
	key := fmt.Sprintf("k%03d", e.n)
	hdr := "From: " + from + "\r\nSubject: s" + key + "\r\nDate: Mon, 21 Sep 2026 10:00:00 +0000\r\n"
	if listID != "" {
		hdr += "List-Id: <" + listID + ">\r\n"
	}
	if to != "" {
		hdr += "To: " + to + "\r\n"
	}
	os.WriteFile(filepath.Join(e.root, folder, "cur", key+",U=1:2,S"), []byte(hdr+"\r\nbody\r\n"), 0o600)
	return key
}

func (e *env) count(folder string) int {
	a, _ := os.ReadDir(filepath.Join(e.root, folder, "cur"))
	b, _ := os.ReadDir(filepath.Join(e.root, folder, "new"))
	return len(a) + len(b)
}

func (e *env) plan() *plan.Plan {
	p, err := plan.Load(e.cfg.PlanPath)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestPublishAndQueue(t *testing.T) {
	e := newEnv(t)
	e.msg("Archive", "news@promo.example", "deals.promo.example", "")
	e.msg("INBOX", "news@promo.example", "deals.promo.example", "")
	e.msg("+Sluice", "news@promo.example", "deals.promo.example", "") // two drops, same list
	e.msg("+Sluice", "news@promo.example", "deals.promo.example", "")
	e.msg("+Sluice", "bot@noise.example", "", "") // no List-Id: sender rule

	rep, err := Run(e.d)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 3 || e.count("+Sluice") != 0 || e.count("Deleted Messages") != 3 {
		t.Fatalf("moved=%d sluice=%d trash=%d", rep.Moved, e.count("+Sluice"), e.count("Deleted Messages"))
	}
	for _, o := range rep.Outcomes {
		if o.Result != Published {
			t.Errorf("outcome %+v", o)
		}
	}
	s, _ := os.ReadFile(e.cfg.SieveFile)
	for _, want := range []string{`header :contains "List-Id" "deals.promo.example"`, `address :all :is "from" "bot@noise.example"`, `"source":"sweep"`} {
		if !strings.Contains(string(s), want) {
			t.Errorf("sieve missing %q:\n%s", want, s)
		}
	}
	if strings.Count(string(s), "deals.promo.example\"") != 2 { // metadata + test line: one rule
		t.Errorf("duplicate rule for two drops:\n%s", s)
	}
	p := e.plan()
	if len(p.Rules) != 0 || len(p.Trash) != 1 || p.Trash[0].Label != "sweep: list deals.promo.example" || len(p.Trash[0].Refs) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	log, _ := ReadLog(e.cfg.SweepLog)
	if len(log) != 3 {
		t.Fatalf("log entries = %d", len(log))
	}

	// Nothing left: a second run is a no-op.
	if rep, err := Run(e.d); err != nil || len(rep.Outcomes) != 0 || rep.Moved != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
}

func TestHeldForCorrespondent(t *testing.T) {
	e := newEnv(t)
	e.msg("Sent Messages", "me@home.example", "", "friend@pal.example")
	e.msg("Archive", "friend@pal.example", "", "")
	e.msg("+Sluice", "friend@pal.example", "", "")
	e.msg("+Sluice", "me@home.example", "", "") // own address

	rep, err := Run(e.d)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range rep.Outcomes {
		if o.Result != Held {
			t.Errorf("want held: %+v", o)
		}
	}
	if s, _ := os.ReadFile(e.cfg.SieveFile); strings.Contains(string(s), "pal.example") {
		t.Fatal("held rule was published")
	}
	p := e.plan()
	if len(p.Rules) != 2 || !strings.HasPrefix(p.Trash[0].Label, "sweep (held: friend@pal.example is someone you've sent mail to)") {
		t.Fatalf("plan = %+v", p)
	}
	if e.count("+Sluice") != 0 {
		t.Fatal("held drops should be moved out of the training folder")
	}
}

func TestSkippedAndFailedStay(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.root, "+Sluice", "cur", "junk:2,"), []byte("Subject: no from\r\n\r\n"), 0o600)
	rep, _ := Run(e.d)
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Result != Skipped || e.count("+Sluice") != 1 {
		t.Fatalf("skip: %+v", rep)
	}
	if rep, _ := Run(e.d); len(rep.Outcomes) != 0 {
		t.Fatalf("skip logged twice: %+v", rep.Outcomes)
	}

	// Remote edited elsewhere: publish fails, drop stays for the next sweep.
	os.WriteFile(filepath.Join(filepath.Dir(e.cfg.SieveFile), "remote", "kolab"), []byte("edited;\n"), 0o644)
	e.msg("+Sluice", "a@spam.example", "", "")
	rep, err := Run(e.d)
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, o := range rep.Outcomes {
		if o.Result == Failed {
			failed++
		}
	}
	if failed != 1 || e.count("+Sluice") != 2 || len(e.plan().Trash) != 0 {
		t.Fatalf("failed=%d sluice=%d plan=%+v", failed, e.count("+Sluice"), e.plan())
	}
}

func TestRuleAlreadyExists(t *testing.T) {
	e := newEnv(t)
	e.msg("+Sluice", "a@spam.example", "", "")
	Run(e.d)
	e.msg("Archive", "a@spam.example", "", "")
	e.msg("+Sluice", "a@spam.example", "", "")
	rep, err := Run(e.d)
	if err != nil || len(rep.Outcomes) != 1 || rep.Outcomes[0].Result != Ruled || rep.Outcomes[0].Planned != 1 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestWatch(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, e.d, WatchOptions{Interval: time.Hour, Debounce: 50 * time.Millisecond})
	}()
	time.Sleep(200 * time.Millisecond) // initial sweep of an empty folder
	e.msg("+Sluice", "x@spam.example", "", "")
	deadline := time.Now().Add(5 * time.Second)
	for e.count("+Sluice") != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if e.count("+Sluice") != 0 {
		t.Fatal("watch did not sweep the drop")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTimingLine(t *testing.T) {
	e := newEnv(t)
	e.d.Timing = &timing.Recorder{}
	e.msg("+Sluice", "a@spam.example", "", "")
	rep, err := Run(e.d)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"scan ", "plan ", "move ", "total "} {
		if !strings.Contains(rep.Timing, stage) {
			t.Errorf("timing %q lacks %q", rep.Timing, stage)
		}
	}
	if rep, _ := Run(e.d); rep.Timing != "" {
		t.Errorf("empty sweep reported timing %q", rep.Timing)
	}
}
