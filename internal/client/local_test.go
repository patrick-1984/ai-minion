//go:build unix

package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// startWorker serves ex on a private socket through the real localapi server
// and returns the socket path.
func startWorker(t *testing.T, ex localapi.Executor) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "minion", localapi.SocketName)
	l, err := localapi.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: ex})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Serve(l); close(done) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
		<-done
	})
	return path
}

func TestLocalImplementsBackend(t *testing.T) {
	var _ Backend = Local()
}

func TestCallRoundTripsArgsAndResult(t *testing.T) {
	var gotOp string
	var gotArgs json.RawMessage
	var gotActions []ops.Action
	path := startWorker(t, localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		gotOp, gotArgs, gotActions = rq.Op, rq.Args, rq.Actions
		return json.RawMessage(`{"sessions":[{"name":"minion-1"}]}`), nil
	}))
	b := Local(WithSocketPath(path))

	res, err := b.Call(context.Background(), ops.OpSessionsList, map[string]any{"limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"sessions":[{"name":"minion-1"}]}` {
		t.Fatalf("result = %s", res)
	}
	if gotOp != "sessions_list" || string(gotArgs) != `{"limit":3}` {
		t.Fatalf("op %q args %s", gotOp, gotArgs)
	}
	if !reflect.DeepEqual(gotActions, localapi.FullActions()) {
		t.Fatalf("actions = %v", gotActions)
	}
}

func TestCallAcceptsStructRawNilAndBytesArgs(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	path := startWorker(t, localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		mu.Lock()
		seen = append(seen, string(rq.Args))
		mu.Unlock()
		return json.RawMessage(`{}`), nil
	}))
	b := Local(WithSocketPath(path))
	type a struct {
		X int `json:"x"`
	}
	for _, args := range []any{a{X: 1}, json.RawMessage(`{"y":2}`), []byte(`{"z":3}`), nil} {
		if _, err := b.Call(context.Background(), "x", args); err != nil {
			t.Fatalf("args %T: %v", args, err)
		}
	}
	want := []string{`{"x":1}`, `{"y":2}`, `{"z":3}`, ``}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("args seen = %q, want %q", seen, want)
	}
	if _, err := b.Call(context.Background(), "x", make(chan int)); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("unencodable args: %v", err)
	}
	if _, err := b.Call(context.Background(), "", nil); err == nil {
		t.Fatal("empty op accepted")
	}
}

func TestErrorFramesBecomeProtoErrors(t *testing.T) {
	path := startWorker(t, localapi.ExecutorFunc(func(context.Context, localapi.Request, func(json.RawMessage)) (json.RawMessage, error) {
		return nil, &proto.Error{Code: proto.CodeForbidden, Message: "preview only", Details: json.RawMessage(`{"reason":"preview_only"}`)}
	}))
	_, err := Local(WithSocketPath(path)).Call(context.Background(), ops.OpSendText, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeForbidden || pe.Message != "preview only" || string(pe.Details) != `{"reason":"preview_only"}` {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, &proto.Error{Code: proto.CodeForbidden}) {
		t.Fatal("errors.Is on the code failed")
	}
}

func TestStreamDeliversProgressInOrderThenTheResult(t *testing.T) {
	path := startWorker(t, localapi.ExecutorFunc(func(_ context.Context, _ localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		for i := 1; i <= 3; i++ {
			progress(json.RawMessage(`{"turn":` + string(rune('0'+i)) + `}`))
		}
		return json.RawMessage(`{"done":true}`), nil
	}))
	var got []string
	res, err := Local(WithSocketPath(path)).Stream(context.Background(), ops.OpPipeWait, nil, func(m json.RawMessage) error {
		got = append(got, string(m))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`{"turn":1}`, `{"turn":2}`, `{"turn":3}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %v, want %v", got, want)
	}
	if string(res) != `{"done":true}` {
		t.Fatalf("result = %s", res)
	}
}

func TestProgressArrivesBeforeTheOperationFinishes(t *testing.T) {
	release := make(chan struct{})
	path := startWorker(t, localapi.ExecutorFunc(func(_ context.Context, _ localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`"first"`))
		<-release
		return nil, nil
	}))
	first := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		_, err := Local(WithSocketPath(path)).Stream(context.Background(), "pipe_wait", nil, func(m json.RawMessage) error {
			first <- string(m)
			return nil
		})
		errc <- err
	}()
	select {
	case got := <-first:
		if got != `"first"` {
			t.Fatalf("progress = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the progress element did not arrive while the op was still running")
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestCallDiscardsProgressAndStreamErrorAfterProgress(t *testing.T) {
	path := startWorker(t, localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`1`))
		if rq.Op == "fail" {
			return nil, proto.NewError(proto.CodeTargetNotFound, "gone")
		}
		return json.RawMessage(`"ok"`), nil
	}))
	b := Local(WithSocketPath(path))
	if res, err := b.Call(context.Background(), "pipe_wait", nil); err != nil || string(res) != `"ok"` {
		t.Fatalf("Call = %s, %v", res, err)
	}
	var n int
	_, err := b.Stream(context.Background(), "fail", nil, func(json.RawMessage) error { n++; return nil })
	if proto.CodeOf(err) != proto.CodeTargetNotFound || n != 1 {
		t.Fatalf("err = %v after %d progress", err, n)
	}
}

func TestAnErrorFromTheProgressCallbackStopsTheStream(t *testing.T) {
	cancelled := make(chan struct{})
	path := startWorker(t, localapi.ExecutorFunc(func(ctx context.Context, _ localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`1`))
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}))
	stop := errors.New("enough")
	_, err := Local(WithSocketPath(path)).Stream(context.Background(), "pipe_wait", nil, func(json.RawMessage) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the Worker kept the operation running after the client stopped")
	}
}

func TestContextCancelStopsAStream(t *testing.T) {
	started := make(chan struct{})
	path := startWorker(t, localapi.ExecutorFunc(func(ctx context.Context, _ localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := Local(WithSocketPath(path)).Call(ctx, "pipe_wait", nil)
		errc <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call did not return after the context ended")
	}
}

func TestNoWorkerGivesAClearError(t *testing.T) {
	dir := shortDir(t)
	// No socket file at all.
	_, err := Local(WithSocketPath(filepath.Join(dir, "none.sock"))).Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrWorkerNotRunning) || !strings.Contains(err.Error(), "none.sock") {
		t.Fatalf("missing socket: %v", err)
	}
	// A stale socket file with nobody behind it.
	stale := filepath.Join(dir, "stale.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := Local(WithSocketPath(stale)).Call(context.Background(), "sessions_list", nil); !errors.Is(err, ErrWorkerNotRunning) {
		t.Fatalf("stale socket: %v", err)
	}
}

func TestSocketOfAnotherUserGivesPermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can always connect")
	}
	path := startWorker(t, localapi.ExecutorFunc(func(context.Context, localapi.Request, func(json.RawMessage)) (json.RawMessage, error) {
		return nil, nil
	}))
	// What a second user sees: no access to the socket file.
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err := Local(WithSocketPath(path)).Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v", err)
	}
}

// isolateRuntimeDirs points both inputs of the default socket lookup at
// private directories: XDG_RUNTIME_DIR (honoured on Linux only) and TMPDIR (the
// fallback, and the only runtime directory on macOS). It returns them.
func isolateRuntimeDirs(t *testing.T) (xdg, tmp string) {
	t.Helper()
	xdg, tmp = shortDir(t), shortDir(t)
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	t.Setenv("TMPDIR", tmp)
	return xdg, tmp
}

// serveAt runs a real localapi server on the socket at path.
func serveAt(t *testing.T, path, answer string) {
	t.Helper()
	l, err := localapi.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: localapi.ExecutorFunc(func(context.Context, localapi.Request, func(json.RawMessage)) (json.RawMessage, error) {
		return json.RawMessage(answer), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Serve(l); close(done) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
		<-done
	})
}

func TestDefaultSocketPathFollowsXDGRuntimeDir(t *testing.T) {
	isolateRuntimeDirs(t)
	cands, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && !strings.HasPrefix(cands[0], os.Getenv("XDG_RUNTIME_DIR")) {
		t.Fatalf("first candidate %s is not below XDG_RUNTIME_DIR", cands[0])
	}
	serveAt(t, cands[0], `"found"`)

	res, err := Local().Call(context.Background(), "sessions_list", nil)
	if err != nil || string(res) != `"found"` {
		t.Fatalf("Call = %s, %v", res, err)
	}
}

// A Worker started without XDG_RUNTIME_DIR (a system service) listens below
// the temp directory, while the login shell of its user has XDG_RUNTIME_DIR.
func TestLocalFindsAWorkerThatStartedWithoutXDGRuntimeDir(t *testing.T) {
	isolateRuntimeDirs(t)
	cands, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && len(cands) != 2 {
		t.Fatalf("candidates = %v, want the XDG path and the fallback", cands)
	}
	fallback := cands[len(cands)-1]
	serveAt(t, fallback, `"from the fallback"`)

	res, err := Local().Call(context.Background(), "sessions_list", nil)
	if err != nil || string(res) != `"from the fallback"` {
		t.Fatalf("Call = %s, %v", res, err)
	}
}

func TestNoDefaultWorkerNamesEveryPathTried(t *testing.T) {
	isolateRuntimeDirs(t)
	cands, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Local().Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrWorkerNotRunning) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range cands {
		if !strings.Contains(err.Error(), c) {
			t.Errorf("error %q does not name %s", err, c)
		}
	}
}

// A stale socket file at the first candidate does not hide a live Worker at
// the next one.
func TestLocalSkipsAStaleCandidate(t *testing.T) {
	isolateRuntimeDirs(t)
	cands, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) < 2 {
		t.Skip("one candidate on this platform")
	}
	if err := os.MkdirAll(filepath.Dir(cands[0]), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: cands[0], Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()
	serveAt(t, cands[1], `"live"`)

	res, err := Local().Call(context.Background(), "sessions_list", nil)
	if err != nil || string(res) != `"live"` {
		t.Fatalf("Call = %s, %v", res, err)
	}
}

// listenUnaccepted serves path and reports every connection it gets; it
// answers nothing.
func listenUnaccepted(t *testing.T, path string) *atomic.Int32 {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return &n
}

// The client never sends a request to a socket whose directory other users can
// reach: another user could have planted it.
func TestLocalRefusesASocketInADirectoryOpenToOthers(t *testing.T) {
	for _, m := range []os.FileMode{0o755, 0o770, 0o707} {
		dir := filepath.Join(shortDir(t), "minion")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, localapi.SocketName)
		connections := listenUnaccepted(t, path)
		if err := os.Chmod(dir, m); err != nil {
			t.Fatal(err)
		}
		_, err := Local(WithSocketPath(path)).Call(context.Background(), ops.OpSendText, map[string]string{"text": "secret"})
		if !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("mode %o: err = %v, want ErrPermissionDenied", m, err)
		}
		time.Sleep(50 * time.Millisecond)
		if n := connections.Load(); n != 0 {
			t.Fatalf("mode %o: the client connected %d time(s) to an untrusted socket", m, n)
		}
	}
}

// The same refusal applies to the default candidates: a socket planted at the
// temp-directory fallback is not used, and the error says so.
func TestLocalRefusesAnUntrustedDefaultCandidate(t *testing.T) {
	isolateRuntimeDirs(t)
	cands, err := localapi.SocketCandidates(config.SystemPathEnv())
	if err != nil {
		t.Fatal(err)
	}
	last := cands[len(cands)-1]
	if err := os.Mkdir(filepath.Dir(last), 0o700); err != nil {
		t.Fatal(err)
	}
	connections := listenUnaccepted(t, last)
	if err := os.Chmod(filepath.Dir(last), 0o777); err != nil {
		t.Fatal(err)
	}
	_, err = Local().Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrPermissionDenied) || !strings.Contains(err.Error(), last) {
		t.Fatalf("err = %v, want ErrPermissionDenied naming %s", err, last)
	}
	time.Sleep(50 * time.Millisecond)
	if n := connections.Load(); n != 0 {
		t.Fatalf("the client connected %d time(s) to an untrusted socket", n)
	}
}

// rawServer answers each connection with the bytes the test chooses.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "raw.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); handle(c) }()
		}
	}()
	return path
}

func frameBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

func TestConnectionLostBeforeTheResultIsReported(t *testing.T) {
	cases := map[string]func(net.Conn){
		"closed at once":        func(net.Conn) {},
		"closed after progress": func(c net.Conn) { _, _ = c.Write(frameBytes(t, map[string]any{"type": "op.progress", "data": 1})) },
		"cut inside a frame":    func(c net.Conn) { _, _ = c.Write([]byte{0, 0, 0, 20, '{'}) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			path := rawServer(t, func(c net.Conn) { _, _ = proto.ReadFrame(c, proto.StreamFrameLimit); h(c) })
			_, err := Local(WithSocketPath(path)).Call(context.Background(), ops.OpSendText, nil)
			if !errors.Is(err, ErrConnectionLost) {
				t.Fatalf("err = %v, want ErrConnectionLost", err)
			}
		})
	}
}

func TestUnexpectedFramesAreProtocolErrors(t *testing.T) {
	cases := map[string][]byte{
		"unknown type":      frameBytes(t, map[string]any{"type": "hello"}),
		"not an object":     append([]byte{0, 0, 0, 2}, '[', ']'),
		"progress then end": frameBytes(t, map[string]any{"type": "op.progress", "data": 1, "extra": nil, "type2": 1}),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			path := rawServer(t, func(c net.Conn) { _, _ = proto.ReadFrame(c, proto.StreamFrameLimit); _, _ = c.Write(payload) })
			_, err := Local(WithSocketPath(path)).Call(context.Background(), "x", nil)
			switch name {
			case "progress then end":
				// A valid progress frame followed by the end of the stream.
				if !errors.Is(err, ErrConnectionLost) {
					t.Fatalf("err = %v, want ErrConnectionLost", err)
				}
			default:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("err = %v, want ErrProtocol", err)
				}
			}
		})
	}
}

func TestAnOversizedRequestIsRefusedBeforeSending(t *testing.T) {
	path := startWorker(t, localapi.ExecutorFunc(func(context.Context, localapi.Request, func(json.RawMessage)) (json.RawMessage, error) {
		t.Error("the Worker received an oversized request")
		return nil, nil
	}))
	huge := map[string]string{"text": strings.Repeat("a", proto.StreamFrameLimit)}
	_, err := Local(WithSocketPath(path)).Call(context.Background(), "send_text", huge)
	if !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
}
