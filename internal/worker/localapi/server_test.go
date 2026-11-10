package localapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// pipeServer runs a server on one end of an in-memory connection pair per
// call, so the server logic is tested without a socket file.
type pipeListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }

// dial returns the client end of a new connection.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	t.Helper()
	c, s := net.Pipe()
	select {
	case l.ch <- s:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept")
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func startServer(t *testing.T, ex Executor, mut ...func(*Config)) *pipeListener {
	t.Helper()
	cfg := Config{Executor: ex, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, m := range mut {
		m(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l := newPipeListener()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return l
}

func sendRequest(t *testing.T, c net.Conn, v any) {
	t.Helper()
	go func() { _ = proto.WriteFrame(c, proto.StreamFrameLimit, v) }()
}

func request(op string, args any) RequestFrame {
	var raw json.RawMessage
	if args != nil {
		raw, _ = json.Marshal(args)
	}
	return RequestFrame{Type: proto.TypeOpRequest, Op: op, Args: raw}
}

// readAll reads frames until the server closes the connection.
func readAll(t *testing.T, c net.Conn) []json.RawMessage {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var out []json.RawMessage
	for {
		b, err := proto.ReadFrame(c, proto.StreamFrameLimit)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("ReadFrame: %v (frames so far %s)", err, out)
		}
		out = append(out, b)
	}
}

func frameTypes(t *testing.T, frames []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, f := range frames {
		typ, err := proto.FrameType(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, typ)
	}
	return out
}

func echoExecutor() Executor {
	return ExecutorFunc(func(_ context.Context, rq Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"op": rq.Op, "args": rq.Args, "actions": rq.Actions})
	})
}

func TestRoundTripAnswersOpResultThenCloses(t *testing.T) {
	l := startServer(t, echoExecutor())
	c := l.dial(t)
	sendRequest(t, c, request("sessions_list", map[string]any{"x": 1}))

	frames := readAll(t, c)
	if got, want := frameTypes(t, frames), []string{"op.result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	var res ResultFrame
	if err := proto.DecodeFrame(frames[0], &res); err != nil {
		t.Fatal(err)
	}
	var echo struct {
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(res.Result, &echo); err != nil {
		t.Fatal(err)
	}
	if echo.Op != "sessions_list" || string(echo.Args) != `{"x":1}` {
		t.Fatalf("result = %s", res.Result)
	}
}

func TestLocalOwnerHoldsEveryActionWhateverTheRequestSays(t *testing.T) {
	var got []ops.Action
	ex := ExecutorFunc(func(_ context.Context, rq Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		got = rq.Actions
		return json.RawMessage(`{}`), nil
	})
	l := startServer(t, ex)
	c := l.dial(t)
	// A request that tries to narrow or name actions is ignored: the local OS
	// user is the owner.
	sendRequest(t, c, map[string]any{"type": "op.request", "op": "sessions_list", "actions": []string{"read"}})
	readAll(t, c)
	if !reflect.DeepEqual(got, FullActions()) {
		t.Fatalf("actions = %v, want %v", got, FullActions())
	}
	for _, a := range []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage} {
		if !slicesContain(got, a) {
			t.Errorf("missing action %s", a)
		}
	}
}

func slicesContain(s []ops.Action, a ops.Action) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

func TestProgressFramesPrecedeTheResult(t *testing.T) {
	ex := ExecutorFunc(func(_ context.Context, _ Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`{"n":1}`))
		progress(json.RawMessage(`{"n":2}`))
		progress(nil)
		return nil, nil
	})
	l := startServer(t, ex)
	c := l.dial(t)
	sendRequest(t, c, request("pipe_wait", nil))

	frames := readAll(t, c)
	if got, want := frameTypes(t, frames), []string{"op.progress", "op.progress", "op.progress", "op.result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
	var p ProgressFrame
	if err := proto.DecodeFrame(frames[1], &p); err != nil || string(p.Data) != `{"n":2}` {
		t.Fatalf("second progress = %s (%v)", frames[1], err)
	}
	var r ResultFrame
	if err := proto.DecodeFrame(frames[3], &r); err != nil || string(r.Result) != "null" {
		t.Fatalf("empty result = %s (%v)", frames[3], err)
	}
}

type wireErr struct{}

func (wireErr) Error() string { return "wire" }
func (wireErr) ProtoError() *proto.Error {
	return &proto.Error{Code: proto.CodeBusy, Message: "busy now", Details: json.RawMessage(`{"reason":"x"}`)}
}

func TestErrorsAreMappedToErrorFrames(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code proto.Code
		msg  string // substring that must be in the message
		deny string // substring that must not leak
	}{
		{"proto error", proto.NewError(proto.CodeForbidden, "no way"), proto.CodeForbidden, "no way", ""},
		{"wrapped proto error", errors.Join(errors.New("ctx"), proto.NewError(proto.CodeNotFound, "gone")), proto.CodeNotFound, "gone", ""},
		{"ProtoError method", wireErr{}, proto.CodeBusy, "busy now", ""},
		{"internal detail does not leak", errors.New("open /secret/path: boom"), proto.CodeInternal, "failed on the Worker", "/secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := startServer(t, ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
				return nil, tc.err
			}))
			c := l.dial(t)
			sendRequest(t, c, request("send_text", nil))
			frames := readAll(t, c)
			if got := frameTypes(t, frames); !reflect.DeepEqual(got, []string{"error"}) {
				t.Fatalf("frames = %v", got)
			}
			var ef proto.ErrorFrame
			if err := proto.DecodeFrame(frames[0], &ef); err != nil {
				t.Fatal(err)
			}
			if ef.Code != tc.code || !strings.Contains(ef.Message, tc.msg) {
				t.Fatalf("error = %+v", ef)
			}
			if tc.deny != "" && bytes.Contains(frames[0], []byte(tc.deny)) {
				t.Fatalf("frame leaks %q: %s", tc.deny, frames[0])
			}
		})
	}
}

func TestErrorAfterProgressEndsWithOneErrorFrame(t *testing.T) {
	ex := ExecutorFunc(func(_ context.Context, _ Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`1`))
		return nil, proto.NewError(proto.CodeTargetNotFound, "gone")
	})
	l := startServer(t, ex)
	c := l.dial(t)
	sendRequest(t, c, request("pipe_wait", nil))
	if got, want := frameTypes(t, readAll(t, c)), []string{"op.progress", "error"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("frames = %v, want %v", got, want)
	}
}

func TestAPanicInTheExecutorIsAnInternalError(t *testing.T) {
	l := startServer(t, ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
		panic("kaboom")
	}))
	c := l.dial(t)
	sendRequest(t, c, request("sessions_list", nil))
	frames := readAll(t, c)
	var ef proto.ErrorFrame
	if len(frames) != 1 || proto.DecodeFrame(frames[0], &ef) != nil || ef.Code != proto.CodeInternal || strings.Contains(ef.Message, "kaboom") {
		t.Fatalf("frames = %s", frames)
	}
}

func TestOversizedResultIsAnErrorFrame(t *testing.T) {
	l := startServer(t, ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
		return json.Marshal(strings.Repeat("a", proto.StreamFrameLimit))
	}))
	c := l.dial(t)
	sendRequest(t, c, request("session_capture", nil))
	frames := readAll(t, c)
	var ef proto.ErrorFrame
	if len(frames) != 1 || proto.DecodeFrame(frames[0], &ef) != nil || ef.Code != proto.CodeFrameTooLarge {
		t.Fatalf("frames = %s", frames)
	}
}

func TestBadRequestsAreRefusedWithoutCallingTheExecutor(t *testing.T) {
	called := make(chan struct{}, 1)
	ex := ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
		called <- struct{}{}
		return nil, nil
	})
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, proto.StreamFrameLimit+1)
	raw := func(b []byte) func(net.Conn) { return func(c net.Conn) { go func() { _, _ = c.Write(b) }() } }
	frame := func(s string) []byte {
		b := make([]byte, 4, 4+len(s))
		binary.BigEndian.PutUint32(b, uint32(len(s)))
		return append(b, s...)
	}
	cases := []struct {
		name string
		send func(net.Conn)
		code proto.Code
	}{
		{"wrong frame type", func(c net.Conn) { sendRequest(t, c, map[string]any{"type": "ping"}) }, proto.CodeInvalidArgument},
		{"no op", func(c net.Conn) { sendRequest(t, c, map[string]any{"type": "op.request"}) }, proto.CodeInvalidArgument},
		{"not json", raw(frame("not json")), proto.CodeInvalidArgument},
		{"no type field", raw(frame(`{"op":"x"}`)), proto.CodeInvalidArgument},
		{"op of the wrong type", raw(frame(`{"type":"op.request","op":5}`)), proto.CodeInvalidArgument},
		{"oversized frame", raw(oversize), proto.CodeFrameTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := startServer(t, ex)
			c := l.dial(t)
			tc.send(c)
			frames := readAll(t, c)
			var ef proto.ErrorFrame
			if len(frames) != 1 || proto.DecodeFrame(frames[0], &ef) != nil || ef.Type != "error" || ef.Code != tc.code {
				t.Fatalf("frames = %s, want error %s", frames, tc.code)
			}
		})
	}
	select {
	case <-called:
		t.Fatal("the executor ran for a bad request")
	default:
	}
}

func TestAConnectionThatLeavesWithoutARequestIsIgnored(t *testing.T) {
	l := startServer(t, echoExecutor())
	c := l.dial(t)
	_ = c.Close()
	// The server is still alive.
	c2 := l.dial(t)
	sendRequest(t, c2, request("sessions_list", nil))
	if got := frameTypes(t, readAll(t, c2)); !reflect.DeepEqual(got, []string{"op.result"}) {
		t.Fatalf("frames = %v", got)
	}
}

func TestASilentConnectionIsDroppedAfterTheRequestTimeout(t *testing.T) {
	l := startServer(t, echoExecutor(), func(c *Config) { c.RequestTimeout = 50 * time.Millisecond })
	c := l.dial(t)
	// The server drops the connection without sending a request answer.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := proto.ReadFrame(c, proto.StreamFrameLimit); err == nil {
		t.Fatal("expected the connection to end")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the server did not drop the silent connection: %v", err)
	}
}

func TestDisconnectCancelsTheOperation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	ex := ExecutorFunc(func(ctx context.Context, _ Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	l := startServer(t, ex)
	c := l.dial(t)
	sendRequest(t, c, request("pipe_wait", nil))
	<-started
	_ = c.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the operation was not cancelled after the client left")
	}
}

func TestCloseCancelsOperationsInFlightAndWaits(t *testing.T) {
	started := make(chan struct{})
	ex := ExecutorFunc(func(ctx context.Context, _ Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	srv, err := New(Config{Executor: ex})
	if err != nil {
		t.Fatal(err)
	}
	l := newPipeListener()
	go func() { _ = srv.Serve(l) }()
	c := l.dial(t)
	sendRequest(t, c, request("pipe_wait", nil))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if got := readAll(t, c); len(got) != 0 {
		t.Fatalf("frames after shutdown = %s", got)
	}
}

func TestCloseReportsAnOperationThatIgnoresItsContext(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ex := ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	})
	srv, _ := New(Config{Executor: ex})
	l := newPipeListener()
	go func() { _ = srv.Serve(l) }()
	c := l.dial(t)
	sendRequest(t, c, request("x", nil))
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.Close(ctx); err == nil {
		t.Fatal("Close returned nil although an operation is still running")
	}
	close(release)
}

func TestTooManyConnectionsAreRefused(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	ex := ExecutorFunc(func(context.Context, Request, func(json.RawMessage)) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	})
	l := startServer(t, ex, func(c *Config) { c.MaxConns = 1 })
	c1 := l.dial(t)
	sendRequest(t, c1, request("x", nil))
	<-started

	c2 := l.dial(t)
	frames := readAll(t, c2)
	var ef proto.ErrorFrame
	if len(frames) != 1 || proto.DecodeFrame(frames[0], &ef) != nil || ef.Code != proto.CodeRateLimited {
		t.Fatalf("second connection got %s", frames)
	}
	close(release)
	if got := frameTypes(t, readAll(t, c1)); !reflect.DeepEqual(got, []string{"op.result"}) {
		t.Fatalf("first connection frames = %v", got)
	}
}

func TestConcurrentRequestsAreIndependent(t *testing.T) {
	l := startServer(t, ExecutorFunc(func(_ context.Context, rq Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(rq.Args)
		return rq.Args, nil
	}))
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := l.dial(t)
			arg := map[string]int{"i": i}
			sendRequest(t, c, request("echo", arg))
			frames := readAll(t, c)
			want, _ := json.Marshal(arg)
			var r ResultFrame
			if len(frames) != 2 || proto.DecodeFrame(frames[1], &r) != nil || string(r.Result) != string(want) {
				t.Errorf("request %d got %s", i, frames)
			}
		}()
	}
	wg.Wait()
}

func TestNewNeedsAnExecutorAndServeRunsOnce(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without an executor succeeded")
	}
	srv, _ := New(Config{Executor: echoExecutor()})
	l1 := newPipeListener()
	go func() { _ = srv.Serve(l1) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		serving := srv.listener != nil
		srv.mu.Unlock()
		if serving {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := srv.Serve(newPipeListener()); err == nil {
		t.Fatal("second Serve succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Close(ctx)
	if err := srv.Serve(newPipeListener()); err != nil {
		t.Fatalf("Serve after Close = %v, want nil", err)
	}
}

// skipOnWindows skips a test that compares literal unix paths: SocketPath uses
// the host's path rules, under which "/run/user/1000" is not absolute on
// Windows. The Windows pipe path is tested in pipe_test.go.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("compares literal unix paths; the Windows pipe path is tested in pipe_test.go")
	}
}

func TestSocketPathIsInTheRuntimeDirectory(t *testing.T) {
	skipOnWindows(t)
	env := func(goos, xdg string) config.PathEnv {
		return config.PathEnv{
			GOOS:    goos,
			Getenv:  func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": xdg}[k] },
			HomeDir: func() (string, error) { return "/home/u", nil },
			TempDir: func() string { return "/tmp" },
			UID:     1000,
		}
	}
	cases := []struct {
		name string
		env  config.PathEnv
		want string
	}{
		{"linux with XDG_RUNTIME_DIR", env("linux", "/run/user/1000"), "/run/user/1000/minion/worker.sock"},
		{"linux fallback", env("linux", ""), "/tmp/minion-1000/worker.sock"},
		{"linux relative XDG_RUNTIME_DIR is ignored", env("linux", "run"), "/tmp/minion-1000/worker.sock"},
	}
	for _, tc := range cases {
		got, err := SocketPath(tc.env)
		if err != nil || got != tc.want {
			t.Errorf("%s: SocketPath = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestSocketCandidatesListTheXDGPathFirstThenTheFallbackWithoutDuplicates(t *testing.T) {
	skipOnWindows(t)
	env := func(goos, xdg string) config.PathEnv {
		return config.PathEnv{
			GOOS:    goos,
			Getenv:  func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": xdg}[k] },
			HomeDir: func() (string, error) { return "/home/u", nil },
			TempDir: func() string { return "/tmp" },
			UID:     1000,
		}
	}
	cases := []struct {
		name string
		env  config.PathEnv
		want []string
	}{
		{"linux with XDG_RUNTIME_DIR", env("linux", "/run/user/1000"), []string{"/run/user/1000/minion/worker.sock", "/tmp/minion-1000/worker.sock"}},
		{"linux without XDG_RUNTIME_DIR", env("linux", ""), []string{"/tmp/minion-1000/worker.sock"}},
		{"linux relative XDG_RUNTIME_DIR", env("linux", "run"), []string{"/tmp/minion-1000/worker.sock"}},
		{"darwin ignores XDG_RUNTIME_DIR", env("darwin", "/run/user/1000"), []string{"/tmp/minion-1000/worker.sock"}},
	}
	for _, tc := range cases {
		got, err := SocketCandidates(tc.env)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: SocketCandidates = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestAProgressElementThatCannotBeSentEndsTheOperationWithAnError(t *testing.T) {
	cases := map[string]json.RawMessage{
		"oversized": json.RawMessage(`"` + strings.Repeat("a", proto.StreamFrameLimit) + `"`),
		"malformed": json.RawMessage(`{"broken":`),
	}
	for name, elem := range cases {
		t.Run(name, func(t *testing.T) {
			sawCancel := make(chan bool, 1)
			ex := ExecutorFunc(func(ctx context.Context, _ Request, progress func(json.RawMessage)) (json.RawMessage, error) {
				progress(json.RawMessage(`1`))
				progress(elem)
				select {
				case <-ctx.Done():
					sawCancel <- true
				case <-time.After(time.Second):
					sawCancel <- false
				}
				return json.RawMessage(`"success"`), nil // an executor that ignores the failure
			})
			l := startServer(t, ex)
			c := l.dial(t)
			sendRequest(t, c, request("pipe_wait", nil))
			frames := readAll(t, c)
			if got, want := frameTypes(t, frames), []string{"op.progress", "error"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("frames = %v, want %v (no op.result after a lost progress element)", got, want)
			}
			var ef proto.ErrorFrame
			if err := proto.DecodeFrame(frames[1], &ef); err != nil || ef.Code != proto.CodeInternal {
				t.Fatalf("error frame = %s (%v)", frames[1], err)
			}
			if !<-sawCancel {
				t.Fatal("the operation was not cancelled after its progress element was lost")
			}
		})
	}
}

func TestCloseInterruptsAConnectionThatHasNotSentItsRequest(t *testing.T) {
	srv, err := New(Config{Executor: echoExecutor()}) // default 10 s request timeout
	if err != nil {
		t.Fatal(err)
	}
	l := newPipeListener()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	// An accepted connection that sends only the first bytes of a frame.
	c := l.dial(t)
	go func() { _, _ = c.Write([]byte{0, 0, 0, 20, '{'}) }()
	time.Sleep(100 * time.Millisecond) // let the server accept and start reading

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Close(ctx); err != nil {
		t.Fatalf("Close with an incomplete request pending: %v (after %v)", err, time.Since(start))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQualifiedOpRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		op   string
		need []string
		wire string
	}{
		{"send_text", nil, "send_text"},
		{"answer", []string{"answer.reviewed"}, "answer+answer.reviewed"},
		{"send_text", []string{"send_text.submit", "text.verbatim"}, "send_text+send_text.submit+text.verbatim"},
	} {
		if got := QualifiedOp(tc.op, tc.need); got != tc.wire {
			t.Errorf("QualifiedOp(%s, %v) = %q, want %q", tc.op, tc.need, got, tc.wire)
		}
		op, need := SplitOp(tc.wire)
		if op != tc.op || strings.Join(need, ",") != strings.Join(tc.need, ",") {
			t.Errorf("SplitOp(%q) = %q, %v", tc.wire, op, need)
		}
	}
}
