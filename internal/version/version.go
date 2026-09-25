// Package version reports what build of sluice is running. The Makefile
// stamps the variables with -ldflags -X from git; unstamped builds fall back
// to the VCS info Go embeds. See lode/development.md#versioning.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set at link time: -X github.com/jfryman/sluice/internal/version.Version=…
var (
	Version = ""
	Commit  = ""
	Branch  = ""
	Date    = ""
)

type Info struct {
	Version, Commit, Branch, Go string
	Date                        string // build time (stamped builds only)
	CommitTime                  string // from embedded VCS info
	Dirty                       bool
}

// Get merges link-time values with the embedded build info.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Branch: Branch, Date: Date, Go: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" && len(s.Value) >= 7 {
					i.Commit = s.Value[:7]
				}
			case "vcs.time":
				i.CommitTime = s.Value
			case "vcs.modified":
				i.Dirty = s.Value == "true"
			}
		}
	}
	if strings.HasSuffix(i.Version, "-dirty") {
		i.Dirty = true
	}
	if i.Version == "" {
		i.Version = "dev"
		if i.Commit != "" {
			i.Version = i.Commit
			if i.Dirty {
				i.Version += "-dirty"
			}
		}
	}
	return i
}

// Short is "sluice <version>".
func Short() string { return "sluice " + Get().Version }

// Long is the multi-line `sluice version` output.
func Long() string {
	i := Get()
	var b strings.Builder
	fmt.Fprintf(&b, "sluice %s\n", i.Version)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "  %-11s %s\n", k, v)
		}
	}
	commit := i.Commit
	if commit != "" && i.Dirty {
		commit += " (uncommitted changes)"
	}
	row("commit", commit)
	row("branch", i.Branch)
	row("built", i.Date)
	if i.Date == "" {
		row("commit time", i.CommitTime)
	}
	row("go", i.Go+" "+runtime.GOOS+"/"+runtime.GOARCH)
	return strings.TrimRight(b.String(), "\n")
}
