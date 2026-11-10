package api

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	hubOp    = ops.Op{Name: "hub_probe", Scope: ops.ScopeHub}
	workerOp = ops.Op{Name: "worker_probe", Scope: ops.ScopeWorker}
)

func auditEvent(target string) audit.Event {
	return audit.Event{Actor: "a", Action: "x", Target: target, Outcome: AuditOutcomeDenied}
}

// optTarget is the shape of args with a *ops.Target field.
type optTarget struct {
	Target *ops.Target `json:"target,omitempty"`
}

func TestDefaultResource(t *testing.T) {
	p := authz.NewSessionPrincipal("u1")
	w1, w2, w3, w4, w5, w6 := ops.NewUUIDv7(), ops.NewUUIDv7(), ops.NewUUIDv7(), ops.NewUUIDv7(), ops.NewUUIDv7(), ops.NewUUIDv7()
	cases := []struct {
		name string
		op   ops.Op
		args any
		want authz.Resource
	}{
		{"hub op without a target is the caller's user resource", hubOp, &pingArgs{}, authz.UserResource{ID: "u1"}},
		{"target with a session", workerOp, &ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: w1, SessionName: "minion-1"}},
			authz.SessionResource{WorkerID: w1, Name: "minion-1"}},
		{"write args with embedded WriteArgs and a target", workerOp, &ops.SendTextArgs{Target: ops.Target{WorkerID: w2, SessionName: "s"}},
			authz.SessionResource{WorkerID: w2, Name: "s"}},
		{"worker_id alone", workerOp, &shareArgs{WorkerID: w3}, authz.WorkerResource{ID: w3}},
		{"worker_id in an embedding struct", workerOp, &shareSetArgs{WorkerID: w4}, authz.WorkerResource{ID: w4}},
		{"a target without a session is the Worker", workerOp, &ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: w5}},
			authz.WorkerResource{ID: w5}},
		{"a *Target field is a target", workerOp, &optTarget{Target: &ops.Target{WorkerID: w6, SessionName: "s"}},
			authz.SessionResource{WorkerID: w6, Name: "s"}},
	}
	for _, c := range cases {
		got, err := DefaultResource(p, c.op, c.args)
		if err != nil || got != c.want {
			t.Errorf("%s: got %#v, %v; want %#v", c.name, got, err, c.want)
		}
	}
}

func TestDefaultResourceFailsClosedForAWorkerOpWithoutAWorker(t *testing.T) {
	p := authz.NewSessionPrincipal("u1")
	for name, args := range map[string]any{
		"sessions_list shape (no args)":    &ops.SessionsListArgs{},
		"session_create shape (no target)": &ops.SessionCreateArgs{},
		"nil *Target":                      &optTarget{},
		"no struct at all":                 nil,
	} {
		res, err := DefaultResource(p, workerOp, args)
		e := ErrorOf(err)
		if res != nil || e.Code != proto.CodeInvalidArgument || e.Details["field"] != "worker_id" {
			t.Errorf("%s: got %#v, %v", name, res, err)
		}
	}
	// A Hub op keeps the user-resource fallback, also with a nil *Target.
	if res, err := DefaultResource(p, hubOp, &optTarget{}); err != nil || res != (authz.UserResource{ID: "u1"}) {
		t.Errorf("hub op: %#v, %v", res, err)
	}
	// The request's worker_id routes a Worker op whose arguments have none.
	wid := ops.NewUUIDv7()
	if res, err := defaultResource(p, workerOp, &ops.SessionsListArgs{}, wid); err != nil || res != (authz.WorkerResource{ID: wid}) {
		t.Errorf("request worker: %#v, %v", res, err)
	}
	if _, err := defaultResource(p, workerOp, &ops.SessionsListArgs{}, "w1"); ErrorOf(err).Code != proto.CodeInvalidArgument {
		t.Errorf("request worker that is no UUID: %v", err)
	}
}

func TestDefaultResourceNeedsAValidWorker(t *testing.T) {
	p := authz.NewSessionPrincipal("u1")
	for name, args := range map[string]any{
		"session without a worker": &ops.SessionSnapshotArgs{Target: ops.Target{SessionName: "s"}},
		"empty worker_id":          &shareArgs{},
		"not a UUID":               &shareArgs{WorkerID: "w1"},
		"300 characters":           &shareArgs{WorkerID: strings.Repeat("w", 300)},
	} {
		_, err := DefaultResource(p, workerOp, args)
		e := ErrorOf(err)
		if e.Code != proto.CodeInvalidArgument || e.Details["field"] != "worker_id" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDefaultResourceWithoutAUserFailsClosed(t *testing.T) {
	_, err := DefaultResource(authz.Principal{}, hubOp, &pingArgs{})
	if ErrorOf(err).Code != proto.CodeForbidden {
		t.Errorf("err = %v", err)
	}
}

func TestResourceLabel(t *testing.T) {
	cases := map[string]authz.Resource{
		"u":    authz.UserResource{ID: "u"},
		"w":    authz.WorkerResource{ID: "w"},
		"w:s1": authz.SessionResource{WorkerID: "w", Name: "s1"},
	}
	for want, r := range cases {
		if got := ResourceLabel(r); got != want {
			t.Errorf("label = %q, want %q", got, want)
		}
	}
}

func TestResourceLabelIsAlwaysAValidAuditTarget(t *testing.T) {
	long := strings.Repeat("\u00e9", 300) // multi-byte: the cut is by characters
	for name, r := range map[string]authz.Resource{
		"long worker":    authz.WorkerResource{ID: long},
		"long session":   authz.SessionResource{WorkerID: "w", Name: long},
		"invalid UTF-8":  authz.SessionResource{WorkerID: "w", Name: "a\xffb"},
		"control chars":  authz.SessionResource{WorkerID: "w", Name: "a\nb\x00c"},
		"cut mid-letter": authz.UserResource{ID: strings.Repeat("x", 255) + "\u00e9\u20ac"},
	} {
		got := ResourceLabel(r)
		if utf8.RuneCountInString(got) > ops.MaxAuditFieldLen || !utf8.ValidString(got) {
			t.Errorf("%s: label is %d characters, valid=%v", name, utf8.RuneCountInString(got), utf8.ValidString(got))
		}
		if err := (auditEvent(got)).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
