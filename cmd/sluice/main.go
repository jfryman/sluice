// Command sluice is a local TUI for turning mail noise into Sieve rules
// and cleaning up old mail. Everything goes into a plan first; see
// lode/summary.md and lode/apply/plan.md.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/doctor"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sandbox"
	"github.com/jfryman/sluice/internal/sieve"
	"github.com/jfryman/sluice/internal/sweep"
	"github.com/jfryman/sluice/internal/timing"
	"github.com/jfryman/sluice/internal/tui"
	"github.com/jfryman/sluice/internal/version"
)

type opts struct {
	cfgPath, planPath           string
	scan, sandbox, reset, apply bool
	yes                         bool
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "doctor":
			os.Exit(doctorCmd(os.Args[2:]))
		case "sweep":
			os.Exit(sweepCmd(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println(version.Long())
			return
		}
	}
	var o opts
	flag.StringVar(&o.cfgPath, "config", config.DefaultPath(), "config file")
	flag.StringVar(&o.planPath, "plan", "", "plan file (default $XDG_STATE_HOME/sluice/plan.json)")
	flag.BoolVar(&o.scan, "scan", false, "update the index, print top candidates and exit")
	flag.BoolVar(&o.sandbox, "sandbox", false, "operate on the full sandbox clone instead of real mail")
	flag.BoolVar(&o.reset, "reset-sandbox", false, "re-clone the sandbox from real mail first (implies -sandbox)")
	flag.BoolVar(&o.apply, "apply", false, "apply the plan headlessly to the selected environment and exit")
	flag.BoolVar(&o.yes, "yes", false, "with -apply: don't ask for confirmation")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "%s\n\nUsage: sluice [flags]\n       sluice doctor [-online] [-config PATH]\n       sluice sweep [-watch] [-sandbox] [-plan PATH] [-config PATH]\n       sluice version\n\nFlags:\n", version.Short())
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "sluice:", err)
		os.Exit(1)
	}
}

// remoteFor builds the ManageSieve publisher for the real server.
func remoteFor(c config.Config, trace *timing.Recorder) sieve.Remote {
	return sieve.Remote{Server: c.SieveServer, Port: c.SievePort, Script: c.SieveScript,
		User: c.User, UserCmd: c.UserCmd, PassCmd: c.PassCmd, Trace: trace}
}

func openIndex(c config.Config) (*index.Index, error) {
	return index.Open(c.IndexPath, c.MailRoot, index.Folders{
		Trash:     c.TrashFolder,
		BlackHole: c.BlackHoleFolders,
		News:      c.NewsFolders,
		Exclude:   c.ExcludeFolders,
		Sent:      c.SentFolders,
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
	realPub := remoteFor(realCfg, nil)
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

// sweepCmd runs `sluice sweep`: once (exit 0 done, 3 mail moved, 1 error)
// or, with -watch, as a long-running service until SIGINT/SIGTERM.
func sweepCmd(args []string) int {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	watch := fs.Bool("watch", false, "keep running: sweep on new drops and every sweep_interval")
	useSandbox := fs.Bool("sandbox", false, "operate on the sandbox clone")
	planPath := fs.String("plan", "", "plan file for queued cleanup (default $XDG_STATE_HOME/sluice/plan.json)")
	fs.Parse(args)

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05")+" "+format+"\n", a...)
	}
	realCfg, err := config.Load(*cfgPath)
	if err != nil {
		logf("%v", err)
		return 1
	}
	if *planPath != "" {
		realCfg.PlanPath = config.Expand(*planPath)
	}
	cfg := realCfg
	rec := &timing.Recorder{}
	var pub sieve.Publisher = remoteFor(realCfg, rec)
	if *useSandbox {
		sb := sandbox.New(realCfg)
		if !sb.Exists() {
			if err := sb.Refresh(realCfg); err != nil {
				logf("%v", err)
				return 1
			}
		}
		cfg, pub = sb.Config(realCfg), sb.Publisher(realCfg)
		cfg.PostSweepCmd = nil // never kick the real sync from a sandbox sweep
	}
	ix, err := openIndex(cfg)
	if err != nil {
		logf("%v", err)
		return 1
	}
	defer ix.Close()
	d := sweep.Deps{Cfg: cfg, Index: ix, Publisher: pub, Timing: rec}
	logf("%s", version.Short())

	if *watch {
		interval, err := time.ParseDuration(cfg.SweepInterval)
		if err != nil {
			logf("sweep_interval %q: %v", cfg.SweepInterval, err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := sweep.Watch(ctx, d, sweep.WatchOptions{Interval: interval, PostCmd: cfg.PostSweepCmd, Logf: logf}); err != nil {
			logf("%v", err)
			return 1
		}
		return 0
	}
	rep, err := sweep.Run(d)
	for _, l := range rep.Summary() {
		fmt.Println(l)
	}
	if rep.Timing != "" {
		logf("timing: %s", rep.Timing)
	}
	if len(rep.Outcomes) == 0 && err == nil {
		fmt.Printf("nothing in %s\n", cfg.TrainingFolder)
	}
	if err != nil {
		logf("%v", err)
		return 1
	}
	if rep.Changed() {
		return 3
	}
	return 0
}

// doctorCmd runs `sluice doctor`; exit 1 if any check failed.
func doctorCmd(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "config file")
	online := fs.Bool("online", false, "also log in to the Sieve server (runs user_cmd/pass_cmd; may prompt)")
	fs.Parse(args)

	cfg, cfgErr := config.Load(*cfgPath)
	if cfgErr != nil {
		cfg, _ = config.Load(filepath.Join(os.TempDir(), "sluice-no-config.toml")) // defaults
	}
	rep := doctor.Run(cfg, doctor.Options{Online: *online, ConfigPath: *cfgPath, ConfigErr: cfgErr})
	fmt.Println(version.Short())
	for _, l := range rep.Lines() {
		fmt.Println(colorize(l))
	}
	if !*online {
		fmt.Println("\n(server not checked; run `sluice doctor -online` before a first real apply)")
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

var symStyle = map[string]lipgloss.Style{
	"✓": lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
	"!": lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
	"✗": lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
}

// colorize colours status symbols that start a line or a capability.
func colorize(l string) string {
	words := strings.Split(l, " ")
	for i, w := range words {
		st, ok := symStyle[w]
		if ok && (i == 0 || i+1 < len(words) && words[i-1] == "" || strings.HasSuffix(words[max(i-1, 0)], ":")) {
			words[i] = st.Render(w)
		}
	}
	return strings.Join(words, " ")
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
	if t.ChangesSieve(p) {
		st, err := t.Publisher.Status(t.SieveFile)
		if err != nil {
			return fmt.Errorf("sieve server precheck: %w", err)
		}
		ours := realCfg.SieveScript
		other := st.OtherActive(ours)
		switch {
		case !st.Exists && other != "":
			return sieve.ErrOtherActive{Ours: ours, Active: other}
		case st.Drift:
			return sieve.ErrDrift
		case !st.Exists:
			fmt.Printf("  ! no %q on the server: this apply creates and activates it\n", ours)
		case other != "":
			if yes {
				return fmt.Errorf("%q is the active script; switching to %q needs interactive confirmation (drop -yes)", other, ours)
			}
			fmt.Printf("  ! %q is active: applying makes %q active instead\n", other, ours)
			t.AllowSwitchFrom = other
		default:
			fmt.Printf("  ✓ server: %q matches local\n", ours)
		}
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
