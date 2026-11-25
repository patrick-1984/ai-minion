package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// SC-5: the local socket forwards session-control requests whose target is on
// another Worker to the Hub, with the calling session as source_session.

func TestForwardsToHubOnlyForeignSessionControlTargets(t *testing.T) {
	self, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	target := func(worker string) json.RawMessage {
		return mustJSON(t, map[string]any{"target": ops.Target{WorkerID: worker, SessionName: "x"}})
	}
	for _, tc := range []struct {
		name string
		op   string
		args json.RawMessage
		self string
		want bool
	}{
		{"workers_list", ops.OpWorkersList, nil, self, true},
		{"sessions_list of another Worker", ops.OpSessionsList, mustJSON(t, map[string]string{"worker_id": other}), self, true},
		{"sessions_list of this Worker", ops.OpSessionsList, mustJSON(t, map[string]string{"worker_id": self}), self, false},
		{"sessions_list plain", ops.OpSessionsList, nil, self, false},
		{"snapshot on another Worker", ops.OpSessionSnapshot, target(other), self, true},
		{"send_text on another Worker", ops.OpSendText, target(other), self, true},
		{"answer on another Worker without an own id", ops.OpAnswer, target(other), "", true},
		{"snapshot on this Worker", ops.OpSessionSnapshot, target(self), self, false},
		{"snapshot without a Worker", ops.OpSessionSnapshot, target(""), self, false},
		{"pipe_send stays with the pipe", ops.OpPipeSend, target(other), self, false},
		{"session_create is not forwarded", ops.OpSessionCreate, mustJSON(t, map[string]string{"worker_id": other}), self, false},
	} {
		if got := forwardsToHub(tc.op, tc.args, tc.self); got != tc.want {
			t.Errorf("%s: forwardsToHub = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionOfPaneMapsThePaneIDOfTheWorkersHost(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("one", "bash")
	ref := e.host.addSession("boss", "claude")
	if got := e.ex.sessionOfPane(context.Background(), ref.ID); got != "boss" {
		t.Fatalf("sessionOfPane(%s) = %q, want boss", ref.ID, got)
	}
	for _, p := range []string{"", "%999"} {
		if got := e.ex.sessionOfPane(context.Background(), p); got != "" {
			t.Errorf("sessionOfPane(%q) = %q, want none", p, got)
		}
	}
}

// fwdEnv is a local executor whose call client is linked to a fake Hub that
// answers every call with result.
func fwdEnv(t *testing.T, result any) (*env, localExecutor, *callHub) {
	t.Helper()
	e := newEnv(t)
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": result})
	})
	c := NewCallClient(CallConfig{})
	t.Cleanup(h.link(c))
	return e, localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return c }}, h
}

func TestForeignTargetIsForwardedWithTheSourceSession(t *testing.T) {
	e, l, h := fwdEnv(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	ref := e.host.addSession("boss", "claude")
	other := ops.NewUUIDv7()
	args := mustJSON(t, ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: other, SessionName: "build"}, Text: "hi"})
	rq := localapiRequest(ops.OpSendText, args)
	rq.Pane = ref.ID
	raw, err := l.Execute(context.Background(), rq, func(json.RawMessage) {})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s", raw)
	}
	req := h.requestAt(0)
	if string(req["op"]) != `"send_text"` || string(req["source_session"]) != `"boss"` || !strings.Contains(string(req["args"]), other) {
		t.Fatalf("the Hub got %v", req)
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("a forwarded request was typed into a local pane")
	}
}

func TestForwardWithAnUnknownPaneSendsNoSourceSession(t *testing.T) {
	_, l, h := fwdEnv(t, ops.WorkersListResult{})
	rq := localapiRequest(ops.OpWorkersList, nil)
	rq.Pane = "%999"
	if _, err := l.Execute(context.Background(), rq, func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.requestAt(0)["source_session"]; ok {
		t.Fatalf("the Hub got a source_session for an unknown pane: %v", h.requestAt(0))
	}
}

func TestSameWorkerTargetStaysLocal(t *testing.T) {
	e, l, h := fwdEnv(t, struct{}{})
	self := ops.NewUUIDv7()
	e.policy.SetWorkerID(self)
	e.host.addSession("work", "bash")
	for _, w := range []string{"", self} {
		args := mustJSON(t, ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: w, SessionName: "work"}})
		if _, err := l.Execute(context.Background(), localapiRequest(ops.OpSessionSnapshot, args), func(json.RawMessage) {}); err != nil {
			t.Fatalf("worker %q: %v", w, err)
		}
	}
	h.mu.Lock()
	n := len(h.requests)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d requests reached the Hub", n)
	}
}

func TestForeignTargetWithoutAHubConnectionIsAClearError(t *testing.T) {
	e := newEnv(t)
	args := mustJSON(t, ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "x"}})
	for name, calls := range map[string]func() PipeCaller{
		"no call client":   nil,
		"not connected":    func() PipeCaller { return NewCallClient(CallConfig{}) },
		"nil call service": func() PipeCaller { return nil },
	} {
		l := localExecutor{ex: e.ex, log: discardLogger(), calls: calls}
		_, err := l.Execute(context.Background(), localapi.Request{Op: ops.OpSessionSnapshot, Actions: localapi.FullActions(), Args: args}, func(json.RawMessage) {})
		if proto.CodeOf(err) != proto.CodeWorkerOffline || !strings.Contains(err.Error(), "not connected to a Hub") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// TestTheLocalSocketHasTheForwardFeature: the client qualifies a request that
// names a Worker with session.forward (ops.LocalRequiredFeatures); this Worker
// accepts it and forwards it, while an older daemon refuses the qualified name.
func TestTheLocalSocketHasTheForwardFeature(t *testing.T) {
	_, l, h := fwdEnv(t, struct{}{})
	args := mustJSON(t, ops.SessionSnapshotArgs{Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "x"}})
	wire := localapi.QualifiedOp(ops.OpSessionSnapshot, ops.LocalRequiredFeatures(ops.OpSessionSnapshot, args))
	if wire != "session_snapshot+session.forward" {
		t.Fatalf("wire name = %q", wire)
	}
	if _, err := l.Execute(context.Background(), localapiRequest(wire, args), func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if string(h.requestAt(0)["op"]) != `"session_snapshot"` {
		t.Fatalf("the Hub got %v", h.requestAt(0))
	}
}

// socketHost is the fake host as a tmux server on one socket (the port
// tmuxServerHost of the tmux host); psmux marks it as psmux.
type socketHost struct {
	*fakeHost
	socket string
	psmux  bool
}

func (h socketHost) Socket() string { return h.socket }
func (h socketHost) Psmux() bool    { return h.psmux }

// onSocket makes the fake host of newEnv a tmux server on socket.
func onSocket(socket string) func(*Config) {
	return func(c *Config) { c.Host = socketHost{fakeHost: c.Host.(*fakeHost), socket: socket} }
}

// touch creates an empty file at path, for a socket path that exists.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A-02: a pane id is only meaningful on one tmux server. The caller's socket
// ($TMUX) must be the one of the server this Worker serves.

func TestSourceSessionMapsAPaneOfTheSameTmuxServer(t *testing.T) {
	dir := t.TempDir()
	own := touch(t, filepath.Join(dir, "default"))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	e := newEnv(t, onSocket(own))
	ref := e.host.addSession("boss", "claude")
	for name, socket := range map[string]string{
		"the same path":           own,
		"the same path, unclean":  filepath.Join(dir, "x", "..", "default") + string(filepath.Separator),
		"another path, same file": link,
	} {
		if got := e.ex.sourceSession(context.Background(), ref.ID, socket); got != "boss" {
			t.Errorf("%s: sourceSession(%s, %s) = %q, want boss", name, ref.ID, socket, got)
		}
	}
	// A server whose socket file is gone (not started yet) still is itself.
	gone := newEnv(t, onSocket(filepath.Join(dir, "gone")))
	ref = gone.host.addSession("boss", "claude")
	if got := gone.ex.sourceSession(context.Background(), ref.ID, filepath.Join(dir, "gone")); got != "boss" {
		t.Errorf("the same path without a file: %q, want boss", got)
	}
}

func TestSourceSessionOfAPaneOnAnotherTmuxServerIsNone(t *testing.T) {
	dir := t.TempDir()
	own := touch(t, filepath.Join(dir, "default"))
	other := touch(t, filepath.Join(dir, "other"))
	e := newEnv(t, onSocket(own))
	ref := e.host.addSession("boss", "claude") // the other server has this pane id too
	for name, socket := range map[string]string{
		"another server":          other,
		"a server that is gone":   filepath.Join(dir, "gone"),
		"a relative path":         "default",
		"a path that is too long": "/" + strings.Repeat("a", maxTmuxSocketPath),
	} {
		if got := e.ex.sourceSession(context.Background(), ref.ID, socket); got != "" {
			t.Errorf("%s: sourceSession(%s, %s) = %q, want none", name, ref.ID, socket, got)
		}
	}
	// The Worker's own socket file is gone and the caller names another path.
	gone := newEnv(t, onSocket(filepath.Join(dir, "gone")))
	ref = gone.host.addSession("boss", "claude")
	if got := gone.ex.sourceSession(context.Background(), ref.ID, other); got != "" {
		t.Errorf("a Worker whose socket is gone mapped a pane of another server to %q", got)
	}
}

func TestSourceSessionOnAHostThatIsNotATmuxServerIsNone(t *testing.T) {
	plain := newEnv(t) // the fake host has no socket (as ConPTY)
	ref := plain.host.addSession("boss", "claude")
	if got := plain.ex.sourceSession(context.Background(), ref.ID, "/tmp/tmux-1000/default"); got != "" {
		t.Errorf("a host without a tmux server mapped the pane to %q", got)
	}
	unnamed := newEnv(t, onSocket("")) // a tmux host that names no socket
	ref = unnamed.host.addSession("boss", "claude")
	if got := unnamed.ex.sourceSession(context.Background(), ref.ID, "/tmp/tmux-1000/default"); got != "" {
		t.Errorf("a host that names no socket mapped the pane to %q", got)
	}
	// psmux has one server per session and no socket to compare: unchanged.
	psmux := newEnv(t, func(c *Config) { c.Host = socketHost{fakeHost: c.Host.(*fakeHost), psmux: true} })
	ref = psmux.host.addSession("boss", "claude")
	if got := psmux.ex.sourceSession(context.Background(), ref.ID, "/tmp/tmux-1000/default"); got != "boss" {
		t.Errorf("psmux: sourceSession = %q, want boss", got)
	}
}

// TestSourceSessionOfAnOlderClientIsUnchangedAndCounted: a client that sends
// no tmux_socket is mapped as before, on any host, and the debug log counts it.
func TestSourceSessionOfAnOlderClientIsUnchangedAndCounted(t *testing.T) {
	var buf bytes.Buffer
	log := func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	before := panesWithoutServer.Load()
	for name, e := range map[string]*env{
		"a host without a socket": newEnv(t, log),
		"a tmux server":           newEnv(t, log, onSocket(filepath.Join(t.TempDir(), "default"))),
	} {
		ref := e.host.addSession("boss", "claude")
		if got := e.ex.sourceSession(context.Background(), ref.ID, ""); got != "boss" {
			t.Errorf("%s: sourceSession(%s) = %q, want boss", name, ref.ID, got)
		}
		if got := e.ex.sourceSession(context.Background(), "%999", ""); got != "" {
			t.Errorf("%s: an unknown pane gave %q", name, got)
		}
		if got := e.ex.sourceSession(context.Background(), "", ""); got != "" {
			t.Errorf("%s: no pane gave %q", name, got)
		}
	}
	// Two hosts, two requests with a pane each; a request without a pane is
	// not counted.
	if n := panesWithoutServer.Load() - before; n < 4 {
		t.Errorf("counted %d requests without a tmux server, want at least 4", n)
	}
	if got := strings.Count(buf.String(), "predates tmux_socket"); got != 4 {
		t.Errorf("%d debug lines, want 4:\n%s", got, buf.String())
	}
}

// TestForwardNamesNoSourceSessionForAPaneOfAnotherTmuxServer: the whole path
// from the local request to the call frame. The pane id exists on this
// Worker's server too (session boss), and still boss is not named.
func TestForwardNamesNoSourceSessionForAPaneOfAnotherTmuxServer(t *testing.T) {
	dir := t.TempDir()
	own := touch(t, filepath.Join(dir, "default"))
	other := touch(t, filepath.Join(dir, "other"))
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": ops.WorkersListResult{}})
	})
	c := NewCallClient(CallConfig{})
	t.Cleanup(h.link(c))
	e := newEnv(t, onSocket(own))
	l := localExecutor{ex: e.ex, log: discardLogger(), calls: func() PipeCaller { return c }}
	ref := e.host.addSession("boss", "claude")

	rq := localapiRequest(ops.OpWorkersList, nil)
	rq.Pane, rq.TmuxSocket = ref.ID, other
	if _, err := l.Execute(context.Background(), rq, func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if v, ok := h.requestAt(0)["source_session"]; ok {
		t.Fatalf("the Hub got source_session %s for a pane of another tmux server", v)
	}
	rq.TmuxSocket = own
	if _, err := l.Execute(context.Background(), rq, func(json.RawMessage) {}); err != nil {
		t.Fatal(err)
	}
	if got := string(h.requestAt(1)["source_session"]); got != `"boss"` {
		t.Fatalf("the same server: source_session = %s, want boss", got)
	}
}
