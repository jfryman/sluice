package cleanup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jfryman/sluice/internal/imap"
	"github.com/jfryman/sluice/internal/imap/imaptest"
	"github.com/jfryman/sluice/internal/index"
)

func serverEnv(t *testing.T) (*Cleaner, *imaptest.Server, string) {
	root := t.TempDir()
	mkdirs(t, root, "Archive", "+SaneLater", "Deleted Messages")
	now := time.Now()
	// Archive: near U=1,2 ↔ far 101,102 (synced); k3 has no U= (never synced).
	msg(t, filepath.Join(root, "Archive/cur/k1,U=1:2,S"), "a@spam.example", "", now)
	msg(t, filepath.Join(root, "Archive/cur/k2,U=2:2,"), "a@spam.example", "", now)
	msg(t, filepath.Join(root, "Archive/cur/k3:2,S"), "a@spam.example", "", now)
	os.WriteFile(filepath.Join(root, "Archive/.mbsyncstate"), []byte("FarUidValidity 111\nNearUidValidity 5\n\n101 1 S\n102 2 \n"), 0o600)
	// +SaneLater: mbsync was interrupted (journal present) → local move.
	msg(t, filepath.Join(root, "+SaneLater/cur/k4,U=1:2,"), "a@spam.example", "", now)
	os.WriteFile(filepath.Join(root, "+SaneLater/.mbsyncstate"), []byte("FarUidValidity 222\n\n401 1 \n"), 0o600)
	os.WriteFile(filepath.Join(root, "+SaneLater/.mbsyncstate.journal"), []byte("+ 0 2\n"), 0o600)

	srv := imaptest.New(t)
	srv.Put("Archive", 111, 101, 102, 103)
	srv.Put("+SaneLater", 222, 401)
	srv.Put("Deleted Messages", 999, 5)
	h, p := srv.HostPort()
	var stages []string
	c := &Cleaner{Root: root, TrashFolder: "Deleted Messages", LockFile: filepath.Join(root, "lock"),
		JournalPath: filepath.Join(root, "j.jsonl"),
		Server:      imap.Remote{Server: h, Port: p, User: srv.User, PassCmd: []string{"echo", srv.Pass}, TLS: srv.ClientTLS},
		Stage:       func(s string) { stages = append(stages, s) }}
	return c, srv, root
}

func refs(keys ...string) []index.Message {
	var out []index.Message
	for _, k := range keys {
		f := "Archive"
		if k == "k4" {
			f = "+SaneLater"
		}
		out = append(out, index.Message{Key: k, Folder: f}) // no path: resolved by key, as from a plan
	}
	return out
}

func TestTrashOnServerAndUndo(t *testing.T) {
	c, srv, root := serverEnv(t)
	res, err := c.Trash(refs("k1", "k2", "k3", "k4"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 4 || res.Server != 2 || len(res.Skipped) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "+SaneLater") {
		t.Errorf("notes = %q", res.Notes)
	}
	// Server: 101, 102 moved; 103 untouched; +SaneLater untouched (fell back).
	if got := srv.UIDs("Archive"); !reflect.DeepEqual(got, []uint32{103}) {
		t.Errorf("server Archive = %v", got)
	}
	if got := srv.UIDs("Deleted Messages"); !reflect.DeepEqual(got, []uint32{5, 6, 7}) {
		t.Errorf("server trash = %v", got)
	}
	// Local: server-moved files stay for mbsync; k3/k4 moved as files.
	for _, f := range []string{"Archive/cur/k1,U=1:2,S", "Archive/cur/k2,U=2:2,"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("server-moved file should stay until the sync: %v", err)
		}
	}
	trash, _ := os.ReadDir(filepath.Join(root, "Deleted Messages/cur"))
	if len(trash) != 2 {
		t.Errorf("local trash = %d files", len(trash))
	}
	entries, _ := c.readJournal()
	modes := map[string]Entry{}
	for _, e := range entries {
		modes[e.Key] = e
	}
	if e := modes["k1"]; e.Mode != ModeServer || e.FarUID != 101 || e.DstUID != 6 || e.DstValid != 999 {
		t.Errorf("k1 entry = %+v", e)
	}
	if e := modes["k3"]; e.Mode != "" || e.Batch != res.Batch {
		t.Errorf("k3 entry = %+v", e)
	}

	u, err := c.Undo()
	if err != nil || u.Moved != 4 || u.Server != 2 {
		t.Fatalf("undo = %+v %v", u, err)
	}
	if got := srv.UIDs("Archive"); len(got) != 3 || got[0] != 103 {
		t.Errorf("server Archive after undo = %v", got) // back with new UIDs
	}
	if got := srv.UIDs("Deleted Messages"); !reflect.DeepEqual(got, []uint32{5}) {
		t.Errorf("server trash after undo = %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "Archive/cur/k3:2,S")); err != nil {
		t.Errorf("k3 not restored locally: %v", err)
	}
}

func TestTrashOnServerStaleAndLoginFailure(t *testing.T) {
	c, srv, _ := serverEnv(t)
	srv.Put("Archive", 112, 101, 102) // UIDVALIDITY changed since mbsync's state
	res, err := c.Trash(refs("k1"))
	if err != nil || res.Server != 0 || res.Moved != 1 || len(res.Notes) != 1 {
		t.Fatalf("stale: res = %+v err = %v", res, err)
	}

	c2, srv2, root2 := serverEnv(t)
	r := c2.Server.(imap.Remote)
	r.PassCmd = []string{"echo", "wrong"}
	c2.Server = r
	res, err = c2.Trash(refs("k1", "k3"))
	if err == nil || res.Moved != 0 {
		t.Fatalf("login failure should abort before any move: res = %+v err = %v", res, err)
	}
	if len(srv2.UIDs("Archive")) != 3 {
		t.Error("server changed despite failed login")
	}
	if _, err := os.Stat(filepath.Join(root2, "Archive/cur/k3:2,S")); err != nil {
		t.Error("local move happened after the server step failed")
	}
}
