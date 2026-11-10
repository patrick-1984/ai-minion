package workers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Stream serving of the pipe: one accept loop per session (Story 4.7), the call
// streams of the pipe ops through Handler.HandleCallOp, the turn channel through
// Ingest.OnStream. The Hub side is composed like the production Hub.

// readFails reads until the stream ends: it may deliver a last frame (the error
// an interrupted op answers with) but must then end, within the deadline.
func readFails(t *testing.T, name string, st *transport.Stream, within time.Duration) {
	t.Helper()
	_ = st.SetReadDeadline(time.Now().Add(within))
	for i := 0; i < 3; i++ {
		if _, err := st.ReadFrame(); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
				t.Errorf("%s: the stream was not interrupted within %v: %v", name, within, err)
			}
			return
		}
	}
	t.Errorf("%s: the stream goes on delivering frames", name)
}

// The server's context ends while an event stream, a turn channel, a call
// stream without a request and a running pipe_wait are all idle: the handlers
// are interrupted first and the accept loop returns, long before any stream
// deadline, and nothing keeps running.
func TestServerCancellationInterruptsIdleEventTurnAndCallStreamsBeforeWaiting(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ev, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(snapFrame(0)); err != nil {
		t.Fatal(err)
	}
	turns, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := turns.WriteFrame(map[string]string{"type": FrameTurnsOpen}); err != nil {
		t.Fatal(err)
	}
	idleCall, err := ww.sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := ww.sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := waiting.WriteFrame(request(ops.OpPipeWait, map[string]any{"target": "pi-1:minion-1", "timeout_ms": 120000}, "")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event stream to be served", func() bool {
		s, ok := w.events.Sessions(wAlicePi1.ID)
		return ok && s.Online
	})
	time.Sleep(150 * time.Millisecond) // every Hub side is reading now
	if streamGoroutines() < 4 {
		t.Fatalf("only %d stream goroutines run: the probe is blind", streamGoroutines())
	}

	start := time.Now()
	w.cancel()
	select {
	case <-ww.served:
	case <-time.After(3 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("the accept loop of the session did not return: it waits for handlers that are blocked in a read\n%s", buf[:n])
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the session was served for %v after cancellation", d)
	}
	for name, st := range map[string]*transport.Stream{"events": ev, "turn channel": turns, "idle call": idleCall, "pipe_wait": waiting} {
		readFails(t, name, st, 3*time.Second)
	}
	waitNoStreamGoroutines(t)
}

// An ev stream that never sends its first frame is reset after the first-frame
// timeout; the event stream and the turn channel of the same session keep working.
func TestASilentEventStreamIsResetAndOthersKeepWorking(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	silent, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A turn channel opened meanwhile is not held up by it, and the event
	// stream that follows is served too, in the order of the accept.
	if ack := ww.emit(t, "minion-1", "still works", w.clock.Now()); ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	ev, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event stream behind the silent one to be applied", func() bool {
		s, ok := w.events.Sessions(wAlicePi1.ID)
		return ok && len(s.Sessions) == 1
	})
	readFails(t, "silent stream", silent, 5*time.Second)
	// An ev stream with an unknown first frame is abandoned as well.
	odd, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := odd.WriteFrame(map[string]string{"type": "what"}); err != nil {
		t.Fatal(err)
	}
	readFails(t, "unknown first frame", odd, 5*time.Second)
}

// A turn channel is a stream of its own: opening it does not replace the event
// stream of the connection (a new event stream does), and the events go on.
func TestTurnChannelDoesNotReplaceTheEventStream(t *testing.T) {
	w := newWorld(t, wAlicePi1)
	ww := w.workers[wAlicePi1.ID]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub := w.events.Subscribe(64)
	ev, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the snapshot", func() bool {
		s, _ := w.events.Sessions(wAlicePi1.ID)
		return len(s.Sessions) == 1
	})
	for i := 0; i < 3; i++ {
		if ack := ww.emit(t, "minion-1", "reply", w.clock.Now().Add(time.Duration(i)*time.Millisecond)); ack.Status != pipe.AckStored {
			t.Fatalf("ack = %+v", ack)
		}
	}
	// The event stream still feeds the hub: a state change after the turns.
	if err := ev.WriteFrame(evFrame(1, EventStateChanged, "minion-1", map[string]any{"kind": "agent:claude", "state": "busy", "task": "t"})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event after the turn channels", func() bool {
		s, _ := w.events.Sessions(wAlicePi1.ID)
		return len(s.Sessions) == 1 && s.Sessions[0].State == "busy"
	})
	_ = sub
}

// One real session of the real Hub service carries everything at once: session
// events, pipe calls (Worker identity), the turn channel and, at the end,
// worker_retire. Over and over, with a freshly enrolled Worker each round: no
// stream is stolen by a second reader, nothing is left running.
func TestRealSessionCarriesEventsPipeCallsTurnsAndRetirement(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, err := NewIngest(IngestOptions{Hub: events})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Registry().OnAdd(ing.Attach)

	rt := &scriptRouter{fn: func(int, CallRequest) (any, error) { return deliveredResult(), nil }}
	p, err := NewPipe(PipeConfig{Store: h.st.Pipe(), Authz: allowAllDecider{}, Router: rt, Trust: trustAll{}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.RegisterTurns(ing) {
		t.Fatal("RegisterTurns")
	}
	cs := NewCallServer(CallServerConfig{})
	if err := p.RegisterCalls(cs); err != nil {
		t.Fatal(err)
	}
	if err := cs.Bind(h.svc); err != nil {
		t.Fatal(err)
	}
	sub := events.Subscribe(256)

	for round := 0; round < 8; round++ {
		key := newKey(t)
		wk := h.enrol(key, "owner-1", "pi")
		sess := h.admitted(key)
		if ev := recvEv(t, sub); ev.Type != EventWorkerOnline || ev.WorkerID != wk.ID {
			t.Fatalf("round %d: %+v", round, ev)
		}
		evSt := openEvents(t, sess)
		if err := evSt.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"})); err != nil {
			t.Fatal(err)
		}
		if ev := recvEv(t, sub); ev.Type != EventWorkerResynced {
			t.Fatalf("round %d: %+v", round, ev)
		}

		// pipe_send on a call stream, as the Worker identity of the connection.
		rq := map[string]any{"type": proto.TypeOpRequest, "op": ops.OpPipeSend, "request_id": ops.NewUUIDv7(), "args": map[string]any{
			"request_id": ops.NewUUIDv7(), "target": map[string]any{"worker_id": ops.NewUUIDv7(), "session_name": "minion-2"}, "text": "hello"}}
		if typ, body := callOnce(t, sess, rq); typ != proto.TypeOpResult || !strings.Contains(string(body), "delivered") {
			t.Fatalf("round %d: pipe_send answered %s", round, body)
		}

		// A turn on its own stream, acknowledged; the event stream goes on.
		turns := openEvents(t, sess)
		id, _ := pipe.NewTurnID(time.Now())
		for _, f := range []any{map[string]string{"type": FrameTurnsOpen},
			TurnFrame{Type: FrameTurn, TurnID: id, Session: "minion-1", MessageID: mustMessage(t, h.st.Pipe(), wk.ID, "minion-1"), Text: "reply " + wk.ID}} {
			if err := turns.WriteFrame(f); err != nil {
				t.Fatal(err)
			}
		}
		_ = turns.SetReadDeadline(time.Now().Add(5 * time.Second))
		body, err := turns.ReadFrame()
		var ack TurnAckFrame
		if err != nil || json.Unmarshal(body, &ack) != nil || ack.TurnID != id || ack.Status != pipe.AckStored {
			t.Fatalf("round %d: ack = %s, %v", round, body, err)
		}
		stored, _ := h.st.Pipe().Turns(context.Background(), pipe.Peer{WorkerID: wk.ID, SessionName: "minion-1"}, "", 0)
		if len(stored) != 1 || stored[0].TurnID != id {
			t.Fatalf("round %d: stored turns = %+v: the turn must be filed under the authenticated Worker", round, stored)
		}
		if err := evSt.WriteFrame(evFrame(1, EventStateChanged, "minion-1", map[string]any{"kind": "agent:claude", "state": "busy", "task": "t"})); err != nil {
			t.Fatal(err)
		}
		if ev := recvEv(t, sub); ev.Type != EventStateChanged || ev.State != "busy" || ev.WorkerID != wk.ID {
			t.Fatalf("round %d: %+v", round, ev)
		}

		// worker_retire is still served on the same session, by the same loop.
		if typ, body := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
			t.Fatalf("round %d: worker_retire answered %s", round, body)
		}
		_ = sess.Close()
		eventually(t, "the Worker to go offline", func() bool {
			s, _ := events.Sessions(wk.ID)
			return !s.Online && !h.svc.Registry().IsOnline(wk.ID)
		})
		for drain := true; drain; {
			select {
			case <-sub.C():
			case <-time.After(20 * time.Millisecond):
				drain = false
			}
		}
	}
	if rt.count() != 8 {
		t.Errorf("%d pipe_send writes reached the router, want 8", rt.count())
	}
	waitNoStreamGoroutines(t)
}

// The Hub stops while a real session has idle event, turn and call streams:
// every one is interrupted at once and nothing is left running.
func TestHubShutdownInterruptsIdlePipeStreamsOfARealSession(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: events})
	h.svc.Registry().OnAdd(ing.Attach)
	rt := &scriptRouter{fn: func(int, CallRequest) (any, error) { return deliveredResult(), nil }}
	p, err := NewPipe(PipeConfig{Store: h.st.Pipe(), Authz: allowAllDecider{}, Router: rt, Trust: trustAll{}})
	if err != nil {
		t.Fatal(err)
	}
	p.RegisterTurns(ing)
	cs := NewCallServer(CallServerConfig{})
	if err := p.RegisterCalls(cs); err != nil {
		t.Fatal(err)
	}
	if err := cs.Bind(h.svc); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	wk := h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evSt := openEvents(t, sess)
	_ = evSt.WriteFrame(snapFrame(0))
	turns := openEvents(t, sess)
	_ = turns.WriteFrame(map[string]string{"type": FrameTurnsOpen})
	idle, err := sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = waiting.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": ops.OpPipeWait, "request_id": ops.NewUUIDv7(),
		"args": map[string]any{"target": map[string]any{"worker_id": wk.ID, "session_name": "minion-1"}, "timeout_ms": 120000}})
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(wk.ID) })
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	h.svc.Close()
	for name, st := range map[string]*transport.Stream{"events": evSt, "turn channel": turns, "idle call": idle, "pipe_wait": waiting} {
		readFails(t, name, st, 5*time.Second)
	}
	if d := time.Since(start); d > callTimeout/2 {
		t.Errorf("idle streams ended after %v, want much less than the call timeout %v", d, callTimeout)
	}
	waitNoStreamGoroutines(t)
}

// --- small fakes for the real-service tests ---

type allowAllDecider struct{}

func (allowAllDecider) Decide(context.Context, authz.Principal, ops.ActionSet, authz.Resource) error {
	return nil
}

type trustAll struct{}

func (trustAll) Trusted(context.Context, string, string, string) (bool, error) { return true, nil }
func (trustAll) Owner(context.Context, string) (string, error)                 { return "owner-1", nil }
