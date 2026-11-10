package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestHubPipeModuleIsRegisteredAfterTheOpServer(t *testing.T) {
	names := DefaultRegistry.Names()
	op, hp := slices.Index(names, ModuleOpServer), slices.Index(names, ModuleHubPipe)
	if op < 0 || hp < 0 || hp < op {
		t.Fatalf("default modules = %v, want the hub pipe links after the op server", names)
	}
}

func TestHubPipeModuleNeedsTheConnector(t *testing.T) {
	r := NewRegistry()
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return newFakeHost(), nil },
		classifier: newFakeClassifier(),
	}
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleHubPipe, OrderHubPipe, hubPipeDeps{}.start)
	err := NewRuntime(r, Options{}).Start(context.Background(), Settings{WorkerCore: config.WorkerCore{StateDir: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "requires service") {
		t.Fatalf("err = %v", err)
	}
}

// acceptTurnChannel accepts streams of the Worker until it finds the turn
// channel (the ev stream whose first frame is turns.open); the event stream of
// the op server module is on the same link and is left alone.
func acceptTurnChannel(t *testing.T, hub *transport.Session) *transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		st, err := hub.Accept(ctx)
		if err != nil {
			t.Fatalf("no turn channel: %v", err)
		}
		if st.Kind() != transport.KindEvents {
			continue
		}
		body, ok := readHubFrame(t, st, 5*time.Second)
		if !ok {
			continue
		}
		if typ, _ := proto.FrameType(body); typ == proto.TypeTurnsOpen {
			return st
		}
	}
}

// The hub pipe links on the real module chain: the turn emitter and the call
// client are services, serve every link next to the op server and the event
// stream, send the unacknowledged turn again after a reconnect, never send an
// unshared session and forward a call to the Hub.
func TestHubPipeModuleServesTurnsAndCallsOnEveryLink(t *testing.T) {
	dir := t.TempDir()
	hubKey, _ := identity.GenerateKey()
	st0, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enrolledState(t, hubKey, false)(dir, st0)
	if err := st0.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	_ = st0.Close()

	host := newFakeHost()
	cls := newFakeClassifier()
	cls.set("claude", "agent:claude", ops.PaneStateStable)
	for _, n := range []string{"open", "secret"} {
		host.setScreen(host.addSession(n, "claude"), "content of "+n)
	}
	core := coreDeps{
		openStore:  func(ctx context.Context, d string) (*store.Store, error) { return store.OpenContext(ctx, d) },
		newHost:    func(string) (term.SessionHost, error) { return host, nil },
		classifier: cls,
	}
	dialer := &pairDialer{hubs: make(chan *transport.Session, 4)}
	r := NewRegistry()
	_ = r.Register(ModuleCore, OrderCore, core.start)
	_ = r.Register(ModuleConnector, OrderConnector, connectorDeps{dialer: dialer}.start)
	_ = r.Register(ModuleOpServer, OrderOpServer, opServerDeps{}.start)
	_ = r.Register(ModuleHubPipe, OrderHubPipe, hubPipeDeps{}.start)
	env := map[string]string{"MINION_SERVER": "https://hub.example", "MINION_SESSION_POLL_INTERVAL": "100ms"}
	rt := NewRuntime(r, Options{})
	err = rt.Start(context.Background(), Settings{
		WorkerCore: config.WorkerCore{StateDir: dir, TmuxSocket: filepath.Join(dir, "tmux.sock"), Name: "w"},
		Resolver:   &config.Resolver{Role: config.ScopeWorker, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }},
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	em, ok := Lookup[*TurnEmitter](rt)
	if !ok {
		t.Fatal("no turn emitter service")
	}
	cc, ok := Lookup[*CallClient](rt)
	if !ok {
		t.Fatal("no call client service")
	}

	// An unshared session never leaves the Worker.
	if _, err := em.Emit(PendingTurn{MessageID: 7, Session: "secret", Text: "private"}); !errors.Is(err, ErrNotShared) {
		t.Fatalf("Emit(unshared) = %v", err)
	}
	id, err := em.Emit(PendingTurn{MessageID: 7, Session: "open", Text: "the reply"})
	if err != nil {
		t.Fatal(err)
	}

	// First connection: the turn arrives; the Hub does not acknowledge it and
	// the link drops.
	hub1 := <-dialer.hubs
	ts := acceptTurnChannel(t, hub1)
	body, ok := readHubFrame(t, ts, 5*time.Second)
	var f struct {
		Type    string `json:"type"`
		TurnID  string `json:"turn_id"`
		Session string `json:"session"`
		Text    string `json:"text"`
	}
	if !ok || json.Unmarshal(body, &f) != nil || f.Type != proto.TypeTurn || f.TurnID != id || f.Session != "open" || f.Text != "the reply" {
		t.Fatalf("turn frame = %s", body)
	}
	if p, _ := em.out.Pending(); len(p) != 1 {
		t.Fatalf("pending = %+v before the ack", p)
	}
	_ = hub1.Close()

	// Second connection: the same turn_id is sent again; the ack removes it.
	var hub2 *transport.Session
	select {
	case hub2 = <-dialer.hubs:
	case <-time.After(10 * time.Second):
		t.Fatal("the Worker did not reconnect")
	}
	ts = acceptTurnChannel(t, hub2)
	body, ok = readHubFrame(t, ts, 5*time.Second)
	if !ok || json.Unmarshal(body, &f) != nil || f.TurnID != id {
		t.Fatalf("the turn was not sent again after the reconnect: %s", body)
	}
	if err := ts.WriteFrame(map[string]any{"type": proto.TypeTurnAck, "turn_id": id, "status": "stored"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, _ := em.out.Pending(); len(p) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the acknowledged turn is still in the outbox")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A call to the Hub on a call stream, as the Worker identity.
	waitConnected := time.Now().Add(5 * time.Second)
	for !cc.Connected() && time.Now().Before(waitConnected) {
		time.Sleep(10 * time.Millisecond)
	}
	got := make(chan error, 1)
	var res ops.WriteResult
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		got <- cc.Do(ctx, ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "hello"}, &res)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		cst, err := hub2.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cst.Kind() != transport.KindCall {
			continue
		}
		body, ok := readHubFrame(t, cst, 5*time.Second)
		var rq struct {
			Op     string `json:"op"`
			APIKey string `json:"api_key"`
		}
		if !ok || json.Unmarshal(body, &rq) != nil || rq.Op != ops.OpPipeSend || rq.APIKey != "" {
			t.Fatalf("call request = %s", body)
		}
		if err := cst.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}}); err != nil {
			t.Fatal(err)
		}
		_ = cst.Close()
		break
	}
	if err := <-got; err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("Do = %v, %+v", err, res)
	}
}
