package sieve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FilePublisher stands in for the server in the sandbox: the "remote" script
// is a file in Dir. It runs the same drift check and local backup/write as
// Remote, and validates by re-parsing the managed block.
type FilePublisher struct {
	Dir    string
	Script string
}

func (f FilePublisher) Describe() string { return "sandbox " + filepath.Join(f.Dir, f.Script) }

func (f FilePublisher) Publish(localPath, newText string) ([]string, error) {
	oldBytes, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	remotePath := filepath.Join(f.Dir, f.Script)
	remote, err := os.ReadFile(remotePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if normalize(string(remote)) != normalize(string(oldBytes)) {
		return nil, ErrDrift
	}
	if _, err := Parse(newText); err != nil {
		return nil, fmt.Errorf("script rejected: %w", err)
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
	if err := os.WriteFile(remotePath, []byte(newText), 0o644); err != nil {
		os.WriteFile(localPath, oldBytes, 0o644)
		return nil, err
	}
	return []string{"remote matches local", "uploaded + activated " + remotePath}, nil
}
