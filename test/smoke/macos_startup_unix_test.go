//go:build unix

package smoke

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
)

// The tests in this file signal and inspect real processes (kill(2), SIGSTOP,
// SIGCONT, ps): the supported release systems for them are Linux and macOS.
// Everything else about the macOS smoke script (the failure reports, the
// redaction, the timeout limit) is tested in macos_startup_test.go on every
// system; the Windows preview has its own PowerShell smoke test (windows.ps1).

// procState returns the state letter of ps(1) for pid ("S", "R", "T", "Z", ...)
// and false when there is no such process. ps -o stat= works on Linux and macOS
// (/proc exists only on Linux).
func procState(pid int) (byte, bool) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false // ps exits 1 when no process matches
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, false
	}
	return s[0], true
}

// processGone reports whether pid is gone or only a zombie (a zombie that init
// has not reaped yet runs nothing).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return true
	}
	st, ok := procState(pid)
	return !ok || st == 'Z'
}

// readPids reads the pids that the fake processes wrote to path.
func readPids(t *testing.T, path string) []int {
	t.Helper()
	var pids []int
	for _, f := range strings.Fields(readLog(t, path)) {
		p, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("bad pid %q in %s", f, path)
		}
		pids = append(pids, p)
	}
	return pids
}

// killLeftovers ends the processes of a failed test (never of a passing one).
func killLeftovers(t *testing.T, pids []int) {
	t.Helper()
	for _, p := range pids {
		if !processGone(p) {
			t.Errorf("process %d is still running", p)
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	}
}

// waitGone gives init a moment to reap what the script ended.
func waitGone(pids []int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, p := range pids {
			all = all && processGone(p)
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The limit of smoke_timeout ends the whole process tree of the diagnostic,
// not only its first process: "worker status" starts a tmux client with a
// timer of its own that dies with its parent, and a client of a stopped
// server then waits for good. The grandchildren are gone when it returns.
func TestSmokeTimeoutEndsTheWholeTree(t *testing.T) {
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	r := sh(t, []string{"PIDS=" + pids}, `smoke_timeout 1 bash -c '
		bash -c "sleep 40 & echo \$! >>\"\$1\"; wait" _ "$PIDS" &
		sleep 40 &
		echo $! >>"$PIDS"
		echo $$ >>"$PIDS"
		wait' >/dev/null || echo "rc=$?"`)
	list := readPids(t, pids)
	waitGone(list)
	killLeftovers(t, list)
	if len(list) != 3 {
		t.Errorf("expected 3 pids (a child, a grandchild and the shell), got %v: %s", list, r.all())
	}
	if !strings.Contains(r.stdout, "rc=124") {
		t.Errorf("a diagnostic that outlived its limit must give 124: %s", r.all())
	}

	// A descendant that outlives a command that ended on time is ended too: a
	// diagnostic leaves nothing behind.
	pids = filepath.Join(dir, "pids2")
	r = sh(t, []string{"PIDS=" + pids}, `smoke_timeout 5 bash -c 'sleep 40 & echo $! >>"$PIDS"; echo done' && echo "rc=0"`)
	list = readPids(t, pids)
	waitGone(list)
	killLeftovers(t, list)
	if !strings.Contains(r.stdout, "done") || !strings.Contains(r.stdout, "rc=0") {
		t.Errorf("the output and status of a command that ended on time: %s", r.all())
	}
}

// A real tmux server of the caller that is stopped (SIGSTOP): the failure
// report gives up on its clients within the limit, no client or other
// descendant of a diagnostic is left, the server is not signalled (still
// stopped, then still alive and working after SIGCONT) and the failure status
// of the script is the one of the failure.
func TestMacosSmokeLeavesACallersStoppedTmuxServerAlone(t *testing.T) {
	needTools(t, "pgrep")
	srv := tmuxtest.New(t)
	srv.NewSession("smoke")
	out := run(t, "", nil, "", "pgrep", "-f", srv.Socket())
	fields := strings.Fields(out.stdout)
	if len(fields) != 1 {
		t.Fatalf("expected exactly the server process on %s, got %q", srv.Socket(), out.stdout)
	}
	server, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(server, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(server, syscall.SIGCONT) }) // runs before the harness ends the server

	tmp := shortTemp(t)
	pidFile := filepath.Join(tmp, "pids")
	start := time.Now()
	r := runFakeMacosOn(t, tmp, srv.Socket(), nil,
		"FAKE_RUN=die-slow", "FAKE_STATUS=spawn", "FAKE_PIDS="+pidFile, "SMOKE_DIAG_TIMEOUT_SECONDS=1", "SMOKE_WORKER_WAIT_SECONDS=10")
	if el := time.Since(start); el > 30*time.Second {
		t.Errorf("the script took %v with a stopped server", el)
	}
	if r.code != 1 {
		t.Errorf("exit %d, want the status 1 of the failure: %s", r.code, r.all())
	}
	mustHave(t, r, "FAIL: the Worker exited with status 3", "(stopped after 1s)", "last lines of the Worker log (redacted):")
	mustNotLeak(t, r)

	list := readPids(t, pidFile)
	if len(list) != 2 {
		t.Errorf("the fake status should have started 2 descendants: %v", list)
	}
	waitGone(list)
	killLeftovers(t, list)
	// No tmux client of the diagnostics is left on the socket (only the server).
	time.Sleep(200 * time.Millisecond)
	out = run(t, "", nil, "", "pgrep", "-f", srv.Socket())
	if got := strings.Fields(out.stdout); len(got) != 1 || got[0] != strconv.Itoa(server) {
		t.Errorf("a diagnostic client is left on the socket (or the server is gone): %q", out.stdout)
	}
	// The server was neither signalled nor ended: still stopped, and it works
	// after SIGCONT.
	if st, ok := procState(server); !ok || st != 'T' {
		t.Errorf("the caller's server is not stopped any more: state %q (found %v)", st, ok)
	}
	if err := syscall.Kill(server, syscall.SIGCONT); err != nil {
		t.Fatalf("the caller's server is gone: %v", err)
	}
	srv.NewSession("after")
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("the work directory was not removed: %v", l)
	}
}
