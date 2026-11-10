//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// sessionOptions returns the task and probe options of session name, read
// through the adapter of the tmux server at socket.
func sessionOptions(t *testing.T, ctx context.Context, socket, name string) (task, probe string) {
	t.Helper()
	host, err := tmux.New(socket)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := host.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Name == name {
			return s.Task, s.Probe
		}
	}
	t.Fatalf("no session %q on %s: %+v", name, socket, sessions)
	return "", ""
}

// TestMnSendTextThroughTheLocalSocket is the integration test of Story 2.9: a
// Worker runtime (core and local socket modules) on a private tmux server, and
// the real `mn` entry point driving it through the default backend, which is
// the local control socket because no Hub is configured.
func TestMnSendTextThroughTheLocalSocket(t *testing.T) {
	srv := tmuxtest.New(t)
	target := srv.NewSession("catwork", "cat")

	run, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	// The default socket path of module and client: XDG_RUNTIME_DIR is honoured
	// on Linux only, TMPDIR decides everywhere else, so set both.
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("TMPDIR", run)
	// Not inside a pane: the target must come from --target.
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")

	reg := agent.NewRegistry()
	if err := agent.RegisterCore(reg); err != nil {
		t.Fatal(err)
	}
	if err := agent.RegisterLocalAPI(reg); err != nil {
		t.Fatal(err)
	}
	rt := agent.NewRuntime(reg, agent.Options{StopTimeout: 10 * time.Second})
	settings := agent.Settings{WorkerCore: config.WorkerCore{
		StateDir: t.TempDir(), TmuxSocket: srv.Socket(), Name: "it", ShareExisting: config.ShareExistingAll,
	}}
	if err := rt.Start(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rt.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// mn sessions_list --json
	var out, errb strings.Builder
	code := Run(ctx, []string{"/usr/local/bin/mn", "sessions_list", "--json"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("mn sessions_list = %d, stderr %q", code, errb.String())
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal([]byte(out.String()), &list); err != nil {
		t.Fatalf("sessions_list output %q: %v", out.String(), err)
	}
	found := false
	for _, s := range list.Sessions {
		found = found || s.Name == "catwork"
	}
	if !found {
		t.Fatalf("sessions_list = %s, want catwork", out.String())
	}

	// mn send_text --target catwork --text ...
	out.Reset()
	errb.Reset()
	code = Run(ctx, []string{"mn", "send_text", "--target", "catwork", "--text", "hello-from-mn"}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("mn send_text = %d, stdout %q stderr %q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "outcome: delivered") {
		t.Fatalf("send_text output = %q", out.String())
	}
	srv.WaitFor(target, "hello-from-mn", 10*time.Second)

	// A target that names another Worker never acts on the local one, even
	// when a local session has the same name.
	out.Reset()
	errb.Reset()
	foreign := "01890a5d-ac96-774b-bcce-b302099a8057"
	code = Run(ctx, []string{"mn", "send_text", "--target", foreign + ":catwork", "--text", "leaked-to-local"}, &out, &errb)
	if code != ExitUsage {
		t.Fatalf("foreign Worker target = %d, stdout %q stderr %q; want a usage error", code, out.String(), errb.String())
	}
	time.Sleep(300 * time.Millisecond)
	if scr := srv.Capture(target); strings.Contains(scr, "leaked-to-local") {
		t.Fatalf("the text of a foreign Worker target reached the local pane:\n%s", scr)
	}

	// The default target: inside the pane of catwork (TMUX and TMUX_PANE of
	// the private server), no --target.
	var paneID string
	for _, s := range list.Sessions {
		if s.Name == "catwork" && s.Pane != nil {
			paneID = s.Pane.ID
		}
	}
	if paneID == "" {
		t.Fatalf("sessions_list has no pane id for catwork: %+v", list.Sessions)
	}
	t.Setenv("TMUX", srv.Socket()+",0,0")
	t.Setenv("TMUX_PANE", paneID)
	hostA, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	// The same server is accepted whatever the task holds: unset, text that
	// begins like a probe value, and the largest task SetOption stores. The
	// task is exactly the same afterwards and no probe is left on the session.
	tasks := []string{"", "minion-probe-debugging", strings.Repeat("t", tmux.MaxOptionValue)}
	for i, task := range tasks {
		if err := hostA.SetOption(ctx, "catwork", tmux.OptionTask, task); err != nil {
			t.Fatal(err)
		}
		marker := fmt.Sprintf("hello-default-%d", i)
		out.Reset()
		errb.Reset()
		code = Run(ctx, []string{"mn", "send_text", "--text", marker}, &out, &errb)
		if code != ExitOK {
			t.Fatalf("task %d: mn send_text without --target = %d, stdout %q stderr %q", i, code, out.String(), errb.String())
		}
		srv.WaitFor(target, marker, 10*time.Second)
		if gotTask, gotProbe := sessionOptions(t, ctx, srv.Socket(), "catwork"); gotTask != task || gotProbe != "" {
			t.Errorf("task %d: after an implicit-target command the task is %.40q (want %.40q), probe %q", i, gotTask, task, gotProbe)
		}
	}
}

// TestImplicitTargetRefusedOnAnotherTmuxServer is the two-server collision of
// Story 2.9: the shell runs in a session named catwork on one tmux server, the
// Worker serves another tmux server that also has a session named catwork. The
// implicit target must not reach the Worker's catwork.
func TestImplicitTargetRefusedOnAnotherTmuxServer(t *testing.T) {
	workerSrv := tmuxtest.New(t)
	workerPane := workerSrv.NewSession("catwork", "cat")
	shellSrv := tmuxtest.New(t)
	shellPane := shellSrv.NewSession("catwork", "cat")

	run, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("TMPDIR", run)

	reg := agent.NewRegistry()
	if err := agent.RegisterCore(reg); err != nil {
		t.Fatal(err)
	}
	if err := agent.RegisterLocalAPI(reg); err != nil {
		t.Fatal(err)
	}
	rt := agent.NewRuntime(reg, agent.Options{StopTimeout: 10 * time.Second})
	settings := agent.Settings{WorkerCore: config.WorkerCore{
		StateDir: t.TempDir(), TmuxSocket: workerSrv.Socket(), Name: "it", ShareExisting: config.ShareExistingAll,
	}}
	if err := rt.Start(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rt.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The pane id of the shell's catwork, read through the shell's server.
	host, err := tmux.New(shellSrv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := host.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paneID string
	for _, s := range sessions {
		if s.Name == "catwork" {
			if p, ok := s.ActivePane(); ok {
				paneID = p.Ref.ID
			}
		}
	}
	if paneID == "" {
		t.Fatalf("no pane id for the shell's catwork: %+v", sessions)
	}
	t.Setenv("TMUX", shellSrv.Socket()+",0,0")
	t.Setenv("TMUX_PANE", paneID)
	// Both catwork sessions carry a task that begins like a probe value.
	const userTask = "minion-probe-debugging"
	for _, sock := range []string{shellSrv.Socket(), workerSrv.Socket()} {
		h, err := tmux.New(sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.SetOption(ctx, "catwork", tmux.OptionTask, userTask); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb strings.Builder
	code := Run(ctx, []string{"mn", "send_text", "--text", "wrong-server"}, &out, &errb)
	if code != ExitUsage {
		t.Fatalf("implicit target on another tmux server = %d, stdout %q stderr %q; want a usage error", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "not the one the Worker serves") {
		t.Errorf("stderr = %q", errb.String())
	}
	time.Sleep(300 * time.Millisecond)
	for name, scr := range map[string]string{"worker pane": workerSrv.Capture(workerPane), "shell pane": shellSrv.Capture(shellPane)} {
		if strings.Contains(scr, "wrong-server") {
			t.Errorf("text reached the %s:\n%s", name, scr)
		}
	}
	// The refusal names the way out, and the probe left nothing behind and
	// changed no task on either server.
	if !strings.Contains(errb.String(), "--target") {
		t.Errorf("stderr = %q, want guidance to pass --target", errb.String())
	}
	for name, sock := range map[string]string{"shell": shellSrv.Socket(), "worker": workerSrv.Socket()} {
		if task, probe := sessionOptions(t, ctx, sock, "catwork"); task != userTask || probe != "" {
			t.Errorf("%s server: task %q (want %q), probe %q", name, task, userTask, probe)
		}
	}
}
