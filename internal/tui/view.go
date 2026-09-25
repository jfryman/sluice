package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/jfryman/sluice/internal/sieve"
)

var (
	accent   = lipgloss.AdaptiveColor{Light: "#5A3FC0", Dark: "#A48CFF"}
	muted    = lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#8A8A8A"}
	danger   = lipgloss.AdaptiveColor{Light: "#B3261E", Dark: "#FF8A80"}
	ok       = lipgloss.AdaptiveColor{Light: "#1E7B34", Dark: "#7FD99A"}
	warn     = lipgloss.AdaptiveColor{Light: "#8A5A00", Dark: "#FFCC66"}
	cursorBg = lipgloss.AdaptiveColor{Light: "#E6E0FA", Dark: "#2E2750"}
	visualBg = lipgloss.AdaptiveColor{Light: "#D8E8FF", Dark: "#23344F"}

	sTabOn   = lipgloss.NewStyle().Bold(true).Foreground(accent).Underline(true)
	sTabOff  = lipgloss.NewStyle().Foreground(muted)
	sMuted   = lipgloss.NewStyle().Foreground(muted)
	sHead    = lipgloss.NewStyle().Bold(true)
	sCursor  = lipgloss.NewStyle().Background(cursorBg)
	sVisual  = lipgloss.NewStyle().Background(visualBg)
	sErr     = lipgloss.NewStyle().Foreground(danger)
	sOK      = lipgloss.NewStyle().Foreground(ok)
	sWarn    = lipgloss.NewStyle().Foreground(warn)
	sAccent  = lipgloss.NewStyle().Foreground(accent)
	sOverlay = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1)
	sReal    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#B3261E")).Padding(0, 1)
	sSandbox = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#7FD99A")).Padding(0, 1)
)

func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.FillRight(runewidth.Truncate(s, w, "…"), w)
}

// Layout: tab bar · body · status line · 1-line help.
func (m *Model) bodyHeight() int { return max(m.h-4, 3) }

// dialogHeight is the number of content lines a dialog shows.
func (m *Model) dialogHeight() int { return max(m.bodyHeight()-5, 1) }

func (m *Model) View() string {
	if m.w == 0 {
		return "starting…"
	}
	var b strings.Builder
	b.WriteString(m.viewTabs() + "\n")

	var body string
	switch m.ov {
	case ovDialog:
		body = m.viewDialog()
	case ovPicker:
		body = m.viewPicker()
	default:
		switch m.tab {
		case tabCandidates:
			body = m.viewCandidates()
		case tabRules:
			body = m.viewRules()
		case tabSearch:
			body = m.viewSearch()
		default:
			body = m.viewPlan()
		}
	}
	b.WriteString(lipgloss.NewStyle().Height(m.bodyHeight()).MaxHeight(m.bodyHeight()).Render(body))
	b.WriteString("\n" + m.viewStatus() + "\n" + sMuted.Render(trunc(m.help(), m.w)))
	return b.String()
}

func (m *Model) viewTabs() string {
	badge := sReal.Render("REAL")
	if m.d.Env == "sandbox" {
		badge = sSandbox.Render("SANDBOX " + m.d.Created.Format("01-02 15:04"))
	}
	names := []string{"1 Candidates", "2 Rules", "3 Search", "4 Plan"}
	var parts []string
	for i, n := range names {
		if tab(i) == m.tab {
			parts = append(parts, sTabOn.Render(n))
		} else {
			parts = append(parts, sTabOff.Render(n))
		}
	}
	right := sMuted.Render(fmt.Sprintf("%d msgs indexed", m.indexCount))
	if !m.plan.Empty() {
		p := fmt.Sprintf("plan: %d rules · %d msgs", len(m.plan.Rules), m.plan.Messages())
		if m.plan.Validated != nil {
			right = sOK.Render(p+" ✓") + "  " + right
		} else {
			right = sWarn.Render(p) + "  " + right
		}
	}
	left := badge + " " + sAccent.Bold(true).Render("sluice") + "  " + strings.Join(parts, "  ")
	gap := max(m.w-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

// viewStatus is vim's command/status line: prompt, or message on the left,
// mode and pending keys (showcmd) on the right.
func (m *Model) viewStatus() string {
	switch {
	case m.cmdOn:
		return m.cmdInput.View()
	case m.findOn:
		return m.findIn.View()
	}
	var left string
	switch {
	case m.busy != "":
		left = sAccent.Render(m.spin.View() + " " + m.busy)
	case m.statusErr:
		left = sErr.Render(m.status)
	default:
		left = m.status
	}
	right := m.keys.show()
	if m.visual {
		right = sHead.Render("-- VISUAL LINE --") + "  " + right
	}
	if right == "" {
		return trunc(left, m.w)
	}
	lw := max(m.w-lipgloss.Width(right)-2, 0)
	return lipgloss.NewStyle().MaxWidth(lw).Render(left) +
		strings.Repeat(" ", max(lw-lipgloss.Width(left), 0)+2) + right
}

func (m *Model) help() string {
	switch m.ov {
	case ovDialog:
		var hints []string
		for _, a := range m.dialog.actions {
			hints = append(hints, a.key+" "+a.label)
		}
		hints = append(hints, "esc close", "j/k ctrl+d/u gg/G scroll")
		return strings.Join(hints, " · ")
	case ovPicker:
		return "j/k move · enter choose folder · esc cancel"
	}
	tail := " · A apply · u undo · : cmd · ? help"
	switch m.tab {
	case tabCandidates:
		return "t/f/r plan rule · x plan trash · space mark · V visual · gl/gd/gs group · s sort · / find" + tail
	case tabRules:
		return "dd remove · x plan trash · V visual · / find" + tail
	case tabSearch:
		return "/ edit query · X plan trash of all matches" + tail
	}
	return "dd drop · enter messages · D discard · V visual" + tail
}

func pct(a, b int) string {
	if b == 0 {
		return "  -"
	}
	return fmt.Sprintf("%3.0f%%", 100*float64(a)/float64(b))
}

// rowStyle picks cursor / visual / plain rendering for row i.
func (m *Model) rowStyle(i, cur int, line string) string {
	if m.visual && i >= min(m.vanchor, cur) && i <= max(m.vanchor, cur) {
		if i == cur {
			return sCursor.Render(line)
		}
		return sVisual.Render(line)
	}
	if i == cur {
		return sCursor.Render(line)
	}
	return line
}

func (m *Model) viewCandidates() string {
	var b strings.Builder
	info := fmt.Sprintf("group by %s · sort %s · %d groups", sHead.Render(string(m.kind)), sHead.Render(m.sortKey.String()), len(m.view))
	if n := len(m.marked); n > 0 {
		info += sWarn.Render(fmt.Sprintf(" · %d marked", n))
	}
	if m.hideRule {
		info += " · hiding ruled"
	}
	if m.filterText != "" {
		info += sAccent.Render(" · filter: " + m.filterText)
	}
	b.WriteString(info + "\n")

	fixed := 2 + 7 + 6 + 7 + 6 + 6 + 6 + 6 + 10 + 2
	vw := max((m.w-fixed)*3/5, 16)
	sw := max(m.w-fixed-vw, 0)
	b.WriteString(sHead.Render(fmt.Sprintf("  %6s %5s %6s %5s %5s %5s %5s %-10s %s %s",
		"score", "90d", "total", "unrd", "bhole", "news", "trash", "rule", trunc(m.kind.Column(), vw), trunc("latest subject", sw))) + "\n")

	start, end := m.lc.window(len(m.view), m.listHeight())
	for i := start; i < end; i++ {
		g := m.view[i]
		mark := "  "
		if m.marked[g.Value] {
			mark = sWarn.Render("▸ ")
		}
		rule := ""
		if r, ok := m.ruleFor(g); ok {
			rule = string(r.Action)
			if m.pending(r) {
				rule += "*"
			}
		}
		subj := trunc(g.Subject, sw)
		if i != m.lc.cur {
			subj = sMuted.Render(subj)
		}
		line := fmt.Sprintf("%6.0f %5d %6d %5s %5d %5d %5d %-10s %s %s",
			g.Score, g.Recent90, g.Total, pct(g.Unread, g.Total), g.BlackHole, g.News, g.Trashed,
			rule, trunc(g.Value, vw), subj)
		b.WriteString(mark + m.rowStyle(i, m.lc.cur, line) + "\n")
	}
	if len(m.view) == 0 && m.busy == "" {
		b.WriteString(sMuted.Render("  no groups"))
	}
	return b.String()
}

func actionStyled(r sieve.Rule) string {
	act := fmt.Sprintf("%-6s", r.Action)
	switch r.Action {
	case sieve.ActionTrash:
		return sErr.Render(act)
	case sieve.ActionFile:
		return sAccent.Render(act) + " → " + r.Folder
	}
	return sOK.Render(act)
}

func (m *Model) viewRules() string {
	var b strings.Builder
	if m.base == nil {
		return sErr.Render(fmt.Sprintf("cannot use %s: %v", m.d.Config.SieveFile, m.baseErr))
	}
	b.WriteString(fmt.Sprintf("managed rules in %s as they will be after the plan  (* = planned change)\n", m.d.Config.SieveFile))
	rules := m.desired()
	if len(rules) == 0 {
		b.WriteString(sMuted.Render("\n  no managed rules yet — plan some from the Candidates tab (t / f / r)"))
	}
	start, end := m.lr.window(len(rules), m.listHeight())
	for i := start; i < end; i++ {
		r := rules[i]
		flag := " "
		if m.pending(r) {
			flag = sWarn.Render("*")
		}
		line := fmt.Sprintf("%s %-7s %-40s %s  %s", flag, r.Kind, trunc(r.Value, 40), actionStyled(r), sMuted.Render(r.Added))
		b.WriteString(m.rowStyle(i, m.lr.cur, line) + "\n")
	}
	var removed []string
	for _, o := range m.plan.Rules {
		if o.Op == "remove" {
			removed = append(removed, o.Rule.Describe())
		}
	}
	if len(removed) > 0 {
		b.WriteString("\n" + sWarn.Render("planned removal: "+strings.Join(removed, "; ")) + "\n")
	}
	return b.String()
}

func (m *Model) viewSearch() string {
	var b strings.Builder
	b.WriteString(m.qinput.View() + "\n")
	if m.rtotal > len(m.results) {
		b.WriteString(sMuted.Render(fmt.Sprintf("%d matches (showing newest %d)", m.rtotal, len(m.results))) + "\n")
	} else {
		b.WriteString(sMuted.Render(fmt.Sprintf("%d matches", m.rtotal)) + "\n")
	}
	planned := m.plannedKeys()
	fw := max(m.w/4, 16)
	folderW := 16
	sw := max(m.w-10-2-folderW-fw-4, 10)
	start, end := m.ls.window(len(m.results), m.listHeight())
	for i := start; i < end; i++ {
		r := m.results[i]
		dot := " "
		switch {
		case planned[r.Key]:
			dot = sErr.Render("✗")
		case !r.Seen:
			dot = sAccent.Render("•")
		}
		line := fmt.Sprintf("%s %s %s %s %s", dot, r.Date.Format("2006-01-02"), trunc(r.Folder, folderW), trunc(r.FromAddr, fw), trunc(r.Subject, sw))
		b.WriteString(m.rowStyle(i, m.ls.cur, line) + "\n")
	}
	return b.String()
}

func (m *Model) viewPlan() string {
	var b strings.Builder
	p := m.plan
	if p.Empty() {
		b.WriteString(sMuted.Render("The plan is empty.\n\nEverything you do in Candidates / Rules / Search is added here first.\n" +
			"Nothing touches mail or the server until you press A and apply it — to a fresh sandbox to validate, then to real.\n\n" +
			"Plan file: " + m.d.Config.PlanPath))
		return b.String()
	}
	v := sWarn.Render(m.validatedLine())
	if p.Validated != nil {
		v = sOK.Render(m.validatedLine())
	}
	b.WriteString(fmt.Sprintf("%d rule changes · %d messages to trash   %s\n", len(p.Rules), p.Messages(), v))
	b.WriteString(sMuted.Render(m.d.Config.PlanPath) + "\n")

	var rows []string
	for _, o := range p.Rules {
		op := sOK.Render("+ add   ")
		if o.Op == "remove" {
			op = sErr.Render("- remove")
		}
		rows = append(rows, fmt.Sprintf("rule   %s %-7s %-40s %s", op, o.Rule.Kind, trunc(o.Rule.Value, 40), actionStyled(o.Rule)))
	}
	for _, t := range p.Trash {
		rows = append(rows, fmt.Sprintf("trash  %6d msgs  %s  %s", len(t.Refs), trunc(t.Label, 50), sMuted.Render(t.FolderCounts())))
	}
	start, end := m.lp.window(len(rows), m.listHeight())
	for i := start; i < end; i++ {
		b.WriteString(m.rowStyle(i, m.lp.cur, rows[i]) + "\n")
	}
	return b.String()
}

func (m *Model) overlayBox(title string, lines []string, off int) string {
	h := m.dialogHeight()
	w := max(m.w-4, 20)
	off = max(0, min(off, len(lines)-h))
	end := min(off+h, len(lines))
	var body []string
	for _, l := range lines[off:end] {
		switch {
		case strings.HasPrefix(l, "+ "):
			l = sOK.Render(trunc(l, w-2))
		case strings.HasPrefix(l, "- "):
			l = sErr.Render(trunc(l, w-2))
		case strings.HasPrefix(l, "⚠"):
			l = sWarn.Render(trunc(l, w-2))
		case strings.HasPrefix(l, "✓"):
			l = sOK.Render(trunc(l, w-2))
		case strings.HasPrefix(l, "## "):
			l = sHead.Render(trunc(strings.TrimPrefix(l, "## "), w-2))
		default:
			l = trunc(l, w-2)
		}
		body = append(body, l)
	}
	pos := ""
	if len(lines) > h {
		pos = sMuted.Render(fmt.Sprintf("  %d-%d/%d", off+1, end, len(lines)))
	}
	return sOverlay.Width(w).Render(sHead.Render(title) + pos + "\n\n" + strings.Join(body, "\n"))
}

func (m *Model) viewDialog() string {
	d := m.dialog
	var keys []string
	for _, a := range d.actions {
		keys = append(keys, a.key)
	}
	title := d.title
	if len(keys) > 0 {
		title += "  [" + strings.Join(keys, "/") + "/esc]"
	}
	return m.overlayBox(title, d.lines, d.off)
}

func (m *Model) viewPicker() string {
	p := m.picker
	names := make([]string, len(p.targets))
	for i, g := range p.targets {
		names[i] = g.Value
	}
	var lines []string
	for i, f := range p.folders {
		l := "  " + f
		if i == p.cur {
			l = "▸ " + f
		}
		lines = append(lines, l)
	}
	h := m.dialogHeight()
	return m.overlayBox("File "+strings.Join(names, ", ")+" into…", lines, max(0, p.cur-h/2))
}

// openHelp lists every binding reachable from the current screen, plus
// the command line, generated from the binding table.
func (m *Model) openHelp() {
	var lines []string
	for _, sc := range m.scopes() {
		lines = append(lines, "## "+sc.String())
		seen := map[string]int{}
		var order []string
		keysFor := map[string][]string{}
		for _, b := range bindings {
			if b.scope != sc {
				continue
			}
			if _, ok := seen[b.help]; !ok {
				seen[b.help] = len(order)
				order = append(order, b.help)
			}
			keysFor[b.help] = append(keysFor[b.help], b.keys)
		}
		for _, h := range order {
			lines = append(lines, fmt.Sprintf("  %-22s %s", strings.Join(keysFor[h], " "), h))
		}
		lines = append(lines, "")
	}
	lines = append(lines, "## counts", "  {n} before a motion or dd repeats it: 5j, 3dd, 10G, 2gt", "")
	lines = append(lines, "## commands (: then tab to complete, up/down for history)")
	for _, c := range commands {
		if c.name == "q" {
			continue
		}
		arg := ""
		if len(c.args) > 0 {
			arg = " " + strings.Join(c.args, "|")
		}
		lines = append(lines, fmt.Sprintf("  :%-22s %s", c.name+arg, c.help))
	}
	lines = append(lines, "  :{n}                    go to line n")
	m.openDialog("Help — "+m.tabName(), lines)
}

func (m *Model) tabName() string {
	return [...]string{"Candidates", "Rules", "Search", "Plan"}[m.tab]
}
