package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeActAs is an ActAsController that records its calls.
type fakeActAs struct {
	startErr, stopErr error
	stopped           bool // what StopActAs reports
	starts            []fakeStart
	stops             int
}

type fakeStart struct {
	session *session.Session
	target  string
	d       time.Duration
}

func (f *fakeActAs) StartActAs(_ context.Context, s *session.Session, target string, d time.Duration) (session.ActAs, error) {
	f.starts = append(f.starts, fakeStart{s, target, d})
	return session.ActAs{TargetID: target}, f.startErr
}

func (f *fakeActAs) StopActAs(context.Context, *session.Session) (bool, error) {
	f.stops++
	return f.stopped, f.stopErr
}

type actAsRig struct {
	*rig
	ctl  *fakeActAs
	sess *session.Session
}

func newActAsRig(t *testing.T) *actAsRig {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpActAsStart, ops.OpActAsStop} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	sess := &session.Session{ID: "sess-1", UserID: "admin-1", Username: "root", Role: authz.RoleAdmin}
	r := &rig{
		authn: &fakeAuthn{id: Identity{
			Principal: sess.Principal(), Actor: "admin-1", Via: ViaSession, Session: sess,
			IP: netip.MustParseAddr("192.0.2.7"),
		}},
		decide: &fakeDecider{}, audit: &fakeAudit{}, reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	ctl := &fakeActAs{stopped: true}
	if err := RegisterActAs(r.reg, ctl); err != nil {
		t.Fatal(err)
	}
	return &actAsRig{rig: r, ctl: ctl, sess: sess}
}

func startBody(user string, minutes int) string {
	b, _ := json.Marshal(map[string]any{"request_id": ops.NewUUIDv7(), "user_id": user, "duration_minutes": minutes})
	return string(b)
}

func TestRegisterActAsNeedsAController(t *testing.T) {
	if err := RegisterActAs(NewRegistry(ops.Default), nil); err == nil {
		t.Error("a nil controller was accepted")
	}
}

func TestActAsStartDefaultsToOneHourAndIsDecidedOnTheTarget(t *testing.T) {
	r := newActAsRig(t)
	target := ops.NewUUIDv7()
	w := r.do("POST", "/api/v1/ops/act_as_start", startBody(target, 0))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(r.ctl.starts) != 1 {
		t.Fatalf("starts = %+v", r.ctl.starts)
	}
	got := r.ctl.starts[0]
	if got.session != r.sess || got.target != target || got.d != time.Hour {
		t.Errorf("start = %+v, want the session, the target and one hour", got)
	}
	c := r.decide.calls[0]
	if c.res != (authz.UserResource{ID: target}) || !c.accepted.Permits(ops.ActionManage) || c.accepted.Permits(ops.ActionOperate) {
		t.Errorf("Decide on %#v with %v, want manage on the target user", c.res, c.accepted)
	}
	ev := r.audit.all()
	if len(ev) != 1 || ev[0].Action != "act_as_start" || ev[0].Actor != "admin-1" || ev[0].Target != target || ev[0].Outcome != AuditOutcomeSuccess {
		t.Errorf("audit = %+v", ev)
	}
}

func TestActAsStartPassesTheRequestedDuration(t *testing.T) {
	r := newActAsRig(t)
	if w := r.do("POST", "/api/v1/ops/act_as_start", startBody(ops.NewUUIDv7(), 480)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.ctl.starts[0].d != 8*time.Hour {
		t.Errorf("duration = %v, want 8h", r.ctl.starts[0].d)
	}
	n := len(r.ctl.starts)
	if w := r.do("POST", "/api/v1/ops/act_as_start", startBody(ops.NewUUIDv7(), 481)); errCode(t, w) != proto.CodeInvalidArgument || len(r.ctl.starts) != n {
		t.Errorf("481 minutes: %d %s", w.Code, w.Body)
	}
}

func TestActAsStartErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		err  error
		code proto.Code
	}{
		{session.ErrActAsNotAdmin, proto.CodeForbidden},
		{session.ErrActAsSelf, proto.CodeInvalidArgument},
		{session.ErrActAsDuration, proto.CodeInvalidArgument},
		{session.ErrActAsTargetUnknown, proto.CodeNotFound},
		{session.ErrActAsTargetInactive, proto.CodeConflict},
		{session.ErrActAsActive, proto.CodeConflict},
		{session.ErrActAsUnavailable, proto.CodeInternal},
		{errors.New("boom secret detail"), proto.CodeInternal},
	}
	for _, c := range cases {
		r := newActAsRig(t)
		r.ctl.startErr = c.err
		w := r.do("POST", "/api/v1/ops/act_as_start", startBody(ops.NewUUIDv7(), 0))
		if errCode(t, w) != c.code {
			t.Errorf("%v: %d %s, want %s", c.err, w.Code, w.Body, c.code)
		}
		if strings.Contains(w.Body.String(), "secret detail") {
			t.Errorf("the cause leaked: %s", w.Body)
		}
		if ev := r.audit.all(); len(ev) != 1 || ev[0].Outcome != "failed" {
			t.Errorf("%v: audit = %+v, want one failed event", c.err, ev)
		}
	}
}

func TestActAsStartIsRefusedForNonAdminsBeforeTheHandlerRuns(t *testing.T) {
	r := newActAsRig(t)
	r.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeForbidden, "not allowed") }
	w := r.do("POST", "/api/v1/ops/act_as_start", startBody(ops.NewUUIDv7(), 0))
	if errCode(t, w) != proto.CodeForbidden || len(r.ctl.starts) != 0 {
		t.Fatalf("%d %s starts=%d", w.Code, w.Body, len(r.ctl.starts))
	}
	if ev := r.audit.all(); len(ev) != 1 || ev[0].Outcome != AuditOutcomeDenied {
		t.Errorf("audit = %+v, want one denied event", ev)
	}
}

func TestActAsRefusesAnAPIKey(t *testing.T) {
	for _, path := range []string{"act_as_start", "act_as_stop"} {
		r := newActAsRig(t)
		r.authn.id = Identity{
			Principal: authz.NewAPIKeyPrincipal("admin-1", allScope{}, ""), Actor: "key:k1", Via: ViaAPIKey,
		}
		body := startBody(ops.NewUUIDv7(), 0)
		if path == "act_as_stop" {
			body = `{}`
		}
		w := r.do("POST", "/api/v1/ops/"+path, body)
		if errCode(t, w) != proto.CodeForbidden || len(r.ctl.starts) != 0 || r.ctl.stops != 0 {
			t.Errorf("%s with a key: %d %s", path, w.Code, w.Body)
		}
	}
}

func TestActAsStopAcceptsEmptyArgumentsAndReportsAnIdleStop(t *testing.T) {
	r := newActAsRig(t)
	// The banner's Exit button posts {} while acting as user-9.
	r.authn.id.Principal = authz.NewActAsPrincipal("admin-1", "user-9", time.Now().Add(time.Hour))
	r.authn.id.Actor, r.authn.id.ActingAs = "admin-1", "user-9"
	w := r.do("POST", "/api/v1/ops/act_as_stop", `{}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Outcome != ops.OutcomeDelivered || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	c := r.decide.calls[0]
	if c.res != (authz.UserResource{ID: "user-9"}) || !c.accepted.Permits(ops.ActionOperate) || c.accepted.Permits(ops.ActionManage) {
		t.Errorf("Decide on %#v with %v, want operate on the caller's own (target) user", c.res, c.accepted)
	}
	ev := r.audit.all()
	if len(ev) != 1 || ev[0].Action != "act_as_stop" || ev[0].Actor != "admin-1" || ev[0].ActingAs != "user-9" {
		t.Errorf("audit = %+v, want actor admin-1 and acting_as user-9", ev)
	}

	r.ctl.stopped = false
	w = r.do("POST", "/api/v1/ops/act_as_stop", `{"request_id":"`+ops.NewUUIDv7()+`"}`)
	res = ops.WriteResult{}
	if err := json.Unmarshal(w.Body.Bytes(), &res); w.Code != 200 || err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != ops.WarnNotActing {
		t.Fatalf("idle stop: %d %s", w.Code, w.Body)
	}
	if w := r.do("POST", "/api/v1/ops/act_as_stop", `{"request_id":"nope"}`); errCode(t, w) != proto.CodeInvalidArgument {
		t.Errorf("malformed request id: %d %s", w.Code, w.Body)
	}
}

func TestActAsStopIsAPostOnly(t *testing.T) {
	r := newActAsRig(t)
	for _, op := range []string{"act_as_start", "act_as_stop"} {
		req := httptest.NewRequest("GET", "/api/v1/ops/"+op, nil)
		w := httptest.NewRecorder()
		r.mux.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405 (state changes never ride on GET)", op, w.Code)
		}
	}
	if r.ctl.stops != 0 || len(r.ctl.starts) != 0 {
		t.Error("a GET reached a handler")
	}
}

func TestActAsStopErrorIsInternal(t *testing.T) {
	r := newActAsRig(t)
	r.ctl.stopErr = errors.New("db secret detail")
	w := r.do("POST", "/api/v1/ops/act_as_stop", `{}`)
	if errCode(t, w) != proto.CodeInternal || strings.Contains(w.Body.String(), "secret") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}
