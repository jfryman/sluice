package plan

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jfryman/sluice/internal/cleanup"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/timing"
)

// Target is an environment a plan can be applied to.
type Target struct {
	Name        string // "sandbox" | "real"
	Root        string
	TrashFolder string
	LockFile    string
	JournalPath string
	SieveFile   string
	Publisher   sieve.Publisher
	Index       *index.Index // optional; kept in sync with moves when set
	// AllowSwitchFrom is the other active script the user confirmed
	// replacing (from a Status precheck); empty means never switch.
	AllowSwitchFrom string
	// Server, if set, moves synced messages on the IMAP server (real only).
	Server cleanup.Mover
	// Progress, if set, gets a short description of each apply step
	// ("publishing rules", "moving 120/5012 messages").
	Progress func(stage string)
}

func (t Target) progress(stage string) {
	if t.Progress != nil {
		t.Progress(stage)
	}
}

func (t Target) Cleaner() *cleanup.Cleaner {
	return &cleanup.Cleaner{Index: t.Index, Root: t.Root, TrashFolder: t.TrashFolder,
		LockFile: t.LockFile, JournalPath: t.JournalPath, Server: t.Server}
}

// SieveDiff renders what the plan's rule ops would change in the target's
// sieve file: (current text, new text).
func (t Target) SieveDiff(p *Plan) (string, string, error) {
	b, err := os.ReadFile(t.SieveFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	s, err := sieve.Parse(string(b))
	if err != nil {
		return "", "", err
	}
	if len(p.Rules) == 0 {
		return string(b), string(b), nil
	}
	p.ApplyRules(s)
	next := s.Render(t.TrashFolder)
	if !s.HasBlock && len(s.Rules) == 0 {
		next = string(b) // don't insert an empty block
	}
	return string(b), next, nil
}

type ItemReport struct {
	Label   string
	Planned int
	Moved   int
}

type Report struct {
	Target    string
	RuleLog   []string
	RulesSame bool // rule ops produced no change
	Batch     string
	Items     []ItemReport
	Moved     int
	Server    int // of Moved: moved on the IMAP server; local copies follow on the next sync
	Notes     []string
	Skipped   []string
}

func (r Report) Lines() []string {
	var out []string
	out = append(out, "Applied to "+r.Target+":", "")
	switch {
	case r.RulesSame:
		out = append(out, "sieve: no change")
	case len(r.RuleLog) > 0:
		out = append(out, "sieve: "+strings.Join(r.RuleLog, " · "))
	}
	for _, it := range r.Items {
		out = append(out, fmt.Sprintf("trash: %d/%d moved  %s", it.Moved, it.Planned, it.Label))
	}
	if r.Moved > 0 {
		out = append(out, "", fmt.Sprintf("%d messages moved in batch %s (U undoes it)", r.Moved, r.Batch))
	}
	if r.Server > 0 {
		out = append(out, fmt.Sprintf("%d of them moved on the server; local copies update after the next sync", r.Server))
	}
	for _, n := range r.Notes {
		out = append(out, "note: "+n)
	}
	if len(r.Skipped) > 0 {
		out = append(out, fmt.Sprintf("%d skipped:", len(r.Skipped)))
		for i, s := range r.Skipped {
			if i == 10 {
				out = append(out, fmt.Sprintf("  … %d more", len(r.Skipped)-10))
				break
			}
			out = append(out, "  "+s)
		}
	}
	return out
}

// ChangesSieve reports whether applying p would change t's sieve script
// (i.e. whether a publish, and so a server precheck, is involved).
func (t Target) ChangesSieve(p *Plan) bool {
	if len(p.Rules) == 0 {
		return false
	}
	cur, next, err := t.SieveDiff(p)
	return err != nil || cur != next
}

// Apply publishes rule changes, then trashes every planned ref in one batch.
// A publish failure aborts before any mail moves.
func Apply(p *Plan, t Target) (Report, error) {
	rep := Report{Target: t.Name}

	if len(p.Rules) > 0 {
		// Another machine may have published since this one last synced:
		// bring the local file up to date before computing the change.
		if s, ok := t.Publisher.(sieve.Syncer); ok {
			t.progress("syncing rules from server")
			res, err := s.Sync(t.SieveFile)
			if err != nil {
				return rep, fmt.Errorf("sync rules: %w", err)
			}
			if res.Updated {
				rep.RuleLog = append(rep.RuleLog, res.Summary())
			}
		}
		cur, next, err := t.SieveDiff(p)
		if err != nil {
			return rep, err
		}
		if cur == next {
			rep.RulesSame = true
		} else {
			t.progress("publishing rules")
			log, err := t.Publisher.Publish(t.SieveFile, next, sieve.PublishOptions{AllowSwitchFrom: t.AllowSwitchFrom})
			rep.RuleLog = append(rep.RuleLog, log...)
			if err != nil {
				return rep, fmt.Errorf("publish: %w", err)
			}
		}
	}

	if len(p.Trash) == 0 {
		return rep, nil
	}
	var msgs []index.Message
	for _, it := range p.Trash {
		for _, r := range it.Refs {
			// No path: the cleaner resolves key+folder against the target maildir.
			msgs = append(msgs, index.Message{Key: r.Key, Folder: r.Folder})
		}
	}
	c := t.Cleaner()
	c.Progress = func(done, total int) { t.progress(fmt.Sprintf("moving %d/%d messages", done, total)) }
	c.OnWait = func(w time.Duration) { t.progress(timing.WaitingForMbsync(w)) }
	c.Stage = t.progress
	res, err := c.Trash(msgs)
	rep.Batch, rep.Moved, rep.Skipped = res.Batch, res.Moved, res.Skipped
	rep.Server, rep.Notes = res.Server, res.Notes
	for _, it := range p.Trash {
		ir := ItemReport{Label: it.Label, Planned: len(it.Refs)}
		for _, r := range it.Refs {
			if !res.SkippedKeys[r.Key] && r.Folder != t.TrashFolder {
				ir.Moved++
			}
		}
		rep.Items = append(rep.Items, ir)
	}
	return rep, err
}

// Sync runs the configured sync command (e.g. mail-sync) after server moves.
// mail-sync's flock exits 75 when mbsync is already running: not an error,
// that run or the next one picks the changes up (Busy reports it).
func Sync(argv []string) (busy bool, err error) {
	if len(argv) == 0 {
		return false, nil
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 75 {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return false, nil
}
