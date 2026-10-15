package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfryman/sluice/internal/imap"
	"github.com/jfryman/sluice/internal/imap/imaptest"
	"github.com/jfryman/sluice/internal/sieve"
)

func TestPlanOps(t *testing.T) {
	var p Plan
	r := sieve.Rule{Kind: sieve.KindDomain, Value: "spam.example", Action: sieve.ActionTrash}
	p.SetRule("add", r)
	p.SetRule("add", sieve.Rule{Kind: sieve.KindDomain, Value: "SPAM.example", Action: sieve.ActionRead})
	if len(p.Rules) != 1 || p.Rules[0].Rule.Action != sieve.ActionRead {
		t.Fatalf("rules = %+v", p.Rules)
	}
	if n := p.AddTrash("a", []Ref{{"k1", "Archive"}, {"k2", "Archive"}}); n != 2 {
		t.Fatal(n)
	}
	if n := p.AddTrash("b", []Ref{{"k2", "Archive"}, {"k3", "INBOX"}}); n != 1 {
		t.Fatalf("dedupe: %d", n)
	}
	if n := p.AddTrash("c", []Ref{{"k1", "Archive"}}); n != 0 || len(p.Trash) != 2 {
		t.Fatal("fully duplicate item should not be added")
	}
	if p.Messages() != 3 {
		t.Fatal(p.Messages())
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if _, err := Update(path, func(q *Plan) error { *q = p; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Messages() != 3 || len(got.Rules) != 1 {
		t.Fatalf("load = %+v, %v", got, err)
	}
	dst, err := Archive(path, filepath.Join(t.TempDir(), "applied"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := Load(dst); a.Messages() != 3 {
		t.Fatal("archive lost content")
	}
	if e, _ := Load(path); !e.Empty() {
		t.Fatal("plan not reset after archive")
	}
}

func TestApply(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"Archive", "Deleted Messages"} {
		for _, s := range []string{"cur", "new", "tmp"} {
			os.MkdirAll(filepath.Join(root, "Mail", f, s), 0o700)
		}
	}
	mail := filepath.Join(root, "Mail")
	os.WriteFile(filepath.Join(mail, "Archive/cur/k1,U=1:2,S"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(mail, "Archive/cur/k2,U=2:2,"), []byte("x"), 0o600)
	sieveFile := filepath.Join(root, "kolab.sieve")
	os.WriteFile(sieveFile, []byte("require [\"fileinto\"];\nkeep;\n"), 0o644)
	remote := filepath.Join(root, "remote")
	os.MkdirAll(remote, 0o700)
	os.WriteFile(filepath.Join(remote, "kolab"), []byte("require [\"fileinto\"];\nkeep;\n"), 0o644)

	tgt := Target{Name: "sandbox", Root: mail, TrashFolder: "Deleted Messages",
		LockFile: filepath.Join(root, "lock"), JournalPath: filepath.Join(root, "j.jsonl"),
		SieveFile: sieveFile, Publisher: sieve.FilePublisher{Dir: remote, Script: "kolab"}}

	var p Plan
	p.SetRule("add", sieve.Rule{Kind: sieve.KindList, Value: "promo.example", Action: sieve.ActionTrash})
	p.AddTrash("promo", []Ref{{"k1", "Archive"}, {"k2", "Archive"}, {"gone", "Archive"}})

	rep, err := Apply(&p, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 2 || len(rep.Skipped) != 1 || rep.Items[0].Moved != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if b, _ := os.ReadFile(filepath.Join(remote, "kolab")); !strings.Contains(string(b), `"promo.example"`) {
		t.Fatalf("remote not published:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(mail, "Deleted Messages/cur/k1:2,S")); err != nil {
		t.Fatal(err)
	}

	// Re-applying is harmless: rules unchanged, refs already in trash are skipped.
	rep, err = Apply(&p, tgt)
	if err != nil || !rep.RulesSame || rep.Moved != 0 {
		t.Fatalf("reapply = %+v, %v", rep, err)
	}

	// Drift aborts before any mail moves.
	os.WriteFile(filepath.Join(remote, "kolab"), []byte("edited elsewhere;\n"), 0o644)
	os.WriteFile(filepath.Join(mail, "Archive/cur/k3:2,"), []byte("x"), 0o600)
	var p2 Plan
	p2.SetRule("add", sieve.Rule{Kind: sieve.KindList, Value: "other", Action: sieve.ActionTrash})
	p2.AddTrash("k3", []Ref{{"k3", "Archive"}})
	if _, err := Apply(&p2, tgt); err == nil {
		t.Fatal("expected drift error")
	}
	if _, err := os.Stat(filepath.Join(mail, "Archive/cur/k3:2,")); err != nil {
		t.Fatal("mail moved despite publish failure")
	}
}

func TestApplyServerMoves(t *testing.T) {
	root := t.TempDir()
	mail := filepath.Join(root, "Mail")
	for _, f := range []string{"Archive", "Deleted Messages"} {
		os.MkdirAll(filepath.Join(mail, f, "cur"), 0o700)
	}
	os.WriteFile(filepath.Join(mail, "Archive/cur/k1,U=1:2,S"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(mail, "Archive/cur/k2:2,"), []byte("x"), 0o600) // unsynced
	os.WriteFile(filepath.Join(mail, "Archive/.mbsyncstate"), []byte("FarUidValidity 7\n\n41 1 S\n"), 0o600)
	srv := imaptest.New(t)
	srv.Put("Archive", 7, 41)
	srv.Put("Deleted Messages", 9)
	h, port := srv.HostPort()
	var steps []string
	tgt := Target{Name: "real", Root: mail, TrashFolder: "Deleted Messages",
		LockFile: filepath.Join(root, "lock"), JournalPath: filepath.Join(root, "j.jsonl"),
		Server:   imap.Remote{Server: h, Port: port, User: srv.User, PassCmd: []string{"echo", srv.Pass}, TLS: srv.ClientTLS},
		Progress: func(s string) { steps = append(steps, s) }}
	var p Plan
	p.AddTrash("promo", []Ref{{"k1", "Archive"}, {"k2", "Archive"}})
	rep, err := Apply(&p, tgt)
	if err != nil || rep.Moved != 2 || rep.Server != 1 || rep.Items[0].Moved != 2 {
		t.Fatalf("rep = %+v err = %v", rep, err)
	}
	if len(srv.UIDs("Archive")) != 0 || len(srv.UIDs("Deleted Messages")) != 1 {
		t.Errorf("server: archive %v trash %v", srv.UIDs("Archive"), srv.UIDs("Deleted Messages"))
	}
	if !strings.Contains(strings.Join(rep.Lines(), "\n"), "1 of them moved on the server") {
		t.Errorf("report lines: %q", rep.Lines())
	}
	if !strings.Contains(strings.Join(steps, "|"), "moving on server") {
		t.Errorf("progress = %q", steps)
	}
}

func TestSync(t *testing.T) {
	if busy, err := Sync(nil); busy || err != nil {
		t.Errorf("no command: %v %v", busy, err)
	}
	if busy, err := Sync([]string{"sh", "-c", "exit 75"}); !busy || err != nil {
		t.Errorf("exit 75 (mbsync running): busy=%v err=%v", busy, err)
	}
	if _, err := Sync([]string{"sh", "-c", "echo boom; exit 1"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("failure: %v", err)
	}
}
