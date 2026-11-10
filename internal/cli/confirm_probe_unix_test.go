//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func probeTestEnv(k string) string {
	return map[string]string{"TMUX": "/tmp/shell.sock,1,0", "TMUX_PANE": "%1"}[k]
}

// slowWorker is a Worker on server whose sessions_list takes a while and
// calls hook (when set) after it read the session list.
func slowWorker(server *fakeServer, delay time.Duration, hook func()) *fakeOpsBackend {
	inner := workerOn(server, "work")
	return &fakeOpsBackend{call: func(op string, args json.RawMessage) (json.RawMessage, error) {
		raw, err := inner.call(op, args)
		time.Sleep(delay)
		if hook != nil {
			hook()
		}
		return raw, err
	}}
}

func probeServer(task string) *fakeServer {
	s := paneSession("work", "%1")
	s.Task = task
	return &fakeServer{sessions: []term.Session{s}}
}

func tempLocker(t *testing.T) probeLocker {
	dir := filepath.Join(t.TempDir(), "run")
	return func(ctx context.Context, key string) (func(), error) {
		return lockFile(ctx, filepath.Join(dir, "probe.lock"))
	}
}

// TestConfirmProbesOfOneSessionDoNotInterfere runs overlapping implicit-target
// probes in one session, as parallel mn commands do. Every call on the right
// server must be accepted and the task text must be the user's afterwards.
func TestConfirmProbesOfOneSessionDoNotInterfere(t *testing.T) {
	openOn := func(s *fakeServer) func(string) (sessionLister, error) {
		return func(string) (sessionLister, error) { return s, nil }
	}
	var seq atomic.Uint64
	nonce := func() (string, error) { return fmt.Sprintf("minion-probe-%d", seq.Add(1)), nil }
	for round := 0; round < 30; round++ {
		shell := probeServer("my task")
		lock := tempLocker(t)
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		for i := 0; i < 3; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(i*(3+round%5)) * time.Millisecond)
				ctx := context.Background()
				errs <- confirmTmuxServer(probeTestEnv, openOn(shell), nonce, lock)(ctx, "work", slowWorker(shell, 8*time.Millisecond, nil))
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: a same-server probe was refused: %v", round, err)
			}
		}
		if got := shell.task("work"); got != "my task" {
			t.Fatalf("round %d: task = %q, want the user's text", round, got)
		}
	}
}

// TestLockFileSerialisesAndHonoursContext.
func TestLockFileSerialisesAndHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "x.lock")
	release, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); err == nil {
		t.Fatal("a held lock was taken twice")
	}
	release()
	rel2, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	rel2()
}
