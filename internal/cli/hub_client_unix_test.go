//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// startCountingWorker serves the local control socket under dir with a Worker
// that answers sessions_list with "local-session" and counts every request.
func startCountingWorker(t *testing.T, dir string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	l, err := localapi.Listen(filepath.Join(dir, "minion", localapi.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: localapi.ExecutorFunc(func(_ context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"sessions":[{"name":"local-session","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`), nil
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
	return &calls
}

// The real entry points (Run, as `minion` and as `mn`) with a live local Worker
// on this machine: a configured Hub wins and is the only thing called, and
// without it the same Worker serves the command.
func TestEntrypointsHubWinsOverALiveLocalSocketAndLocalServesWithoutOne(t *testing.T) {
	run, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("TMPDIR", run)
	t.Setenv("MINION_STATE_DIR", t.TempDir())
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	for _, k := range []string{"MINION_HUB_URL", "MINION_API_KEY_FILE", "MINION_API_KEY", "MINION_HUB_CA", "MINION_HUB_INSECURE", "MINION_HUB_WORKER"} {
		t.Setenv(k, "")
	}
	local := startCountingWorker(t, run)
	hub := newHubStub(t, func(w http.ResponseWriter, r *http.Request, s *hubStub) {
		stubJSON(w, 200, `{"sessions":[{"name":"hub-session","created":"2026-11-09T10:00:00.000Z","windows":1,"attached":false}]}`)
	})

	runArgv := func(argv ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Run(context.Background(), argv, &out, &errb)
		return code, out.String(), errb.String()
	}

	// No Hub: the live local Worker serves the command.
	code, out, errs := runArgv("mn", "sessions_list", "--json")
	if code != 0 || !strings.Contains(out, "local-session") || local.Load() != 1 {
		t.Fatalf("no Hub: exit %d, stdout %q, stderr %q, local calls %d", code, out, errs, local.Load())
	}

	// A Hub in the environment: the Hub serves it, under both names of the binary.
	t.Setenv("MINION_HUB_URL", hub.srv.URL)
	t.Setenv("MINION_API_KEY_FILE", hub.keyFile)
	t.Setenv("MINION_HUB_CA", hub.caFile)
	for _, name := range []string{"mn", "minion"} {
		code, out, errs = runArgv(name, "sessions_list", "--hub-worker", hubWorkerID, "--json")
		if code != 0 || !strings.Contains(out, "hub-session") || strings.Contains(out, "local-session") {
			t.Fatalf("%s with a Hub: exit %d, stdout %q, stderr %q", name, code, out, errs)
		}
	}
	if local.Load() != 1 {
		t.Errorf("the local Worker was called (%d) although a Hub is configured", local.Load())
	}

	// A Hub that fails: no fallback to the live local Worker.
	t.Setenv("MINION_API_KEY_FILE", filepath.Join(run, "missing.key"))
	code, out, errs = runArgv("mn", "sessions_list", "--hub-worker", hubWorkerID)
	if code != 1 || out != "" || !strings.Contains(errs, "configure the Hub backend") || local.Load() != 1 {
		t.Errorf("failing Hub: exit %d, stdout %q, stderr %q, local calls %d", code, out, errs, local.Load())
	}
}
