//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// serveWorkerSocket serves ex on a real private control socket, the way the
// Worker's local socket module does (localapi.Listen on a short path under a
// 0700 directory, an executor that maps localapi.Request to agent.Request),
// and returns the socket path.
func serveWorkerSocket(t *testing.T, ex *agent.Executor) string {
	t.Helper()
	// A short directory keeps the path within the unix socket limit.
	base, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	path := filepath.Join(base, "minion", localapi.SocketName)
	l, err := localapi.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := localapi.New(localapi.Config{Executor: localapi.ExecutorFunc(
		func(ctx context.Context, rq localapi.Request, _ func(json.RawMessage)) (json.RawMessage, error) {
			return ex.Execute(ctx, agent.Request{Op: rq.Op, Actions: rq.Actions, Args: rq.Args})
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
	return path
}

// listNames returns the session names that sessions_list returns over the
// local socket at sock.
func listNames(t *testing.T, sock string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := client.Local(client.WithSocketPath(sock)).Call(ctx, ops.OpSessionsList, ops.SessionsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range list.Sessions {
		names[s.Name] = true
	}
	return names
}

// TestConfigShareChangesWhatARunningWorkerListsAndStatusCountsTheSessions
// changes the share mode with the CLI while an executor (the Worker's op
// path) already runs on the same state directory, and observes sessions_list
// through the Worker's local control socket.
func TestConfigShareChangesWhatARunningWorkerListsAndStatusCountsTheSessions(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("build", "cat")
	srv.NewSession("docs", "cat")
	host, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ex, err := agent.NewExecutor(agent.Config{
		Host: host, Classifier: agentdetect.New(), State: st, Policy: agent.NewPolicy(st), LockWait: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	sock := serveWorkerSocket(t, ex)
	cd := testConfigDeps(nil)
	if got := listNames(t, sock); len(got) != 0 {
		t.Fatalf("nothing is shared before a choice: %v", got)
	}
	steps := []struct {
		args []string
		want []string
	}{
		{[]string{"share", "all"}, []string{"build", "docs"}},
		{[]string{"share", "none"}, nil},
		{[]string{"share", "select", "docs"}, []string{"docs"}},
		{[]string{"share", "later"}, nil},
	}
	for _, s := range steps {
		if _, _, err := configCmd(t, cd, dir, s.args...); err != nil {
			t.Fatalf("%v: %v", s.args, err)
		}
		got := listNames(t, sock)
		if len(got) != len(s.want) {
			t.Fatalf("after %v sessions_list = %v, want %v", s.args, got, s.want)
		}
		for _, n := range s.want {
			if !got[n] {
				t.Fatalf("after %v sessions_list = %v, want %v", s.args, got, s.want)
			}
		}
	}

	// Status counts the real sessions on the private socket and shows the mode.
	sd := testStatusDeps(nil)
	sd.sessions = func(socket string) agent.StatusSessions { return tmuxSessions(socket) }
	out, _, err := runWorkerCmds(t, sd, cd, "worker", "status", "--json", "--state-dir", dir, "--tmux-socket", srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var r agent.StatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Sessions == nil || *r.Sessions != 2 || r.Share.Mode != "later" || r.TmuxSocket != srv.Socket() {
		t.Fatalf("status = %+v", r)
	}
}
