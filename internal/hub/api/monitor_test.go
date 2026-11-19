package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// --- the gate of compliance_set --------------------------------------------

func TestRequireFreshSessionTable(t *testing.T) {
	now := time.Now()
	fresh := &session.Session{AuthAt: now.Add(-time.Minute), AuthMethod: session.AuthMethodLocal}
	stale := &session.Session{AuthAt: now.Add(-6 * time.Minute), AuthMethod: session.AuthMethodLocal}
	never := &session.Session{}
	actAs := &session.Session{AuthAt: now, AuthMethod: session.AuthMethodLocal, ActAs: &session.ActAs{TargetID: "u2"}}
	cases := []struct {
		name string
		id   Identity
		want proto.Code
	}{
		{"fresh browser session", Identity{Via: ViaSession, Session: fresh, Principal: authz.NewSessionPrincipal("a")}, ""},
		{"stale session", Identity{Via: ViaSession, Session: stale, Principal: authz.NewSessionPrincipal("a")}, proto.CodeReauthRequired},
		{"session that never checked a credential", Identity{Via: ViaSession, Session: never, Principal: authz.NewSessionPrincipal("a")}, proto.CodeReauthRequired},
		{"act-as session", Identity{Via: ViaSession, Session: actAs, Principal: authz.NewActAsPrincipal("a", "u2", now.Add(time.Hour))}, proto.CodeForbidden},
		{"API key", Identity{Via: ViaAPIKey, Principal: authz.NewAPIKeyPrincipal("a", nil, "")}, proto.CodeForbidden},
		{"API key presenting a fresh session", Identity{Via: ViaAPIKey, Session: fresh, Principal: authz.NewAPIKeyPrincipal("a", nil, "")}, proto.CodeForbidden},
		{"Worker", Identity{Via: ViaSession, Session: fresh, Principal: authz.NewWorkerPrincipal("w")}, proto.CodeForbidden},
	}
	for _, c := range cases {
		err := RequireFreshSession(&Call{Identity: c.id, Principal: c.id.Principal}, now)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || ErrorOf(err).Code != c.want {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.want)
		}
	}
}

// The dispatcher refuses compliance_set for everything but a browser session
// before any handler runs; the handler's own freshness check does the rest.
func TestComplianceSetDispatchRefusesKeysAndActAs(t *testing.T) {
	now := time.Now()
	fresh := &session.Session{AuthAt: now, AuthMethod: session.AuthMethodLocal}
	for _, c := range []struct {
		name   string
		id     Identity
		status int
	}{
		{"api key", Identity{Via: ViaAPIKey, Actor: "key:k", Principal: authz.NewAPIKeyPrincipal("a", nil, "")}, http.StatusForbidden},
		{"act as", Identity{Via: ViaSession, Actor: "a", Session: fresh, Principal: authz.NewActAsPrincipal("a", "u", now.Add(time.Hour))}, http.StatusForbidden},
		{"browser", Identity{Via: ViaSession, Actor: "a", Session: fresh, Principal: authz.NewSessionPrincipal("a")}, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCatalogRig(t, ops.Default, nil)
			ran := false
			if err := HandleTyped(r.reg, ops.OpComplianceSet, func(ctx context.Context, call *Call, a ops.ComplianceSetArgs) (ops.ComplianceSetResult, error) {
				ran = true
				return ops.ComplianceSetResult{}, RequireFreshSession(call, now)
			}); err != nil {
				t.Fatal(err)
			}
			r.authn.id = c.id
			w := r.do("POST", "/api/v1/ops/compliance_set", `{"changes":[{"key":"compliance_monitoring","value":"actions"}]}`)
			if w.Code != c.status || ran != (c.status == http.StatusOK) {
				t.Fatalf("status %d ran=%v body=%s", w.Code, ran, w.Body.String())
			}
		})
	}
}

// --- capture through the REST front door -----------------------------------

type recSink struct {
	mu  sync.Mutex
	rec []compliance.Record
	log *[]string
}

func (s *recSink) Emit(r compliance.Record) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec = append(s.rec, r)
	if s.log != nil {
		*s.log = append(*s.log, r.Event)
	}
	return true
}

func (s *recSink) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.rec {
		out = append(out, r.Event)
	}
	return out
}

func attrOf(r compliance.Record, key string) any {
	for _, a := range r.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

func newCatalogRig(t *testing.T, cat *ops.Registry, mon *compliance.Monitor) *rig {
	t.Helper()
	r := &rig{
		authn: &fakeAuthn{id: testIdentity}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs), Monitor: mon}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	return r
}

func newMonitor(pol compliance.Policy, managed bool) (*compliance.Monitor, *recSink) {
	m := compliance.NewMonitor(compliance.Deps{
		UserManaged:      func(context.Context, string) (bool, error) { return managed, nil },
		WorkerHasFeature: func(string, string) bool { return true },
	})
	s := &recSink{}
	m.SetSink(s)
	m.SetPolicy(pol)
	return m, s
}

func sendTextBody(worker string) string {
	b, _ := json.Marshal(map[string]any{
		"request_id": ops.NewUUIDv7(), "target": map[string]string{"worker_id": worker, "session_name": "work"}, "text": "echo hi mn_" + strings.Repeat("a", 52),
	})
	return string(b)
}

func TestRESTCaptureLevelsAndOrder(t *testing.T) {
	worker := ops.NewUUIDv7()
	for _, c := range []struct {
		name    string
		pol     compliance.Policy
		managed bool
		want    []string
	}{
		{"off", compliance.Policy{Version: 1, Level: "off"}, true, []string{"handler"}},
		{"actions only", compliance.Policy{Version: 1, Level: "actions"}, true, []string{"handler", compliance.EventAction}},
		{"input on: attempted before routing", compliance.Policy{Version: 1, Level: "actions", Input: true}, true, []string{compliance.EventInput, "handler", compliance.EventAction}},
		{"output level has no input without the switch", compliance.Policy{Version: 1, Level: "output"}, true, []string{"handler", compliance.EventAction}},
		{"not user-managed: actions only at every level", compliance.Policy{Version: 1, Level: "output", Input: true}, false, []string{"handler", compliance.EventAction}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mon, sink := newMonitor(c.pol, c.managed)
			var order []string
			sink.log = &order
			r := newCatalogRig(t, ops.Default, mon)
			mustHandle(t, r.reg, ops.OpSendText, func(ctx context.Context, call *Call) (any, error) {
				order = append(order, "handler")
				return ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{ID: "%1"}}, nil
			})
			w := r.do("POST", "/api/v1/ops/send_text", sendTextBody(worker))
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			if strings.Join(order, ",") != strings.Join(c.want, ",") {
				t.Fatalf("order = %v, want %v", order, c.want)
			}
			if w.Header().Get(HeaderMonitoring) != c.pol.Header() {
				t.Fatalf("header %q", w.Header().Get(HeaderMonitoring))
			}
		})
	}
}

func TestRESTCaptureRecordContentAndSecrets(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 3, Level: "output", Input: true}, true)
	r := newCatalogRig(t, ops.Default, mon)
	mustHandle(t, r.reg, ops.OpSendText, func(context.Context, *Call) (any, error) {
		return ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "internal", Replayed: true, Pane: &ops.PaneInfo{ID: "%7"}}, nil
	})
	w := r.do("POST", "/api/v1/ops/send_text", sendTextBody(worker))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(sink.rec) != 2 {
		t.Fatalf("records = %v", sink.events())
	}
	in, act := sink.rec[0], sink.rec[1]
	if in.Event != compliance.EventInput || attrOf(in, "minion.input.status") != "attempted" || attrOf(in, "minion.input.kind") != "text" {
		t.Fatalf("input = %+v", in)
	}
	if strings.Contains(in.Body, "mn_aaaa") || !strings.Contains(in.Body, "mn_[redacted]") || !strings.HasPrefix(in.Body, "echo hi ") {
		t.Fatalf("body = %q: the key was not masked or the text changed", in.Body)
	}
	reqID := attrOf(in, "minion.request.id")
	if reqID == nil || attrOf(act, "minion.request.id") != reqID {
		t.Fatalf("request id does not join: %v %v", reqID, attrOf(act, "minion.request.id"))
	}
	if attrOf(act, "minion.outcome") != "partial" || attrOf(act, "minion.replayed") != "true" || attrOf(act, "minion.policy.version") != int64(3) ||
		attrOf(act, "minion.client") != "browser" || attrOf(act, "minion.actor") != "user-1" || attrOf(act, "minion.worker.id") != worker || act.Body != "" {
		t.Fatalf("action = %+v", act)
	}
}

func TestRESTDeniedCallIsActionOnly(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 1, Level: "output", Input: true}, true)
	r := newCatalogRig(t, ops.Default, mon)
	r.decide.fn = func(decideCall) error { return NewError(proto.CodeForbidden, "no") }
	called := false
	mustHandle(t, r.reg, ops.OpSendText, func(context.Context, *Call) (any, error) { called = true; return ops.WriteResult{}, nil })
	w := r.do("POST", "/api/v1/ops/send_text", sendTextBody(worker))
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status %d called=%v", w.Code, called)
	}
	if ev := sink.events(); len(ev) != 1 || ev[0] != compliance.EventAction || attrOf(sink.rec[0], "minion.outcome") != "denied" || sink.rec[0].Body != "" {
		t.Fatalf("records = %v", ev)
	}
}

func TestRESTRefusedCallIsRecordedWithItsCode(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 1, Level: "actions"}, true)
	r := newCatalogRig(t, ops.Default, mon)
	mustHandle(t, r.reg, ops.OpSendText, func(context.Context, *Call) (any, error) {
		return nil, NewError(proto.CodeWorkerOffline, "gone")
	})
	_ = r.do("POST", "/api/v1/ops/send_text", sendTextBody(worker))
	if len(sink.rec) != 1 || attrOf(sink.rec[0], "minion.outcome") != "refused:worker_offline" {
		t.Fatalf("records = %+v", sink.rec)
	}
}

func TestRESTSnapshotOutputDedupe(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 1, Level: "output"}, true)
	r := newCatalogRig(t, ops.Default, mon)
	screen := ops.Screen{Width: 80, Height: 2, Lines: []string{"hello", "world"}}
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) {
		return ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: screen}, nil
	})
	body := `{"target":{"worker_id":"` + worker + `","session_name":"work"}}`
	for i := 0; i < 2; i++ {
		if w := r.do("POST", "/api/v1/ops/session_snapshot", body); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	screen.Lines = []string{"hello", "changed"}
	_ = r.do("POST", "/api/v1/ops/session_snapshot", body)
	var bodies []string
	for _, rec := range sink.rec {
		if rec.Event == compliance.EventOutput {
			bodies = append(bodies, rec.Body)
		}
	}
	// A record per read; the text only when the fingerprint changed.
	if len(bodies) != 3 || bodies[0] != "hello\nworld" || bodies[1] != "" || bodies[2] != "hello\nchanged" {
		t.Fatalf("output bodies = %q", bodies)
	}
}

func TestRESTStreamRecordsElements(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 1, Level: "output"}, true)
	r := newCatalogRig(t, ops.Default, mon)
	turn := ops.PipeTurn{TurnID: ops.NewUUIDv7(), Session: "work", Direction: ops.PipeReceived, Text: "the answer"}
	if err := r.reg.HandleStream(ops.OpPipeWait, func(ctx context.Context, c *Call, emit func(any) error) error {
		return emit(turn)
	}); err != nil {
		t.Fatal(err)
	}
	w := r.do("POST", "/api/v1/ops/pipe_wait", `{"target":{"worker_id":"`+worker+`","session_name":"work"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ev := sink.events()
	want := []string{compliance.EventAction, compliance.EventOutput, compliance.EventAction}
	if strings.Join(ev, ",") != strings.Join(want, ",") || sink.rec[1].Body != "the answer" || attrOf(sink.rec[0], "minion.turn.id") != turn.TurnID {
		t.Fatalf("records = %v", ev)
	}
}

// --- the live notice -------------------------------------------------------

func TestStaleClientIsRefusedOnceForContentOps(t *testing.T) {
	worker := ops.NewUUIDv7()
	cases := []struct {
		name   string
		cur    compliance.Policy
		seen   string
		op     string
		refuse bool
	}{
		{"never seen, monitoring on", compliance.Policy{Version: 2, Level: "actions"}, "0", ops.OpSendText, true},
		{"seen the current version", compliance.Policy{Version: 2, Level: "actions"}, "2", ops.OpSendText, false},
		{"no header: informed by the response header only", compliance.Policy{Version: 2, Level: "actions"}, "", ops.OpSendText, false},
		{"monitoring off never refuses", compliance.Policy{Version: 3, Level: "off"}, "1", ops.OpSendText, false},
		{"not a content op", compliance.Policy{Version: 2, Level: "output", Input: true}, "0", ops.OpSessionsList, false},
		{"unknown older version", compliance.Policy{Version: 9, Level: "output"}, "5", ops.OpSessionSnapshot, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mon, sink := newMonitor(c.cur, true)
			r := newCatalogRig(t, ops.Default, mon)
			routed := 0
			for _, op := range []string{ops.OpSendText, ops.OpSessionSnapshot, ops.OpSessionsList} {
				mustHandle(t, r.reg, op, func(context.Context, *Call) (any, error) {
					routed++
					return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
				})
			}
			body := sendTextBody(worker)
			if c.op != ops.OpSendText {
				body = `{"target":{"worker_id":"` + worker + `","session_name":"work"}}`
			}
			if c.op == ops.OpSessionsList {
				body = `{"worker_id":"` + worker + `"}`
			}
			do := func(seen string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", "/api/v1/ops/"+c.op, strings.NewReader(body))
				req.RemoteAddr = netip.MustParseAddrPort("192.0.2.7:1").String()
				if seen != "" {
					req.Header.Set(HeaderMonitoringSeen, seen)
				}
				w := httptest.NewRecorder()
				r.mux.ServeHTTP(w, req)
				return w
			}
			w := do(c.seen)
			if c.refuse {
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "monitoring_changed") || routed != 0 || len(sink.rec) != 0 {
					t.Fatalf("status %d routed %d records %d body %s", w.Code, routed, len(sink.rec), w.Body.String())
				}
				if w.Header().Get(HeaderMonitoring) != c.cur.Header() {
					t.Fatalf("the refusal must carry the new policy: %q", w.Header().Get(HeaderMonitoring))
				}
				// The client now sends the version it was told: it goes through.
				if w2 := do("v=" + strings.TrimPrefix(strings.Split(w.Header().Get(HeaderMonitoring), ";")[0], "v=")); w2.Code != 200 {
					t.Fatalf("second attempt: %d %s", w2.Code, w2.Body.String())
				}
				return
			}
			if w.Code != 200 || routed != 1 {
				t.Fatalf("status %d routed %d %s", w.Code, routed, w.Body.String())
			}
		})
	}
}

func TestLooserPolicyNeverRefuses(t *testing.T) {
	mon, _ := newMonitor(compliance.Policy{Version: 1, Level: "output", Input: true}, true)
	mon.SetPolicy(compliance.Policy{Version: 2, Level: "actions"})
	if _, refuse, _ := mon.Stale(1, true, ops.OpSendText); refuse {
		t.Fatal("a looser policy refused a client that saw the stricter one")
	}
	mon.SetPolicy(compliance.Policy{Version: 3, Level: "output", Input: true})
	if _, refuse, _ := mon.Stale(2, true, ops.OpSendText); !refuse {
		t.Fatal("a stricter policy did not refuse")
	}
}

// One snapshot of the policy decides a refusal and answers it: the header names
// the version of the notice, so the client's retry carries the one that passes.
func TestRefusalHeaderMatchesTheNotice(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, _ := newMonitor(compliance.Policy{Version: 2, Level: "actions"}, true)
	r := newCatalogRig(t, ops.Default, mon)
	mustHandle(t, r.reg, ops.OpSendText, func(context.Context, *Call) (any, error) { return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil })
	// The policy tightens between the header (set first) and the decision.
	r.decide.fn = func(decideCall) error {
		mon.SetPolicy(compliance.Policy{Version: 3, Level: "output", Input: true})
		return nil
	}
	req := httptest.NewRequest("POST", "/api/v1/ops/send_text", strings.NewReader(sendTextBody(worker)))
	req.Header.Set(HeaderMonitoringSeen, "2")
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "policy v3") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(HeaderMonitoring); got != (compliance.Policy{Version: 3, Level: "output", Input: true}).Header() {
		t.Fatalf("header = %q", got)
	}
}

// A read is recorded only after its answer was written: a client that went
// away is not reported as having received the screen.
func TestRESTReadIsRecordedAfterTheAnswerWasWritten(t *testing.T) {
	worker := ops.NewUUIDv7()
	mon, sink := newMonitor(compliance.Policy{Version: 1, Level: "output"}, true)
	r := newCatalogRig(t, ops.Default, mon)
	mustHandle(t, r.reg, ops.OpSessionSnapshot, func(context.Context, *Call) (any, error) {
		return ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Lines: []string{"private"}}}, nil
	})
	body := `{"target":{"worker_id":"` + worker + `","session_name":"work"}}`
	req := httptest.NewRequest("POST", "/api/v1/ops/session_snapshot", strings.NewReader(body))
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder()}
	r.mux.ServeHTTP(w, req)
	for _, rec := range sink.rec {
		if rec.Event == compliance.EventOutput || rec.Body != "" {
			t.Fatalf("content recorded for an answer that was not written: %+v", rec)
		}
	}
	if len(sink.rec) != 1 || attrOf(sink.rec[0], "minion.outcome") != "refused:reply_failed" {
		t.Fatalf("records = %+v", sink.rec)
	}
}

type failingWriter struct{ *httptest.ResponseRecorder }

func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
