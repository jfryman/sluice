// Package maildir understands mbsync-managed maildirs: folder discovery,
// filename anatomy, header parsing and UID-safe moves.
package maildir

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Name is a parsed maildir basename: "<key>,U=<uid>:2,<flags>".
type Name struct {
	Key   string
	UID   string
	Flags string
}

func ParseName(base string) Name {
	var n Name
	rest := base
	if i := strings.Index(rest, ":2,"); i >= 0 {
		n.Flags = rest[i+3:]
		rest = rest[:i]
	}
	if i := strings.Index(rest, ",U="); i >= 0 {
		n.UID = rest[i+3:]
		rest = rest[:i]
	}
	n.Key = rest
	return n
}

// Seen reports whether the S flag is set.
func (n Name) Seen() bool { return strings.ContainsRune(n.Flags, 'S') }

// MovedName is the filename a message gets in another folder: U= stripped,
// flags kept. See lode/mail/maildir-and-mbsync.md.
func (n Name) MovedName() string { return n.Key + ":2," + n.Flags }

// Folders lists every maildir folder (a dir with cur/) under root, as
// root-relative paths, sorted.
func Folders(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "cur", "new", "tmp":
			return filepath.SkipDir
		}
		if fi, err := os.Stat(filepath.Join(p, "cur")); err == nil && fi.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if rel != "." {
				out = append(out, rel)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// File is one message file found during a walk.
type File struct {
	Folder string
	Path   string
	Name   Name
}

// Files lists message files in folder's cur/ and new/.
func Files(root, folder string) ([]File, error) {
	var out []File
	for _, sub := range []string{"cur", "new"} {
		dir := filepath.Join(root, folder, sub)
		ents, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			out = append(out, File{Folder: folder, Path: filepath.Join(dir, e.Name()), Name: ParseName(e.Name())})
		}
	}
	return out, nil
}

// Headers is the subset of headers sluice indexes.
type Headers struct {
	Date      time.Time
	FromAddr  string
	FromName  string
	ListID    string
	Subject   string
	Unsub     bool
	MessageID string
}

const maxHeaderBytes = 64 << 10

var dec = mime.WordDecoder{CharsetReader: func(charset string, in io.Reader) (io.Reader, error) {
	// Unknown charsets: pass bytes through rather than failing the header.
	return in, nil
}}

// ReadHeaders parses the header block of the message at path (bounded read).
func ReadHeaders(path string) (Headers, error) {
	f, err := os.Open(path)
	if err != nil {
		return Headers{}, err
	}
	defer f.Close()
	var h Headers
	r := bufio.NewReader(io.LimitReader(f, maxHeaderBytes))
	msg, err := mail.ReadMessage(r)
	if err != nil {
		// Truncated or odd headers: fall back to whatever textproto managed.
		if msg == nil {
			return h, err
		}
	}
	hd := msg.Header
	if d, err := hd.Date(); err == nil {
		h.Date = d
	}
	if fromRaw := hd.Get("From"); fromRaw != "" {
		if a, err := mail.ParseAddress(fromRaw); err == nil {
			h.FromAddr, h.FromName = strings.ToLower(a.Address), a.Name
		} else {
			h.FromAddr = strings.ToLower(extractAddr(fromRaw))
		}
	}
	h.ListID = ListID(hd.Get("List-Id"))
	h.Subject = decode(hd.Get("Subject"))
	h.Unsub = hd.Get("List-Unsubscribe") != ""
	h.MessageID = strings.Trim(hd.Get("Message-Id"), "<> ")
	return h, nil
}

// ListID returns the lower-cased identifier inside <...>, or the trimmed value.
func ListID(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.LastIndex(v, "<"); i >= 0 {
		if j := strings.Index(v[i:], ">"); j > 0 {
			v = v[i+1 : i+j]
		}
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// Domain returns the part after the last '@'.
func Domain(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func decode(s string) string {
	if d, err := dec.DecodeHeader(s); err == nil {
		s = d
	}
	return strings.Join(strings.Fields(s), " ")
}

func extractAddr(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	for _, f := range strings.Fields(s) {
		if strings.Contains(f, "@") {
			return strings.Trim(f, "<>\"'(),;")
		}
	}
	return ""
}

// Move renames src into toFolder/cur with U= stripped and returns the new
// path. It never overwrites. The caller must hold the mbsync lock.
func Move(root, src, toFolder string) (string, error) {
	n := ParseName(filepath.Base(src))
	dst := filepath.Join(root, toFolder, "cur", n.MovedName())
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	// link+remove instead of rename: link refuses to clobber an existing file.
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("destination exists: %s", dst)
		}
		return "", err
	}
	if err := os.Remove(src); err != nil {
		os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// KeyMap maps every message key in folder to its current path.
func KeyMap(root, folder string) (map[string]string, error) {
	files, err := Files(root, folder)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(files))
	for _, f := range files {
		m[f.Name.Key] = f.Path
	}
	return m, nil
}

// FindKey locates the file with the given key in folder (any UID/flags).
func FindKey(root, folder, key string) (string, error) {
	m, err := KeyMap(root, folder)
	if err != nil {
		return "", err
	}
	if p, ok := m[key]; ok {
		return p, nil
	}
	return "", os.ErrNotExist
}
