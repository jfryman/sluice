package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/sweep"
	"github.com/jfryman/sluice/internal/version"
)

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case spinner.TickMsg:
		if m.busy == "" {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case progressMsg:
		if msg.ch == m.progCh && m.busy != "" {
			m.busyStage = msg.stage
		}
		return m, waitChan(msg.ch) // keep reading: the result comes on the same channel

	case scanProgressMsg:
		m.busyStage = fmt.Sprintf("parsing headers %d/%d", msg.done, msg.total)
		return m, waitChan(m.scanCh)

	case scanDoneMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.indexCount = msg.st.Total
		m.setStatus("indexed %d messages in %d folders (+%d ~%d -%d) in %s",
			msg.st.Total, msg.st.Folders, msg.st.Added, msg.st.Updated, msg.st.Removed, msg.st.Took.Round(1e6))
		if !m.rulesSynced {
			m.rulesSynced = true // once per session, after the startup scan
			return m, tea.Batch(m.loadGroups(), m.syncRules(false, false))
		}
		return m, m.loadGroups()

	case rulesSyncMsg:
		m.busy = ""
		switch {
		case msg.err != nil:
			m.setErr(fmt.Errorf("rules: %w", msg.err))
			return m, nil
		case msg.res.Updated:
			m.loadScript()
			m.applyFilter()
			m.setStatus("%s", msg.res.Summary())
			if msg.res.Backup != "" {
				m.status += " · previous local file: " + msg.res.Backup
			}
		case msg.manual:
			m.setStatus("%s", msg.res.Summary())
		}
		return m, nil

	case groupsMsg:
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		if msg.kind != m.kind {
			return m, nil // stale
		}
		index.SortGroups(msg.groups, m.sortKey)
		m.groups = msg.groups
		m.applyFilter()
		return m, nil

	case foldersMsg:
		if msg.req != m.flFolder {
			return m, nil // stale: the folder was switched meanwhile
		}
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.flFolders, m.flFolder, m.flGroups = msg.folders, msg.folder, msg.groups
		m.applyFilter()
		return m, nil

	case detailMsg:
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		g := msg.group
		m.openDialog(msg.title, msg.lines, action{"s", "search this group", func() tea.Cmd {
			m.switchTab(tabSearch)
			m.qinput.SetValue(g.Kind.QueryTerm(g.Value))
			return tea.Batch(m.startBusy("searching"), m.runSearch(m.qinput.Value()))
		}})
		return m, nil

	case searchMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.results, m.rtotal, m.ls = msg.results, msg.total, listView{}
		m.setStatus("%d matches", msg.total)
		return m, nil

	case searchRulesMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.confirmSearchRules(msg)
		return m, nil

	case retroMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.confirmPlanTrash(msg.label, msg.msgs)
		return m, nil

	case precheckMsg:
		m.busy = ""
		if msg.err != nil {
			m.openDialog("Can't reach the Sieve server", []string{msg.err.Error(), "", "Nothing was changed."})
			return m, nil
		}
		m.confirmReal(&msg.st)
		return m, nil

	case sweepMsg:
		m.busy = ""
		m.loadScript()
		m.reloadPlan()
		m.applyFilter()
		lines := msg.rep.Summary()
		if len(lines) == 0 {
			lines = []string{"nothing in " + m.d.Config.TrainingFolder}
		}
		if msg.err != nil {
			lines = append(lines, "", "error: "+msg.err.Error())
		}
		m.openDialog("Sweep "+m.d.Config.TrainingFolder, lines)
		return m, m.loadGroups()

	case cleanupMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		var lines []string
		total := 0
		if err := m.mutate(func(p *plan.Plan) error {
			for _, it := range msg.items {
				if n := p.AddTrash(it.label, it.refs); n > 0 {
					total += n
					lines = append(lines, fmt.Sprintf("  %6d  %s", n, it.label))
				}
			}
			return nil
		}); err != nil {
			m.setErr(err)
			return m, nil
		}
		if total == 0 {
			lines = []string{"No existing mail matches a trash rule outside " + m.d.Config.TrashFolder + " (or it's already planned)."}
		} else {
			lines = append([]string{fmt.Sprintf("Queued %d messages in %d plan items:", total, len(lines)), ""}, lines...)
			lines = append(lines, "", "Review in the Plan tab, then A s · A r y (u undoes).")
			m.setStatus("queued cleanup of %d messages — A to apply", total)
		}
		if msg.nonTrash > 0 {
			lines = append(lines, "", fmt.Sprintf("%d file/read rule(s) skipped: cleanup only trashes.", msg.nonTrash))
		}
		m.openDialog("Re-run cleanup for all rules", lines)
		return m, nil

	case sweepLogMsg:
		m.sweepSize, m.sweepSeen = msg.size, msg.seen
		b := sweep.Brief(msg.outs)
		if b == "" {
			return m, m.pollSweepLog()
		}
		m.loadScript()
		m.reloadPlan()
		m.applyFilter()
		m.setStatus("swept %s: %s", m.d.Config.TrainingFolder, b)
		return m, tea.Batch(m.loadGroups(), m.pollSweepLog())

	case doctorMsg:
		m.busy = ""
		m.openDialog("Doctor ("+m.d.Env+") — "+version.Short(), msg.lines)
		return m, nil

	case itemMsg:
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.openDialog(msg.title, msg.lines)
		return m, nil

	case undoMsg:
		m.busy = ""
		if msg.err != nil {
			m.setErr(msg.err)
			return m, nil
		}
		m.setStatus("undid batch %s: restored %d messages", msg.res.Batch, msg.res.Moved)
		if msg.res.Server > 0 {
			m.status += fmt.Sprintf(" (%d on the server)", msg.res.Server)
		}
		if len(msg.res.Skipped) > 0 {
			m.status += fmt.Sprintf("; skipped %d (first: %s)", len(msg.res.Skipped), msg.res.Skipped[0])
			m.statusErr = true
		}
		if msg.res.Server > 0 {
			return m, tea.Batch(m.loadGroups(), m.syncAfterServerMoves())
		}
		return m, m.loadGroups()

	case syncMsg:
		m.busy = ""
		switch {
		case msg.err != nil:
			m.setErr(fmt.Errorf("sync after server moves: %w (local copies update on the next sync)", msg.err))
			return m, nil
		case msg.busy:
			m.setStatus("mbsync is already running; local copies update when it picks up the server moves")
			return m, nil
		}
		m.setStatus("synced: local copies match the server")
		return m, m.rescan()

	case applyMsg:
		m.busy = ""
		m.reloadPlan()
		m.loadScript()
		m.applyFilter()
		var lines []string
		if msg.rep.Target != "" { // empty: failed before anything was applied
			lines = append(msg.rep.Lines(), "")
		}
		title := "Applied to " + msg.target
		switch {
		case msg.err != nil:
			title = "Apply to " + msg.target + " FAILED"
			lines = append(lines, "error: "+msg.err.Error())
			if msg.rep.Target == "" {
				lines = append(lines, "", "Nothing was published or moved.")
			}
			m.setErr(msg.err)
		case msg.target == "real":
			// The plan was archived; undoing back into it would re-apply it.
			m.undoStack, m.redoStack = nil, nil
			lines = append(lines, "Plan archived to "+msg.archived+"; the plan is now empty.")
			m.setStatus("applied to real; plan archived")
		default:
			lines = append(lines, m.validatedLine(), "Browse the result: sluice -sandbox")
			m.setStatus("validated in sandbox — A r y applies to real")
		}
		m.openDialog(title, lines)
		var cmds []tea.Cmd
		if msg.target == m.d.Env {
			cmds = append(cmds, m.loadGroups()) // local moves already reflected in this index
		}
		if msg.rep.Server > 0 {
			cmds = append(cmds, m.syncAfterServerMoves())
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" && !m.cmdOn && !m.findOn && !m.qOn {
		return tea.Quit
	}

	// Prompts capture keys first.
	switch {
	case m.cmdOn:
		return m.keyCmdline(k)
	case m.findOn:
		return m.keyFind(k)
	case m.qOn:
		switch k.String() {
		case "enter":
			m.qOn = false
			m.qinput.Blur()
			return tea.Batch(m.startBusy("searching"), m.runSearch(m.qinput.Value()))
		case "esc", "ctrl+c":
			m.qOn = false
			m.qinput.Blur()
			return nil
		}
		var cmd tea.Cmd
		m.qinput, cmd = m.qinput.Update(k)
		return cmd
	}

	switch m.ov {
	case ovPicker:
		return m.keyPicker(k.String())
	case ovDialog:
		// Dialog action keys win when no count/prefix is pending.
		if m.keys == (keyState{}) {
			for _, a := range m.dialog.actions {
				if k.String() == a.key {
					m.ov = ovNone
					return a.run() // may open a follow-up dialog
				}
			}
		}
	}
	return m.dispatch(k)
}

func (m *Model) keyPicker(s string) tea.Cmd {
	p := &m.picker
	switch s {
	case "esc", "q":
		m.ov = ovNone
	case "j", "down":
		p.cur = min(p.cur+1, len(p.folders)-1)
	case "k", "up":
		p.cur = max(p.cur-1, 0)
	case "g", "home":
		p.cur = 0
	case "G", "end":
		p.cur = max(len(p.folders)-1, 0)
	case "enter":
		m.ov = ovNone
		if len(p.folders) == 0 {
			return nil
		}
		folder := p.folders[p.cur]
		if err := m.stageRules(p.targets, sieve.ActionFile, folder); err != nil {
			m.setErr(err)
			return nil
		}
		m.setStatus("planned %d file-into %q rule(s) — A to apply", len(p.targets), folder)
	}
	return nil
}

// published reports whether r's (kind, value) is in this env's sieve file.
func (m *Model) published(r sieve.Rule) bool {
	if m.base == nil {
		return false
	}
	_, ok := m.base.Find(r.Kind, r.Value)
	return ok
}
