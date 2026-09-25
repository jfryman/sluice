package sieve

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Publisher makes newText the active script, keeping localPath in sync.
type Publisher interface {
	Publish(localPath, newText string) (log []string, err error)
	Describe() string
}

// Remote publishes scripts with sieve-connect, fetching credentials from
// helper commands exactly like ~/.local/bin/sieve-edit. See lode/sieve/remote.md.
type Remote struct {
	Bin     string // sieve-connect executable; "" means "sieve-connect" on PATH
	Server  string
	Script  string
	UserCmd []string
	PassCmd []string
}

type creds struct {
	user string
	pass []byte
}

func (c *creds) wipe() {
	for i := range c.pass {
		c.pass[i] = 0
	}
}

func output(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty credential command")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return bytes.TrimRight(out, "\r\n"), nil
}

func (r Remote) Describe() string { return r.Server + " as " + r.Script }

func (r Remote) creds() (*creds, error) {
	u, err := output(r.UserCmd)
	if err != nil {
		return nil, err
	}
	p, err := output(r.PassCmd)
	if err != nil {
		return nil, err
	}
	return &creds{user: string(u), pass: p}, nil
}

// run invokes sieve-connect with the password on fd 3 (never on disk or argv).
func (r Remote) run(c *creds, args ...string) (string, error) {
	base := []string{"--server", r.Server, "--user", c.user, "--passwordfd", "3"}
	bin := r.Bin
	if bin == "" {
		bin = "sieve-connect"
	}
	cmd := exec.Command(bin, append(base, args...)...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.ExtraFiles = []*os.File{pr}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return "", err
	}
	pr.Close()
	// --passwordfd reads until the newline before EOF.
	pw.Write(append(append([]byte{}, c.pass...), '\n'))
	pw.Close()
	err = cmd.Wait()
	if err != nil {
		return out.String(), fmt.Errorf("sieve-connect %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func (r Remote) download(c *creds) (string, error) {
	dir, err := os.MkdirTemp("", "sluice-sieve-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, r.Script+".sieve")
	if _, err := r.run(c, "--download", "--remotesieve", r.Script, "--localsieve", path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

func normalize(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ErrDrift means the server's script is not what the local file says.
var ErrDrift = errors.New("remote script differs from local file; reconcile with sieve-edit first")

// Publish writes newText to localPath and makes it the active remote script:
// drift check → backup + write → checkscript → upload → activate.
// Any failure after the write restores the previous local file.
func (r Remote) Publish(localPath, newText string) (log []string, err error) {
	c, err := r.creds()
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	defer c.wipe()

	oldBytes, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := string(oldBytes)

	remote, err := r.download(c)
	if err != nil {
		if strings.TrimSpace(old) != "" {
			return nil, fmt.Errorf("download remote script: %w", err)
		}
		log = append(log, "no remote script yet")
	} else if normalize(remote) != normalize(old) {
		return nil, ErrDrift
	}
	log = append(log, "remote matches local")

	bak := localPath + ".bak"
	if err := os.WriteFile(bak, oldBytes, 0o644); err != nil {
		return log, err
	}
	if err := os.WriteFile(localPath, []byte(newText), 0o644); err != nil {
		return log, err
	}
	restore := func() { os.WriteFile(localPath, oldBytes, 0o644) }

	if out, err := r.run(c, "--checkscript", "--localsieve", localPath); err != nil {
		// Servers without a VERSION capability can't CHECKSCRIPT; PUTSCRIPT still validates.
		if !strings.Contains(strings.ToUpper(out), "VERSION") {
			restore()
			return log, fmt.Errorf("server rejected script: %w", err)
		}
		log = append(log, "checkscript unsupported; relying on upload validation")
	} else {
		log = append(log, "server validated script")
	}
	if _, err := r.run(c, "--upload", "--localsieve", localPath, "--remotesieve", r.Script); err != nil {
		restore()
		return log, err
	}
	log = append(log, "uploaded "+r.Script)
	if _, err := r.run(c, "--activate", "--remotesieve", r.Script); err != nil {
		return log, fmt.Errorf("uploaded but activation failed: %w", err)
	}
	log = append(log, "activated "+r.Script)
	return log, nil
}

// ServerStatus is what the server says about the configured script.
type ServerStatus struct {
	Scripts []string
	Exists  bool
	Active  bool
	Drift   bool // remote text differs from localPath
}

// Status logs in, lists scripts and compares the remote script with
// localPath. It runs the credential commands (may prompt).
func (r Remote) Status(localPath string) (ServerStatus, error) {
	var st ServerStatus
	c, err := r.creds()
	if err != nil {
		return st, fmt.Errorf("credentials: %w", err)
	}
	defer c.wipe()
	out, err := r.run(c, "--list")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		name := strings.Trim(f[0], `"`)
		st.Scripts = append(st.Scripts, name)
		if name == r.Script {
			st.Exists = true
			st.Active = strings.Contains(strings.ToLower(line), "active")
		}
	}
	if !st.Exists {
		return st, nil
	}
	remote, err := r.download(c)
	if err != nil {
		return st, err
	}
	local, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	st.Drift = normalize(remote) != normalize(string(local))
	return st, nil
}
