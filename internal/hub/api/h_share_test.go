package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type fakeFence struct {
	mu    sync.Mutex
	calls []struct {
		worker string
		names  []string
	}
	begun []*fakeShareCall
	// confirms counts the probes the handler asked for (ConfirmShare).
	confirms int
}

// ConfirmShare records the Hub's follow-up after a share_set of unknown outcome:
// it is an internal probe on the Worker's connection, not a routed share_get.
func (f *fakeFence) ConfirmShare(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms++
	return nil
}

func (f *fakeFence) ApplyShare(worker string, names []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		worker string
		names  []string
	}{worker, names})
}

// fakeShareCall records how the handler settled one call.
type fakeShareCall struct {
	f       *fakeFence
	worker  string
	next    *ops.ShareRule
	outcome string
	result  ops.ShareResult
}

// BeginShare records the call; Applied tells the view the unshared names as the
// earlier, name-only contract did, so the assertions about what the Hub's view
// was told keep their meaning.
func (f *fakeFence) BeginShare(worker string, next *ops.ShareRule) workers.ShareCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeShareCall{f: f, worker: worker, next: next}
	f.begun = append(f.begun, c)
	return c
}

func (c *fakeShareCall) settle(outcome string) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.outcome = outcome
}
func (c *fakeShareCall) Err() error  { return nil }
func (c *fakeShareCall) Seq() uint64 { return 0 }
func (c *fakeShareCall) Refused()    { c.settle("refused") }
func (c *fakeShareCall) Unknown()    { c.settle("unknown") }
func (c *fakeShareCall) Applied(res ops.ShareResult) {
	c.settle("applied")
	c.f.mu.Lock()
	c.result = res
	c.f.mu.Unlock()
	c.f.ApplyShare(c.worker, res.Unshared)
}

// outcomes returns how each call that asked for a restriction (share_set) was settled.
func (f *fakeFence) outcomes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.begun {
		if c.next != nil {
			out = append(out, c.outcome)
		}
	}
	return out
}

const shareTestWorker = "01900000-0000-7000-8000-0000000000aa"

type shareRig struct {
	*rig
	rt    *woRouter
	fence *fakeFence
}

func newShareRig(t *testing.T) *shareRig {
	t.Helper()
	r := &rig{authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(ops.Default), mux: http.NewServeMux(), logs: &strings.Builder{}}
	r.d = &Dispatcher{Catalog: ops.Default, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	s := &shareRig{rig: r, rt: &woRouter{}, fence: &fakeFence{}}
	mustRegister(t, RegisterShareOps(r.reg, s.rt, s.fence))
	return s
}

func (s *shareRig) post(op, body string) *httptest.ResponseRecorder {
	return s.do("POST", "/api/v1/ops/"+op, body)
}

// Both share ops are decided as operate on the Worker named by worker_id, routed
// to that Worker, and the Hub's view follows the answer before it is returned;
// the answer is the Worker's rule and names, sorted, and nothing else.
func TestShareOpsAreDecidedOnTheWorkerRoutedAndFollowTheAnswer(t *testing.T) {
	s := newShareRig(t)
	s.rt.callRes = ops.ShareResult{Rule: ops.ShareRule{Mode: "select", Names: []string{"b", "a"}}, Unshared: []string{"z", "y"}}

	w := s.post(ops.OpShareSet, `{"worker_id":"`+shareTestWorker+`","mode":"select","names":["a","b"]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unshared":["y","z"]`) || !strings.Contains(w.Body.String(), `"names":["a","b"]`) {
		t.Fatalf("share_set = %d %s", w.Code, w.Body)
	}
	req := s.rt.last(t)
	if req.Op != ops.OpShareSet || req.WorkerID != shareTestWorker || req.Session != "" {
		t.Fatalf("routed %+v", req)
	}
	if len(s.fence.calls) != 1 || s.fence.calls[0].worker != shareTestWorker || strings.Join(s.fence.calls[0].names, ",") != "y,z" {
		t.Fatalf("the Hub's view was told %+v", s.fence.calls)
	}
	last := s.decide.calls[len(s.decide.calls)-1]
	if !last.accepted.Permits(ops.ActionOperate) || last.accepted.Permits(ops.ActionManage) || last.accepted.Permits(ops.ActionRead) {
		t.Fatalf("accepted %v, want exactly operate", last.accepted)
	}
	if res, ok := last.res.(authz.WorkerResource); !ok || res.ID != shareTestWorker {
		t.Fatalf("decided on %#v", last.res)
	}

	s.rt.callRes = ops.ShareResult{Rule: ops.ShareRule{Mode: "all"}}
	if w := s.post(ops.OpShareGet, `{"worker_id":"`+shareTestWorker+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"unshared":[]`) {
		t.Fatalf("share_get = %d %s", w.Code, w.Body)
	}
	if n := len(s.fence.calls); n != 2 || len(s.fence.calls[1].names) != 0 {
		t.Fatalf("share_get did not bring the Hub's view up to date: %+v", s.fence.calls)
	}
	// share_set is POST only; share_get may be read with GET (it does not change the Worker).
	if w := s.do("GET", "/api/v1/ops/"+ops.OpShareSet+"?worker_id="+shareTestWorker+"&mode=none", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET share_set = %d", w.Code)
	}
	if w := s.do("GET", "/api/v1/ops/"+ops.OpShareGet+"?worker_id="+shareTestWorker, ""); w.Code != 200 {
		t.Fatalf("GET share_get = %d %s", w.Code, w.Body)
	}
}

// A call that definitely changed nothing (a refusal of the Worker's policy, a
// refusal of the dispatcher, an invalid argument) leaves the Hub's view alone,
// and what it had hidden for the request is given back: the outcome is
// "refused", never an applied answer. The dispatcher's refusals and invalid
// arguments never reach the Worker.
//
// This test used to put every failing call in that group, a call lost after the
// request went out included ("a lost call = no change to the view"). That was
// the assumption the privacy review found wrong: a lost or unreadable answer
// does not say that the Worker did not change its rule, and leaving the Hub's
// view as it was then leaves stored history readable that the Worker has just
// stopped sharing. The lost and the failing-after-delivery cases are
// TestShareSetWhoseOutcomeIsUnknownKeepsTheRestriction; this one keeps the
// definite refusals.
func TestShareOpsFailuresLeaveTheHubViewAlone(t *testing.T) {
	s := newShareRig(t)
	s.rt.callErr = proto.NewError(proto.CodeForbidden, "the share selection of this Worker is locked locally (share_lock)")
	w := s.post(ops.OpShareSet, `{"worker_id":"`+shareTestWorker+`","mode":"all"}`)
	if w.Code != 403 || len(s.fence.calls) != 0 {
		t.Fatalf("a Worker refusal = %d %s, fence %+v", w.Code, w.Body, s.fence.calls)
	}
	if got := s.fence.outcomes(); len(got) != 1 || got[0] != "refused" {
		t.Fatalf("a definite refusal was settled as %v, want refused", got)
	}

	calls := len(s.rt.calls)
	s.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeNotFound, "no such Worker") }
	if w := s.post(ops.OpShareSet, `{"worker_id":"`+shareTestWorker+`","mode":"all"}`); w.Code != 404 {
		t.Fatalf("a foreign Worker = %d %s", w.Code, w.Body)
	}
	s.decide.fn = nil
	for _, bad := range []string{
		`{"worker_id":"` + shareTestWorker + `","mode":"select"}`,
		`{"worker_id":"` + shareTestWorker + `","mode":"all","names":["a"]}`,
		`{"worker_id":"` + shareTestWorker + `","mode":"later"}`,
		`{"worker_id":"not-a-uuid","mode":"all"}`,
		`{"mode":"all"}`,
	} {
		if w := s.post(ops.OpShareSet, bad); w.Code != 400 {
			t.Fatalf("%s = %d %s", bad, w.Code, w.Body)
		}
	}
	if len(s.rt.calls) != calls || len(s.fence.calls) != 0 {
		t.Fatalf("a refused request reached the Worker or the view: %d calls, %+v", len(s.rt.calls)-calls, s.fence.calls)
	}
}

// A share_set that may have changed the Worker's rule but whose outcome the Hub
// does not know (the connection was lost after the request went out, the answer
// could not be read, the Worker failed in a way that does not say whether it
// changed anything) is NOT a call that changed nothing: the restriction that was
// requested stays in force (the call is settled as unknown, never as refused and
// never as applied), and the Hub asks the Worker for its rule to settle it.
func TestShareSetWhoseOutcomeIsUnknownKeepsTheRestriction(t *testing.T) {
	for name, err := range map[string]error{
		"a lost call":                  errors.New("connection lost"),
		"unknown outcome of the Hub":   &workers.OutcomeUnknownError{Err: proto.NewError(proto.CodeConnectionLost, "the connection ended")},
		"a failure of the Worker":      proto.NewError(proto.CodeInternal, "save the share selection failed"),
		"a Worker that did not answer": proto.NewError(proto.CodeConnectionLost, "no answer"),
	} {
		t.Run(name, func(t *testing.T) {
			s := newShareRig(t)
			s.rt.callErr = err
			w := s.post(ops.OpShareSet, `{"worker_id":"`+shareTestWorker+`","mode":"none"}`)
			if w.Code < 400 {
				t.Fatalf("an unknown outcome = %d %s, want an error status", w.Code, w.Body)
			}
			if got := s.fence.outcomes(); len(got) != 1 || got[0] != "unknown" {
				t.Fatalf("the call was settled as %v, want unknown (the restriction stays)", got)
			}
			if len(s.fence.calls) != 0 {
				t.Fatalf("an unknown outcome was applied as an answer: %+v", s.fence.calls)
			}
			// The Hub asks the Worker for its rule to find out, with the internal
			// probe on the Worker's connection (it raises the Worker's barrier),
			// not with a routed share_get an actor could race.
			deadline := time.Now().Add(3 * time.Second)
			for {
				s.fence.mu.Lock()
				n := s.fence.confirms
				s.fence.mu.Unlock()
				if n >= 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the Hub did not confirm the Worker's rule after an unknown outcome (%d probes)", n)
				}
				time.Sleep(5 * time.Millisecond)
			}
			s.rt.mu.Lock()
			routed := len(s.rt.calls)
			s.rt.mu.Unlock()
			if routed != 1 {
				t.Fatalf("the follow-up must not be a routed call: %d routed calls", routed)
			}
		})
	}
}

// share_set is audited like a write: success, failure and denial, with the
// Worker as target, whatever the dispatcher decided.
func TestShareSetIsAuditedIncludingDenials(t *testing.T) {
	s := newShareRig(t)
	s.rt.callRes = ops.ShareResult{Rule: ops.ShareRule{Mode: "none"}, Unshared: []string{"a"}}
	body := `{"worker_id":"` + shareTestWorker + `","mode":"none"}`
	s.post(ops.OpShareSet, body)
	s.rt.callErr = proto.NewError(proto.CodeForbidden, "locked")
	s.post(ops.OpShareSet, body)
	s.rt.callErr = nil
	s.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeForbidden, "no") }
	s.post(ops.OpShareSet, body)

	got := s.audit.all()
	if len(got) != 3 {
		t.Fatalf("audit rows = %+v", got)
	}
	for i, want := range []string{AuditOutcomeSuccess, auditOutcomeError, AuditOutcomeDenied} {
		if got[i].Action != ops.OpShareSet || got[i].Outcome != want || !strings.Contains(got[i].Target, shareTestWorker) {
			t.Fatalf("row %d = %+v, want %s on the Worker", i, got[i], want)
		}
	}
	// share_get is a read: not audited.
	s.decide.fn = nil
	s.post(ops.OpShareGet, `{"worker_id":"`+shareTestWorker+`"}`)
	if n := len(s.audit.all()); n != 3 {
		t.Fatalf("share_get was audited: %d rows", n)
	}
}

func TestRegisterShareOpsNeedsItsParts(t *testing.T) {
	if err := RegisterShareOps(NewRegistry(ops.Default), nil, &fakeFence{}); err == nil {
		t.Fatal("no router accepted")
	}
	if err := RegisterShareOps(NewRegistry(ops.Default), &woRouter{}, nil); err == nil {
		t.Fatal("no fence accepted")
	}
}
