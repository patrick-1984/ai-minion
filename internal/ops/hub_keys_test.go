package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyOpsAreDeclared(t *testing.T) {
	want := map[string]struct {
		actions string
		write   bool
	}{
		OpAPIKeyCreate: {"operate", false},
		OpAPIKeyList:   {"read", false},
		OpAPIKeyRevoke: {"operate", true},
	}
	for name, w := range want {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != w.actions || op.Write != w.write {
			t.Errorf("%s: scope=%s actions=%s write=%v, want hub %s write=%v", name, op.Scope, op.Actions, op.Write, w.actions, w.write)
		}
	}
	// A revoke must never be reachable by GET (a link must not revoke keys).
	op, _ := Lookup(OpAPIKeyRevoke)
	if got := op.RESTMethods(); len(got) != 1 || got[0] != "POST" {
		t.Errorf("apikey_revoke REST methods = %v", got)
	}
}

func TestKeyResourceValidate(t *testing.T) {
	u, w := NewUUIDv7(), NewUUIDv7()
	tests := []struct {
		name string
		r    KeyResource
		ok   bool
	}{
		{"everything", KeyResource{Kind: KeyResourceEverything}, true},
		{"user", KeyResource{Kind: KeyResourceUser, UserID: u}, true},
		{"worker", KeyResource{Kind: KeyResourceWorker, WorkerID: w}, true},
		{"session", KeyResource{Kind: KeyResourceSession, WorkerID: w, SessionName: "minion-1"}, true},
		{"unknown kind", KeyResource{Kind: "team"}, false},
		{"empty kind", KeyResource{}, false},
		{"everything with a user", KeyResource{Kind: KeyResourceEverything, UserID: u}, false},
		{"user without id", KeyResource{Kind: KeyResourceUser}, false},
		{"user with a worker", KeyResource{Kind: KeyResourceUser, UserID: u, WorkerID: w}, false},
		{"user id not uuid", KeyResource{Kind: KeyResourceUser, UserID: "alice"}, false},
		{"worker without id", KeyResource{Kind: KeyResourceWorker}, false},
		{"worker with a session", KeyResource{Kind: KeyResourceWorker, WorkerID: w, SessionName: "s"}, false},
		{"worker id not uuid", KeyResource{Kind: KeyResourceWorker, WorkerID: "w1"}, false},
		{"session without name", KeyResource{Kind: KeyResourceSession, WorkerID: w}, false},
		{"session without worker", KeyResource{Kind: KeyResourceSession, SessionName: "s"}, false},
		{"session name with colon", KeyResource{Kind: KeyResourceSession, WorkerID: w, SessionName: "a:b"}, false},
		{"session name with control", KeyResource{Kind: KeyResourceSession, WorkerID: w, SessionName: "a\nb"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want invalid_argument", err)
			}
		})
	}
}

func goodKeyArgs() APIKeyCreateArgs {
	return APIKeyCreateArgs{
		Name:      "ci bot",
		Resources: []KeyResource{{Kind: KeyResourceWorker, WorkerID: NewUUIDv7()}},
		Actions:   []Action{ActionRead},
	}
}

func TestAPIKeyCreateArgsValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*APIKeyCreateArgs)
		field  string // empty: valid
	}{
		{"valid", func(*APIKeyCreateArgs) {}, ""},
		{"valid with expiry and binding", func(a *APIKeyCreateArgs) {
			ts := Timestamp(time.Now().Add(time.Hour))
			a.ExpiresAt, a.BoundWorkerID = &ts, NewUUIDv7()
		}, ""},
		{"everything alone", func(a *APIKeyCreateArgs) { a.Resources = []KeyResource{{Kind: KeyResourceEverything}} }, ""},
		{"all actions", func(a *APIKeyCreateArgs) {
			a.Actions = []Action{ActionRead, ActionOperate, ActionMessage, ActionManage}
		}, ""},
		{"no name", func(a *APIKeyCreateArgs) { a.Name = "" }, "name"},
		{"long name", func(a *APIKeyCreateArgs) { a.Name = strings.Repeat("a", MaxAPIKeyNameLen+1) }, "name"},
		{"padded name", func(a *APIKeyCreateArgs) { a.Name = " bot" }, "name"},
		{"control in name", func(a *APIKeyCreateArgs) { a.Name = "a\x00b" }, "name"},
		{"bad utf8 name", func(a *APIKeyCreateArgs) { a.Name = "a\xffb" }, "name"},
		{"no resources", func(a *APIKeyCreateArgs) { a.Resources = nil }, "resources"},
		{"too many resources", func(a *APIKeyCreateArgs) {
			a.Resources = make([]KeyResource, MaxKeyResources+1)
			for i := range a.Resources {
				a.Resources[i] = KeyResource{Kind: KeyResourceWorker, WorkerID: NewUUIDv7()}
			}
		}, "resources"},
		{"bad resource", func(a *APIKeyCreateArgs) { a.Resources = []KeyResource{{Kind: "x"}} }, "resources"},
		{"everything with others", func(a *APIKeyCreateArgs) {
			a.Resources = append(a.Resources, KeyResource{Kind: KeyResourceEverything})
		}, "resources"},
		{"no actions", func(a *APIKeyCreateArgs) { a.Actions = nil }, "actions"},
		{"unknown action", func(a *APIKeyCreateArgs) { a.Actions = []Action{"admin"} }, "actions"},
		{"too many actions", func(a *APIKeyCreateArgs) {
			a.Actions = []Action{ActionRead, ActionRead, ActionRead, ActionRead, ActionRead}
		}, "actions"},
		{"bound id not uuid", func(a *APIKeyCreateArgs) { a.BoundWorkerID = "w1" }, "bound_worker_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := goodKeyArgs()
			tc.mutate(&a)
			err := a.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ae *ArgumentError
			if !errors.As(err, &ae) || ae.Field != tc.field {
				t.Fatalf("err = %v, want invalid_argument on %s", err, tc.field)
			}
		})
	}
}

func TestAPIKeyCreateArgsJSONRoundTrip(t *testing.T) {
	in := `{"name":"n","expires_at":"2030-01-02T03:04:05.000Z","resources":[{"kind":"session","worker_id":"` + NewUUIDv7() +
		`","session_name":"minion-1"}],"actions":["read","message"],"bound_worker_id":"` + NewUUIDv7() + `","added_later":1}`
	var a APIKeyCreateArgs
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.ExpiresAt == nil || a.ExpiresAt.Time().Year() != 2030 || a.Actions[1] != ActionMessage {
		t.Fatalf("decoded %+v", a)
	}
}

func TestAPIKeyListAndRevokeArgsValidate(t *testing.T) {
	id := NewUUIDv7()
	if err := (APIKeyListArgs{}).Validate(); err != nil {
		t.Errorf("empty list args: %v", err)
	}
	if err := (APIKeyListArgs{OwnerID: id}).Validate(); err != nil {
		t.Errorf("owner filter: %v", err)
	}
	if err := (APIKeyListArgs{All: true}).Validate(); err != nil {
		t.Errorf("all: %v", err)
	}
	for _, a := range []APIKeyListArgs{{OwnerID: "bob"}, {OwnerID: id, All: true}} {
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: err = %v", a, err)
		}
	}
	if err := (APIKeyRevokeArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, ID: id}).Validate(); err != nil {
		t.Errorf("revoke: %v", err)
	}
	for _, a := range []APIKeyRevokeArgs{
		{ID: id},
		{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, ID: "key1"},
		{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}},
	} {
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: err = %v", a, err)
		}
	}
}

func TestAPIKeyListResultMarshalsAnEmptyArray(t *testing.T) {
	b, err := json.Marshal(APIKeyListResult{})
	if err != nil || string(b) != `{"keys":[]}` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
}

func TestAPIKeyCreateResultNeverPrintsTheKey(t *testing.T) {
	r := APIKeyCreateResult{Key: "mn_secretsecretsecret", Info: APIKeyInfo{ID: "id-1"}}
	//nolint:staticcheck // checks that the %s verb redacts the key too
	formatted := []string{fmt.Sprint(r), fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%s", r), fmt.Sprintf("%#v", r)}
	for _, s := range formatted {
		if strings.Contains(s, "secret") {
			t.Errorf("formatted result leaks the key: %s", s)
		}
	}
	// The wire form carries it, once.
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"key":"mn_secretsecretsecret"`) {
		t.Errorf("wire form lacks the key: %s", b)
	}
}
