// Package imaptest is an in-process IMAP server for tests: implicit TLS
// with a throwaway certificate, AUTHENTICATE PLAIN, CAPABILITY,
// SELECT/EXAMINE, UID MOVE / COPY / STORE / EXPUNGE with COPYUID, LOGOUT.
// Mailboxes hold UIDs only.
package imaptest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type Mailbox struct {
	Validity uint32
	UIDs     map[uint32]bool
	Next     uint32
}

type Server struct {
	Addr      string
	ClientTLS *tls.Config
	User      string
	Pass      string

	mu        sync.Mutex
	boxes     map[string]*Mailbox
	noMove    bool
	noUIDPlus bool
	Logins    int
	Commands  []string // command verbs seen (no arguments: AUTHENTICATE carries the password)
}

// New starts a server with user "me" / "s3cret". Options: "nomove" (no MOVE
// capability), "nouidplus".
func New(t *testing.T, opts ...string) *Server {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "imap.test"},
		DNSNames: []string{"imap.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	srvTLS := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: ln.Addr().String(), ClientTLS: &tls.Config{RootCAs: pool, ServerName: "imap.test"},
		User: "me", Pass: "s3cret", boxes: map[string]*Mailbox{}}
	for _, o := range opts {
		switch o {
		case "nomove":
			s.noMove = true
		case "nouidplus":
			s.noUIDPlus = true
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

// Put creates or replaces a mailbox with the given UIDs.
func (s *Server) Put(name string, validity uint32, uids ...uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &Mailbox{Validity: validity, UIDs: map[uint32]bool{}, Next: 1}
	for _, u := range uids {
		b.UIDs[u] = true
		b.Next = max(b.Next, u+1)
	}
	s.boxes[name] = b
}

// UIDs lists a mailbox's UIDs, sorted.
func (s *Server) UIDs(name string) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint32
	if b := s.boxes[name]; b != nil {
		for u := range b.UIDs {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HostPort splits Addr for a client config.
func (s *Server) HostPort() (string, int) {
	h, p, _ := net.SplitHostPort(s.Addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func (s *Server) caps() string {
	c := "IMAP4rev1 LITERAL+ AUTH=PLAIN SASL-IR"
	if !s.noUIDPlus {
		c += " UIDPLUS"
	}
	if !s.noMove {
		c += " MOVE"
	}
	return c
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	out := func(format string, a ...any) { fmt.Fprintf(w, format+"\r\n", a...) }
	out("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] imaptest ready")
	w.Flush()
	authed, selected := false, ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		args := tokens(rest)
		if len(args) == 0 {
			out("%s BAD empty", tag)
			w.Flush()
			continue
		}
		verb := strings.ToUpper(args[0])
		if verb == "UID" && len(args) > 1 {
			verb += " " + strings.ToUpper(args[1])
			args = args[1:]
		}
		s.mu.Lock()
		s.Commands = append(s.Commands, verb)
		switch {
		case verb == "AUTHENTICATE" && len(args) == 3:
			raw, _ := base64.StdEncoding.DecodeString(args[2])
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.User && parts[2] == s.Pass {
				authed = true
				s.Logins++
				out("%s OK [CAPABILITY %s] logged in", tag, s.caps())
			} else {
				out("%s NO [AUTHENTICATIONFAILED] invalid credentials", tag)
			}
		case verb == "CAPABILITY":
			if authed {
				out("* CAPABILITY %s", s.caps())
			} else {
				out("* CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR")
			}
			out("%s OK done", tag)
		case verb == "LOGOUT":
			out("* BYE bye")
			out("%s OK done", tag)
			s.mu.Unlock()
			w.Flush()
			return
		case !authed:
			out("%s NO not authenticated", tag)
		case (verb == "SELECT" || verb == "EXAMINE") && len(args) == 2:
			if b := s.boxes[args[1]]; b != nil {
				selected = args[1]
				out("* %d EXISTS", len(b.UIDs))
				out("* OK [UIDVALIDITY %d] ok", b.Validity)
				out("%s OK [READ-WRITE] selected", tag)
			} else {
				out("%s NO [NONEXISTENT] no such mailbox", tag)
			}
		case (verb == "UID MOVE" && !s.noMove || verb == "UID COPY") && len(args) == 3:
			src, dst := s.boxes[selected], s.boxes[args[2]]
			if src == nil || dst == nil {
				out("%s NO [TRYCREATE] no such mailbox", tag)
				break
			}
			var from, to []uint32
			for _, u := range parseSet(args[1]) {
				if src.UIDs[u] {
					from = append(from, u)
					to = append(to, dst.Next)
					dst.UIDs[dst.Next] = true
					dst.Next++
					if verb == "UID MOVE" {
						delete(src.UIDs, u)
					}
				}
			}
			code := ""
			if len(from) > 0 && !s.noUIDPlus {
				code = fmt.Sprintf("[COPYUID %d %s %s] ", dst.Validity, join(from), join(to))
			}
			if verb == "UID MOVE" {
				if code != "" {
					out("* OK %smoved", code)
				}
				out("%s OK done", tag)
			} else {
				out("%s OK %scopied", tag, code)
			}
		case verb == "UID STORE":
			out("%s OK stored", tag)
		case verb == "UID EXPUNGE" && len(args) == 2 && !s.noUIDPlus:
			if b := s.boxes[selected]; b != nil {
				for _, u := range parseSet(args[1]) {
					delete(b.UIDs, u)
				}
			}
			out("%s OK expunged", tag)
		default:
			out("%s BAD unsupported %s", tag, verb)
		}
		s.mu.Unlock()
		w.Flush()
	}
}

// tokens splits on spaces, honouring "quoted strings" (with \" escapes).
func tokens(s string) []string {
	var out []string
	var b strings.Builder
	inQ, esc, have := false, false, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			esc = false
		case inQ && r == '\\':
			esc = true
		case r == '"':
			inQ, have = !inQ, true
		case r == ' ' && !inQ:
			if have {
				out = append(out, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, b.String())
	}
	return out
}

func parseSet(s string) []uint32 {
	var out []uint32
	for _, p := range strings.Split(s, ",") {
		a, b, rng := strings.Cut(p, ":")
		lo, _ := strconv.ParseUint(a, 10, 32)
		hi := lo
		if rng {
			hi, _ = strconv.ParseUint(b, 10, 32)
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		for u := lo; u <= hi && lo > 0; u++ {
			out = append(out, uint32(u))
		}
	}
	return out
}

func join(u []uint32) string {
	parts := make([]string, len(u))
	for i, x := range u {
		parts[i] = strconv.FormatUint(uint64(x), 10)
	}
	return strings.Join(parts, ",")
}
