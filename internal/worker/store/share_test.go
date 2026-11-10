package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestShareDefaultIsLaterAndUnconfigured(t *testing.T) {
	s := openStore(t, t.TempDir())
	sel, err := s.LoadShare()
	if err != nil {
		t.Fatal(err)
	}
	if sel.Mode != ShareLater || sel.Configured || len(sel.Sessions) != 0 {
		t.Fatalf("default = %+v", sel)
	}
	ok, err := s.IsShared("minion-1")
	if err != nil || ok {
		t.Fatalf("IsShared on unconfigured store = %v, %v; want false", ok, err)
	}
}

func TestShareRoundTrip(t *testing.T) {
	cases := []ShareSelection{
		{Mode: ShareAll},
		{Mode: ShareNone},
		{Mode: ShareLater},
		{Mode: ShareSelect, Sessions: []string{"work", "minion-2", "alpha"}},
		{Mode: ShareSelect},
		{Mode: ShareNone, Sessions: []string{"kept"}}, // list kept for other modes
	}
	for _, in := range cases {
		s := openStore(t, t.TempDir())
		if err := s.SaveShare(in); err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
		got, err := s.LoadShare()
		if err != nil {
			t.Fatal(err)
		}
		want := in
		want.Configured = true
		if want.Sessions == nil {
			want.Sessions = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %+v = %+v, want %+v", in, got, want)
		}
	}
}

func TestShareSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := ShareSelection{Mode: ShareSelect, Sessions: []string{"b", "a"}}
	if err := s.SaveShare(in); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openStore(t, dir)
	got, err := s.LoadShare()
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ShareSelect || !reflect.DeepEqual(got.Sessions, []string{"b", "a"}) || !got.Configured {
		t.Fatalf("after reopen = %+v", got)
	}
}

func TestShareSaveReplacesPreviousSelection(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.SaveShare(ShareSelection{Mode: ShareSelect, Sessions: []string{"a", "b", "c"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveShare(ShareSelection{Mode: ShareSelect, Sessions: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadShare()
	if !reflect.DeepEqual(got.Sessions, []string{"c"}) {
		t.Fatalf("Sessions = %v, want [c]", got.Sessions)
	}
}

func TestShareDropsDuplicateNames(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.SaveShare(ShareSelection{Mode: ShareSelect, Sessions: []string{"a", "b", "a"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadShare()
	if !reflect.DeepEqual(got.Sessions, []string{"a", "b"}) {
		t.Fatalf("Sessions = %v, want [a b]", got.Sessions)
	}
}

func TestIsSharedSemantics(t *testing.T) {
	cases := []struct {
		name string
		sel  ShareSelection
		want map[string]bool
	}{
		{"all", ShareSelection{Mode: ShareAll},
			map[string]bool{"a": true, "anything": true, "minion-99": true}},
		{"all ignores list", ShareSelection{Mode: ShareAll, Sessions: []string{"a"}},
			map[string]bool{"a": true, "b": true}},
		{"none", ShareSelection{Mode: ShareNone, Sessions: []string{"a"}},
			map[string]bool{"a": false, "b": false}},
		{"later", ShareSelection{Mode: ShareLater, Sessions: []string{"a"}},
			map[string]bool{"a": false, "b": false}},
		{"select", ShareSelection{Mode: ShareSelect, Sessions: []string{"a", "minion-2"}},
			map[string]bool{"a": true, "minion-2": true, "b": false, "A": false, "": false}},
		{"select empty", ShareSelection{Mode: ShareSelect},
			map[string]bool{"a": false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			if err := s.SaveShare(c.sel); err != nil {
				t.Fatal(err)
			}
			for name, want := range c.want {
				got, err := s.IsShared(name)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("IsShared(%q) = %v, want %v", name, got, want)
				}
				// The pure rule agrees with the database answer.
				if rule := c.sel.Includes(name); rule != want {
					t.Errorf("Includes(%q) = %v, want %v", name, rule, want)
				}
			}
		})
	}
}

func TestShareAllCoversSessionsCreatedLater(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.SaveShare(ShareSelection{Mode: ShareAll}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NextSessionNumber()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.IsShared(SessionName(n))
	if err != nil || !ok {
		t.Fatalf("IsShared(%s) = %v, %v; want true", SessionName(n), ok, err)
	}
}

func TestShareSelectDoesNotCoverNewSessions(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.SaveShare(ShareSelection{Mode: ShareSelect, Sessions: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.NextSessionNumber()
	if ok, _ := s.IsShared(SessionName(n)); ok {
		t.Fatal("a session created after a select choice is shared")
	}
}

func TestSaveShareValidates(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.SaveShare(ShareSelection{Mode: ShareAll}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ShareSelection{
		{Mode: "everything"},
		{Mode: ""},
		{Mode: ShareSelect, Sessions: []string{"ok", ""}},
	} {
		if err := s.SaveShare(bad); !errors.Is(err, ErrInvalidShare) {
			t.Errorf("SaveShare(%+v) = %v, want ErrInvalidShare", bad, err)
		}
	}
	// A refused save leaves the previous choice untouched.
	got, _ := s.LoadShare()
	if got.Mode != ShareAll {
		t.Fatalf("mode after refused saves = %q, want all", got.Mode)
	}
}

func TestShareModeValid(t *testing.T) {
	for _, m := range []ShareMode{ShareAll, ShareNone, ShareSelect, ShareLater} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	if ShareMode("x").Valid() {
		t.Error("x should be invalid")
	}
}
