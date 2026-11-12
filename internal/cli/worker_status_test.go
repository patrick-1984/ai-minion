package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// fakeLister reports a fixed number of sessions.
type fakeLister struct{ n int }

func (f fakeLister) ListSessions(context.Context) ([]term.Session, error) {
	return make([]term.Session, f.n), nil
}

// fakeLive is a running Worker that answers with a fixed status.
type fakeLive struct {
	ls  agent.LiveStatus
	err error
}

func (f fakeLive) LiveStatus(context.Context) (agent.LiveStatus, error) { return f.ls, f.err }

func mapEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func testStatusDeps(env map[string]string) workerStatusDeps {
	return workerStatusDeps{
		lookupEnv:  mapEnv(env),
		loadConfig: loadWorkerConfig,
		version:    func() string { return "v9.9.9" },
		live:       func(string) agent.LiveSource { return fakeLive{err: agent.ErrNoLiveWorker} },
		sessions:   func(string) agent.StatusSessions { return fakeLister{n: 2} },
	}
}

// runWorkerCmds runs args on a root that holds "worker status" and "worker
// config" built on the given deps, and returns stdout, stderr and the error.
func runWorkerCmds(t *testing.T, sd workerStatusDeps, cd workerConfigDeps, args ...string) (string, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	w := workerCommand(root)
	w.AddCommand(newWorkerStatusCmd(sd), newWorkerConfigCmd(cd))
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

// enrollWorker stores a pairing, a key and a share choice in dir.
func enrollWorker(t *testing.T, dir string) (hubFpr, workerFpr string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	hub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(ctx, agent.Enrollment{
		Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w1", Owner: "alice", Name: "lab",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveShareContext(ctx, store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"build"}}); err != nil {
		t.Fatal(err)
	}
	return hub.Fingerprint().String(), key.Fingerprint().String()
}

func TestWorkerStatusPrintsEveryFactOfAPairedWorker(t *testing.T) {
	dir := t.TempDir()
	hubFpr, workerFpr := enrollWorker(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"server":"https://hub.example:8443","name":"lab-pi","preview_only":"true"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sd := testStatusDeps(nil)
	sd.live = func(string) agent.LiveSource {
		return fakeLive{ls: agent.LiveStatus{
			Connector:          agent.Status{State: agent.ConnBackoff, LastError: "dial: refused"},
			PendingPairingCode: "MN-ABCD-EFGH",
		}}
	}
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--state-dir", dir, "--tmux-socket", "/tmp/x/tmux-1/default")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"v9.9.9", "lab-pi", "Hub\n", "https://hub.example:8443", "backoff", "dial: refused",
		hubFpr, workerFpr, "paired to alice", "MN-ABCD-EFGH", "select (build)", "yes", "/tmp/x/tmux-1/default", "2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

// Without an answer from a Worker, status does not tell the owner to start
// one (it may run) and does not call an unknown last error "none".
func TestWorkerStatusClaimsNothingAboutAWorkerItDidNotReach(t *testing.T) {
	dir := t.TempDir()
	sd := testStatusDeps(map[string]string{"MINION_SERVER": "https://hub.example"})
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "known only to the running Worker and is not available here") || !strings.Contains(out, "Last error:") {
		t.Fatalf("status = %s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Last error:") && !strings.HasSuffix(line, "unknown") {
			t.Errorf("line = %q", line)
		}
	}
	if strings.Contains(out, "start it") || strings.Contains(out, "minion worker run") {
		t.Fatalf("status tells the owner to start a Worker: %s", out)
	}
}

func TestWorkerStatusJSONHasTheSameFacts(t *testing.T) {
	dir := t.TempDir()
	hubFpr, workerFpr := enrollWorker(t, dir)
	sd := testStatusDeps(map[string]string{"MINION_SERVER": "https://hub.example"})
	out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	var r agent.StatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if r.Version != "v9.9.9" || r.Mode != agent.ModeHub || r.HubURL != "https://hub.example" ||
		r.HubFingerprint != hubFpr || r.WorkerFingerprint != workerFpr || !r.Paired || r.Owner != "alice" ||
		r.Share.Mode != "select" || r.Sessions == nil || *r.Sessions != 2 || r.Connection.State != agent.ConnUnknown {
		t.Fatalf("report = %+v", r)
	}
}

func TestWorkerStatusOfANeverStartedWorkerCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	out, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Solo") || !strings.Contains(out, "not paired") || !strings.Contains(out, "later (not decided yet)") {
		t.Fatalf("status = %s", out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created the state directory: %v", err)
	}
}

func TestWorkerStatusReportsAnUnreadableStateAsAProblem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.DBFile), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Problem:") || !strings.Contains(out, "cannot open the Worker state") {
		t.Fatalf("status = %s", out)
	}
}

func TestWorkerStatusRejectsUnknownFlagsAndArguments(t *testing.T) {
	if _, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "extra"); err == nil {
		t.Fatal("status accepted a positional argument")
	}
	if _, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "--share-existing", "all"); err == nil {
		t.Fatal("status accepted a flag it ignores")
	}
}

func TestWorkerStatusIsRegisteredOnTheRoot(t *testing.T) {
	root := NewRoot()
	for _, path := range [][]string{{"worker", "status"}, {"worker", "config", "set"}, {"worker", "config", "share"}} {
		if c, _, err := root.Find(path); err != nil || c.Name() != path[len(path)-1] {
			t.Fatalf("command %v not found: %v", path, err)
		}
	}
}

func TestStatusSessionsOfAnUnknownSocketCannotBeCounted(t *testing.T) {
	if tmuxSessions("") != nil {
		t.Fatal("an empty socket must mean cannot count")
	}
}

// Pairing data that no running Worker supplied is "unknown", never "none
// pending", with or without a running Worker.
func TestWorkerStatusSaysUnknownForPairingDataItDoesNotHave(t *testing.T) {
	pairingLine := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Pairing code:") {
				return l
			}
		}
		t.Fatalf("no pairing line in %s", out)
		return ""
	}
	cases := []struct {
		name  string
		live  fakeLive
		want  string
		state string
	}{
		{"no Worker answered", fakeLive{err: agent.ErrNoLiveWorker}, "unknown", agent.PairingUnknown},
		{"Worker without a pairing source", fakeLive{ls: agent.LiveStatus{Connector: agent.Status{State: agent.ConnConnected}}}, "unknown", agent.PairingUnknown},
		{"Worker with nothing pending", fakeLive{ls: agent.LiveStatus{PairingKnown: true}}, "none pending", agent.PairingNone},
		{"Worker with a pending code", fakeLive{ls: agent.LiveStatus{PendingPairingCode: "MN-AAAA-BBBB"}}, "MN-AAAA-BBBB", agent.PairingPending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := testStatusDeps(nil)
			sd.live = func(string) agent.LiveSource { return c.live }
			dir := t.TempDir()
			out, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--state-dir", dir)
			if err != nil {
				t.Fatal(err)
			}
			if line := pairingLine(out); !strings.Contains(line, c.want) || (c.want == "unknown" && strings.Contains(line, "none")) {
				t.Errorf("line = %q, want %q", line, c.want)
			}
			js, _, err := runWorkerCmds(t, sd, workerConfigDeps{}, "worker", "status", "--json", "--state-dir", dir)
			if err != nil {
				t.Fatal(err)
			}
			var r agent.StatusReport
			if err := json.Unmarshal([]byte(js), &r); err != nil || r.PairingState != c.state {
				t.Errorf("pairing_state = %q (%v), want %q", r.PairingState, err, c.state)
			}
		})
	}
}

// fakeBackend is the local socket client with a scripted answer.
type fakeBackend struct {
	raw json.RawMessage
	err error
	op  string
}

func (f *fakeBackend) Call(_ context.Context, op string, _ any) (json.RawMessage, error) {
	f.op = op
	return f.raw, f.err
}

func (f *fakeBackend) Stream(ctx context.Context, op string, args any, _ func(json.RawMessage) error) (json.RawMessage, error) {
	return f.Call(ctx, op, args)
}

func TestLocalLiveMapsTheAnswersOfTheLocalSocket(t *testing.T) {
	dir := t.TempDir()
	ask := func(b *fakeBackend, stateDir string) (agent.LiveStatus, error) {
		return localLive{stateDir: stateDir, backend: b}.LiveStatus(context.Background())
	}

	b := &fakeBackend{raw: json.RawMessage(`{"state_dir":"` + filepath.ToSlash(dir) + `","state":"connected","owner":"alice","pairing_known":true}`)}
	ls, err := ask(b, dir)
	if err != nil || b.op != agent.OpLocalStatus || ls.Connector.State != agent.ConnConnected || ls.Connector.Welcome.Owner != "alice" || !ls.PairingKnown {
		t.Fatalf("answer = %+v, %v (op %q)", ls, err, b.op)
	}
	if _, err := ask(&fakeBackend{err: client.ErrWorkerNotRunning}, dir); !errors.Is(err, agent.ErrNoLiveWorker) {
		t.Fatalf("not running = %v", err)
	}
	if _, err := ask(&fakeBackend{err: client.ErrPermissionDenied}, dir); err == nil || errors.Is(err, agent.ErrNoLiveWorker) {
		t.Fatalf("an untrusted socket must be an error, got %v", err)
	}
	if _, err := ask(&fakeBackend{raw: json.RawMessage(`not json`)}, dir); err == nil || errors.Is(err, agent.ErrNoLiveWorker) {
		t.Fatalf("a broken answer must be an error, got %v", err)
	}
	// A Worker that uses another state directory is not this Worker.
	other := t.TempDir()
	if _, err := ask(b, other); !errors.Is(err, agent.ErrNoLiveWorker) {
		t.Fatalf("another state directory = %v", err)
	}
}

// Status may observe a database before the Worker's migrations have begun.
// It must leave its schema and journal mode alone, even when no Worker answers.
func TestWorkerStatusDoesNotInitializeDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, store.DBFile)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE sentinel (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runWorkerCmds(t, testStatusDeps(nil), workerConfigDeps{}, "worker", "status", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name != 'sentinel'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("status initialized the database (%d new schema objects): %s", n, out)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "delete" {
		t.Errorf("status changed journal mode to %q", mode)
	}
}
