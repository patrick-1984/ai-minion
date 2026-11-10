//go:build unix

package tmuxtest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestWaitForHonorsDeadline suspends the server, so every tmux call hangs: the
// call must still return near the caller's timeout, and never report a match
// that arrives after it.
func TestWaitForHonorsDeadline(t *testing.T) {
	srv := New(t)
	target := srv.NewSession("stuck", "sh", "-c", "echo present; sleep 60")
	srv.WaitFor(target, "present", 5*time.Second)
	out, err := srv.run("display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("pid %q: %v", out, err)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}
	defer resume()
	// A stopped process still dies on SIGKILL, so the cleanup of New works too,
	// but resume it first anyway; t.Cleanup runs before New's cleanup (LIFO).
	t.Cleanup(resume)

	start := time.Now()
	_, err = srv.waitFor(target, "present", 150*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a capture that cannot finish before the deadline must not succeed")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("waitFor took %s for a 150ms timeout", elapsed)
	}
}

// TestWaitAllExecWaitsForTheProgram starts a pane whose process is blocked on a
// FIFO before it execs sleep (a delayed start that holds for as long as the
// test wants). The wait for sleep must keep waiting while the pane process is
// still another image, a wait for any exec must already be satisfied, and the
// wait for sleep must finish once the FIFO is released and sleep has exec'd.
func TestWaitAllExecWaitsForTheProgram(t *testing.T) {
	srv := New(t)
	fifo := filepath.Join(t.TempDir(), "go")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.NewSession("delayed", "sh", "-c", `read x < "$1"; exec sleep 60`, "sh", fifo)
	if err := srv.waitAllExec("sleep", 300*time.Millisecond); err == nil {
		t.Fatal("waited for sleep returned while the pane process is still sh")
	}
	if err := srv.waitAllExec("", 5*time.Second); err != nil {
		t.Fatalf("wait for any exec: %v", err)
	}
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.waitAllExec("sleep", 5*time.Second); err != nil {
		t.Fatalf("wait for sleep after the release: %v", err)
	}
}

// fakePS writes an executable ps stand-in that runs body and returns its path.
func fakePS(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ps")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOSInspectorDistinguishesErrorsFromAbsence covers the lookup of a process
// image name. Only a confirmed absence may count as "gone" (which the exec
// wait treats as settled); a missing or failing ps, or a /proc that is not
// there, is an error. The ps fallback is the path of macOS and BSD, which the
// Linux runs of the other tests never take.
func TestOSInspectorDistinguishesErrorsFromAbsence(t *testing.T) {
	procWithSelf := func(pid int, comm string) string {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
			t.Fatal(err)
		}
		if comm != "" {
			if err := os.Mkdir(filepath.Join(root, strconv.Itoa(pid)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "comm"), []byte(comm+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	noProc := filepath.Join(t.TempDir(), "no-proc")
	missingPS := filepath.Join(t.TempDir(), "no-such-ps")
	cases := []struct {
		name    string
		in      osInspector
		want    string
		state   procState
		wantErr bool
	}{
		{"proc present", osInspector{procRoot: procWithSelf(42, "sleep"), ps: missingPS}, "sleep", procPresent, false},
		{"proc absent, /proc mounted", osInspector{procRoot: procWithSelf(42, ""), ps: missingPS}, "", procGone, false},
		{"no /proc, ps missing", osInspector{procRoot: noProc, ps: missingPS}, "", 0, true},
		{"no /proc, ps not executable", osInspector{procRoot: noProc, ps: t.TempDir()}, "", 0, true},
		{"no /proc, ps fails with 2", osInspector{procRoot: noProc, ps: fakePS(t, "echo boom >&2; exit 2")}, "", 0, true},
		{"no /proc, ps exit 0 without output", osInspector{procRoot: noProc, ps: fakePS(t, "exit 0")}, "", 0, true},
		{"no /proc, ps exit 1 with output", osInspector{procRoot: noProc, ps: fakePS(t, "echo sleep; exit 1")}, "", 0, true},
		{"no /proc, ps names the process", osInspector{procRoot: noProc, ps: fakePS(t, "echo /bin/sleep")}, "sleep", procPresent, false},
		{"no /proc, ps exit 1: no such pid", osInspector{procRoot: noProc, ps: fakePS(t, "exit 1")}, "", procGone, false},
	}
	for _, c := range cases {
		got, state, err := c.in.comm(context.Background(), 42)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
			continue
		}
		if !c.wantErr && (got != c.want || state != c.state) {
			t.Errorf("%s: = %q, %v; want %q, %v", c.name, got, state, c.want, c.state)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (osInspector{procRoot: noProc, ps: fakePS(t, "echo sleep")}).comm(ctx, 42); err == nil {
		t.Error("a cancelled context must be an error")
	}
}

// stubProc is a process view for the exec wait: it passes every lookup to the
// real inspector, except that the first preExec lookups of the pane process
// report the image of the tmux server (a child that has forked and not exec'd
// yet), and every lookup can be delayed or made to fail.
type stubProc struct {
	real      osInspector
	serverPID int
	preExec   int
	delay     time.Duration // ignores ctx on purpose
	err       error

	mu        sync.Mutex
	paneCalls int
}

func (p *stubProc) comm(ctx context.Context, pid int) (string, procState, error) {
	if p.delay > 0 {
		time.Sleep(p.delay)
		// The late answer is a real one: it would settle the panes.
		ctx = context.Background()
	}
	if p.err != nil {
		return "", 0, p.err
	}
	if pid == p.serverPID {
		return p.real.comm(ctx, pid)
	}
	p.mu.Lock()
	p.paneCalls++
	pre := p.paneCalls <= p.preExec
	p.mu.Unlock()
	if pre {
		return p.real.comm(ctx, p.serverPID)
	}
	return p.real.comm(ctx, pid)
}

func (p *stubProc) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paneCalls
}

// settledServer starts a private server with a pane that runs sleep, created
// directly through tmux, and waits until the real process view says that it has
// exec'd. From then on tmux itself reports sleep for the pane, which is the
// state of the "sleep" fallback of a pane that has not started.
func settledServer(t *testing.T) (*Server, *stubProc) {
	t.Helper()
	srv := New(t)
	if out, err := srv.run("new-session", "-d", "-s", "direct", "--", "sleep", "60"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	if err := srv.waitAllExec("sleep", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	out, err := srv.run("display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("pid %q: %v", out, err)
	}
	stub := &stubProc{real: defaultInspector(), serverPID: pid}
	srv.proc = stub
	return srv, stub
}

// TestWaitAllExecDelayedChildStart is the deterministic form of the delayed
// start seen on tmux 3.6: tmux already reports the configured command (sleep)
// for the pane, but the pane process still has the image of the tmux server for
// its first lookups. The wait must not return before the process lookup shows
// the exec, and must return once it does.
func TestWaitAllExecDelayedChildStart(t *testing.T) {
	srv, stub := settledServer(t)
	stub.preExec = 6
	// Any exec (no program): only the process image can tell the forked child
	// from the program here, since tmux already reports sleep.
	if err := srv.waitAllExec("", 5*time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if n := stub.calls(); n <= stub.preExec {
		t.Fatalf("wait returned after %d pane lookups, while the first %d still showed the forked child", n, stub.preExec)
	}
	// A child that never execs makes the wait fail at the deadline.
	stub.mu.Lock()
	stub.paneCalls, stub.preExec = 0, 1<<30
	stub.mu.Unlock()
	if err := srv.waitAllExec("sleep", 200*time.Millisecond); err == nil {
		t.Fatal("wait succeeded for a pane process that never left the server image")
	}
}

// TestWaitAllExecRejectsSuccessAfterDeadline: an answer that would settle the
// panes but arrives after the deadline does not count (the contract of WaitFor),
// whether it is late in the process lookup (which ignores the context here) or
// late because tmux itself is suspended.
func TestWaitAllExecRejectsSuccessAfterDeadline(t *testing.T) {
	srv, stub := settledServer(t)
	stub.delay = 300 * time.Millisecond
	start := time.Now()
	err := srv.waitAllExec("sleep", 100*time.Millisecond)
	if err == nil {
		t.Fatalf("a settled answer that arrived after %s must not succeed", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait took %s for a 100ms timeout", elapsed)
	}
}

func TestWaitAllExecHonorsDeadlineOfSuspendedServer(t *testing.T) {
	srv, stub := settledServer(t)
	if err := syscall.Kill(stub.serverPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resume := func() { _ = syscall.Kill(stub.serverPID, syscall.SIGCONT) }
	defer resume()
	t.Cleanup(resume)
	start := time.Now()
	if err := srv.waitAllExec("sleep", 150*time.Millisecond); err == nil {
		t.Fatal("a suspended server cannot answer: the wait must fail")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("wait took %s for a 150ms timeout", elapsed)
	}
}

// TestWaitAllExecInspectionErrorIsNotSettled: a process that cannot be
// inspected is never a settled pane, for an injected error and for the real
// ps fallback with no /proc and no usable ps; the wait reports the error.
func TestWaitAllExecInspectionErrorIsNotSettled(t *testing.T) {
	srv, stub := settledServer(t)
	boom := errors.New("inspection failed")
	stub.err = boom
	err := srv.waitAllExec("", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "inspection failed") {
		t.Fatalf("injected error: wait err = %v, want the inspection error", err)
	}
	srv.proc = osInspector{procRoot: filepath.Join(t.TempDir(), "no-proc"), ps: filepath.Join(t.TempDir(), "no-such-ps")}
	if err := srv.waitAllExec("sleep", 300*time.Millisecond); err == nil {
		t.Fatal("with no /proc and no ps the panes cannot be inspected: the wait must fail, not succeed")
	} else if !strings.Contains(err.Error(), "no-such-ps") {
		t.Fatalf("wait err = %v, want the ps launch error", err)
	}
	// A cached server image must not hide the error for the panes themselves.
	srv.serverImage = ""
	if err := srv.waitAllExec("", 300*time.Millisecond); err == nil {
		t.Fatal("wait succeeded without any process inspection")
	}
}

// TestTmuxRunsOnThePrivateSocket checks that Tmux reaches the harness's own
// server: it reads the server's socket path back.
func TestTmuxRunsOnThePrivateSocket(t *testing.T) {
	srv := New(t)
	out, err := srv.Tmux("display-message", "-p", "#{socket_path}")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != srv.Socket() {
		t.Fatalf("socket_path = %q, want %q", got, srv.Socket())
	}
}
