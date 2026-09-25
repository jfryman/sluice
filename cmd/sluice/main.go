// Command sluice is a local TUI for turning mail noise into Sieve rules
// and cleaning up old mail. Everything goes into a plan first; see
// lode/summary.md and lode/apply/plan.md.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sandbox"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/tui"
)

type opts struct {
	cfgPath, planPath           string
	scan, sandbox, reset, apply bool
	yes                         bool
}

func main() {
	var o opts
	flag.StringVar(&o.cfgPath, "config", config.DefaultPath(), "config file")
	flag.StringVar(&o.planPath, "plan", "", "plan file (default $XDG_STATE_HOME/sluice/plan.json)")
	flag.BoolVar(&o.scan, "scan", false, "update the index, print top candidates and exit")
	flag.BoolVar(&o.sandbox, "sandbox", false, "operate on the full sandbox clone instead of real mail")
	flag.BoolVar(&o.reset, "reset-sandbox", false, "re-clone the sandbox from real mail first (implies -sandbox)")
	flag.BoolVar(&o.apply, "apply", false, "apply the plan headlessly to the selected environment and exit")
	flag.BoolVar(&o.yes, "yes", false, "with -apply: don't ask for confirmation")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "sluice:", err)
		os.Exit(1)
	}
}

func openIndex(c config.Config) (*index.Index, error) {
	return index.Open(c.IndexPath, c.MailRoot, index.Folders{
		Trash:     c.TrashFolder,
		BlackHole: c.BlackHoleFolders,
		News:      c.NewsFolders,
		Exclude:   c.ExcludeFolders,
	})
}

func target(name string, c config.Config, pub sieve.Publisher, ix *index.Index) plan.Target {
	return plan.Target{Name: name, Root: c.MailRoot, TrashFolder: c.TrashFolder, LockFile: c.LockFile,
		JournalPath: c.JournalPath, SieveFile: c.SieveFile, Publisher: pub, Index: ix}
}

func run(o opts) error {
	realCfg, err := config.Load(o.cfgPath)
	if err != nil {
		return err
	}
	if o.planPath != "" {
		realCfg.PlanPath = config.Expand(o.planPath)
	}
	sb := sandbox.New(realCfg)
	sbCfg := sb.Config(realCfg)
	realPub := sieve.Remote{Bin: realCfg.SieveConnect, Server: realCfg.SieveServer, Script: realCfg.SieveScript,
		UserCmd: realCfg.UserCmd, PassCmd: realCfg.PassCmd}
	sbPub := sb.Publisher(realCfg)

	env, cfg := "real", realCfg
	var pub sieve.Publisher = realPub
	if o.sandbox || o.reset {
		env, cfg, pub = "sandbox", sbCfg, sbPub
		if o.reset || !sb.Exists() {
			fmt.Fprintf(os.Stderr, "cloning %s → %s …\n", realCfg.MailRoot, sb.MailDir())
			start := time.Now()
			if err := sb.Refresh(realCfg); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "sandbox ready in %s\n", time.Since(start).Round(time.Millisecond))
		}
	}

	ix, err := openIndex(cfg)
	if err != nil {
		return err
	}
	defer ix.Close()
	self := target(env, cfg, pub, ix)

	switch {
	case o.scan:
		return scan(ix)
	case o.apply:
		return applyHeadless(realCfg, self, o.yes)
	}

	deps := tui.Deps{Env: env, Config: cfg, Index: ix, Target: self}
	if env == "sandbox" {
		deps.Created = sb.Created()
	} else {
		deps.Sandbox = &tui.SandboxTarget{
			Target:  target("sandbox", sbCfg, sbPub, nil),
			Refresh: func() error { return sb.Refresh(realCfg) },
		}
	}
	_, err = tea.NewProgram(tui.New(deps), tea.WithAltScreen()).Run()
	return err
}

func applyHeadless(realCfg config.Config, t plan.Target, yes bool) error {
	p, err := plan.Load(realCfg.PlanPath)
	if err != nil {
		return err
	}
	if p.Empty() {
		fmt.Println("plan is empty; nothing to apply")
		return nil
	}
	fmt.Printf("Plan %s → %s\n", realCfg.PlanPath, strings.ToUpper(t.Name))
	for _, o := range p.Rules {
		fmt.Printf("  rule  %-6s %s\n", o.Op, o.Rule.Describe())
	}
	for _, it := range p.Trash {
		fmt.Printf("  trash %6d  %s (%s)\n", len(it.Refs), it.Label, it.FolderCounts())
	}
	if p.Validated != nil {
		fmt.Printf("  ✓ validated in sandbox %s\n", p.Validated.Format("2006-01-02 15:04"))
	} else {
		fmt.Println("  ⚠ not validated in sandbox since last change")
	}
	if !yes {
		fmt.Printf("Apply to %s? [y/N] ", t.Name)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			fmt.Println("aborted")
			return nil
		}
	}
	rep, err := plan.Apply(p, t)
	for _, l := range rep.Lines() {
		fmt.Println(l)
	}
	if err != nil {
		return err
	}
	if t.Name == "real" {
		dst, err := plan.Archive(realCfg.PlanPath, realCfg.AppliedDir)
		if err != nil {
			return err
		}
		fmt.Println("plan archived to", dst)
	}
	return nil
}

func scan(ix *index.Index) error {
	st, err := ix.Scan(func(done, total int) {
		fmt.Fprintf(os.Stderr, "\rparsing headers %d/%d", done, total)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("%d messages in %d folders: +%d ~%d -%d (%s)\n",
		st.Total, st.Folders, st.Added, st.Updated, st.Removed, st.Took.Round(time.Millisecond))
	for _, k := range []index.Kind{index.KindList, index.KindDomain} {
		gs, err := ix.Groups(k, time.Now())
		if err != nil {
			return err
		}
		index.SortGroups(gs, index.SortScore)
		fmt.Printf("\ntop %s candidates:\n", k)
		for i, g := range gs {
			if i == 15 {
				break
			}
			fmt.Printf("  %7.1f  %5d total %4d/90d %3.0f%% unread %4d blackhole  %s\n",
				g.Score, g.Total, g.Recent90, 100*float64(g.Unread)/float64(g.Total), g.BlackHole, g.Value)
		}
	}
	return nil
}
