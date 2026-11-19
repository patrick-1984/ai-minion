package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// fakeInviteDialer enrolls every call at the Hub of testHubFingerprint().
type fakeInviteDialer struct {
	mu    sync.Mutex
	calls int
}

func (d *fakeInviteDialer) Enroll(_ context.Context, _ agent.InviteParams) (*agent.InviteOutcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return &agent.InviteOutcome{Pin: identity.Pin{Pub: bytes.Repeat([]byte{9}, 32)}, Welcome: agent.HubWelcome{
		Type: proto.TypeWelcome, WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private",
	}}, nil
}

func (d *fakeInviteDialer) n() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// fakeLiveWorker is the local API of a running Worker: status answers with
// the instance, a restart starts a new one. lossy sessions end on a restart
// unless the caller accepts that many.
type fakeLiveWorker struct {
	mu       sync.Mutex
	state    string
	running  bool
	instance int
	lossy    int
	restarts int
}

func (w *fakeLiveWorker) call(_ context.Context, op string, args any) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return nil, client.ErrWorkerNotRunning
	}
	switch op {
	case agent.OpLocalStatus:
		return json.Marshal(agent.LiveStatusReply{StateDir: w.state, Instance: "i" + string(rune('0'+w.instance))})
	case "worker_lifecycle+lifecycle.v1":
		a := args.(agent.LocalLifecycleArgs)
		if w.lossy > 0 && a.AcceptLoss < w.lossy {
			det, _ := json.Marshal(map[string]any{"reason": agent.ReasonSessionsWouldEnd, "sessions_lost": w.lossy})
			pe := proto.NewError(proto.CodeConflict, "sessions would end; nothing was done")
			pe.Details = det
			return nil, pe
		}
		w.instance++
		w.restarts++
		return json.Marshal(agent.LifecycleResult{Action: "restart", SessionsLost: w.lossy, Exiting: true})
	}
	return nil, errors.New("unexpected op " + op)
}

type joinCLI struct {
	t      *testing.T
	state  string
	env    map[string]string
	deps   workerPairDeps
	invite *fakeInviteDialer
	worker *fakeLiveWorker
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newJoinCLI(t *testing.T) *joinCLI {
	t.Helper()
	state := t.TempDir()
	c := &joinCLI{t: t, state: state, env: map[string]string{"MINION_STATE_DIR": state},
		invite: &fakeInviteDialer{}, worker: &fakeLiveWorker{state: state, running: true, instance: 1},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	c.deps = workerPairDeps{
		stdin:        strings.NewReader(""),
		stdout:       c.stdout,
		stderr:       c.stderr,
		lookupEnv:    func(k string) (string, bool) { v, ok := c.env[k]; return v, ok },
		loadConfig:   loadWorkerConfig,
		saveServer:   saveWorkerHub,
		inviteDialer: c.invite,
		call:         c.worker.call,
		restartSleep: func(time.Duration) {},
		restartWait:  2 * time.Second,
	}
	return c
}

// run executes "minion worker join <args> --json" with stdin and returns the
// result object and the exit status.
func (c *joinCLI) run(stdin string, args ...string) (map[string]any, int) {
	c.t.Helper()
	c.stdout.Reset()
	c.stderr.Reset()
	c.deps.stdin = strings.NewReader(stdin)
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerJoinCmd(c.deps))
	var errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "join", "--json"}, args...), &bytes.Buffer{}, &errOut)
	var m map[string]any
	if err := json.Unmarshal(c.stdout.Bytes(), &m); err != nil {
		c.t.Fatalf("stdout is not one JSON object: %q (%v)\nstderr: %s", c.stdout.String(), err, c.stderr.String())
	}
	return m, code
}

func (c *joinCLI) config() map[string]string {
	c.t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.state, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}
	}
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *joinCLI) share() store.ShareSelection {
	c.t.Helper()
	st, err := store.OpenReadOnlyContext(context.Background(), c.state)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sel, err := st.LoadShareContext(context.Background())
	if err != nil {
		c.t.Fatal(err)
	}
	return sel
}

func stages(m map[string]any) [3]any { return [3]any{m["enrollment"], m["config"], m["activation"]} }

func TestJoinStagesAllDone(t *testing.T) {
	c := newJoinCLI(t)
	text, tok := testJoinToken(t, "https://hub.example:8443")
	m, code := c.run(text+"\n", "--token-file", "-", "--share-existing", "later")
	if code != ExitOK || m["ok"] != true || stages(m) != [3]any{"committed", "saved", "restarted"} {
		t.Fatalf("join = %v, %d\nstderr: %s", m, code, c.stderr.String())
	}
	if c.invite.n() != 1 || c.worker.restarts != 1 || m["instance"] != "i2" {
		t.Fatalf("invites %d, restarts %d, result %v", c.invite.n(), c.worker.restarts, m)
	}
	cfg := c.config()
	if cfg[config.KeyServer] != tok.HubURL || cfg[config.KeyHubFingerprint] != testHubFingerprint() {
		t.Fatalf("config = %v", cfg)
	}
	if sel := c.share(); !sel.Configured || sel.Mode != store.ShareLater {
		t.Fatalf("share choice = %+v", sel)
	}
	secret := tok.Invite.String()[len(tok.Invite.String())-52:]
	raw, _ := os.ReadFile(filepath.Join(c.state, "config.json"))
	for name, s := range map[string]string{"stdout": c.stdout.String(), "stderr": c.stderr.String(), "config": string(raw)} {
		if strings.Contains(s, text) || strings.Contains(s, secret) {
			t.Errorf("%s holds the secret: %s", name, s)
		}
	}
	// From a file too; the Worker is enrolled now, so this is "already".
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	m, code = c.run("", "--token-file", path)
	if code != ExitOK || stages(m) != [3]any{"already", "saved", "restarted"} || c.invite.n() != 1 {
		t.Fatalf("second join = %v, %d (invites %d)", m, code, c.invite.n())
	}
}

func TestJoinConfigSaveFails(t *testing.T) {
	c := newJoinCLI(t)
	text, tok := testJoinToken(t, "https://hub.example:8443")
	c.deps.saveServer = func(string, string, string) error { return errors.New("disk full") }
	m, code := c.run(text, "--token-file", "-", "--share-existing", "none")
	if code != ExitError || stages(m) != [3]any{"committed", "failed", nil} || !strings.Contains(m["message"].(string), "--resume") {
		t.Fatalf("join = %v, %d", m, code)
	}
	if c.worker.restarts != 0 {
		t.Fatal("restarted although the config was not saved")
	}
	// Resume: no token on stdin, no invitation sent.
	c.deps.saveServer = saveWorkerHub
	m, code = c.run("", "--resume")
	if code != ExitOK || stages(m) != [3]any{"already", "saved", "restarted"} {
		t.Fatalf("resume = %v, %d\nstderr: %s", m, code, c.stderr.String())
	}
	if c.invite.n() != 1 {
		t.Fatalf("resume dialed the Hub: %d invite calls in all", c.invite.n())
	}
	if cfg := c.config(); cfg[config.KeyServer] != tok.HubURL || cfg[config.KeyHubFingerprint] != testHubFingerprint() {
		t.Fatalf("config = %v", cfg)
	}
}

func TestJoinRestartExit23(t *testing.T) {
	c := newJoinCLI(t)
	c.worker.lossy = 2
	text, _ := testJoinToken(t, "https://hub.example:8443")
	m, code := c.run(text, "--token-file", "-", "--share-existing", "all")
	if code != ExitSessionsWouldEnd || stages(m) != [3]any{"committed", "saved", "failed"} || m["sessions_lost"] != float64(2) {
		t.Fatalf("join = %v, %d", m, code)
	}
	m, code = c.run("", "--resume", "--accept-loss", "2")
	if code != ExitOK || stages(m) != [3]any{"already", "saved", "restarted"} || c.invite.n() != 1 {
		t.Fatalf("resume = %v, %d (invites %d)", m, code, c.invite.n())
	}
}

func TestResumeRefusesOtherHub(t *testing.T) {
	for _, other := range []struct{ key, value string }{
		{config.KeyServer, "https://other.example:8443"},
		{config.KeyHubFingerprint, func() string { f, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32)); return string(f) }()},
	} {
		c := newJoinCLI(t)
		text, _ := testJoinToken(t, "https://hub.example:8443")
		if _, code := c.run(text, "--token-file", "-", "--share-existing", "all"); code != ExitOK {
			t.Fatalf("join: %d %s", code, c.stderr.String())
		}
		if err := saveWorkerHub(c.state, "https://hub.example:8443", ""); err != nil {
			t.Fatal(err)
		}
		f, err := config.LoadFile(filepath.Join(c.state, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		f.Set(other.key, other.value)
		if err := f.Save(); err != nil {
			t.Fatal(err)
		}
		restarts := c.worker.restarts
		m, code := c.run("", "--resume")
		if code != ExitStateChanged || m["code"] != "state_changed" || c.worker.restarts != restarts || c.invite.n() != 1 {
			t.Fatalf("%s: resume = %v, %d", other.key, m, code)
		}
		if got := c.config()[other.key]; got != other.value {
			t.Fatalf("%s: the config was changed to %q", other.key, got)
		}
	}
}

func TestAlreadyEnrolledIsNotSuccessWhenUnfinished(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	// Enrolled, but no Worker runs: activation not_running, exit 24.
	c.worker.running = false
	m, code := c.run(text, "--token-file", "-", "--share-existing", "all")
	if code != ExitNotRunning || m["ok"] != false || stages(m) != [3]any{"committed", "saved", "not_running"} {
		t.Fatalf("join = %v, %d", m, code)
	}
	// Again with the token: already enrolled is still not a success.
	m, code = c.run(text, "--token-file", "-")
	if code != ExitNotRunning || m["ok"] != false || stages(m) != [3]any{"already", "saved", "not_running"} || c.invite.n() != 1 {
		t.Fatalf("again = %v, %d", m, code)
	}
	// And with a config that cannot be saved: 1.
	c.deps.saveServer = func(string, string, string) error { return errors.New("read-only") }
	m, code = c.run(text, "--token-file", "-")
	if code != ExitError || m["ok"] != false || stages(m) != [3]any{"already", "failed", nil} {
		t.Fatalf("config fails = %v, %d", m, code)
	}
	// A token of another Hub than the enrollment: 27, nothing sent.
	c.deps.saveServer = saveWorkerHub
	k, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32))
	inv, _ := identity.NewInviteKey(k)
	otherTok, _ := identity.NewJoinToken("https://hub.example:8443", inv)
	if err := os.Remove(filepath.Join(c.state, "config.json")); err != nil {
		t.Fatal(err)
	}
	m, code = c.run(otherTok.Encode(), "--token-file", "-")
	if code != ExitStateChanged || stages(m) != [3]any{"failed", nil, nil} || c.invite.n() != 1 {
		t.Fatalf("other Hub = %v, %d", m, code)
	}
}

func TestJoinFirstRunWithoutShareChoiceIsUsage(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	m, code := c.run(text, "--token-file", "-")
	if code != ExitUsage || m["code"] != "usage" || !strings.Contains(m["message"].(string), "--share-existing") {
		t.Fatalf("join = %v, %d", m, code)
	}
	if c.invite.n() != 0 || len(c.config()) != 0 {
		t.Fatalf("something was done: invites %d, config %v", c.invite.n(), c.config())
	}
	if _, err := os.Stat(filepath.Join(c.state, store.DBFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state was created: %v", err)
	}
}

func TestJoinTokenWhilePausedIs25(t *testing.T) {
	c := newJoinCLI(t)
	if _, err := agent.SetPause(c.state, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	text, _ := testJoinToken(t, "https://hub.example:8443")
	for _, args := range [][]string{{"--token-file", "-", "--share-existing", "all"}, {"--resume"}} {
		m, code := c.run(text, args...)
		if code != ExitPaused || m["code"] != "paused" || c.invite.n() != 0 || c.worker.restarts != 0 {
			t.Fatalf("%v while paused = %v, %d", args, m, code)
		}
	}
}

// The check against a different saved server or fingerprint stays: nothing
// is sent and nothing is saved.
func TestJoinTokenConflictWithSavedHubKept(t *testing.T) {
	for _, saved := range []struct{ key, value string }{
		{config.KeyServer, "https://other.example:8443"},
		{config.KeyHubFingerprint, func() string { f, _ := identity.FingerprintOf(bytes.Repeat([]byte{7}, 32)); return string(f) }()},
	} {
		c := newJoinCLI(t)
		f, err := config.LoadFile(filepath.Join(c.state, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		f.Set(saved.key, saved.value)
		if err := f.Save(); err != nil {
			t.Fatal(err)
		}
		text, _ := testJoinToken(t, "https://hub.example:8443")
		m, code := c.run(text, "--token-file", "-", "--share-existing", "all")
		if code != ExitError || !strings.Contains(m["message"].(string), "nothing was sent") || c.invite.n() != 0 {
			t.Fatalf("%s: join = %v, %d", saved.key, m, code)
		}
		if got := c.config(); got[saved.key] != saved.value || len(got) != 1 {
			t.Fatalf("%s: config = %v", saved.key, got)
		}
		// A flag that names another Hub is refused the same way.
		c2 := newJoinCLI(t)
		m, code = c2.run(text, "--token-file", "-", "--share-existing", "all", "--server", "https://other.example:8443")
		if code != ExitError || !strings.Contains(m["message"].(string), "give only one of them") || c2.invite.n() != 0 {
			t.Fatalf("flag: join = %v, %d", m, code)
		}
	}
}

func TestJoinTokenFormsAndArgs(t *testing.T) {
	c := newJoinCLI(t)
	text, _ := testJoinToken(t, "https://hub.example:8443")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{text}, ExitUsage},                              // the token as an argument
		{[]string{"--token-file", text}, ExitUsage},              // the token in place of a path
		{[]string{"--token-file", "-", "--resume"}, ExitUsage},   // both
		{[]string{"https://hub.example", "--resume"}, ExitUsage}, // a URL with the token flow
		{[]string{}, ExitUsage},                                  // neither
		{[]string{"--token-file", filepath.Join(c.state, "missing"), "--share-existing", "all"}, ExitError},
	} {
		m, code := c.run(text, tc.args...)
		if code != tc.code {
			t.Errorf("%d args: exit %d, want %d (%v)", len(tc.args), code, tc.code, m)
		}
		if strings.Contains(c.stdout.String()+c.stderr.String(), text) {
			t.Errorf("%d args: the token was echoed", len(tc.args))
		}
	}
	if c.invite.n() != 0 {
		t.Fatal("a refused join dialed the Hub")
	}
}
