//go:build unix

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// TestWorkerUpForegroundWithTmux runs the foreground mode on the real core
// module and a private tmux socket, in this process, and ends it by SIGTERM.
func TestWorkerUpForegroundWithTmux(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("adopted", "cat")
	state := t.TempDir()
	runDeps, p := coreDeps(t, map[string]string{"MINION_STATE_DIR": state})
	deps := workerUpDeps{
		run:       runDeps,
		detectEnv: service.Env{GOOS: "linux"}, // no service manager
		targetEnv: service.SystemTargetEnv(),
		service:   service.Deps{Runner: failRunner{t}},
	}
	_, wait, out := startUp(t, deps, p, "--tmux-socket", srv.Socket(), "--share-existing", "all", "--name", "ctr")
	if !strings.Contains(out.String(), "No service manager") {
		t.Errorf("output: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(state, store.DBFile)); err != nil {
		t.Fatalf("the Worker did not open its state: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatalf("worker up = %v after SIGTERM", err)
	}
}

// TestWorkerUpForegroundRealProcess builds the binary and runs
// "minion worker up --foreground" as a real process against a private tmux
// socket, configured like a container (environment only), then stops it with
// SIGTERM.
func TestWorkerUpForegroundRealProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	srv := tmuxtest.New(t)
	srv.NewSession("work", "cat")

	bin := filepath.Join(t.TempDir(), "minion")
	build := exec.Command("go", "build", "-o", bin, "github.com/patrick-1984/ai-minion/cmd/minion")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	state := t.TempDir()
	cmd := exec.Command(bin, "worker", "up", "--foreground")
	cmd.Env = withoutTmuxEnv(os.Environ(),
		"MINION_STATE_DIR="+state,
		"MINION_TMUX_SOCKET="+srv.Socket(),
		"MINION_SHARE_EXISTING=all",
		"MINION_NAME=real-process",
		"MINION_LOG_FORMAT=json",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	db := filepath.Join(state, store.DBFile)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(db); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the process ended early: %v\n%s", err, stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Worker did not open its state:\n%s", stderr.String())
		}
	}
	// The Worker reads its tmux sessions from the private socket only.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the process did not stop after SIGTERM:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(state, "config.json")); err == nil {
		t.Error("foreground mode wrote a config file")
	}
}

// withoutTmuxEnv returns env without the variables that would point a child
// at a tmux server (TMUX, TMUX_PANE), plus extra.
func withoutTmuxEnv(env []string, extra ...string) []string {
	out := make([]string, 0, len(env)+len(extra))
	for _, e := range env {
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") || strings.HasPrefix(e, "MINION_") {
			continue
		}
		out = append(out, e)
	}
	return append(out, extra...)
}
