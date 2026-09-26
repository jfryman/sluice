package sieve

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jfryman/sluice/internal/timing"
)

// Client is a minimal ManageSieve (RFC 5804) client: STARTTLS, SASL PLAIN,
// LISTSCRIPTS, GETSCRIPT, CHECKSCRIPT, PUTSCRIPT, SETACTIVE, LOGOUT.
// It never logs protocol traffic (the AUTHENTICATE line carries the
// password); see lode/sieve/remote.md.
type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	caps    map[string]string
	timeout time.Duration
	trace   *timing.Recorder
}

// ServerError is a NO or BYE response.
type ServerError struct {
	Cmd, Status, Code, Msg string
}

func (e *ServerError) Error() string {
	s := e.Cmd + ": " + e.Status
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if e.Msg != "" {
		s += " " + e.Msg
	}
	return s
}

// ErrNoTLS: the server doesn't offer STARTTLS; credentials are never sent in clear.
var ErrNoTLS = errors.New("managesieve server does not offer STARTTLS; refusing to send credentials in clear text")

type response struct {
	lines              [][]string
	status, code, text string
}

// Dial connects, reads the greeting, upgrades with STARTTLS and re-reads
// the capabilities. tlsCfg.ServerName must be set.
func Dial(ctx context.Context, addr string, tlsCfg *tls.Config, timeout time.Duration, trace *timing.Recorder) (*Client, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	done := trace.Time("connect")
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	done()
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), timeout: timeout, trace: trace}
	if err := c.readCaps("greeting"); err != nil {
		conn.Close()
		return nil, err
	}
	if _, ok := c.caps["STARTTLS"]; !ok {
		conn.Close()
		return nil, ErrNoTLS
	}
	done = trace.Time("starttls")
	defer done()
	if _, err := c.cmd("STARTTLS", "STARTTLS"); err != nil {
		conn.Close()
		return nil, err
	}
	tc := tls.Client(conn, tlsCfg)
	tc.SetDeadline(time.Now().Add(timeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	c.conn, c.r = tc, bufio.NewReader(tc)
	// RFC 5804 §2.2: the server re-issues its capabilities after TLS.
	if err := c.readCaps("post-TLS capabilities"); err != nil {
		tc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// HasCap reports whether the server advertised a capability (e.g. "VERSION").
func (c *Client) HasCap(name string) bool { _, ok := c.caps[name]; return ok }

func (c *Client) readCaps(what string) error {
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	resp, err := c.read()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.status != "OK" {
		return &ServerError{Cmd: what, Status: resp.status, Code: resp.code, Msg: resp.text}
	}
	c.caps = map[string]string{}
	for _, l := range resp.lines {
		if len(l) == 0 {
			continue
		}
		v := ""
		if len(l) > 1 {
			v = l[1]
		}
		c.caps[strings.ToUpper(l[0])] = v
	}
	return nil
}

// AuthPlain authenticates with SASL PLAIN. pass is not retained.
func (c *Client) AuthPlain(user string, pass []byte) error {
	defer c.trace.Time("auth")()
	if sasl, ok := c.caps["SASL"]; ok && !containsWord(sasl, "PLAIN") {
		return fmt.Errorf("server SASL mechanisms %q don't include PLAIN", sasl)
	}
	raw := make([]byte, 0, len(user)*2+len(pass)+2)
	raw = append(append(append(append(raw, 0), user...), 0), pass...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	for i := range raw {
		raw[i] = 0
	}
	_, err := c.cmd("AUTHENTICATE", `AUTHENTICATE "PLAIN" "`+b64+`"`)
	var se *ServerError
	if errors.As(err, &se) {
		se.Msg = strings.TrimSpace(se.Msg + " (check user / pass_cmd)")
	}
	return err
}

// List returns script names and the active one ("" if none).
func (c *Client) List() ([]string, string, error) {
	defer c.trace.Time("list")()
	resp, err := c.cmd("LISTSCRIPTS", "LISTSCRIPTS")
	if err != nil {
		return nil, "", err
	}
	var names []string
	active := ""
	for _, l := range resp.lines {
		if len(l) == 0 {
			continue
		}
		names = append(names, l[0])
		if len(l) > 1 && strings.EqualFold(l[1], "ACTIVE") {
			active = l[0]
		}
	}
	return names, active, nil
}

// Get returns a script's text.
func (c *Client) Get(name string) (string, error) {
	defer c.trace.Time("get")()
	resp, err := c.cmd("GETSCRIPT", "GETSCRIPT "+quote(name))
	if err != nil {
		return "", err
	}
	if len(resp.lines) == 0 || len(resp.lines[0]) == 0 {
		return "", errors.New("GETSCRIPT: empty response")
	}
	return resp.lines[0][0], nil
}

// Check validates a script server-side (only when VERSION is advertised).
func (c *Client) Check(text string) error {
	defer c.trace.Time("check")()
	_, err := c.cmd("CHECKSCRIPT", "CHECKSCRIPT "+literal(text))
	return err
}

func (c *Client) Put(name, text string) error {
	defer c.trace.Time("put")()
	_, err := c.cmd("PUTSCRIPT", "PUTSCRIPT "+quote(name)+" "+literal(text))
	return err
}

func (c *Client) SetActive(name string) error {
	defer c.trace.Time("setactive")()
	_, err := c.cmd("SETACTIVE", "SETACTIVE "+quote(name))
	return err
}

func (c *Client) Logout() error {
	_, err := c.cmd("LOGOUT", "LOGOUT")
	return err
}

// cmd sends one command line (which may embed literals) and reads the reply.
func (c *Client) cmd(name, line string) (*response, error) {
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := io.WriteString(c.conn, line+"\r\n"); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	resp, err := c.read()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if resp.status != "OK" {
		return resp, &ServerError{Cmd: name, Status: resp.status, Code: resp.code, Msg: resp.text}
	}
	return resp, nil
}

// read collects data lines until an OK / NO / BYE status line.
func (c *Client) read() (*response, error) {
	resp := &response{}
	for {
		toks, err := readTokens(c.r)
		if err != nil {
			return nil, err
		}
		if len(toks) > 0 {
			switch s := strings.ToUpper(toks[0]); s {
			case "OK", "NO", "BYE":
				resp.status = s
				rest := toks[1:]
				if len(rest) > 0 && strings.HasPrefix(rest[0], "(") {
					resp.code = strings.Trim(rest[0], "()")
					rest = rest[1:]
				}
				if len(rest) > 0 {
					resp.text = rest[0]
				}
				return resp, nil
			}
		}
		resp.lines = append(resp.lines, toks)
	}
}

// readTokens reads one logical line: atoms, "quoted" strings, (codes) and
// {n}/{n+} literals (whose bytes follow the CRLF) — used by both the client
// and the test server.
func readTokens(r *bufio.Reader) ([]string, error) {
	var toks []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		i := 0
		for i < len(line) {
			switch ch := line[i]; {
			case ch == ' ':
				i++
			case ch == '"':
				var b strings.Builder
				i++
				for i < len(line) && line[i] != '"' {
					if line[i] == '\\' && i+1 < len(line) {
						i++
					}
					b.WriteByte(line[i])
					i++
				}
				i++
				toks = append(toks, b.String())
			case ch == '(':
				j := strings.IndexByte(line[i:], ')')
				if j < 0 {
					j = len(line) - i - 1
				}
				toks = append(toks, line[i:i+j+1])
				i += j + 1
			case ch == '{' && strings.HasSuffix(line, "}") && !strings.Contains(line[i:], " "):
				n, err := strconv.Atoi(strings.TrimSuffix(line[i+1:len(line)-1], "+"))
				if err != nil || n < 0 {
					return nil, fmt.Errorf("bad literal %q", line[i:])
				}
				buf := make([]byte, n)
				if _, err := io.ReadFull(r, buf); err != nil {
					return nil, err
				}
				toks = append(toks, string(buf))
				i = len(line)
				goto next // the rest of the logical line follows the literal
			default:
				j := strings.IndexByte(line[i:], ' ')
				if j < 0 {
					j = len(line) - i
				}
				toks = append(toks, line[i:i+j])
				i += j
			}
		}
		return toks, nil
	next:
	}
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// literal encodes text as a non-synchronising literal: {n+}CRLF text.
func literal(text string) string {
	return fmt.Sprintf("{%d+}\r\n%s", len(text), text)
}

func containsWord(list, w string) bool {
	for _, f := range strings.Fields(strings.ToUpper(list)) {
		if f == w {
			return true
		}
	}
	return false
}
