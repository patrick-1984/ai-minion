package workers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// scriptedWorker connects over the real handshake and transport as a Worker
// that announces the given ops and features, and records the op requests it
// receives. A Worker without features is a scripted older Worker: like the
// real one it ignores every argument field it does not know, so it answers
// delivered to anything it gets (which is exactly the danger).
type scriptedWorker struct {
	id   string
	mu   sync.Mutex
	seen []OpRequest
}

func (s *scriptedWorker) requests() []OpRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]OpRequest(nil), s.seen...)
}

func connectScripted(t *testing.T, h *testHub, owner, name string, features []string, announce ...string) *scriptedWorker {
	t.Helper()
	key := newKey(t)
	w := h.enrol(key, owner, name)
	cfg := h.workerConfig(key, announce...)
	cfg.Features = features
	res, err := h.dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	s := &scriptedWorker{id: w.ID}
	serveOps(t, sess, func(req OpRequest) []any {
		s.mu.Lock()
		s.seen = append(s.seen, req)
		s.mu.Unlock()
		return []any{map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}}}
	})
	return s
}

func allFeatures() []string {
	return []string{proto.FeatureAnswerReviewed, proto.FeaturePipeCausal, proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}
}

func reviewed() ops.ReviewedDialog {
	return ops.ReviewedDialog{PaneID: "%1", PID: 42, Command: "claude", SessionCreated: 1700000000, DialogDigest: strings.Repeat("ab", 32)}
}

// TestOlderWorkerNeverGetsGuardedRequests runs the real Router over the real
// transport against two scripted Workers: an older one that announces the same
// op names but no features, and a modern one. A request that relies on a
// safeguard reaches only the modern one; to the older one nothing is sent and
// the caller gets unsupported_op that names the update. Requests that an older
// Worker handles the same way (a plain one-line send_text, send_keys) still
// reach it. The refusal comes from the connection's signed hello, not from a
// version string: both Workers speak protocol 1.0.
func TestOlderWorkerNeverGetsGuardedRequests(t *testing.T) {
	h := newTestHub(t, nil)
	opNames := []string{ops.OpSendText, ops.OpSendKeys, ops.OpAnswer, ops.OpPipeSend}
	old := connectScripted(t, h, idAlice, "old-1", nil, opNames...)
	modern := connectScripted(t, h, idAlice, "new-1", allFeatures(), opNames...)

	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	call := func(w *scriptedWorker, op string, args any) (any, error) {
		return rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: op, WorkerID: w.id, Session: "s1", Args: args})
	}
	target := func(w *scriptedWorker) ops.Target { return ops.Target{WorkerID: w.id, SessionName: "s1"} }
	wa := func() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }

	refused := map[string]func(w *scriptedWorker) (any, error){
		"answer": func(w *scriptedWorker) (any, error) {
			return call(w, ops.OpAnswer, ops.AnswerArgs{WriteArgs: wa(), Target: target(w), Choice: "1", Expect: reviewed()})
		},
		"send_text submit": func(w *scriptedWorker) (any, error) {
			return call(w, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target(w), Text: "go", Submit: true})
		},
		"send_text multi-line": func(w *scriptedWorker) (any, error) {
			return call(w, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target(w), Text: "one\ntwo"})
		},
		"send_text tab": func(w *scriptedWorker) (any, error) {
			return call(w, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target(w), Text: "a\tb"})
		},
		"pipe_send": func(w *scriptedWorker) (any, error) {
			return call(w, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: wa(), Target: target(w), Text: "hello", MessageID: 7})
		},
	}
	for name, run := range refused {
		out, err := run(old)
		if proto.CodeOf(err) != proto.CodeUnsupportedOp || !strings.Contains(err.Error(), "must be updated") || !strings.Contains(err.Error(), "nothing was sent") {
			t.Errorf("%s to the older Worker: out=%#v err=%v, want unsupported_op asking for an update", name, out, err)
		}
		if _, isResult := out.(ops.WriteResult); isResult {
			t.Errorf("%s: a refusal must be an error, not an outcome (%#v)", name, out)
		}
	}
	if got := old.requests(); len(got) != 0 {
		t.Fatalf("the older Worker received %d request(s) it must never see: %+v", len(got), got)
	}

	// Compatible requests still go through.
	if out, err := call(old, ops.OpSendText, ops.SendTextArgs{WriteArgs: wa(), Target: target(old), Text: "plain line"}); err != nil || out.(ops.WriteResult).Outcome != ops.OutcomeDelivered {
		t.Errorf("a plain send_text to the older Worker: %#v, %v", out, err)
	}
	if out, err := call(old, ops.OpSendKeys, ops.SendKeysArgs{WriteArgs: wa(), Target: target(old), Keys: []string{"Enter"}}); err != nil || out.(ops.WriteResult).Outcome != ops.OutcomeDelivered {
		t.Errorf("send_keys to the older Worker: %#v, %v", out, err)
	}
	if got := old.requests(); len(got) != 2 || got[0].Op != ops.OpSendText || got[1].Op != ops.OpSendKeys {
		t.Fatalf("the older Worker saw %+v, want the plain send_text and send_keys only", got)
	}

	// The modern Worker gets every one of them, with the fields intact.
	for name, run := range refused {
		if out, err := run(modern); err != nil || out.(ops.WriteResult).Outcome != ops.OutcomeDelivered {
			t.Errorf("%s to the modern Worker: %#v, %v", name, out, err)
		}
	}
	got := modern.requests()
	if len(got) != len(refused) {
		t.Fatalf("the modern Worker saw %d requests, want %d", len(got), len(refused))
	}
	for _, req := range got {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(req.Args, &m); err != nil {
			t.Fatal(err)
		}
		switch req.Op {
		case ops.OpAnswer:
			if _, ok := m["expect"]; !ok {
				t.Errorf("answer reached the Worker without expect: %s", req.Args)
			}
		case ops.OpPipeSend:
			if string(m["message_id"]) != "7" {
				t.Errorf("pipe_send lost its message_id: %s", req.Args)
			}
		}
	}
}

// TestFeaturesAreReadAgainOnEveryConnection checks that the features belong
// to the connection: a Worker that comes back as an older build does not keep
// the features of the one before it, and workers_list shows what is announced.
func TestFeaturesAreReadAgainOnEveryConnection(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, idAlice, "pi-1")
	connect := func(features []string) *transport.Session {
		cfg := h.workerConfig(key, ops.OpAnswer)
		cfg.Features = features
		res, err := h.dial(cfg)
		if err != nil {
			t.Fatal(err)
		}
		decodeWelcome(t, res)
		sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}
	first := connect(allFeatures())
	defer first.Close()
	eventually(t, "the modern Worker to be online", func() bool {
		on, ok := h.svc.Registry().Get(w.ID)
		return ok && on.HasFeature(proto.FeatureAnswerReviewed)
	})
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, h.svc.OwnerLookup())
	rt, err := NewRouter(RouterConfig{Backend: RegistryBackend(h.svc.Registry()), Directory: StoreDirectory{Store: h.st}, Authz: az})
	if err != nil {
		t.Fatal(err)
	}
	list, err := rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil || len(list) != 1 || len(list[0].Features) != 4 {
		t.Fatalf("workers_list = %+v, %v", list, err)
	}

	// The same key reconnects as an older build: the registry replaces the
	// connection, and the old feature list is gone with it.
	second := connect(nil)
	defer second.Close()
	eventually(t, "the older build to replace the connection", func() bool {
		on, ok := h.svc.Registry().Get(w.ID)
		return ok && !on.HasFeature(proto.FeatureAnswerReviewed)
	})
	_, err = rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpAnswer, WorkerID: w.ID, Session: "s1",
		Args: ops.AnswerArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: w.ID, SessionName: "s1"}, Choice: "1", Expect: reviewed()}})
	if proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Errorf("answer after the downgrade: %v", err)
	}
}

func TestRequiredFeatures(t *testing.T) {
	type tc struct {
		name string
		op   string
		args string
		want []string
	}
	for _, c := range []tc{
		{"answer", ops.OpAnswer, `{}`, []string{proto.FeatureAnswerReviewed}},
		{"plain text", ops.OpSendText, `{"text":"hello world"}`, nil},
		{"submit", ops.OpSendText, `{"text":"hi","submit":true}`, []string{proto.FeatureSendTextSubmit}},
		{"lf", ops.OpSendText, `{"text":"a\nb"}`, []string{proto.FeatureTextVerbatim}},
		{"submit and tab", ops.OpSendText, `{"text":"a\tb","submit":true}`, []string{proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}},
		{"c1 control", ops.OpSendText, `{"text":"a\u0085b"}`, []string{proto.FeatureTextVerbatim}},
		{"unreadable send_text", ops.OpSendText, `[1]`, []string{proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}},
		{"pipe plain", ops.OpPipeSend, `{"text":"hi"}`, []string{proto.FeaturePipeCausal}},
		{"pipe lf", ops.OpPipeSend, `{"text":"a\nb"}`, []string{proto.FeaturePipeCausal, proto.FeatureTextVerbatim}},
		{"send_keys", ops.OpSendKeys, `{"keys":["Enter"]}`, nil},
		{"read", ops.OpSessionSnapshot, `{}`, nil},
	} {
		got := requiredFeatures(c.op, json.RawMessage(c.args))
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: requiredFeatures = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRegistryChecksFeaturesWithTheSession checks the check and the open are
// one step: OpenOp refuses without opening a stream.
func TestRegistryChecksFeaturesWithTheSession(t *testing.T) {
	reg := NewRegistry()
	sess := newFakeSession()
	reg.Add(Online{WorkerID: "w1", Ops: []string{"answer"}, Features: []string{"zzz bad", proto.FeatureTextVerbatim, proto.FeatureTextVerbatim}}, sess)
	on, _ := reg.Get("w1")
	if len(on.Features) != 1 || on.Features[0] != proto.FeatureTextVerbatim {
		t.Fatalf("features = %v, want the one well-formed name once", on.Features)
	}
	if _, err := reg.OpenOp(context.Background(), "w1", "answer", proto.FeatureAnswerReviewed); proto.CodeOf(err) != proto.CodeUnsupportedOp {
		t.Errorf("missing feature: %v", err)
	}
	if sess.opens != 0 {
		t.Errorf("a stream was opened (%d) for a refused request", sess.opens)
	}
}
