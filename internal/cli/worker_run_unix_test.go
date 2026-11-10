//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// coreDeps registers the real core module (store, tmux host, classifier,
// executor) and a probe on a private registry.
func coreDeps(t *testing.T, env map[string]string) (workerRunDeps, *probe) {
	t.Helper()
	reg := agent.NewRegistry()
	if err := agent.RegisterCore(reg); err != nil {
		t.Fatal(err)
	}
	p := newProbe()
	p.register(t, reg)
	if _, ok := env["MINION_STATE_DIR"]; !ok {
		env["MINION_STATE_DIR"] = t.TempDir()
	}
	return workerRunDeps{stderr: &lockedBuf{}, lookupEnv: envFrom(env), loadConfig: loadWorkerConfig, registry: reg}, p
}

// TestWorkerRunWithTmuxStartsAndStopsCleanly is the integration test of the
// story: a Worker on a private tmux socket, no Hub, ended by SIGTERM.
func TestWorkerRunWithTmuxStartsAndStopsCleanly(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("adopted", "cat")
	state := t.TempDir()
	deps, p := coreDeps(t, map[string]string{"MINION_STATE_DIR": state})
	var gotSocket string
	var executor *agent.Executor
	p.services = func(env *agent.Env) error {
		gotSocket = env.Settings().TmuxSocket
		var err error
		executor, err = agent.Require[*agent.Executor](env)
		return err
	}

	_, wait := startWorker(t, deps, p, "--tmux-socket", srv.Socket(), "--share-existing", "all", "--name", "solo")
	if gotSocket != srv.Socket() || !filepath.IsAbs(gotSocket) {
		t.Fatalf("module saw socket %q, want the absolute %q", gotSocket, srv.Socket())
	}
	if _, err := os.Stat(filepath.Join(state, store.DBFile)); err != nil {
		t.Fatalf("the Worker did not open its state: %v", err)
	}

	// The executor talks to the private server and applies --share-existing.
	raw, err := executor.Execute(context.Background(), agent.Request{Op: ops.OpSessionsList, Actions: []ops.Action{ops.ActionRead}})
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Sessions []struct {
			Name string `json:"name"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].Name != "adopted" {
		t.Fatalf("sessions_list = %s (%v)", raw, err)
	}

	// SIGTERM ends the run; the modules stop in reverse order.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatalf("worker run = %v after SIGTERM", err)
	}
	if got := strings.Join(p.log(), ","); got != "probe start,probe stop" {
		t.Fatalf("events = %s", got)
	}

	// The state was closed and holds the choice: reopen it.
	st, err := store.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if sel, err := st.LoadShare(); err != nil || sel.Mode != store.ShareAll {
		t.Fatalf("share = %+v, %v", sel, err)
	}
}

func TestWorkerRunWithTmuxPreviewOnlyRefusesWrites(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("work", "cat")
	deps, p := coreDeps(t, map[string]string{
		"MINION_TMUX_SOCKET": srv.Socket(), "MINION_SHARE_EXISTING": "all", "MINION_PREVIEW_ONLY": "true",
	})
	var executor *agent.Executor
	var host term.SessionHost
	p.services = func(env *agent.Env) (err error) {
		if executor, err = agent.Require[*agent.Executor](env); err != nil {
			return err
		}
		host, err = agent.Require[term.SessionHost](env)
		return err
	}
	cancel, wait := startWorker(t, deps, p)
	args, _ := json.Marshal(ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "work"}, Text: "x"})
	_, err := executor.Execute(context.Background(), agent.Request{Op: ops.OpSendText, Actions: []ops.Action{ops.ActionOperate}, Args: args})
	if ae, ok := agent.AsAPIError(err); !ok || ae.Code != "forbidden" || ae.Reason != "preview_only" {
		t.Fatalf("err = %v, want forbidden/preview_only", err)
	}
	if ss, err := host.ListSessions(context.Background()); err != nil || len(ss) != 1 {
		t.Fatalf("sessions = %v, %v", ss, err)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRunWithoutTmuxFailsWithAClearMessage(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	deps, _ := coreDeps(t, map[string]string{"MINION_TMUX_SOCKET": filepath.Join(t.TempDir(), "s.sock")})
	root := workerRoot(deps)
	root.SetArgs([]string{"worker", "run"})
	root.SetErr(&lockedBuf{})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "terminal host") {
		t.Fatalf("err = %v", err)
	}
}
