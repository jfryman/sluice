package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFolderGroups(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "Mail")
	n := 0
	put := func(folder, from, listID, flags string) {
		os.MkdirAll(filepath.Join(root, folder, "cur"), 0o700)
		n++
		hdr := "From: Promo Co <" + from + ">\r\nSubject: s\r\nDate: Mon, 21 Sep 2026 10:00:00 +0000\r\n"
		if listID != "" {
			hdr += "List-Id: <" + listID + ">\r\n"
		}
		os.WriteFile(filepath.Join(root, folder, "cur", fmt.Sprintf("k%03d:2,%s", n, flags)), []byte(hdr+"\r\nbody\r\n"), 0o600)
	}
	put("+SaneLater", "a@spam.example", "", "")
	put("+SaneLater", "a@spam.example", "", "S")
	put("+SaneLater", "news@deals.example", "deals.example", "")
	put("Archive", "a@spam.example", "", "S") // other folder: not counted

	ix, err := Open(filepath.Join(dir, "index.db"), root, Folders{Trash: "Deleted Messages"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.Scan(nil); err != nil {
		t.Fatal(err)
	}
	gs, err := ix.FolderGroups("+SaneLater")
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("groups = %+v", gs)
	}
	if g := gs[0]; g.Kind != KindSender || g.Value != "a@spam.example" || g.Total != 2 || g.Unread != 1 || g.Name != "Promo Co" {
		t.Errorf("sender group = %+v", g)
	}
	if g := gs[1]; g.Kind != KindList || g.Value != "deals.example" || g.Total != 1 {
		t.Errorf("list group = %+v", g)
	}
	fs, err := ix.Folders()
	if err != nil || len(fs) != 2 || fs[0].Folder != "+SaneLater" || fs[0].Count != 3 || fs[0].Unread != 2 {
		t.Errorf("folders = %+v %v", fs, err)
	}
}
