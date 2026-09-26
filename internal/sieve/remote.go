package sieve

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jfryman/sluice/internal/timing"
)

// Publisher makes newText the active script, keeping localPath in sync.
type Publisher interface {
	Publish(localPath, newText string, opt PublishOptions) (log []string, err error)
	Status(localPath string) (ServerStatus, error)
	Describe() string
}

// PublishOptions carries what the user confirmed before publishing.
type PublishOptions struct {
	// AllowSwitchFrom names another active script the user agreed to
	// replace. Publishing never deactivates any other script silently.
	AllowSwitchFrom string
}

// ServerStatus is what the server says about the configured script.
type ServerStatus struct {
	Scripts []string
	Active  string // name of the active script, "" if none
	Exists  bool   // the configured script is on the server
	Drift   bool   // exists and differs from the local file
}

// OtherActive reports a different script that is currently active.
func (s ServerStatus) OtherActive(ours string) string {
	if s.Active != "" && s.Active != ours {
		return s.Active
	}
	return ""
}

// ErrOtherActive: publishing would silently switch off another active script.
type ErrOtherActive struct{ Ours, Active string }

func (e ErrOtherActive) Error() string {
	return fmt.Sprintf("script %q is active on the server; publishing %q would switch it off "+
		"(set sieve_script = %q to manage it, or deactivate it first)", e.Active, e.Ours, e.Active)
}

// checkSwitch enforces the active-script guard.
func checkSwitch(st ServerStatus, ours string, opt PublishOptions) error {
	other := st.OtherActive(ours)
	if other == "" {
		return nil
	}
	if st.Exists && opt.AllowSwitchFrom == other {
		return nil
	}
	return ErrOtherActive{Ours: ours, Active: other}
}

// Remote publishes to a ManageSieve server with the native client: one
// session (TLS + login) per operation. See lode/sieve/remote.md.
type Remote struct {
	Server  string // host name (also the TLS server name)
	Port    int    // default 4190
	Script  string
	User    string        // username; if empty, the output of UserCmd
	UserCmd []string      // e.g. ["mail-user"]
	PassCmd []string      // e.g. ["mail-pass"]
	TLS     *tls.Config   // optional override (tests); default: system roots, ServerName=Server
	Timeout time.Duration // per network step; default 60s
	Trace   *timing.Recorder
}

func (r Remote) Describe() string { return r.Server + " as " + r.Script }

func (r Remote) addr() string {
	port := r.Port
	if port == 0 {
		port = 4190
	}
	return net.JoinHostPort(r.Server, strconv.Itoa(port))
}

func output(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty credential command")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return bytes.TrimRight(out, "\r\n"), nil
}

// session runs fn inside one authenticated connection.
func (r Remote) session(fn func(c *Client) error) error {
	user := r.User
	if user == "" {
		done := r.Trace.Time("user")
		u, err := output(r.UserCmd)
		done()
		if err != nil {
			return fmt.Errorf("credentials: %w", err)
		}
		user = string(u)
	}
	done := r.Trace.Time("pass")
	pass, err := output(r.PassCmd)
	done()
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	defer func() {
		for i := range pass {
			pass[i] = 0
		}
	}()
	cfg := r.TLS
	if cfg == nil {
		cfg = &tls.Config{ServerName: r.Server, MinVersion: tls.VersionTLS12}
	}
	c, err := Dial(context.Background(), r.addr(), cfg, r.Timeout, r.Trace)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.AuthPlain(user, pass); err != nil {
		return err
	}
	if err := fn(c); err != nil {
		c.Logout()
		return err
	}
	return c.Logout()
}

// serverStatus lists scripts and, if ours exists, compares it with localText.
func (r Remote) serverStatus(c *Client, localText string) (ServerStatus, error) {
	var st ServerStatus
	names, active, err := c.List()
	if err != nil {
		return st, err
	}
	st.Scripts, st.Active = names, active
	for _, n := range names {
		if n == r.Script {
			st.Exists = true
		}
	}
	if !st.Exists {
		return st, nil
	}
	remote, err := c.Get(r.Script)
	if err != nil {
		return st, err
	}
	st.Drift = normalize(remote) != normalize(localText)
	return st, nil
}

func normalize(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ErrDrift means the server's script is not what the local file says.
var ErrDrift = errors.New("remote script differs from local file; reconcile with sieve-edit first")

// Publish writes newText to localPath and makes it the active remote script,
// in one session: list → (drift check if ours exists) → backup + write →
// CHECKSCRIPT (if VERSION) → PUTSCRIPT → SETACTIVE. With no script on the
// server the first publish creates it. Any failure after the local write
// before a successful upload restores the previous local file.
func (r Remote) Publish(localPath, newText string, opt PublishOptions) (log []string, err error) {
	unlock, err := lockLocal(localPath)
	if err != nil {
		return nil, err
	}
	defer unlock()
	oldBytes, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	err = r.session(func(c *Client) error {
		st, err := r.serverStatus(c, string(oldBytes))
		if err != nil {
			return err
		}
		if err := checkSwitch(st, r.Script, opt); err != nil {
			return err
		}
		switch {
		case !st.Exists:
			log = append(log, fmt.Sprintf("no %q on server; creating it", r.Script))
		case st.Drift:
			return ErrDrift
		default:
			log = append(log, "remote matches local")
		}
		if other := st.OtherActive(r.Script); other != "" {
			log = append(log, fmt.Sprintf("replacing active script %q (confirmed)", other))
		}
		if err := os.WriteFile(localPath+".bak", oldBytes, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(localPath, []byte(newText), 0o644); err != nil {
			return err
		}
		restore := func() { os.WriteFile(localPath, oldBytes, 0o644) }
		if c.HasCap("VERSION") {
			if err := c.Check(newText); err != nil {
				restore()
				return fmt.Errorf("server rejected script: %w", err)
			}
			log = append(log, "server validated script")
		}
		if err := c.Put(r.Script, newText); err != nil {
			restore()
			return err
		}
		log = append(log, "uploaded "+r.Script)
		if err := c.SetActive(r.Script); err != nil {
			return fmt.Errorf("uploaded but activation failed: %w", err)
		}
		log = append(log, "activated "+r.Script)
		return nil
	})
	return log, err
}

// Status logs in, lists scripts and compares ours with localPath. It runs
// the credential commands (may prompt).
func (r Remote) Status(localPath string) (ServerStatus, error) {
	local, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ServerStatus{}, err
	}
	var st ServerStatus
	err = r.session(func(c *Client) error {
		var err error
		st, err = r.serverStatus(c, string(local))
		return err
	})
	return st, err
}
