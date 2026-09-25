// Package doctor checks whether this machine meets sluice's requirements
// and says how to fix what doesn't. See lode/doctor.md.
package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/jfryman/sluice/internal/config"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/maildir"
	"github.com/jfryman/sluice/internal/plan"
	"github.com/jfryman/sluice/internal/sandbox"
	"github.com/jfryman/sluice/internal/sieve"
)

type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) Symbol() string { return [...]string{"✓", "!", "✗"}[s] }

type Capability string

const (
	CapBrowse  Capability = "browse & plan"
	CapSandbox Capability = "apply to sandbox"
	CapReal    Capability = "apply to real"
)

var Capabilities = []Capability{CapBrowse, CapSandbox, CapReal}

type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
	Gates  []Capability
}

type Report struct{ Checks []Check }

// Available reports whether no failing check gates c. Browse gates
// everything else too.
func (r Report) Available(c Capability) bool {
	for _, ch := range r.Checks {
		if ch.Status != Fail {
			continue
		}
		for _, g := range ch.Gates {
			if g == c || g == CapBrowse {
				return false
			}
		}
	}
	return true
}

func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

type Options struct {
	Online     bool
	ConfigPath string
	ConfigErr  error // error from config.Load, if any
	// Test seams; nil means the real implementation.
	LookPath   func(string) (string, error)
	MbsyncRC   string // path override for the mbsync config
	RemoteStat func() (sieve.ServerStatus, error)
}

var (
	all      = []Capability{CapBrowse}
	realOnly = []Capability{CapReal}
	sandbx   = []Capability{CapSandbox}
	applies  = []Capability{CapSandbox, CapReal}
)

// Run executes every check against cfg.
func Run(cfg config.Config, o Options) Report {
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	d := &run{cfg: cfg, o: o}
	d.platform()
	d.config()
	folders := d.mailRoot()
	d.folders(folders)
	d.trash(folders)
	d.index()
	d.state()
	d.sieveFile()
	d.binary("sieve-connect", cfg.SieveConnect, "sieve_connect",
		"install sieve-connect (e.g. `pacman -S sieve-connect`, or from github.com/philpennock/sieve-connect)")
	d.credentials()
	d.mbsync()
	d.lock()
	if d.sandbox() {
		d.reflink() // only probe a valid sandbox location: never write near mail_root
	}
	if o.Online {
		d.server()
	}
	return Report{Checks: d.checks}
}

type run struct {
	cfg    config.Config
	o      Options
	checks []Check
}

func (d *run) add(name string, st Status, gates []Capability, detail, fix string) {
	if st == OK {
		fix = ""
	}
	d.checks = append(d.checks, Check{Name: name, Status: st, Detail: detail, Fix: fix, Gates: gates})
}

func (d *run) platform() {
	if runtime.GOOS != "linux" {
		d.add("platform", Warn, all, runtime.GOOS+" is untested",
			"sluice relies on Linux flock and reflink semantics; expect rough edges elsewhere")
		return
	}
	d.add("platform", OK, all, "linux/"+runtime.GOARCH, "")
}

func (d *run) config() {
	switch {
	case d.o.ConfigErr != nil:
		d.add("config", Fail, all, d.o.ConfigErr.Error(), "fix the TOML syntax in "+d.o.ConfigPath)
	case fileExists(d.o.ConfigPath):
		d.add("config", OK, all, d.o.ConfigPath, "")
	default:
		d.add("config", OK, all, "defaults (no "+d.o.ConfigPath+")", "")
	}
}

func (d *run) mailRoot() []string {
	root := d.cfg.MailRoot
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		d.add("mail root", Fail, all, root+" does not exist",
			"set mail_root in "+d.o.ConfigPath+" to your mbsync MaildirStore Path, and run mbsync once")
		return nil
	}
	folders, err := maildir.Folders(root)
	if err != nil || len(folders) == 0 {
		d.add("mail root", Fail, all, root+" contains no maildir folders (dirs with cur/)",
			"point mail_root at the MaildirStore Path and run mbsync to populate it")
		return nil
	}
	d.add("mail root", OK, all, fmt.Sprintf("%s (%d folders)", root, len(folders)), "")
	return folders
}

func (d *run) folders(have []string) {
	if have == nil {
		return
	}
	set := map[string]bool{}
	for _, f := range have {
		set[f] = true
	}
	var missing []string
	for _, group := range []struct {
		key  string
		list []string
	}{{"exclude_folders", d.cfg.ExcludeFolders}, {"blackhole_folders", d.cfg.BlackHoleFolders}, {"news_folders", d.cfg.NewsFolders}} {
		for _, f := range group.list {
			if !set[f] {
				missing = append(missing, group.key+": "+f)
			}
		}
	}
	if len(missing) > 0 {
		d.add("folders", Warn, all, "configured but not found: "+strings.Join(missing, ", "),
			"fix the folder names in "+d.o.ConfigPath+" (names are relative to mail_root) or remove them")
		return
	}
	d.add("folders", OK, all, "exclude / blackhole / news folders all present", "")
}

func (d *run) trash(have []string) {
	if have == nil {
		return
	}
	for _, f := range have {
		if f == d.cfg.TrashFolder {
			d.add("trash folder", OK, applies, f, "")
			return
		}
	}
	d.add("trash folder", Fail, applies, fmt.Sprintf("%q is not a folder under %s", d.cfg.TrashFolder, d.cfg.MailRoot),
		"set trash_folder to your server's trash folder name (it must already exist; Sieve can't create it)")
}

func (d *run) index() {
	ix, err := index.Open(d.cfg.IndexPath, d.cfg.MailRoot, index.Folders{Trash: d.cfg.TrashFolder})
	if err != nil {
		d.add("index", Fail, all, err.Error(), "make "+filepath.Dir(d.cfg.IndexPath)+" writable, or delete a corrupt index.db (it is a cache)")
		return
	}
	defer ix.Close()
	n, _, _ := ix.Stats()
	d.add("index", OK, all, fmt.Sprintf("%s (%d messages indexed)", d.cfg.IndexPath, n), "")
}

func (d *run) state() {
	dir := filepath.Dir(d.cfg.PlanPath)
	if err := writable(dir); err != nil {
		d.add("state", Fail, all, err.Error(), "make "+dir+" writable")
		return
	}
	p, err := plan.Load(d.cfg.PlanPath)
	if err != nil {
		d.add("state", Fail, all, err.Error(), "move the corrupt plan aside (a copy of applied plans is in "+d.cfg.AppliedDir+")")
		return
	}
	detail := "no pending plan"
	if !p.Empty() {
		detail = fmt.Sprintf("plan: %d rule ops, %d messages", len(p.Rules), p.Messages())
	}
	d.add("state", OK, all, detail, "")
}

func (d *run) sieveFile() {
	b, err := os.ReadFile(d.cfg.SieveFile)
	if errors.Is(err, fs.ErrNotExist) {
		d.add("sieve file", Warn, realOnly, d.cfg.SieveFile+" does not exist",
			"download your active script there (e.g. `sieve-edit`), otherwise the first publish will fail the drift check")
		return
	}
	if err != nil {
		d.add("sieve file", Fail, realOnly, err.Error(), "make "+d.cfg.SieveFile+" readable")
		return
	}
	s, err := sieve.Parse(string(b))
	if err != nil {
		d.add("sieve file", Fail, realOnly, err.Error(), "repair the sluice managed block markers in "+d.cfg.SieveFile)
		return
	}
	d.add("sieve file", OK, realOnly, fmt.Sprintf("%s (%d managed rules)", d.cfg.SieveFile, len(s.Rules)), "")
}

func (d *run) binary(name, bin, key, fix string) {
	if p, err := d.o.LookPath(bin); err == nil {
		d.add(name, OK, realOnly, p, "")
		return
	}
	d.add(name, Fail, realOnly, bin+" not found", fix+"; or set "+key+" in "+d.o.ConfigPath)
}

func (d *run) credentials() {
	var bad []string
	for _, c := range []struct {
		key  string
		argv []string
	}{{"user_cmd", d.cfg.UserCmd}, {"pass_cmd", d.cfg.PassCmd}} {
		if len(c.argv) == 0 {
			bad = append(bad, c.key+" is empty")
			continue
		}
		if _, err := d.o.LookPath(c.argv[0]); err != nil {
			bad = append(bad, c.key+": "+c.argv[0]+" not found")
		}
	}
	if len(bad) > 0 {
		d.add("credentials", Fail, realOnly, strings.Join(bad, "; "),
			"set user_cmd / pass_cmd to commands that print your mail username / password (run `sluice doctor -online` to test them)")
		return
	}
	d.add("credentials", OK, realOnly, strings.Join(d.cfg.UserCmd, " ")+" / "+strings.Join(d.cfg.PassCmd, " ")+" (not run; use -online)", "")
}

func (d *run) mbsync() {
	if p, err := d.o.LookPath("mbsync"); err == nil {
		d.add("mbsync", OK, realOnly, p, "")
	} else {
		d.add("mbsync", Warn, realOnly, "mbsync not on PATH",
			"install isync; sluice only moves local files, mbsync is what carries them to the server")
	}

	rc := d.o.MbsyncRC
	if rc == "" {
		rc = config.Expand("~/.mbsyncrc")
		if !fileExists(rc) {
			if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && fileExists(filepath.Join(x, "isyncrc")) {
				rc = filepath.Join(x, "isyncrc")
			}
		}
	}
	mc, err := parseMbsyncRC(rc)
	if err != nil {
		d.add("mbsync config", Warn, realOnly, "cannot read "+rc, "sluice expects the maildir to be managed by mbsync")
		return
	}
	var problems []string
	var fixes []string
	root := filepath.Clean(d.cfg.MailRoot)
	found := false
	for _, p := range mc.paths {
		if filepath.Clean(config.Expand(p)) == root {
			found = true
		}
	}
	if !found {
		problems = append(problems, "no MaildirStore Path is "+d.cfg.MailRoot)
		fixes = append(fixes, "make mail_root match the MaildirStore Path")
	}
	if !mc.pushes() {
		problems = append(problems, "Sync ("+strings.Join(mc.sync, ", ")+") never pushes local changes")
		fixes = append(fixes, "use `Sync All` (or include Push) so moves reach the server")
	}
	if !mc.expungesFar() {
		problems = append(problems, "Expunge ("+orNone(mc.expunge)+") leaves originals on the server")
		fixes = append(fixes, "use `Expunge Both` (or Far) so trashed originals are removed upstream")
	}
	if len(problems) > 0 {
		d.add("mbsync config", Warn, realOnly, rc+": "+strings.Join(problems, "; "), strings.Join(fixes, "; "))
		return
	}
	d.add("mbsync config", OK, realOnly, rc+": store path matches, pushes changes, expunges far side", "")
}

func (d *run) lock() {
	lf := d.cfg.LockFile
	if !fileExists(lf) {
		d.add("sync lock", Warn, applies, lf+" not found",
			"wrap mbsync in `flock "+lf+" mbsync …` (as mail-sync does) so sluice and mbsync never run at once; or set lock_file")
		return
	}
	f, err := os.Open(lf)
	if err != nil {
		d.add("sync lock", Fail, applies, err.Error(), "make "+lf+" readable")
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		d.add("sync lock", Warn, applies, lf+" is held right now (a sync is running)",
			"nothing to fix if it clears; applies wait up to 2 minutes for it")
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	d.add("sync lock", OK, applies, lf, "")
}

func (d *run) sandbox() bool {
	if err := sandbox.New(d.cfg).Validate(d.cfg); err != nil {
		d.add("sandbox dir", Fail, sandbx, err.Error(), "set sandbox_dir to an absolute path ending in /sandbox, outside mail_root")
		return false
	}
	d.add("sandbox dir", OK, sandbx, d.cfg.SandboxDir, "")
	return true
}

// reflink copies one real message (read-only) next to the sandbox with
// --reflink=always to see whether sandbox clones will be free.
func (d *run) reflink() {
	parent := existingParent(d.cfg.SandboxDir)
	src := firstMessage(d.cfg.MailRoot)
	root := filepath.Clean(d.cfg.MailRoot)
	if parent == "" || src == "" || parent == root || strings.HasPrefix(parent, root+"/") {
		return // never write inside the maildir
	}
	tmp, err := os.CreateTemp(parent, ".sluice-doctor-")
	if err != nil {
		d.add("reflink", Warn, sandbx, "cannot write in "+parent, "make "+parent+" writable")
		return
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	out, err := exec.Command("cp", "--reflink=always", src, tmp.Name()).CombinedOutput()
	if err == nil {
		d.add("reflink", OK, sandbx, "sandbox clones share blocks with "+d.cfg.MailRoot+" (near-free)", "")
		return
	}
	size := dirSize(d.cfg.MailRoot)
	free := freeBytes(parent)
	detail := fmt.Sprintf("no reflink between %s and %s (%s); a sandbox is a full %s copy",
		d.cfg.MailRoot, parent, strings.TrimSpace(string(out)), human(size))
	if free >= 0 && free < size {
		d.add("reflink", Fail, sandbx, detail+fmt.Sprintf(", only %s free", human(free)),
			"free space, or move sandbox_dir to a filesystem with room (same btrfs/XFS as mail_root makes it free)")
		return
	}
	d.add("reflink", Warn, sandbx, detail,
		"put sandbox_dir on the same btrfs/XFS filesystem as mail_root for instant, space-free clones")
}

func (d *run) server() {
	stat := d.o.RemoteStat
	if stat == nil {
		r := sieve.Remote{Bin: d.cfg.SieveConnect, Server: d.cfg.SieveServer, Script: d.cfg.SieveScript,
			UserCmd: d.cfg.UserCmd, PassCmd: d.cfg.PassCmd}
		stat = func() (sieve.ServerStatus, error) { return r.Status(d.cfg.SieveFile) }
	}
	st, err := stat()
	switch {
	case err != nil:
		d.add("server", Fail, realOnly, err.Error(),
			"check sieve_server, and that user_cmd/pass_cmd print valid credentials")
	case !st.Exists:
		d.add("server", Fail, realOnly, fmt.Sprintf("script %q not on %s (have: %s)", d.cfg.SieveScript, d.cfg.SieveServer, orNone(st.Scripts)),
			"set sieve_script to your active script's name, or upload one first")
	case st.Drift:
		d.add("server", Warn, realOnly, "remote script differs from "+d.cfg.SieveFile+"; applies will stop at the drift check",
			"re-download the active script (e.g. `sieve-edit`) so local matches the server")
	case !st.Active:
		d.add("server", Warn, realOnly, fmt.Sprintf("script %q exists but is not active", d.cfg.SieveScript),
			"activate it (publishing through sluice also activates it)")
	default:
		d.add("server", OK, realOnly, fmt.Sprintf("%s: %q active, matches local", d.cfg.SieveServer, d.cfg.SieveScript), "")
	}
}

// ---- helpers ----

type mbsyncConf struct {
	paths, sync, expunge []string
}

func parseMbsyncRC(path string) (mbsyncConf, error) {
	var c mbsyncConf
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		val = strings.TrimSpace(val)
		switch strings.ToLower(key) {
		case "path":
			c.paths = append(c.paths, strings.Trim(val, `"`))
		case "sync":
			c.sync = append(c.sync, val)
		case "expunge":
			c.expunge = append(c.expunge, val)
		}
	}
	return c, sc.Err()
}

// pushes: mbsync's default Sync is All; any Sync line must allow Push.
func (c mbsyncConf) pushes() bool {
	for _, s := range c.sync {
		v := strings.ToLower(s)
		if !strings.Contains(v, "all") && !strings.Contains(v, "push") && !strings.Contains(v, "full") {
			return false
		}
	}
	return true
}

// expungesFar: mbsync's default Expunge is None.
func (c mbsyncConf) expungesFar() bool {
	for _, e := range c.expunge {
		v := strings.ToLower(e)
		if strings.Contains(v, "both") || strings.Contains(v, "far") || strings.Contains(v, "master") {
			return true
		}
	}
	return false
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writable creates dir if needed and verifies a file can be written in it.
func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sluice-doctor-")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

func existingParent(p string) string {
	for d := filepath.Dir(p); d != "/" && d != "."; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}

func firstMessage(root string) string {
	folders, _ := maildir.Folders(root)
	for _, f := range folders {
		files, _ := maildir.Files(root, f)
		if len(files) > 0 {
			return files[0].Path
		}
	}
	return ""
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func freeBytes(dir string) int64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return -1
	}
	return int64(s.Bavail) * int64(s.Bsize)
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Lines renders the report: one line per check (symbol, name, detail),
// an indented fix line for anything not ok, then the capability summary.
func (r Report) Lines() []string {
	var out []string
	for _, c := range r.Checks {
		out = append(out, fmt.Sprintf("%s %-14s %s", c.Status.Symbol(), c.Name, c.Detail))
		if c.Fix != "" {
			out = append(out, fmt.Sprintf("  %-14s fix: %s", "", c.Fix))
		}
	}
	out = append(out, "")
	var caps []string
	for _, c := range Capabilities {
		sym := "✓"
		if !r.Available(c) {
			sym = "✗"
		}
		caps = append(caps, sym+" "+string(c))
	}
	out = append(out, "capabilities: "+strings.Join(caps, "   "))
	return out
}
