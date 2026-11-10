package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseHumanTarget(t *testing.T) {
	tests := []struct {
		in      string
		want    HumanTarget
		wantErr bool
	}{
		{"pi-1:minion-3", HumanTarget{"pi-1", "minion-3", ""}, false},
		{"pi-1:minion-3:0.1", HumanTarget{"pi-1", "minion-3", "0.1"}, false},
		{"pi-1:minion-3:12.34", HumanTarget{"pi-1", "minion-3", "12.34"}, false},
		{"office-pi:my session", HumanTarget{"office-pi", "my session", ""}, false}, // adopted names may hold spaces
		{"", HumanTarget{}, true},
		{"pi-1", HumanTarget{}, true},
		{"pi-1:", HumanTarget{}, true},
		{":minion-3", HumanTarget{}, true},
		{"pi-1:minion-3:", HumanTarget{}, true},
		{"pi-1:minion-3:0", HumanTarget{}, true},
		{"pi-1:minion-3:0.", HumanTarget{}, true},
		{"pi-1:minion-3:.1", HumanTarget{}, true},
		{"pi-1:minion-3:a.b", HumanTarget{}, true},
		{"pi-1:minion-3:0.1.2", HumanTarget{}, true},
		{"pi-1:minion-3:0.1:x", HumanTarget{}, true},
		{"pi 1:minion-3", HumanTarget{}, true},
		{"pi-1:minion.3", HumanTarget{}, true},
		{"pi-1: minion-3", HumanTarget{}, true},
		{"pi-1:minion-3\n", HumanTarget{}, true},
		{"pi-1:min\x1bion", HumanTarget{}, true},
		{"pi-1:\xff", HumanTarget{}, true},
		{"pi-1:" + strings.Repeat("a", 129), HumanTarget{}, true},
		{strings.Repeat("w", 65) + ":s", HumanTarget{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseHumanTarget(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				var ae *ArgumentError
				if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ae) || ae.Reason == "" {
					t.Fatalf("error %v is not invalid_argument", err)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got.String() != tc.in {
				t.Fatalf("String() = %q, want %q", got.String(), tc.in)
			}
		})
	}
}

func TestHumanTargetResolve(t *testing.T) {
	id := NewUUIDv7()
	h, err := ParseHumanTarget("pi-1:minion-3:0.1")
	if err != nil {
		t.Fatal(err)
	}
	got := h.Resolve(id)
	want := Target{WorkerID: id, SessionName: "minion-3", Pane: "0.1"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTargetValidate(t *testing.T) {
	id := NewUUIDv7()
	tests := []struct {
		name    string
		t       Target
		wantErr bool
	}{
		{"canonical", Target{WorkerID: id, SessionName: "minion-1"}, false},
		{"with pane", Target{WorkerID: id, SessionName: "minion-1", Pane: "0.1"}, false},
		{"local, no worker id", Target{SessionName: "minion-1"}, false},
		{"no session", Target{WorkerID: id}, true},
		{"bad worker id", Target{WorkerID: "pi-1", SessionName: "s"}, true},
		{"bad pane", Target{WorkerID: id, SessionName: "s", Pane: "%3"}, true},
		{"colon in session", Target{WorkerID: id, SessionName: "a:b"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.t.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("not invalid_argument: %v", err)
			}
		})
	}
}

func TestTargetJSON(t *testing.T) {
	id := NewUUIDv7()
	b, err := json.Marshal(Target{WorkerID: id, SessionName: "minion-1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"worker_id":"` + id + `","session_name":"minion-1"}`; string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
	b, _ = json.Marshal(Target{WorkerID: id, SessionName: "minion-1", Pane: "0.1"})
	var back Target
	if err := json.Unmarshal(b, &back); err != nil || back.Pane != "0.1" || back.SessionName != "minion-1" {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
	if got := back.String(); got != id+":minion-1:0.1" {
		t.Fatalf("String() = %q", got)
	}
}
