package imap

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jfryman/sluice/internal/imap/imaptest"
	"github.com/jfryman/sluice/internal/timing"
)

func remoteFor(s *imaptest.Server, rec *timing.Recorder) Remote {
	h, p := s.HostPort()
	return Remote{Server: h, Port: p, User: s.User, PassCmd: []string{"echo", s.Pass}, TLS: s.ClientTLS, Trace: rec}
}

func TestSets(t *testing.T) {
	if got := FormatSet([]uint32{7, 1, 2, 3, 3, 10, 11}); got != "1:3,7,10:11" {
		t.Errorf("FormatSet = %q", got)
	}
	got, err := ParseSet("5:3,9,12:13")
	if err != nil || !reflect.DeepEqual(got, []uint32{3, 4, 5, 9, 12, 13}) {
		t.Errorf("ParseSet = %v %v", got, err)
	}
	if _, err := ParseSet("1:x"); err == nil {
		t.Error("bad set: expected error")
	}
	if q, err := quote(`a"b\c`); err != nil || q != `"a\"b\\c"` {
		t.Errorf("quote = %q %v", q, err)
	}
	if _, err := quote("Entwürfe"); err == nil {
		t.Error("non-ASCII mailbox: expected error")
	}
}

func TestMoveAll(t *testing.T) {
	for _, opt := range []string{"", "nomove"} { // MOVE, then the COPY+EXPUNGE fallback
		t.Run("opt="+opt, func(t *testing.T) {
			s := imaptest.New(t, opt)
			s.Put("Archive", 111, 1, 2, 3, 4)
			s.Put("+SaneLater", 222, 50, 51)
			s.Put("Deleted Messages", 999, 7)
			rec := &timing.Recorder{}
			var steps []string
			res, err := remoteFor(s, rec).MoveAll([]Group{
				{From: "Archive", Validity: 111, UIDs: []uint32{2, 4, 6}, To: "Deleted Messages"}, // 6 isn't there
				{From: "+SaneLater", Validity: 333, UIDs: []uint32{50}, To: "Deleted Messages"},   // stale validity
			}, func(st string) { steps = append(steps, st) })
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 2 || res[0].DstValidity != 999 || !reflect.DeepEqual(res[0].Pairs, []Pair{{2, 8}, {4, 9}}) {
				t.Fatalf("archive result = %+v", res)
			}
			if !res[1].Stale || len(res[1].Pairs) != 0 {
				t.Errorf("stale result = %+v", res[1])
			}
			if got := s.UIDs("Archive"); !reflect.DeepEqual(got, []uint32{1, 3}) {
				t.Errorf("Archive left = %v", got)
			}
			if got := s.UIDs("+SaneLater"); len(got) != 2 {
				t.Errorf("stale folder was touched: %v", got)
			}
			if got := s.UIDs("Deleted Messages"); !reflect.DeepEqual(got, []uint32{7, 8, 9}) {
				t.Errorf("trash = %v", got)
			}
			if s.Logins != 1 || len(steps) == 0 || !strings.HasPrefix(steps[0], "moving on server 0/4 (Archive)") {
				t.Errorf("logins=%d steps=%q", s.Logins, steps)
			}
			names := ""
			for _, st := range rec.Stages() {
				names += st.Name + ","
			}
			if !strings.HasPrefix(names, "pass,connect,tls,auth,") || !strings.Contains(names, "move") {
				t.Errorf("trace = %s", names)
			}
		})
	}
}

func TestMoveAllRefusesWithoutUIDPlus(t *testing.T) {
	s := imaptest.New(t, "nouidplus")
	s.Put("Archive", 1, 1)
	s.Put("Deleted Messages", 2)
	_, err := remoteFor(s, nil).MoveAll([]Group{{From: "Archive", Validity: 1, UIDs: []uint32{1}, To: "Deleted Messages"}}, nil)
	if err != ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
	if got := s.UIDs("Archive"); len(got) != 1 {
		t.Errorf("moved without UIDPLUS: %v", got)
	}
}

func TestProbeAndBadLogin(t *testing.T) {
	s := imaptest.New(t)
	s.Put("Deleted Messages", 42)
	p, err := remoteFor(s, nil).Probe([]string{"Deleted Messages", "Nope"})
	if err != nil || !p.Move || p.Validity["Deleted Messages"] != 42 || p.Validity["Nope"] != 0 {
		t.Fatalf("probe = %+v %v", p, err)
	}
	r := remoteFor(s, nil)
	r.PassCmd = []string{"echo", "wrong"}
	if _, err := r.Probe(nil); err == nil || !strings.Contains(err.Error(), "check user / pass_cmd") {
		t.Errorf("bad login: %v", err)
	}
	for _, c := range s.Commands {
		if strings.Contains(c, s.Pass) {
			t.Fatal("password leaked into a recorded command")
		}
	}
}
