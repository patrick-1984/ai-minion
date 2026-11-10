package ops

import (
	"strings"
	"testing"
)

func TestActionSet(t *testing.T) {
	tests := []struct {
		name    string
		in      []Action
		want    string
		wantErr bool
	}{
		{"single", []Action{ActionRead}, "read", false},
		{"canonical order", []Action{ActionOperate, ActionMessage}, "message|operate", false},
		{"empty", nil, "", true},
		{"unknown", []Action{"admin"}, "", true},
		{"duplicate", []Action{ActionRead, ActionRead}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := NewActionSet(tc.in...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && set.String() != tc.want {
				t.Fatalf("String() = %q, want %q", set.String(), tc.want)
			}
		})
	}
}

func TestActionSetPermits(t *testing.T) {
	pipe := MustActionSet(ActionMessage, ActionOperate)
	tests := []struct {
		held []Action
		want bool
	}{
		{[]Action{ActionMessage}, true},
		{[]Action{ActionOperate}, true},
		{[]Action{ActionRead, ActionOperate}, true},
		{[]Action{ActionRead}, false},
		{[]Action{ActionManage}, false}, // manage does not imply operate
		{nil, false},
	}
	for _, tc := range tests {
		if got := pipe.Permits(tc.held...); got != tc.want {
			t.Errorf("Permits(%v) = %v, want %v", tc.held, got, tc.want)
		}
	}
}

func TestParseActionSetRoundTrip(t *testing.T) {
	for _, s := range []string{"read", "operate", "message|operate", "manage"} {
		set, err := ParseActionSet(s)
		if err != nil || set.String() != s {
			t.Errorf("ParseActionSet(%q) = %v, %v", s, set, err)
		}
	}
	for _, s := range []string{"", "read|", "operate|message", "read|read", "Read"} {
		// "operate|message" parses (it is canonicalised) so only reject the rest.
		set, err := ParseActionSet(s)
		if s == "operate|message" {
			if err != nil || set.String() != "message|operate" {
				t.Errorf("ParseActionSet(%q) should canonicalise, got %v, %v", s, set, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseActionSet(%q) = %v, want error", s, set)
		}
	}
}

func TestMustActionSetPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(error).Error(), "empty") {
			t.Fatalf("recover = %v, want empty-set panic", r)
		}
	}()
	MustActionSet()
}
