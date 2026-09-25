// Package cleanup moves messages into the trash folder under the mbsync lock,
// journaling each batch so it can be undone. See lode/cleanup/search-and-trash.md.
package cleanup

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/maildir"
)

type Entry struct {
	Batch string `json:"batch,omitempty"`
	Key   string `json:"key,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Undo  string `json:"undo,omitempty"` // marker: this batch was undone
}

type Cleaner struct {
	Index       *index.Index
	Root        string
	TrashFolder string
	LockFile    string
	JournalPath string
	LockTimeout time.Duration
}

type Result struct {
	Batch       string
	Moved       int
	Skipped     []string        // human-readable reasons
	SkippedKeys map[string]bool // keys that were not moved
}

func (r *Result) skip(key, reason string) {
	r.Skipped = append(r.Skipped, reason)
	if r.SkippedKeys == nil {
		r.SkippedKeys = map[string]bool{}
	}
	r.SkippedKeys[key] = true
}

func (c *Cleaner) relocate(key, folder, path string) {
	if c.Index != nil {
		c.Index.Relocate(key, folder, path)
	}
}

func (c *Cleaner) lock() (func(), error) {
	t := c.LockTimeout
	if t == 0 {
		t = 2 * time.Minute
	}
	return maildir.Lock(c.LockFile, t)
}

// Trash moves msgs into the trash folder as one journaled batch.
func (c *Cleaner) Trash(msgs []index.Message) (Result, error) {
	res := Result{Batch: time.Now().Format("20060102T150405.000")}
	unlock, err := c.lock()
	if err != nil {
		return res, err
	}
	defer unlock()

	j, err := c.openJournal()
	if err != nil {
		return res, err
	}
	defer j.Close()
	// Unbuffered: a line must hit the journal for every file that moved.
	enc := json.NewEncoder(j)
	keys := keyCache{root: c.Root}

	for _, m := range msgs {
		if m.Folder == c.TrashFolder {
			continue
		}
		src := m.Path
		if _, err := os.Stat(src); err != nil {
			// Flags may have changed since indexing; find by key.
			if src, err = keys.find(m.Folder, m.Key); err != nil {
				res.skip(m.Key, m.Key+": no longer in "+m.Folder)
				continue
			}
		}
		dst, err := maildir.Move(c.Root, src, c.TrashFolder)
		if err != nil {
			res.skip(m.Key, err.Error())
			continue
		}
		if err := enc.Encode(Entry{Batch: res.Batch, Key: m.Key, From: m.Folder, To: c.TrashFolder}); err != nil {
			return res, fmt.Errorf("journal write failed after moving %s: %w", m.Key, err)
		}
		c.relocate(m.Key, c.TrashFolder, dst)
		res.Moved++
	}
	return res, nil
}

// Undo reverts the most recent batch not already undone.
func (c *Cleaner) Undo() (Result, error) {
	entries, err := c.readJournal()
	if err != nil {
		return Result{}, err
	}
	undone := map[string]bool{}
	last := ""
	for _, e := range entries {
		if e.Undo != "" {
			undone[e.Undo] = true
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if b := entries[i].Batch; b != "" && !undone[b] {
			last = b
			break
		}
	}
	res := Result{Batch: last}
	if last == "" {
		return res, errors.New("nothing to undo")
	}

	unlock, err := c.lock()
	if err != nil {
		return res, err
	}
	defer unlock()

	keys := keyCache{root: c.Root}
	for _, e := range entries {
		if e.Batch != last {
			continue
		}
		src, err := keys.find(e.To, e.Key)
		if err != nil {
			res.skip(e.Key, e.Key+": not in "+e.To+" (expunged?)")
			continue
		}
		dst, err := maildir.Move(c.Root, src, e.From)
		if err != nil {
			res.skip(e.Key, err.Error())
			continue
		}
		c.relocate(e.Key, e.From, dst)
		res.Moved++
	}

	j, err := c.openJournal()
	if err != nil {
		return res, err
	}
	defer j.Close()
	return res, json.NewEncoder(j).Encode(Entry{Undo: last})
}

func (c *Cleaner) openJournal() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(c.JournalPath), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(c.JournalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func (c *Cleaner) readJournal() ([]Entry, error) {
	f, err := os.Open(c.JournalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// keyCache resolves keys to paths, scanning each folder at most once.
type keyCache struct {
	root    string
	folders map[string]map[string]string
}

func (k *keyCache) find(folder, key string) (string, error) {
	if k.folders == nil {
		k.folders = map[string]map[string]string{}
	}
	m, ok := k.folders[folder]
	if !ok {
		var err error
		if m, err = maildir.KeyMap(k.root, folder); err != nil {
			return "", err
		}
		k.folders[folder] = m
	}
	if p, ok := m[key]; ok {
		return p, nil
	}
	return "", os.ErrNotExist
}
