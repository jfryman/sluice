package sweep

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchOptions configures the long-running service loop.
type WatchOptions struct {
	Interval time.Duration // periodic rescan fallback
	Debounce time.Duration // wait for mbsync to finish writing a burst
	PostCmd  []string      // run after a sweep that moved local mail
	Logf     func(format string, args ...any)
}

// Watch sweeps once at start, then whenever files appear in the training
// folder (after a quiet Debounce period) and every Interval, until ctx ends.
func Watch(ctx context.Context, d Deps, o WatchOptions) error {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Minute
	}
	if o.Debounce <= 0 {
		o.Debounce = 3 * time.Second
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := EnsureFolder(d.Cfg); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dirs := []string{
		filepath.Join(d.Cfg.MailRoot, d.Cfg.TrainingFolder, "new"),
		filepath.Join(d.Cfg.MailRoot, d.Cfg.TrainingFolder, "cur"),
	}
	watch := func() {
		for _, dir := range dirs {
			if err := w.Add(dir); err != nil {
				logf("watch %s: %v", dir, err)
			}
		}
	}
	watch()
	logf("watching %s (rescan every %s)", filepath.Join(d.Cfg.MailRoot, d.Cfg.TrainingFolder), o.Interval)

	sweepOnce := func(why string) {
		rep, err := Run(d)
		if len(rep.Outcomes) > 0 {
			logf("sweep triggered by %s: %d message(s)", why, len(rep.Outcomes))
		}
		for _, l := range rep.Summary() {
			logf("%s", l)
		}
		if rep.Timing != "" {
			logf("timing: %s", rep.Timing)
		}
		if err != nil {
			logf("sweep (%s): %v", why, err)
		}
		if rep.Changed() && len(o.PostCmd) > 0 {
			out, err := exec.CommandContext(ctx, o.PostCmd[0], o.PostCmd[1:]...).CombinedOutput()
			if err != nil {
				logf("post_sweep_cmd %s: %v: %s", strings.Join(o.PostCmd, " "), err, strings.TrimSpace(string(out)))
			}
		}
	}
	sweepOnce("start")

	tick := time.NewTicker(o.Interval)
	defer tick.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				debounce = time.After(o.Debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			logf("watch error: %v", err)
		case <-debounce:
			debounce = nil
			sweepOnce("new mail")
		case <-tick.C:
			watch() // re-arm if the folder was recreated
			sweepOnce("interval")
		}
	}
}
