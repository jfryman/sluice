// Package imap is a minimal IMAP4rev1 client for server-side trash moves:
// implicit TLS, AUTHENTICATE PLAIN (SASL-IR when offered), CAPABILITY,
// SELECT/EXAMINE, UID MOVE (RFC 6851) with UIDPLUS COPYUID (RFC 4315), a
// UID COPY + STORE + UID EXPUNGE fallback, LOGOUT. It never logs protocol
// traffic (the AUTHENTICATE line carries the password). See
// lode/mail/server-moves.md.
package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jfryman/sluice/internal/timing"
)

type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	tag     int
	caps    map[string]bool
	timeout time.Duration
	trace   *timing.Recorder
}

// ServerError is a tagged NO or BAD (or an untagged BYE).
type ServerError struct {
	Cmd, Status, Text string
}

func (e *ServerError) Error() string { return e.Cmd + ": " + e.Status + " " + e.Text }

// CopyUID is a UIDPLUS COPYUID response: src[i] became dst[i] in a
// mailbox whose UIDVALIDITY is Validity.
type CopyUID struct {
	Validity uint32
	Src, Dst []uint32
}

type response struct {
	untagged []string // without the leading "* "
	status   string   // OK | NO | BAD
	text     string   // tagged response text (with any [code])
}

// Dial connects with implicit TLS (port 993) and reads the greeting.
// tlsCfg.ServerName must be set.
func Dial(ctx context.Context, addr string, tlsCfg *tls.Config, timeout time.Duration, trace *timing.Recorder) (*Client, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	done := trace.Time("connect")
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	done()
	if err != nil {
		return nil, err
	}
	done = trace.Time("tls")
	tc := tls.Client(raw, tlsCfg)
	tc.SetDeadline(time.Now().Add(timeout))
	err = tc.HandshakeContext(ctx)
	done()
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	c := &Client{conn: tc, r: bufio.NewReader(tc), timeout: timeout, trace: trace, caps: map[string]bool{}}
	c.conn.SetDeadline(time.Now().Add(timeout))
	greet, err := c.readLine()
	if err != nil {
		tc.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if !strings.HasPrefix(greet, "* OK") {
		tc.Close()
		return nil, &ServerError{Cmd: "greeting", Status: "not OK", Text: greet}
	}
	c.parseCapsCode(greet)
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Has reports whether the server advertised a capability (case-insensitive).
func (c *Client) Has(name string) bool { return c.caps[strings.ToUpper(name)] }

// Caps lists the advertised capabilities, sorted.
func (c *Client) Caps() []string {
	out := make([]string, 0, len(c.caps))
	for k := range c.caps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Login authenticates with SASL PLAIN and re-reads the capabilities
// (servers advertise MOVE/UIDPLUS only after login). pass is not retained.
func (c *Client) Login(user string, pass []byte) error {
	done := c.trace.Time("auth")
	raw := make([]byte, 0, len(user)+len(pass)+2)
	raw = append(append(append(append(raw, 0), user...), 0), pass...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	var err error
	if c.Has("SASL-IR") {
		_, err = c.cmd("AUTHENTICATE", "AUTHENTICATE PLAIN "+b64)
	} else {
		_, err = c.cmdContinue("AUTHENTICATE", "AUTHENTICATE PLAIN", b64)
	}
	done()
	if err != nil {
		var se *ServerError
		if errors.As(err, &se) {
			se.Text = strings.TrimSpace(se.Text + " (check user / pass_cmd)")
		}
		return err
	}
	return c.Capability()
}

// Capability refreshes the capability list.
func (c *Client) Capability() error {
	resp, err := c.cmd("CAPABILITY", "CAPABILITY")
	if err != nil {
		return err
	}
	for _, u := range resp.untagged {
		if f := strings.Fields(u); len(f) > 0 && strings.EqualFold(f[0], "CAPABILITY") {
			c.caps = map[string]bool{}
			for _, cp := range f[1:] {
				c.caps[strings.ToUpper(cp)] = true
			}
		}
	}
	return nil
}

// Select opens folder read-write (Examine: read-only) and returns its
// UIDVALIDITY.
func (c *Client) Select(folder string) (uint32, error)  { return c.open("SELECT", folder) }
func (c *Client) Examine(folder string) (uint32, error) { return c.open("EXAMINE", folder) }

func (c *Client) open(verb, folder string) (uint32, error) {
	defer c.trace.Time("select")()
	q, err := quote(folder)
	if err != nil {
		return 0, err
	}
	resp, err := c.cmd(verb, verb+" "+q)
	if err != nil {
		return 0, err
	}
	for _, u := range append(resp.untagged, resp.text) {
		if v, ok := respCode(u, "UIDVALIDITY"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
			if err == nil {
				return uint32(n), nil
			}
		}
	}
	return 0, fmt.Errorf("%s %s: no UIDVALIDITY in response", verb, folder)
}

// UIDMove moves uids from the selected mailbox to dst. With MOVE it is one
// command; otherwise UID COPY + UID STORE \Deleted + UID EXPUNGE (UIDPLUS
// required, so only these UIDs are expunged). Returns the COPYUID mapping.
func (c *Client) UIDMove(uids []uint32, dst string) (CopyUID, error) {
	defer c.trace.Time("move")()
	q, err := quote(dst)
	if err != nil {
		return CopyUID{}, err
	}
	set := FormatSet(uids)
	if c.Has("MOVE") {
		resp, err := c.cmd("UID MOVE", "UID MOVE "+set+" "+q)
		if err != nil {
			return CopyUID{}, err
		}
		return findCopyUID(resp)
	}
	if !c.Has("UIDPLUS") {
		return CopyUID{}, errors.New("server has neither MOVE nor UIDPLUS: can't move without risking other expunges")
	}
	resp, err := c.cmd("UID COPY", "UID COPY "+set+" "+q)
	if err != nil {
		return CopyUID{}, err
	}
	cu, err := findCopyUID(resp)
	if err != nil {
		return cu, err
	}
	if len(cu.Src) == 0 {
		return cu, nil
	}
	moved := FormatSet(cu.Src) // delete only what was copied
	if _, err := c.cmd("UID STORE", "UID STORE "+moved+` +FLAGS.SILENT (\Deleted)`); err != nil {
		return cu, err
	}
	_, err = c.cmd("UID EXPUNGE", "UID EXPUNGE "+moved)
	return cu, err
}

// Logout ends the session politely.
func (c *Client) Logout() error {
	_, err := c.cmd("LOGOUT", "LOGOUT")
	return err
}

// ---- protocol ----

func (c *Client) send(line string) (string, error) {
	c.tag++
	tag := fmt.Sprintf("s%d", c.tag)
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := io.WriteString(c.conn, tag+" "+line+"\r\n"); err != nil {
		return "", err
	}
	return tag, nil
}

func (c *Client) cmd(name, line string) (response, error) {
	tag, err := c.send(line)
	if err != nil {
		return response{}, err
	}
	return c.wait(name, tag, "")
}

// cmdContinue sends line, then data after the server's "+" continuation.
func (c *Client) cmdContinue(name, line, data string) (response, error) {
	tag, err := c.send(line)
	if err != nil {
		return response{}, err
	}
	return c.wait(name, tag, data)
}

func (c *Client) wait(name, tag, cont string) (response, error) {
	var resp response
	for {
		c.conn.SetDeadline(time.Now().Add(c.timeout))
		l, err := c.readLine()
		if err != nil {
			return resp, fmt.Errorf("%s: %w", name, err)
		}
		switch {
		case strings.HasPrefix(l, "+"):
			if cont == "" {
				return resp, fmt.Errorf("%s: unexpected continuation", name)
			}
			if _, err := io.WriteString(c.conn, cont+"\r\n"); err != nil {
				return resp, err
			}
			cont = ""
		case strings.HasPrefix(l, "* "):
			u := l[2:]
			if strings.HasPrefix(strings.ToUpper(u), "BYE") && name != "LOGOUT" {
				return resp, &ServerError{Cmd: name, Status: "BYE", Text: strings.TrimSpace(u[3:])}
			}
			resp.untagged = append(resp.untagged, u)
		case strings.HasPrefix(l, tag+" "):
			st, text, _ := strings.Cut(l[len(tag)+1:], " ")
			resp.status, resp.text = strings.ToUpper(st), text
			if resp.status != "OK" {
				return resp, &ServerError{Cmd: name, Status: resp.status, Text: text}
			}
			c.parseCapsCode(text)
			return resp, nil
		}
	}
}

// readLine reads one response line, inlining any {n} / {n+} literals.
func (c *Client) readLine() (string, error) {
	var b strings.Builder
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		l = strings.TrimRight(l, "\r\n")
		b.WriteString(l)
		n, ok := literalLen(l)
		if !ok {
			return b.String(), nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return "", err
		}
		b.Write(buf)
	}
}

func literalLen(l string) (int, bool) {
	if !strings.HasSuffix(l, "}") {
		return 0, false
	}
	i := strings.LastIndexByte(l, '{')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(l[i+1:len(l)-1], "+"))
	return n, err == nil && n >= 0
}

func (c *Client) parseCapsCode(text string) {
	if v, ok := respCode(text, "CAPABILITY"); ok {
		c.caps = map[string]bool{}
		for _, cp := range strings.Fields(v) {
			c.caps[strings.ToUpper(cp)] = true
		}
	}
}

// respCode finds "[NAME args]" in a response line and returns args.
func respCode(line, name string) (string, bool) {
	i := strings.Index(strings.ToUpper(line), "["+name)
	if i < 0 {
		return "", false
	}
	rest := line[i+1+len(name):]
	j := strings.IndexByte(rest, ']')
	if j < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:j]), true
}

func findCopyUID(resp response) (CopyUID, error) {
	for _, l := range append(resp.untagged, resp.text) {
		v, ok := respCode(l, "COPYUID")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) != 3 {
			return CopyUID{}, fmt.Errorf("malformed COPYUID %q", v)
		}
		val, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			return CopyUID{}, fmt.Errorf("COPYUID validity %q", f[0])
		}
		src, err1 := ParseSet(f[1])
		dst, err2 := ParseSet(f[2])
		if err1 != nil || err2 != nil || len(src) != len(dst) {
			return CopyUID{}, fmt.Errorf("malformed COPYUID %q", v)
		}
		return CopyUID{Validity: uint32(val), Src: src, Dst: dst}, nil
	}
	// No COPYUID: nothing matched (MOVE/COPY of absent UIDs is a no-op) or
	// the server lacks UIDPLUS; callers treat it as "nothing confirmed".
	return CopyUID{}, nil
}

// quote renders an ASCII mailbox name as an IMAP quoted string. Non-ASCII
// names would need modified UTF-7; they are refused (callers fall back).
func quote(s string) (string, error) {
	for _, r := range s {
		if r > 126 || r < 32 {
			return "", fmt.Errorf("mailbox %q: only printable ASCII names are supported", s)
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

// FormatSet renders uids as a compact sequence set ("1:3,7").
func FormatSet(uids []uint32) string {
	s := append([]uint32(nil), uids...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && (s[j+1] == s[j]+1 || s[j+1] == s[j]) {
			j++
		}
		if s[i] == s[j] {
			parts = append(parts, strconv.FormatUint(uint64(s[i]), 10))
		} else {
			parts = append(parts, fmt.Sprintf("%d:%d", s[i], s[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// ParseSet expands a UID set ("1:3,7", ranges in either order) in order,
// each range ascending — the order COPYUID pairs source and destination.
func ParseSet(s string) ([]uint32, error) {
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(part, ":")
		lo, err := strconv.ParseUint(a, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("uid set %q", s)
		}
		hi := lo
		if isRange {
			if hi, err = strconv.ParseUint(b, 10, 32); err != nil {
				return nil, fmt.Errorf("uid set %q", s)
			}
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi-lo > 1_000_000 {
			return nil, fmt.Errorf("uid set %q: range too large", s)
		}
		for u := lo; u <= hi; u++ {
			out = append(out, uint32(u))
		}
	}
	return out, nil
}
