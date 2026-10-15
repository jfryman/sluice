// Package sweep turns messages dropped into the training folder into Sieve
// rules (published right away) and cleanup of existing matching mail (moved
// to trash now, or queued as plan items). See lode/sweep.md.
package sweep

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jfryman/sluice/internal/cleanup"
	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/maildir"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/timing"
)

// Result of processing one dropped message.
const (
	Published = "published" // new rule published
	Ruled     = "ruled"     // a managed rule already existed; cleanup queued
	Held      = "held"      // guard hit: rule + cleanup queued in the plan, nothing published
	Skipped   = "skipped"   // no usable List-Id / From; left in the folder
	Failed    = "error"     // publish failed; left in the folder for the next sweep
)

type Outcome struct {
	Time    string `json:"time"`
	Key     string `json:"key"`
	From    string `json:"from,omitempty"`
	Subject string `json:"subject,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Value   string `json:"value,omitempty"`
	Result  string `json:"result"`
	Reason  string `json:"reason,omitempty"`
	Planned int    `json:"planned,omitempty"` // existing messages queued for cleanup
	Trashed int    `json:"trashed,omitempty"` // existing messages moved to trash now
}

type Report struct {
	Outcomes   []Outcome
	PublishLog []string
	Moved      int    // training messages moved to trash
	Cleaned    int    // existing matching messages moved to trash
	Timing     string // "scan 0.52s · … · total 1.20s" (empty when nothing ran)
}

// Changed reports whether local mail was moved (a sync would propagate it).
func (r Report) Changed() bool { return r.Moved > 0 || r.Cleaned > 0 }

type Deps struct {
	Cfg       config.Config
	Index     *index.Index
	Publisher sieve.Publisher
	Now       func() time.Time
	// Timing, if set, collects stage durations; the Publisher should record
	// into the same Recorder (sieve.Remote.Trace) so one line covers a sweep.
	Timing *timing.Recorder
	// Mover, if set, moves the cleanup (existing matches) on the IMAP
	// server. Drops always move locally: they are few, and a server move
	// would leave them in the training folder until the next sync, where
	// the next sweep would process them again.
	Mover cleanup.Mover
}

// EnsureFolder creates the training maildir if missing; mbsync's
// `Create Both` then creates it on the server.
func EnsureFolder(cfg config.Config) error {
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.MailRoot, cfg.TrainingFolder, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

type group struct {
	rule    sieve.Rule
	files   []maildir.File
	heads   []maildir.Headers
	result  string
	reason  string
	planned int
	cleanup []index.Message // existing matches to trash now (sweep_cleanup = apply)
	trashed int
}

// Run processes everything currently in the training folder once.
func Run(d Deps) (rep Report, err error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	cfg := d.Cfg
	if cfg.TrainingFolder == "" {
		return rep, errors.New("training_folder is not set")
	}
	unlock, err := lock(filepath.Join(filepath.Dir(cfg.SweepLog), "sweep.lock"))
	if err != nil {
		return rep, err
	}
	defer unlock()

	files, err := maildir.Files(cfg.MailRoot, cfg.TrainingFolder)
	if err != nil || len(files) == 0 {
		return rep, err // missing folder or nothing dropped: nothing to do
	}
	d.Timing.Reset()
	defer func() { rep.Timing = d.Timing.String() }()
	done := d.Timing.Time("scan")
	_, err = d.Index.Scan(nil)
	done()
	if err != nil {
		return rep, fmt.Errorf("index scan: %w", err)
	}
	logged := loggedSkips(cfg.SweepLog)

	// Group dropped messages by the rule they imply.
	groups := map[string]*group{}
	var order []string
	var skipped []Outcome
	for _, f := range files {
		h, err := maildir.ReadHeaders(f.Path)
		if err != nil && h.FromAddr == "" {
			h = maildir.Headers{}
		}
		kind, value := sieve.KindList, h.ListID
		if value == "" {
			kind, value = sieve.KindSender, h.FromAddr
		}
		if value == "" {
			if !logged[f.Name.Key] {
				skipped = append(skipped, Outcome{Key: f.Name.Key, Subject: h.Subject, Result: Skipped,
					Reason: "no List-Id or From address; left in " + cfg.TrainingFolder})
			}
			continue
		}
		id := string(kind) + "\x00" + value
		g, ok := groups[id]
		if !ok {
			g = &group{rule: sieve.Rule{Kind: kind, Value: value, Action: sieve.ActionTrash,
				Added: d.Now().Format("2006-01-02"), Source: "sweep"}}
			groups[id] = g
			order = append(order, id)
		}
		g.files = append(g.files, f)
		g.heads = append(g.heads, h)
		if known, why, err := d.Index.Known(h.FromAddr); err != nil {
			return rep, err
		} else if known && g.result == "" {
			g.result, g.reason = Held, why
		}
	}

	// Publish every new, unheld rule in one go, on top of the server's
	// current script (another machine may have published since).
	if s, ok := d.Publisher.(sieve.Syncer); ok {
		if res, err := s.Sync(cfg.SieveFile); err != nil {
			return rep, fmt.Errorf("sync rules: %w", err)
		} else if res.Updated {
			rep.PublishLog = append(rep.PublishLog, res.Summary())
		}
	}
	base, err := readScript(cfg.SieveFile)
	if err != nil {
		return rep, err
	}
	var fresh []*group
	for _, id := range order {
		g := groups[id]
		switch {
		case g.result == Held:
		case hasRule(base, g.rule):
			g.result = Ruled
		default:
			fresh = append(fresh, g)
		}
	}
	if len(fresh) > 0 {
		for _, g := range fresh {
			base.Upsert(g.rule)
		}
		log, err := d.Publisher.Publish(cfg.SieveFile, base.Render(cfg.TrashFolder), sieve.PublishOptions{})
		rep.PublishLog = append(rep.PublishLog, log...)
		for _, g := range fresh {
			if err != nil {
				g.result, g.reason = Failed, "publish: "+err.Error()
			} else {
				g.result = Published
			}
		}
	}

	// Queue cleanup (and held rules) in the plan, move processed drops, then
	// trash existing matches as a separate batch so U undoes just the cleanup
	// (undoing the drops would only get them swept again).
	var finished []*group
	for _, id := range order {
		if g := groups[id]; g.result != Failed {
			finished = append(finished, g)
		}
	}
	if len(finished) > 0 {
		stop := d.Timing.Time("plan")
		err := queue(d, finished)
		stop()
		if err != nil {
			return rep, err
		}
		var msgs []index.Message
		for _, g := range finished {
			for _, f := range g.files {
				msgs = append(msgs, index.Message{Key: f.Name.Key, Folder: f.Folder, Path: f.Path})
			}
		}
		c := &cleanup.Cleaner{Index: d.Index, Root: cfg.MailRoot, TrashFolder: cfg.TrashFolder,
			LockFile: cfg.LockFile, JournalPath: cfg.JournalPath}
		stop = d.Timing.Time("move")
		res, err := c.Trash(msgs)
		stop()
		rep.Moved = res.Moved
		if err != nil {
			return rep, fmt.Errorf("move processed messages: %w", err)
		}

		msgs = nil
		for _, g := range finished {
			msgs = append(msgs, g.cleanup...)
		}
		if len(msgs) > 0 {
			stop = d.Timing.Time("cleanup")
			c.Server = d.Mover
			res, err := c.Trash(msgs)
			stop()
			rep.Cleaned = res.Moved
			for _, g := range finished {
				for _, m := range g.cleanup {
					if !res.SkippedKeys[m.Key] {
						g.trashed++
					}
				}
			}
			if err != nil {
				return rep, fmt.Errorf("clean up existing mail: %w", err)
			}
		}
	}

	now := d.Now().Format(time.RFC3339)
	for _, id := range order {
		g := groups[id]
		for i, f := range g.files {
			rep.Outcomes = append(rep.Outcomes, Outcome{Time: now, Key: f.Name.Key, From: g.heads[i].FromAddr,
				Subject: g.heads[i].Subject, Kind: string(g.rule.Kind), Value: g.rule.Value,
				Result: g.result, Reason: g.reason, Planned: g.planned, Trashed: g.trashed})
		}
	}
	for _, o := range skipped {
		o.Time = now
		rep.Outcomes = append(rep.Outcomes, o)
	}
	return rep, appendLog(cfg.SweepLog, rep.Outcomes)
}

// queue collects, per group, existing matching mail: held groups (and every
// group with sweep_cleanup = queue) get a plan trash item, held groups also
// the pending rule; the rest keep it in g.cleanup to trash now. Published
// rules drop conflicting pending ops.
func queue(d Deps, gs []*group) error {
	_, err := plan.Update(d.Cfg.PlanPath, func(p *plan.Plan) error {
		for _, g := range gs {
			ms, err := d.Index.Matching(index.Kind(g.rule.Kind), []string{g.rule.Value}, true, 0)
			if err != nil {
				return err
			}
			var refs []plan.Ref
			g.cleanup = nil
			for _, m := range ms {
				if m.Folder != d.Cfg.TrainingFolder {
					refs = append(refs, plan.Ref{Key: m.Key, Folder: m.Folder})
					g.cleanup = append(g.cleanup, m)
				}
			}
			if g.result == Published {
				p.DropRule(g.rule) // the published rule supersedes any pending op
			}
			if g.result != Held && d.Cfg.SweepCleanup != "queue" {
				continue
			}
			g.cleanup = nil
			label := fmt.Sprintf("sweep: %s %s", g.rule.Kind, g.rule.Value)
			switch g.result {
			case Held:
				label = fmt.Sprintf("sweep (held: %s): %s %s", g.reason, g.rule.Kind, g.rule.Value)
				p.SetRule("add", g.rule)
			}
			g.planned = p.AddTrash(label, refs)
		}
		return nil
	})
	return err
}

func readScript(path string) (*sieve.Script, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return sieve.Parse(string(b))
}

func hasRule(s *sieve.Script, r sieve.Rule) bool {
	_, ok := s.Find(r.Kind, r.Value)
	return ok
}

func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// ReadLog returns logged outcomes, newest last.
func ReadLog(path string) ([]Outcome, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Outcome
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var o Outcome
		if json.Unmarshal(sc.Bytes(), &o) == nil {
			out = append(out, o)
		}
	}
	return out, sc.Err()
}

func loggedSkips(path string) map[string]bool {
	out := map[string]bool{}
	prev, _ := ReadLog(path)
	for _, o := range prev {
		if o.Result == Skipped {
			out[o.Key] = true
		}
	}
	return out
}

func appendLog(path string, outs []Outcome) error {
	if len(outs) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, o := range outs {
		if err := enc.Encode(o); err != nil {
			return err
		}
	}
	return nil
}

// Summary is a one-line-per-outcome human report.
func (r Report) Summary() []string {
	outs := append([]Outcome(nil), r.Outcomes...)
	sort.SliceStable(outs, func(i, j int) bool { return outs[i].Result < outs[j].Result })
	var out []string
	for _, o := range outs {
		line := fmt.Sprintf("%-9s %s %s", o.Result, o.Kind, o.Value)
		if o.Trashed > 0 {
			line += fmt.Sprintf("  (%d existing moved to trash)", o.Trashed)
		}
		if o.Planned > 0 {
			line += fmt.Sprintf("  (%d existing queued in plan)", o.Planned)
		}
		if o.Reason != "" {
			line += "  — " + o.Reason
		}
		out = append(out, line)
	}
	return out
}

// Brief is a one-line summary of outcomes, one entry per (kind, value), for
// status lines and notifications; "" when there is nothing worth showing.
func Brief(outs []Outcome) string {
	seen := map[string]bool{}
	var parts []string
	for _, o := range outs {
		id := o.Result + "\x00" + o.Kind + "\x00" + o.Value
		if seen[id] || o.Result == Skipped {
			continue
		}
		seen[id] = true
		verb := map[string]string{Published: "blocked", Ruled: "already blocked", Held: "held", Failed: "failed"}[o.Result]
		part := verb + " " + o.Value
		switch {
		case o.Trashed > 0:
			part += fmt.Sprintf(" (%d trashed)", o.Trashed)
		case o.Planned > 0:
			part += fmt.Sprintf(" (%d queued)", o.Planned)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}
