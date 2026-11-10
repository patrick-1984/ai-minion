package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkerAdministrationOpsAreDeclaredWithTheirActions(t *testing.T) {
	want := map[string]struct {
		actions string
		write   bool
	}{
		OpWorkerReassign: {"manage", true}, // admins only
		OpWorkerRestrict: {"operate", true},
		OpWorkerRename:   {"operate", true},
		OpTrustSet:       {"read", true},
		OpTrustList:      {"read", false},
	}
	for name, w := range want {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != w.actions || op.Write != w.write {
			t.Errorf("%s = scope %s actions %s write %v", name, op.Scope, op.Actions, op.Write)
		}
		for _, g := range op.Guards {
			if g.Class != GuardKeep {
				t.Errorf("%s: guard %s is %s; none of these may be overridden by force", name, g.Name, g.Class)
			}
		}
	}
}

func TestWorkerAdministrationArgumentValidation(t *testing.T) {
	wa := WriteArgs{RequestID: NewUUIDv7()}
	id, other := NewUUIDv7(), NewUUIDv7()
	tests := []struct {
		name    string
		v       Validator
		wantErr bool
	}{
		{"reassign ok", WorkerReassignArgs{wa, id, other}, false},
		{"reassign no request id", WorkerReassignArgs{WriteArgs{}, id, other}, true},
		{"reassign bad worker", WorkerReassignArgs{wa, "x", other}, true},
		{"reassign bad user", WorkerReassignArgs{wa, id, ""}, true},
		{"restrict on", WorkerRestrictArgs{wa, id, true}, false},
		{"restrict off", WorkerRestrictArgs{wa, id, false}, false},
		{"restrict bad worker", WorkerRestrictArgs{wa, "x", true}, true},
		{"rename ok", WorkerRenameArgs{wa, id, "pi-1"}, false},
		{"rename empty", WorkerRenameArgs{wa, id, ""}, true},
		{"rename colon", WorkerRenameArgs{wa, id, "a:b"}, true},
		{"rename space", WorkerRenameArgs{wa, id, "my pi"}, true},
		{"rename control", WorkerRenameArgs{wa, id, "a\x00b"}, true},
		{"rename too long", WorkerRenameArgs{wa, id, strings.Repeat("x", 65)}, true},
		{"trust whole worker", TrustSetArgs{wa, id, "", false}, false},
		{"trust session", TrustSetArgs{wa, id, "minion-1", false}, false},
		{"trust remove", TrustSetArgs{wa, id, "minion-1", true}, false},
		{"trust bad session", TrustSetArgs{wa, id, "a:b", false}, true},
		{"trust bad worker", TrustSetArgs{wa, "", "", false}, true},
	}
	for _, tc := range tests {
		err := tc.v.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if err != nil && !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: error does not match ErrInvalidArgument: %v", tc.name, err)
		}
	}
}

func TestTrustListResultEncodesAnEmptyArray(t *testing.T) {
	b, err := json.Marshal(TrustListResult{})
	if err != nil || string(b) != `{"trust":[]}` {
		t.Fatalf("empty result = %s, %v", b, err)
	}
	b, err = json.Marshal(TrustListResult{Trust: []TrustInfo{{WorkerID: "w"}}})
	if err != nil || !strings.Contains(string(b), `"session_name":""`) || !strings.Contains(string(b), `"created_at":"0001-01-01T00:00:00.000Z"`) {
		t.Fatalf("result = %s, %v", b, err)
	}
}
