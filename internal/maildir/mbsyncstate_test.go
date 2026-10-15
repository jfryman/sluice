package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFarUIDs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Archive")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, ".mbsyncstate"), []byte(
		"FarUidValidity 1467142788\nNearUidValidity 1788208369\nMaxPulledUid 102545\n\n"+
			"1 1 S\n5 4 FS\n0 9 \n12 0 \n-1 7 \n266157 579 S\n"), 0o600)
	v, m, err := FarUIDs(root, "Archive")
	if err != nil || v != 1467142788 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	want := map[uint32]uint32{1: 1, 4: 5, 579: 266157}
	if len(m) != len(want) {
		t.Fatalf("map = %v", m)
	}
	for n, f := range want {
		if m[n] != f {
			t.Errorf("near %d -> %d, want %d", n, m[n], f)
		}
	}

	os.WriteFile(filepath.Join(dir, ".mbsyncstate.journal"), []byte("+ 0 13\n"), 0o600)
	if _, _, err := FarUIDs(root, "Archive"); !errors.Is(err, ErrStatePending) {
		t.Errorf("journal present: err = %v", err)
	}
	if _, _, err := FarUIDs(root, "Missing"); err == nil {
		t.Error("missing folder: expected error")
	}
	os.MkdirAll(filepath.Join(root, "Bad"), 0o700)
	os.WriteFile(filepath.Join(root, "Bad", ".mbsyncstate"), []byte("NearUidValidity 1\n\n1 1 S\n"), 0o600)
	if _, _, err := FarUIDs(root, "Bad"); err == nil {
		t.Error("no FarUidValidity: expected error")
	}
}

func TestNearUID(t *testing.T) {
	for in, want := range map[string]uint32{
		"1788208371.122291_10.voyager,U=10:2,S": 10,
		"k1:2,S":                                0,
		"k2,U=579:2,":                           579,
	} {
		if got := NearUID(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}
