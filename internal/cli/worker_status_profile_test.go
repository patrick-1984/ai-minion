package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// replyBackend is the control socket of one running Worker.
type replyBackend struct {
	reply agent.LiveStatusReply
	err   error
}

func (b replyBackend) Call(context.Context, string, any) (json.RawMessage, error) {
	if b.err != nil {
		return nil, b.err
	}
	return json.Marshal(b.reply)
}

func (b replyBackend) Stream(ctx context.Context, op string, args any, _ func(json.RawMessage) error) (json.RawMessage, error) {
	return b.Call(ctx, op, args)
}

func statusJSON(t *testing.T, sd workerStatusDeps, args ...string) map[string]any {
	t.Helper()
	out, _, err := runWorkerCmds(t, sd, testConfigDeps(nil), append([]string{"worker", "status", "--json"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return m
}

// A Worker running with --state-dir X: status of X shows it live with the
// daemon's version beside the CLI's; status of the default profile names it
// as live_elsewhere instead of "stopped".
func TestStatusCustomStateForegroundWorker(t *testing.T) {
	x := t.TempDir()
	home := t.TempDir()
	reply := agent.LiveStatusReply{StateDir: x, Instance: "inst-x", State: "idle", Version: "v0.15.0-daemon", Capabilities: []string{"pause.v1"}}
	sd := testStatusDeps(map[string]string{"XDG_STATE_HOME": home, "HOME": home, "MINION_TMUX_SOCKET": filepath.Join(x, "t.sock")})
	sd.live = func(dir string) agent.LiveSource {
		return localLive{stateDir: dir, backend: replyBackend{reply: reply}}
	}

	m := statusJSON(t, sd, "--state-dir", x)
	live := m["live"].(map[string]any)
	if live["running"] != true || live["state_dir"] != x || live["version"] != "v0.15.0-daemon" || live["instance"] != "inst-x" {
		t.Fatalf("live = %v", live)
	}
	if m["cli_version"] != "v9.9.9" || m["live_elsewhere"] != nil {
		t.Fatalf("cli_version %v, live_elsewhere %v", m["cli_version"], m["live_elsewhere"])
	}
	if p := m["profile"].(map[string]any); p["state_dir"] != x || p["default"] != false {
		t.Fatalf("profile = %v", p)
	}
	if caps := m["capabilities"].([]any); len(caps) != 1 || caps[0] != "pause.v1" {
		t.Fatalf("capabilities = %v", caps)
	}

	// The default profile: the Worker of X is elsewhere, this one is not live.
	d := statusJSON(t, sd)
	if d["live"].(map[string]any)["running"] != false {
		t.Fatalf("default profile live = %v", d["live"])
	}
	el, _ := d["live_elsewhere"].(map[string]any)
	if el == nil || el["state_dir"] != x || el["instance"] != "inst-x" {
		t.Fatalf("live_elsewhere = %v", d["live_elsewhere"])
	}
	if d["profile"].(map[string]any)["default"] != true {
		t.Fatalf("default profile = %v", d["profile"])
	}
}

// Without a running Worker the connection is unknown and nothing is live.
func TestStatusOfflineUnknownIsNotLive(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(map[string]string{"MINION_SERVER": "https://hub.example"})
	m := statusJSON(t, sd, "--state-dir", dir)
	if m["connection"].(map[string]any)["state"] != "unknown" || m["live"].(map[string]any)["running"] != false {
		t.Fatalf("connection %v, live %v", m["connection"], m["live"])
	}
}

// An untrusted control socket is a problem with a code, not "stopped".
func TestStatusDeniedOwnerDistinct(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(nil)
	sd.live = func(d string) agent.LiveSource {
		return localLive{stateDir: d, backend: replyBackend{err: fmt.Errorf("dial: %w", client.ErrPermissionDenied)}}
	}
	m := statusJSON(t, sd, "--state-dir", dir)
	codes, _ := m["problem_codes"].([]any)
	if len(codes) != 1 || codes[0] != agent.ProblemControlSocketDenied {
		t.Fatalf("problem_codes = %v (problems %v)", m["problem_codes"], m["problems"])
	}
	if m["live"].(map[string]any)["running"] != false || m["live_elsewhere"] != nil {
		t.Fatalf("live %v elsewhere %v", m["live"], m["live_elsewhere"])
	}
}

// A stored mismatch is readable while no Worker runs.
func TestStatusStoredMismatchWhileStopped(t *testing.T) {
	dir := t.TempDir()
	hubFpr, _ := enrollWorker(t, dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	e, _, err := enr.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetHubCheck(context.Background(), e.Pin.Pub, store.HubCheckMismatch, hubFpr); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	m := statusJSON(t, testStatusDeps(nil), "--state-dir", dir)
	if m["hub_check"] != store.HubCheckMismatch || m["live"].(map[string]any)["running"] != false {
		t.Fatalf("hub_check %v, live %v", m["hub_check"], m["live"])
	}
}
