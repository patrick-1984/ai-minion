//go:build e2e && linux

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// SC-7: session control end to end. A real Hub and three Workers of one owner:
// W1, the controller, is the real binary ("minion worker up") with its local
// control socket; W2 and W3 are real Workers in this process with the default
// pipe_accept_from (none). The admin grants the session W1:boss all of W2 plus
// the session W3:build through the op. The controller session is simulated the
// way the local client names it: the real CLI runs with $TMUX_PANE set to a
// pane of W1's tmux server, and W1 maps that pane to its session.

// controller is the real "minion worker up" process of W1 and what its CLI
// needs: the runtime directory of its control socket and the pane ids of its
// sessions.
type controller struct {
	wid   string
	rt    string
	home  string
	panes map[string]string // session name -> pane id ($TMUX_PANE)
}

// startController enrolls W1 (named name, owned by ownerID) in its state
// directory as a paired Worker would be, starts the binary on it and waits
// until the Hub sees it online.
func startController(t *testing.T, ph *shareHub, ownerID, name string, sessions ...string) *controller {
	t.Helper()
	bin := minionBinary(t)
	state := t.TempDir()
	st, err := wstore.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, state)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: ph.wsv.Identity().Public()}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	c := &controller{wid: store.NewID(), home: t.TempDir(), panes: map[string]string{}}
	if err := ph.st.InsertWorker(context.Background(), nil, store.WorkerRow{ID: c.wid, Pub: key.Public(), Name: name, OwnerID: ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	srv := tmuxtest.New(t)
	for _, s := range sessions {
		srv.NewSession(s, "cat")
		pane, err := srv.Tmux("display-message", "-p", "-t", "="+s+":", "#{pane_id}")
		if err != nil {
			t.Fatal(err)
		}
		c.panes[s] = strings.TrimSpace(pane)
	}
	// A short runtime directory: unix socket paths are limited.
	if c.rt, err = os.MkdirTemp("", "sc"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(c.rt) })
	w := &minionWorker{done: make(chan error, 1)}
	w.cmd = exec.Command(bin, "worker", "up", "--foreground", "--accept-usage", "--server", "https://"+ph.addr, "--share-existing", "all",
		"--state-dir", state, "--tmux-socket", srv.Socket(), "--name", name)
	w.cmd.Env = c.env("")
	w.cmd.Stdout, w.cmd.Stderr = &w.out, &w.out
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { w.done <- w.cmd.Wait() }()
	t.Cleanup(func() { _ = w.stop(t) })
	deadline := time.Now().Add(20 * time.Second)
	for !ph.wsv.Registry().IsOnline(c.wid) || !c.socketUp() {
		if time.Now().After(deadline) {
			t.Fatalf("the controller Worker did not come online:\n%s", w.out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c
}

// env is the environment of a process on W1's host: no Hub URL, no inherited
// MINION_* or TMUX, and $TMUX_PANE when pane is not empty.
func (c *controller) env(pane string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.home, "XDG_RUNTIME_DIR=" + c.rt,
		"XDG_STATE_HOME=" + filepath.Join(c.home, "state"), "XDG_CONFIG_HOME=" + filepath.Join(c.home, "config")}
	if pane != "" {
		env = append(env, "TMUX_PANE="+pane)
	}
	return env
}

func (c *controller) socketUp() bool {
	_, err := os.Stat(filepath.Join(c.rt, "minion", "worker.sock"))
	return err == nil
}

// cli runs "minion <args>" from the session of W1 and returns its exit code
// and output.
func (c *controller) cli(t *testing.T, session string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, minionBinary(t), args...)
	cmd.Env = c.env(c.panes[session])
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("minion %v: %v", args, err)
	}
	return code, string(out)
}

// mcpCall runs "minion mcp --allow-writes" from the session of W1, calls the
// tool once and returns the tools/call result.
func (c *controller) mcpCall(t *testing.T, session, tool string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, minionBinary(t), "mcp", "--allow-writes")
	cmd.Env = c.env(c.panes[session])
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`+"\n"+
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+string(call)+"\n")
	var result string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.ID == 2 {
			result = string(msg.Result)
			break
		}
	}
	_ = in.Close()
	_ = cmd.Wait()
	if result == "" {
		t.Fatalf("minion mcp gave no answer to tools/call %s; stderr:\n%s", tool, stderr.String())
	}
	return result
}

func TestSessionControlE2EGrantFromAControllerSessionAcrossWorkers(t *testing.T) {
	ph := startShareHub(t)
	ownerID, _ := (&shareE2E{t: t, ph: ph}).user("olivia")
	w2 := startCallPeer(t, ph, ownerID, "w2", "", "x")
	w3 := startCallPeer(t, ph, ownerID, "w3", "", "build", "other")
	w1 := startController(t, ph, ownerID, "w1", "boss", "other")
	w2.tmux.SendKeys("=x:", "SCREEN-OF-W2-X", "Enter")
	w2.tmux.WaitFor("=x:", "SCREEN-OF-W2-X", 5*time.Second)

	list := func() []ops.SessionControlInfo {
		t.Helper()
		code, body := ph.do("POST", "/api/v1/ops/"+ops.OpSessionControlList, map[string]any{}, ph.admin)
		var res ops.SessionControlListResult
		if code != 200 || json.Unmarshal([]byte(body), &res) != nil {
			t.Fatalf("session_control_list = %d %s", code, body)
		}
		return res.Grants
	}
	// v0.9.0: the grant is made in Minion control: W1:boss becomes a master
	// session, W2 accepts W1 (all its shared sessions) and W3 accepts W1 for
	// build only. set(nil) ends the master session.
	control := func(op string, b map[string]any) {
		t.Helper()
		b["request_id"] = ops.NewUUIDv7()
		code, body := ph.do("POST", "/api/v1/ops/"+op, b, ph.admin)
		if code != 200 || !strings.Contains(body, string(ops.OutcomeDelivered)) {
			t.Fatalf("%s = %d %s", op, code, body)
		}
	}
	set := func(targets []map[string]any) {
		t.Helper()
		if targets == nil {
			control(ops.OpControlSetMaster, map[string]any{"worker_id": w1.wid, "session_name": "boss", "master": false})
			return
		}
		control(ops.OpControlSetMaster, map[string]any{"worker_id": w1.wid, "session_name": "boss", "master": true})
		control(ops.OpControlSetAccept, map[string]any{"worker_id": w2.wid, "source_worker_id": w1.wid, "accept": true})
		control(ops.OpControlSetAccept, map[string]any{"worker_id": w3.wid, "source_worker_id": w1.wid, "accept": true, "sessions": []string{"build"}})
	}
	// refused runs the CLI from session and wants it refused with the error code want.
	refused := func(session, want string, args ...string) {
		t.Helper()
		if code, out := w1.cli(t, session, append(args, "--json")...); code != 1 || !strings.Contains(out, "Error: "+want+":") {
			t.Fatalf("minion %v from W1:%s = %d %s, want refused with %s", args, session, code, out, want)
		}
	}
	inW3 := func(session, text string) bool { return strings.Contains(w3.tmux.Capture("="+session+":"), text) }

	// Acceptance 7: a fresh Hub holds no grant, and nothing crosses Workers.
	if g := list(); len(g) != 0 {
		t.Fatalf("a fresh Hub lists grants: %+v", g)
	}
	refused("boss", "forbidden", "workers_list")
	refused("boss", "forbidden", "send_text", "--target", w3.wid+":build", "--text", "BEFORE-GRANT")

	// Acceptance 1: the admin creates the grant W1:boss -> W2 (whole) + W3:build.
	set([]map[string]any{
		{"kind": "worker", "worker_id": w2.wid},
		{"kind": "session", "worker_id": w3.wid, "session_name": "build"},
	})
	if g := list(); len(g) != 1 || g[0].WorkerID != w1.wid || g[0].SessionName != "boss" || len(g[0].Targets) != 2 {
		t.Fatalf("grants after the set = %+v", g)
	}

	// Acceptance 2: from W1:boss the CLI lists, reads and types across Workers;
	// W2 and W3 have no pipe_accept_from configuration.
	// (workers_list lists the Workers the grant may read as a whole: W2 here.)
	code, out := w1.cli(t, "boss", "workers_list", "--json")
	if code != 0 || !strings.Contains(out, w2.wid) {
		t.Fatalf("workers_list from W1:boss = %d %s", code, out)
	}
	if code, out := w1.cli(t, "boss", "sessions_list", "--hub-worker", w2.wid, "--json"); code != 0 || !strings.Contains(out, `"x"`) {
		t.Fatalf("sessions_list --hub-worker W2 from W1:boss = %d %s", code, out)
	}
	if code, out := w1.cli(t, "boss", "session_snapshot", "--target", w2.wid+":x", "--json"); code != 0 || !strings.Contains(out, "SCREEN-OF-W2-X") {
		t.Fatalf("session_snapshot W2:x from W1:boss = %d %s", code, out)
	}
	if code, out := w1.cli(t, "boss", "send_text", "--target", w3.wid+":build", "--text", "TYPED-BY-BOSS", "--json"); code != 0 {
		t.Fatalf("send_text W3:build from W1:boss = %d %s", code, out)
	}
	waitFor(t, "the text in W3:build", func() bool { return inW3("build", "TYPED-BY-BOSS") })
	// The same through minion mcp --allow-writes.
	if res := w1.mcpCall(t, "boss", ops.OpSendText, map[string]any{"request_id": ops.NewUUIDv7(),
		"target": map[string]any{"worker_id": w3.wid, "session_name": "build"}, "text": "TYPED-BY-MCP"}); strings.Contains(res, `"isError":true`) {
		t.Fatalf("send_text through MCP from W1:boss = %s", res)
	}
	waitFor(t, "the MCP text in W3:build", func() bool { return inW3("build", "TYPED-BY-MCP") })
	if res := w1.mcpCall(t, "boss", ops.OpSessionSnapshot, map[string]any{
		"target": map[string]any{"worker_id": w2.wid, "session_name": "x"}}); !strings.Contains(res, "SCREEN-OF-W2-X") {
		t.Fatalf("session_snapshot through MCP from W1:boss = %s", res)
	}
	if res := w1.mcpCall(t, "boss", ops.OpWorkersList, map[string]any{}); !strings.Contains(res, w2.wid) {
		t.Fatalf("workers_list through MCP from W1:boss = %s", res)
	}

	// Acceptance 3: another session of the same Worker is refused.
	refused("other", "forbidden", "workers_list")
	refused("other", "forbidden", "session_snapshot", "--target", w2.wid+":x")
	refused("other", "forbidden", "send_text", "--target", w3.wid+":build", "--text", "FROM-OTHER")

	// Acceptance 4: a session of W3 outside the grant is refused.
	refused("boss", "not_found", "send_text", "--target", w3.wid+":other", "--text", "OUT-OF-SCOPE")
	refused("boss", "not_found", "session_snapshot", "--target", w3.wid+":other")

	// Acceptance 5: removing the grant ends access on the next call.
	set(nil)
	if g := list(); len(g) != 0 {
		t.Fatalf("grants after the removal = %+v", g)
	}
	refused("boss", "forbidden", "session_snapshot", "--target", w2.wid+":x")
	refused("boss", "forbidden", "send_text", "--target", w3.wid+":build", "--text", "AFTER-REMOVAL")

	// Nothing refused was typed anywhere.
	for _, s := range []string{"build", "other"} {
		for _, text := range []string{"BEFORE-GRANT", "FROM-OTHER", "OUT-OF-SCOPE", "AFTER-REMOVAL"} {
			if inW3(s, text) {
				t.Fatalf("a refused text %q reached W3:%s", text, s)
			}
		}
	}
}
