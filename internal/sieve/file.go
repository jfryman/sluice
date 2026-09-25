package sieve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FilePublisher stands in for the server in the sandbox: the "remote" script
// is a file in Dir, and it is active whenever it exists. It applies the same
// first-publish and drift rules as Remote, and validates by re-parsing.
type FilePublisher struct {
	Dir    string
	Script string
}

func (f FilePublisher) Describe() string { return "sandbox " + filepath.Join(f.Dir, f.Script) }

func (f FilePublisher) Status(localPath string) (ServerStatus, error) {
	var st ServerStatus
	remote, err := os.ReadFile(filepath.Join(f.Dir, f.Script))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	local, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	st.Scripts, st.Active, st.Exists = []string{f.Script}, f.Script, true
	st.Drift = normalize(string(remote)) != normalize(string(local))
	return st, nil
}

func (f FilePublisher) Publish(localPath, newText string, _ PublishOptions) ([]string, error) {
	st, err := f.Status(localPath)
	if err != nil {
		return nil, err
	}
	var log []string
	switch {
	case !st.Exists:
		log = append(log, fmt.Sprintf("no %q in sandbox remote; creating it", f.Script))
	case st.Drift:
		return nil, ErrDrift
	default:
		log = append(log, "remote matches local")
	}
	if _, err := Parse(newText); err != nil {
		return nil, fmt.Errorf("script rejected: %w", err)
	}
	oldBytes, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.WriteFile(localPath+".bak", oldBytes, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(localPath, []byte(newText), 0o644); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return nil, err
	}
	remotePath := filepath.Join(f.Dir, f.Script)
	if err := os.WriteFile(remotePath, []byte(newText), 0o644); err != nil {
		os.WriteFile(localPath, oldBytes, 0o644)
		return nil, err
	}
	return append(log, "uploaded + activated "+remotePath), nil
}
