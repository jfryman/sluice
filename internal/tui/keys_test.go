package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/sweep"
)

func testModel(t *testing.T, groups int) *Model {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.PlanPath = filepath.Join(dir, "plan.json")
	cfg.SieveFile = filepath.Join(dir, "kolab.sieve")
	m := New(Deps{Env: "sandbox", Config: cfg, Target: plan.Target{
		Name: "sandbox", SieveFile: cfg.SieveFile, Publisher: sieve.FilePublisher{Dir: filepath.Join(dir, "remote"), Script: "kolab"},
	}})
	m.w, m.h = 120, 24 // listHeight = 18
	for i := 0; i < groups; i++ {
		m.groups = append(m.groups, index.Group{Kind: index.KindDomain, Value: fmt.Sprintf("d%02d.example", i), Total: 1})
	}
	m.applyFilter()
	return m
}

// press sends keys; "ctrl+x" style names and single runes are supported.
func press(m *Model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
		case "ctrl+e":
			msg = tea.KeyMsg{Type: tea.KeyCtrlE}
		case "ctrl+r":
			msg = tea.KeyMsg{Type: tea.KeyCtrlR}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.key(msg)
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		press(m, string(r))
	}
}

func TestMotions(t *testing.T) {
	m := testModel(t, 50)
	h := m.listHeight()
	press(m, "5", "j")
	if m.lc.cur != 5 {
		t.Fatalf("5j: cur = %d", m.lc.cur)
	}
	press(m, "G")
	if m.lc.cur != 49 || m.lc.off != 50-h {
		t.Fatalf("G: cur=%d off=%d", m.lc.cur, m.lc.off)
	}
	press(m, "g", "g")
	if m.lc.cur != 0 || m.lc.off != 0 {
		t.Fatalf("gg: cur=%d off=%d", m.lc.cur, m.lc.off)
	}
	press(m, "1", "0", "G")
	if m.lc.cur != 9 {
		t.Fatalf("10G: cur=%d", m.lc.cur)
	}
	press(m, "ctrl+d")
	if m.lc.cur != 9+h/2 {
		t.Fatalf("ctrl+d: cur=%d", m.lc.cur)
	}
	press(m, "L")
	if m.lc.cur != m.lc.off+h-1-scrolloff {
		t.Fatalf("L: cur=%d off=%d", m.lc.cur, m.lc.off)
	}
	press(m, "z", "z")
	if m.lc.off != m.lc.cur-h/2 {
		t.Fatalf("zz: cur=%d off=%d", m.lc.cur, m.lc.off)
	}
	// Minimal scrolling: moving down within the window doesn't scroll.
	m.lc = listView{}
	press(m, "j", "j", "j")
	if m.lc.off != 0 {
		t.Fatalf("scrolled too early: off=%d", m.lc.off)
	}
	press(m, "ctrl+e")
	if m.lc.off != 1 || m.lc.cur < 1+scrolloff {
		t.Fatalf("ctrl+e: cur=%d off=%d", m.lc.cur, m.lc.off)
	}
}

func TestPrefixAndTabs(t *testing.T) {
	m := testModel(t, 3)
	press(m, "4", "g", "t")
	if m.tab != tabSearch {
		t.Fatalf("4gt: tab=%d", m.tab)
	}
	press(m, "l")
	if m.tab != tabPlan {
		t.Fatalf("l: tab=%d", m.tab)
	}
	press(m, "h", "h")
	if m.tab != tabRules {
		t.Fatalf("hh: tab=%d", m.tab)
	}
	press(m, "g", "T")
	if m.tab != tabSanebox {
		t.Fatalf("gT: tab=%d", m.tab)
	}
	press(m, "g", "T")
	if m.tab != tabCandidates {
		t.Fatalf("gTgT: tab=%d", m.tab)
	}
	press(m, "g")
	if m.keys.prefix != "g" || m.keys.show() != "g" {
		t.Fatalf("pending prefix = %+v", m.keys)
	}
	press(m, "q") // gq is not a binding: clears, does not quit
	if m.keys != (keyState{}) {
		t.Fatalf("prefix not cleared: %+v", m.keys)
	}
}

func TestVisualAndPlanUndo(t *testing.T) {
	m := testModel(t, 10)
	press(m, "j", "V", "j", "j")
	ts := m.targets(0)
	if len(ts) != 3 || ts[0].Value != "d01.example" || ts[2].Value != "d03.example" {
		t.Fatalf("visual targets = %+v", ts)
	}
	if err := m.stageRules(ts, sieve.ActionRead, ""); err != nil {
		t.Fatal(err)
	}
	m.exitSelection()
	if len(m.plan.Rules) != 3 {
		t.Fatalf("plan rules = %d", len(m.plan.Rules))
	}

	// Plan tab: 2dd drops two items; u restores; ctrl+r redoes.
	press(m, "5", "g", "t", "2", "d", "d")
	if len(m.plan.Rules) != 1 {
		t.Fatalf("2dd: rules = %d", len(m.plan.Rules))
	}
	press(m, "u")
	if len(m.plan.Rules) != 3 {
		t.Fatalf("u: rules = %d", len(m.plan.Rules))
	}
	press(m, "ctrl+r")
	if len(m.plan.Rules) != 1 {
		t.Fatalf("ctrl+r: rules = %d", len(m.plan.Rules))
	}
	press(m, "u", "u")
	if len(m.plan.Rules) != 0 {
		t.Fatalf("uu: rules = %d", len(m.plan.Rules))
	}
	if p, _ := plan.Load(m.d.Config.PlanPath); len(p.Rules) != 0 {
		t.Fatal("undo not persisted")
	}
	press(m, "u")
	if m.status != "Already at oldest change" {
		t.Fatalf("status = %q", m.status)
	}
}

func TestFindAndCommands(t *testing.T) {
	m := testModel(t, 30)
	press(m, "/")
	typeText(m, "d2")
	press(m, "enter")
	if m.lc.cur != 20 {
		t.Fatalf("/d2: cur=%d", m.lc.cur)
	}
	press(m, "n")
	if m.lc.cur != 21 {
		t.Fatalf("n: cur=%d", m.lc.cur)
	}
	press(m, "N", "N")
	if m.lc.cur != 29 || m.status != "search hit TOP, continuing at BOTTOM" {
		t.Fatalf("NN: cur=%d status=%q", m.lc.cur, m.status)
	}

	press(m, ":")
	typeText(m, "fil")
	press(m, "tab")
	if m.cmdInput.Value() != "filter " {
		t.Fatalf("completion = %q", m.cmdInput.Value())
	}
	typeText(m, "d1")
	press(m, "enter")
	if len(m.view) != 10 {
		t.Fatalf(":filter d1: %d groups", len(m.view))
	}
	press(m, ":")
	typeText(m, "5")
	press(m, "enter")
	if m.lc.cur != 4 {
		t.Fatalf(":5: cur=%d", m.lc.cur)
	}
	press(m, ":")
	typeText(m, "sort 90d")
	press(m, "enter")
	if m.sortKey != index.SortRecent {
		t.Fatalf("sort = %v", m.sortKey)
	}
	press(m, ":")
	typeText(m, "bogus")
	press(m, "enter")
	if !m.statusErr {
		t.Fatal("expected error for unknown command")
	}
	press(m, "?")
	if m.ov != ovDialog || m.dialog.title != "Help — Candidates" {
		t.Fatalf("help: ov=%d title=%q", m.ov, m.dialog.title)
	}
	press(m, "esc")
	if m.ov != ovNone {
		t.Fatal("esc did not close help")
	}
}

func TestConfirmRealServerStates(t *testing.T) {
	m := testModel(t, 1)
	m.d.Config.SieveScript = "kolab"
	cases := []struct {
		st      sieve.ServerStatus
		actions int
		title   string
	}{
		{sieve.ServerStatus{Exists: true, Active: "kolab"}, 1, "Apply plan to REAL mail and server?"},
		{sieve.ServerStatus{}, 1, "Apply plan to REAL mail and server?"},                                        // first publish
		{sieve.ServerStatus{Exists: true, Active: "rc"}, 1, "Apply plan to REAL mail and server?"},              // switch, confirmed by y
		{sieve.ServerStatus{Active: "rc", Scripts: []string{"rc"}}, 0, "Can't apply: another script is active"}, // blocked
		{sieve.ServerStatus{Exists: true, Active: "kolab", Drift: true}, 0, "Can't apply: server script differs from local"},
	}
	for i, c := range cases {
		st := c.st
		m.confirmReal(&st)
		if m.dialog.title != c.title || len(m.dialog.actions) != c.actions {
			t.Errorf("case %d: title=%q actions=%d", i, m.dialog.title, len(m.dialog.actions))
		}
	}
	st := sieve.ServerStatus{Exists: true, Active: "rc"}
	m.confirmReal(&st)
	found := false
	for _, l := range m.dialog.lines {
		found = found || strings.Contains(l, `switching "rc" off`)
	}
	if !found {
		t.Errorf("switch warning missing: %q", m.dialog.lines)
	}
}

// After a sandbox validation the report dialog says "A r y applies to real";
// that sequence must work without closing the report first.
func TestApplyFromReportDialog(t *testing.T) {
	m := testModel(t, 1)
	m.d.Env, m.d.Target.Name = "real", "real"
	if err := m.mutate(func(p *plan.Plan) error {
		p.AddTrash("x", []plan.Ref{{Key: "k1", Folder: "Archive"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.openDialog("Applied to sandbox", []string{"report"})
	press(m, "A")
	if m.dialog.title != "Apply plan" {
		t.Fatalf("A from report: dialog %q", m.dialog.title)
	}
	press(m, "r")
	if m.dialog.title != "Apply plan to REAL mail and server?" {
		t.Fatalf("A r: dialog %q", m.dialog.title)
	}
	// A inside an action dialog doesn't replace it.
	press(m, "A")
	if m.dialog.title != "Apply plan to REAL mail and server?" || m.ov != ovDialog {
		t.Fatalf("A in confirm dialog: ov=%v dialog %q", m.ov, m.dialog.title)
	}
}

func TestSweepLogAndCleanupMsgs(t *testing.T) {
	m := testModel(t, 1)
	m.Update(sweepLogMsg{size: 10, seen: 2, outs: []sweep.Outcome{
		{Kind: "sender", Value: "a@spam.example", Result: sweep.Published, Trashed: 4},
		{Kind: "sender", Value: "a@spam.example", Result: sweep.Published, Trashed: 4},
	}})
	if m.status != "swept +Sluice: blocked a@spam.example (4 trashed)" || m.sweepSeen != 2 {
		t.Errorf("status %q seen %d", m.status, m.sweepSeen)
	}

	item := cleanupItem{label: "cleanup: sender a@spam.example", refs: []plan.Ref{{Key: "k1", Folder: "Archive"}, {Key: "k2", Folder: "INBOX"}}}
	m.Update(cleanupMsg{items: []cleanupItem{item}, nonTrash: 1})
	if len(m.plan.Trash) != 1 || m.plan.Messages() != 2 || m.ov != ovDialog {
		t.Fatalf("plan %+v ov %v", m.plan.Trash, m.ov)
	}
	// Re-running adds nothing already planned.
	m.Update(cleanupMsg{items: []cleanupItem{item}})
	if len(m.plan.Trash) != 1 || !strings.HasPrefix(m.dialog.lines[0], "No existing mail") {
		t.Fatalf("rerun: plan %+v dialog %q", m.plan.Trash, m.dialog.lines)
	}
	press(m, "esc", "u")
	if !m.plan.Empty() {
		t.Errorf("u should undo the queued cleanup: %+v", m.plan)
	}
}

func TestSearchRules(t *testing.T) {
	m := testModel(t, 1)
	g := func(k index.Kind, v string, n int) index.Group { return index.Group{Kind: k, Value: v, Total: n} }
	m.Update(searchRulesMsg{query: "name:x", rules: []searchRule{
		{group: g(index.KindSender, "a@spam.example", 5)},
		{group: g(index.KindList, "deals.example", 3)},
		{group: g(index.KindSender, "pal@friend.example", 1), held: "someone you've sent mail to"},
	}})
	if m.ov != ovDialog || len(m.dialog.actions) != 1 || !strings.Contains(strings.Join(m.dialog.lines, "\n"), "Left out") {
		t.Fatalf("dialog %q %v", m.dialog.lines, m.dialog.actions)
	}
	press(m, "y")
	if len(m.plan.Rules) != 2 {
		t.Fatalf("want 2 planned rules, got %+v", m.plan.Rules)
	}
	for _, r := range m.plan.Rules {
		if r.Rule.Action != sieve.ActionTrash || r.Rule.Value == "pal@friend.example" {
			t.Errorf("rule %+v", r.Rule)
		}
	}

	// Already-planned rules are not added again; too many new ones refuse.
	m.Update(searchRulesMsg{query: "name:x", rules: []searchRule{{group: g(index.KindSender, "a@spam.example", 5)}}})
	if !strings.Contains(m.dialog.lines[0], "Add 0 trash rule") {
		t.Errorf("existing rule: %q", m.dialog.lines)
	}
	var many []searchRule
	for i := 0; i <= maxSearchRules; i++ {
		many = append(many, searchRule{group: g(index.KindSender, fmt.Sprintf("s%d@x.example", i), 1)})
	}
	m.Update(searchRulesMsg{query: "older:1y", rules: many})
	if len(m.dialog.actions) != 0 || !strings.Contains(m.dialog.lines[0], "limit") {
		t.Errorf("limit: %q", m.dialog.lines)
	}
}

func TestSaneboxTab(t *testing.T) {
	m := testModel(t, 3) // Candidates rows d00–d02
	m.sbFolders = []index.FolderCount{{Folder: "+SaneLater", Count: 5}, {Folder: "+SaneNews", Count: 2}}
	m.sbFolder = "+SaneLater"
	m.sbGroups = []index.Group{
		{Kind: index.KindSender, Value: "a@spam.example", Total: 3},
		{Kind: index.KindList, Value: "deals.example", Total: 2},
	}
	m.applyFilter()
	press(m, "2", "g", "t")
	if m.tab != tabSanebox || len(m.sbView) != 2 {
		t.Fatalf("tab=%d view=%d", m.tab, len(m.sbView))
	}
	// Marks are per list: SaneBox marks don't leak into Candidates.
	press(m, "space", "space")
	if ts := m.targets(0); len(ts) != 2 || ts[1].Kind != index.KindList {
		t.Fatalf("targets = %+v", ts)
	}
	if len(m.marked) != 0 {
		t.Fatalf("candidates marks = %v", m.marked)
	}
	// Rules staged from SaneBox keep each row's kind.
	if err := m.stageRules(m.targets(0), sieve.ActionTrash, ""); err != nil {
		t.Fatal(err)
	}
	if len(m.plan.Rules) != 2 || m.plan.Rules[1].Rule.Kind != sieve.KindList || len(m.sbMarked) != 0 {
		t.Fatalf("plan = %+v marks = %v", m.plan.Rules, m.sbMarked)
	}
	press(m, "z", "h")
	if len(m.sbView) != 0 {
		t.Fatalf("zh should hide ruled senders: %+v", m.sbView)
	}
	// ] moves to the next folder (and wraps), resetting the cursor.
	m.d.Index = nil
	m.switchSanebox(1)
	if m.sbFolder != "+SaneNews" {
		t.Fatalf("]: folder %q", m.sbFolder)
	}
	m.switchSanebox(1)
	if m.sbFolder != "+SaneLater" {
		t.Fatalf("] wrap: folder %q", m.sbFolder)
	}
}

func TestSaneboxFolders(t *testing.T) {
	all := []index.FolderCount{{Folder: "Archive", Count: 9}, {Folder: "+SaneLater", Count: 5}, {Folder: "+SaneTomorrow"}, {Folder: "+SaneNews", Count: 2}}
	got := saneboxFolders(all, nil)
	if len(got) != 2 || got[0].Folder != "+SaneLater" || got[1].Folder != "+SaneNews" {
		t.Errorf("auto: %+v", got)
	}
	got = saneboxFolders(all, []string{"+SaneNews", "+SaneTomorrow", "+SaneMissing"})
	if len(got) != 3 || got[0].Count != 2 || got[2].Folder != "+SaneMissing" {
		t.Errorf("configured: %+v", got)
	}
}

func TestBusyIndicator(t *testing.T) {
	m := testModel(t, 2)
	ch := make(chan tea.Msg, 1)
	m.progCh = ch
	m.startBusy("applying plan to REAL")
	m.busyStart = m.busyStart.Add(-75 * time.Second)
	m.Update(progressMsg{ch: ch, stage: "moving 3/9 messages"})
	if b := m.busyBadge(); !strings.Contains(b, "applying plan to REAL · moving 3/9 messages · 1:15") {
		t.Errorf("badge = %q", b)
	}
	if !strings.Contains(m.viewTabs(), "moving 3/9 messages") {
		t.Errorf("header lacks the badge: %q", m.viewTabs())
	}
	// Progress from an older operation doesn't overwrite the current step.
	m.Update(progressMsg{ch: make(chan tea.Msg), stage: "stale"})
	if m.busyStage != "moving 3/9 messages" {
		t.Errorf("stage = %q", m.busyStage)
	}
	// Action keys are refused with a reason; motions still work.
	press(m, "t")
	if !m.statusErr || !strings.HasPrefix(m.status, "busy — applying plan to REAL") {
		t.Errorf("status = %q", m.status)
	}
	press(m, "j")
	if m.lc.cur != 1 {
		t.Errorf("motion while busy: cur = %d", m.lc.cur)
	}
}

func TestSyncAfterServerMoves(t *testing.T) {
	m := testModel(t, 1)
	m.d.Config.SyncCmd = nil
	if cmd := m.syncAfterServerMoves(); cmd != nil {
		t.Error("no sync_cmd: expected no command")
	}
	m.busy = "syncing mail"
	m.Update(syncMsg{busy: true})
	if m.busy != "" || !strings.Contains(m.status, "already running") {
		t.Errorf("busy sync: busy=%q status=%q", m.busy, m.status)
	}
	m.Update(syncMsg{err: fmt.Errorf("exit 1")})
	if !m.statusErr || !strings.Contains(m.status, "next sync") {
		t.Errorf("failed sync: %q", m.status)
	}
}
