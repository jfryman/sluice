package sieve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeServer installs a sieve-connect stub on PATH backed by a directory:
// each file in dir/scripts is a server script, dir/active names the active
// one. It logs argv and the fd-3 password, and --checkscript rejects "BAD".
type fakeServer struct{ dir, log string }

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	dir := t.TempDir()
	f := &fakeServer{dir: dir, log: filepath.Join(dir, "log")}
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o700)
	os.MkdirAll(filepath.Join(dir, "bin"), 0o700)
	script := `#!/bin/bash
pw=$(head -n1 <&3)
echo "pw=$pw $*" >> ` + f.log + `
D=` + dir + `
local=""; remote=""; prev=""; action=""
for a in "$@"; do
  case "$prev" in --localsieve) local="$a";; --remotesieve) remote="$a";; esac
  case "$a" in --list|--download|--upload|--checkscript|--activate) action="$a";; esac
  prev="$a"
done
active=$(cat "$D/active" 2>/dev/null)
case "$action" in
  --list) for s in "$D"/scripts/*; do [ -e "$s" ] || continue; n=$(basename "$s")
            if [ "$n" = "$active" ]; then echo "\"$n\" ACTIVE"; else echo "\"$n\""; fi; done ;;
  --download) [ -f "$D/scripts/$remote" ] || { echo "NO script not found"; exit 1; }; cp "$D/scripts/$remote" "$local" ;;
  --checkscript) grep -q BAD "$local" && { echo "script error"; exit 1; } ;;
  --upload) cp "$local" "$D/scripts/$remote" ;;
  --activate) echo "$remote" > "$D/active" ;;
esac
exit 0
`
	os.WriteFile(filepath.Join(dir, "bin", "sieve-connect"), []byte(script), 0o755)
	t.Setenv("PATH", filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
	return f
}

func (f *fakeServer) put(name, text string, active bool) {
	os.WriteFile(filepath.Join(f.dir, "scripts", name), []byte(text), 0o600)
	if active {
		os.WriteFile(filepath.Join(f.dir, "active"), []byte(name+"\n"), 0o600)
	}
}

func (f *fakeServer) script(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "scripts", name))
	return string(b)
}

func (f *fakeServer) active() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "active"))
	return strings.TrimSpace(string(b))
}

func testRemote() Remote {
	return Remote{Server: "srv", Script: "kolab", UserCmd: []string{"echo", "me"}, PassCmd: []string{"echo", "s3cret"}}
}

func localFile(t *testing.T, text string) string {
	p := filepath.Join(t.TempDir(), "kolab.sieve")
	os.WriteFile(p, []byte(text), 0o644)
	return p
}

func TestParseList(t *testing.T) {
	st := parseList("\"kolab\" ACTIVE\n\"roundcube\"\n\"with \\\"quote\\\"\"\nnoise\n", "kolab")
	if !st.Exists || st.Active != "kolab" || len(st.Scripts) != 3 || st.Scripts[2] != `with "quote"` {
		t.Fatalf("parseList = %+v", st)
	}
	if st := parseList("", "kolab"); st.Exists || st.Active != "" || len(st.Scripts) != 0 {
		t.Fatalf("empty = %+v", st)
	}
}

func TestPublishNormal(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "keep;  \n\n", true)
	local := localFile(t, "keep;\n")
	if _, err := testRemote().Publish(local, "new;\n", PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(local); string(b) != "new;\n" || f.script("kolab") != "new;\n" || f.active() != "kolab" {
		t.Fatalf("local=%q remote=%q active=%q", b, f.script("kolab"), f.active())
	}
	log, _ := os.ReadFile(f.log)
	for _, want := range []string{"pw=s3cret --server srv --user me --passwordfd 3 --list", "--checkscript", "--upload", "--activate --remotesieve kolab"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if strings.Count(string(log), "s3cret") != strings.Count(string(log), "pw=s3cret") {
		t.Error("password leaked into argv")
	}
}

func TestPublishFirstScript(t *testing.T) {
	f := newFakeServer(t) // no scripts at all
	local := localFile(t, "require [\"fileinto\"];\nkeep;\n")
	log, err := testRemote().Publish(local, "require [\"fileinto\"];\nnew;\n", PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f.script("kolab") != "require [\"fileinto\"];\nnew;\n" || f.active() != "kolab" {
		t.Fatalf("remote=%q active=%q", f.script("kolab"), f.active())
	}
	if !strings.Contains(strings.Join(log, "|"), "creating it") {
		t.Errorf("log = %v", log)
	}
	// An inactive unrelated script doesn't block creation.
	f2 := newFakeServer(t)
	f2.put("old", "keep;\n", false)
	if _, err := testRemote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{}); err != nil {
		t.Fatalf("inactive other script blocked: %v", err)
	}
}

func TestPublishGuards(t *testing.T) {
	// Ours missing, another active: never switch it off.
	f := newFakeServer(t)
	f.put("roundcube", "keep;\n", true)
	local := localFile(t, "keep;\n")
	_, err := testRemote().Publish(local, "new;\n", PublishOptions{AllowSwitchFrom: "roundcube"})
	var oa ErrOtherActive
	if !errors.As(err, &oa) || oa.Active != "roundcube" {
		t.Fatalf("want ErrOtherActive, got %v", err)
	}
	if f.active() != "roundcube" || f.script("kolab") != "" {
		t.Fatal("server changed despite guard")
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatal("local changed despite guard")
	}

	// Ours exists but another active: blocked unless that switch was confirmed.
	f = newFakeServer(t)
	f.put("kolab", "keep;\n", false)
	f.put("roundcube", "keep;\n", true)
	if _, err := testRemote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{}); !errors.As(err, &oa) {
		t.Fatalf("unconfirmed switch: %v", err)
	}
	if _, err := testRemote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{AllowSwitchFrom: "other"}); !errors.As(err, &oa) {
		t.Fatalf("switch confirmed for a different script: %v", err)
	}
	if _, err := testRemote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{AllowSwitchFrom: "roundcube"}); err != nil {
		t.Fatalf("confirmed switch: %v", err)
	}
	if f.active() != "kolab" {
		t.Fatalf("active = %q", f.active())
	}
}

func TestPublishDriftAndReject(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "something else;\n", true)
	local := localFile(t, "keep;\n")
	if _, err := testRemote().Publish(local, "new;\n", PublishOptions{}); !errors.Is(err, ErrDrift) {
		t.Fatalf("want drift, got %v", err)
	}

	f.put("kolab", "keep;\n", true)
	if _, err := testRemote().Publish(local, "BAD;\n", PublishOptions{}); err == nil {
		t.Fatal("want checkscript rejection")
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatalf("local not restored: %q", b)
	}
}

func TestStatus(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "keep;\n", false)
	f.put("roundcube", "x;\n", true)
	st, err := testRemote().Status(localFile(t, "other;\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || !st.Drift || st.Active != "roundcube" || st.OtherActive("kolab") != "roundcube" {
		t.Fatalf("status = %+v", st)
	}
}

func TestFilePublisherFirstScript(t *testing.T) {
	dir := t.TempDir()
	fp := FilePublisher{Dir: filepath.Join(dir, "remote"), Script: "kolab"}
	local := localFile(t, "keep;\n")
	if st, _ := fp.Status(local); st.Exists {
		t.Fatal("empty sandbox remote reported existing")
	}
	if _, err := fp.Publish(local, "new;\n", PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := fp.Status(local); !st.Exists || st.Drift || st.Active != "kolab" {
		t.Fatalf("after publish: %+v", st)
	}
}
