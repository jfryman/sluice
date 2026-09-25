package tui

import (
	"strings"
	"testing"
)

func TestCollapse(t *testing.T) {
	var d []string
	for i := 0; i < 20; i++ {
		d = append(d, "  same")
	}
	d[10] = "+ new"
	got := collapse(d, 2)
	want := "  … 8 unchanged lines|  same|  same|+ new|  same|  same|  … 7 unchanged lines"
	if strings.Join(got, "|") != want {
		t.Errorf("got %q", strings.Join(got, "|"))
	}
}
