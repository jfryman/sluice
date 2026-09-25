package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
)

// command is one ex-style command. args lists completions for the first
// argument (nil = free text or none).
type command struct {
	name string
	args []string
	help string
	run  func(m *Model, arg string) tea.Cmd
}

var commands []command

func init() {
	commands = []command{
		{"quit", nil, "quit", func(m *Model, a string) tea.Cmd { return tea.Quit }},
		{"q", nil, "quit", func(m *Model, a string) tea.Cmd { return tea.Quit }},
		{"apply", []string{"sandbox", "real"}, "apply plan (dialog, or straight to sandbox/real)", func(m *Model, a string) tea.Cmd {
			return m.applyCommand(a)
		}},
		{"group", []string{"list", "domain", "sender"}, "group candidates by", func(m *Model, a string) tea.Cmd {
			switch a {
			case "list", "domain", "sender":
				m.switchTab(tabCandidates)
				return m.setKind(index.Kind(a))
			}
			m.setErr(fmt.Errorf("usage: :group list|domain|sender"))
			return nil
		}},
		{"sort", []string{"score", "90d", "total", "unread"}, "sort candidates by", func(m *Model, a string) tea.Cmd {
			for k := index.SortKey(0); k < index.SortKey(4); k++ {
				if strings.TrimSuffix(k.String(), "%") == a {
					m.switchTab(tabCandidates)
					m.setSort(k)
					return nil
				}
			}
			m.setErr(fmt.Errorf("usage: :sort score|90d|total|unread"))
			return nil
		}},
		{"filter", nil, "narrow candidates to TEXT (empty clears)", func(m *Model, a string) tea.Cmd {
			m.filterText = a
			m.switchTab(tabCandidates)
			m.applyFilter()
			if a == "" {
				m.setStatus("filter cleared")
			} else {
				m.setStatus("filter: %s (%d groups)", a, len(m.view))
			}
			return nil
		}},
		{"hide", nil, "hide groups that have rules", func(m *Model, a string) tea.Cmd { m.hideRule = true; m.applyFilter(); return nil }},
		{"nohide", nil, "show groups that have rules", func(m *Model, a string) tea.Cmd { m.hideRule = false; m.applyFilter(); return nil }},
		{"search", nil, "run QUERY in the Search tab", func(m *Model, a string) tea.Cmd {
			m.switchTab(tabSearch)
			m.qinput.SetValue(a)
			return tea.Batch(m.startBusy("searching"), m.runSearch(a))
		}},
		{"w", nil, "write a copy of the plan to PATH", func(m *Model, a string) tea.Cmd {
			if a == "" {
				m.setStatus("the plan autosaves to %s; :w PATH writes a copy", m.d.Config.PlanPath)
				return nil
			}
			path := config.Expand(a)
			if _, err := os.Stat(path); err == nil {
				m.setErr(fmt.Errorf("%s exists (not overwriting)", path))
				return nil
			}
			if err := plan.Save(path, m.plan); err != nil {
				m.setErr(err)
			} else {
				m.setStatus("plan written to %s", path)
			}
			return nil
		}},
		{"discard", nil, "discard the whole plan", func(m *Model, a string) tea.Cmd { m.confirmDiscard(); return nil }},
		{"undo", nil, "undo plan edit", func(m *Model, a string) tea.Cmd { m.planUndo(1); return nil }},
		{"redo", nil, "redo plan edit", func(m *Model, a string) tea.Cmd { m.planRedo(1); return nil }},
		{"rescan", nil, "rescan index, reload sieve + plan", func(m *Model, a string) tea.Cmd { return m.rescan() }},
		{"tab", []string{"1", "2", "3", "4"}, "go to tab N", func(m *Model, a string) tea.Cmd {
			n, err := strconv.Atoi(a)
			if err != nil || n < 1 || n > int(numTabs) {
				m.setErr(fmt.Errorf("usage: :tab 1-%d", numTabs))
				return nil
			}
			m.switchTab(tab(n - 1))
			return nil
		}},
		{"help", nil, "key + command help", func(m *Model, a string) tea.Cmd { m.openHelp(); return nil }},
	}
}

func (m *Model) openCmdline() tea.Cmd {
	m.cmdOn = true
	m.cmdHistIdx = len(m.cmdHist)
	m.cmdInput.SetValue("")
	return m.cmdInput.Focus()
}

// execCommand runs one command line (without the leading ':').
func (m *Model) execCommand(line string) tea.Cmd {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	if len(m.cmdHist) == 0 || m.cmdHist[len(m.cmdHist)-1] != line {
		m.cmdHist = append(m.cmdHist, line)
	}
	if n, err := strconv.Atoi(line); err == nil { // :{n}
		l, cnt, h := m.cur()
		l.cur = n - 1
		l.fix(cnt, h)
		return nil
	}
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	var match []command
	for _, c := range commands {
		if c.name == name {
			match = []command{c}
			break
		}
		if strings.HasPrefix(c.name, name) {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		m.setErr(fmt.Errorf("not a command: %s", name))
		return nil
	case 1:
		return match[0].run(m, arg)
	}
	m.setErr(fmt.Errorf("ambiguous command %q: %s", name, strings.Join(names(match), ", ")))
	return nil
}

func names(cs []command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

// complete extends line to the longest common completion.
func complete(line string) (string, []string) {
	name, arg, hasArg := strings.Cut(line, " ")
	var cands []string
	if !hasArg {
		for _, c := range commands {
			if strings.HasPrefix(c.name, name) && c.name != "q" {
				cands = append(cands, c.name)
			}
		}
	} else {
		for _, c := range commands {
			if c.name != name {
				continue
			}
			for _, a := range c.args {
				if strings.HasPrefix(a, arg) {
					cands = append(cands, a)
				}
			}
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return line, nil
	}
	common := cands[0]
	for _, c := range cands[1:] {
		for !strings.HasPrefix(c, common) {
			common = common[:len(common)-1]
		}
	}
	if len(cands) == 1 && !hasArg {
		common += " "
	}
	if hasArg {
		return name + " " + common, cands
	}
	return common, cands
}

// keyCmdline handles keys while the command line is open.
func (m *Model) keyCmdline(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "ctrl+c":
		m.cmdOn = false
		m.cmdInput.Blur()
		return nil
	case "enter":
		m.cmdOn = false
		m.cmdInput.Blur()
		return m.execCommand(m.cmdInput.Value())
	case "backspace":
		if m.cmdInput.Value() == "" {
			m.cmdOn = false
			m.cmdInput.Blur()
			return nil
		}
	case "tab":
		line, cands := complete(m.cmdInput.Value())
		m.cmdInput.SetValue(line)
		m.cmdInput.CursorEnd()
		if len(cands) > 1 {
			m.setStatus("%s", strings.Join(cands, "  "))
		}
		return nil
	case "up", "ctrl+p":
		if m.cmdHistIdx > 0 {
			m.cmdHistIdx--
			m.cmdInput.SetValue(m.cmdHist[m.cmdHistIdx])
			m.cmdInput.CursorEnd()
		}
		return nil
	case "down", "ctrl+n":
		if m.cmdHistIdx < len(m.cmdHist) {
			m.cmdHistIdx++
			v := ""
			if m.cmdHistIdx < len(m.cmdHist) {
				v = m.cmdHist[m.cmdHistIdx]
			}
			m.cmdInput.SetValue(v)
			m.cmdInput.CursorEnd()
		}
		return nil
	}
	var cmd tea.Cmd
	m.cmdInput, cmd = m.cmdInput.Update(k)
	return cmd
}
