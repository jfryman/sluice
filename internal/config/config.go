// Package config loads sluice settings from an optional TOML file,
// filling every unset key with a default that matches the KolabNow setup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	MailRoot       string   `toml:"mail_root"`
	TrashFolder    string   `toml:"trash_folder"`
	ExcludeFolders []string `toml:"exclude_folders"`
	// SaneBox triage folders used as "ignored mail" signals in scoring.
	BlackHoleFolders []string `toml:"blackhole_folders"`
	NewsFolders      []string `toml:"news_folders"`
	LockFile         string   `toml:"lock_file"`
	SieveFile        string   `toml:"sieve_file"`
	SieveServer      string   `toml:"sieve_server"`
	SieveScript      string   `toml:"sieve_script"`
	SievePort        int      `toml:"sieve_port"`
	User             string   `toml:"user"` // if set, user_cmd is not run
	UserCmd          []string `toml:"user_cmd"`
	PassCmd          []string `toml:"pass_cmd"`

	SandboxDir string `toml:"sandbox_dir"`
	// TrainingFolder is the drop-to-block folder `sluice sweep` watches.
	TrainingFolder string `toml:"training_folder"`
	// SentFolders hold mail James sent; their recipients are never auto-blocked.
	SentFolders []string `toml:"sent_folders"`
	// SweepInterval is the watch-mode rescan fallback (Go duration).
	SweepInterval string `toml:"sweep_interval"`
	// PostSweepCmd runs after a sweep moved local mail (e.g. ["mail-sync"]).
	PostSweepCmd []string `toml:"post_sweep_cmd"`

	// Derived, not configurable.
	IndexPath   string `toml:"-"`
	JournalPath string `toml:"-"`
	PlanPath    string `toml:"-"` // shared by real and sandbox modes
	SweepLog    string `toml:"-"` // sweep outcomes (JSONL)
	AppliedDir  string `toml:"-"` // archive of plans applied to real
}

func Default() Config {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return Config{
		MailRoot:         "~/Mail/kolab",
		TrashFolder:      "Deleted Messages",
		ExcludeFolders:   []string{"Drafts", "Sent Messages", "Sent Items"},
		BlackHoleFolders: []string{"+SaneBlackHole"},
		NewsFolders:      []string{"+SaneNews"},
		LockFile:         filepath.Join(runtime, "mbsync.lock"),
		SieveFile:        "~/.config/sieve/kolab.sieve",
		SieveServer:      "imap.kolabnow.com",
		SieveScript:      "kolab",
		SievePort:        4190,
		User:             "james@fryman.io",
		UserCmd:          []string{"mail-user"},
		PassCmd:          []string{"mail-pass"},
		SandboxDir:       filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "sluice", "sandbox"),
		TrainingFolder:   "+Sluice",
		SentFolders:      []string{"Sent Messages", "Sent Items"},
		SweepInterval:    "5m",
		PostSweepCmd:     []string{"mail-sync"},
	}
}

// DefaultPath is ~/.config/sluice/config.toml (respecting XDG_CONFIG_HOME).
func DefaultPath() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "sluice", "config.toml")
}

// Load reads path (missing file is fine) over the defaults and expands paths.
func Load(path string) (Config, error) {
	c := Default()
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
	}
	c.MailRoot = Expand(c.MailRoot)
	c.LockFile = Expand(c.LockFile)
	c.SieveFile = Expand(c.SieveFile)
	c.IndexPath = filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "sluice", "index.db")
	c.SandboxDir = Expand(c.SandboxDir)
	state := filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "sluice")
	c.JournalPath = filepath.Join(state, "journal.jsonl")
	c.PlanPath = filepath.Join(state, "plan.json")
	c.AppliedDir = filepath.Join(state, "applied")
	c.SweepLog = filepath.Join(state, "sweep.jsonl")
	return c, nil
}

// Expand resolves a leading ~ and $VARS.
func Expand(p string) string {
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}
