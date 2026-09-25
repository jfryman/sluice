package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
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
	press(m, "3", "g", "t")
	if m.tab != tabSearch {
		t.Fatalf("3gt: tab=%d", m.tab)
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
	if m.tab != tabCandidates {
		t.Fatalf("gT: tab=%d", m.tab)
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
	press(m, "4", "g", "t", "2", "d", "d")
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
