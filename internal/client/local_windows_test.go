//go:build windows

package client

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// startPipeWorker serves ex on a control pipe of its own (a valid control
// pipe name with a random SID, so that parallel tests and a real Worker never
// meet) with the DACL of the user's pipe, and returns the pipe name.
func startPipeWorker(t *testing.T, ex localapi.Executor) string {
	t.Helper()
	var r [4]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal(err)
	}
	name, err := localapi.PipeName(fmt.Sprintf("S-1-5-21-%d-%d", binary.BigEndian.Uint32(r[:]), os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := localapi.PipeSDDL(u.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	l, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: ex, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return name
}

// AC: the local client connects to the user's named pipe and runs ops over it
// exactly as over a unix socket.
func TestLocalCallsTheWorkerOverTheNamedPipe(t *testing.T) {
	name := startPipeWorker(t, localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`{"n":1}`))
		progress(json.RawMessage(`{"n":2}`))
		return json.RawMessage(fmt.Sprintf(`{"op":%q,"args":%s}`, rq.Op, rq.Args)), nil
	}))
	b := Local(WithSocketPath(name))

	res, err := b.Call(context.Background(), "sessions_list", map[string]any{"all": true})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		Op   string
		Args map[string]any
	}
	if err := json.Unmarshal(res, &got); err != nil || got.Op != "sessions_list" || got.Args["all"] != true {
		t.Fatalf("result = %s (%v)", res, err)
	}

	var progress []string
	if _, err := b.Stream(context.Background(), "pipe_wait", nil, func(d json.RawMessage) error {
		progress = append(progress, string(d))
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(progress) != 2 || progress[0] != `{"n":1}` || progress[1] != `{"n":2}` {
		t.Fatalf("progress = %v", progress)
	}
}

func TestLocalWithoutAWorkerReportsNotRunning(t *testing.T) {
	var r [4]byte
	_, _ = rand.Read(r[:])
	name, _ := localapi.PipeName(fmt.Sprintf("S-1-5-21-%d", binary.BigEndian.Uint32(r[:])))
	_, err := Local(WithSocketPath(name), WithDialTimeout(300*time.Millisecond)).Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrWorkerNotRunning) {
		t.Fatalf("err = %v, want ErrWorkerNotRunning", err)
	}
}

// A name that is not a Minion control pipe is never dialled.
func TestLocalRefusesAPipeThatIsNotAControlPipe(t *testing.T) {
	_, err := Local(WithSocketPath(`\\.\pipe\not-minion`)).Call(context.Background(), "sessions_list", nil)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestLocalHonoursACancelledContext(t *testing.T) {
	name := startPipeWorker(t, localapi.ExecutorFunc(func(ctx context.Context, _ localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Local(WithSocketPath(name)).Call(ctx, "pipe_wait", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context error", err)
	}
}

func TestMapDialError(t *testing.T) {
	const path = `\\.\pipe\minion-worker-S-1-5-21-1-2-3-4`
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		is   error
	}{
		{"missing pipe", live, windows.ERROR_FILE_NOT_FOUND, ErrWorkerNotRunning},
		{"missing path", live, windows.ERROR_PATH_NOT_FOUND, ErrWorkerNotRunning},
		{"access denied", live, windows.ERROR_ACCESS_DENIED, ErrPermissionDenied},
		{"cancelled", cancelled, windows.ERROR_FILE_NOT_FOUND, context.Canceled},
	} {
		if got := mapDialError(c.ctx, path, c.err); !errors.Is(got, c.is) {
			t.Errorf("%s: %v, want %v", c.name, got, c.is)
		}
	}
	// A busy pipe that never frees up is a Worker that does not answer: not
	// "not running", and not a permission problem.
	for _, e := range []error{context.DeadlineExceeded, fmt.Errorf("pipe: %w", context.DeadlineExceeded)} {
		got := mapDialError(live, path, e)
		if errors.Is(got, ErrWorkerNotRunning) || errors.Is(got, ErrPermissionDenied) || got == nil {
			t.Errorf("%v mapped to %v", e, got)
		}
	}
	if got := mapDialError(live, path, windows.ERROR_BROKEN_PIPE); errors.Is(got, ErrWorkerNotRunning) || !errors.Is(got, windows.ERROR_BROKEN_PIPE) {
		t.Errorf("other error: %v", got)
	}
}

// A guarded request travels under its capability-qualified name over the named
// pipe, exactly as over a unix socket (the client code is the same): a Worker
// that predates the feature does not know the name and refuses it as an
// unsupported operation, the client reports ErrWorkerTooOld, and a plain request
// is untouched. (Compiled and vetted on every runner; run by the Windows job.)
func TestLocalQualifiesGuardedRequestsOverTheNamedPipe(t *testing.T) {
	var ran []string
	name := startPipeWorker(t, localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		if rq.Op != "send_text" && rq.Op != "sessions_list" { // an older Worker knows plain names only
			return nil, &proto.Error{Code: proto.CodeUnsupportedOp, Message: "the Worker does not implement this operation"}
		}
		ran = append(ran, rq.Op)
		return json.RawMessage(`{"outcome":"delivered"}`), nil
	}))
	b := Local(WithSocketPath(name))
	_, err := b.Call(context.Background(), "send_text", map[string]any{"text": "hello", "submit": true})
	if !errors.Is(err, ErrWorkerTooOld) {
		t.Fatalf("a submitting send_text to an older Worker: %v, want ErrWorkerTooOld", err)
	}
	if _, err := b.Call(context.Background(), "send_text", map[string]any{"text": "plain line"}); err != nil {
		t.Fatalf("a plain send_text: %v", err)
	}
	if len(ran) != 1 {
		t.Fatalf("the older Worker ran %v, want the plain text only", ran)
	}
}
