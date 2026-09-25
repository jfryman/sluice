package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/query"
)

func mkdirs(t *testing.T, root string, folders ...string) {
	for _, f := range folders {
		for _, s := range []string{"cur", "new", "tmp"} {
			if err := os.MkdirAll(filepath.Join(root, f, s), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func msg(t *testing.T, path, from, list string, date time.Time) {
	body := fmt.Sprintf("From: %s\r\nList-Id: <%s>\r\nSubject: hi\r\nDate: %s\r\n\r\nx\r\n", from, list, date.Format(time.RFC1123Z))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanGroupsTrashUndo(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "Archive", "+SaneBlackHole", "Deleted Messages", "Sent Messages")
	now := time.Now()
	msg(t, filepath.Join(root, "Archive/cur/a1,U=1:2,S"), "news@spam.example", "promo.spam.example", now.AddDate(0, 0, -5))
	msg(t, filepath.Join(root, "Archive/cur/a2,U=2:2,"), "news@spam.example", "promo.spam.example", now.AddDate(-2, 0, 0))
	msg(t, filepath.Join(root, "+SaneBlackHole/new/b1"), "news@spam.example", "promo.spam.example", now.AddDate(0, 0, -1))
	msg(t, filepath.Join(root, "Archive/cur/c1,U=3:2,S"), "friend@good.example", "", now.AddDate(0, 0, -3))
	msg(t, filepath.Join(root, "Sent Messages/cur/s1,U=1:2,S"), "me@me.example", "", now)

	ix, err := index.Open(filepath.Join(t.TempDir(), "i.db"), root, index.Folders{
		Trash: "Deleted Messages", BlackHole: []string{"+SaneBlackHole"}, News: []string{"+SaneNews"},
		Exclude: []string{"Sent Messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	st, err := ix.Scan(nil)
	if err != nil || st.Added != 4 {
		t.Fatalf("scan = %+v, %v", st, err)
	}
	gs, err := ix.Groups(index.KindDomain, now)
	if err != nil || len(gs) != 2 {
		t.Fatalf("groups = %+v, %v", gs, err)
	}
	var spam, good index.Group
	for _, g := range gs {
		if g.Value == "spam.example" {
			spam = g
		} else {
			good = g
		}
	}
	if spam.Total != 3 || spam.Recent90 != 2 || spam.Unread != 2 || spam.BlackHole != 1 {
		t.Fatalf("spam group = %+v", spam)
	}
	if spam.Score <= good.Score {
		t.Fatalf("spam score %v <= good score %v", spam.Score, good.Score)
	}

	q, _ := query.Parse("list:promo.spam.example older:1y")
	found, total, err := ix.Search(q, 0)
	if err != nil || total != 1 || found[0].Key != "a2" {
		t.Fatalf("search = %+v %d %v", found, total, err)
	}

	c := &Cleaner{Index: ix, Root: root, TrashFolder: "Deleted Messages",
		LockFile: filepath.Join(t.TempDir(), "lock"), JournalPath: filepath.Join(t.TempDir(), "j.jsonl")}
	res, err := c.Trash(found)
	if err != nil || res.Moved != 1 {
		t.Fatalf("trash = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Deleted Messages/cur/a2:2,")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}

	// Simulate mbsync assigning a new UID after upload.
	os.Rename(filepath.Join(root, "Deleted Messages/cur/a2:2,"), filepath.Join(root, "Deleted Messages/cur/a2,U=99:2,"))

	un, err := c.Undo()
	if err != nil || un.Moved != 1 {
		t.Fatalf("undo = %+v, %v", un, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Archive/cur/a2:2,")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if _, err := c.Undo(); err == nil {
		t.Fatal("second undo should have nothing to do")
	}

	st, err = ix.Scan(nil)
	if err != nil || st.Added != 0 || st.Removed != 0 {
		t.Fatalf("rescan = %+v, %v", st, err)
	}
}
