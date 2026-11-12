package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The default Hub composes the pipe: these tests start the Hub from a copy of
// the DefaultRegistry (every module that init registers, nothing wired by
// hand) and add only a module that reads services back.

func TestPipeModuleIsInTheDefaultCompositionAfterRouteAndEvents(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("pipe") < 0 || idx("pipe") < idx("route") || idx("pipe") < idx("events") || idx("pipe") < idx("apikey") || idx("pipe") < idx("worker-kinds") && idx("worker-kinds") >= 0 {
		t.Errorf("modules = %v", names)
	}
	if !IsLiveSetting(config.KeyPipeBudgetPerDay) {
		t.Error("pipe_budget_per_day is not declared live")
	}
}

func TestPipeModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerPipeModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"pipe"`) {
		t.Errorf("err = %v", err)
	}
}

// pipeHub is a started default Hub with an admin and a way to connect Workers.
type pipeHub struct {
	h        *running
	hc       *hubClient
	st       *store.Store
	wsv      *workers.Service
	settings *SettingsService
	pipe     *workers.Pipe
	events   *workers.EventHub
	admin    store.UserRow
	mgr      *session.Manager
	asAdmin  map[string]string
}

func startPipeHub(t *testing.T, env map[string]string, before func(r *Registry)) *pipeHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "pipe-integration-pw-1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	r := DefaultRegistry.clone()
	if before != nil {
		before(r)
	}
	ph := &pipeHub{}
	var mgr *session.Manager
	if err := r.Register("grab", 100, func(_ context.Context, e *Env) error {
		var err error
		if ph.st, err = Require[*store.Store](e); err != nil {
			return err
		}
		if ph.wsv, err = Require[*workers.Service](e); err != nil {
			return err
		}
		if ph.settings, err = Require[*SettingsService](e); err != nil {
			return err
		}
		if ph.pipe, err = Require[*workers.Pipe](e); err != nil {
			return err
		}
		if ph.events, err = Require[*workers.EventHub](e); err != nil {
			return err
		}
		mgr, err = Require[*session.Manager](e)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ph.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	ph.mgr = mgr
	ph.hc = newHubClient(t, ph.h, "https")
	var err error
	if ph.admin, err = ph.st.UserByIdentifier(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	is, err := mgr.Create(context.Background(), ph.admin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ph.asAdmin = asSession(is.Token, is.CSRFToken)
	return ph
}

// pipeWorker is a Worker connected to the Hub over the real handshake that
// plays the Worker's op server: every write op is recorded and delivered.
type pipeWorker struct {
	id   string
	name string
	key  *identity.Key
	sess *transport.Session
	mu   sync.Mutex
	ops  []workers.OpRequest
	// answer is what the Worker answers a pipe_send with (the zero value is a delivery); a test sets it with setAnswer.
	answer ops.WriteResult
}

// setAnswer changes the answer of the Worker to the next pipe_send.
func (pw *pipeWorker) setAnswer(r ops.WriteResult) {
	pw.mu.Lock()
	pw.answer = r
	pw.mu.Unlock()
}

// opResult is what the Worker answers one request with: the session reads that the web view makes get a plausible agent session,
// every write gets the configured answer (a delivery by default).
func (pw *pipeWorker) opResult(rq workers.OpRequest) any {
	switch rq.Op {
	case ops.OpSessionSnapshot:
		return ops.SessionSnapshotResult{Pane: ops.PaneInfo{Session: "minion-1", ID: "%1", Kind: "agent:claude", State: ops.PaneStateStable},
			Screen: ops.Screen{Width: 20, Height: 1, Lines: []string{"> ready"}}}
	case ops.OpSessionsList:
		return ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-1", Task: "t"}}}
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.answer.Outcome == "" {
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}
	}
	return pw.answer
}

func (ph *pipeHub) connect(t *testing.T, name string) *pipeWorker {
	t.Helper()
	ctx := context.Background()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: key.Public(), Name: name, OwnerID: ph.admin.ID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := ph.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.Dial(ctx, "https://"+ph.h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: key, Name: name, Ops: []string{ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait, ops.OpSessionSnapshot, ops.OpSessionsList}, Features: []string{proto.FeaturePipeCausal, proto.FeatureTextVerbatim}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: ph.wsv.Identity().Public()}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit); err != nil { // welcome
		t.Fatal(err)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	pw := &pipeWorker{id: w.ID, name: name, key: key, sess: sess}
	go func() {
		for {
			st, err := sess.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				// The Hub's internal share probe (not an op): this Worker declares
				// the rule all, revision 1, with its snapshot, and answers the same.
				if typ, _ := proto.FrameType(body); typ == proto.TypeShareProbe {
					_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult,
						"result": ops.ShareProbe{Rule: ops.ShareRule{Mode: ops.ShareModeAll}, Revision: "1", OpBarrier: "0"}})
					_ = st.Close()
					return
				}
				var rq workers.OpRequest
				if json.Unmarshal(body, &rq) != nil {
					return
				}
				pw.mu.Lock()
				pw.ops = append(pw.ops, rq)
				pw.mu.Unlock()
				_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": pw.opResult(rq)})
				_ = st.Close()
			}()
		}
	}()
	waitFor(t, name+" online", func() bool { return ph.wsv.Registry().IsOnline(w.ID) })
	return pw
}

// connectShared connects a Worker that, as a Worker of this version does, declares its share rule (every session, the
// session minion-1 listed) on its event stream, and waits until the Hub holds that rule: the Hub shows the stored history of a
// session only while the rule it holds shares it. The stream stays open until the test ends.
func (ph *pipeHub) connectShared(t *testing.T, name string) *pipeWorker {
	t.Helper()
	pw := ph.connect(t, name)
	ph.declareShare(t, pw, map[string]any{"mode": "all"}, 1)
	return pw
}

// declareShare opens an event stream of the Worker and declares rule at revision rev with a snapshot that lists minion-1.
func (ph *pipeHub) declareShare(t *testing.T, pw *pipeWorker, rule map[string]any, rev int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evs, err := pw.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { evs.Reset() })
	if err := evs.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
		{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"}},
		"share": map[string]any{"rule": rule, "revision": rev}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, pw.name+" share rule held", func() bool { return !ph.events.Fenced(pw.id, "minion-1") })
}

func (pw *pipeWorker) requests() []workers.OpRequest {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return append([]workers.OpRequest(nil), pw.ops...)
}

// call is `mn pipe_send` on a Worker host: one call stream, one op.request.
func (pw *pipeWorker) call(t *testing.T, op string, args map[string]any) (typ string, body json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := pw.sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	if err := st.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": ops.NewUUIDv7(), "args": args}); err != nil {
		t.Fatal(err)
	}
	for {
		b, err := st.ReadFrame()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		typ, _ = proto.FrameType(b)
		if typ != proto.TypeOpProgress {
			return typ, b
		}
	}
}

// emit sends one turn that answers the delivery msg (the message_id of a
// pipe_send) on a turn channel of its own and returns the ack.
func (pw *pipeWorker) emit(t *testing.T, session string, msg int64, text string) workers.TurnAckFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := pw.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	id, _ := pipe.NewTurnID(time.Now())
	for _, f := range []any{
		map[string]string{"type": proto.TypeTurnsOpen},
		map[string]any{"type": proto.TypeTurn, "turn_id": id, "session": session, "message_id": msg, "text": text, "time": ops.Timestamp(time.Now())},
	} {
		if err := st.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := st.ReadFrame()
	var ack workers.TurnAckFrame
	if err != nil || json.Unmarshal(b, &ack) != nil || ack.TurnID != id || ack.Type != proto.TypeTurnAck {
		t.Fatalf("turn answer = %s, %v", b, err)
	}
	return ack
}

func (ph *pipeHub) rest(t *testing.T, op string, body map[string]any) (int, string) {
	t.Helper()
	code, out, _ := ph.hc.do("POST", "/api/v1/ops/"+op, body, ph.asAdmin)
	return code, out
}

// The default Hub serves REST pipe ops, the Worker's call streams and the turn
// channel through the one broker, next to the regular events and worker_retire
// on the same sessions.
func TestDefaultHubServesRESTCallStreamsAndTurnsThroughOneBroker(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	ctx := context.Background()
	pi1, pi2 := ph.connect(t, "pi-1"), ph.connect(t, "pi-2")
	peer := pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}

	// The regular event stream of pi-1 still works.
	evs, err := pi1.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The Worker declares its share rule with the snapshot, as a Worker of this
	// version does: the Hub shows the stored history of a session only while the
	// rule it holds shares it, and not while the rule is unknown.
	if err := evs.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
		{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"}},
		"share": map[string]any{"rule": map[string]any{"mode": "all"}, "revision": 1}}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := ph.hc.do("GET", api.SessionsPath, nil, ph.asAdmin)
	waitFor(t, "the ingested session", func() bool {
		_, body, _ = ph.hc.do("GET", api.SessionsPath, nil, ph.asAdmin)
		return strings.Contains(body, "minion-1")
	})
	_ = code

	// 1. REST pipe_send reaches the broker (a history row, the budget) and the Worker.
	rid := ops.NewUUIDv7()
	code, body = ph.rest(t, "pipe_send", map[string]any{"request_id": rid, "target": "pi-1:minion-1", "text": "please run the tests"})
	if code != 200 || !strings.Contains(body, `"outcome":"delivered"`) {
		t.Fatalf("REST pipe_send = %d %s", code, body)
	}
	if reqs := pi1.requests(); len(reqs) != 1 || reqs[0].Op != ops.OpPipeSend {
		t.Fatalf("pi-1 received %+v", reqs)
	}
	msgs, _ := ph.st.Pipe().Messages(ctx, peer, 0, 0)
	if len(msgs) != 1 || msgs[0].Text != "please run the tests" {
		t.Fatalf("history = %+v: the send did not pass through the broker", msgs)
	}
	// The Hub names the stored message to the Worker: the reply carries it.
	var routed ops.PipeSendArgs
	if err := json.Unmarshal(pi1.requests()[0].Args, &routed); err != nil || routed.MessageID != msgs[0].ID {
		t.Fatalf("the Worker was sent message_id %d (%v), want %d", routed.MessageID, err, msgs[0].ID)
	}
	// A retry with the same request_id is the same send: no second write, no second row.
	if code, body = ph.rest(t, "pipe_send", map[string]any{"request_id": rid, "target": "pi-1:minion-1", "text": "please run the tests"}); code != 200 {
		t.Fatalf("retry = %d %s", code, body)
	}
	if len(pi1.requests()) != 1 {
		t.Fatalf("the retry reached the Worker again: %+v", pi1.requests())
	}

	// 2. Turn ingestion and its ack, filed under the authenticated Worker.
	if ack := pi1.emit(t, "minion-1", 0, "unsolicited"); ack.Status != pipe.AckRejected {
		t.Fatalf("a turn that answers nothing: ack = %+v, want rejected", ack)
	}
	if ack := pi1.emit(t, "minion-1", msgs[0].ID, "all tests pass"); ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	if turns, _ := ph.st.Pipe().Turns(ctx, peer, "", 0); len(turns) != 1 || turns[0].Text != "all tests pass" {
		t.Fatalf("turns = %+v", turns)
	}
	code, body = ph.rest(t, "pipe_read", map[string]any{"target": "pi-1:minion-1"})
	if code != 200 || !strings.Contains(body, "all tests pass") || !strings.Contains(body, "please run the tests") {
		t.Fatalf("REST pipe_read = %d %s", code, body)
	}

	// 3. REST pipe_wait streams the next turn.
	type waited struct {
		code int
		body string
	}
	got := make(chan waited, 1)
	go func() {
		c, b := ph.rest(t, "pipe_wait", map[string]any{"target": "pi-1:minion-1", "timeout_ms": 20000})
		got <- waited{c, b}
	}()
	waitFor(t, "the turn after the wait started", func() bool {
		// Each attempt answers a delivery of its own: a delivery is answered once.
		m, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: "again", At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		pi1.emit(t, "minion-1", m.ID, "second answer")
		select {
		case w := <-got:
			got <- w
			return true
		case <-time.After(200 * time.Millisecond):
			return false
		}
	})
	if w := <-got; w.code != 200 || !strings.Contains(w.body, "second answer") {
		t.Fatalf("REST pipe_wait = %d %s", w.code, w.body)
	}

	// 4. A Worker's call stream. F-01: the Worker identity of pi-2 cannot reach
	// a session of pi-1, a Worker of the same owner; pi-1 reaches its own.
	typ, b := pi2.call(t, ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "from pi-2"})
	if typ != proto.TypeError || !strings.Contains(string(b), string(proto.CodeTargetNotFound)) {
		t.Fatalf("keyless cross-Worker pipe_send on a call stream = %s", b)
	}
	typ, b = pi1.call(t, ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "from pi-1 itself"})
	if typ != proto.TypeOpResult || !strings.Contains(string(b), "delivered") {
		t.Fatalf("pipe_send on a call stream = %s", b)
	}
	if msgs, _ := ph.st.Pipe().Messages(ctx, peer, 0, 0); len(msgs) == 0 || msgs[len(msgs)-1].Text != "from pi-1 itself" {
		t.Fatalf("history = %+v: the call stream did not pass through the broker", msgs)
	}
	typ, b = pi2.call(t, ops.OpPipeRead, map[string]any{"target": "pi-1:minion-1"})
	if typ != proto.TypeError {
		t.Fatalf("keyless cross-Worker pipe_read on a call stream = %s", b)
	}
	typ, b = pi1.call(t, ops.OpPipeRead, map[string]any{"target": "pi-1:minion-1"})
	if typ != proto.TypeOpResult || !strings.Contains(string(b), "from pi-1 itself") {
		t.Fatalf("pipe_read on a call stream = %s", b)
	}
	// What the Hub does not serve on a call stream is answered, not reset.
	if typ, b = pi2.call(t, ops.OpSendText, map[string]any{}); typ != proto.TypeError || !strings.Contains(string(b), string(proto.CodeUnsupportedOp)) {
		t.Fatalf("send_text on a call stream = %s", b)
	}

	// 5. worker_retire on the same session, with the events and the pipe live.
	typ, b = pi2.call(t, ops.OpWorkerRetire, map[string]any{"request_id": ops.NewUUIDv7(), "force": false})
	if typ != proto.TypeOpResult {
		t.Fatalf("worker_retire = %s", b)
	}
	if _, err := ph.st.WorkerByID(ctx, pi2.id); err == nil {
		t.Fatal("pi-2 survived its retirement")
	}
}

// The task budget is the real setting: the environment value limits sends.
func TestDefaultHubUsesThePipeBudgetSettingFromTheEnvironment(t *testing.T) {
	ph := startPipeHub(t, map[string]string{"MINION_PIPE_BUDGET_PER_DAY": "2"}, nil)
	ph.connect(t, "pi-1")
	for i := 0; i < 2; i++ {
		code, body := ph.rest(t, "pipe_send", map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "hi"})
		if code != 200 {
			t.Fatalf("send %d = %d %s", i, code, body)
		}
		time.Sleep(1100 * time.Millisecond) // the rate limit of the pipe is 1 per second after the burst
	}
	code, body := ph.rest(t, "pipe_send", map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "hi"})
	if code == 200 || !strings.Contains(body, string(proto.CodePipeBudgetExceeded)) {
		t.Fatalf("third send = %d %s, want pipe_budget_exceeded", code, body)
	}
}

// A value stored in the panel applies to the very next send (live setting).
func TestDefaultHubAppliesAPanelChangeOfThePipeBudgetAtOnce(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	ph.connect(t, "pi-1")
	send := func() (int, string) {
		return ph.rest(t, "pipe_send", map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "hi"})
	}
	if code, body := send(); code != 200 {
		t.Fatalf("first send = %d %s", code, body)
	}
	if _, err := ph.settings.Set(context.Background(), SettingsActor{ID: ph.admin.ID},
		ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyPipeBudgetPerDay, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	if code, body := send(); code == 200 || !strings.Contains(body, string(proto.CodePipeBudgetExceeded)) {
		t.Fatalf("second send after budget 1 = %d %s", code, body)
	}
}

// The retention loop starts with the Hub and stops with it: an expired
// message is pruned while the Hub runs, and Run returns after the stop.
func TestPipeRetentionLoopRunsWithTheHubAndStopsWithIt(t *testing.T) {
	saved := pipePruneInterval
	pipePruneInterval = 30 * time.Millisecond
	t.Cleanup(func() { pipePruneInterval = saved })
	ph := startPipeHub(t, nil, nil)
	ctx := context.Background()
	peer := pipe.Peer{WorkerID: store.NewID(), SessionName: "minion-1"}
	if _, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: "old", At: time.Now().Add(-pipe.Retention - time.Hour)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the expired message to be pruned", func() bool {
		msgs, _ := ph.st.Pipe().Messages(ctx, peer, 0, 0)
		return len(msgs) == 0
	})
	ph.h.stop(t) // Run returns without error: the loop was stopped and waited for
}

// Two owners of one op do not silently replace each other: a Hub whose call
// loop already serves pipe_send does not start.
func TestPipeModuleRefusesAnOpThatIsAlreadyServed(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "pipe-integration-pw-1")
	r := DefaultRegistry.clone()
	if err := r.Register("squatter", pipeModuleOrder-1, func(_ context.Context, e *Env) error {
		svc, err := Require[*workers.Service](e)
		if err != nil {
			return err
		}
		svc.HandleCallOp(ops.OpPipeSend, func(context.Context, workers.Call) {})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}, r)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "pipe") || !strings.Contains(err.Error(), ops.OpPipeSend) {
		t.Fatalf("Run = %v, want the pipe module to refuse %s", err, ops.OpPipeSend)
	}
}
