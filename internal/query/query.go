// Package query parses the search mini-language into a SQL WHERE clause over
// the index's messages table. See lode/cleanup/search-and-trash.md.
package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Term struct {
	Field string // "" for bare words
	Value string
}

type Query struct {
	Terms []Term
	Now   time.Time // for older:, defaults to time.Now()
}

func Parse(s string) (Query, error) {
	q := Query{Now: time.Now()}
	toks, err := tokenize(s)
	if err != nil {
		return q, err
	}
	for _, t := range toks {
		field, val, ok := strings.Cut(t, ":")
		if !ok {
			switch strings.ToLower(t) {
			case "unread", "read":
				q.Terms = append(q.Terms, Term{Field: strings.ToLower(t)})
			default:
				q.Terms = append(q.Terms, Term{Value: t})
			}
			continue
		}
		field = strings.ToLower(field)
		switch field {
		case "from", "domain", "list", "folder", "subject", "before", "after", "older":
		default:
			return q, fmt.Errorf("unknown field %q", field)
		}
		if val == "" {
			return q, fmt.Errorf("%s: needs a value", field)
		}
		q.Terms = append(q.Terms, Term{Field: field, Value: val})
	}
	return q, nil
}

// tokenize splits on spaces, honouring double quotes anywhere in a token
// (so subject:"weekly digest" is one token with the quotes removed).
func tokenize(s string) ([]string, error) {
	var out []string
	var b strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ, have = !inQ, true
		case unicode.IsSpace(r) && !inQ:
			if have {
				out = append(out, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteRune(r)
			have = true
		}
	}
	if inQ {
		return nil, fmt.Errorf("unterminated quote")
	}
	if have {
		out = append(out, b.String())
	}
	return out, nil
}

func like(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.ToLower(v)) + "%"
}

// SQL renders the AND of all terms. An empty query matches everything.
func (q Query) SQL() (string, []any, error) {
	var parts []string
	var args []any
	for _, t := range q.Terms {
		switch t.Field {
		case "":
			parts = append(parts, `(lower(subject) LIKE ? ESCAPE '\' OR from_addr LIKE ? ESCAPE '\' OR lower(from_name) LIKE ? ESCAPE '\')`)
			l := like(t.Value)
			args = append(args, l, l, l)
		case "from":
			parts = append(parts, `from_addr LIKE ? ESCAPE '\'`)
			args = append(args, like(t.Value))
		case "list":
			parts = append(parts, `list_id LIKE ? ESCAPE '\'`)
			args = append(args, like(t.Value))
		case "subject":
			parts = append(parts, `lower(subject) LIKE ? ESCAPE '\'`)
			args = append(args, like(t.Value))
		case "domain":
			d := strings.ToLower(t.Value)
			parts = append(parts, `(from_domain = ? OR from_domain LIKE ? ESCAPE '\')`)
			args = append(args, d, "%."+strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(d))
		case "folder":
			parts = append(parts, `folder = ?`)
			args = append(args, t.Value)
		case "unread":
			parts = append(parts, `seen = 0`)
		case "read":
			parts = append(parts, `seen = 1`)
		case "before", "after":
			d, err := time.ParseInLocation("2006-01-02", t.Value, time.Local)
			if err != nil {
				return "", nil, fmt.Errorf("%s: want YYYY-MM-DD", t.Field)
			}
			op := "<"
			if t.Field == "after" {
				op = ">="
			}
			parts = append(parts, "date "+op+" ?")
			args = append(args, d.Unix())
		case "older":
			dur, err := ParseAge(t.Value)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, "date < ?")
			args = append(args, q.Now.Add(-dur).Unix())
		}
	}
	if len(parts) == 0 {
		return "1=1", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// ParseAge parses "90d", "2w", "6m" (30d), "1y" (365d).
func ParseAge(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("older: want e.g. 90d, 6m, 1y")
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("older: want e.g. 90d, 6m, 1y")
	}
	day := 24 * time.Hour
	switch s[len(s)-1] {
	case 'd':
		return time.Duration(n) * day, nil
	case 'w':
		return time.Duration(n) * 7 * day, nil
	case 'm':
		return time.Duration(n) * 30 * day, nil
	case 'y':
		return time.Duration(n) * 365 * day, nil
	}
	return 0, fmt.Errorf("older: unit must be d, w, m or y")
}

// Quote renders a value safely for re-parsing.
func Quote(v string) string {
	if strings.ContainsAny(v, " \t\"") {
		return `"` + strings.ReplaceAll(v, `"`, "") + `"`
	}
	return v
}
