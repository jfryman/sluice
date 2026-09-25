package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
)

func bindCandidates() {
	bind(scCandidates, "space", false, "toggle mark", func(m *Model, c int) tea.Cmd {
		for i := 0; i < or1(c) && m.lc.cur < len(m.view); i++ {
			v := m.view[m.lc.cur].Value
			if m.marked[v] {
				delete(m.marked, v)
			} else {
				m.marked[v] = true
			}
			if m.lc.cur == len(m.view)-1 {
				break
			}
			m.lc.cur++
		}
		m.lc.fix(len(m.view), m.listHeight())
		return nil
	})
	for keys, kind := range map[string]index.Kind{"gl": index.KindList, "gd": index.KindDomain, "gs": index.KindSender} {
		kind := kind
		bind(scCandidates, keys, false, "group by "+string(kind), func(m *Model, c int) tea.Cmd { return m.setKind(kind) })
	}
	bind(scCandidates, "s", false, "next sort", func(m *Model, c int) tea.Cmd { m.setSort(m.sortKey.Next()); return nil })
	bind(scCandidates, "S", false, "previous sort", func(m *Model, c int) tea.Cmd { m.setSort(m.sortKey.Prev()); return nil })
	bind(scCandidates, "zh", false, "toggle hiding groups that have rules", func(m *Model, c int) tea.Cmd {
		m.hideRule = !m.hideRule
		m.applyFilter()
		return nil
	})
	bind(scCandidates, "enter", false, "group detail", func(m *Model, c int) tea.Cmd {
		if m.lc.cur < len(m.view) {
			return m.loadDetail(m.view[m.lc.cur])
		}
		return nil
	})
	bind(scCandidates, "t", true, "plan trash rule (+ trash of existing mail)", func(m *Model, c int) tea.Cmd { return m.planRule(sieve.ActionTrash, c) })
	bind(scCandidates, "r", true, "plan mark-read rule", func(m *Model, c int) tea.Cmd { return m.planRule(sieve.ActionRead, c) })
	bind(scCandidates, "f", true, "plan file-into rule (folder picker)", func(m *Model, c int) tea.Cmd {
		if ts := m.targets(c); len(ts) > 0 {
			m.exitSelection()
			if err := m.openPicker(ts); err != nil {
				m.setErr(err)
			}
		}
		return nil
	})
	bind(scCandidates, "x", true, "plan trash of existing mail", func(m *Model, c int) tea.Cmd {
		ts := m.targets(c)
		if len(ts) == 0 {
			return nil
		}
		m.exitSelection()
		return tea.Batch(m.startBusy("finding existing mail"), m.retro(ts))
	})
}

func (m *Model) planRule(act sieve.Action, count int) tea.Cmd {
	ts := m.targets(count)
	if len(ts) == 0 {
		return nil
	}
	m.exitSelection()
	if err := m.stageRules(ts, act, ""); err != nil {
		m.setErr(err)
		return nil
	}
	m.setStatus("planned %d %s rule(s) — A to apply", len(ts), act)
	if act == sieve.ActionTrash {
		return tea.Batch(m.startBusy("finding existing mail"), m.retro(ts))
	}
	return nil
}

func bindTabs() {
	// ---- rules ----
	bind(scRules, "dd", true, "plan removal of rule(s)", func(m *Model, c int) tea.Cmd {
		rules := m.desired()
		if len(rules) == 0 {
			return nil
		}
		lo, hi := m.selection(c)
		sel := append([]sieve.Rule(nil), rules[lo:hi+1]...)
		m.exitSelection()
		err := m.mutate(func(p *plan.Plan) error {
			for _, r := range sel {
				if o, ok := p.PendingRule(r.Kind, r.Value); ok && o.Op == "add" && !m.published(r) {
					p.DropRule(r)
				} else {
					p.SetRule("remove", r)
				}
			}
			return nil
		})
		if err != nil {
			m.setErr(err)
		} else {
			m.setStatus("planned removal of %d rule(s) — A to apply, u to undo", len(sel))
		}
		return nil
	})
	bind(scRules, "x", true, "plan trash of existing mail matching rule(s)", func(m *Model, c int) tea.Cmd {
		rules := m.desired()
		if len(rules) == 0 {
			return nil
		}
		lo, hi := m.selection(c)
		var gs []index.Group
		for _, r := range rules[lo : hi+1] {
			gs = append(gs, index.Group{Kind: index.Kind(r.Kind), Value: r.Value})
		}
		m.exitSelection()
		return tea.Batch(m.startBusy("finding existing mail"), m.retro(gs))
	})

	// ---- search ----
	bind(scSearch, "/ i e enter", false, "edit query", func(m *Model, c int) tea.Cmd {
		m.qOn = true
		return m.qinput.Focus()
	})
	bind(scSearch, "X", true, "plan trash of all matches", func(m *Model, c int) tea.Cmd {
		if m.rtotal == 0 {
			return nil
		}
		return tea.Batch(m.startBusy("collecting matches"), m.searchAll(m.qinput.Value()))
	})

	// ---- plan ----
	bind(scPlan, "dd", true, "drop item(s) from plan", func(m *Model, c int) tea.Cmd {
		if m.planLen() == 0 {
			return nil
		}
		lo, hi := m.selection(c)
		nr := len(m.plan.Rules)
		var rules []sieve.Rule
		ids := map[string]bool{}
		for i := lo; i <= hi; i++ {
			if i < nr {
				rules = append(rules, m.plan.Rules[i].Rule)
			} else {
				ids[m.plan.Trash[i-nr].ID] = true
			}
		}
		m.exitSelection()
		err := m.mutate(func(p *plan.Plan) error {
			for _, r := range rules {
				p.DropRule(r)
			}
			for i := len(p.Trash) - 1; i >= 0; i-- {
				if ids[p.Trash[i].ID] {
					p.DropTrash(i)
				}
			}
			return nil
		})
		if err != nil {
			m.setErr(err)
		} else {
			m.setStatus("dropped %d item(s) — u to undo", hi-lo+1)
		}
		return nil
	})
	bind(scPlan, "enter", false, "show a trash item's messages", func(m *Model, c int) tea.Cmd {
		nr := len(m.plan.Rules)
		if m.lp.cur >= nr && m.lp.cur < m.planLen() {
			return m.showItem(m.plan.Trash[m.lp.cur-nr])
		}
		return nil
	})
	bind(scPlan, "D", true, "discard the whole plan", func(m *Model, c int) tea.Cmd { m.confirmDiscard(); return nil })
}

func bindDialog() {
	scroll := func(f func(d *dialogState, h int)) func(*Model, int) tea.Cmd {
		return func(m *Model, c int) tea.Cmd {
			h := m.dialogHeight()
			f(&m.dialog, h)
			m.dialog.off = max(0, min(m.dialog.off, len(m.dialog.lines)-h))
			return nil
		}
	}
	bind(scDialog, "j down", false, "scroll down", scroll(func(d *dialogState, h int) { d.off++ }))
	bind(scDialog, "k up", false, "scroll up", scroll(func(d *dialogState, h int) { d.off-- }))
	bind(scDialog, "ctrl+d", false, "half page down", scroll(func(d *dialogState, h int) { d.off += h / 2 }))
	bind(scDialog, "ctrl+u", false, "half page up", scroll(func(d *dialogState, h int) { d.off -= h / 2 }))
	bind(scDialog, "ctrl+f pgdown space", false, "page down", scroll(func(d *dialogState, h int) { d.off += h }))
	bind(scDialog, "ctrl+b pgup", false, "page up", scroll(func(d *dialogState, h int) { d.off -= h }))
	bind(scDialog, "gg home", false, "top", scroll(func(d *dialogState, h int) { d.off = 0 }))
	bind(scDialog, "G end", false, "bottom", scroll(func(d *dialogState, h int) { d.off = len(d.lines) }))
	bind(scDialog, "esc q n", false, "close / cancel", func(m *Model, c int) tea.Cmd {
		m.ov = ovNone
		if len(m.dialog.actions) > 0 {
			m.setStatus("cancelled")
		}
		return nil
	})
	bind(scDialog, "enter", false, "close (info dialogs)", func(m *Model, c int) tea.Cmd {
		if len(m.dialog.actions) == 0 {
			m.ov = ovNone
		}
		return nil
	})
}
