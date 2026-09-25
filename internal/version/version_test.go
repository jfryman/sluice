package version

import (
	"strings"
	"testing"
)

func TestStamped(t *testing.T) {
	defer func(v, c, b, d string) { Version, Commit, Branch, Date = v, c, b, d }(Version, Commit, Branch, Date)
	Version, Commit, Branch, Date = "v0.2.0-3-gabc1234-dirty", "abc1234", "main", "2026-09-25T00:00:00Z"
	i := Get()
	if !i.Dirty || i.Version != "v0.2.0-3-gabc1234-dirty" {
		t.Fatalf("info = %+v", i)
	}
	l := Long()
	for _, want := range []string{"sluice v0.2.0-3-gabc1234-dirty", "commit      abc1234 (uncommitted changes)", "branch      main", "built       2026-09-25T00:00:00Z"} {
		if !strings.Contains(l, want) {
			t.Errorf("Long() missing %q:\n%s", want, l)
		}
	}
	if Short() != "sluice v0.2.0-3-gabc1234-dirty" {
		t.Errorf("Short() = %q", Short())
	}
}

func TestUnstampedFallback(t *testing.T) {
	defer func(v, c, b, d string) { Version, Commit, Branch, Date = v, c, b, d }(Version, Commit, Branch, Date)
	Version, Commit, Branch, Date = "", "", "", ""
	i := Get()
	// Test binaries carry no VCS info, so this is the bare fallback.
	if i.Version == "" || (i.Commit == "" && i.Version != "dev") {
		t.Fatalf("info = %+v", i)
	}
	if strings.Contains(Long(), "built") {
		t.Errorf("unstamped build claims a build time:\n%s", Long())
	}
}
