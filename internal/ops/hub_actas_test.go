package ops

import (
	"errors"
	"testing"
)

func TestActAsOpsAreDeclared(t *testing.T) {
	start, ok := Lookup(OpActAsStart)
	if !ok {
		t.Fatal("act_as_start not registered")
	}
	stop, ok := Lookup(OpActAsStop)
	if !ok {
		t.Fatal("act_as_stop not registered")
	}
	for _, op := range []Op{start, stop} {
		if op.Scope != ScopeHub || !op.Write {
			t.Errorf("%s: scope %s write %v, want a Hub write op", op.Name, op.Scope, op.Write)
		}
		if got := op.RESTMethods(); len(got) != 1 || got[0] != "POST" {
			t.Errorf("%s REST methods = %v, want POST only (a state change must not be a GET)", op.Name, got)
		}
	}
	if start.Actions.String() != "manage" {
		t.Errorf("act_as_start actions = %s, want manage (admin only)", start.Actions)
	}
	if stop.Actions.String() != "operate" {
		t.Errorf("act_as_stop actions = %s, want operate", stop.Actions)
	}
}

func TestActAsStartArgsValidate(t *testing.T) {
	id, user := NewUUIDv7(), NewUUIDv7()
	tests := []struct {
		name string
		args ActAsStartArgs
		ok   bool
	}{
		{"default duration", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: user}, true},
		{"one minute", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: user, DurationMinutes: 1}, true},
		{"maximum", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: user, DurationMinutes: MaxActAsMinutes}, true},
		{"over the maximum", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: user, DurationMinutes: MaxActAsMinutes + 1}, false},
		{"negative", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: user, DurationMinutes: -1}, false},
		{"no request id", ActAsStartArgs{UserID: user}, false},
		{"no user", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}}, false},
		{"user is not a uuid", ActAsStartArgs{WriteArgs: WriteArgs{RequestID: id}, UserID: "alice"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.Validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want invalid argument", err)
			}
		})
	}
}

func TestActAsStartEffectiveMinutes(t *testing.T) {
	if got := (ActAsStartArgs{}).EffectiveMinutes(); got != 60 {
		t.Errorf("default = %d minutes, want 60 (one hour)", got)
	}
	if got := (ActAsStartArgs{DurationMinutes: 90}).EffectiveMinutes(); got != 90 {
		t.Errorf("explicit = %d, want 90", got)
	}
	if MaxActAsMinutes != 480 {
		t.Errorf("max = %d minutes, want 480 (eight hours)", MaxActAsMinutes)
	}
}

func TestActAsStopArgsRequestIDIsOptional(t *testing.T) {
	if err := (ActAsStopArgs{}).Validate(); err != nil {
		t.Errorf("empty arguments (the banner's {}) rejected: %v", err)
	}
	if err := (ActAsStopArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}}).Validate(); err != nil {
		t.Errorf("valid request id rejected: %v", err)
	}
	if err := (ActAsStopArgs{WriteArgs: WriteArgs{RequestID: "nope"}}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("malformed request id: error = %v, want invalid argument", err)
	}
}
