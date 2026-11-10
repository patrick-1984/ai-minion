//go:build windows

package localapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// testPipe returns a pipe name of its own, a valid control pipe name with a
// random SID, and the security descriptor [Listen] would give the pipe of the
// current user. The name of the real Worker is fixed by the user's SID (AD-22)
// and pipe names are machine-wide, so only TestListenOnTheUsersRealPipe
// touches it; every other test listens on a name of its own with [listenPipe].
func testPipe(t *testing.T) (name, sddl string) {
	t.Helper()
	var r [4]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal(err)
	}
	name, err := PipeName(fmt.Sprintf("S-1-5-21-%d-%d", binary.BigEndian.Uint32(r[:]), os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	self, err := processUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err = PipeSDDL(self)
	if err != nil {
		t.Fatal(err)
	}
	return name, sddl
}

func newTestServer(t *testing.T, ex Executor) *Server {
	t.Helper()
	srv, err := New(Config{Executor: ex, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// serve starts srv on a new pipe listener and stops it with the test.
func serve(t *testing.T, name, sddl string, srv *Server) net.Listener {
	t.Helper()
	l, err := listenPipe(name, sddl)
	if err != nil {
		t.Fatalf("listenPipe(%s): %v", name, err)
	}
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

func pipeEchoExecutor() Executor {
	return ExecutorFunc(func(_ context.Context, rq Request, progress func(json.RawMessage)) (json.RawMessage, error) {
		progress(json.RawMessage(`{"n":1}`))
		out, _ := json.Marshal(map[string]any{"op": rq.Op, "actions": len(rq.Actions)})
		return out, nil
	})
}

// exchangeOnce sends one op.request on a new connection and returns the type
// of every frame it reads up to the result.
func exchangeOnce(t *testing.T, name string) []string {
	t.Helper()
	conn, err := Dial(name, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial(%s): %v", name, err)
	}
	defer conn.Close()
	if err := proto.WriteFrame(conn, proto.StreamFrameLimit, RequestFrame{Type: proto.TypeOpRequest, Op: "sessions_list"}); err != nil {
		t.Fatal(err)
	}
	var types []string
	for {
		body, err := proto.ReadFrame(conn, proto.StreamFrameLimit)
		if err != nil {
			t.Fatalf("read frame: %v (after %v)", err, types)
		}
		typ, err := proto.FrameType(body)
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
		if typ == proto.TypeOpResult || typ == proto.TypeError {
			break
		}
	}
	// The server closes the connection after the result: one connection, one
	// request.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := proto.ReadFrame(conn, proto.StreamFrameLimit); err == nil {
		t.Fatal("the connection stayed open after the result")
	}
	return types
}

// AC: the Worker listens on \\.\pipe\minion-worker-<user SID> and the local
// side connects to it.
func TestListenOnTheUsersRealPipe(t *testing.T) {
	self, err := processUserSID()
	if err != nil {
		t.Fatal(err)
	}
	name, err := PipeName(self)
	if err != nil {
		t.Fatal(err)
	}
	if want := `\\.\pipe\minion-worker-` + self; name != want {
		t.Fatalf("name = %q, want %q", name, want)
	}
	if conn, err := Dial(name, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Skipf("a Minion Worker of this user already runs on %s", name)
	}
	srv := newTestServer(t, pipeEchoExecutor())
	l, err := Listen(name)
	if errors.Is(err, ErrAlreadyRunning) {
		t.Skipf("another process holds %s", name)
	}
	if err != nil {
		t.Fatalf("Listen(%s): %v", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	got := exchangeOnce(t, name)
	if len(got) != 2 || got[0] != proto.TypeOpProgress || got[1] != proto.TypeOpResult {
		t.Fatalf("frames = %v, want op.progress then op.result", got)
	}
	// One connection is one request; the next connection is served again.
	if again := exchangeOnce(t, name); len(again) != 2 {
		t.Fatalf("second connection: frames = %v", again)
	}
}

func TestPipeServesOpFramesAndClosesAfterTheResult(t *testing.T) {
	name, sddl := testPipe(t)
	serve(t, name, sddl, newTestServer(t, pipeEchoExecutor()))
	for i := 0; i < 3; i++ {
		got := exchangeOnce(t, name)
		if len(got) != 2 || got[0] != proto.TypeOpProgress || got[1] != proto.TypeOpResult {
			t.Fatalf("connection %d: frames = %v, want op.progress then op.result", i, got)
		}
	}
}

// aclOfConn returns the SIDs of the allow ACEs on the DACL of the pipe behind
// conn and whether the DACL is protected.
func aclOfConn(t *testing.T, conn net.Conn) ([]string, bool) {
	t.Helper()
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("the connection has no handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("dacl: %v", err)
	}
	var sids []string
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d has type %d, want a plain allow ACE", i, ace.Header.AceType)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
	}
	return sids, ctrl&windows.SE_DACL_PROTECTED != 0
}

// AC: the pipe has an ACL for that user only (SYSTEM, the service account,
// aside); nothing else is granted and nothing is inherited.
func TestPipeACLIsProtectedAndNamesOnlyTheUserAndSystem(t *testing.T) {
	name, sddl := testPipe(t)
	serve(t, name, sddl, newTestServer(t, pipeEchoExecutor()))

	conn, err := Dial(name, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sids, protected := aclOfConn(t, conn)
	if !protected {
		t.Error("the pipe DACL inherits entries (SE_DACL_PROTECTED is not set)")
	}
	self, _ := processUserSID()
	want := map[string]bool{self: true, SIDSystem: true}
	for _, s := range sids {
		if !want[s] {
			t.Errorf("the pipe DACL grants access to %s", s)
		}
		delete(want, s)
	}
	for s := range want {
		t.Errorf("the pipe DACL does not grant %s", s)
	}
}

// A second Worker, or any process, must not take over the pipe.
func TestSecondListenIsRefusedAsAlreadyRunning(t *testing.T) {
	name, sddl := testPipe(t)
	serve(t, name, sddl, newTestServer(t, pipeEchoExecutor()))
	l, err := listenPipe(name, sddl)
	if err == nil {
		_ = l.Close()
		t.Fatal("a second Listen on a live pipe succeeded")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	// The first Worker keeps answering.
	if got := exchangeOnce(t, name); len(got) != 2 {
		t.Fatalf("frames after the refused Listen = %v", got)
	}
}

func TestListenAgainAfterCloseWorks(t *testing.T) {
	name, sddl := testPipe(t)
	l, err := listenPipe(name, sddl)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = listenPipe(name, sddl)
	if err != nil {
		t.Fatalf("Listen after Close: %v", err)
	}
	_ = l.Close()
}

func TestListenRefusesOtherNames(t *testing.T) {
	for _, bad := range []string{`\\.\pipe\something-else`, `C:\Users\x\worker.sock`, ""} {
		if l, err := Listen(bad); err == nil {
			_ = l.Close()
			t.Errorf("Listen(%q) succeeded", bad)
		}
	}
}

// A user cannot serve the pipe of another user: the pipe would admit that
// user and SYSTEM, not the Worker itself.
func TestListenRefusesThePipeOfAnotherUser(t *testing.T) {
	self, _ := processUserSID()
	if self == SIDSystem {
		t.Skip("SYSTEM may serve any user's pipe")
	}
	other := "S-1-5-21-1-2-3-4"
	name, _ := PipeName(other)
	if l, err := Listen(name); err == nil {
		_ = l.Close()
		t.Fatal("Listen on the pipe of another user succeeded")
	}
}

func TestVerifySocketChecksTheName(t *testing.T) {
	self, _ := processUserSID()
	good, _ := PipeName(self)
	if err := VerifySocket(good); err != nil {
		t.Errorf("VerifySocket(%q): %v", good, err)
	}
	for _, bad := range []string{`\\.\pipe\evil`, `\\.\pipe\minion-worker-..\x`, `C:\x`} {
		if err := VerifySocket(bad); !errors.Is(err, ErrUntrusted) {
			t.Errorf("VerifySocket(%q) = %v, want ErrUntrusted", bad, err)
		}
	}
}

// The client checks who created the pipe on the open connection.
func TestVerifyConnAcceptsThePipeOfTheWorkerOfThisUser(t *testing.T) {
	name, sddl := testPipe(t)
	serve(t, name, sddl, newTestServer(t, pipeEchoExecutor()))
	conn, err := Dial(name, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := VerifyConn(conn); err != nil {
		t.Fatalf("VerifyConn: %v", err)
	}
}

func TestVerifyConnRejectsAConnectionThatIsNotAPipe(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if err := VerifyConn(a); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("VerifyConn(net.Pipe) = %v, want ErrUntrusted", err)
	}
}

func TestDialToAMissingPipeFails(t *testing.T) {
	var r [4]byte
	_, _ = rand.Read(r[:])
	name, err := PipeName(fmt.Sprintf("S-1-5-21-%d", binary.BigEndian.Uint32(r[:])))
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := Dial(name, 300*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("Dial to a pipe that does not exist succeeded")
	}
}
