// Package index is a disposable SQLite cache of maildir headers used for
// ranking candidates and searching. See lode/mail/index.md.
package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jfryman/sluice/internal/maildir"
	"github.com/jfryman/sluice/internal/query"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  key         TEXT PRIMARY KEY,
  folder      TEXT NOT NULL,
  path        TEXT NOT NULL,
  date        INTEGER,
  from_addr   TEXT,
  from_domain TEXT,
  from_name   TEXT,
  list_id     TEXT,
  subject     TEXT,
  seen        INTEGER NOT NULL,
  unsub       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_list   ON messages(list_id);
CREATE INDEX IF NOT EXISTS messages_domain ON messages(from_domain);
CREATE INDEX IF NOT EXISTS messages_addr   ON messages(from_addr);
CREATE INDEX IF NOT EXISTS messages_folder ON messages(folder);
`

// Folders tells the index which folders carry which meaning.
type Folders struct {
	Trash     string
	BlackHole []string // SaneBox "never want to see"
	News      []string // SaneBox bulk / newsletters
	Exclude   []string // never indexed (own mail)
}

type Index struct {
	db   *sql.DB
	root string
	f    Folders
}

func Open(path, root string, f Folders) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("index schema: %w", err)
	}
	return &Index{db: db, root: root, f: f}, nil
}

func (ix *Index) Close() error  { return ix.db.Close() }
func (ix *Index) Root() string  { return ix.root }
func (ix *Index) Trash() string { return ix.f.Trash }

type ScanStats struct {
	Folders, Total, Added, Updated, Removed int
	Took                                    time.Duration
}

type row struct {
	folder, path string
	seen         bool
}

// Scan brings the index in line with the maildir. progress (may be nil) is
// called with (parsed, toParse) while parsing new messages.
func (ix *Index) Scan(progress func(done, total int)) (ScanStats, error) {
	start := time.Now()
	var st ScanStats

	existing := map[string]row{}
	rows, err := ix.db.Query(`SELECT key, folder, path, seen FROM messages`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var k string
		var r row
		if err := rows.Scan(&k, &r.folder, &r.path, &r.seen); err != nil {
			rows.Close()
			return st, err
		}
		existing[k] = r
	}
	rows.Close()

	folders, err := maildir.Folders(ix.root)
	if err != nil {
		return st, err
	}
	excluded := map[string]bool{}
	for _, f := range ix.f.Exclude {
		excluded[f] = true
	}

	var fresh []maildir.File
	var moved []maildir.File
	seenKeys := map[string]bool{}
	for _, folder := range folders {
		if excluded[folder] {
			continue
		}
		st.Folders++
		files, err := maildir.Files(ix.root, folder)
		if err != nil {
			return st, err
		}
		for _, f := range files {
			if seenKeys[f.Name.Key] {
				continue // same message present twice; first wins
			}
			seenKeys[f.Name.Key] = true
			st.Total++
			old, ok := existing[f.Name.Key]
			switch {
			case !ok:
				fresh = append(fresh, f)
			case old.path != f.Path || old.folder != f.Folder || old.seen != f.Name.Seen():
				moved = append(moved, f)
			}
		}
	}

	parsed := parseAll(fresh, progress)

	tx, err := ix.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR REPLACE INTO messages
		(key, folder, path, date, from_addr, from_domain, from_name, list_id, subject, seen, unsub)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return st, err
	}
	for i, f := range fresh {
		h := parsed[i]
		date := h.Date
		if date.IsZero() {
			if fi, err := os.Stat(f.Path); err == nil {
				date = fi.ModTime()
			}
		}
		if _, err := ins.Exec(f.Name.Key, f.Folder, f.Path, date.Unix(), h.FromAddr, maildir.Domain(h.FromAddr),
			h.FromName, h.ListID, h.Subject, f.Name.Seen(), h.Unsub); err != nil {
			return st, err
		}
		st.Added++
	}
	upd, err := tx.Prepare(`UPDATE messages SET folder=?, path=?, seen=? WHERE key=?`)
	if err != nil {
		return st, err
	}
	for _, f := range moved {
		if _, err := upd.Exec(f.Folder, f.Path, f.Name.Seen(), f.Name.Key); err != nil {
			return st, err
		}
		st.Updated++
	}
	del, err := tx.Prepare(`DELETE FROM messages WHERE key=?`)
	if err != nil {
		return st, err
	}
	for k := range existing {
		if !seenKeys[k] {
			if _, err := del.Exec(k); err != nil {
				return st, err
			}
			st.Removed++
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	st.Took = time.Since(start)
	return st, nil
}

func parseAll(files []maildir.File, progress func(done, total int)) []maildir.Headers {
	out := make([]maildir.Headers, len(files))
	if len(files) == 0 {
		return out
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				h, _ := maildir.ReadHeaders(files[i].Path) // unparseable: indexed with empty headers
				out[i] = h
				if progress != nil {
					mu.Lock()
					done++
					if done%500 == 0 || done == len(files) {
						progress(done, len(files))
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// Kind is a grouping dimension; its value is the indexed column.
type Kind string

const (
	KindList   Kind = "list"
	KindDomain Kind = "domain"
	KindSender Kind = "sender"
)

func (k Kind) Column() string {
	switch k {
	case KindList:
		return "list_id"
	case KindDomain:
		return "from_domain"
	default:
		return "from_addr"
	}
}

// Next cycles list → domain → sender → list.
func (k Kind) Next() Kind {
	switch k {
	case KindList:
		return KindDomain
	case KindDomain:
		return KindSender
	default:
		return KindList
	}
}

// QueryTerm is the search term selecting exactly this group.
func (k Kind) QueryTerm(value string) string {
	switch k {
	case KindList:
		return "list:" + query.Quote(value)
	case KindDomain:
		return "domain:" + query.Quote(value)
	default:
		return "from:" + query.Quote(value)
	}
}

type Group struct {
	Kind      Kind
	Value     string
	Name      string // a display name (from_name) for context
	Total     int
	Recent30  int
	Recent90  int
	Unread    int
	BlackHole int
	News      int
	Trashed   int
	Inbox     int
	Last      time.Time
	Subject   string // most recent subject
	Unsub     bool
	Score     float64
}

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Groups aggregates every non-empty group of the given kind.
func (ix *Index) Groups(kind Kind, now time.Time) ([]Group, error) {
	col := kind.Column()
	bh, news := ix.f.BlackHole, ix.f.News
	// max(date) is the only min/max aggregate, so SQLite takes the bare
	// columns from_name and subject from the newest row of each group.
	q := fmt.Sprintf(`
SELECT %[1]s,
  max(date), from_name, subject,
  count(*),
  sum(date >= ?), sum(date >= ?),
  sum(seen = 0),
  sum(folder IN (%[2]s)),
  sum(folder IN (%[3]s)),
  sum(folder = ?),
  sum(folder = 'INBOX'),
  sum(unsub) > 0,
  sum(CASE WHEN folder IN (%[2]s) OR folder = ? OR seen = 0 THEN 1.0
           WHEN folder IN (%[3]s) THEN 0.5 ELSE 0 END)
FROM messages WHERE %[1]s != '' GROUP BY %[1]s`, col, placeholders(len(bh)), placeholders(len(news)))

	var args []any
	args = append(args, now.AddDate(0, 0, -30).Unix(), now.AddDate(0, 0, -90).Unix())
	for _, f := range bh {
		args = append(args, f)
	}
	for _, f := range news {
		args = append(args, f)
	}
	args = append(args, ix.f.Trash)
	for _, f := range bh {
		args = append(args, f)
	}
	args = append(args, ix.f.Trash)
	for _, f := range news {
		args = append(args, f)
	}

	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g := Group{Kind: kind}
		var last int64
		var name, subject sql.NullString
		var ignored float64
		if err := rows.Scan(&g.Value, &last, &name, &subject, &g.Total, &g.Recent30, &g.Recent90, &g.Unread, &g.BlackHole,
			&g.News, &g.Trashed, &g.Inbox, &g.Unsub, &ignored); err != nil {
			return nil, err
		}
		g.Name, g.Subject = name.String, subject.String
		g.Last = time.Unix(last, 0)
		g.Score = Score(g.Recent90, g.Total, ignored)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Score ranks trash candidates: recent volume scaled by the share of mail
// that is ignored (unread, black-holed, trashed; news counts half).
func Score(recent90, total int, ignored float64) float64 {
	share := 0.0
	if total > 0 {
		share = ignored / float64(total)
	}
	if share > 1 {
		share = 1
	}
	return float64(recent90+1) * (0.25 + share)
}

type FolderCount struct {
	Folder string
	Count  int
	Unread int
}

// GroupFolders is the per-folder breakdown of one group.
func (ix *Index) GroupFolders(kind Kind, value string) ([]FolderCount, error) {
	rows, err := ix.db.Query(fmt.Sprintf(
		`SELECT folder, count(*), sum(seen=0) FROM messages WHERE %s = ? GROUP BY folder ORDER BY 2 DESC`, kind.Column()), value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderCount
	for rows.Next() {
		var fc FolderCount
		if err := rows.Scan(&fc.Folder, &fc.Count, &fc.Unread); err != nil {
			return nil, err
		}
		out = append(out, fc)
	}
	return out, rows.Err()
}

type Message struct {
	Key, Folder, Path  string
	Date               time.Time
	FromAddr, FromName string
	ListID, Subject    string
	Seen               bool
}

// Search returns up to limit newest matches (limit <= 0: all) and the total count.
func (ix *Index) Search(q query.Query, limit int) ([]Message, int, error) {
	where, args, err := q.SQL()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := ix.db.QueryRow(`SELECT count(*) FROM messages WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sqlq := `SELECT key, folder, path, date, from_addr, from_name, list_id, subject, seen
		FROM messages WHERE ` + where + ` ORDER BY date DESC`
	if limit > 0 {
		sqlq += fmt.Sprintf(" LIMIT %d", limit)
	}
	out, err := ix.messages(sqlq, args...)
	return out, total, err
}

// Relocate records that a message moved (after a trash or undo).
func (ix *Index) Relocate(key, folder, path string) error {
	_, err := ix.db.Exec(`UPDATE messages SET folder=?, path=? WHERE key=?`, folder, path, key)
	return err
}

// Stats returns (messages, folders) currently indexed.
func (ix *Index) Stats() (int, int, error) {
	var n, f int
	err := ix.db.QueryRow(`SELECT count(*), count(DISTINCT folder) FROM messages`).Scan(&n, &f)
	return n, f, err
}

// SortKey orders candidate groups.
type SortKey int

const (
	SortScore SortKey = iota
	SortRecent
	SortTotal
	SortUnread
	sortKeys
)

func (s SortKey) String() string {
	return [...]string{"score", "90d", "total", "unread%"}[s]
}

func (s SortKey) Next() SortKey { return (s + 1) % sortKeys }

// SortGroups sorts descending by key, ties broken by total then value.
func SortGroups(gs []Group, key SortKey) {
	metric := func(g Group) float64 {
		switch key {
		case SortRecent:
			return float64(g.Recent90)
		case SortTotal:
			return float64(g.Total)
		case SortUnread:
			return float64(g.Unread) / float64(max(g.Total, 1))
		}
		return g.Score
	}
	sort.SliceStable(gs, func(i, j int) bool {
		a, b := metric(gs[i]), metric(gs[j])
		if a != b {
			return a > b
		}
		if gs[i].Total != gs[j].Total {
			return gs[i].Total > gs[j].Total
		}
		return gs[i].Value < gs[j].Value
	})
}

// Matching returns messages whose group column equals one of values, newest
// first. It mirrors what the equivalent Sieve rule would have caught.
func (ix *Index) Matching(kind Kind, values []string, excludeTrash bool, limit int) ([]Message, error) {
	if len(values) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(values)+1)
	for _, v := range values {
		args = append(args, v)
	}
	q := fmt.Sprintf(`SELECT key, folder, path, date, from_addr, from_name, list_id, subject, seen
		FROM messages WHERE %s IN (%s)`, kind.Column(), placeholders(len(values)))
	if excludeTrash {
		q += ` AND folder != ?`
		args = append(args, ix.f.Trash)
	}
	q += ` ORDER BY date DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	return ix.messages(q, args...)
}

func (ix *Index) messages(q string, args ...any) ([]Message, error) {
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var d int64
		if err := rows.Scan(&m.Key, &m.Folder, &m.Path, &d, &m.FromAddr, &m.FromName, &m.ListID, &m.Subject, &m.Seen); err != nil {
			return nil, err
		}
		m.Date = time.Unix(d, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ByKeys returns the indexed messages with the given keys, newest first.
func (ix *Index) ByKeys(keys []string) ([]Message, error) {
	var out []Message
	const chunk = 500
	for i := 0; i < len(keys); i += chunk {
		part := keys[i:min(i+chunk, len(keys))]
		args := make([]any, len(part))
		for j, k := range part {
			args[j] = k
		}
		ms, err := ix.messages(`SELECT key, folder, path, date, from_addr, from_name, list_id, subject, seen
			FROM messages WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

func (s SortKey) Prev() SortKey { return (s + sortKeys - 1) % sortKeys }
