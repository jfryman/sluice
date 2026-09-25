package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- viewport ----

const scrolloff = 2

// listView is a cursor plus scroll offset with vim-like minimal scrolling.
type listView struct{ cur, off int }

// fix bounds cur and off for n rows shown h at a time, scrolling just
// enough to keep scrolloff rows around the cursor.
func (l *listView) fix(n, h int) {
	if n <= 0 {
		l.cur, l.off = 0, 0
		return
	}
	h = max(h, 1)
	l.cur = max(0, min(l.cur, n-1))
	so := min(scrolloff, (h-1)/2)
	if l.cur < l.off+so {
		l.off = l.cur - so
	}
	if l.cur > l.off+h-1-so {
		l.off = l.cur - h + 1 + so
	}
	l.off = max(0, min(l.off, n-h))
}

func (l *listView) window(n, h int) (int, int) {
	l.fix(n, h)
	return l.off, min(l.off+max(h, 1), n)
}

// ---- key state ----

// prefixKeys start two-key sequences (gg, gt, zz, zh, dd, ZZ, …).
const prefixKeys = "gzdZ"

type keyState struct {
	count  int
	prefix string
}

func (k keyState) show() string {
	s := k.prefix
	if k.count > 0 {
		s = strconv.Itoa(k.count) + s
	}
	return s
}

func or1(n int) int { return max(n, 1) }

// ---- bindings ----

type scope int

const (
	scGlobal scope = iota
	scList
	scCandidates
	scRules
	scSearch
	scPlan
	scDialog
)

func (s scope) String() string {
	return [...]string{"global", "lists", "candidates", "rules", "search", "plan", "dialog"}[s]
}

type binding struct {
	keys  string
	scope scope
	act   bool
	help  string
	run   func(m *Model, count int) tea.Cmd
}

var bindings []binding

func bind(sc scope, keys string, act bool, help string, run func(m *Model, count int) tea.Cmd) {
	for _, k := range strings.Split(keys, " ") {
		bindings = append(bindings, binding{keys: k, scope: sc, act: act, help: help, run: run})
	}
}

// keyName normalises a bubbletea key to binding notation.
func keyName(k tea.KeyMsg) string {
	s := k.String()
	if s == " " {
		return "space"
	}
	return s
}

func (m *Model) scopes() []scope {
	if m.ov == ovDialog {
		return []scope{scDialog}
	}
	var tabScope scope
	switch m.tab {
	case tabCandidates:
		tabScope = scCandidates
	case tabRules:
		tabScope = scRules
	case tabSearch:
		tabScope = scSearch
	default:
		tabScope = scPlan
	}
	return []scope{tabScope, scList, scGlobal}
}

// dispatch runs the binding for key k, handling counts and prefixes.
func (m *Model) dispatch(k tea.KeyMsg) tea.Cmd {
	name := keyName(k)
	if len(name) == 1 && name[0] >= '0' && name[0] <= '9' && m.keys.prefix == "" {
		if name != "0" || m.keys.count > 0 {
			m.keys.count = min(m.keys.count*10+int(name[0]-'0'), 99999)
			return nil
		}
	}
	seq := name
	if m.keys.prefix != "" {
		seq = m.keys.prefix + name
	}
	scopes := m.scopes()
	var partial bool
	for _, sc := range scopes {
		for _, b := range bindings {
			if b.scope != sc {
				continue
			}
			if b.keys == seq {
				count := m.keys.count
				m.keys = keyState{}
				if b.act && m.busy != "" {
					return nil
				}
				return b.run(m, count)
			}
			// Two-key sequences start with a prefix key; named keys like
			// "home" or "esc" are never sequences.
			if len(seq) == 1 && strings.Contains(prefixKeys, seq) && len(b.keys) == 2 && b.keys[:1] == seq {
				partial = true
			}
		}
	}
	if partial {
		m.keys.prefix = seq
		return nil
	}
	m.keys = keyState{}
	return nil
}

// ---- current list helpers ----

// cur returns the active list view, its length and visible height.
func (m *Model) cur() (*listView, int, int) {
	switch m.tab {
	case tabCandidates:
		return &m.lc, len(m.view), m.listHeight()
	case tabRules:
		return &m.lr, len(m.desired()), m.listHeight()
	case tabSearch:
		return &m.ls, len(m.results), m.listHeight()
	default:
		return &m.lp, m.planLen(), m.listHeight()
	}
}

// listHeight is the number of list rows the current tab renders.
func (m *Model) listHeight() int {
	switch m.tab {
	case tabRules:
		return max(m.bodyHeight()-3, 1)
	default:
		return max(m.bodyHeight()-2, 1)
	}
}

// motion wraps a cursor/offset change on the active list.
func motion(f func(l *listView, n, h, count int)) func(*Model, int) tea.Cmd {
	return func(m *Model, count int) tea.Cmd {
		l, n, h := m.cur()
		f(l, n, h, count)
		l.fix(n, h)
		return nil
	}
}

// selection returns [lo, hi] of the visual range, or the count-sized range
// at the cursor.
func (m *Model) selection(count int) (int, int) {
	l, n, _ := m.cur()
	if m.visual {
		lo, hi := min(m.vanchor, l.cur), max(m.vanchor, l.cur)
		return lo, min(hi, n-1)
	}
	return l.cur, min(l.cur+or1(count)-1, n-1)
}

func (m *Model) switchTab(t tab) {
	m.tab = (t%numTabs + numTabs) % numTabs
	m.visual = false
	m.reloadPlan()
}

// ---- search within lists (/ n N) ----

func (m *Model) rowText(i int) string {
	switch m.tab {
	case tabCandidates:
		g := m.view[i]
		return g.Value + " " + g.Name + " " + g.Subject
	case tabRules:
		return m.desired()[i].Describe()
	case tabPlan:
		if i < len(m.plan.Rules) {
			o := m.plan.Rules[i]
			return o.Op + " " + o.Rule.Describe()
		}
		return m.plan.Trash[i-len(m.plan.Rules)].Label
	}
	return ""
}

// findFrom returns the next row matching pat starting after/before from.
func (m *Model) findFrom(pat string, from, dir int) (int, bool) {
	_, n, _ := m.cur()
	if pat == "" || n == 0 {
		return 0, false
	}
	pat = strings.ToLower(pat)
	for step := 1; step <= n; step++ {
		i := ((from+dir*step)%n + n) % n
		if strings.Contains(strings.ToLower(m.rowText(i)), pat) {
			return i, true
		}
	}
	return 0, false
}

func (m *Model) searchNext(dir, count int) tea.Cmd {
	if m.lastFind == "" {
		m.setErr(fmt.Errorf("no previous search"))
		return nil
	}
	l, n, h := m.cur()
	pos := l.cur
	for i := 0; i < or1(count); i++ {
		j, ok := m.findFrom(m.lastFind, pos, dir)
		if !ok {
			m.setErr(fmt.Errorf("pattern not found: %s", m.lastFind))
			return nil
		}
		if (dir > 0 && j <= pos) || (dir < 0 && j >= pos) {
			if dir > 0 {
				m.setStatus("search hit BOTTOM, continuing at TOP")
			} else {
				m.setStatus("search hit TOP, continuing at BOTTOM")
			}
		} else {
			m.setStatus("/%s", m.lastFind)
		}
		pos = j
	}
	l.cur = pos
	l.fix(n, h)
	return nil
}

func init() {
	// ---- global ----
	bind(scGlobal, "h gT", false, "previous tab", func(m *Model, c int) tea.Cmd { m.switchTab(m.tab - tab(or1(c))); return nil })
	bind(scGlobal, "l", false, "next tab", func(m *Model, c int) tea.Cmd { m.switchTab(m.tab + tab(or1(c))); return nil })
	bind(scGlobal, "gt", false, "next tab / {n}gt: tab n", func(m *Model, c int) tea.Cmd {
		if c > 0 {
			m.switchTab(tab(min(c, int(numTabs)) - 1))
		} else {
			m.switchTab(m.tab + 1)
		}
		return nil
	})
	bind(scGlobal, "tab", false, "next tab", func(m *Model, c int) tea.Cmd { m.switchTab(m.tab + 1); return nil })
	bind(scGlobal, "shift+tab", false, "previous tab", func(m *Model, c int) tea.Cmd { m.switchTab(m.tab - 1); return nil })
	bind(scGlobal, "u", true, "undo last plan edit", func(m *Model, c int) tea.Cmd { m.planUndo(or1(c)); return nil })
	bind(scGlobal, "ctrl+r", true, "redo plan edit", func(m *Model, c int) tea.Cmd { m.planRedo(or1(c)); return nil })
	bind(scGlobal, "U", true, "undo last applied trash batch", func(m *Model, c int) tea.Cmd { m.confirmUndoBatch(); return nil })
	bind(scGlobal, "A", true, "apply plan", func(m *Model, c int) tea.Cmd { m.openApply(); return nil })
	bind(scGlobal, ":", false, "command line", func(m *Model, c int) tea.Cmd { return m.openCmdline() })
	bind(scGlobal, "?", false, "help", func(m *Model, c int) tea.Cmd { m.openHelp(); return nil })
	bind(scGlobal, "R", true, "rescan index, reload sieve + plan", func(m *Model, c int) tea.Cmd { return m.rescan() })
	bind(scGlobal, "q ZZ", false, "quit (plan persists)", func(m *Model, c int) tea.Cmd { return tea.Quit })

	// ---- lists ----
	bind(scList, "j down", false, "down", motion(func(l *listView, n, h, c int) { l.cur += or1(c) }))
	bind(scList, "k up", false, "up", motion(func(l *listView, n, h, c int) { l.cur -= or1(c) }))
	bind(scList, "gg home", false, "first line / {n}gg: line n", motion(func(l *listView, n, h, c int) { l.cur = or1(c) - 1 }))
	bind(scList, "G end", false, "last line / {n}G: line n", motion(func(l *listView, n, h, c int) {
		if c > 0 {
			l.cur = c - 1
		} else {
			l.cur = n - 1
		}
	}))
	bind(scList, "ctrl+d", false, "half page down", motion(func(l *listView, n, h, c int) { l.cur += h / 2; l.off += h / 2 }))
	bind(scList, "ctrl+u", false, "half page up", motion(func(l *listView, n, h, c int) { l.cur -= h / 2; l.off -= h / 2 }))
	bind(scList, "ctrl+f pgdown", false, "page down", motion(func(l *listView, n, h, c int) { d := or1(c) * max(h-2, 1); l.cur += d; l.off += d }))
	bind(scList, "ctrl+b pgup", false, "page up", motion(func(l *listView, n, h, c int) { d := or1(c) * max(h-2, 1); l.cur -= d; l.off -= d }))
	bind(scList, "H", false, "top of screen", motion(func(l *listView, n, h, c int) {
		l.cur = l.off + max(or1(c)-1, min(scrolloff, (h-1)/2)*boolInt(l.off > 0))
	}))
	bind(scList, "M", false, "middle of screen", motion(func(l *listView, n, h, c int) { l.cur = l.off + (min(h, n-l.off)-1)/2 }))
	bind(scList, "L", false, "bottom of screen", motion(func(l *listView, n, h, c int) {
		bottom := min(l.off+h, n) - 1
		l.cur = bottom - max(or1(c)-1, min(scrolloff, (h-1)/2)*boolInt(bottom < n-1))
	}))
	bind(scList, "ctrl+e", false, "scroll down a line", motion(func(l *listView, n, h, c int) {
		l.off = min(l.off+or1(c), max(n-h, 0))
		l.cur = max(l.cur, l.off+min(scrolloff, (h-1)/2)*boolInt(l.off > 0))
	}))
	bind(scList, "ctrl+y", false, "scroll up a line", motion(func(l *listView, n, h, c int) {
		l.off = max(l.off-or1(c), 0)
		l.cur = min(l.cur, l.off+h-1-min(scrolloff, (h-1)/2)*boolInt(l.off+h < n))
	}))
	bind(scList, "zz", false, "center cursor line", motion(func(l *listView, n, h, c int) { l.off = l.cur - h/2 }))
	bind(scList, "zt", false, "cursor line to top", motion(func(l *listView, n, h, c int) { l.off = l.cur - min(scrolloff, (h-1)/2) }))
	bind(scList, "zb", false, "cursor line to bottom", motion(func(l *listView, n, h, c int) { l.off = l.cur - h + 1 + min(scrolloff, (h-1)/2) }))
	bind(scList, "n", false, "next match", func(m *Model, c int) tea.Cmd { return m.searchNext(1, c) })
	bind(scList, "N", false, "previous match", func(m *Model, c int) tea.Cmd { return m.searchNext(-1, c) })
	bind(scList, "/", false, "search in list", func(m *Model, c int) tea.Cmd { return m.openFind() })
	bind(scList, "V", false, "visual line mode", func(m *Model, c int) tea.Cmd {
		if m.tab == tabSearch {
			return nil
		}
		l, _, _ := m.cur()
		m.visual, m.vanchor = !m.visual, l.cur
		return nil
	})
	bind(scList, "esc", false, "leave visual / clear marks", func(m *Model, c int) tea.Cmd {
		m.visual = false
		m.marked = map[string]bool{}
		return nil
	})

	bindCandidates()
	bindTabs()
	bindDialog()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
