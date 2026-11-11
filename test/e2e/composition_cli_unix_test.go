//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Story 9.4 composition cases through the real binary: "minion worker up
// --foreground --invite-key <file>" against the real Hub (self-signed
// certificate for another name, no TLS flag), then the same Worker started
// again from its state directory with no key at all.

var (
	minionOnce sync.Once
	minionBin  string
	minionDir  string // the directory of minionBin, removed by TestMain
	minionErr  error
)

// TestMain gives the cached CLI binary the lifetime of the whole package run:
// the directory is built once by the first case that needs it, used by later
// ones, and removed here after every case has finished, including when the
// build failed half way. (t.TempDir of the first case would delete the binary
// under the cases that run later.)
func TestMain(m *testing.M) {
	code := m.Run()
	if minionDir != "" {
		_ = os.RemoveAll(minionDir)
	}
	os.Exit(code)
}

// minionBinary builds cmd/minion once per test run, into a directory outside
// the source tree.
func minionBinary(t *testing.T) string {
	t.Helper()
	minionOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..")
		dir, err := os.MkdirTemp("", "minion-e2e-bin-")
		if err != nil {
			minionErr = err
			return
		}
		minionDir = dir
		minionBin = filepath.Join(dir, "minion")
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", minionBin, "./cmd/minion")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			minionErr = &buildError{err, string(out)}
		}
	})
	if minionErr != nil {
		t.Fatal(minionErr)
	}
	return minionBin
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return "go build ./cmd/minion: " + e.err.Error() + "\n" + e.out }

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// minionWorker is one foreground "minion worker up" process. Its environment
// is built from scratch: its own HOME and XDG_RUNTIME_DIR (a short path, unix
// sockets are limited), no TMUX, no inherited MINION_*.
type minionWorker struct {
	cmd  *exec.Cmd
	out  lockedBuf
	done chan error
}

func startMinionWorker(t *testing.T, hubURL, stateDir, keyFile string) *minionWorker {
	t.Helper()
	bin := minionBinary(t)
	rt, err := os.MkdirTemp("", "mw")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Cleanup(func() { _ = os.RemoveAll(rt) })
	args := []string{"worker", "up", "--foreground", "--server", hubURL, "--share-existing", "none",
		"--state-dir", stateDir, "--tmux-socket", filepath.Join(rt, "tmux.sock"), "--name", "pi"}
	if keyFile != "" {
		args = append(args, "--invite-key", keyFile)
	}
	w := &minionWorker{done: make(chan error, 1)}
	w.cmd = exec.Command(bin, args...)
	w.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_RUNTIME_DIR=" + rt,
		"XDG_STATE_HOME=" + filepath.Join(home, "state"), "XDG_CONFIG_HOME=" + filepath.Join(home, "config")}
	w.cmd.Stdout, w.cmd.Stderr = &w.out, &w.out
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { w.done <- w.cmd.Wait() }()
	t.Cleanup(func() { _ = w.stop(t) })
	return w
}

// stop sends SIGINT and returns the exit error (nil for a clean exit).
func (w *minionWorker) stop(t *testing.T) error {
	t.Helper()
	if w.cmd.ProcessState != nil {
		return nil
	}
	_ = w.cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-w.done:
		w.done <- err
		return err
	case <-time.After(15 * time.Second):
		_ = w.cmd.Process.Kill()
		t.Errorf("the Worker did not stop on SIGINT:\n%s", w.out.String())
		return nil
	}
}

func TestRealCLIEnrollsWithAnInvitationKeyFileThenRestartsFromItsStateWithoutAKey(t *testing.T) {
	h := newInviteHub(t)
	key := h.newKey(t, ops.InvitationReusable)
	keyFile := filepath.Join(t.TempDir(), "invite.key")
	if err := os.WriteFile(keyFile, []byte(key.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()

	w := startMinionWorker(t, h.url(), state, keyFile)
	eventually(t, "the CLI Worker to enroll and be online", func() bool {
		rows, _ := h.st.ListWorkers(context.Background())
		return len(rows) == 1 && h.service().Registry().IsOnline(rows[0].ID)
	})
	rows, _ := h.st.ListWorkers(context.Background())
	if rows[0].Name != "pi" || rows[0].OwnerID != "alice" || rows[0].Kind != "private" {
		t.Fatalf("worker row = %+v", rows[0])
	}
	if !strings.Contains(w.out.String(), "enrolled as pi, owner alice") {
		t.Fatalf("the CLI did not report the enrollment:\n%s", w.out.String())
	}
	if strings.Contains(w.out.String(), key.String()) {
		t.Fatal("the CLI printed the invitation key")
	}
	// The one-line warning of the LITERAL form must not appear for a file. (The
	// generic "--invite-key was given on the command line" notice of the
	// registry does print for any value of the flag; it is not asserted.)
	if strings.Contains(w.out.String(), config.InviteKeyArgvWarning) {
		t.Fatalf("a key FILE is not the literal form, but the CLI printed its warning:\n%s", w.out.String())
	}
	id := rows[0].ID
	// F-02: a newly enrolled Worker is pending, connected but not routable. An
	// approval ends its connection and the real CLI Worker reconnects as an
	// approved Worker.
	if !rows[0].Pending {
		t.Fatalf("the enrolled Worker is not pending: %+v", rows[0])
	}
	if on, ok := h.service().Registry().Get(id); !ok || !on.Pending {
		t.Fatalf("the connected Worker is not registered as pending: %+v", on)
	}
	approver, err := workers.NewApprover(workers.ApproverConfig{Store: h.st, Connections: h.service().Registry()})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := approver.Approve(context.Background(), workers.Actor{ID: "alice"}, id, ""); err != nil || !res.Changed || !res.Disconnected {
		t.Fatalf("approve = %+v, %v", res, err)
	}
	eventually(t, "the approved CLI Worker to reconnect as approved", func() bool {
		on, ok := h.service().Registry().Get(id)
		return ok && !on.Pending
	})
	if err := w.stop(t); err != nil {
		t.Fatalf("the CLI Worker did not exit cleanly: %v\n%s", err, w.out.String())
	}
	eventually(t, "the Hub to see the Worker leave", func() bool { return !h.service().Registry().IsOnline(id) })

	// The process starts again: no key, no code. The state directory holds the
	// enrollment, so the same Worker is online again and no second row exists.
	again := startMinionWorker(t, h.url(), state, "")
	eventually(t, "the restarted CLI Worker to be online again", func() bool { return h.service().Registry().IsOnline(id) })
	if rows, _ := h.st.ListWorkers(context.Background()); len(rows) != 1 {
		t.Fatalf("the restart created %d Workers", len(rows))
	}
	if strings.Contains(strings.ToLower(again.out.String()), "pairing code") {
		t.Fatalf("the restarted CLI Worker asked for a pairing code:\n%s", again.out.String())
	}
	if err := again.stop(t); err != nil {
		t.Fatalf("the restarted CLI Worker did not exit cleanly: %v\n%s", err, again.out.String())
	}
}
