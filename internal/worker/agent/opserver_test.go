package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// fakeOpStream is an OpStream with one scripted request and a record of
// everything the server wrote.
type fakeOpStream struct {
	mu       sync.Mutex
	in       []json.RawMessage
	readErr  error
	frames   []json.RawMessage
	closed   bool
	reset    bool
	writeErr error

	// Once the scripted frames are read, ReadFrame blocks like a real stream
	// whose Hub stays silent, until the Hub ends it (abandon), the server
	// resets it, or a read deadline in the past is set.
	readAny bool
	hubEnd  chan struct{}
	endOnce sync.Once
}

// abandon ends the stream from the Hub side, like a reset: a blocked
// ReadFrame returns io.EOF.
func (s *fakeOpStream) abandon() { s.endOnce.Do(func() { close(s.hubEndCh()) }) }

func (s *fakeOpStream) hubEndCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hubEnd == nil {
		s.hubEnd = make(chan struct{})
	}
	return s.hubEnd
}

func newFakeOpStream(t testing.TB, rq any) *fakeOpStream {
	t.Helper()
	return &fakeOpStream{in: []json.RawMessage{mustJSON(t, rq)}}
}

func (s *fakeOpStream) ReadFrame() (json.RawMessage, error) {
	s.mu.Lock()
	if s.readErr != nil {
		defer s.mu.Unlock()
		return nil, s.readErr
	}
	if len(s.in) == 0 {
		blocked := s.readAny
		s.readAny = true
		s.mu.Unlock()
		if !blocked {
			return nil, io.EOF
		}
		<-s.hubEndCh()
		return nil, io.EOF
	}
	defer s.mu.Unlock()
	s.readAny = true
	f := s.in[0]
	s.in = s.in[1:]
	return f, nil
}

func (s *fakeOpStream) WriteFrame(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := proto.FrameType(b); err != nil {
		return err
	}
	s.frames = append(s.frames, b)
	return nil
}

func (s *fakeOpStream) Close() error { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }
func (s *fakeOpStream) Reset() {
	s.mu.Lock()
	s.reset = true
	s.closed = true
	s.mu.Unlock()
	s.abandon()
}
func (s *fakeOpStream) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		s.abandon() // a deadline in the past wakes a blocked read
	}
	return nil
}
func (s *fakeOpStream) SetWriteDeadline(time.Time) error { return nil }

func (s *fakeOpStream) types(t testing.TB) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.frames {
		ty, err := proto.FrameType(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ty)
	}
	return out
}

func (s *fakeOpStream) frame(i int) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[i]
}

// errorFrame decodes the last frame as an error frame.
func (s *fakeOpStream) errorFrame(t testing.TB) proto.ErrorFrame {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var f proto.ErrorFrame
	if err := json.Unmarshal(s.frames[len(s.frames)-1], &f); err != nil || f.Type != proto.TypeError {
		t.Fatalf("last frame %s is not an error frame", s.frames[len(s.frames)-1])
	}
	return f
}

func (s *fakeOpStream) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *fakeOpStream) wasReset() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.reset }

// fakeExec is an OpExecutor that records requests and runs a script.
type fakeExec struct {
	mu   sync.Mutex
	got  []Request
	run  func(ctx context.Context, rq Request) (json.RawMessage, error)
	seen []context.Context
}

func (f *fakeExec) Execute(ctx context.Context, rq Request) (json.RawMessage, error) {
	f.mu.Lock()
	f.got = append(f.got, rq)
	f.seen = append(f.seen, ctx)
	run := f.run
	f.mu.Unlock()
	if run == nil {
		return json.RawMessage(`{"ok":true}`), nil
	}
	return run(ctx, rq)
}

func (f *fakeExec) requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.got...)
}

func newOpServer(t testing.TB, ex OpExecutor, mut ...func(*OpServerConfig)) *OpServer {
	t.Helper()
	cfg := OpServerConfig{Executor: ex}
	for _, m := range mut {
		m(&cfg)
	}
	s, err := NewOpServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func opRequest(op string, actions []ops.Action, args any) map[string]any {
	m := map[string]any{"type": proto.TypeOpRequest, "op": op, "actions": actions}
	if args != nil {
		m["args"] = args
	}
	return m
}

func TestNewOpServerNeedsAnExecutor(t *testing.T) {
	if _, err := NewOpServer(OpServerConfig{}); err == nil {
		t.Fatal("an op server without an executor was accepted")
	}
}

// AC 1: request in, progress frames, exactly one op.result, stream closed.
func TestOpStreamFrameOrderIsRequestProgressResult(t *testing.T) {
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		_ = EmitProgress(ctx, "starting")
		_ = EmitProgress(ctx, map[string]int{"n": 1})
		return json.RawMessage(`{"sessions":[]}`), nil
	}}
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	newOpServer(t, ex).ServeStream(context.Background(), st)

	want := []string{proto.TypeOpProgress, proto.TypeOpProgress, proto.TypeOpResult}
	if got := st.types(t); !equalStrings(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	var p OpProgressFrame
	if err := json.Unmarshal(st.frame(1), &p); err != nil || string(p.Data) != `{"n":1}` {
		t.Fatalf("progress frame = %s (%v)", st.frame(1), err)
	}
	var res OpResultFrame
	if err := json.Unmarshal(st.frame(2), &res); err != nil || string(res.Result) != `{"sessions":[]}` {
		t.Fatalf("result frame = %s (%v)", st.frame(2), err)
	}
	if !st.isClosed() || st.wasReset() {
		t.Fatalf("stream closed=%v reset=%v, want a graceful close", st.isClosed(), st.wasReset())
	}
	rq := ex.requests()
	if len(rq) != 1 || rq[0].Op != ops.OpSessionsList || len(rq[0].Actions) != 1 || rq[0].Actions[0] != ops.ActionRead {
		t.Fatalf("executor got %+v", rq)
	}
}

func TestOpStreamProgressAfterTheResultIsDropped(t *testing.T) {
	var late func(json.RawMessage)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		late, _ = ctx.Value(progressKey{}).(func(json.RawMessage))
		return json.RawMessage(`{}`), nil
	}}
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	newOpServer(t, ex).ServeStream(context.Background(), st)
	late(json.RawMessage(`"too late"`))
	if got := st.types(t); !equalStrings(got, []string{proto.TypeOpResult}) {
		t.Fatalf("frames = %v, want only the result", got)
	}
}

func TestOpStreamWithoutProgressReporterIsHarmless(t *testing.T) {
	_ = EmitProgress(context.Background(), "nobody listens") // the local socket path
}

func TestOpStreamAPIErrorBecomesOneErrorFrame(t *testing.T) {
	ex := &fakeExec{run: func(context.Context, Request) (json.RawMessage, error) {
		_ = EmitProgress(context.Background(), "ignored")
		return nil, &APIError{Code: proto.CodeForbidden, Reason: proto.CodeNotShared, Message: "the session is not shared", Err: errors.New("secret cause")}
	}}
	st := newFakeOpStream(t, opRequest(ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, nil))
	newOpServer(t, ex).ServeStream(context.Background(), st)

	if got := st.types(t); !equalStrings(got, []string{proto.TypeError}) {
		t.Fatalf("frames = %v, want one error frame", got)
	}
	f := st.errorFrame(t)
	if f.Code != proto.CodeForbidden || f.Message != "the session is not shared" {
		t.Fatalf("error frame = %+v", f)
	}
	if string(f.Details) != `{"reason":"not_shared"}` {
		t.Fatalf("details = %s", f.Details)
	}
	if bytes.Contains(st.frame(0), []byte("secret cause")) {
		t.Fatal("the cause of an API error reached the Hub")
	}
	if !st.isClosed() {
		t.Fatal("the stream stays open after an error")
	}
}

func TestOpStreamUnexpectedErrorIsAGenericInternalError(t *testing.T) {
	ex := &fakeExec{run: func(context.Context, Request) (json.RawMessage, error) {
		return nil, errors.New("sqlite: /var/lib/minion/worker.db is locked")
	}}
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	newOpServer(t, ex).ServeStream(context.Background(), st)
	f := st.errorFrame(t)
	if f.Code != proto.CodeInternal || strings.Contains(f.Message, "sqlite") {
		t.Fatalf("error frame = %+v", f)
	}
}

func TestOpStreamExecTimeoutIsAnErrorFrame(t *testing.T) {
	ex := &fakeExec{run: func(ctx context.Context, _ Request) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	newOpServer(t, ex, func(c *OpServerConfig) { c.ExecTimeout = 20 * time.Millisecond }).ServeStream(context.Background(), st)
	if f := st.errorFrame(t); f.Code != proto.CodeInternal {
		t.Fatalf("error frame = %+v", f)
	}
}

func TestOpStreamUnknownOpGoesThroughTheExecutor(t *testing.T) {
	e := newEnv(t)
	st := newFakeOpStream(t, opRequest("format_disk", allActions, nil))
	newOpServer(t, e.ex).ServeStream(context.Background(), st)
	if f := st.errorFrame(t); f.Code != proto.CodeUnsupportedOp {
		t.Fatalf("error frame = %+v", f)
	}
}

func TestOpStreamBadFirstFrame(t *testing.T) {
	for name, rq := range map[string]any{
		"wrong type": map[string]any{"type": proto.TypeOpResult, "op": "sessions_list"},
		"no op":      map[string]any{"type": proto.TypeOpRequest, "actions": []string{"read"}},
		"bad shape":  map[string]any{"type": proto.TypeOpRequest, "op": 5},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExec{}
			st := newFakeOpStream(t, rq)
			newOpServer(t, ex).ServeStream(context.Background(), st)
			if f := st.errorFrame(t); f.Code != proto.CodeInvalidArgument {
				t.Fatalf("error frame = %+v", f)
			}
			if len(ex.requests()) != 0 {
				t.Fatal("a malformed request reached the executor")
			}
		})
	}
}

func TestOpStreamWithoutARequestIsReset(t *testing.T) {
	for name, err := range map[string]error{
		"eof":          io.EOF,
		"read timeout": errors.New("i/o timeout"),
		"too large":    proto.NewError(proto.CodeFrameTooLarge, "frame too big"),
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExec{}
			st := &fakeOpStream{readErr: err}
			newOpServer(t, ex).ServeStream(context.Background(), st)
			if !st.wasReset() || len(st.frames) != 0 || len(ex.requests()) != 0 {
				t.Fatalf("reset=%v frames=%d requests=%d", st.wasReset(), len(st.frames), len(ex.requests()))
			}
		})
	}
}

func TestOpStreamResultTooLargeIsTold(t *testing.T) {
	ex := &fakeExec{run: func(context.Context, Request) (json.RawMessage, error) {
		return json.RawMessage(`"` + strings.Repeat("x", proto.StreamFrameLimit) + `"`), nil
	}}
	// The fake stream does not enforce the limit; use the real codec.
	var buf bytes.Buffer
	st := &codecOpStream{fakeOpStream: newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil)), w: &buf}
	newOpServer(t, ex).ServeStream(context.Background(), st)
	body, err := proto.ReadFrame(&buf, proto.StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var f proto.ErrorFrame
	if err := json.Unmarshal(body, &f); err != nil || f.Code != proto.CodeFrameTooLarge {
		t.Fatalf("frame = %s (%v)", body, err)
	}
	if _, err := proto.ReadFrame(&buf, proto.StreamFrameLimit); err != io.EOF {
		t.Fatalf("a second frame followed: %v", err)
	}
}

// codecOpStream writes through the production frame codec.
type codecOpStream struct {
	*fakeOpStream
	w io.Writer
}

func (s *codecOpStream) WriteFrame(v any) error {
	return proto.WriteFrame(s.w, proto.StreamFrameLimit, v)
}

func TestOpStreamFinalWriteFailureResets(t *testing.T) {
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	st.writeErr = errors.New("broken pipe")
	newOpServer(t, &fakeExec{}).ServeStream(context.Background(), st)
	if !st.wasReset() {
		t.Fatal("a stream whose answer could not be written must be reset, so the Hub sees a loss")
	}
}

// AC 1: the actions of the request are intersected with local policy.
func TestActionsAreIntersectedWithLocalPolicy(t *testing.T) {
	st := func(p *Policy, actions []ops.Action) []ops.Action {
		ex := &fakeExec{}
		s := newFakeOpStream(t, opRequest(ops.OpSessionsList, actions, nil))
		newOpServer(t, ex, func(c *OpServerConfig) { c.Policy = p }).ServeStream(context.Background(), s)
		return ex.requests()[0].Actions
	}
	open := NewPolicy(nil)
	local := NewPolicy(nil)
	local.SetPreviewOnly(true)
	hub := NewPolicy(nil)
	hub.SetHubPreviewOnly(true)

	rw := []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}
	cases := []struct {
		name    string
		policy  *Policy
		actions []ops.Action
		want    []ops.Action
	}{
		{"open policy keeps all, sorted", open, []ops.Action{ops.ActionOperate, ops.ActionRead}, []ops.Action{ops.ActionOperate, ops.ActionRead}},
		{"no policy keeps all", nil, rw, []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}},
		{"local preview-only keeps read", local, rw, []ops.Action{ops.ActionRead}},
		{"hub preview-only keeps read", hub, rw, []ops.Action{ops.ActionRead}},
		{"unknown actions and duplicates are dropped", open, []ops.Action{"root", ops.ActionRead, ops.ActionRead, ""}, []ops.Action{ops.ActionRead}},
		{"no actions stay none", open, nil, []ops.Action{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := st(c.policy, c.actions)
			if !equalActions(got, c.want) {
				t.Fatalf("actions = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPreviewOnlyWriteIsRefusedWithItsReason(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "> ")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.policy.SetHubPreviewOnly(true)

	args := sendText("work", "hello", false)
	st := newFakeOpStream(t, opRequest(ops.OpSendText, []ops.Action{ops.ActionOperate, ops.ActionRead}, args))
	newOpServer(t, e.ex, func(c *OpServerConfig) { c.Policy = e.policy }).ServeStream(context.Background(), st)

	f := st.errorFrame(t)
	if f.Code != proto.CodeForbidden || string(f.Details) != `{"reason":"preview_only"}` {
		t.Fatalf("error frame = %+v (%s)", f, f.Details)
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("text was pasted into a preview-only Worker")
	}

	// A caller that never held operate is told the plain reason.
	st = newFakeOpStream(t, opRequest(ops.OpSendText, []ops.Action{ops.ActionRead}, sendText("work", "hello", false)))
	newOpServer(t, e.ex, func(c *OpServerConfig) { c.Policy = e.policy }).ServeStream(context.Background(), st)
	f = st.errorFrame(t)
	if f.Code != proto.CodeForbidden || len(f.Details) != 0 {
		t.Fatalf("error frame = %+v (%s)", f, f.Details)
	}

	// Reads still work on a preview-only Worker.
	st = newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead, ops.ActionOperate}, nil))
	newOpServer(t, e.ex, func(c *OpServerConfig) { c.Policy = e.policy }).ServeStream(context.Background(), st)
	if got := st.types(t); !equalStrings(got, []string{proto.TypeOpResult}) {
		t.Fatalf("frames = %v", got)
	}
}

func TestWriteRequestIDOfTheFrameJoinsTheArguments(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "> ")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	id := ops.NewUUIDv7()

	args := map[string]any{"target": target("work"), "text": "hi"} // no request_id inside
	rq := opRequest(ops.OpSendText, allActions, args)
	rq["request_id"] = id
	st := newFakeOpStream(t, rq)
	newOpServer(t, e.ex).ServeStream(context.Background(), st)
	if got := st.types(t); !equalStrings(got, []string{proto.TypeOpResult}) {
		t.Fatalf("frames = %v (%s)", got, st.frame(0))
	}
	var res ops.WriteResult
	var rf OpResultFrame
	_ = json.Unmarshal(st.frame(0), &rf)
	if err := json.Unmarshal(rf.Result, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s (%v)", rf.Result, err)
	}
	// The same id again is a duplicate answered from the cache: no second paste.
	st = newFakeOpStream(t, rq)
	newOpServer(t, e.ex).ServeStream(context.Background(), st)
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}

	// An id that disagrees with the arguments is refused before anything runs.
	args["request_id"] = ops.NewUUIDv7()
	st = newFakeOpStream(t, rq)
	newOpServer(t, e.ex).ServeStream(context.Background(), st)
	if f := st.errorFrame(t); f.Code != proto.CodeInvalidArgument {
		t.Fatalf("error frame = %+v", f)
	}
	// So is a request whose arguments are not an object.
	rq["args"] = []int{1}
	st = newFakeOpStream(t, rq)
	newOpServer(t, e.ex).ServeStream(context.Background(), st)
	if f := st.errorFrame(t); f.Code != proto.CodeInvalidArgument {
		t.Fatalf("error frame = %+v", f)
	}
}

// AC 3: an unshared session appears in no op result.
func TestUnsharedSessionIsInNoOpResult(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"open", "secret"} {
		ref := e.host.addSession(n, "claude")
		e.host.setScreen(ref, "content of "+n)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	srv := newOpServer(t, e.ex, func(c *OpServerConfig) { c.Policy = e.policy })

	run := func(op string, args any) *fakeOpStream {
		st := newFakeOpStream(t, opRequest(op, allActions, args))
		srv.ServeStream(context.Background(), st)
		return st
	}
	list := run(ops.OpSessionsList, nil)
	if frame := string(list.frame(0)); !strings.Contains(frame, `"open"`) || strings.Contains(frame, "secret") {
		t.Fatalf("sessions_list = %s", frame)
	}
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture} {
		st := run(op, ops.SessionSnapshotArgs{Target: target("secret")})
		if f := st.errorFrame(t); f.Code != proto.CodeForbidden || string(f.Details) != `{"reason":"not_shared"}` {
			t.Fatalf("%s: error frame = %+v (%s)", op, f, f.Details)
		}
		if bytes.Contains(st.frame(0), []byte("content of secret")) {
			t.Fatalf("%s leaked the screen of an unshared session", op)
		}
	}
}

// ---- Serve over a real yamux session ----

// hubLink pairs a Hub-role and a Worker-role transport session over a pipe and
// returns the Worker's Link and the Hub's session.
func hubLink(t *testing.T) (*Link, context.CancelCauseFunc, *transport.Session) {
	t.Helper()
	a, b := net.Pipe()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Link{Session: worker, ctx: ctx}, cancel, hub
}

func hubRequest(t *testing.T, hub *transport.Session, rq any) *transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(rq); err != nil {
		t.Fatal(err)
	}
	return st
}

func readTyped(t *testing.T, st *transport.Stream) (string, json.RawMessage) {
	t.Helper()
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := st.ReadFrame()
	if err != nil {
		return "", nil
	}
	ty, _ := proto.FrameType(body)
	return ty, body
}

func TestServeAnswersOpStreamsOfALink(t *testing.T) {
	link, cancel, hub := hubLink(t)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		_ = EmitProgress(ctx, "working")
		return json.RawMessage(`{"op":"` + rq.Op + `"}`), nil
	}}
	srv := newOpServer(t, ex)
	done := make(chan struct{})
	go func() { srv.Serve(link); close(done) }()

	// Two requests at the same time on two streams.
	a := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	b := hubRequest(t, hub, opRequest(ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, nil))
	for _, c := range []struct {
		st *transport.Stream
		op string
	}{{a, ops.OpSessionsList}, {b, ops.OpSessionSnapshot}} {
		if ty, _ := readTyped(t, c.st); ty != proto.TypeOpProgress {
			t.Fatalf("first frame = %q", ty)
		}
		ty, body := readTyped(t, c.st)
		if ty != proto.TypeOpResult || !strings.Contains(string(body), c.op) {
			t.Fatalf("second frame = %q %s", ty, body)
		}
		if ty, _ := readTyped(t, c.st); ty != "" {
			t.Fatalf("a frame followed the result: %q", ty)
		}
	}
	cancel(ErrLinkLost)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the link ended")
	}
}

func TestServeRefusesStreamsBeyondTheLimit(t *testing.T) {
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	release := make(chan struct{})
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}}
	srv := newOpServer(t, ex, func(c *OpServerConfig) { c.MaxOps = 1 })
	go srv.Serve(link)

	first := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	waitFor(t, func() bool { return len(ex.requests()) == 1 })
	second := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	ty, body := readTyped(t, second)
	var f proto.ErrorFrame
	_ = json.Unmarshal(body, &f)
	if ty != proto.TypeError || f.Code != proto.CodeRateLimited {
		t.Fatalf("second stream got %q %s", ty, body)
	}
	close(release)
	if ty, _ := readTyped(t, first); ty != proto.TypeOpResult {
		t.Fatalf("first stream got %q", ty)
	}
}

// AD-12, AD-15: a read ends with the link, a write is finished and cached.
func TestLinkLossEndsReadsButNotWrites(t *testing.T) {
	link, cancel, hub := hubLink(t)
	reading, writing := make(chan error, 1), make(chan error, 1)
	started := make(chan string, 2)
	release := make(chan struct{})
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		started <- rq.Op
		if rq.Op == ops.OpSessionsList {
			<-ctx.Done()
			reading <- ctx.Err()
			return nil, ctx.Err()
		}
		<-release
		writing <- ctx.Err()
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}}
	srv := newOpServer(t, ex)
	done := make(chan struct{})
	go func() { srv.Serve(link); close(done) }()

	r := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	w := hubRequest(t, hub, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	<-started
	<-started

	cancel(ErrLinkLost)
	if err := <-reading; !errors.Is(err, context.Canceled) {
		t.Fatalf("read ctx error = %v, want canceled", err)
	}
	close(release)
	if err := <-writing; err != nil {
		t.Fatalf("the write context was cancelled by the link loss: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	_ = r
	_ = w
}

func TestServeStopsWhenTheLinkContextEnds(t *testing.T) {
	link, cancel, _ := hubLink(t)
	srv := newOpServer(t, &fakeExec{})
	done := make(chan struct{})
	go func() { srv.Serve(link); close(done) }()
	cancel(ErrLinkLost)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalActions(a, b []ops.Action) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- Hub abandons an op stream, admission across links, stop ----

// The Hub resets the op stream when its caller goes away: a blocked read gets
// its context cancelled and frees its slot, over a real yamux session.
func TestAbandonedReadIsCancelledAndFreesItsSlot(t *testing.T) {
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	cancelled := make(chan error, 1)
	var calls int
	var mu sync.Mutex
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-ctx.Done()
			cancelled <- ctx.Err()
			return nil, ctx.Err()
		}
		return json.RawMessage(`{"second":true}`), nil
	}}
	srv := newOpServer(t, ex, func(c *OpServerConfig) { c.MaxOps = 1 })
	go srv.Serve(link)

	first := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	waitFor(t, func() bool { return len(ex.requests()) == 1 })
	first.Reset() // the Hub's caller left

	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read ctx error = %v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned read was not cancelled")
	}
	// The slot is free again: with MaxOps=1 a refused request would get rate_limited.
	var body json.RawMessage
	var ty string
	waitFor(t, func() bool {
		second := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
		ty, body = readTyped(t, second)
		return ty == proto.TypeOpResult
	})
	if !strings.Contains(string(body), "second") {
		t.Fatalf("second answer = %s", body)
	}
}

// AD-15: a write whose stream the Hub abandons is not cancelled and finishes.
func TestAbandonedWriteIsNotCancelled(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ctxErr := make(chan error, 1)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		close(started)
		<-release
		ctxErr <- ctx.Err()
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}}
	srv := newOpServer(t, ex)
	st := newFakeOpStream(t, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	done := make(chan struct{})
	go func() { srv.ServeStream(context.Background(), st); close(done) }()
	<-started
	st.abandon() // the Hub resets its side
	select {
	case <-done:
		t.Fatal("ServeStream returned before the write finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-ctxErr; err != nil {
		t.Fatalf("the write context was cancelled by an abandoned stream: %v", err)
	}
	<-done
	// The write finished; its final frame went to a dead stream, which ends reset or closed.
	if !st.isClosed() {
		t.Fatal("the stream was left open")
	}
}

// A read that finishes normally ends its watcher: nothing keeps reading the
// stream afterwards.
func TestWatcherEndsWithTheFinalFrame(t *testing.T) {
	srv := newOpServer(t, &fakeExec{})
	st := newFakeOpStream(t, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	srv.ServeStream(context.Background(), st)
	if got := st.types(t); !equalStrings(got, []string{proto.TypeOpResult}) {
		t.Fatalf("frames = %v", got)
	}
	select {
	case <-st.hubEndCh(): // the deadline in the past woke the watcher's read
	case <-time.After(5 * time.Second):
		t.Fatal("the stream watcher was not stopped")
	}
}

// The op budget belongs to the server: a write that outlives its link keeps
// its slot, and the next link gets no fresh allowance.
func TestAdmissionBudgetSurvivesTheLink(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		started <- struct{}{}
		<-release
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}}
	srv := newOpServer(t, ex, func(c *OpServerConfig) { c.MaxOps = 1 })

	link1, cancel1, hub1 := hubLink(t)
	done1 := make(chan struct{})
	go func() { srv.Serve(link1); close(done1) }()
	w := hubRequest(t, hub1, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	<-started
	cancel1(ErrLinkLost) // the link is lost, the write runs on

	link2, cancel2, hub2 := hubLink(t)
	defer cancel2(nil)
	go srv.Serve(link2)
	second := hubRequest(t, hub2, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	ty, body := readTyped(t, second)
	var f proto.ErrorFrame
	_ = json.Unmarshal(body, &f)
	if ty != proto.TypeError || f.Code != proto.CodeRateLimited {
		t.Fatalf("the new link got %q %s, want rate_limited while the old write runs", ty, body)
	}
	close(release)
	waitFor(t, func() bool { return len(srv.sem) == 0 })
	_ = w
	<-done1
}

// Refused streams are bounded for their whole drain: a Hub that opens streams
// and never finishes them gets its link closed.
func TestRefusedStreamsAreBounded(t *testing.T) {
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	release := make(chan struct{})
	defer close(release)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}}
	srv := newOpServer(t, ex, func(c *OpServerConfig) { c.MaxOps = 1; c.MaxRefused = 2 })
	go srv.Serve(link)

	hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	waitFor(t, func() bool { return len(ex.requests()) == 1 })
	for i := 0; i < 8; i++ {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		st, err := hub.OpenOp(ctx)
		c()
		if err != nil {
			break // the link is already closed
		}
		_ = st.WriteFrame(opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
		// The Hub reads nothing and never closes: the refusal stays in its drain.
	}
	select {
	case <-hub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the link was not closed when the refused streams piled up")
	}
}

// A refused stream that the Hub finishes frees its slot, so ordinary load
// spikes never close the link.
func TestRefusedStreamsThatFinishFreeTheirSlot(t *testing.T) {
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	release := make(chan struct{})
	defer close(release)
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	}}
	srv := newOpServer(t, ex, func(c *OpServerConfig) { c.MaxOps = 1; c.MaxRefused = 1 })
	go srv.Serve(link)
	hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	waitFor(t, func() bool { return len(ex.requests()) == 1 })
	for i := 0; i < 10; i++ {
		st := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
		if ty, _ := readTyped(t, st); ty != proto.TypeError {
			t.Fatalf("round %d: got %q", i, ty)
		}
		_ = st.Close()
		time.Sleep(20 * time.Millisecond) // let the drain end
	}
	select {
	case <-hub.Done():
		t.Fatal("the link was closed although every refused stream finished")
	default:
	}
}

// Stop ends admission, cancels reads and waits for the writes in flight.
func TestStopWaitsForWritesAndRefusesNewStreams(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	readCancelled := make(chan struct{})
	ex := &fakeExec{run: func(ctx context.Context, rq Request) (json.RawMessage, error) {
		started <- struct{}{}
		if rq.Op == ops.OpSessionsList {
			<-ctx.Done()
			close(readCancelled)
			return nil, ctx.Err()
		}
		<-release
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}}
	srv := newOpServer(t, ex)
	link, cancel, hub := hubLink(t)
	defer cancel(nil)
	go srv.Serve(link)
	hubRequest(t, hub, opRequest(ops.OpSendText, allActions, sendText("work", "hi", false)))
	hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	<-started
	<-started

	// Stop with a short budget: the write is still running, so it reports that.
	short, c := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := srv.Stop(short)
	c()
	if err == nil {
		t.Fatal("Stop returned nil while a write was in flight")
	}
	select {
	case <-readCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the read")
	}
	// New streams are refused.
	late := hubRequest(t, hub, opRequest(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil))
	if ty, body := readTyped(t, late); ty != proto.TypeError || !strings.Contains(string(body), "shutting down") {
		t.Fatalf("a stream after Stop got %q %s", ty, body)
	}
	// With the write done, Stop returns.
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the write finished (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the write finished")
	}
}
