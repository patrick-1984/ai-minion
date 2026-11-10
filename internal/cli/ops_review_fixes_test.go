package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func hubDeps(hub *fakeOpsBackend) opsDeps {
	deps := testDeps(&fakeOpsBackend{})
	deps.hub = func() (client.Backend, error) { return hub, nil }
	return deps
}

func deliveredHub() *fakeOpsBackend {
	return &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// password_change needs two secrets; each has its own argv-free source.
func TestPasswordChangeWithBothPasswordsOffArgv(t *testing.T) {
	oldFile := writeSecretFile(t, "old-password-1\n")
	newFile := writeSecretFile(t, "new-password-22\r\n")

	cases := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"two files", "", []string{"--old-password-file", oldFile, "--new-password-file", newFile}},
		{"file and stdin", "new-password-22\n", []string{"--old-password-file", oldFile, "--new-password", "-"}},
		{"stdin and file", "old-password-1\r\n", []string{"--old-password", "-", "--new-password-file", newFile}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hub := deliveredHub()
			code, _, errs := runWith(t, hubDeps(hub), c.stdin, append([]string{"password_change"}, c.args...)...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			got := decodeArgs(t, hub.last(t).Args)
			if got["old_password"] != "old-password-1" || got["new_password"] != "new-password-22" {
				t.Errorf("the Hub got old=%q new=%q", got["old_password"], got["new_password"])
			}
			if !strings.Contains(string(hub.last(t).Args), `"request_id"`) {
				t.Errorf("no request id in %s", hub.last(t).Args)
			}
		})
	}

	// Only one trailing line break is removed.
	hub := deliveredHub()
	long := writeSecretFile(t, "new-password-22\n\n")
	if code, _, errs := runWith(t, hubDeps(hub), "", "password_change", "--old-password-file", oldFile, "--new-password-file", long); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, hub.last(t).Args)["new_password"]; got != "new-password-22\n" {
		t.Errorf("new_password = %q", got)
	}
}

func TestPasswordChangeRefusesLiteralsAndAmbiguousSources(t *testing.T) {
	oldFile := writeSecretFile(t, "old-password-1\n")
	for _, args := range [][]string{
		{"password_change", "--old-password", "hunter2", "--new-password-file", oldFile},
		{"password_change", "--old-password-file", oldFile, "--new-password", "hunter2hunter2"},
		{"password_change", "--old-password", "-", "--old-password-file", oldFile, "--new-password-file", oldFile},
		{"password_change", "--old-password", "-", "--new-password", "-"},
		{"password_change", "--old-password-file", filepath.Join(t.TempDir(), "missing"), "--new-password-file", oldFile},
	} {
		hub := deliveredHub()
		code, _, errs := runWith(t, hubDeps(hub), "x\n", args...)
		if code != ExitUsage || hub.count() != 0 {
			t.Errorf("%v: exit %d, calls %d, stderr %q", args, code, hub.count(), errs)
		}
		if strings.Contains(errs, "hunter2") {
			t.Errorf("%v: the literal secret was echoed: %q", args, errs)
		}
	}
}

func TestSecretSettingFromStdinLosesItsNewline(t *testing.T) {
	hub := deliveredHub()
	if code, _, errs := runWith(t, hubDeps(hub), "s3cret\r\n", "settings_set", "--key", "oidc_client_secret", "--value", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got := decodeArgs(t, hub.last(t).Args)["value"]; got != "s3cret" {
		t.Errorf("value = %q", got)
	}
	// Content that is not a secret stays verbatim.
	if code, _, errs := runWith(t, hubDeps(hub), "text\n", "send_text", "--target", "w", "--text", "-"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestProtocolErrorDuringAWriteIsUnknown(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(nil, fmt.Errorf("%w: bad frame", client.ErrProtocol))}
	code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
	if code != ExitPartial {
		t.Errorf("exit %d, want 4", code)
	}
	if !strings.Contains(out, "outcome: unknown") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errs, "--request-id "+testRequestID) || !strings.Contains(errs, "10 minutes") {
		t.Errorf("stderr lacks the retry hint: %q", errs)
	}
	// A read with a protocol error is an ordinary error.
	if code, _, _ := runWith(t, testDeps(b), "", "session_snapshot", "--target", "w"); code != ExitError {
		t.Errorf("read: exit %d, want 1", code)
	}
}

func TestUndecodableWriteResultIsUnknown(t *testing.T) {
	b := &fakeOpsBackend{call: answerWith(json.RawMessage(`"not a write result"`), nil)}
	code, out, errs := runWith(t, testDeps(b), "", "send_text", "--target", "w", "--text", "x")
	if code != ExitPartial {
		t.Errorf("exit %d, want 4", code)
	}
	if !strings.Contains(out, "outcome: unknown") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errs, "--request-id "+testRequestID) || !strings.Contains(errs, "10 minutes") {
		t.Errorf("stderr lacks the retry hint: %q", errs)
	}
	if b.count() != 1 {
		t.Errorf("calls = %d", b.count())
	}
}

func TestExtendedAccountResultsKeepTheirExtraMembers(t *testing.T) {
	res := ops.UserCreateResult{
		WriteResult:       ops.WriteResult{Outcome: ops.OutcomeDelivered},
		User:              ops.UserInfo{ID: "u1", Username: "alice", Role: "user"},
		TemporaryPassword: "Temp-Pass-123",
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	hub := &fakeOpsBackend{call: answerWith(raw, nil)}
	code, out, errs := runWith(t, hubDeps(hub), "", "user_create", "--username", "alice")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "temporary_password: Temp-Pass-123") || !strings.Contains(out, "alice") || !strings.Contains(out, "only this once") {
		t.Errorf("human output lacks the extra members:\n%s", out)
	}
	_, jsonOut, _ := runWith(t, hubDeps(hub), "", "user_create", "--username", "alice", "--json")
	if !strings.Contains(jsonOut, `"temporary_password":"Temp-Pass-123"`) {
		t.Errorf("json output = %q", jsonOut)
	}

	reset, _ := json.Marshal(ops.UserResetPasswordResult{WriteResult: ops.WriteResult{Outcome: ops.OutcomeDelivered}, UserID: "u1", TemporaryPassword: "Reset-Pass-9"})
	hub.call = answerWith(reset, nil)
	_, out, _ = runWith(t, hubDeps(hub), "", "user_reset_password", "--user-id", "01890a5d-ac96-774b-bcce-b302099a8057")
	if !strings.Contains(out, "temporary_password: Reset-Pass-9") {
		t.Errorf("reset output = %q", out)
	}
	// A plain write result prints no extra lines.
	_, plain, _ := runWith(t, testDeps(deliveredHub()), "", "send_text", "--target", "w", "--text", "x")
	if strings.Contains(plain, "only this once") {
		t.Errorf("plain output = %q", plain)
	}
}

// --- the confirmed Worker is the one that is written to -----------------------

func TestImplicitTargetIsSentToTheConfirmedSocketOnly(t *testing.T) {
	a := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	b := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	byPath := map[string]*fakeOpsBackend{"/a.sock": a, "/b.sock": b}
	deps := testDeps(&fakeOpsBackend{call: answerWith(nil, errors.New("the unpinned backend was used"))})
	deps.session = func(context.Context) (string, error) { return "minion-9", nil }
	deps.localSockets = func() ([]string, error) { return []string{"/a.sock", "/b.sock"}, nil }
	deps.localAt = func(p string) client.Backend { return byPath[p] }

	// A serves another tmux server: B, which confirms, receives the write.
	deps.confirm = func(_ context.Context, _ string, w client.Backend) error {
		if w == client.Backend(a) {
			return errors.New("not the one the Worker serves")
		}
		return nil
	}
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != 0 || a.count() != 0 || b.count() != 1 {
		t.Fatalf("exit %d (%s), A %d, B %d", code, errs, a.count(), b.count())
	}

	// A confirms and then disappears: the write fails and B never gets it.
	a.call = func(op string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, fmt.Errorf("%w (socket: /a.sock)", client.ErrWorkerNotRunning)
	}
	b.mu.Lock()
	b.calls = nil
	b.mu.Unlock()
	deps.confirm = func(context.Context, string, client.Backend) error { return nil }
	code, _, errs := runWith(t, deps, "", "send_text", "--text", "x")
	if code == 0 || b.count() != 0 {
		t.Errorf("failover: exit %d (%s), B calls %d", code, errs, b.count())
	}

	// Nothing confirms: refused, nothing sent, and the refusal names the cause.
	deps.confirm = func(context.Context, string, client.Backend) error { return errors.New("other server") }
	before := a.count()
	code, _, errs = runWith(t, deps, "", "send_text", "--text", "x")
	if code != ExitUsage || !strings.Contains(errs, "other server") || a.count() != before || b.count() != 0 {
		t.Errorf("refused: exit %d, stderr %q", code, errs)
	}
	// Nobody runs: the refusal says so.
	deps.confirm = func(context.Context, string, client.Backend) error { return client.ErrWorkerNotRunning }
	if code, _, errs := runWith(t, deps, "", "send_text", "--text", "x"); code != ExitUsage || !strings.Contains(errs, "Worker") {
		t.Errorf("no worker: exit %d, stderr %q", code, errs)
	}
}

// --- the server probe never touches the user's task text ---------------------

// markFailServer applies SetOption and then reports an error once, like a
// tmux call that ran into its timeout after it had done its work.
type markFailServer struct {
	*fakeServer
	once sync.Once
}

func (m *markFailServer) SetOption(ctx context.Context, session, key, value string) error {
	err := m.fakeServer.SetOption(ctx, session, key, value)
	if err != nil {
		return err
	}
	var fail error
	m.once.Do(func() { fail = errors.New("timeout after the option was applied") })
	return fail
}

// afterListServer runs hook(n) after the n-th tmux call (ListSessions or
// SetOption, 1-based), the way another writer lands between two calls of the
// probe.
type afterListServer struct {
	*fakeServer
	mu    sync.Mutex
	calls int
	hook  func(n int)
}

func (a *afterListServer) SetOption(ctx context.Context, session, key, value string) error {
	err := a.fakeServer.SetOption(ctx, session, key, value)
	a.after()
	return err
}

func (a *afterListServer) ListSessions(ctx context.Context) ([]term.Session, error) {
	out, err := a.fakeServer.ListSessions(ctx)
	a.after()
	return out, err
}

func (a *afterListServer) after() {
	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()
	if a.hook != nil {
		a.hook(n)
	}
}

func probeFixtures() (getenv func(string) string, nonce func() (string, error), newShell func(task string) *fakeServer) {
	getenv = func(k string) string {
		return map[string]string{"TMUX": "/tmp/shell.sock,1,0", "TMUX_PANE": "%1"}[k]
	}
	nonce = func() (string, error) { return probeNoncePrefix + "n1", nil }
	newShell = func(task string) *fakeServer {
		s := paneSession("work", "%1")
		s.Task = task
		return &fakeServer{sessions: []term.Session{s}}
	}
	return
}

func TestProbeCleansUpAMarkThatWasAppliedBeforeAnError(t *testing.T) {
	getenv, nonce, newShell := probeFixtures()
	shell := newShell("my task")
	host := &markFailServer{fakeServer: shell}
	err := confirmTmuxServer(getenv, func(string) (sessionLister, error) { return host, nil }, nonce, noLock)(context.Background(), "work", workerOn(shell, "work"))
	if err == nil {
		t.Fatal("a failed mark was accepted")
	}
	if got := shell.task("work"); got != "my task" {
		t.Errorf("task after a mark that applied and failed = %q", got)
	}
	if got := shell.probe("work"); got != "" {
		t.Errorf("probe left behind: %q", got)
	}
}

// TestProbeLeavesEveryTaskUntouched: whatever the task holds (unset, larger
// than SetOption accepts, not UTF-8, a NUL, text that looks like a probe value,
// even this very nonce), the check is accepted on the same server, the task is
// byte for byte what it was, and the task option is never written.
func TestProbeLeavesEveryTaskUntouched(t *testing.T) {
	getenv, nonce, newShell := probeFixtures()
	n, _ := nonce()
	for name, task := range map[string]string{
		"unset":                 "",
		"large":                 strings.Repeat("x", 100000),
		"not utf-8":             "bad \xff\xfe text",
		"holds a NUL":           "a\x00b",
		"probe-like prefix":     "minion-probe-debugging",
		"bare prefix":           "minion-probe-",
		"equals the nonce":      n,
		"ordinary":              "fix the build",
		"looks like a leftover": "minion-probe-deadbeef",
	} {
		shell := newShell(task)
		open := func(string) (sessionLister, error) { return shell, nil }
		if err := confirmTmuxServer(getenv, open, nonce, noLock)(context.Background(), "work", workerOn(shell, "work")); err != nil {
			t.Errorf("%s: same server refused: %v", name, err)
		}
		if got := shell.task("work"); got != task {
			t.Errorf("%s: task changed to %q", name, got)
		}
		for _, w := range shell.sets {
			if !strings.HasPrefix(w, "@minion_probe=") {
				t.Errorf("%s: wrote %q, only the probe option may be written", name, w)
			}
		}
		if got := shell.probe("work"); got != "" {
			t.Errorf("%s: probe left behind: %q", name, got)
		}
	}
}

// TestProbeNeverOverwritesAConcurrentSetTask lands a set_task after every
// single tmux call of the probe (reads, the mark, the cleanup, the Worker's
// answers included), so that every interleaving of the check is covered: the
// writer's text always survives.
func TestProbeNeverOverwritesAConcurrentSetTask(t *testing.T) {
	getenv, nonce, newShell := probeFixtures()
	const rounds = 10 // more than the probe has tmux calls; the extra rounds fire nowhere
	fired := 0
	for k := 1; k <= rounds; k++ {
		shell := newShell("old task")
		host := &afterListServer{fakeServer: shell}
		write := fmt.Sprintf("minion-probe-set-after-call-%d", k) // also a probe-like text
		var wFired atomic.Bool
		hook := func(n int) {
			if n == k && !wFired.Swap(true) {
				shell.setTask("work", write)
			}
		}
		host.hook = hook
		// The Worker answers from the same server; its list is read through
		// the same counter, so the hook also fires between the two
		// sessions_list calls.
		worker := &fakeOpsBackend{call: func(op string, args json.RawMessage) (json.RawMessage, error) {
			sessions, _ := host.ListSessions(context.Background())
			var res ops.SessionsListResult
			for _, s := range sessions {
				res.Sessions = append(res.Sessions, ops.SessionInfo{Name: s.Name, Task: s.Task, Probe: s.Probe})
			}
			return json.Marshal(res)
		}}
		if err := confirmTmuxServer(getenv, func(string) (sessionLister, error) { return host, nil }, nonce, noLock)(context.Background(), "work", worker); err != nil {
			t.Fatalf("call %d: same server refused: %v", k, err)
		}
		want := "old task"
		if wFired.Load() {
			want = write
			fired++
		}
		if got := shell.task("work"); got != want {
			t.Errorf("a set_task after call %d: task = %q, want %q", k, got, want)
		}
		for _, w := range shell.sets {
			if !strings.HasPrefix(w, "@minion_probe=") {
				t.Errorf("call %d: wrote %q", k, w)
			}
		}
	}
	if fired < 6 {
		t.Fatalf("only %d interleavings were exercised", fired)
	}
}

// TestProbeRefusesAWorkerThatDoesNotEchoTheProbe: a Worker that predates the
// probe field lists the session but never reports a probe. The implicit target
// is refused (fail closed) with the explicit way out, and nothing stays behind.
func TestProbeRefusesAWorkerThatDoesNotEchoTheProbe(t *testing.T) {
	getenv, nonce, newShell := probeFixtures()
	shell := newShell("my task")
	legacy := &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		sessions, _ := shell.ListSessions(context.Background())
		var res ops.SessionsListResult
		for _, s := range sessions {
			res.Sessions = append(res.Sessions, ops.SessionInfo{Name: s.Name, Task: s.Task}) // no probe
		}
		return json.Marshal(res)
	}}
	err := confirmTmuxServer(getenv, func(string) (sessionLister, error) { return shell, nil }, nonce, noLock)(context.Background(), "work", legacy)
	if err == nil || !strings.Contains(err.Error(), "--target") || !strings.Contains(err.Error(), "Worker") {
		t.Fatalf("legacy Worker: err = %v, want a refusal that names --target", err)
	}
	if shell.task("work") != "my task" || shell.probe("work") != "" {
		t.Errorf("task %q probe %q", shell.task("work"), shell.probe("work"))
	}
}

// TestProbeIgnoresAStaleProbeValue: a probe a crashed run left behind is plain
// metadata; it neither blocks nor satisfies the check, and it is replaced.
func TestProbeIgnoresAStaleProbeValue(t *testing.T) {
	getenv, nonce, newShell := probeFixtures()
	shell := newShell("my task")
	shell.sessions[0].Probe = probeNoncePrefix + "stale"
	// Another server (the Worker's) that holds a session of the same name and
	// the very same stale value must not be accepted.
	other := newShell("")
	other.sessions[0].Probe = probeNoncePrefix + "stale"
	open := func(string) (sessionLister, error) { return shell, nil }
	if err := confirmTmuxServer(getenv, open, nonce, noLock)(context.Background(), "work", workerOn(other, "work")); err == nil {
		t.Fatal("a stale probe value was accepted as proof")
	}
	if err := confirmTmuxServer(getenv, open, nonce, noLock)(context.Background(), "work", workerOn(shell, "work")); err != nil {
		t.Fatalf("same server refused: %v", err)
	}
	if shell.task("work") != "my task" || shell.probe("work") != "" {
		t.Errorf("task %q probe %q", shell.task("work"), shell.probe("work"))
	}
}
