package plan

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jfryman/sluice/internal/cleanup"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/sieve"
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
}

func (t Target) Cleaner() *cleanup.Cleaner {
	return &cleanup.Cleaner{Index: t.Index, Root: t.Root, TrashFolder: t.TrashFolder,
		LockFile: t.LockFile, JournalPath: t.JournalPath}
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

// Apply publishes rule changes, then trashes every planned ref in one batch.
// A publish failure aborts before any mail moves.
func Apply(p *Plan, t Target) (Report, error) {
	rep := Report{Target: t.Name}

	if len(p.Rules) > 0 {
		cur, next, err := t.SieveDiff(p)
		if err != nil {
			return rep, err
		}
		if cur == next {
			rep.RulesSame = true
		} else {
			log, err := t.Publisher.Publish(t.SieveFile, next)
			rep.RuleLog = log
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
	res, err := t.Cleaner().Trash(msgs)
	rep.Batch, rep.Moved, rep.Skipped = res.Batch, res.Moved, res.Skipped
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
