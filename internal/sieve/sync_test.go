package sieve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	scriptA = "require [\"fileinto\"];\n# >>> sluice managed block — edit via sluice >>>\n" +
		"# sluice: {\"kind\":\"sender\",\"value\":\"a@spam.example\",\"action\":\"trash\"}\n" +
		"if address :all :is \"from\" \"a@spam.example\" {\n    fileinto \"Deleted Messages\";\n    stop;\n}\n" +
		"# <<< sluice managed block <<<\nkeep;\n"
)

// scriptB is scriptA with a second managed rule, as another machine's
// sweep would publish it.
var scriptB = strings.Replace(scriptA, "# <<< sluice managed block <<<",
	"# sluice: {\"kind\":\"list\",\"value\":\"deals.example\",\"action\":\"trash\"}\n"+
		"if header :contains \"List-Id\" \"deals.example\" {\n    fileinto \"Deleted Messages\";\n    stop;\n}\n"+
		"# <<< sluice managed block <<<", 1)

// machine is one computer: its local sieve file and sync record.
type machine struct {
	r    Remote
	file string
}

// newMachine is a fresh computer: neither its state dir nor its sieve dir
// exists yet.
func newMachine(t *testing.T, f *fakeServer) machine {
	dir := t.TempDir()
	r := f.remote()
	r.SyncState = filepath.Join(dir, "state", "sluice", "sieve-synced")
	return machine{r: r, file: filepath.Join(dir, "config", "sieve", "kolab.sieve")}
}

func (m machine) local(t *testing.T) string {
	b, err := os.ReadFile(m.file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClassify(t *testing.T) {
	rec := fingerprint(scriptA)
	cases := []struct {
		local             string
		localOK, remoteOK bool
		remote, record    string
		want              SyncState
	}{
		{scriptA, true, true, scriptA, "", InSync},
		{scriptA, true, true, scriptA + "  \n", "", InSync}, // whitespace-normalized
		{"", false, true, scriptA, "", LocalMissing},
		{scriptA, true, false, "", rec, NoRemote},
		{scriptA, true, true, scriptB, rec, Behind},
		{scriptB, true, true, scriptA, rec, Ahead},
		{scriptB, true, true, scriptA + "# edited\n", rec, Conflict},
		{scriptB, true, true, scriptA, "", Conflict}, // no record: can't tell who changed
	}
	for i, c := range cases {
		if got := classify(c.local, c.localOK, c.remote, c.remoteOK, c.record); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

// Two machines share one server: a new machine downloads, a stale one
// fast-forwards, and both can publish.
func TestSyncAcrossMachines(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", scriptA, true)
	desk, laptop := newMachine(t, f), newMachine(t, f)

	// New machine: no local file → downloaded.
	res, err := laptop.r.Sync(laptop.file)
	if err != nil || res.State != LocalMissing || !res.Updated || res.Added != 1 || res.Backup != "" {
		t.Fatalf("new machine: %+v %v", res, err)
	}
	if laptop.local(t) != scriptA {
		t.Fatal("not downloaded")
	}
	// Already in sync: records, changes nothing.
	os.MkdirAll(filepath.Dir(desk.file), 0o700)
	os.WriteFile(desk.file, []byte(scriptA), 0o644)
	if res, err := desk.r.Sync(desk.file); err != nil || res.State != InSync || res.Updated {
		t.Fatalf("desk in sync: %+v %v", res, err)
	}

	// The desk publishes a new rule (e.g. its sweep).
	if _, err := desk.r.Publish(desk.file, scriptB, PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	// The laptop is now behind: Status says so without calling it drift, and
	// publishing from the stale base is refused.
	st, err := laptop.r.Status(laptop.file)
	if err != nil || st.Sync != Behind || st.Drift {
		t.Fatalf("laptop status: %+v %v", st, err)
	}
	if _, err := laptop.r.Publish(laptop.file, scriptA+"# x\n", PublishOptions{}); !errors.Is(err, ErrBehind) {
		t.Fatalf("stale publish: %v", err)
	}
	// Sync fast-forwards it (with a .bak), and then it can publish.
	res, err = laptop.r.Sync(laptop.file)
	if err != nil || res.State != Behind || !res.Updated || res.Added != 1 || res.Removed != 0 || laptop.local(t) != scriptB {
		t.Fatalf("fast-forward: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(laptop.file + ".bak"); string(b) != scriptA {
		t.Error("no .bak of the previous local file")
	}
	if _, err := laptop.r.Publish(laptop.file, scriptB+"# laptop\n", PublishOptions{}); err != nil {
		t.Fatalf("publish after sync: %v", err)
	}
}

func TestSyncAheadConflictAndPull(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", scriptA, true)
	m := newMachine(t, f)
	if _, err := m.r.Sync(m.file); err != nil {
		t.Fatal(err)
	}
	// Hand edit locally, server unchanged: Ahead — nothing pulled, publishable.
	os.WriteFile(m.file, []byte(scriptA+"# my edit\n"), 0o644)
	if res, err := m.r.Sync(m.file); err != nil || res.State != Ahead || res.Updated {
		t.Fatalf("ahead: %+v %v", res, err)
	}
	if log, err := m.r.Publish(m.file, scriptA+"# my edit\n", PublishOptions{}); err != nil || !strings.Contains(strings.Join(log, " "), "local edits") {
		t.Fatalf("publish local edits: %q %v", log, err)
	}

	// Both change: conflict on sync and publish; the local file is untouched.
	os.WriteFile(m.file, []byte(scriptA+"# second edit\n"), 0o644)
	f.put("kolab", scriptB, true)
	if _, err := m.r.Sync(m.file); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict sync: %v", err)
	}
	if _, err := m.r.Publish(m.file, scriptA, PublishOptions{}); !errors.Is(err, ErrDrift) {
		t.Fatalf("conflict publish: %v", err)
	}
	if m.local(t) != scriptA+"# second edit\n" {
		t.Fatal("conflict changed the local file")
	}
	// Pull adopts the server's and keeps the edit in a timestamped backup.
	res, err := m.r.Pull(m.file)
	if err != nil || !res.Updated || !strings.Contains(res.Backup, ".bak-") || m.local(t) != scriptB {
		t.Fatalf("pull: %+v %v", res, err)
	}
	if b, _ := os.ReadFile(res.Backup); string(b) != scriptA+"# second edit\n" {
		t.Error("pull backup lost the local edit")
	}
	if st, _ := m.r.Status(m.file); st.Sync != InSync {
		t.Errorf("after pull: %s", st.Sync)
	}
}
