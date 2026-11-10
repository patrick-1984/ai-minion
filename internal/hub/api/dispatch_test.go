package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func errCode(t *testing.T, w *httptest.ResponseRecorder) proto.Code {
	t.Helper()
	var b struct {
		Error struct {
			Code    proto.Code     `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Error.Code == "" || b.Error.Details == nil {
		t.Fatalf("not an error envelope (%d): %s", w.Code, w.Body)
	}
	if w.Code != proto.HTTPStatus(b.Error.Code) {
		t.Errorf("status %d does not match code %s", w.Code, b.Error.Code)
	}
	return b.Error.Code
}

func echoHandler(_ context.Context, c *Call) (any, error) {
	a := c.Args.(*pingArgs)
	return pingResult{Echo: a.Message, At: ops.Timestamp(time.UnixMilli(1_700_000_000_042))}, nil
}

func TestPostRunsTheHandlerAfterDecide(t *testing.T) {
	r := newRig(t)
	var order []string
	r.decide.log = &order
	mustHandle(t, r.reg, opPing, func(ctx context.Context, c *Call) (any, error) {
		order = append(order, "handler")
		return echoHandler(ctx, c)
	})
	w := r.do("POST", "/api/v1/ops/ping_read", `{"message":"hi","unknown_future_field":true}`)
	if w.Code != 200 || w.Body.String() != `{"echo":"hi","at":"2023-11-14T22:13:20.042Z"}`+"\n" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Join(order, ",") != "decide,handler" {
		t.Errorf("order = %v, want authz.Decide before the handler", order)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	// Decide got the caller's principal, the op's accepted actions and the
	// caller's own user resource (a Hub op names no Worker).
	c := r.decide.calls[0]
	if c.p.UserID != "user-1" || !c.accepted.Permits(ops.ActionManage) || c.accepted.Permits(ops.ActionRead) ||
		c.res != (authz.UserResource{ID: "user-1"}) {
		t.Errorf("Decide(%+v)", c)
	}
}

func TestGetForReadOpsWithQueryArgs(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler)
	w := r.do("GET", "/api/v1/ops/ping_read?message=from-query&limit=5", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"echo":"from-query"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestGetOnAWriteOpIsRefused(t *testing.T) {
	r := newRig(t)
	called := false
	mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { called = true; return ops.WriteResult{}, nil })
	w := r.do("GET", "/api/v1/ops/ping_set?message=x", "")
	if errCode(t, w) != proto.CodeMethodNotAllowed || w.Header().Get("Allow") != "POST" {
		t.Errorf("%d %s allow=%q", w.Code, w.Body, w.Header().Get("Allow"))
	}
	if called || r.decide.count() != 0 {
		t.Error("the handler or Decide ran for a refused method")
	}
	for _, m := range []string{"PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
		if w := r.do(m, "/api/v1/ops/ping_set", ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d", m, w.Code)
		}
	}
}

func TestOpWithoutHandlerIsUnsupported(t *testing.T) {
	r := newRig(t)
	w := r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if errCode(t, w) != proto.CodeUnsupportedOp || w.Code != 501 {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	// A name that is not in the catalog at all answers the same.
	if w := r.do("POST", "/api/v1/ops/no_such_op", `{}`); errCode(t, w) != proto.CodeUnsupportedOp {
		t.Errorf("unknown op: %d %s", w.Code, w.Body)
	}
	if r.decide.count() != 0 {
		t.Error("Decide ran for an op without a handler")
	}
}

func TestUnauthenticatedNeverReachesTheHandlerOrTheCatalogLookup(t *testing.T) {
	r := newRig(t)
	r.authn.err = unauthenticated("sign in first")
	called := false
	mustHandle(t, r.reg, opPing, func(context.Context, *Call) (any, error) { called = true; return nil, nil })
	for _, path := range []string{"/api/v1/ops/ping_read", "/api/v1/ops/no_such_op"} {
		w := r.do("POST", path, `{}`)
		if errCode(t, w) != proto.CodeUnauthenticated {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if called || r.decide.count() != 0 {
		t.Error("handler or Decide ran for an unauthenticated request")
	}
}

func TestDecideDenialStopsTheHandler(t *testing.T) {
	for _, code := range []proto.Code{proto.CodeForbidden, proto.CodeNotFound} {
		r := newRig(t)
		r.decide.fn = func(decideCall) error { return proto.NewError(code, "no") }
		called := false
		mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { called = true; return ops.WriteResult{}, nil })
		w := r.do("POST", "/api/v1/ops/ping_set", `{"request_id":"`+ops.NewUUIDv7()+`","message":"m"}`)
		if errCode(t, w) != code || called {
			t.Errorf("%s: %d %s called=%v", code, w.Code, w.Body, called)
		}
		ev := r.audit.all()
		if len(ev) != 1 || ev[0].Outcome != AuditOutcomeDenied || ev[0].Action != opPingSet || ev[0].Actor != "user-1" {
			t.Errorf("audit = %+v", ev)
		}
	}
}

func TestAuthzFailureOfALookupIsInternalAndLogged(t *testing.T) {
	r := newRig(t)
	r.decide.fn = func(decideCall) error { return authz.ErrNotWired }
	mustHandle(t, r.reg, opPing, echoHandler)
	w := r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if errCode(t, w) != proto.CodeInternal || !strings.Contains(r.logs.String(), "authorisation failed") {
		t.Errorf("%d %s logs=%s", w.Code, w.Body, r.logs)
	}
}

func TestArgumentValidation(t *testing.T) {
	r := newRig(t)
	called := false
	mustHandle(t, r.reg, opPing, func(ctx context.Context, c *Call) (any, error) { called = true; return echoHandler(ctx, c) })
	cases := map[string]string{
		`{"message":"this message is much too long"}`: "message",
		`{"limit":"x"}`: "limit",
		`{`:             "",
	}
	for body, field := range cases {
		w := r.do("POST", "/api/v1/ops/ping_read", body)
		if errCode(t, w) != proto.CodeInvalidArgument {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
			continue
		}
		var env struct {
			Error struct{ Details map[string]any }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if field != "" && env.Error.Details["field"] != field {
			t.Errorf("%s: details %v", body, env.Error.Details)
		}
	}
	if called {
		t.Error("the handler ran for invalid arguments")
	}
	big := `{"message":"` + strings.Repeat("x", MaxBodyBytes) + `"}`
	if w := r.do("POST", "/api/v1/ops/ping_read", big); errCode(t, w) != proto.CodeMessageTooLarge {
		t.Errorf("oversized body: %d", w.Code)
	}
}

func TestResourceFromTheTargetDecidesTheCall(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) { return ops.SessionSnapshotResult{}, nil })
	mustHandle(t, r.reg, opShareGet, func(context.Context, *Call) (any, error) { return shareResult{Mode: "all"}, nil })
	wid := ops.NewUUIDv7()

	w := r.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"`+wid+`","session_name":"minion-1"}}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := r.decide.calls[0].res; got != (authz.SessionResource{WorkerID: wid, Name: "minion-1"}) || !r.decide.calls[0].accepted.Permits(ops.ActionRead) {
		t.Errorf("Decide resource = %#v accepted = %v", got, r.decide.calls[0].accepted)
	}

	w = r.do("GET", "/api/v1/ops/probe_share_get?worker_id="+wid, "")
	if w.Code != 200 || w.Body.String() != `{"mode":"all"}`+"\n" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	last := r.decide.calls[len(r.decide.calls)-1]
	if last.res != (authz.WorkerResource{ID: wid}) || !last.accepted.Permits(ops.ActionOperate) || last.accepted.Permits(ops.ActionManage) {
		t.Errorf("share_get decided on %#v with %v, want operate on the Worker", last.res, last.accepted)
	}

	// Arguments that fail their own validation never reach Decide.
	n := r.decide.count()
	w = r.do("POST", "/api/v1/ops/session_snapshot", `{"target":{"worker_id":"not-a-uuid","session_name":"minion-1"}}`)
	if errCode(t, w) != proto.CodeInvalidArgument || r.decide.count() != n {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestWithResourceOverridesTheDefault(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler, WithResource(func(p authz.Principal, _ any) (authz.Resource, error) {
		return authz.UserResource{ID: "someone-else"}, nil
	}))
	r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if r.decide.calls[0].res != (authz.UserResource{ID: "someone-else"}) {
		t.Errorf("resource = %#v", r.decide.calls[0].res)
	}
	r2 := newRig(t)
	mustHandle(t, r2.reg, opPing, echoHandler, WithResource(func(authz.Principal, any) (authz.Resource, error) {
		return nil, invalidArg("user_id", "is required")
	}))
	if w := r2.do("POST", "/api/v1/ops/ping_read", `{}`); errCode(t, w) != proto.CodeInvalidArgument || r2.decide.count() != 0 {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestHandlerErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		err  error
		code proto.Code
	}{
		{proto.NewError(proto.CodeConflict, "env"), proto.CodeConflict},
		{&ops.ArgumentError{Field: "x", Reason: "bad"}, proto.CodeInvalidArgument},
		{errBoom, proto.CodeInternal},
	}
	for _, c := range cases {
		r := newRig(t)
		mustHandle(t, r.reg, opPing, func(context.Context, *Call) (any, error) { return nil, c.err })
		w := r.do("POST", "/api/v1/ops/ping_read", `{}`)
		if errCode(t, w) != c.code {
			t.Errorf("%v: %d %s", c.err, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret detail") {
			t.Errorf("the cause leaked: %s", w.Body)
		}
	}
}

func TestPanicInAHandlerIsAnInternalEnvelope(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, func(context.Context, *Call) (any, error) { panic("secret detail") })
	w := r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if errCode(t, w) != proto.CodeInternal || strings.Contains(w.Body.String(), "secret") || strings.Contains(r.logs.String(), "secret detail") {
		t.Errorf("%d %s logs=%s", w.Code, w.Body, r.logs)
	}
}

func TestNilResultIsAnEmptyObject(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, func(context.Context, *Call) (any, error) { return nil, nil })
	if w := r.do("POST", "/api/v1/ops/ping_read", `{}`); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestWriteOpsAreAudited(t *testing.T) {
	cases := []struct {
		name    string
		result  any
		err     error
		outcome string
	}{
		{"delivered", ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil, AuditOutcomeSuccess},
		{"refused", ops.Refused("pane_busy", nil), nil, string(ops.OutcomeFailedNotSent)},
		{"partial", &ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "x"}, nil, string(ops.OutcomePartial)},
		{"error", nil, proto.NewError(proto.CodeConflict, "c"), "failed"},
	}
	for _, c := range cases {
		r := newRig(t)
		r.authn.id.Principal = authz.NewActAsPrincipal("admin-1", "user-9", time.Now().Add(time.Hour))
		r.authn.id.Actor, r.authn.id.ActingAs = "admin-1", "user-9"
		mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { return c.result, c.err })
		r.do("POST", "/api/v1/ops/ping_set", `{"request_id":"`+ops.NewUUIDv7()+`","message":"secret message body"}`)
		ev := r.audit.all()
		if len(ev) != 1 {
			t.Fatalf("%s: audit = %+v", c.name, ev)
		}
		e := ev[0]
		if e.Outcome != c.outcome || e.Action != opPingSet || e.Actor != "admin-1" || e.ActingAs != "user-9" || e.Target != "user-1" && e.Target != "admin-1" && e.Target != "user-9" {
			t.Errorf("%s: event %+v", c.name, e)
		}
		if strings.Contains(e.Actor+e.Action+e.Target+e.Outcome, "secret message") {
			t.Errorf("content in the audit event: %+v", e)
		}
	}
}

func TestReadOpsAreNotAudited(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler)
	r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if ev := r.audit.all(); len(ev) != 0 {
		t.Errorf("audit = %+v", ev)
	}
}

func TestAuditFailureDoesNotChangeTheAnswerButIsLogged(t *testing.T) {
	r := newRig(t)
	r.audit.err = errors.New("disk full")
	mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil })
	w := r.do("POST", "/api/v1/ops/ping_set", `{"request_id":"`+ops.NewUUIDv7()+`"}`)
	if w.Code != 200 || !strings.Contains(r.logs.String(), "audit write failed") {
		t.Errorf("%d %s logs=%s", w.Code, w.Body, r.logs)
	}
}

func TestCallCarriesTheIdentity(t *testing.T) {
	r := newRig(t)
	var got *Call
	mustHandle(t, r.reg, opPing, func(_ context.Context, c *Call) (any, error) { got = c; return struct{}{}, nil })
	r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if got == nil || got.Op.Name != opPing || got.Principal.UserID != "user-1" || got.IP != testIdentity.IP || got.Identity.Via != ViaSession {
		t.Errorf("call = %+v", got)
	}
	if _, ok := got.Args.(*pingArgs); !ok {
		t.Errorf("Args is %T, want *pingArgs", got.Args)
	}
}

func TestStreamOpIsNDJSON(t *testing.T) {
	r := newRig(t)
	if err := r.reg.HandleStream(opTick, func(_ context.Context, c *Call, emit func(any) error) error {
		for i := 1; i <= c.Args.(*tickArgs).Count; i++ {
			if err := emit(tickResult{N: i}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := r.do("GET", "/api/v1/ops/tick_stream?count=3", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	sc := bufio.NewScanner(w.Body)
	var n []int
	for sc.Scan() {
		var v tickResult
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		n = append(n, v.N)
	}
	if len(n) != 3 || n[2] != 3 {
		t.Errorf("lines = %v", n)
	}
}

func TestStreamErrorBeforeAndAfterTheFirstElement(t *testing.T) {
	r := newRig(t)
	_ = r.reg.HandleStream(opTick, func(_ context.Context, c *Call, emit func(any) error) error {
		if c.Args.(*tickArgs).Count == 0 {
			return proto.NewError(proto.CodeNotFound, "nothing")
		}
		_ = emit(tickResult{N: 1})
		return proto.NewError(proto.CodeConflict, "later")
	})
	if w := r.do("GET", "/api/v1/ops/tick_stream", ""); errCode(t, w) != proto.CodeNotFound {
		t.Errorf("before the first element: %d %s", w.Code, w.Body)
	}
	w := r.do("GET", "/api/v1/ops/tick_stream?count=1", "")
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if w.Code != 200 || len(lines) != 2 || !strings.Contains(lines[1], `"conflict"`) {
		t.Errorf("after the first element: %d %q", w.Code, lines)
	}
}

func TestStreamReauthorisesAndStopsWhenPermissionIsGone(t *testing.T) {
	r := newRig(t)
	r.d.ReauthInterval = 10 * time.Millisecond
	deny := make(chan struct{})
	r.decide.fn = func(decideCall) error {
		select {
		case <-deny:
			return proto.NewError(proto.CodeForbidden, "revoked")
		default:
			return nil
		}
	}
	started := make(chan struct{})
	_ = r.reg.HandleStream(opTick, func(ctx context.Context, _ *Call, emit func(any) error) error {
		if err := emit(tickResult{N: 1}); err != nil {
			return err
		}
		close(started)
		<-ctx.Done() // runs until the dispatcher cancels it
		return ctx.Err()
	})
	go func() { <-started; close(deny) }()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do("GET", "/api/v1/ops/tick_stream", "") }()
	select {
	case w := <-done:
		lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
		if len(lines) != 2 || !strings.Contains(lines[1], `"forbidden"`) {
			t.Errorf("body = %q, want the element and a forbidden error line", w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end after the permission was revoked")
	}
	if r.decide.count() < 2 {
		t.Errorf("Decide ran %d times, want the initial check and a re-check", r.decide.count())
	}
}

// A Worker op that names no Worker must not be decided on the caller's user
// resource (a plain user holds read, operate and message there): it fails
// closed before Decide and before the handler.
func TestWorkerOpWithoutAWorkerFailsClosed(t *testing.T) {
	wid := ops.NewUUIDv7()
	cases := []struct{ name, op, body string }{
		{"sessions_list shape", ops.OpSessionsList, `{}`},
		{"session_create shape", ops.OpSessionCreate, `{"request_id":"` + ops.NewUUIDv7() + `"}`},
		{"*Target field absent", opPtrRead, `{}`},
		{"*Target field null", opPtrRead, `{"target":null}`},
		{"worker_id member null", ops.OpSessionsList, `{"worker_id":null}`},
		{"empty worker_id member", ops.OpSessionsList, `{"worker_id":""}`},
		{"an unknown member is no worker", ops.OpSessionsList, `{"worker":"` + wid + `"}`},
	}
	for _, c := range cases {
		r := newRig(t)
		ran := false
		h := func(context.Context, *Call) (any, error) { ran = true; return struct{}{}, nil }
		for _, n := range []string{ops.OpSessionsList, ops.OpSessionCreate, opPtrRead} {
			mustHandle(t, r.reg, n, h)
		}
		w := r.do("POST", "/api/v1/ops/"+c.op, c.body)
		if errCode(t, w) != proto.CodeInvalidArgument || !strings.Contains(w.Body.String(), `"field":"worker_id"`) {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
		if ran || r.decide.count() != 0 {
			t.Errorf("%s: handler ran=%v, Decide calls=%d; neither may happen", c.name, ran, r.decide.count())
		}
	}
	// Over GET too.
	r := newRig(t)
	mustHandle(t, r.reg, ops.OpSessionsList, func(context.Context, *Call) (any, error) { return struct{}{}, nil })
	if w := r.do("GET", "/api/v1/ops/sessions_list", ""); errCode(t, w) != proto.CodeInvalidArgument || r.decide.count() != 0 {
		t.Errorf("GET: %d %s", w.Code, w.Body)
	}
}

func TestWorkerOpIsDecidedOnTheWorkerNamedByTheRequest(t *testing.T) {
	wid := ops.NewUUIDv7()
	r := newRig(t)
	var got *Call
	h := func(_ context.Context, c *Call) (any, error) { got = c; return struct{}{}, nil }
	mustHandle(t, r.reg, ops.OpSessionsList, h)
	mustHandle(t, r.reg, opPtrRead, h)

	for _, req := range [][2]string{
		{"POST", `{"worker_id":"` + wid + `"}`}, // no such member in SessionsListArgs
		{"GET", ""},
	} {
		path := "/api/v1/ops/sessions_list"
		if req[0] == "GET" {
			path += "?worker_id=" + wid
		}
		before := r.decide.count()
		if w := r.do(req[0], path, req[1]); w.Code != 200 {
			t.Fatalf("%s: %d %s", req[0], w.Code, w.Body)
		}
		c := r.decide.calls[before]
		if c.res != (authz.WorkerResource{ID: wid}) || got.Resource != (authz.WorkerResource{ID: wid}) {
			t.Errorf("%s: decided on %#v, call carries %#v", req[0], c.res, got.Resource)
		}
	}

	// A *Target field is a target.
	w := r.do("POST", "/api/v1/ops/"+opPtrRead, `{"target":{"worker_id":"`+wid+`","session_name":"minion-1"}}`)
	if w.Code != 200 || r.decide.calls[r.decide.count()-1].res != (authz.SessionResource{WorkerID: wid, Name: "minion-1"}) {
		t.Errorf("%d %s %#v", w.Code, w.Body, r.decide.calls)
	}
	// The worker_id of the request must be a string UUIDv7.
	n := r.decide.count()
	for _, body := range []string{`{"worker_id":5}`, `{"worker_id":"w1"}`} {
		if w := r.do("POST", "/api/v1/ops/sessions_list", body); errCode(t, w) != proto.CodeInvalidArgument {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if r.decide.count() != n {
		t.Error("Decide ran for an invalid worker_id")
	}
}

func TestWriteWithAnOverlongWorkerIDIsInvalidBeforeDecide(t *testing.T) {
	r := newRig(t)
	ran := false
	mustHandle(t, r.reg, opShareSet, func(context.Context, *Call) (any, error) { ran = true; return ops.WriteResult{}, nil })
	body := `{"request_id":"` + ops.NewUUIDv7() + `","worker_id":"` + strings.Repeat("w", 300) + `","mode":"all"}`
	w := r.do("POST", "/api/v1/ops/"+opShareSet, body)
	if errCode(t, w) != proto.CodeInvalidArgument || !strings.Contains(w.Body.String(), `"field":"worker_id"`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if ran || r.decide.count() != 0 {
		t.Errorf("handler ran=%v, Decide calls=%d", ran, r.decide.count())
	}
}

func TestDeniedWriteWithAnOverlongSessionLabelIsStillAudited(t *testing.T) {
	r := newRig(t)
	r.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeForbidden, "no") }
	mustHandle(t, r.reg, opSessSet, func(context.Context, *Call) (any, error) { return ops.WriteResult{}, nil })
	wid := ops.NewUUIDv7()
	body := `{"request_id":"` + ops.NewUUIDv7() + `","worker_id":"` + wid + `","session_name":"` + strings.Repeat("\u00e9", 300) + `"}`
	w := r.do("POST", "/api/v1/ops/"+opSessSet, body)
	if errCode(t, w) != proto.CodeForbidden {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	ev := r.audit.all() // the fake refuses events the real log would refuse
	if len(ev) != 1 || ev[0].Outcome != AuditOutcomeDenied || ev[0].Action != opSessSet || !strings.HasPrefix(ev[0].Target, wid+":") {
		t.Errorf("audit = %+v; logs = %s", ev, r.logs)
	}
	if strings.Contains(r.logs.String(), "audit write failed") {
		t.Errorf("the audit write failed: %s", r.logs)
	}
}

func TestWriteHandlerPanicIsAuditedOnce(t *testing.T) {
	r := newRig(t)
	mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { panic("after the mutation") })
	w := r.do("POST", "/api/v1/ops/ping_set", `{"request_id":"`+ops.NewUUIDv7()+`","message":"m"}`)
	if errCode(t, w) != proto.CodeInternal {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	ev := r.audit.all()
	if len(ev) != 1 || ev[0].Action != opPingSet || ev[0].Outcome != string(ops.OutcomeUnknown) || ev[0].Actor != "user-1" {
		t.Errorf("audit = %+v", ev)
	}
	// A read panic and a panic before the handler write no audit row.
	r2 := newRig(t)
	mustHandle(t, r2.reg, opPing, func(context.Context, *Call) (any, error) { panic("x") })
	r2.do("POST", "/api/v1/ops/ping_read", `{}`)
	if ev := r2.audit.all(); len(ev) != 0 {
		t.Errorf("read panic audited: %+v", ev)
	}
}

func TestWriteHandlerPanicDoesNotDoubleAuditAfterTheNormalAudit(t *testing.T) {
	// The panic comes after d.audit has run: here from the audit port itself.
	r := newRig(t)
	mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil })
	r.d.Audit = panicAuditOnce{inner: r.audit}
	r.do("POST", "/api/v1/ops/ping_set", `{"request_id":"`+ops.NewUUIDv7()+`","message":"m"}`)
	if ev := r.audit.all(); len(ev) != 1 {
		t.Errorf("audit = %+v, want exactly one row", ev)
	}
}

type panicAuditOnce struct{ inner *fakeAudit }

func (p panicAuditOnce) Record(ctx context.Context, ev audit.Event) error {
	_ = p.inner.Record(ctx, ev)
	panic("audit port failed")
}

func TestStreamEndsWhenTheCredentialsAreRevoked(t *testing.T) {
	for name, change := range map[string]func(r *rig){
		"session or key revoked": func(r *rig) { r.authn.setErr(unauthenticated("sign in first")) },
		"another caller": func(r *rig) {
			r.authn.mu.Lock()
			r.authn.id.Principal = authz.NewSessionPrincipal("user-2")
			r.authn.id.Actor = "user-2"
			r.authn.mu.Unlock()
		},
	} {
		r := newRig(t)
		r.d.ReauthInterval = 10 * time.Millisecond
		started := make(chan struct{})
		_ = r.reg.HandleStream(opTick, func(ctx context.Context, _ *Call, emit func(any) error) error {
			if err := emit(tickResult{N: 1}); err != nil {
				return err
			}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		go func() { <-started; change(r) }()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- r.do("GET", "/api/v1/ops/tick_stream", "") }()
		select {
		case w := <-done:
			lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
			if len(lines) != 2 || !strings.Contains(lines[1], `"unauthenticated"`) {
				t.Errorf("%s: body = %q, want the element and an unauthenticated error line", name, w.Body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the stream did not end after the credentials were revoked", name)
		}
		if r.authn.calls() < 2 {
			t.Errorf("%s: Authenticate ran %d times, want the request and a re-check", name, r.authn.calls())
		}
	}
}

// A client that sends part of a body and stops must not hold the request for
// ever: the body read has a deadline.
func TestStalledRequestBodyIsCutOff(t *testing.T) {
	r := newRig(t)
	r.d.BodyTimeout = 150 * time.Millisecond
	mustHandle(t, r.reg, opPing, echoHandler)
	srv := httptest.NewServer(r.mux)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("POST /api/v1/ops/ping_read HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" +
		"Content-Length: 100\r\n\r\n{\"message\":"))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), `"invalid_argument"`) {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

// Every answer written before the body is complete is under the body
// deadline too: net/http reads the rest of an unread body before it answers,
// and a client that announced a body and stops must not hold the connection.
func TestEarlyRejectionsOfAnIncompleteBodyAreCutOff(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(r *rig)
		method string
		path   string
		want   int
	}{
		{"unauthenticated", func(r *rig) { r.authn.err = unauthenticated("sign in first") }, "POST", "/api/v1/ops/ping_read", 401},
		{"unsupported operation", func(*rig) {}, "POST", "/api/v1/ops/ping_read", 501},
		{"method not allowed", func(r *rig) {
			mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) { return nil, nil })
		}, "PUT", "/api/v1/ops/ping_set", 405},
		{"route fallback", func(*rig) {}, "POST", "/api/v1/no/such/route", 404},
		{"GET with a body", func(r *rig) { mustHandle(t, r.reg, opPing, echoHandler) }, "GET", "/api/v1/ops/ping_read", 400},
		// The mux redirects these before any route runs; only the wrapper of
		// Router.Handler puts the deadline on them.
		{"bare prefix redirect", func(*rig) {}, "POST", "/api/v1", 307},
		{"repeated slash redirect", func(*rig) {}, "POST", "/api/v1//nothing", 307},
		{"dot segment redirect", func(*rig) {}, "POST", "/api/v1/./nothing", 307},
		{"parent segment redirect", func(*rig) {}, "POST", "/api/v1/ops/../nothing", 307},
		{"redirect out of the API", func(*rig) {}, "POST", "/api/v1/../elsewhere", 307},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.d.BodyTimeout = 150 * time.Millisecond
			c.setup(r)
			srv := httptest.NewServer(r.router.Handler(r.mux))
			defer srv.Close()
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_, err = conn.Write([]byte(c.method + " " + c.path + " HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" +
				"Content-Length: 100\r\n\r\n{\"message\":"))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no answer to a stalled body: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			if time.Since(start) > 3*time.Second {
				t.Errorf("answered after %v, want about the body timeout", time.Since(start))
			}
			// (A GET reads and drops its body under the deadline: the stalled
			// body is a failed read, answered as invalid_argument.)
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
			if !resp.Close {
				t.Error("the connection is kept open after a stalled body")
			}
		})
	}
}

// streamToANonReadingClient starts a stream whose handler writes big elements until its
// emit fails, on a real socket, against a client that sends the request and
// never reads. It returns a channel that closes when the handler has
// returned.
func streamToANonReadingClient(t *testing.T, r *rig) <-chan struct{} {
	t.Helper()
	returned := make(chan struct{})
	chunk := strings.Repeat("x", 256<<10)
	if err := r.reg.HandleStream(opTick, func(_ context.Context, _ *Call, emit func(any) error) error {
		defer close(returned)
		for {
			if err := emit(chunk); err != nil {
				return err
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.mux)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET /api/v1/ops/tick_stream HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	return returned
}

// A client that stops reading must not hold the handler: each write of a
// stream has a deadline.
func TestStreamWriteToANonReadingClientTimesOut(t *testing.T) {
	r := newRig(t)
	r.d.StreamWriteTimeout = 200 * time.Millisecond
	returned := streamToANonReadingClient(t, r)
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("the stream handler is still blocked on a client that does not read")
	}
}

// Revoking the caller interrupts a write that is blocked on a client that
// does not read, even when the write timeout is long.
func TestStreamRevocationInterruptsABlockedWrite(t *testing.T) {
	r := newRig(t)
	r.d.StreamWriteTimeout = time.Hour
	r.d.ReauthInterval = 50 * time.Millisecond
	returned := streamToANonReadingClient(t, r)
	time.Sleep(500 * time.Millisecond) // let the socket buffers fill up
	select {
	case <-returned:
		t.Fatal("the handler ended before the revocation (the socket did not fill up)")
	default:
	}
	r.authn.setErr(unauthenticated("sign in first"))
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked stream is still blocked in a write")
	}
}
