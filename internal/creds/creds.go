// Package creds resolves the mail login shared by the ManageSieve and IMAP
// clients: a configured user (or user_cmd output) and pass_cmd output.
// The password is never logged; callers zero it after use (Wipe).
package creds

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jfryman/sluice/internal/timing"
)

// Get returns the user (user, else userCmd's output) and passCmd's output.
// Stages "user" and "pass" are recorded on trace.
func Get(user string, userCmd, passCmd []string, trace *timing.Recorder) (string, []byte, error) {
	if user == "" {
		done := trace.Time("user")
		u, err := Output(userCmd)
		done()
		if err != nil {
			return "", nil, fmt.Errorf("credentials: %w", err)
		}
		user = string(u)
	}
	done := trace.Time("pass")
	pass, err := Output(passCmd)
	done()
	if err != nil {
		return "", nil, fmt.Errorf("credentials: %w", err)
	}
	return user, pass, nil
}

// Output runs argv and returns its stdout without trailing newlines.
func Output(argv []string) ([]byte, error) {
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

// Wipe zeroes a password buffer.
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
