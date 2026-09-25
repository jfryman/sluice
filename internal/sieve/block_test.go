package sieve

import (
	"strings"
	"testing"
)

const existing = `# header comment mentioning require ["x"]; in prose
require ["fileinto"];

# --- Newsletters ---
if header :contains "List-Id" "substack.com" {
    fileinto "Newsletters";
    stop;
}
`

func TestInsertRoundTrip(t *testing.T) {
	s, err := Parse(existing)
	if err != nil {
		t.Fatal(err)
	}
	if s.HasBlock || len(s.Rules) != 0 {
		t.Fatalf("unexpected block: %+v", s)
	}
	s.Upsert(Rule{Kind: KindList, Value: `we"ird.list`, Action: ActionTrash, Added: "2026-09-24"})
	s.Upsert(Rule{Kind: KindDomain, Value: "promo.shop.com", Action: ActionFile, Folder: "+SaneNews"})
	out := s.Render("Deleted Messages")

	for _, want := range []string{
		`require ["fileinto", "imap4flags"];`,
		`if header :contains "List-Id" "we\"ird.list" {`,
		`    fileinto "Deleted Messages";`,
		`if address :domain :is "from" "promo.shop.com" {`,
		`    fileinto "+SaneNews";`,
		`# header comment mentioning require ["x"]; in prose`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Managed block must precede hand-written rules.
	if strings.Index(out, BeginMarker) > strings.Index(out, "# --- Newsletters") {
		t.Error("block not inserted before existing rules")
	}

	s2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.HasBlock || len(s2.Rules) != 2 || s2.Rules[0].Value != `we"ird.list` {
		t.Fatalf("round trip rules = %+v", s2.Rules)
	}
	if again := s2.Render("Deleted Messages"); again != out {
		t.Errorf("render not idempotent:\n%s\n---\n%s", out, again)
	}

	// Upsert replaces, Remove drops.
	s2.Upsert(Rule{Kind: KindList, Value: `WE"IRD.list`, Action: ActionRead})
	if len(s2.Rules) != 2 || s2.Rules[0].Action != ActionRead {
		t.Fatalf("upsert = %+v", s2.Rules)
	}
	s2.Remove(0)
	s2.Remove(0)
	empty := s2.Render("Deleted Messages")
	if !strings.Contains(empty, BeginMarker+"\n"+EndMarker) || !strings.Contains(empty, "# --- Newsletters") {
		t.Errorf("empty block render:\n%s", empty)
	}
}

func TestNoRequire(t *testing.T) {
	s, _ := Parse("keep;\n")
	s.Upsert(Rule{Kind: KindSender, Value: "a@b.c", Action: ActionTrash})
	out := s.Render("Trash")
	if !strings.HasPrefix(out, `require ["fileinto", "imap4flags"];`+"\n"+BeginMarker) {
		t.Errorf("got:\n%s", out)
	}
	if !strings.Contains(out, `address :all :is "from" "a@b.c"`) {
		t.Errorf("sender test missing:\n%s", out)
	}
}

func TestDiff(t *testing.T) {
	d := Diff("a\nb\nc\n", "a\nx\nc\n")
	want := []string{"  a", "- b", "+ x", "  c"}
	if strings.Join(d, "|") != strings.Join(want, "|") {
		t.Errorf("diff = %q", d)
	}
}
