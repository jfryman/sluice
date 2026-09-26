package sieve

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jfryman/sluice/internal/timing"
)

// fakeServer is an in-process ManageSieve server: STARTTLS with a throwaway
// certificate, SASL PLAIN, and a script store. CHECKSCRIPT/PUTSCRIPT reject
// scripts containing "BAD".
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	srvTLS   *tls.Config
	cliTLS   *tls.Config
	user     string
	pass     string
	noTLS    atomic.Bool
	noVer    atomic.Bool
	mu       sync.Mutex
	scripts  map[string]string
	active   string
	logins   int
	sessions int
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sieve.test"},
		DNSNames: []string{"sieve.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{t: t, ln: ln, user: "me", pass: "s3cret", scripts: map[string]string{},
		srvTLS: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		cliTLS: &tls.Config{RootCAs: pool, ServerName: "sieve.test"}}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeServer) remote() Remote {
	_, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return Remote{Server: "127.0.0.1", Port: p, Script: "kolab", User: f.user,
		PassCmd: []string{"echo", f.pass}, TLS: f.cliTLS, Timeout: 5 * time.Second}
}

func (f *fakeServer) put(name, text string, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[name] = text
	if active {
		f.active = name
	}
}

func (f *fakeServer) state() (map[string]string, string, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]string{}
	for k, v := range f.scripts {
		cp[k] = v
	}
	return cp, f.active, f.logins, f.sessions
}

func (f *fakeServer) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeServer) greet(w *bufio.Writer, tlsDone bool) {
	fmt.Fprint(w, "\"IMPLEMENTATION\" \"fake\"\r\n\"SASL\" \"PLAIN\"\r\n\"SIEVE\" \"fileinto imap4flags\"\r\n")
	if !f.noVer.Load() {
		fmt.Fprint(w, "\"VERSION\" \"1.0\"\r\n")
	}
	if !tlsDone && !f.noTLS.Load() {
		fmt.Fprint(w, "\"STARTTLS\"\r\n")
	}
	fmt.Fprint(w, "OK\r\n")
	w.Flush()
}

func (f *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	f.mu.Lock()
	f.sessions++
	f.mu.Unlock()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	f.greet(w, false)
	authed := false
	for {
		toks, err := readTokens(r)
		if err != nil || len(toks) == 0 {
			return
		}
		ok := func(extra string) { fmt.Fprint(w, extra+"OK\r\n"); w.Flush() }
		no := func(code, msg string) { fmt.Fprintf(w, "NO (%s) %q\r\n", code, msg); w.Flush() }
		cmd := strings.ToUpper(toks[0])
		if !authed && cmd != "STARTTLS" && cmd != "AUTHENTICATE" && cmd != "LOGOUT" {
			no("AUTH", "authenticate first")
			continue
		}
		f.mu.Lock()
		switch cmd {
		case "STARTTLS":
			ok("")
			tc := tls.Server(conn, f.srvTLS)
			if err := tc.Handshake(); err != nil {
				f.mu.Unlock()
				return
			}
			conn, r, w = tc, bufio.NewReader(tc), bufio.NewWriter(tc)
			f.greet(w, true)
		case "AUTHENTICATE":
			raw, _ := base64.StdEncoding.DecodeString(toks[2])
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == f.user && parts[2] == f.pass {
				authed = true
				f.logins++
				ok("")
			} else {
				no("AUTH", "authentication failed")
			}
		case "LISTSCRIPTS":
			var b strings.Builder
			for name := range f.scripts {
				b.WriteString(quote(name))
				if name == f.active {
					b.WriteString(" ACTIVE")
				}
				b.WriteString("\r\n")
			}
			ok(b.String())
		case "GETSCRIPT":
			if s, found := f.scripts[toks[1]]; found {
				ok(fmt.Sprintf("{%d}\r\n%s\r\n", len(s), s))
			} else {
				no("NONEXISTENT", "no such script")
			}
		case "CHECKSCRIPT":
			if strings.Contains(toks[1], "BAD") {
				no("", "line 1: syntax error")
			} else {
				ok("")
			}
		case "PUTSCRIPT":
			if strings.Contains(toks[2], "BAD") {
				no("", "line 1: syntax error")
			} else {
				f.scripts[toks[1]] = toks[2]
				ok("")
			}
		case "SETACTIVE":
			f.active = toks[1]
			ok("")
		case "LOGOUT":
			ok("")
			f.mu.Unlock()
			return
		default:
			no("", "unknown command")
		}
		f.mu.Unlock()
	}
}

func localFile(t *testing.T, text string) string {
	p := filepath.Join(t.TempDir(), "kolab.sieve")
	os.WriteFile(p, []byte(text), 0o644)
	return p
}

func TestReadTokens(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\"kolab\" ACTIVE\r\n{5}\r\nab\r\nc\r\nNO (NONEXISTENT) \"gone\"\r\n\"with \\\"quote\\\"\"\r\n"))
	want := [][]string{{"kolab", "ACTIVE"}, {"ab\r\nc"}, {"NO", "(NONEXISTENT)", "gone"}, {`with "quote"`}}
	for i, w := range want {
		got, err := readTokens(r)
		if err != nil || strings.Join(got, "|") != strings.Join(w, "|") {
			t.Fatalf("line %d = %q, %v; want %q", i, got, err, w)
		}
	}
}

func TestPublishNormalOneSession(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "keep;  \n\n", true)
	local := localFile(t, "keep;\n")
	rec := &timing.Recorder{}
	r := f.remote()
	r.Trace = rec
	if _, err := r.Publish(local, "new;\n", PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	scripts, active, logins, sessions := f.state()
	if b, _ := os.ReadFile(local); string(b) != "new;\n" || scripts["kolab"] != "new;\n" || active != "kolab" {
		t.Fatalf("local=%q remote=%q active=%q", b, scripts["kolab"], active)
	}
	if logins != 1 || sessions != 1 {
		t.Fatalf("logins=%d sessions=%d, want 1/1", logins, sessions)
	}
	var names []string
	for _, s := range rec.Stages() {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "pass,connect,starttls,auth,list,get,check,put,setactive" {
		t.Errorf("stages = %s", got)
	}
}

func TestPublishFirstScript(t *testing.T) {
	f := newFakeServer(t) // no scripts at all
	local := localFile(t, "require [\"fileinto\"];\nkeep;\n")
	log, err := f.remote().Publish(local, "require [\"fileinto\"];\nnew;\n", PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scripts, active, _, _ := f.state()
	if scripts["kolab"] != "require [\"fileinto\"];\nnew;\n" || active != "kolab" {
		t.Fatalf("remote=%q active=%q", scripts["kolab"], active)
	}
	if !strings.Contains(strings.Join(log, "|"), "creating it") {
		t.Errorf("log = %v", log)
	}
	f2 := newFakeServer(t)
	f2.put("old", "keep;\n", false) // inactive unrelated script doesn't block
	if _, err := f2.remote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{}); err != nil {
		t.Fatalf("inactive other script blocked: %v", err)
	}
}

func TestPublishGuards(t *testing.T) {
	f := newFakeServer(t)
	f.put("roundcube", "keep;\n", true)
	local := localFile(t, "keep;\n")
	_, err := f.remote().Publish(local, "new;\n", PublishOptions{AllowSwitchFrom: "roundcube"})
	var oa ErrOtherActive
	if !errors.As(err, &oa) || oa.Active != "roundcube" {
		t.Fatalf("want ErrOtherActive, got %v", err)
	}
	scripts, active, _, _ := f.state()
	if active != "roundcube" || scripts["kolab"] != "" {
		t.Fatal("server changed despite guard")
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatal("local changed despite guard")
	}

	f = newFakeServer(t)
	f.put("kolab", "keep;\n", false)
	f.put("roundcube", "keep;\n", true)
	if _, err := f.remote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{}); !errors.As(err, &oa) {
		t.Fatalf("unconfirmed switch: %v", err)
	}
	if _, err := f.remote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{AllowSwitchFrom: "other"}); !errors.As(err, &oa) {
		t.Fatalf("switch confirmed for a different script: %v", err)
	}
	if _, err := f.remote().Publish(localFile(t, "keep;\n"), "new;\n", PublishOptions{AllowSwitchFrom: "roundcube"}); err != nil {
		t.Fatalf("confirmed switch: %v", err)
	}
	if _, active, _, _ := f.state(); active != "kolab" {
		t.Fatalf("active = %q", active)
	}
}

func TestPublishDriftAndReject(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "something else;\n", true)
	local := localFile(t, "keep;\n")
	if _, err := f.remote().Publish(local, "new;\n", PublishOptions{}); !errors.Is(err, ErrDrift) {
		t.Fatalf("want drift, got %v", err)
	}
	f.put("kolab", "keep;\n", true)
	if _, err := f.remote().Publish(local, "BAD;\n", PublishOptions{}); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("want checkscript rejection, got %v", err)
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatalf("local not restored: %q", b)
	}
	// Without VERSION there is no CHECKSCRIPT; PUTSCRIPT still rejects.
	f.noVer.Store(true)
	if _, err := f.remote().Publish(local, "BAD;\n", PublishOptions{}); err == nil {
		t.Fatal("want putscript rejection")
	}
	if b, _ := os.ReadFile(local); string(b) != "keep;\n" {
		t.Fatalf("local not restored after putscript: %q", b)
	}
}

func TestStatusAndAuth(t *testing.T) {
	f := newFakeServer(t)
	f.put("kolab", "keep;\n", false)
	f.put("roundcube", "x;\n", true)
	st, err := f.remote().Status(localFile(t, "other;\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || !st.Drift || st.Active != "roundcube" || len(st.Scripts) != 2 {
		t.Fatalf("status = %+v", st)
	}
	r := f.remote()
	r.PassCmd = []string{"echo", "wrong"}
	if _, err := r.Status(localFile(t, "")); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("want auth failure, got %v", err)
	}
	r = f.remote()
	r.User, r.UserCmd = "", []string{"echo", "me"}
	if _, err := r.Status(localFile(t, "")); err != nil {
		t.Fatalf("user_cmd path: %v", err)
	}
}

func TestRefusesPlaintext(t *testing.T) {
	f := newFakeServer(t)
	f.noTLS.Store(true)
	if _, err := f.remote().Status(localFile(t, "")); !errors.Is(err, ErrNoTLS) {
		t.Fatalf("want ErrNoTLS, got %v", err)
	}
	if _, _, logins, _ := f.state(); logins != 0 {
		t.Fatal("credentials sent without TLS")
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
