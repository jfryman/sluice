package sieve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSieveConnect installs a sieve-connect stub on PATH. It logs argv and
// the fd-3 password, serves `remote` on --download, and fails --checkscript
// when the uploaded file contains "BAD".
func fakeSieveConnect(t *testing.T, remote string) (logPath string) {
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	remoteFile := filepath.Join(dir, "remote")
	os.WriteFile(remoteFile, []byte(remote), 0o600)
	script := `#!/bin/bash
pw=$(head -n1 <&3)
echo "pw=$pw $*" >> ` + logPath + `
local=""; prev=""
for a in "$@"; do [ "$prev" = "--localsieve" ] && local="$a"; prev="$a"; done
case "$*" in
  *--download*) cp ` + remoteFile + ` "$local" ;;
  *--checkscript*) grep -q BAD "$local" && { echo "script error"; exit 1; } ;;
esac
exit 0
`
	os.WriteFile(filepath.Join(dir, "sieve-connect"), []byte(script), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logPath
}

func TestPublish(t *testing.T) {
	local := filepath.Join(t.TempDir(), "kolab.sieve")
	os.WriteFile(local, []byte("keep;\n"), 0o644)
	logPath := fakeSieveConnect(t, "keep;  \n\n")
	r := Remote{Server: "srv", Script: "kolab", UserCmd: []string{"echo", "me"}, PassCmd: []string{"echo", "s3cret"}}

	if _, err := r.Publish(local, "new;\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(local)
	if string(b) != "new;\n" {
		t.Fatalf("local = %q", b)
	}
	log, _ := os.ReadFile(logPath)
	for _, want := range []string{"pw=s3cret --server srv --user me --passwordfd 3 --download", "--checkscript", "--upload", "--activate --remotesieve kolab"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Count(string(log), "s3cret") != strings.Count(string(log), "pw=s3cret") {
		t.Error("password leaked into argv")
	}
}

func TestPublishDriftAndReject(t *testing.T) {
	local := filepath.Join(t.TempDir(), "kolab.sieve")
	os.WriteFile(local, []byte("keep;\n"), 0o644)
	r := Remote{Server: "srv", Script: "kolab", UserCmd: []string{"echo", "me"}, PassCmd: []string{"echo", "pw"}}

	fakeSieveConnect(t, "something else;\n")
	if _, err := r.Publish(local, "new;\n"); !errors.Is(err, ErrDrift) {
		t.Fatalf("want drift, got %v", err)
	}

	fakeSieveConnect(t, "keep;\n")
	if _, err := r.Publish(local, "BAD;\n"); err == nil {
		t.Fatal("want checkscript rejection")
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatalf("local not restored: %q", b)
	}
}
