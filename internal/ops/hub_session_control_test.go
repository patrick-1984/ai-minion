package ops

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSessionControlOpsAreAdminOnly(t *testing.T) {
	for name, write := range map[string]bool{OpSessionControlList: false, OpSessionControlSet: true} {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != "manage" || op.Write != write {
			t.Errorf("%s = scope %s actions %s write %v", name, op.Scope, op.Actions, op.Write)
		}
	}
}

func TestSessionControlSetArgsValidate(t *testing.T) {
	wa := WriteArgs{RequestID: NewUUIDv7()}
	id, other := NewUUIDv7(), NewUUIDv7()
	everything := KeyResource{Kind: KeyResourceEverything}
	worker := KeyResource{Kind: KeyResourceWorker, WorkerID: other}
	session := KeyResource{Kind: KeyResourceSession, WorkerID: other, SessionName: "build"}
	many := make([]KeyResource, MaxKeyResources+1)
	for i := range many {
		many[i] = worker
	}
	tests := []struct {
		name    string
		a       SessionControlSetArgs
		wantErr bool
	}{
		{"everything", SessionControlSetArgs{wa, id, "boss", []KeyResource{everything}}, false},
		{"workers and sessions", SessionControlSetArgs{wa, id, "boss", []KeyResource{worker, session}}, false},
		{"empty targets remove", SessionControlSetArgs{wa, id, "boss", nil}, false},
		{"no request id", SessionControlSetArgs{WriteArgs{}, id, "boss", nil}, true},
		{"bad worker", SessionControlSetArgs{wa, "x", "boss", nil}, true},
		{"no session", SessionControlSetArgs{wa, id, "", nil}, true},
		{"bad session", SessionControlSetArgs{wa, id, "a:b", nil}, true},
		{"everything combined", SessionControlSetArgs{wa, id, "boss", []KeyResource{everything, worker}}, true},
		{"user resource", SessionControlSetArgs{wa, id, "boss", []KeyResource{{Kind: KeyResourceUser, UserID: other}}}, true},
		{"unknown kind", SessionControlSetArgs{wa, id, "boss", []KeyResource{{Kind: "fleet"}}}, true},
		{"worker without id", SessionControlSetArgs{wa, id, "boss", []KeyResource{{Kind: KeyResourceWorker}}}, true},
		{"session without name", SessionControlSetArgs{wa, id, "boss", []KeyResource{{Kind: KeyResourceSession, WorkerID: other}}}, true},
		{"too many", SessionControlSetArgs{wa, id, "boss", many}, true},
	}
	for _, tc := range tests {
		err := tc.a.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		var ae *ArgumentError
		if err != nil && (!errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ae)) {
			t.Errorf("%s: err = %v, want an invalid_argument", tc.name, err)
		}
	}
}

func TestSessionControlListResultEncodesAnEmptyArray(t *testing.T) {
	b, err := json.Marshal(SessionControlListResult{})
	if err != nil || string(b) != `{"grants":[]}` {
		t.Errorf("empty result = %s err=%v", b, err)
	}
}
