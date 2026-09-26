// Package sieve edits the sluice-managed block of a Sieve script and
// publishes it over ManageSieve. See lode/sieve/managed-block.md.
package sieve

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	BeginMarker = "# >>> sluice managed block — edit via sluice >>>"
	EndMarker   = "# <<< sluice managed block <<<"
	metaPrefix  = "# sluice: "
)

type Kind string

const (
	KindList   Kind = "list"
	KindDomain Kind = "domain"
	KindSender Kind = "sender"
)

type Action string

const (
	ActionTrash Action = "trash"
	ActionFile  Action = "file"
	ActionRead  Action = "read"
)

type Rule struct {
	Kind   Kind   `json:"kind"`
	Value  string `json:"value"`
	Action Action `json:"action"`
	Folder string `json:"folder,omitempty"`
	Added  string `json:"added,omitempty"`
	Source string `json:"source,omitempty"` // "sweep" when added by the training folder
}

// Same reports whether r and o match the same mail.
func (r Rule) Same(o Rule) bool { return r.Kind == o.Kind && strings.EqualFold(r.Value, o.Value) }

func (r Rule) Describe() string {
	switch r.Action {
	case ActionFile:
		return fmt.Sprintf("%s %s → %s", r.Kind, r.Value, r.Folder)
	case ActionRead:
		return fmt.Sprintf("%s %s → mark read", r.Kind, r.Value)
	}
	return fmt.Sprintf("%s %s → trash", r.Kind, r.Value)
}

func (r Rule) Validate() error {
	if strings.TrimSpace(r.Value) == "" {
		return fmt.Errorf("rule has empty value")
	}
	switch r.Kind {
	case KindList, KindDomain, KindSender:
	default:
		return fmt.Errorf("unknown rule kind %q", r.Kind)
	}
	switch r.Action {
	case ActionTrash, ActionRead:
	case ActionFile:
		if r.Folder == "" {
			return fmt.Errorf("file rule needs a folder")
		}
	default:
		return fmt.Errorf("unknown action %q", r.Action)
	}
	return nil
}

// Quote renders s as a Sieve quoted string (RFC 5228 §2.4.2).
func Quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (r Rule) test() string {
	switch r.Kind {
	case KindList:
		return "header :contains \"List-Id\" " + Quote(r.Value)
	case KindDomain:
		return "address :domain :is \"from\" " + Quote(r.Value)
	default:
		return "address :all :is \"from\" " + Quote(r.Value)
	}
}

// Render produces the rule's metadata comment and Sieve text.
func (r Rule) Render(trash string) string {
	meta, _ := json.Marshal(r)
	var b strings.Builder
	b.WriteString(metaPrefix + string(meta) + "\n")
	b.WriteString("if " + r.test() + " {\n")
	switch r.Action {
	case ActionTrash:
		b.WriteString("    addflag \"\\\\Seen\";\n")
		b.WriteString("    fileinto " + Quote(trash) + ";\n    stop;\n")
	case ActionFile:
		b.WriteString("    fileinto " + Quote(r.Folder) + ";\n    stop;\n")
	case ActionRead:
		b.WriteString("    addflag \"\\\\Seen\";\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// Script is a parsed Sieve file split around the managed block.
type Script struct {
	Before   string // text before the block (or before the insertion point)
	After    string // text after the block
	HasBlock bool
	Rules    []Rule
}

// Parse splits text and reads the managed rules from their metadata comments.
func Parse(text string) (*Script, error) {
	s := &Script{}
	bi := strings.Index(text, BeginMarker)
	if bi < 0 {
		s.Before, s.After = splitAfterRequire(text)
		return s, nil
	}
	ei := strings.Index(text[bi:], EndMarker)
	if ei < 0 {
		return nil, fmt.Errorf("managed block start marker without end marker")
	}
	ei += bi
	s.HasBlock = true
	s.Before = text[:bi]
	after := text[ei+len(EndMarker):]
	s.After = strings.TrimPrefix(after, "\n")
	for _, line := range strings.Split(text[bi:ei], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, metaPrefix) {
			continue
		}
		var r Rule
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, metaPrefix)), &r); err != nil {
			return nil, fmt.Errorf("bad rule metadata %q: %w", line, err)
		}
		s.Rules = append(s.Rules, r)
	}
	return s, nil
}

var (
	// Anchored to line start so "require" inside comments is ignored.
	requireRe = regexp.MustCompile(`(?ms)^[ \t]*require\s+(\[[^\]]*\]|"[^"]*")\s*;`)
	stringRe  = regexp.MustCompile(`"([^"]*)"`)
)

// splitAfterRequire returns the text up to and including the first require
// statement's line, and the rest. With no require, the block goes first.
func splitAfterRequire(text string) (string, string) {
	loc := requireRe.FindStringIndex(text)
	if loc == nil {
		return "", text
	}
	end := loc[1]
	if nl := strings.Index(text[end:], "\n"); nl >= 0 {
		end += nl + 1
	} else {
		end = len(text)
	}
	return text[:end], text[end:]
}

// Upsert adds r, replacing any rule matching the same mail.
func (s *Script) Upsert(r Rule) {
	for i := range s.Rules {
		if s.Rules[i].Same(r) {
			s.Rules[i] = r
			return
		}
	}
	s.Rules = append(s.Rules, r)
}

// Remove drops the rule at index i.
func (s *Script) Remove(i int) {
	if i >= 0 && i < len(s.Rules) {
		s.Rules = append(s.Rules[:i], s.Rules[i+1:]...)
	}
}

// Find returns the rule matching kind/value, if any.
func (s *Script) Find(kind Kind, value string) (Rule, bool) {
	for _, r := range s.Rules {
		if r.Kind == kind && strings.EqualFold(r.Value, value) {
			return r, true
		}
	}
	return Rule{}, false
}

// Render reassembles the full script with the managed block and a require
// list covering the extensions the rules need.
func (s *Script) Render(trash string) string {
	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	for _, r := range s.Rules {
		b.WriteString(r.Render(trash))
	}
	b.WriteString(EndMarker + "\n")

	before := s.Before
	if !s.HasBlock && before != "" && !strings.HasSuffix(before, "\n\n") {
		before += "\n"
	}
	after := s.After
	if !s.HasBlock && after != "" && !strings.HasPrefix(after, "\n") {
		after = "\n" + after
	}
	return ensureRequire(before+b.String()+after, []string{"fileinto", "imap4flags"})
}

// ensureRequire merges exts into the first require statement, or prepends one.
func ensureRequire(text string, exts []string) string {
	loc := requireRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return "require [" + quoteList(exts) + "];\n" + text
	}
	have := map[string]bool{}
	var all []string
	for _, m := range stringRe.FindAllStringSubmatch(text[loc[2]:loc[3]], -1) {
		have[m[1]] = true
		all = append(all, m[1])
	}
	missing := false
	for _, e := range exts {
		if !have[e] {
			all = append(all, e)
			missing = true
		}
	}
	if !missing {
		return text
	}
	return text[:loc[2]] + "[" + quoteList(all) + "]" + text[loc[3]:]
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = Quote(x)
	}
	return strings.Join(q, ", ")
}

// SortRules orders rules by kind then value, for stable diffs.
func (s *Script) SortRules() {
	sort.SliceStable(s.Rules, func(i, j int) bool {
		if s.Rules[i].Kind != s.Rules[j].Kind {
			return s.Rules[i].Kind < s.Rules[j].Kind
		}
		return s.Rules[i].Value < s.Rules[j].Value
	})
}
