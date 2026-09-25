// Package tui is the Bubble Tea front end. Every action edits the pending
// plan; nothing touches mail or the server until the plan is applied.
// See lode/tui/summary.md and lode/apply/plan.md.
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/cleanup"
	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/doctor"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/maildir"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/query"
	"github.com/jfryman/sluice/internal/sieve"
)

// SandboxTarget lets real mode validate a plan: Refresh re-clones, then the
// plan is applied to Target.
type SandboxTarget struct {
	Target  plan.Target
	Refresh func() error
}

type Deps struct {
	Env     string        // "real" | "sandbox"
	Config  config.Config // this environment's config
	Index   *index.Index
	Target  plan.Target    // this environment
	Sandbox *SandboxTarget // real mode only
	Created time.Time      // sandbox mode: clone time
}

type tab int

const (
	tabCandidates tab = iota
	tabRules
	tabSearch
	tabPlan
	numTabs
)

type overlay int

const (
	ovNone overlay = iota
	ovDialog
	ovPicker
)

// action is a key offered by a dialog.
type action struct {
	key, label string
	run        func() tea.Cmd
}

// dialogState is a scrollable box of lines with optional actions; with no
// actions it is informational and any of esc/enter/q closes it.
type dialogState struct {
	title   string
	lines   []string
	off     int
	actions []action
}

type pickerState struct {
	folders []string
	cur     int
	targets []index.Group
}

type Model struct {
	d    Deps
	w, h int

	tab tab
	ov  overlay

	// Vim state: per-list viewports, pending count/prefix, visual mode.
	lc, lr, ls, lp listView // candidates, rules, search, plan
	keys           keyState
	visual         bool
	vanchor        int

	// "/" search within the current list.
	findOn    bool
	findIn    textinput.Model
	findStart int
	lastFind  string

	// ":" command line.
	cmdOn      bool
	cmdInput   textinput.Model
	cmdHist    []string
	cmdHistIdx int

	// Candidates
	kind       index.Kind
	sortKey    index.SortKey
	groups     []index.Group
	view       []index.Group
	marked     map[string]bool
	filterText string
	hideRule   bool

	// Rules: base is this environment's sieve file as on disk.
	base    *sieve.Script
	baseErr error

	// Plan (shared file; reloaded on every mutation and tab switch) and the
	// session's undo/redo snapshots of it.
	plan      *plan.Plan
	undoStack []*plan.Plan
	redoStack []*plan.Plan

	// Search
	qinput  textinput.Model
	qOn     bool
	results []index.Message
	rtotal  int

	dialog dialogState
	picker pickerState

	busy       string
	spin       spinner.Model
	scanCh     chan tea.Msg
	status     string
	statusErr  bool
	indexCount int
}

func New(d Deps) *Model {
	f := textinput.New()
	f.Prompt = "/"
	c := textinput.New()
	c.Prompt = ":"
	q := textinput.New()
	q.Prompt = "query: "
	q.Placeholder = `list:foo older:1y unread   (from: domain: list: folder: subject: before: after: older: read unread)`
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	m := &Model{d: d, kind: index.KindList, marked: map[string]bool{}, findIn: f, cmdInput: c, qinput: q, spin: sp, plan: &plan.Plan{}}
	m.loadScript()
	m.reloadPlan()
	return m
}

func (m *Model) loadScript() {
	b, err := os.ReadFile(m.d.Config.SieveFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		m.base, m.baseErr = nil, err
		return
	}
	s, err := sieve.Parse(string(b))
	if err != nil {
		m.base, m.baseErr = nil, err
		return
	}
	m.base, m.baseErr = s, nil
}

func (m *Model) reloadPlan() {
	p, err := plan.Load(m.d.Config.PlanPath)
	if err != nil {
		m.setErr(err)
		return
	}
	m.plan = p
}

func clonePlan(p *plan.Plan) *plan.Plan {
	b, _ := json.Marshal(p)
	var c plan.Plan
	json.Unmarshal(b, &c)
	return &c
}

// mutate edits the shared plan file under its lock, records an undo
// snapshot when the content changed, and refreshes views.
func (m *Model) mutate(fn func(*plan.Plan) error) error {
	var before *plan.Plan
	p, err := plan.Update(m.d.Config.PlanPath, func(p *plan.Plan) error {
		before = clonePlan(p)
		return fn(p)
	})
	if p != nil {
		m.plan = p
		if err == nil && before != nil && fingerprint(before) != fingerprint(p) {
			m.undoStack = append(m.undoStack, before)
			m.redoStack = nil
		}
	}
	m.applyFilter()
	return err
}

// restore swaps the plan file content for snap, returning what was there.
func (m *Model) restore(snap *plan.Plan) (*plan.Plan, error) {
	var cur *plan.Plan
	p, err := plan.Update(m.d.Config.PlanPath, func(p *plan.Plan) error {
		cur = clonePlan(p)
		*p = *clonePlan(snap)
		return nil
	})
	if p != nil {
		m.plan = p
	}
	m.applyFilter()
	return cur, err
}

func (m *Model) planUndo(n int) {
	done := 0
	for ; done < n && len(m.undoStack) > 0; done++ {
		snap := m.undoStack[len(m.undoStack)-1]
		m.undoStack = m.undoStack[:len(m.undoStack)-1]
		cur, err := m.restore(snap)
		if err != nil {
			m.setErr(err)
			return
		}
		m.redoStack = append(m.redoStack, cur)
	}
	if done == 0 {
		m.setStatus("Already at oldest change")
		return
	}
	m.setStatus("%d change(s) undone; %d rule ops · %d msgs planned", done, len(m.plan.Rules), m.plan.Messages())
}

func (m *Model) planRedo(n int) {
	done := 0
	for ; done < n && len(m.redoStack) > 0; done++ {
		snap := m.redoStack[len(m.redoStack)-1]
		m.redoStack = m.redoStack[:len(m.redoStack)-1]
		cur, err := m.restore(snap)
		if err != nil {
			m.setErr(err)
			return
		}
		m.undoStack = append(m.undoStack, cur)
	}
	if done == 0 {
		m.setStatus("Already at newest change")
		return
	}
	m.setStatus("%d change(s) redone; %d rule ops · %d msgs planned", done, len(m.plan.Rules), m.plan.Messages())
}

func (m *Model) planLen() int { return len(m.plan.Rules) + len(m.plan.Trash) }

// desired is the managed rule set after applying the plan to the base file.
func (m *Model) desired() []sieve.Rule {
	if m.base == nil {
		return nil
	}
	s := *m.base
	s.Rules = append([]sieve.Rule(nil), m.base.Rules...)
	m.plan.ApplyRules(&s)
	return s.Rules
}

func (m *Model) ruleFor(g index.Group) (sieve.Rule, bool) {
	for _, r := range m.desired() {
		if r.Kind == sieve.Kind(g.Kind) && strings.EqualFold(r.Value, g.Value) {
			return r, true
		}
	}
	return sieve.Rule{}, false
}

func (m *Model) pending(r sieve.Rule) bool {
	_, ok := m.plan.PendingRule(r.Kind, r.Value)
	return ok
}

func (m *Model) plannedKeys() map[string]bool {
	out := map[string]bool{}
	for _, t := range m.plan.Trash {
		for _, r := range t.Refs {
			out[r.Key] = true
		}
	}
	return out
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.startBusy("indexing mail"), m.startScan())
}

func (m *Model) setStatus(format string, a ...any) {
	m.status, m.statusErr = fmt.Sprintf(format, a...), false
}

func (m *Model) setErr(err error) {
	m.status, m.statusErr = err.Error(), true
}

func (m *Model) startBusy(what string) tea.Cmd {
	m.busy = what
	return m.spin.Tick
}

// ---- messages ----

type scanProgressMsg struct{ done, total int }
type scanDoneMsg struct {
	st  index.ScanStats
	err error
}
type groupsMsg struct {
	kind   index.Kind
	groups []index.Group
	err    error
}
type detailMsg struct {
	title string
	lines []string
	group index.Group
	err   error
}
type searchMsg struct {
	results []index.Message
	total   int
	err     error
}
type undoMsg struct {
	res cleanup.Result
	err error
}
type retroMsg struct {
	label string
	msgs  []index.Message
	err   error
}
type itemMsg struct {
	title string
	lines []string
	err   error
}
type applyMsg struct {
	rep      plan.Report
	target   string
	archived string
	err      error
}

// ---- commands ----

func (m *Model) startScan() tea.Cmd {
	ch := make(chan tea.Msg, 8)
	m.scanCh = ch
	ix := m.d.Index
	go func() {
		st, err := ix.Scan(func(done, total int) {
			select {
			case ch <- scanProgressMsg{done, total}:
			default:
			}
		})
		ch <- scanDoneMsg{st, err}
		close(ch)
	}()
	return waitChan(ch)
}

func waitChan(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *Model) loadGroups() tea.Cmd {
	ix, kind := m.d.Index, m.kind
	return func() tea.Msg {
		gs, err := ix.Groups(kind, time.Now())
		return groupsMsg{kind, gs, err}
	}
}

func (m *Model) loadDetail(g index.Group) tea.Cmd {
	ix := m.d.Index
	rule := ""
	if r, ok := m.ruleFor(g); ok {
		rule = "rule: " + r.Describe()
		if m.pending(r) {
			rule += " (planned)"
		}
	}
	return func() tea.Msg {
		fc, err := ix.GroupFolders(g.Kind, g.Value)
		if err != nil {
			return detailMsg{err: err}
		}
		samples, err := ix.Matching(g.Kind, []string{g.Value}, false, 50)
		lines := []string{
			fmt.Sprintf("%s   %s", g.Value, g.Name),
			fmt.Sprintf("total %d · 30d %d · 90d %d · unread %s · last %s · unsubscribe header: %v",
				g.Total, g.Recent30, g.Recent90, pct(g.Unread, g.Total), g.Last.Format("2006-01-02"), g.Unsub),
		}
		if rule != "" {
			lines = append(lines, rule)
		}
		lines = append(lines, "", "by folder:")
		for _, f := range fc {
			lines = append(lines, fmt.Sprintf("  %6d  (%d unread)  %s", f.Count, f.Unread, f.Folder))
		}
		lines = append(lines, "", "recent:")
		for _, s := range samples {
			lines = append(lines, fmt.Sprintf("  %s  %-16.16s  %s", s.Date.Format("2006-01-02"), s.Folder, s.Subject))
		}
		return detailMsg{title: "Group detail: " + string(g.Kind), lines: lines, group: g, err: err}
	}
}

func (m *Model) runSearch(text string) tea.Cmd {
	ix := m.d.Index
	return func() tea.Msg {
		q, err := query.Parse(text)
		if err != nil {
			return searchMsg{err: err}
		}
		r, total, err := ix.Search(q, 1000)
		return searchMsg{results: r, total: total, err: err}
	}
}

func groupLabel(gs []index.Group) string {
	names := make([]string, len(gs))
	for i, g := range gs {
		names[i] = string(g.Kind) + " " + g.Value
	}
	return strings.Join(names, ", ")
}

// retro finds existing (non-trash) mail matched by the targets' rules.
func (m *Model) retro(targets []index.Group) tea.Cmd {
	ix := m.d.Index
	return func() tea.Msg {
		var all []index.Message
		byKind := map[index.Kind][]string{}
		for _, g := range targets {
			byKind[g.Kind] = append(byKind[g.Kind], g.Value)
		}
		for k, vals := range byKind {
			ms, err := ix.Matching(k, vals, true, 0)
			if err != nil {
				return retroMsg{err: err}
			}
			all = append(all, ms...)
		}
		return retroMsg{label: groupLabel(targets), msgs: all}
	}
}

func (m *Model) searchAll(text string) tea.Cmd {
	ix := m.d.Index
	return func() tea.Msg {
		q, err := query.Parse(text)
		if err != nil {
			return retroMsg{err: err}
		}
		all, _, err := ix.Search(q, 0)
		return retroMsg{label: "query " + text, msgs: all, err: err}
	}
}

func (m *Model) undo() tea.Cmd {
	c := m.d.Target.Cleaner()
	return func() tea.Msg {
		res, err := c.Undo()
		return undoMsg{res: res, err: err}
	}
}

func (m *Model) showItem(it plan.TrashItem) tea.Cmd {
	ix := m.d.Index
	return func() tea.Msg {
		keys := make([]string, len(it.Refs))
		for i, r := range it.Refs {
			keys[i] = r.Key
		}
		ms, err := ix.ByKeys(keys)
		lines := []string{fmt.Sprintf("%d messages · %s", len(it.Refs), it.FolderCounts()), ""}
		for _, x := range ms {
			lines = append(lines, fmt.Sprintf("  %s  %-16.16s  %-28.28s  %s", x.Date.Format("2006-01-02"), x.Folder, x.FromAddr, x.Subject))
		}
		if len(ms) < len(it.Refs) {
			lines = append(lines, "", fmt.Sprintf("  %d not in this index (moved or not present)", len(it.Refs)-len(ms)))
		}
		return itemMsg{title: it.Label, lines: lines, err: err}
	}
}

func fingerprint(p *plan.Plan) string {
	b, _ := json.Marshal(struct {
		R []plan.RuleOp
		T []plan.TrashItem
	}{p.Rules, p.Trash})
	return string(b)
}

// applyTo runs the plan against t. Sandbox applies stamp the plan validated
// (if it didn't change meanwhile); real applies archive it.
func (m *Model) applyTo(t plan.Target, refresh func() error) tea.Cmd {
	p := m.plan
	fp := fingerprint(p)
	path, applied := m.d.Config.PlanPath, m.d.Config.AppliedDir
	return func() tea.Msg {
		if refresh != nil {
			if err := refresh(); err != nil {
				return applyMsg{target: t.Name, err: fmt.Errorf("refresh sandbox: %w", err)}
			}
		}
		rep, err := plan.Apply(p, t)
		out := applyMsg{rep: rep, target: t.Name, err: err}
		if err != nil {
			return out
		}
		if t.Name == "real" {
			out.archived, out.err = plan.Archive(path, applied)
			return out
		}
		now := time.Now()
		_, out.err = plan.Update(path, func(q *plan.Plan) error {
			if fingerprint(q) == fp {
				q.Validated = &now
			}
			return nil
		})
		return out
	}
}

// ---- staging ----

// targets is the visual range, else the marked groups, else count rows
// from the cursor.
func (m *Model) targets(count int) []index.Group {
	if len(m.view) == 0 {
		return nil
	}
	if m.visual || len(m.marked) == 0 {
		lo, hi := m.selection(count)
		return append([]index.Group(nil), m.view[lo:hi+1]...)
	}
	var out []index.Group
	for _, g := range m.groups {
		if m.marked[g.Value] {
			out = append(out, g)
		}
	}
	return out
}

func (m *Model) exitSelection() {
	m.visual = false
	m.marked = map[string]bool{}
}

func (m *Model) setKind(k index.Kind) tea.Cmd {
	if k == m.kind {
		return nil
	}
	m.kind = k
	m.marked, m.visual, m.lc = map[string]bool{}, false, listView{}
	return m.loadGroups()
}

func (m *Model) setSort(k index.SortKey) {
	m.sortKey = k
	index.SortGroups(m.groups, m.sortKey)
	m.applyFilter()
	m.setStatus("sort: %s", k)
}

func (m *Model) rescan() tea.Cmd {
	m.loadScript()
	m.reloadPlan()
	return tea.Batch(m.startBusy("indexing mail"), m.startScan())
}

func (m *Model) confirmUndoBatch() {
	m.openDialog("Undo last applied trash batch in "+m.d.Env+"?", []string{
		"Moves the most recent applied batch back to its original folders.",
		"Rule changes are not undone (edit the plan and apply again).",
	}, action{"y", "undo", func() tea.Cmd { return tea.Batch(m.startBusy("undoing"), m.undo()) }})
}

func (m *Model) confirmDiscard() {
	if m.plan.Empty() {
		m.setStatus("plan is already empty")
		return
	}
	m.openDialog("Discard the whole plan?", []string{"All planned rule changes and trash items are removed (u undoes)."},
		action{"y", "discard", func() tea.Cmd {
			if err := m.mutate(func(p *plan.Plan) error { *p = plan.Plan{}; return nil }); err != nil {
				m.setErr(err)
			} else {
				m.setStatus("plan discarded — u to undo")
			}
			return nil
		}})
}

func (m *Model) openFind() tea.Cmd {
	if m.tab == tabSearch {
		return nil
	}
	l, _, _ := m.cur()
	m.findOn, m.findStart = true, l.cur
	m.findIn.SetValue("")
	return m.findIn.Focus()
}

// keyFind handles keys in the "/" prompt: incremental jump, enter keeps,
// esc restores the cursor.
func (m *Model) keyFind(k tea.KeyMsg) tea.Cmd {
	l, n, h := m.cur()
	switch k.String() {
	case "esc", "ctrl+c":
		m.findOn = false
		m.findIn.Blur()
		l.cur = m.findStart
		l.fix(n, h)
		return nil
	case "enter":
		m.findOn = false
		m.findIn.Blur()
		if v := m.findIn.Value(); v != "" {
			m.lastFind = v
			if _, ok := m.findFromStart(v); !ok {
				m.setErr(fmt.Errorf("pattern not found: %s", v))
			} else {
				m.setStatus("/%s", v)
			}
		}
		return nil
	case "backspace":
		if m.findIn.Value() == "" {
			m.findOn = false
			m.findIn.Blur()
			return nil
		}
	}
	var cmd tea.Cmd
	m.findIn, cmd = m.findIn.Update(k)
	if i, ok := m.findFromStart(m.findIn.Value()); ok {
		l.cur = i
	} else {
		l.cur = m.findStart
	}
	l.fix(n, h)
	return cmd
}

// findFromStart searches forward (wrapping) from where "/" was opened.
func (m *Model) findFromStart(pat string) (int, bool) { return m.findFrom(pat, m.findStart, 1) }

func (m *Model) stageRules(targets []index.Group, action sieve.Action, folder string) error {
	if m.base == nil {
		return fmt.Errorf("sieve file unusable: %v", m.baseErr)
	}
	today := time.Now().Format("2006-01-02")
	var rules []sieve.Rule
	for _, g := range targets {
		r := sieve.Rule{Kind: sieve.Kind(g.Kind), Value: g.Value, Action: action, Folder: folder, Added: today}
		if err := r.Validate(); err != nil {
			return err
		}
		rules = append(rules, r)
	}
	m.marked = map[string]bool{}
	return m.mutate(func(p *plan.Plan) error {
		for _, r := range rules {
			p.SetRule("add", r)
		}
		return nil
	})
}

func (m *Model) applyFilter() {
	f := strings.ToLower(strings.TrimSpace(m.filterText))
	m.view = m.view[:0]
	for _, g := range m.groups {
		if f != "" && !strings.Contains(g.Value, f) && !strings.Contains(strings.ToLower(g.Name), f) {
			continue
		}
		if m.hideRule {
			if _, ok := m.ruleFor(g); ok {
				continue
			}
		}
		m.view = append(m.view, g)
	}
	m.lc.fix(len(m.view), m.listHeight())
}

func (m *Model) openDialog(title string, lines []string, actions ...action) {
	m.dialog = dialogState{title: title, lines: lines, actions: actions}
	m.ov = ovDialog
}

// confirmPlanTrash offers to add msgs (minus trash-folder and already
// planned ones) to the plan as one trash item.
func (m *Model) confirmPlanTrash(label string, msgs []index.Message) {
	planned := m.plannedKeys()
	var keep []index.Message
	already := 0
	for _, x := range msgs {
		switch {
		case x.Folder == m.d.Config.TrashFolder:
		case planned[x.Key]:
			already++
		default:
			keep = append(keep, x)
		}
	}
	if len(keep) == 0 {
		if already > 0 {
			m.setStatus("all %d matching messages are already in the plan", already)
		} else {
			m.setStatus("no messages outside %s to trash", m.d.Config.TrashFolder)
		}
		return
	}
	perFolder := map[string]int{}
	for _, x := range keep {
		perFolder[x.Folder]++
	}
	folders := make([]string, 0, len(perFolder))
	for f := range perFolder {
		folders = append(folders, f)
	}
	sort.Slice(folders, func(i, j int) bool { return perFolder[folders[i]] > perFolder[folders[j]] })

	lines := []string{fmt.Sprintf("Add %d messages to the plan (moved to %q when the plan is applied):", len(keep), m.d.Config.TrashFolder)}
	if already > 0 {
		lines = append(lines, fmt.Sprintf("(%d more already planned)", already))
	}
	lines = append(lines, "")
	for _, f := range folders {
		lines = append(lines, fmt.Sprintf("  %6d  from %s", perFolder[f], f))
	}
	lines = append(lines, "", "Newest:")
	for i, x := range keep {
		if i == 15 {
			lines = append(lines, fmt.Sprintf("  … and %d more", len(keep)-15))
			break
		}
		lines = append(lines, fmt.Sprintf("  %s  %-28.28s  %s", x.Date.Format("2006-01-02"), x.FromAddr, x.Subject))
	}
	refs := make([]plan.Ref, len(keep))
	for i, x := range keep {
		refs[i] = plan.Ref{Key: x.Key, Folder: x.Folder}
	}
	m.openDialog("Plan trash: "+label, lines, action{"y", "add to plan", func() tea.Cmd {
		var n int
		if err := m.mutate(func(p *plan.Plan) error { n = p.AddTrash(label, refs); return nil }); err != nil {
			m.setErr(err)
			return nil
		}
		m.setStatus("planned %d messages for trash — A to apply", n)
		return nil
	}})
}

func (m *Model) openPicker(targets []index.Group) error {
	folders, err := maildir.Folders(m.d.Config.MailRoot)
	if err != nil {
		return err
	}
	var out []string
	for _, f := range folders {
		if f != m.d.Config.TrashFolder && f != "INBOX" {
			out = append(out, f)
		}
	}
	m.picker = pickerState{folders: out, targets: targets}
	m.ov = ovPicker
	return nil
}

// planSummary describes the plan against this environment's sieve file.
func (m *Model) planSummary() []string {
	var lines []string
	if n := len(m.plan.Rules); n > 0 {
		lines = append(lines, fmt.Sprintf("Rules (%d):", n))
		for _, o := range m.plan.Rules {
			lines = append(lines, fmt.Sprintf("  %-6s %s", o.Op, o.Rule.Describe()))
		}
		cur, next, err := m.d.Target.SieveDiff(m.plan)
		switch {
		case err != nil:
			lines = append(lines, "  (cannot diff: "+err.Error()+")")
		case cur == next:
			lines = append(lines, "  (no change to the sieve script)")
		default:
			lines = append(lines, "", "Sieve diff ("+m.d.Target.Publisher.Describe()+"):")
			lines = append(lines, collapse(sieve.Diff(cur, next), 3)...)
		}
		lines = append(lines, "")
	}
	if len(m.plan.Trash) > 0 {
		lines = append(lines, fmt.Sprintf("Trash (%d messages → %q, one undoable batch):", m.plan.Messages(), m.d.Config.TrashFolder))
		for _, t := range m.plan.Trash {
			lines = append(lines, fmt.Sprintf("  %6d  %s  (%s)", len(t.Refs), t.Label, t.FolderCounts()))
		}
	}
	return lines
}

func (m *Model) validatedLine() string {
	if v := m.plan.Validated; v != nil {
		return "✓ validated in sandbox " + v.Format("2006-01-02 15:04")
	}
	return "⚠ not validated in sandbox since last change"
}

func (m *Model) openApply() {
	m.reloadPlan()
	if m.plan.Empty() {
		m.setStatus("plan is empty — stage rules or trash first")
		return
	}
	lines := append(m.planSummary(), "", m.validatedLine())
	if m.d.Env == "sandbox" {
		m.openDialog("Apply plan to SANDBOX", lines, action{"y", "apply to sandbox", func() tea.Cmd {
			return tea.Batch(m.startBusy("applying plan to sandbox"), m.applyTo(m.d.Target, nil))
		}})
		return
	}
	var acts []action
	if sb := m.d.Sandbox; sb != nil {
		acts = append(acts, action{"s", "validate in fresh sandbox", func() tea.Cmd {
			return tea.Batch(m.startBusy("cloning sandbox and applying plan"), m.applyTo(sb.Target, sb.Refresh))
		}})
	}
	acts = append(acts, action{"r", "apply to REAL…", func() tea.Cmd {
		if !m.d.Target.ChangesSieve(m.plan) {
			m.confirmReal(nil)
			return nil
		}
		pub, local := m.d.Target.Publisher, m.d.Config.SieveFile
		return tea.Batch(m.startBusy("checking the Sieve server"), func() tea.Msg {
			st, err := pub.Status(local)
			return precheckMsg{st, err}
		})
	}})
	m.openDialog("Apply plan", lines, acts...)
}

type precheckMsg struct {
	st  sieve.ServerStatus
	err error
}

// confirmReal opens the final REAL confirmation. st is the server precheck
// (nil when the plan doesn't change the sieve script). Unsafe server states
// get an informational dialog with no apply action.
func (m *Model) confirmReal(st *sieve.ServerStatus) {
	ours := m.d.Config.SieveScript
	lines := []string{
		fmt.Sprintf("This moves %d real messages and publishes to %s.", m.plan.Messages(), m.d.Target.Publisher.Describe()),
		"mbsync propagates the moves on its next run. U undoes the trash batch (not the rules).",
		"",
	}
	target := m.d.Target
	if st != nil {
		other := st.OtherActive(ours)
		switch {
		case !st.Exists && other != "":
			m.openDialog("Can't apply: another script is active", []string{
				sieve.ErrOtherActive{Ours: ours, Active: other}.Error(),
				"", "Nothing was changed. `sluice doctor -online` shows the server state.",
			})
			return
		case st.Drift:
			m.openDialog("Can't apply: server script differs from local", []string{
				fmt.Sprintf("%q on the server doesn't match %s.", ours, m.d.Config.SieveFile),
				"Re-download it (e.g. `sieve-edit`) so local matches the server, then apply again.",
				"", "Nothing was changed.",
			})
			return
		case !st.Exists:
			lines = append(lines, fmt.Sprintf("⚠ server has no %q: this first apply creates it from %s plus the plan, and activates it.", ours, m.d.Config.SieveFile))
		case other != "":
			lines = append(lines, fmt.Sprintf("⚠ %q is the active script now: applying makes %q active instead (switching %q off).", other, ours, other))
			target.AllowSwitchFrom = other
		default:
			lines = append(lines, fmt.Sprintf("✓ server: %q matches local", ours))
		}
	}
	lines = append(lines, m.validatedLine())
	m.openDialog("Apply plan to REAL mail and server?", lines, action{"y", "apply to real", func() tea.Cmd {
		return tea.Batch(m.startBusy("applying plan to real"), m.applyTo(target, nil))
	}})
}

type doctorMsg struct{ lines []string }

// runDoctor runs the offline requirement checks for this environment.
func (m *Model) runDoctor() tea.Cmd {
	cfg := m.d.Config
	return tea.Batch(m.startBusy("running doctor"), func() tea.Msg {
		return doctorMsg{doctor.Run(cfg, doctor.Options{ConfigPath: config.DefaultPath()}).Lines()}
	})
}

// applyCommand implements :apply [sandbox|real].
func (m *Model) applyCommand(arg string) tea.Cmd {
	m.openApply()
	if arg == "" || m.ov != ovDialog {
		return nil
	}
	key := map[string]string{"sandbox": "s", "real": "r"}[arg]
	if m.d.Env == "sandbox" && arg == "sandbox" {
		key = "y"
	}
	for _, a := range m.dialog.actions {
		if a.key == key {
			m.ov = ovNone
			return a.run()
		}
	}
	m.ov = ovNone
	m.setErr(fmt.Errorf(":apply %s is not available in %s mode", arg, m.d.Env))
	return nil
}

// collapse elides unchanged diff lines further than ctx from any change.
func collapse(diff []string, ctx int) []string {
	keep := make([]bool, len(diff))
	for i, l := range diff {
		if !strings.HasPrefix(l, "  ") {
			for j := max(0, i-ctx); j <= min(len(diff)-1, i+ctx); j++ {
				keep[j] = true
			}
		}
	}
	var out []string
	skipped := 0
	for i, l := range diff {
		if keep[i] {
			if skipped > 0 {
				out = append(out, fmt.Sprintf("  … %d unchanged lines", skipped))
				skipped = 0
			}
			out = append(out, l)
		} else {
			skipped++
		}
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("  … %d unchanged lines", skipped))
	}
	return out
}
