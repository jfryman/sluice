package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jfryman/sluice/internal/imap"
	"github.com/jfryman/sluice/internal/index"
	"github.com/jfryman/sluice/internal/maildir"
)

// Mover moves messages on the IMAP server (imap.Remote). A Cleaner with a
// Mover trashes synced messages server-side and leaves their local files
// for mbsync to reconcile; see lode/mail/server-moves.md.
type Mover interface {
	MoveAll(groups []imap.Group, progress func(string)) ([]imap.Result, error)
}

// trashOnServer moves every message it can map to a far UID with the
// Mover, journaling confirmed moves, and returns the rest for local moves.
// Folders whose mbsync state is pending or whose UIDVALIDITY changed fall
// back to local moves, noted in res.Notes.
func (c *Cleaner) trashOnServer(msgs []index.Message, keys *keyCache, enc *json.Encoder, res *Result) ([]index.Message, error) {
	type folderState struct {
		validity uint32
		far      map[uint32]uint32
		err      error
	}
	states := map[string]*folderState{}
	byFolder := map[string]map[uint32]index.Message{} // folder → far UID → message
	var local []index.Message
	for _, m := range msgs {
		if m.Folder == c.TrashFolder {
			continue
		}
		st, ok := states[m.Folder]
		if !ok {
			st = &folderState{}
			st.validity, st.far, st.err = maildir.FarUIDs(c.Root, m.Folder)
			if st.err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: moved locally (%v)", m.Folder, st.err))
			}
			states[m.Folder] = st
		}
		src := m.Path
		if _, err := os.Stat(src); err != nil {
			if src, err = keys.find(m.Folder, m.Key); err != nil {
				res.skip(m.Key, m.Key+": no longer in "+m.Folder)
				continue
			}
		}
		far, ok := st.far[maildir.NearUID(filepath.Base(src))]
		if st.err != nil || !ok {
			local = append(local, m)
			continue
		}
		if byFolder[m.Folder] == nil {
			byFolder[m.Folder] = map[uint32]index.Message{}
		}
		byFolder[m.Folder][far] = m
	}
	if len(byFolder) == 0 {
		return local, nil
	}

	var groups []imap.Group
	for f, ms := range byFolder {
		g := imap.Group{From: f, Validity: states[f].validity, To: c.TrashFolder}
		for u := range ms {
			g.UIDs = append(g.UIDs, u)
		}
		sort.Slice(g.UIDs, func(i, j int) bool { return g.UIDs[i] < g.UIDs[j] })
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].From < groups[j].From })
	results, err := c.Server.MoveAll(groups, c.stage)
	if errors.Is(err, imap.ErrUnsupported) {
		res.Notes = append(res.Notes, "server can't move with COPYUID: moved locally")
		for _, ms := range byFolder {
			for _, m := range ms {
				local = append(local, m)
			}
		}
		return local, nil
	}
	for _, r := range results {
		ms := byFolder[r.From]
		if r.Stale {
			res.Notes = append(res.Notes, r.From+": UIDVALIDITY changed since mbsync's last sync; moved locally")
			for _, m := range ms {
				local = append(local, m)
			}
			continue
		}
		confirmed := map[uint32]bool{}
		for _, p := range r.Pairs {
			m := ms[p.Src]
			confirmed[p.Src] = true
			if werr := enc.Encode(Entry{Batch: res.Batch, Key: m.Key, From: r.From, To: r.To,
				Mode: ModeServer, FarUID: p.Src, DstUID: p.Dst, DstValid: r.DstValidity}); werr != nil {
				return local, fmt.Errorf("journal write failed after a server move: %w", werr)
			}
			res.Moved++
			res.Server++
		}
		if err == nil { // on error the unconfirmed rest is reported below
			for u, m := range ms {
				if !confirmed[u] {
					res.skip(m.Key, m.Key+": not in "+r.From+" on the server (already moved?)")
				}
			}
		}
		delete(byFolder, r.From)
	}
	if err != nil {
		for _, ms := range byFolder { // groups never reached, or the failed one's remainder
			for _, m := range ms {
				res.skip(m.Key, m.Key+": not moved ("+err.Error()+")")
			}
		}
		return nil, fmt.Errorf("server move: %w", err)
	}
	return local, nil
}

// undoOnServer moves a batch's server entries back to their folders.
func (c *Cleaner) undoOnServer(entries []Entry, res *Result) error {
	if len(entries) == 0 {
		return nil
	}
	if c.Server == nil {
		for _, e := range entries {
			res.skip(e.Key, e.Key+": moved on the server; undo needs server_moves")
		}
		return nil
	}
	type key struct {
		trash, back string
		valid       uint32
	}
	byKey := map[key]map[uint32]Entry{}
	for _, e := range entries {
		if e.DstUID == 0 {
			res.skip(e.Key, e.Key+": no COPYUID recorded; find it in "+e.To+" by hand")
			continue
		}
		k := key{e.To, e.From, e.DstValid}
		if byKey[k] == nil {
			byKey[k] = map[uint32]Entry{}
		}
		byKey[k][e.DstUID] = e
	}
	var groups []imap.Group
	for k, es := range byKey {
		g := imap.Group{From: k.trash, Validity: k.valid, To: k.back}
		for u := range es {
			g.UIDs = append(g.UIDs, u)
		}
		sort.Slice(g.UIDs, func(i, j int) bool { return g.UIDs[i] < g.UIDs[j] })
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].To < groups[j].To })
	results, err := c.Server.MoveAll(groups, c.stage)
	for _, r := range results {
		es := byKey[key{r.From, r.To, r.Validity}]
		if r.Stale {
			for _, e := range es {
				res.skip(e.Key, e.Key+": "+r.From+" UIDVALIDITY changed; can't find it by UID")
			}
			continue
		}
		back := map[uint32]bool{}
		for _, p := range r.Pairs {
			back[p.Src] = true
			res.Moved++
			res.Server++
		}
		for u, e := range es {
			if !back[u] {
				res.skip(e.Key, e.Key+": not in "+r.From+" on the server (expunged?)")
			}
		}
	}
	return err
}

func (c *Cleaner) stage(s string) {
	if c.Stage != nil {
		c.Stage(s)
	}
}
