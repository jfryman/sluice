package query

import (
	"reflect"
	"testing"
	"time"
)

func TestSQL(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		in    string
		where string
		args  []any
	}{
		{"", "1=1", nil},
		{"list:github.com unread", `list_id LIKE ? ESCAPE '\' AND seen = 0`, []any{"%github.com%"}},
		{`subject:"weekly digest"`, `lower(subject) LIKE ? ESCAPE '\'`, []any{"%weekly digest%"}},
		{`folder:"Deleted Messages"`, `folder = ?`, []any{"Deleted Messages"}},
		{"domain:Shop.com", `(from_domain = ? OR from_domain LIKE ? ESCAPE '\')`, []any{"shop.com", "%.shop.com"}},
		{"older:10d", `date < ?`, []any{now.Add(-240 * time.Hour).Unix()}},
		{"from:a_b", `from_addr LIKE ? ESCAPE '\'`, []any{`%a\_b%`}},
	}
	for _, tt := range tests {
		q, err := Parse(tt.in)
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		q.Now = now
		w, a, err := q.SQL()
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		if w != tt.where || !reflect.DeepEqual(a, tt.args) {
			t.Errorf("%q => %q %v, want %q %v", tt.in, w, a, tt.where, tt.args)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"bogus:x", "from:", `subject:"open`} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
	q, _ := Parse("before:2024-13-01")
	if _, _, err := q.SQL(); err == nil {
		t.Error("bad date: expected error")
	}
}
