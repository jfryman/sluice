package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jfryman/sluice/internal/creds"
	"github.com/jfryman/sluice/internal/timing"
)

// ErrUnsupported: the server can't move by UID with confirmation (needs
// UIDPLUS for COPYUID, plus MOVE or UID EXPUNGE). Callers fall back to
// local file moves.
var ErrUnsupported = errors.New("IMAP server lacks UIDPLUS (COPYUID): server-side moves unavailable")

// chunk bounds one UID MOVE command's set.
const chunk = 500

// Remote moves messages on the real IMAP server, one authenticated session
// per call, with the same credentials as sieve.Remote.
type Remote struct {
	Server  string
	Port    int // default 993 (implicit TLS)
	User    string
	UserCmd []string
	PassCmd []string
	TLS     *tls.Config   // optional override (tests)
	Timeout time.Duration // per network step; default 60s
	Trace   *timing.Recorder
}

// Group is one source folder's server moves. Validity is the far
// UIDVALIDITY the UIDs belong to (from mbsync's state, or a journal).
type Group struct {
	From     string
	Validity uint32
	UIDs     []uint32
	To       string
}

type Pair struct{ Src, Dst uint32 }

// Result is what happened to one Group. Stale: the folder's UIDVALIDITY
// changed, nothing was moved (fall back to local moves). Pairs are the
// COPYUID-confirmed moves; requested UIDs not in Pairs weren't there.
type Result struct {
	Group
	Stale       bool
	DstValidity uint32
	Pairs       []Pair
}

func (r Remote) addr() string {
	port := r.Port
	if port == 0 {
		port = 993
	}
	return net.JoinHostPort(r.Server, strconv.Itoa(port))
}

func (r Remote) session(fn func(c *Client) error) error {
	user, pass, err := creds.Get(r.User, r.UserCmd, r.PassCmd, r.Trace)
	if err != nil {
		return err
	}
	defer creds.Wipe(pass)
	cfg := r.TLS
	if cfg == nil {
		cfg = &tls.Config{ServerName: r.Server, MinVersion: tls.VersionTLS12}
	}
	c, err := Dial(context.Background(), r.addr(), cfg, r.Timeout, r.Trace)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Login(user, pass); err != nil {
		return err
	}
	if err := fn(c); err != nil {
		c.Logout()
		return err
	}
	return c.Logout()
}

// MoveAll moves each group's UIDs on the server in one session, in chunks,
// reporting "moving on server N/M (Folder)". On error it returns the
// results confirmed so far.
func (r Remote) MoveAll(groups []Group, progress func(string)) ([]Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	total := 0
	for _, g := range groups {
		total += len(g.UIDs)
	}
	var out []Result
	err := r.session(func(c *Client) error {
		if !c.Has("UIDPLUS") {
			return ErrUnsupported
		}
		done := 0
		for _, g := range groups {
			res := Result{Group: g}
			v, err := c.Select(g.From)
			if err != nil {
				return err
			}
			if v != g.Validity {
				res.Stale = true
				out = append(out, res)
				done += len(g.UIDs)
				continue
			}
			for i := 0; i < len(g.UIDs); i += chunk {
				part := g.UIDs[i:min(i+chunk, len(g.UIDs))]
				progress(fmt.Sprintf("moving on server %d/%d (%s)", done, total, g.From))
				cu, err := c.UIDMove(part, g.To)
				if cu.Validity != 0 {
					res.DstValidity = cu.Validity
				}
				for k := range cu.Src {
					res.Pairs = append(res.Pairs, Pair{cu.Src[k], cu.Dst[k]})
				}
				if err != nil {
					out = append(out, res)
					return fmt.Errorf("move %s → %s: %w", g.From, g.To, err)
				}
				done += len(part)
			}
			out = append(out, res)
		}
		progress(fmt.Sprintf("moving on server %d/%d", done, total))
		return nil
	})
	return out, err
}

// Probe logs in and reports the post-login capabilities and each folder's
// UIDVALIDITY (EXAMINE: read-only). A folder that can't be opened maps to 0.
type ProbeResult struct {
	Caps     []string
	Move     bool // server-side moves possible (UIDPLUS, and MOVE or UID EXPUNGE)
	Validity map[string]uint32
}

func (r Remote) Probe(folders []string) (ProbeResult, error) {
	res := ProbeResult{Validity: map[string]uint32{}}
	err := r.session(func(c *Client) error {
		res.Caps = c.Caps()
		res.Move = c.Has("UIDPLUS")
		for _, f := range folders {
			v, _ := c.Examine(f)
			res.Validity[f] = v
		}
		return nil
	})
	return res, err
}

func (r Remote) Describe() string { return r.addr() }
