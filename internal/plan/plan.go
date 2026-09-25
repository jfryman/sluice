// Package plan records intended rule changes and trash operations and
// applies them to a target (sandbox or real). See lode/apply/plan.md.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jfryman/sluice/internal/sieve"
)

const version = 1

type RuleOp struct {
	Op   string     `json:"op"` // "add" | "remove"
	Rule sieve.Rule `json:"rule"`
}

type Ref struct {
	Key    string `json:"key"`
	Folder string `json:"folder"`
}

type TrashItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Refs  []Ref  `json:"refs"`
}

type Plan struct {
	Version   int         `json:"version"`
	Rules     []RuleOp    `json:"rules"`
	Trash     []TrashItem `json:"trash"`
	Validated *time.Time  `json:"validated,omitempty"`
}

func (p *Plan) Empty() bool { return len(p.Rules) == 0 && len(p.Trash) == 0 }

// Messages counts trash refs across all items.
func (p *Plan) Messages() int {
	n := 0
	for _, t := range p.Trash {
		n += len(t.Refs)
	}
	return n
}

func (p *Plan) touch() { p.Validated = nil }

// SetRule records an add (upsert) or remove for rule's (kind, value),
// replacing any earlier op on the same mail.
func (p *Plan) SetRule(op string, r sieve.Rule) {
	p.touch()
	for i := range p.Rules {
		if p.Rules[i].Rule.Same(r) {
			p.Rules[i] = RuleOp{Op: op, Rule: r}
			return
		}
	}
	p.Rules = append(p.Rules, RuleOp{Op: op, Rule: r})
}

// DropRule removes any pending op for (kind, value).
func (p *Plan) DropRule(r sieve.Rule) bool {
	for i := range p.Rules {
		if p.Rules[i].Rule.Same(r) {
			p.touch()
			p.Rules = append(p.Rules[:i], p.Rules[i+1:]...)
			return true
		}
	}
	return false
}

// PendingRule returns the op for (kind, value), if any.
func (p *Plan) PendingRule(kind sieve.Kind, value string) (RuleOp, bool) {
	probe := sieve.Rule{Kind: kind, Value: value}
	for _, o := range p.Rules {
		if o.Rule.Same(probe) {
			return o, true
		}
	}
	return RuleOp{}, false
}

// AddTrash adds refs not already planned as a new item; returns how many
// were new (0 means nothing was added).
func (p *Plan) AddTrash(label string, refs []Ref) int {
	have := map[string]bool{}
	for _, t := range p.Trash {
		for _, r := range t.Refs {
			have[r.Key] = true
		}
	}
	var fresh []Ref
	for _, r := range refs {
		if !have[r.Key] {
			have[r.Key] = true
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return 0
	}
	p.touch()
	p.Trash = append(p.Trash, TrashItem{ID: time.Now().Format("20060102T150405.000"), Label: label, Refs: fresh})
	return len(fresh)
}

func (p *Plan) DropTrash(i int) {
	if i >= 0 && i < len(p.Trash) {
		p.touch()
		p.Trash = append(p.Trash[:i], p.Trash[i+1:]...)
	}
}

// ApplyRules applies the rule ops to a parsed script in place.
func (p *Plan) ApplyRules(s *sieve.Script) {
	for _, o := range p.Rules {
		switch o.Op {
		case "add":
			s.Upsert(o.Rule)
		case "remove":
			for i := range s.Rules {
				if s.Rules[i].Same(o.Rule) {
					s.Remove(i)
					break
				}
			}
		}
	}
}

// FolderCounts summarises an item's refs by folder, largest first.
func (t TrashItem) FolderCounts() string {
	m := map[string]int{}
	for _, r := range t.Refs {
		m[r.Folder]++
	}
	type kv struct {
		f string
		n int
	}
	var kvs []kv
	for f, n := range m {
		kvs = append(kvs, kv{f, n})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].n > kvs[j].n || (kvs[i].n == kvs[j].n && kvs[i].f < kvs[j].f) })
	parts := make([]string, len(kvs))
	for i, x := range kvs {
		parts[i] = fmt.Sprintf("%s %d", x.f, x.n)
	}
	return strings.Join(parts, ", ")
}

// ---- persistence ----

func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Plan{Version: version}, nil
	}
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("plan %s: %w", path, err)
	}
	if p.Version != version {
		return nil, fmt.Errorf("plan %s: unsupported version %d", path, p.Version)
	}
	return &p, nil
}

// Save writes atomically (temp file + rename).
func Save(path string, p *Plan) error {
	p.Version = version
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Update loads, mutates and saves the plan under an exclusive lock, so a
// real-mode and a sandbox-mode TUI can edit the same plan safely.
func Update(path string, fn func(*Plan) error) (*Plan, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	p, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := fn(p); err != nil {
		return p, err
	}
	return p, Save(path, p)
}

// Archive copies the plan into dir and resets the plan file to empty.
func Archive(path, dir string) (string, error) {
	var dst string
	_, err := Update(path, func(p *Plan) error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		dst = filepath.Join(dir, time.Now().Format("20060102T150405")+".json")
		if err := Save(dst, p); err != nil {
			return err
		}
		*p = Plan{Version: version}
		return nil
	})
	return dst, err
}
