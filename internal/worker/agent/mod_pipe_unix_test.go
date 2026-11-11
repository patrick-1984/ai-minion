//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// pipeRuntime is the Worker's real module chain (core, pipe, connector, op
// server, hub pipe, local socket) over a fake terminal host and a dialer that
// hands out in-memory Hub sessions.
type pipeRuntime struct {
	rt     *Runtime
	host   *anchorHost
	dialer *pairDialer
	client *client.LocalBackend
	// runDir is the runtime directory that holds the local socket; the default
	// socket lookup of the CLI finds it through XDG_RUNTIME_DIR and TMPDIR.
	runDir string
	sock   string
}

func startPipeRuntime(t *testing.T, dir string, host *anchorHost, cls *fakeClassifier, enrolled bool, share string) *pipeRuntime {
	t.Helper()
	return startPipeRuntimeWithCallKey(t, dir, host, cls, enrolled, share, "")
}

// startPipeRuntimeWithCallKey is startPipeRuntime with the Worker setting
// call_api_key_file.
func startPipeRuntimeWithCallKey(t *testing.T, dir string, host *anchorHost, cls *fakeClassifier, enrolled bool, share, callKeyFile string) *pipeRuntime {
	t.Helper()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: cls,
	}
	dialer := &pairDialer{hubs: make(chan *transport.Session, 4)}
	runDir := shortRunDir(t)
	sock := filepath.Join(runDir, "minion", localapi.SocketName)
	lad := defaultLocalAPIDeps()
	lad.socketPath = func() (string, error) { return sock, nil }
	r := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.Register(ModuleCore, OrderCore, core.start))
	must(r.Register(ModulePipe, OrderPipe, pipeDeps{poll: 10 * time.Millisecond, settle: 1}.start))
	must(r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start))
	must(r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start))
	must(r.Register(ModuleHubPipe, OrderHubPipe, hubPipeDeps{}.start))
	must(r.Register(ModuleLocalAPI, OrderLocalAPI, lad.start))
	env := map[string]string{"MINION_SESSION_POLL_INTERVAL": "100ms"}
	if enrolled {
		env["MINION_SERVER"] = "https://hub.example"
	}
	rt := NewRuntime(r, Options{StopTimeout: 5 * time.Second})
	err := rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w", ShareExisting: share, CallAPIKeyFile: callKeyFile},
		Resolver:   &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return &pipeRuntime{rt: rt, host: host, dialer: dialer, client: client.Local(client.WithSocketPath(sock)), runDir: runDir, sock: sock}
}

func newAnchorHost(t *testing.T) (*anchorHost, *fakeClassifier) {
	t.Helper()
	ah := &anchorHost{fakeHost: newFakeHost()}
	ah.addSession("work", "claude")
	ah.setView(claudeView(nil, ""))
	cls := newFakeClassifier()
	cls.set("claude", "agent:claude", ops.PaneStateStable)
	return ah, cls
}

func enrolledDir(t *testing.T) (string, *identity.Key) {
	t.Helper()
	dir := t.TempDir()
	hubKey, _ := identity.GenerateKey()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enrolledState(t, hubKey, false)(dir, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, hubKey
}

// Solo on the real chain: the local socket serves pipe_send, pipe_read and
// pipe_wait through the module's broker over the Worker store.
func TestPipeModuleSoloOverTheLocalSocket(t *testing.T) {
	ah, cls := newAnchorHost(t)
	w := startPipeRuntime(t, t.TempDir(), ah, cls, false, config.ShareExistingAll)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	args := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Text: "ping"}
	raw, err := w.client.Call(ctx, ops.OpPipeSend, args)
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pipe_send = %s (%v)", raw, err)
	}
	raw, err = w.client.Call(ctx, ops.OpPipeRead, ops.PipeReadArgs{Target: target("work")})
	var read ops.PipeReadResult
	if err != nil || json.Unmarshal(raw, &read) != nil || len(read.Turns) != 1 || read.Turns[0].Direction != ops.PipeSent {
		t.Fatalf("pipe_read = %s (%v)", raw, err)
	}

	// The agent answers while pipe_wait streams over the socket.
	go func() {
		time.Sleep(100 * time.Millisecond)
		ah.setView(claudeView([]string{"> ping", "", "● pong"}, ""))
	}()
	var got []ops.PipeTurn
	_, err = w.client.Stream(ctx, ops.OpPipeWait, ops.PipeWaitArgs{Target: target("work"), AfterTurnID: read.Turns[0].TurnID, TimeoutMS: 20000},
		func(m json.RawMessage) error {
			var pt ops.PipeTurn
			if err := json.Unmarshal(m, &pt); err != nil {
				return err
			}
			got = append(got, pt)
			return nil
		})
	if err != nil || len(got) != 1 || got[0].Text != "pong" || got[0].Direction != ops.PipeReceived {
		t.Fatalf("pipe_wait = %+v, %v", got, err)
	}
	if _, ok := Lookup[*PipeService](w.rt); !ok {
		t.Fatal("the pipe module provides no service")
	}
}

// Hub mode on the real chain: a Hub-origin write is only delivered locally,
// its reply travels to the Hub through the durable outbox, survives a restart
// of the Worker until the Hub acknowledges it and is not sent after the
// session stopped being shared; a local call goes to the Hub on a call stream.
func TestPipeModuleHubModeEndToEnd(t *testing.T) {
	dir, _ := enrolledDir(t)
	ah, cls := newAnchorHost(t)
	w := startPipeRuntime(t, dir, ah, cls, true, config.ShareExistingAll)

	hub := <-w.dialer.hubs
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A Hub-origin pipe_send on an op stream: local delivery only.
	opst, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rid := ops.NewUUIDv7()
	frame := opRequest(ops.OpPipeSend, []ops.Action{ops.ActionMessage}, ops.PipeSendArgs{
		WriteArgs: ops.WriteArgs{RequestID: rid}, Target: target("work"), Text: "from the hub", MessageID: 42,
	})
	frame["request_id"] = rid
	if err := opst.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	body, ok := readHubFrame(t, opst, 10*time.Second)
	var ans struct {
		Type   string          `json:"type"`
		Result ops.WriteResult `json:"result"`
	}
	if !ok || json.Unmarshal(body, &ans) != nil || ans.Type != proto.TypeOpResult || ans.Result.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pipe_send answer = %s", body)
	}
	if got := ah.pastes(); len(got) != 1 || got[0].Text != "from the hub" || len(ah.keys()) != 1 {
		t.Fatalf("pastes %v keys %v", got, ah.keys())
	}
	st, _ := Lookup[*store.Store](w.rt)
	if es, _ := st.Entries(ctx, soloPeer("work"), "", 0); len(es) != 0 {
		t.Fatalf("the Worker stored a message of the Hub's pipe: %+v", es)
	}

	// The agent answers: the turn reaches the Hub on the turn channel.
	ah.setView(claudeView([]string{"> from the hub", "", "● pong"}, ""))
	ts := acceptTurnChannel(t, hub)
	body, ok = readHubFrame(t, ts, 10*time.Second)
	var tf struct {
		Type    string `json:"type"`
		TurnID  string `json:"turn_id"`
		Session string `json:"session"`
		// The id the Hub gave the delivery: the reply names it.
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
	}
	if !ok || json.Unmarshal(body, &tf) != nil || tf.Type != proto.TypeTurn || tf.Session != "work" || tf.Text != "pong" || !pipe.IsTurnID(tf.TurnID) || tf.MessageID != 42 {
		t.Fatalf("turn frame = %s", body)
	}
	if es, _ := st.Entries(ctx, soloPeer("work"), "", 0); len(es) != 0 {
		t.Fatalf("a Hub reply was stored on the Worker: %+v", es)
	}
	if p, _ := st.OutboxPending(ctx); len(p) != 1 || p[0].TurnID != tf.TurnID {
		t.Fatalf("outbox before the ack = %+v", p)
	}

	// A local call in Hub mode is forwarded to the Hub on a call stream.
	type callRes struct {
		raw json.RawMessage
		err error
	}
	done := make(chan callRes, 1)
	go func() {
		raw, err := w.client.Call(ctx, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Text: "local to hub"})
		done <- callRes{raw, err}
	}()
	for {
		cst, err := hub.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cst.Kind() != transport.KindCall {
			continue
		}
		body, ok := readHubFrame(t, cst, 10*time.Second)
		var rq struct {
			Op   string `json:"op"`
			Args struct {
				Text string `json:"text"`
			} `json:"args"`
		}
		if !ok || json.Unmarshal(body, &rq) != nil || rq.Op != ops.OpPipeSend || rq.Args.Text != "local to hub" {
			t.Fatalf("call request = %s", body)
		}
		if err := cst.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: []ops.Warning{{Code: "from_hub", Message: "hub says hi"}}}}); err != nil {
			t.Fatal(err)
		}
		_ = cst.Close()
		break
	}
	r := <-done
	var cr ops.WriteResult
	if r.err != nil || json.Unmarshal(r.raw, &cr) != nil || cr.Outcome != ops.OutcomeDelivered || len(cr.Warnings) != 1 || cr.Warnings[0].Code != "from_hub" {
		t.Fatalf("local call = %s, %v", r.raw, r.err)
	}
	if len(ah.pastes()) != 1 {
		t.Fatalf("a local call in Hub mode wrote into the local pane: %v", ah.pastes())
	}

	// Restart without an acknowledgement: the same turn comes again.
	if err := w.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	w2 := startPipeRuntime(t, dir, ah, cls, true, config.ShareExistingAll)
	hub2 := <-w2.dialer.hubs
	ts2 := acceptTurnChannel(t, hub2)
	body, ok = readHubFrame(t, ts2, 10*time.Second)
	var tf2 struct {
		TurnID string `json:"turn_id"`
		Text   string `json:"text"`
	}
	if !ok || json.Unmarshal(body, &tf2) != nil || tf2.TurnID != tf.TurnID || tf2.Text != "pong" {
		t.Fatalf("after the restart the turn frame = %s, want turn %s again", body, tf.TurnID)
	}
	if err := ts2.WriteFrame(map[string]any{"type": proto.TypeTurnAck, "turn_id": tf.TurnID, "status": "stored"}); err != nil {
		t.Fatal(err)
	}
	st2, _ := Lookup[*store.Store](w2.rt)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, _ := st2.OutboxPending(ctx); len(p) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the acknowledged turn is still in the outbox")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A reply whose session is no longer shared when the Hub link comes back is
// dropped from the outbox and never sent.
func TestPipeModuleHubModeKeepsAnUnsharedSessionsReplyOnTheWorker(t *testing.T) {
	dir, _ := enrolledDir(t)
	ah, cls := newAnchorHost(t)
	w := startPipeRuntime(t, dir, ah, cls, true, config.ShareExistingAll)
	hub := <-w.dialer.hubs
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opst, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rid := ops.NewUUIDv7()
	frame := opRequest(ops.OpPipeSend, []ops.Action{ops.ActionMessage}, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: rid}, Target: target("work"), Text: "hello", MessageID: 43})
	frame["request_id"] = rid
	if err := opst.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	if body, ok := readHubFrame(t, opst, 10*time.Second); !ok {
		t.Fatalf("no answer: %s", body)
	}
	ah.setView(claudeView([]string{"> hello", "", "● secret reply"}, ""))
	st, _ := Lookup[*store.Store](w.rt)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p, _ := st.OutboxPending(ctx); len(p) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reply never reached the outbox")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = hub.Close()
	if err := w.rt.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The owner stops sharing the session while the Worker is down
	// (share_existing=none at the next start).
	w2 := startPipeRuntime(t, dir, ah, cls, true, config.ShareExistingNone)
	st2, _ := Lookup[*store.Store](w2.rt)
	hub2 := <-w2.dialer.hubs
	go func() { // no turn channel frame may arrive
		for {
			st, err := hub2.Accept(ctx)
			if err != nil {
				return
			}
			if st.Kind() != transport.KindEvents {
				continue
			}
			if body, ok := readHubFrame(t, st, 2*time.Second); ok {
				if typ, _ := proto.FrameType(body); typ == proto.TypeTurn {
					t.Errorf("an unshared session's reply was sent: %s", body)
				}
			}
		}
	}()
	deadline = time.Now().Add(10 * time.Second)
	for {
		if p, _ := st2.OutboxPending(ctx); len(p) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn of an unshared session stayed in the outbox")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveLocalPipe serves the executor and the pipe service on a real local
// socket through the real local socket module, and returns a client on it.
func serveLocalPipe(t *testing.T, ex *Executor, svc *PipeService, st *store.Store) *client.LocalBackend {
	t.Helper()
	runDir := shortRunDir(t)
	sock := filepath.Join(runDir, "minion", localapi.SocketName)
	lad := defaultLocalAPIDeps()
	lad.socketPath = func() (string, error) { return sock, nil }
	r := NewRegistry()
	if err := r.Register("fakecore", OrderCore, func(_ context.Context, env *Env) (StopFunc, error) {
		for _, p := range []func() error{
			func() error { return Provide(env, ex) },
			func() error { return Provide(env, svc) },
			func() error { return Provide(env, st) },
		} {
			if err := p(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ModuleLocalAPI, OrderLocalAPI, lad.start); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(r, Options{StopTimeout: 5 * time.Second})
	if err := rt.Start(context.Background(), testSettings()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	return client.Local(client.WithSocketPath(sock))
}

// foreignWorker is the id of a Worker that is not this one.
const foreignWorker = "01890a5d-ac96-774b-bcce-b302099a8057"

// Solo on the real chain: a target that names a Worker is refused by the pipe
// service for all three ops, before anything is stored, written or read; a target
// without one still works. The CLI no longer decides this for the pipe ops (it
// cannot tell Solo from Hub mode), so the Worker has to.
func TestPipeModuleSoloRefusesATargetThatNamesAWorker(t *testing.T) {
	ah, cls := newAnchorHost(t)
	w := startPipeRuntime(t, t.TempDir(), ah, cls, false, config.ShareExistingAll)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	foreign := ops.Target{WorkerID: foreignWorker, SessionName: "work"}

	_, err := w.client.Call(ctx, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: foreign, Text: "leaked-to-local"})
	wantAPIErrorCode(t, err, proto.CodeInvalidArgument)
	_, err = w.client.Call(ctx, ops.OpPipeRead, ops.PipeReadArgs{Target: foreign})
	wantAPIErrorCode(t, err, proto.CodeInvalidArgument)
	_, err = w.client.Stream(ctx, ops.OpPipeWait, ops.PipeWaitArgs{Target: foreign, TimeoutMS: 100}, func(json.RawMessage) error { return nil })
	wantAPIErrorCode(t, err, proto.CodeInvalidArgument)
	if len(ah.pastes()) != 0 || len(ah.keys()) != 0 {
		t.Fatalf("a foreign target reached the local pane: pastes %v keys %v", ah.pastes(), ah.keys())
	}
	if _, err := w.client.Call(ctx, ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Text: "own session"}); err != nil {
		t.Fatalf("a target without a Worker is refused: %v", err)
	}
}

func wantAPIErrorCode(t *testing.T, err error, code proto.Code) {
	t.Helper()
	if proto.CodeOf(err) != code {
		t.Fatalf("error = %v (code %q), want %s", err, proto.CodeOf(err), code)
	}
}

// Hub mode on the real chain: the local pipe ops forward the structured target
// with its worker_id to the Hub on a call stream, as the CLI now sends it; the
// Worker does not decide, and nothing is written to the local pane.
func TestPipeModuleHubModeForwardsTheStructuredWorkerTarget(t *testing.T) {
	dir, _ := enrolledDir(t)
	ah, cls := newAnchorHost(t)
	w := startPipeRuntime(t, dir, ah, cls, true, config.ShareExistingAll)
	hub := <-w.dialer.hubs
	// The test Hub has the session as soon as the Worker dialled, but the Worker
	// hands the connection to its link handlers (the call client among them) a
	// moment later. A local call made before that fails at once with
	// worker_offline and nothing ever reaches the Hub, which this test then waited
	// for until its deadline (a flake that was reproduced at about 1 in 100 runs
	// under -race with one CPU). The call client's own readiness is the barrier.
	eventually(t, func() bool {
		cc, ok := Lookup[*CallClient](w.rt)
		return ok && cc.Connected()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	foreign := ops.Target{WorkerID: foreignWorker, SessionName: "work"}

	for _, tc := range []struct {
		op   string
		args any
	}{
		{ops.OpPipeSend, ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: foreign, Text: "to another worker"}},
		{ops.OpPipeRead, ops.PipeReadArgs{Target: foreign}},
	} {
		done := make(chan error, 1)
		go func() { _, err := w.client.Call(ctx, tc.op, tc.args); done <- err }()
		for {
			cst, err := hub.Accept(ctx)
			if err != nil {
				select {
				case cerr := <-done: // keep the real cause: the call had already ended
					t.Fatalf("%s: the local call ended with %v before it reached the Hub (accept: %v)", tc.op, cerr, err)
				default:
				}
				t.Fatal(err)
			}
			if cst.Kind() != transport.KindCall {
				continue
			}
			body, ok := readHubFrame(t, cst, 10*time.Second)
			var rq struct {
				Op   string `json:"op"`
				Args struct {
					Target ops.Target `json:"target"`
				} `json:"args"`
			}
			if !ok || json.Unmarshal(body, &rq) != nil || rq.Op != tc.op || rq.Args.Target != foreign {
				t.Fatalf("%s: the Hub got %s, want the target %+v", tc.op, body, foreign)
			}
			result := any(map[string]any{"turns": []any{}})
			if tc.op == ops.OpPipeSend {
				result = ops.WriteResult{Outcome: ops.OutcomeDelivered}
			}
			if err := cst.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": result}); err != nil {
				t.Fatal(err)
			}
			_ = cst.Close()
			break
		}
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", tc.op, err)
		}
	}
	if len(ah.pastes()) != 0 {
		t.Fatalf("a forwarded call wrote into the local pane: %v", ah.pastes())
	}
}
