package workers

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The inbound streams of a session have exactly one reader: the accept loop of
// the connect handler's stream host. These tests run the real handshake, the
// real service (default call handling, default removal) and the real event
// ingest together, the composition of the production Hub (mod_events attaches
// the ingest with Registry.OnAdd(ing.Attach)).

// streamGoroutines counts the goroutines of this package that serve streams of
// a session: the accept loop, the call handlers and the event feeds.
func streamGoroutines() int {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		for _, marker := range []string{"(*hostedSession).serve", "(*callStreams)", "(*Handler).serveCall", "workers.serveFeed", "(*Ingest).run", "(*ingestSession).serveStream", "(*Pipe).ServeTurns", "(*CallServer)"} {
			if strings.Contains(g, marker) {
				count++
				break
			}
		}
	}
	return count
}

func waitNoStreamGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for streamGoroutines() > 0 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d stream goroutines still running:\n%s", streamGoroutines(), buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func callOpRequest(op string) map[string]any {
	id := ops.NewUUIDv7()
	return map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": id, "args": map[string]any{"request_id": id}}
}

// Live session events and worker_retire on one real session, over and over,
// each round with a Worker that is enrolled again: no stream is stolen by a
// second reader.
func TestRetireAndLiveEventsShareOneSessionWithTheRealIngest(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, err := NewIngest(IngestOptions{Hub: events})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.Registry().OnAdd(ing.Attach)
	sub := events.Subscribe(256)

	for round := 0; round < 12; round++ {
		key := newKey(t)
		w := h.enrol(key, "owner-1", "pi")
		sess := h.admitted(key)
		if ev := recvEv(t, sub); ev.Type != EventWorkerOnline || ev.WorkerID != w.ID {
			t.Fatalf("round %d: %+v", round, ev)
		}
		// The streams of both kinds are open at the same time.
		evSt := openEvents(t, sess)
		if err := evSt.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"})); err != nil {
			t.Fatal(err)
		}
		if ev := recvEv(t, sub); ev.Type != EventWorkerResynced {
			t.Fatalf("round %d: %+v", round, ev)
		}
		// An ordinary call that the Hub does not serve is answered, not reset.
		if typ, body := callOnce(t, sess, callOpRequest("send_text")); typ != proto.TypeError ||
			!strings.Contains(string(body), string(proto.CodeUnsupportedOp)) {
			t.Fatalf("round %d: unsupported call answered %s", round, body)
		}
		// Events and calls interleave on the live session.
		if err := evSt.WriteFrame(evFrame(1, EventStateChanged, "minion-1", map[string]any{"kind": "agent:claude", "state": "busy", "task": "t"})); err != nil {
			t.Fatal(err)
		}
		if ev := recvEv(t, sub); ev.Type != EventStateChanged || ev.State != "busy" || ev.WorkerID != w.ID {
			t.Fatalf("round %d: %+v", round, ev)
		}
		if typ, body := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
			t.Fatalf("round %d: worker_retire answered %s", round, body)
		}
		if _, err := h.st.WorkerByPub(context.Background(), key.Public()); err == nil {
			t.Fatalf("round %d: the Worker row survived the retirement", round)
		}
		if tomb, err := h.st.TombstoneByPub(context.Background(), key.Public()); err != nil || tomb.WorkerID != w.ID || tomb.RemovedBy != "worker:"+w.ID {
			t.Fatalf("round %d: tombstone = %+v, %v", round, tomb, err)
		}
		_ = sess.Close() // the Worker closes after the answer
		eventually(t, "the Worker to go offline", func() bool {
			s, _ := events.Sessions(w.ID)
			return !s.Online && !h.svc.Registry().IsOnline(w.ID)
		})
		for drain := true; drain; { // the offline event
			select {
			case <-sub.C():
			case <-time.After(20 * time.Millisecond):
				drain = false
			}
		}
	}
	waitNoStreamGoroutines(t)
}

// The standalone service retires Workers with no events module at all.
func TestRetireWorksWithoutAnEventsModule(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	if typ, body := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
		t.Fatalf("worker_retire answered %s", body)
	}
}

// An event stream and a call stream of a session that has no event ingest are
// each reset or served by their own owner; the session is not wedged.
func TestStreamsOfAKindNobodyServesAreReset(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	evSt := openEvents(t, sess)
	_ = evSt.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := evSt.ReadFrame(); err == nil {
		t.Fatal("an ev stream nobody serves read a frame")
	}
	if typ, _ := callOnce(t, sess, retireRequest()); typ != proto.TypeOpResult {
		t.Fatal("the call after the reset ev stream was not served")
	}
}

// Idle streams of both kinds are interrupted when the session ends or the Hub
// stops, and nothing of the stream serving stays running.
func TestIdleStreamsEndWithTheSessionAndLeaveNoGoroutines(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: events})
	h.svc.Registry().OnAdd(ing.Attach)

	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idleCall, err := sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	idleEv := openEvents(t, sess)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	// The Hub side of both idle streams is reading now.
	time.Sleep(100 * time.Millisecond)
	if streamGoroutines() == 0 {
		t.Fatal("no stream goroutine found: the probe is blind")
	}

	// The Hub shuts down: the idle streams are interrupted at once, not after
	// the call timeout.
	start := time.Now()
	h.svc.Close()
	for name, st := range map[string]*transport.Stream{"call": idleCall, "ev": idleEv} {
		_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.ReadFrame(); err == nil {
			t.Errorf("idle %s stream read a frame", name)
		}
	}
	if d := time.Since(start); d > callTimeout/2 {
		t.Errorf("idle streams ended after %v, want much less than the call timeout %v", d, callTimeout)
	}
	waitNoStreamGoroutines(t)
}

// A reconnect (the registry replaces the session) ends the loop and the
// handlers of the old session and leaves the new session served.
func TestAReplacedSessionEndsItsStreamServingAndTheNewOneWorks(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: events})
	h.svc.Registry().OnAdd(ing.Attach)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	first := h.admitted(key)
	openEvents(t, first)
	eventually(t, "online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	second := h.admitted(key)
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced session stayed open")
	}
	evSt := openEvents(t, second)
	_ = evSt.WriteFrame(snapFrame(0, map[string]any{"name": "m", "kind": "agent:claude", "state": "stable", "task": "t"}))
	eventually(t, "events of the new session", func() bool {
		s, ok := events.Sessions(w.ID)
		return ok && s.Online && len(s.Sessions) == 1
	})
	if typ, body := callOnce(t, second, retireRequest()); typ != proto.TypeOpResult {
		t.Fatalf("worker_retire on the new session answered %s", body)
	}
}

// The handoff for later stories: a module joins the host for a kind, or
// registers an op on the call streams, without a second Accept loop; the kinds
// the handler owns cannot be taken.
func TestStreamHostHandoffAndCallOpRegistration(t *testing.T) {
	h := newTestHub(t, nil)
	var (
		sawHost  atomic.Bool
		taken    atomic.Bool
		replaced atomic.Bool
	)
	h.svc.Registry().OnAdd(func(_ Online, sess OpSession) {
		host, ok := HostOf(sess)
		if !ok {
			return
		}
		sawHost.Store(true)
		taken.Store(host.Handle(transport.KindCall, nil))               // nil handler
		replaced.Store(host.Handle(transport.KindCall, &callStreams{})) // the call kind is owned
	})
	if h.svc.HandleCallOp(ops.OpWorkerRetire, func(context.Context, Call) {}) {
		t.Error("worker_retire was replaced")
	}
	if h.svc.HandleCallOp("", func(context.Context, Call) {}) || h.svc.HandleCallOp("x", nil) {
		t.Error("an empty op or a nil handler was accepted")
	}
	got := make(chan Call, 1)
	if !h.svc.HandleCallOp("pipe_probe", func(_ context.Context, c Call) {
		got <- c
		_ = c.Stream.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": map[string]any{"ok": true}})
	}) {
		t.Fatal("the op was not registered")
	}
	if h.svc.HandleCallOp("pipe_probe", func(context.Context, Call) {}) {
		t.Error("an op was registered twice")
	}

	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	sess := h.admitted(key)
	if typ, body := callOnce(t, sess, callOpRequest("pipe_probe")); typ != proto.TypeOpResult {
		t.Fatalf("registered op answered %s", body)
	}
	c := <-got
	if c.Worker.ID != w.ID || c.Stream == nil {
		t.Errorf("call = %+v, want the authenticated Worker %s", c, w.ID)
	}
	var args map[string]any
	if err := json.Unmarshal(c.Args, &args); err != nil || args["request_id"] == nil {
		t.Errorf("args = %s", c.Args)
	}
	if !sawHost.Load() || taken.Load() || replaced.Load() {
		t.Errorf("host seen %v, nil handler taken %v, call kind taken %v", sawHost.Load(), taken.Load(), replaced.Load())
	}
}
