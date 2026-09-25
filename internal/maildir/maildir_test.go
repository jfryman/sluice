package maildir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseName(t *testing.T) {
	tests := []struct {
		in    string
		want  Name
		moved string
	}{
		{"1788.12_10.voyager,U=10:2,RS", Name{"1788.12_10.voyager", "10", "RS"}, "1788.12_10.voyager:2,RS"},
		{"1788.12_10.voyager:2,", Name{"1788.12_10.voyager", "", ""}, "1788.12_10.voyager:2,"},
		{"1788.12_10.voyager", Name{"1788.12_10.voyager", "", ""}, "1788.12_10.voyager:2,"},
		{"k,U=7", Name{"k", "7", ""}, "k:2,"},
	}
	for _, tt := range tests {
		got := ParseName(tt.in)
		if got != tt.want {
			t.Errorf("ParseName(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
		if got.MovedName() != tt.moved {
			t.Errorf("MovedName(%q) = %q, want %q", tt.in, got.MovedName(), tt.moved)
		}
	}
}

func TestListID(t *testing.T) {
	for in, want := range map[string]string{
		`"Foo News" <News.Foo.COM>`: "news.foo.com",
		"bare.list.id":              "bare.list.id",
		"":                          "",
	} {
		if got := ListID(in); got != want {
			t.Errorf("ListID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadHeadersAndMove(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"Archive", "Deleted Messages"} {
		for _, s := range []string{"cur", "new", "tmp"} {
			os.MkdirAll(filepath.Join(root, f, s), 0o700)
		}
	}
	src := filepath.Join(root, "Archive", "cur", "k1,U=5:2,S")
	os.WriteFile(src, []byte("From: \"Shop\" <Deals@Shop.Example>\r\nSubject: =?UTF-8?Q?Big_sale?=\r\nList-Id: Deals <deals.shop.example>\r\nList-Unsubscribe: <mailto:x>\r\nDate: Mon, 02 Jan 2023 15:04:05 +0000\r\n\r\nbody\r\n"), 0o600)

	h, err := ReadHeaders(src)
	if err != nil {
		t.Fatal(err)
	}
	if h.FromAddr != "deals@shop.example" || h.FromName != "Shop" || h.ListID != "deals.shop.example" || h.Subject != "Big sale" || !h.Unsub || h.Date.Year() != 2023 {
		t.Fatalf("unexpected headers %+v", h)
	}

	folders, _ := Folders(root)
	if len(folders) != 2 {
		t.Fatalf("folders = %v", folders)
	}

	dst, err := Move(root, src, "Deleted Messages")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dst) != "k1:2,S" {
		t.Fatalf("moved to %s", dst)
	}
	if p, err := FindKey(root, "Deleted Messages", "k1"); err != nil || p != dst {
		t.Fatalf("FindKey = %s, %v", p, err)
	}
}
