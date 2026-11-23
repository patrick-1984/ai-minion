//go:build tmux && (linux || darwin)

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

type paneBindingFixture struct {
	status agent.LiveStatusReply
}

func (p paneBindingFixture) Call(_ context.Context, op string, _ any) (json.RawMessage, error) {
	if op != agent.OpLocalStatus {
		return nil, fmt.Errorf("unexpected local op %q", op)
	}
	return json.Marshal(p.status)
}

func TestHookPaneLive(t *testing.T) {
	temp := t.TempDir()
	harness := filepath.Join(temp, "claude")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, harness); err != nil {
		t.Fatal(err)
	}
	srv := tmuxtest.New(t)
	other := srv.NewSession("unrelated", "sleep", "30")
	otherPane, err := srv.Tmux("display-message", "-p", "-t", other, "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	st, enrollment := claudeFixtureEnrollment(t, true)
	state := st.Dir()
	grant, err := st.UsageGrantID(t.Context(), enrollment.WorkerID)
	if err != nil || grant == "" {
		t.Fatalf("fixture grant unavailable: %v", err)
	}
	manifestPath := filepath.Join(temp, claudeUsageManifest)
	manifest, err := json.Marshal(claudeUsageIntegration{Version: 1, Binary: binary, Shell: "/bin/sh", StateDir: state, Grant: claudeUsageGrant(grant)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"MINION_STATE_DIR": state, "MINION_SHARE_EXISTING": "all", "MINION_TMUX_SOCKET": srv.Socket(),
		"MINION_USAGE_CLAUDE_EXPORTER": "off", "MINION_USAGE_CODEX_EXPORTER": "off",
	}
	runProductionWorker(t, env)
	if status := productionStatus(t, env, state, true); !status.Connection.Live {
		t.Fatal("the fixture Worker did not answer")
	}
	target := srv.NewSession("observed", "env", "BINDING_HARNESS="+state, "BINDING_MANIFEST="+manifestPath, "CLAUDE_CONFIG_DIR="+temp,
		"MINION_USAGE_CLAUDE_EXPORTER=auto",
		"BINDING_WRONG_PANE="+strings.TrimSpace(otherPane),
		"XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"), "TMPDIR="+os.Getenv("TMPDIR"),
		harness, "-test.run=^TestHookPaneLiveHarness$")
	srv.WaitFor(target, "? for shortcuts", 5*time.Second)
	c := client.Local()
	snapshotPane := func() ops.PaneInfo {
		t.Helper()
		raw, err := c.Call(t.Context(), ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: ops.Target{SessionName: "observed"}})
		if err != nil {
			t.Fatal(err)
		}
		var snapshot ops.SessionSnapshotResult
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.Pane
	}
	pane := snapshotPane()
	if pane.Kind != "agent:claude" || pane.State != ops.PaneStateBusy || pane.StateSource != "claude-hooks" || !strings.Contains(pane.StateReason, "shell") {
		t.Fatalf("worker returned kind %q, state %q, reason %q, source %q", pane.Kind, pane.State, pane.StateReason, pane.StateSource)
	}

	listRaw, err := c.Call(t.Context(), ops.OpSessionsList, ops.SessionsListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, session := range list.Sessions {
		if session.Name == "observed" {
			listed = true
			if session.Pane == nil || session.Pane.State != pane.State || session.Pane.PaneStateDetail != pane.PaneStateDetail {
				t.Fatal("sessions_list did not report the observed pane's native background work")
			}
		}
	}
	if !listed {
		t.Fatal("sessions_list omitted the observed session")
	}

	// Respawn keeps the pane ID and session creation time. The old
	// background registry still must not describe the new process.
	if _, err := srv.Tmux("respawn-pane", "-k", "-t", pane.ID, "env", "BINDING_HARNESS="+state, "BINDING_REPLACEMENT=1",
		harness, "-test.run=^TestHookPaneLiveHarness$"); err != nil {
		t.Fatal(err)
	}
	srv.WaitFor(target, "replacement prompt", 5*time.Second)
	replacement := snapshotPane()
	if replacement.ID != pane.ID || replacement.SessionCreated != pane.SessionCreated || replacement.PID == pane.PID ||
		replacement.Kind != "agent:claude" || replacement.State != ops.PaneStateStable || replacement.StateSource != ops.PaneStateSourceScreen {
		t.Fatalf("replacement pane ID %q, PID %d, kind %q, state %q, source %q",
			replacement.ID, replacement.PID, replacement.Kind, replacement.State, replacement.StateSource)
	}
}

func TestHookPaneLiveHarness(t *testing.T) {
	temp := os.Getenv("BINDING_HARNESS")
	if temp == "" {
		t.Skip("runs inside the private tmux pane")
	}
	binary := os.Args[0]
	if !filepath.IsAbs(binary) || filepath.Base(binary) != "claude" {
		t.Fatalf("unexpected fixture executable %q", binary)
	}
	if os.Getenv("BINDING_REPLACEMENT") == "1" {
		fmt.Println("replacement prompt ? for shortcuts")
		time.Sleep(5 * time.Second) // keep the pane open for the parent capture
		return
	}
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestHookPaneLiveExporter$")
	cmd.Env = append(cmd.Environ(), "BINDING_EXPORTER=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("? for shortcuts")
	time.Sleep(5 * time.Second) // keep the pane open for the parent capture
}

func TestHookPaneLiveExporter(t *testing.T) {
	if os.Getenv("BINDING_EXPORTER") != "1" {
		t.Skip("runs beneath the synthetic foreground harness")
	}
	state := os.Getenv("BINDING_HARNESS")
	socket, _ := hookTmuxTarget(os.Getenv("TMUX"))
	session, pane, created, pid, birth := harnessPaneProcess(t.Context(), os.LookupEnv, state, "claude")
	if session != "observed" || pane != os.Getenv("TMUX_PANE") || created <= 0 || pid != os.Getppid() ||
		birth == "" || birth != agentdetect.ProcessBirth(pid) {
		t.Fatalf("unexpected live pane binding %q, %q, %d, %d (birth present %v)", session, pane, created, pid, birth != "")
	}
	cmd := newClaudeUsageExportCmd(os.LookupEnv)
	cmd.SetArgs([]string{"Stop", os.Getenv("BINDING_MANIFEST")})
	cmd.SetIn(bytes.NewBufferString(`{"session_id":"synthetic-native-session","hook_event_name":"Stop","background_tasks":[{"id":"synthetic-task","type":"shell"}]}`))
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		state      string
		socket     string
		lookupPane string
	}{
		{"other profile", filepath.Join(state, "different"), socket, pane},
		{"other socket", state, filepath.Join(state, "other.sock"), pane},
		{"other pane", state, socket, os.Getenv("BINDING_WRONG_PANE")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := paneBindingFixture{status: agent.LiveStatusReply{StateDir: tc.state, TmuxSocket: tc.socket}}
			lookup := func(key string) (string, bool) {
				if key == "TMUX_PANE" {
					return tc.lookupPane, true
				}
				return os.LookupEnv(key)
			}
			if session, _, _, _, _ := bindHarnessPane(t.Context(), lookup, state, "claude", fixture.Call); session != "" {
				t.Fatalf("unexpected binding to %q", session)
			}
		})
	}
}
