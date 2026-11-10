package ops

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestOutcomeVocabulary(t *testing.T) {
	for _, o := range []Outcome{OutcomeDelivered, OutcomeFailedNotSent, OutcomePartial, OutcomeUnknown} {
		if !o.Valid() {
			t.Errorf("%q should be valid", o)
		}
	}
	for _, o := range []Outcome{"", "sent", "failed", "DELIVERED"} {
		if o.Valid() {
			t.Errorf("%q should be invalid", o)
		}
	}
}

func TestFailureOutcomeNeverFailedNotSentAfterSend(t *testing.T) {
	if got := FailureOutcome(true); got != OutcomePartial {
		t.Errorf("FailureOutcome(true) = %s, want partial", got)
	}
	if got := FailureOutcome(false); got != OutcomeFailedNotSent {
		t.Errorf("FailureOutcome(false) = %s, want failed_not_sent", got)
	}
}

func TestGuardClasses(t *testing.T) {
	tests := []struct {
		class       GuardClass
		valid       bool
		overridable bool
		blocks      bool
	}{
		{GuardKeep, true, false, true},
		{GuardWarn, true, false, false},
		{GuardForce, true, true, true},
		{"REMOVE", false, false, false}, // REMOVE guards must not exist
		{"", false, false, false},
	}
	for _, tc := range tests {
		if tc.class.Valid() != tc.valid || tc.class.Overridable() != tc.overridable || tc.class.Blocks() != tc.blocks {
			t.Errorf("class %q: valid=%v overridable=%v blocks=%v", tc.class, tc.class.Valid(), tc.class.Overridable(), tc.class.Blocks())
		}
	}
}

func TestWriteResultValidate(t *testing.T) {
	tests := []struct {
		name    string
		r       WriteResult
		wantErr bool
	}{
		{"delivered", WriteResult{Outcome: OutcomeDelivered}, false},
		{"delivered forced", WriteResult{Outcome: OutcomeDelivered, Forced: true}, false},
		{"force refusal", Refused("busy", nil), false},
		{"pane changed keep refusal", WriteResult{Outcome: OutcomeFailedNotSent, Reason: "pane_changed"}, false},
		{"partial", WriteResult{Outcome: OutcomePartial, Reason: "pane_changed"}, false},
		{"unknown", WriteResult{Outcome: OutcomeUnknown, Reason: "worker_offline"}, false},
		{"bad outcome", WriteResult{Outcome: "sent"}, true},
		{"forceable but delivered", WriteResult{Outcome: OutcomeDelivered, Forceable: true}, true},
		{"forceable after partial", WriteResult{Outcome: OutcomePartial, Reason: "x_y", Forceable: true}, true},
		{"forceable and forced", WriteResult{Outcome: OutcomeFailedNotSent, Reason: "busy", Forceable: true, Forced: true}, true},
		{"failure without reason", WriteResult{Outcome: OutcomeFailedNotSent}, true},
		{"bad warning code", WriteResult{Outcome: OutcomeDelivered, Warnings: []Warning{{Code: "Not Snake"}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error %v does not match ErrInvalidArgument", err)
			}
		})
	}
}

func TestWriteResultJSONShape(t *testing.T) {
	// Warnings is always an array, the pane is null when unresolved.
	b, err := json.Marshal(WriteResult{Outcome: OutcomeDelivered})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"outcome":"delivered","reason":"","forceable":false,"forced":false,"warnings":[],"pane":null}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}

	full := Refused("shell_pane", &PaneInfo{Session: "minion-1", ID: "%3", Kind: PaneKindShell, State: PaneStateStable},
		Warning{Code: "readiness_unknown", Message: "pane state unknown"})
	b, err = json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var back WriteResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Outcome != OutcomeFailedNotSent || !back.Forceable || back.Reason != "shell_pane" ||
		len(back.Warnings) != 1 || back.Pane == nil || back.Pane.ID != "%3" {
		t.Fatalf("round trip lost data: %+v", back)
	}
}

func TestUUIDv7(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_123)
	id := newUUIDv7(now)
	if !ValidUUIDv7(id) {
		t.Fatalf("generated id %q is not a valid UUIDv7", id)
	}
	// The first 48 bits are the unix milliseconds.
	wantPrefix := func() string {
		ms := uint64(now.UnixMilli())
		const h = "0123456789abcdef"
		out := make([]byte, 0, 12)
		for shift := 44; shift >= 0; shift -= 4 {
			out = append(out, h[(ms>>uint(shift))&0xf])
		}
		return string(out)
	}()
	if got := id[:8] + id[9:13]; got != wantPrefix {
		t.Errorf("timestamp prefix = %s, want %s", got, wantPrefix)
	}
	if a, b := NewUUIDv7(), NewUUIDv7(); a == b {
		t.Error("two ids are equal")
	}
	for _, bad := range []string{
		"", "x", id[:35], id + "0",
		"018bcfe5-6e3b-4000-8000-000000000000", // version 4
		"018bcfe5-6e3b-7000-c000-000000000000", // variant 11
		"018BCFE5-6E3B-7000-8000-000000000000", // uppercase
		"018bcfe5_6e3b-7000-8000-000000000000",
		"018bcfe5-6e3b-7000-8000-00000000000g",
	} {
		if ValidUUIDv7(bad) {
			t.Errorf("ValidUUIDv7(%q) = true", bad)
		}
	}
}

func TestWriteArgsValidate(t *testing.T) {
	if err := (WriteArgs{RequestID: NewUUIDv7()}).Validate(); err != nil {
		t.Errorf("valid request id rejected: %v", err)
	}
	err := (WriteArgs{RequestID: "not-a-uuid"}).Validate()
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
	var ae *ArgumentError
	if !errors.As(err, &ae) || ae.Field != "request_id" {
		t.Errorf("argument error = %#v", ae)
	}
}

func TestTimestampJSON(t *testing.T) {
	ts := Timestamp(time.Date(2026, 11, 9, 12, 30, 45, 123_456_789, time.FixedZone("x", 3600)))
	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-11-09T11:30:45.123Z"` {
		t.Fatalf("got %s", b)
	}
	var back Timestamp
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Time().Equal(time.Date(2026, 11, 9, 11, 30, 45, 123_000_000, time.UTC)) {
		t.Fatalf("round trip = %v", back.Time())
	}
	for _, bad := range []string{`"2026-11-09T11:30:45Z"`, `"2026-11-09T11:30:45.123+01:00"`, `"nope"`} {
		if err := json.Unmarshal([]byte(bad), &back); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
