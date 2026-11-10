package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func sess(name, kind, state, task string) SessionState {
	return SessionState{Name: name, Kind: kind, State: state, Task: task}
}

func names(ws WorkerSessions) string {
	var n []string
	for _, s := range ws.Sessions {
		n = append(n, s.Name)
	}
	return strings.Join(n, ",")
}

func recvEv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-s.C():
		if !ok {
			t.Fatalf("subscription closed: %v", s.Err())
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func noEvent(t *testing.T, s *Subscription) {
	t.Helper()
	select {
	case ev, ok := <-s.C():
		t.Fatalf("unexpected event %+v (open %v)", ev, ok)
	case <-time.After(30 * time.Millisecond):
	}
}

func newFeed(t *testing.T, h *EventHub, id string) (*Link, *Feed) {
	t.Helper()
	l := h.Connect(id, id+"-name")
	f, err := l.NewFeed()
	if err != nil {
		t.Fatal(err)
	}
	return l, f
}

func TestSnapshotAndEventsKeepTheLatestState(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(16)
	defer sub.Close()
	_, f := newFeed(t, h, "w1")
	if ev := recvEv(t, sub); ev.Type != EventWorkerOnline || ev.WorkerID != "w1" || ev.Worker != "w1-name" {
		t.Fatalf("online: %+v", ev)
	}

	if err := f.Snapshot(0, []SessionState{sess("b", "shell", "stable", ""), sess("a", "agent:claude", "busy", "fix it")}); err != nil {
		t.Fatal(err)
	}
	if ev := recvEv(t, sub); ev.Type != EventWorkerResynced || ev.Session != "" {
		t.Fatalf("resynced: %+v", ev)
	}
	ws, ok := h.Sessions("w1")
	if !ok || !ws.Online || ws.Name != "w1-name" || names(ws) != "a,b" || ws.Sessions[0].Task != "fix it" {
		t.Fatalf("state: %+v", ws)
	}

	empty := ""
	steps := []struct {
		c    Change
		want Event
	}{
		{Change{Seq: 1, Event: EventSessionAdded, HasTask: true, Session: SessionState{Name: "c", Kind: "shell", State: "stable", ScreenHash: "h1", Task: empty}},
			Event{Type: EventSessionAdded, Session: "c", Kind: "shell", State: "stable", ScreenHash: "h1", Task: &empty}},
		{Change{Seq: 2, Event: EventStateChanged, HasTask: true, Session: SessionState{Name: "c", Kind: "agent:codex", State: "dialog", Task: "t"}},
			Event{Type: EventStateChanged, Session: "c", Kind: "agent:codex", State: "dialog", ScreenHash: "h1"}},
		{Change{Seq: 3, Event: EventScreenChanged, Session: SessionState{Name: "c", ScreenHash: "h2"}},
			Event{Type: EventScreenChanged, Session: "c", Kind: "agent:codex", State: "dialog", ScreenHash: "h2"}},
		{Change{Seq: 4, Event: EventSessionRemoved, Session: SessionState{Name: "c"}},
			Event{Type: EventSessionRemoved, Session: "c"}},
	}
	for _, s := range steps {
		if err := f.Apply(s.c); err != nil {
			t.Fatalf("%s: %v", s.c.Event, err)
		}
		got := recvEv(t, sub)
		if got.Type != s.want.Type || got.Session != s.want.Session || got.Kind != s.want.Kind || got.State != s.want.State ||
			got.ScreenHash != s.want.ScreenHash || got.WorkerID != "w1" || got.Worker != "w1-name" {
			t.Fatalf("%s: got %+v want %+v", s.c.Event, got, s.want)
		}
		if s.want.Task != nil && (got.Task == nil) {
			t.Fatalf("%s: the task is missing", s.c.Event)
		}
		if s.c.Event == EventStateChanged && *got.Task != "t" {
			t.Fatalf("task = %q", *got.Task)
		}
	}
	ws, _ = h.Sessions("w1")
	if names(ws) != "a,b" {
		t.Fatalf("after the events: %s", names(ws))
	}
}

func TestEventWithoutTaskKeepsTheTask(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	_, f := newFeed(t, h, "w1")
	if err := f.Snapshot(0, []SessionState{sess("a", "shell", "stable", "keep me")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(Change{Seq: 1, Event: EventStateChanged, Session: SessionState{Name: "a", Kind: "shell", State: "busy"}}); err != nil {
		t.Fatal(err)
	}
	ws, _ := h.Sessions("w1")
	if ws.Sessions[0].Task != "keep me" || ws.Sessions[0].State != "busy" {
		t.Fatalf("%+v", ws.Sessions[0])
	}
	// A cleared task is sent as "" and does clear it.
	if err := f.Apply(Change{Seq: 2, Event: EventStateChanged, HasTask: true, Session: SessionState{Name: "a", Kind: "shell", State: "busy"}}); err != nil {
		t.Fatal(err)
	}
	ws, _ = h.Sessions("w1")
	if ws.Sessions[0].Task != "" {
		t.Fatalf("task = %q", ws.Sessions[0].Task)
	}
}

func TestStreamProtocolViolations(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	_, f := newFeed(t, h, "w1")
	if err := f.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "a"}}); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("event before snapshot: %v", err)
	}
	if err := f.Snapshot(3, nil); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("snapshot with seq 3: %v", err)
	}
	if err := f.Snapshot(0, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(Change{Seq: 2, Event: EventSessionAdded, Session: SessionState{Name: "a"}}); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("gap: %v", err)
	}
	if err := f.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "b"}}); !errors.Is(err, ErrSeqGap) {
		t.Fatalf("repeat: %v", err)
	}
	many := make([]SessionState, maxSessionsPerWorker+1)
	for i := range many {
		many[i] = SessionState{Name: fmt.Sprint("s", i)}
	}
	if err := f.Snapshot(0, many); !errors.Is(err, ErrTooMany) {
		t.Fatalf("too many: %v", err)
	}
	// The failed snapshot changed nothing.
	if ws, _ := h.Sessions("w1"); names(ws) != "a" {
		t.Fatalf("state = %s", names(ws))
	}
}

func TestUnknownEventsAndSessionsAreIgnored(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(8)
	_, f := newFeed(t, h, "w1")
	_ = f.Snapshot(0, nil)
	recvEv(t, sub)
	recvEv(t, sub)
	for i, c := range []Change{
		{Seq: 1, Event: "from_the_future", Session: SessionState{Name: "a"}},
		{Seq: 2, Event: EventSessionRemoved, Session: SessionState{Name: "never-there"}},
		{Seq: 3, Event: EventScreenChanged, Session: SessionState{Name: "never-there", ScreenHash: "x"}},
		{Seq: 4, Event: EventSessionAdded, Session: SessionState{Name: ""}},
		{Seq: 5, Event: EventSessionAdded, Session: SessionState{Name: strings.Repeat("n", maxSessionNameLen+1)}},
	} {
		if err := f.Apply(c); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	noEvent(t, sub)
	if ws, _ := h.Sessions("w1"); len(ws.Sessions) != 0 {
		t.Fatalf("%+v", ws)
	}
}

func TestReportedValuesAreBounded(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	_, f := newFeed(t, h, "w1")
	task := strings.Repeat("\u00e9", ops.MaxTaskLen) // 2 bytes each
	if err := f.Snapshot(0, []SessionState{
		{Name: "bad\xffname", Kind: strings.Repeat("k", 500), State: "s", Task: task, ScreenHash: strings.Repeat("h", 500)},
		{Name: ""},
	}); err != nil {
		t.Fatal(err)
	}
	ws, _ := h.Sessions("w1")
	if len(ws.Sessions) != 1 {
		t.Fatalf("%+v", ws.Sessions)
	}
	s := ws.Sessions[0]
	if s.Name != "badname" || len(s.Kind) != maxClassLen || len(s.Task) > ops.MaxTaskLen || len(s.ScreenHash) != maxScreenHashLen {
		t.Fatalf("not bounded: %q %d %d %d", s.Name, len(s.Kind), len(s.Task), len(s.ScreenHash))
	}
	if !strings.HasSuffix(s.Task, "\u00e9") {
		t.Fatal("the task was cut inside a rune")
	}
}

func TestReconnectSnapshotReplacesStateAtomically(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(64)
	l1, f1 := newFeed(t, h, "w1")
	if err := f1.Snapshot(0, []SessionState{sess("old1", "shell", "stable", ""), sess("old2", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	// The connection drops: the sessions stay, the Worker is offline.
	l1.Disconnect()
	ws, _ := h.Sessions("w1")
	if ws.Online || names(ws) != "old1,old2" {
		t.Fatalf("offline state: online=%v %s", ws.Online, names(ws))
	}

	// It reconnects: still the old picture until its snapshot arrives.
	l2 := h.Connect("w1", "w1-name")
	ws, _ = h.Sessions("w1")
	if !ws.Online || names(ws) != "old1,old2" {
		t.Fatalf("after reconnect: %+v", ws)
	}
	f2, err := l2.NewFeed()
	if err != nil {
		t.Fatal(err)
	}

	// Readers must never see a mix of old and new sessions.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	bad := make(chan string, 1)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, _ := h.Sessions("w1")
				switch n := names(got); n {
				case "old1,old2", "new1,new2,new3":
				default:
					select {
					case bad <- n:
					default:
					}
				}
			}
		}()
	}
	if err := f2.Snapshot(0, []SessionState{sess("new1", "shell", "stable", ""), sess("new2", "shell", "stable", ""), sess("new3", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	select {
	case n := <-bad:
		t.Fatalf("a reader saw a mixed state: %s", n)
	default:
	}
	ws, _ = h.Sessions("w1")
	if names(ws) != "new1,new2,new3" {
		t.Fatalf("final: %s", names(ws))
	}

	var types []string
	for len(sub.C()) > 0 {
		types = append(types, (<-sub.C()).Type)
	}
	want := []string{EventWorkerOnline, EventWorkerResynced, EventWorkerOffline, EventWorkerOnline, EventWorkerResynced}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
}

func TestSupersededHandlesChangeNothing(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	l1, f1 := newFeed(t, h, "w1")
	_ = f1.Snapshot(0, []SessionState{sess("a", "shell", "stable", "")})

	// A second stream of the same connection supersedes the first.
	f1b, err := l1.NewFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := f1.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "late"}}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old stream: %v", err)
	}
	if err := f1b.Snapshot(0, nil); err != nil {
		t.Fatal(err)
	}

	// A new connection supersedes the old one.
	l2 := h.Connect("w1", "renamed")
	if _, err := l1.NewFeed(); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old link: %v", err)
	}
	if err := f1b.Snapshot(0, []SessionState{sess("ghost", "", "", "")}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old feed: %v", err)
	}
	l1.Disconnect() // the late end of the old connection
	ws, _ := h.Sessions("w1")
	if !ws.Online || ws.Name != "renamed" {
		t.Fatalf("the old connection's end took the Worker offline: %+v", ws)
	}
	l2.Disconnect()
	if ws, _ = h.Sessions("w1"); ws.Online {
		t.Fatal("still online")
	}
	l2.Disconnect() // idempotent
}

func TestForgetAndAll(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	h.Connect("w2", "two")
	h.Connect("w1", "one")
	if all := h.All(); len(all) != 2 || all[0].WorkerID != "w1" || all[1].WorkerID != "w2" {
		t.Fatalf("%+v", all)
	}
	h.Forget("w1")
	if _, ok := h.Sessions("w1"); ok {
		t.Fatal("still known")
	}
	if len(h.All()) != 1 {
		t.Fatal("All still lists it")
	}
}

func TestSubscriptions(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	a, b := h.Subscribe(2), h.Subscribe(100)
	// a never reads: its queue of 2 overflows on the third event.
	for i := 0; i < 3; i++ {
		h.Connect(fmt.Sprint("w", i), "n")
	}
	for i := 0; i < 2; i++ {
		if _, ok := <-a.C(); !ok {
			t.Fatal("closed too early")
		}
	}
	if _, ok := <-a.C(); ok {
		t.Fatal("a slow subscriber was not closed")
	}
	if !errors.Is(a.Err(), ErrSlowSubscriber) {
		t.Fatalf("err = %v", a.Err())
	}
	// The slow one did not hurt the other.
	for i := 0; i < 3; i++ {
		recvEv(t, b)
	}
	b.Close()
	b.Close() // idempotent
	if _, ok := <-b.C(); ok {
		t.Fatal("closed subscription delivers")
	}
	if b.Err() != nil {
		t.Fatalf("err = %v", b.Err())
	}
	h.Connect("w9", "n") // publishing without subscribers is fine
	if got := h.Subscribe(0); cap(got.ch) != DefaultSubscriberBuffer {
		t.Fatalf("default buffer = %d", cap(got.ch))
	}
}

// fakeFrames is a FrameReader over prepared frames.
type fakeFrames struct {
	mu     sync.Mutex
	frames []any
	end    error // returned when the frames are used up; nil blocks
	reset  chan struct{}
	once   sync.Once
}

func newFakeFrames(end error, frames ...any) *fakeFrames {
	return &fakeFrames{frames: frames, end: end, reset: make(chan struct{})}
}

func (f *fakeFrames) ReadFrame() (json.RawMessage, error) {
	f.mu.Lock()
	if len(f.frames) > 0 {
		v := f.frames[0]
		f.frames = f.frames[1:]
		f.mu.Unlock()
		if raw, ok := v.([]byte); ok {
			return raw, nil
		}
		b, _ := json.Marshal(v)
		return b, nil
	}
	f.mu.Unlock()
	if f.end != nil {
		return nil, f.end
	}
	<-f.reset
	return nil, io.ErrClosedPipe
}

func (f *fakeFrames) Reset() { f.once.Do(func() { close(f.reset) }) }

func (f *fakeFrames) wasReset() bool {
	select {
	case <-f.reset:
		return true
	default:
		return false
	}
}

func snapFrame(seq uint64, sessions ...map[string]any) map[string]any {
	if sessions == nil {
		sessions = []map[string]any{}
	}
	return map[string]any{"type": proto.TypeEventsSnap, "seq": seq, "time": "2026-11-10T10:00:00.000Z", "sessions": sessions}
}

func evFrame(seq uint64, event, session string, extra map[string]any) map[string]any {
	m := map[string]any{"type": proto.TypeEvent, "seq": seq, "event": event, "session": session}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestServeFeedDecodesFrames(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	_, f := newFeed(t, h, "w1")
	st := newFakeFrames(io.EOF,
		snapFrame(0, map[string]any{"name": "a", "kind": "shell", "state": "stable", "task": "x", "screen_hash": "h"}),
		map[string]any{"type": "turn.ack", "id": "ignored"},
		evFrame(1, EventStateChanged, "a", map[string]any{"kind": "agent:claude", "state": "busy", "task": "y", "future_field": 1}),
	)
	if err := ServeFeed(context.Background(), st, f); err != nil {
		t.Fatal(err)
	}
	ws, _ := h.Sessions("w1")
	want := SessionState{Name: "a", Kind: "agent:claude", State: "busy", Task: "y", ScreenHash: "h"}
	if len(ws.Sessions) != 1 || ws.Sessions[0] != want {
		t.Fatalf("%+v", ws.Sessions)
	}
	if st.wasReset() {
		t.Fatal("a clean end resets the stream")
	}
}

func TestServeFeedResetsOnProtocolErrors(t *testing.T) {
	cases := map[string][]any{
		"gap":                {snapFrame(0), evFrame(2, EventSessionAdded, "a", nil)},
		"event first":        {evFrame(1, EventSessionAdded, "a", nil)},
		"snapshot with seq":  {snapFrame(4)},
		"bad snapshot shape": {map[string]any{"type": proto.TypeEventsSnap, "seq": "zero"}},
		"bad event shape":    {snapFrame(0), map[string]any{"type": proto.TypeEvent, "seq": "one"}},
		"not a frame":        {snapFrame(0), []byte(`[1]`)},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			h := NewEventHub(EventHubOptions{})
			_, f := newFeed(t, h, "w1")
			st := newFakeFrames(nil, frames...)
			if err := ServeFeed(context.Background(), st, f); err == nil {
				t.Fatal("no error")
			}
			if !st.wasReset() {
				t.Fatal("the stream was not reset")
			}
		})
	}
}

func TestServeFeedEnds(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	l, f := newFeed(t, h, "w1")

	// ctx ends a blocked read and resets the stream.
	ctx, cancel := context.WithCancel(context.Background())
	st := newFakeFrames(nil)
	done := make(chan error, 1)
	go func() { done <- ServeFeed(ctx, st, f) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx end: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeFeed ignored ctx")
	}
	if !st.wasReset() {
		t.Fatal("not reset")
	}

	// A superseded stream ends quietly; a read error is returned.
	f2, _ := l.NewFeed()
	st = newFakeFrames(nil, snapFrame(0))
	if err := ServeFeed(context.Background(), st, f); err != nil {
		t.Fatalf("superseded: %v", err)
	}
	boom := errors.New("boom")
	if err := ServeFeed(context.Background(), newFakeFrames(boom), f2); !errors.Is(err, boom) {
		t.Fatalf("read error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Ingest on real transport sessions

func hubAndWorker(t *testing.T) (hub, worker *transport.Session) {
	t.Helper()
	a, b := net.Pipe()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err = transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })
	return hub, worker
}

func openEvents(t *testing.T, w *transport.Session) *transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := w.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestIngestRoutesEvStreamsAndTheOtherKinds(t *testing.T) {
	hub := NewEventHub(EventHubOptions{})
	var (
		mu    sync.Mutex
		other []string
	)
	otherDone := make(chan struct{}, 1)
	ing, err := NewIngest(IngestOptions{Hub: hub, Other: func(info Online, st *transport.Stream) {
		mu.Lock()
		other = append(other, info.WorkerID+"/"+st.Kind().String())
		mu.Unlock()
		st.Reset()
		otherDone <- struct{}{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	hs, ws := hubAndWorker(t)
	sub := hub.Subscribe(32)
	ing.Attach(Online{WorkerID: "w1", Name: "pi-1"}, hs)
	if ev := recvEv(t, sub); ev.Type != EventWorkerOnline {
		t.Fatalf("%+v", ev)
	}

	ev := openEvents(t, ws)
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "a", "kind": "shell", "state": "stable", "task": ""})); err != nil {
		t.Fatal(err)
	}
	if got := recvEv(t, sub); got.Type != EventWorkerResynced {
		t.Fatalf("%+v", got)
	}
	if err := ev.WriteFrame(evFrame(1, EventStateChanged, "a", map[string]any{"kind": "shell", "state": "busy", "task": "go"})); err != nil {
		t.Fatal(err)
	}
	if got := recvEv(t, sub); got.Type != EventStateChanged || got.State != "busy" {
		t.Fatalf("%+v", got)
	}

	// A call stream goes to the other handler.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca, err := ws.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Reset()
	select {
	case <-otherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the call stream was not handed over")
	}
	mu.Lock()
	if fmt.Sprint(other) != "[w1/call]" {
		t.Fatalf("other = %v", other)
	}
	mu.Unlock()

	// A second ev stream supersedes the first and starts from a snapshot.
	ev2 := openEvents(t, ws)
	if err := ev2.WriteFrame(snapFrame(0, map[string]any{"name": "z", "kind": "shell", "state": "stable", "task": ""})); err != nil {
		t.Fatal(err)
	}
	if got := recvEv(t, sub); got.Type != EventWorkerResynced {
		t.Fatalf("%+v", got)
	}
	state, _ := hub.Sessions("w1")
	if names(state) != "z" {
		t.Fatalf("state = %s", names(state))
	}
	_ = ev.WriteFrame(evFrame(2, EventSessionAdded, "old-stream", nil)) // may fail: the stream was reset
	time.Sleep(20 * time.Millisecond)
	if state, _ = hub.Sessions("w1"); names(state) != "z" {
		t.Fatalf("the replaced stream still feeds: %s", names(state))
	}

	// The session ends: the Worker is offline and keeps its sessions.
	_ = hs.Close()
	for {
		if got := recvEv(t, sub); got.Type == EventWorkerOffline {
			break
		}
	}
	state, _ = hub.Sessions("w1")
	if state.Online || names(state) != "z" {
		t.Fatalf("%+v", state)
	}
}

func TestIngestResetsUnwantedStreamsAndIgnoresPlainSessions(t *testing.T) {
	hub := NewEventHub(EventHubOptions{})
	ing, err := NewIngest(IngestOptions{Hub: hub})
	if err != nil {
		t.Fatal(err)
	}
	hs, ws := hubAndWorker(t)
	ing.Attach(Online{WorkerID: "w1", Name: "pi-1"}, hs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ca, err := ws.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Without a handler the Hub resets the stream: the Worker's read ends.
	_ = ca.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := ca.Read(make([]byte, 1)); err == nil {
		t.Fatal("the call stream was not reset")
	}

	// A session that cannot accept streams is not attached.
	ing.Attach(Online{WorkerID: "w2"}, newFakeSession())
	if _, ok := hub.Sessions("w2"); ok {
		t.Fatal("a session without Accept was attached")
	}
	if _, err := NewIngest(IngestOptions{}); err == nil {
		t.Fatal("an ingest without a hub")
	}
}

func TestRegistryHookStartsTheIngest(t *testing.T) {
	hub := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: hub})
	reg := NewRegistry()
	reg.OnAdd(ing.Attach)
	reg.OnAdd(nil) // ignored
	hs, ws := hubAndWorker(t)
	reg.Add(Online{WorkerID: "w1", Name: "pi-1", Ops: []string{"sessions_list"}}, hs)
	ev := openEvents(t, ws)
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "a", "kind": "shell", "state": "stable", "task": ""})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the snapshot", func() bool {
		s, ok := hub.Sessions("w1")
		return ok && s.Online && names(s) == "a"
	})
	_ = ws.Close()
	eventually(t, "offline", func() bool {
		s, _ := hub.Sessions("w1")
		return !s.Online
	})
}

// TestInProcessHubReceivesEventsFromATestWorker runs the whole path: the
// connect handler with a real handshake, the registry hook, the ev stream.
func TestInProcessHubReceivesEventsFromATestWorker(t *testing.T) {
	h := newTestHub(t, nil)
	events := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: events})
	h.svc.Registry().OnAdd(ing.Attach)
	sub := events.Subscribe(32)

	key := newKey(t)
	w := h.enrol(key, idAlice, "pi-1")
	res, err := h.dial(h.workerConfig(key, ops.OpSessionsList))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	wsess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer wsess.Close()
	if ev := recvEv(t, sub); ev.Type != EventWorkerOnline || ev.WorkerID != w.ID || ev.Worker != "pi-1" {
		t.Fatalf("%+v", ev)
	}
	st := openEvents(t, wsess)
	_ = st.WriteFrame(snapFrame(0, map[string]any{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "t"}))
	if ev := recvEv(t, sub); ev.Type != EventWorkerResynced {
		t.Fatalf("%+v", ev)
	}
	_ = st.WriteFrame(evFrame(1, EventStateChanged, "minion-1", map[string]any{"kind": "agent:claude", "state": "busy", "task": "t"}))
	ev := recvEv(t, sub)
	if ev.Type != EventStateChanged || ev.Session != "minion-1" || ev.State != "busy" || ev.WorkerID != w.ID {
		t.Fatalf("%+v", ev)
	}
	got, _ := events.Sessions(w.ID)
	if !got.Online || len(got.Sessions) != 1 || got.Sessions[0].State != "busy" {
		t.Fatalf("%+v", got)
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(ev)
	if !strings.Contains(buf.String(), `"type":"state_changed"`) {
		t.Fatalf("event JSON: %s", buf.String())
	}
}

// TestIngestOnFrameDeliversOtherFramesAndRepliesOnTheStream: a frame of a
// registered type between events reaches its handler without breaking seq, the
// reply is read on the Worker side of the ev stream, and unregistered types
// stay ignored.
func TestIngestOnFrameDeliversOtherFramesAndRepliesOnTheStream(t *testing.T) {
	hub := NewEventHub(EventHubOptions{})
	ing, err := NewIngest(IngestOptions{Hub: hub, ReplyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		info Online
		body string
	}
	calls := make(chan call, 4)
	ing.OnFrame("turn", func(info Online, body json.RawMessage, reply func(v any) error) {
		calls <- call{info, string(body)}
		var f struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &f)
		if err := reply(map[string]any{"type": proto.TypeTurnAck, "id": f.ID}); err != nil {
			t.Errorf("reply: %v", err)
		}
	})
	// Ignored registrations: nil, empty and the event protocol's own types.
	ing.OnFrame("turn", nil)
	ing.OnFrame("", func(Online, json.RawMessage, func(any) error) { t.Error("empty type") })
	ing.OnFrame(proto.TypeEvent, func(Online, json.RawMessage, func(any) error) { t.Error("event frames are not routed") })
	ing.OnFrame(proto.TypeEventsSnap, func(Online, json.RawMessage, func(any) error) { t.Error("snapshots are not routed") })

	hs, ws := hubAndWorker(t)
	sub := hub.Subscribe(32)
	ing.Attach(Online{WorkerID: "w1", Name: "pi-1"}, hs)
	if ev := recvEv(t, sub); ev.Type != EventWorkerOnline {
		t.Fatalf("%+v", ev)
	}
	ev := openEvents(t, ws)
	write := func(v any) {
		t.Helper()
		if err := ev.WriteFrame(v); err != nil {
			t.Fatal(err)
		}
	}
	write(snapFrame(0, map[string]any{"name": "a", "kind": "shell", "state": "stable", "task": ""}))
	if got := recvEv(t, sub); got.Type != EventWorkerResynced {
		t.Fatalf("%+v", got)
	}
	write(evFrame(1, EventStateChanged, "a", map[string]any{"kind": "shell", "state": "busy", "task": "go"}))
	if got := recvEv(t, sub); got.Type != EventStateChanged {
		t.Fatalf("%+v", got)
	}
	write(map[string]any{"type": "turn", "id": "t1", "future_field": true})
	write(map[string]any{"type": "unregistered"}) // ignored (AD-4)
	write(evFrame(2, EventStateChanged, "a", map[string]any{"kind": "shell", "state": "stable", "task": "go"}))

	select {
	case c := <-calls:
		if c.info.WorkerID != "w1" || c.info.Name != "pi-1" || !strings.Contains(c.body, `"id":"t1"`) {
			t.Fatalf("%+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was not called")
	}
	// The Worker reads the reply on the same stream.
	_ = ev.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := ev.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if typ, _ := proto.FrameType(body); typ != proto.TypeTurnAck || !strings.Contains(string(body), `"id":"t1"`) {
		t.Fatalf("reply = %s", body)
	}
	// seq was not disturbed by the extra frames: the event after them applies.
	if got := recvEv(t, sub); got.Type != EventStateChanged || got.State != "stable" {
		t.Fatalf("%+v", got)
	}
	select {
	case c := <-calls:
		t.Fatalf("a second call: %+v", c)
	default:
	}
}

// TestOverlappingAddsAttachInRegistryOrder: an older connection whose hook is
// delayed must not supersede the newer one in the event state.
func TestOverlappingAddsAttachInRegistryOrder(t *testing.T) {
	hub := NewEventHub(EventHubOptions{})
	ing, _ := NewIngest(IngestOptions{Hub: hub})
	reg := NewRegistry()
	var (
		first   atomic.Bool
		entered = make(chan struct{})
		release = make(chan struct{})
	)
	reg.OnAdd(func(info Online, sess OpSession) {
		if first.CompareAndSwap(false, true) { // the hook of the first Add is delayed
			close(entered)
			<-release
		}
		ing.Attach(info, sess)
	})
	hsA, _ := hubAndWorker(t)
	hsB, wsB := hubAndWorker(t)
	aDone, bDone := make(chan struct{}), make(chan struct{})
	go func() { reg.Add(Online{WorkerID: "w1", Name: "pi-1"}, hsA); close(aDone) }()
	<-entered
	go func() { reg.Add(Online{WorkerID: "w1", Name: "pi-1"}, hsB); close(bDone) }()
	time.Sleep(50 * time.Millisecond) // let the second Add run into the first
	close(release)
	<-aDone
	<-bDone

	ev := openEvents(t, wsB)
	if err := ev.WriteFrame(snapFrame(0, map[string]any{"name": "b", "kind": "shell", "state": "stable", "task": ""})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the newer connection owns the state", func() bool {
		s, ok := hub.Sessions("w1")
		return ok && s.Online && names(s) == "b"
	})
	time.Sleep(50 * time.Millisecond)
	if s, _ := hub.Sessions("w1"); !s.Online {
		t.Fatal("the older connection took the state over and marked the Worker offline")
	}
}
