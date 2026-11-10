package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/setup"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ---- fakes for the wizard ----

type listerOf []string

func (l listerOf) ListSessions(context.Context) ([]term.Session, error) {
	var out []term.Session
	for _, n := range l {
		out = append(out, term.Session{Name: n})
	}
	return out, nil
}

// okChecker passes every address; it records what it was asked.
type okChecker struct {
	mu    sync.Mutex
	asked []string
}

func (c *okChecker) Check(_ context.Context, hubURL string) setup.Report {
	c.mu.Lock()
	c.asked = append(c.asked, hubURL)
	c.mu.Unlock()
	st := func(n string) setup.Stage {
		return setup.Stage{Name: n, Status: setup.StatusOK, Message: n + " is fine."}
	}
	return setup.Report{URL: hubURL, Stages: []setup.Stage{st("DNS"), st("TCP"), st("TLS"), st("Protocol")}}
}

func (c *okChecker) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// readFailer fails the test when the command reads its input.
type readFailer struct{ t *testing.T }

func (r readFailer) Read([]byte) (int, error) {
	r.t.Error("the command read its input, but no question had to be asked")
	return 0, io.EOF
}

// syncBuf is a writer that the command and the test use at once.
type syncBuf struct {
	mu      sync.Mutex
	b       bytes.Buffer
	changed chan struct{} // closed and replaced on every write
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
	return n, err
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// keys plays the user at a terminal: lines are written to the command's input.
type keys struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newKeys(t *testing.T) *keys {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	return &keys{r: r, w: w}
}

func (k *keys) typeLines(lines ...string) {
	go func() {
		for _, l := range lines {
			if _, err := io.WriteString(k.w, l+"\n"); err != nil {
				return
			}
		}
	}()
}

func (k *keys) enter(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.WriteString(k.w, "\n") }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nobody read the Enter key")
	}
}

// setupRig is "minion worker setup" with every outside effect replaced except
// the Worker state, which is real (a store and a config file in a temp dir).
type setupRig struct {
	t      *testing.T
	state  string
	env    map[string]string
	deps   workerSetupDeps
	pair   *fakePairDialer
	check  *okChecker
	stdout *syncBuf
}

func newSetupRig(t *testing.T, tty bool, sessions ...string) *setupRig {
	t.Helper()
	r := &setupRig{t: t, state: t.TempDir(), pair: &fakePairDialer{result: okPairResult("alice")}, check: &okChecker{}, stdout: &syncBuf{}}
	r.env = map[string]string{"MINION_STATE_DIR": r.state, "MINION_TMUX_SOCKET": filepath.Join(r.state, "no-tmux.sock")}
	r.deps = workerSetupDeps{
		lookupEnv:   func(k string) (string, bool) { v, ok := r.env[k]; return v, ok },
		loadConfig:  loadWorkerConfig,
		interactive: func(io.Reader, io.Writer) bool { return tty },
		sessions:    func(string) setup.SessionLister { return listerOf(sessions) },
		checker:     func(config.WorkerHub) (setup.HubChecker, error) { return r.check, nil },
		dialer:      r.pair,
		backoff:     &transport.Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond},
		newCode: func() identity.PairingCode {
			pc, _ := identity.ParsePairingCode("K7Q4XY12")
			return pc
		},
	}
	return r
}

func (r *setupRig) run(ctx context.Context, in io.Reader, args ...string) error {
	r.t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerSetupCmd(r.deps))
	root.SetIn(in)
	root.SetOut(r.stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"worker", "setup"}, args...))
	return root.ExecuteContext(ctx)
}

func (r *setupRig) share() store.ShareSelection {
	r.t.Helper()
	st, err := store.OpenContext(context.Background(), r.state)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sel, err := st.LoadShareContext(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return sel
}

func (r *setupRig) enrollment() (agent.Enrollment, bool) {
	r.t.Helper()
	st, err := store.OpenContext(context.Background(), r.state)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	e, ok, err := agent.NewEnrollments(st, r.state).Load(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return e, ok
}

func (r *setupRig) file() map[string]string {
	r.t.Helper()
	return readWorkerUpConfig(r.t, filepath.Join(r.state, workerConfigName))
}

func (r *setupRig) dbExists() bool {
	_, err := os.Stat(filepath.Join(r.state, store.DBFile))
	return err == nil
}

// ---- worker setup ----

// The whole wizard on the real state: choose one session, give a Hub address,
// get paired. What is saved is read back from the database and the file.
func TestWorkerSetupInteractiveSavesChoicesAndPairs(t *testing.T) {
	r := newSetupRig(t, true, "build", "docs")
	k := newKeys(t)
	k.typeLines("2", "2", "", "https://hub.example:8443") // choose; tick docs; done; Hub
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.run(ctx, k.r); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	out := r.stdout.String()
	for _, want := range []string{"I see 2 tmux sessions:", "[x] 2  docs", "Pairing code: MN-K7Q4-XY12", "paired to alice", "Pairing: paired to alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	sel := r.share()
	if !sel.Configured || sel.Mode != store.ShareSelect || len(sel.Sessions) != 1 || sel.Sessions[0] != "docs" {
		t.Errorf("saved share = %+v, want select [docs]", sel)
	}
	if sel.Includes("build") || !sel.Includes("docs") {
		t.Errorf("share policy = %+v", sel)
	}
	if got := r.file()[config.KeyServer]; got != "https://hub.example:8443" {
		t.Errorf("saved server = %q", got)
	}
	if e, ok := r.enrollment(); !ok || e.Owner != "alice" {
		t.Errorf("enrollment = %+v %v", e, ok)
	}
	if r.pair.n() != 1 {
		t.Errorf("pairing calls = %d", r.pair.n())
	}
	if got := r.check.calls(); len(got) != 1 || got[0] != "https://hub.example:8443" {
		t.Errorf("checked = %v", got)
	}
}

// R85: with the answers given as flags, no question is asked and nothing is
// read, and without a terminal the wizard does not pair.
func TestWorkerSetupWithAllFlagsAsksNothingAndReadsNothing(t *testing.T) {
	r := newSetupRig(t, false, "build")
	if err := r.run(context.Background(), readFailer{t}, "--share-existing", "none", "--server", "https://hub.example:8443"); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	out := r.stdout.String()
	if strings.Contains(out, "Your choice [") || strings.Contains(out, "Hub address:") {
		t.Errorf("a question was printed:\n%s", out)
	}
	if !strings.Contains(out, "Not pairing now") {
		t.Errorf("output lacks the pairing note:\n%s", out)
	}
	if sel := r.share(); !sel.Configured || sel.Mode != store.ShareNone {
		t.Errorf("share = %+v", sel)
	}
	if got := r.file()[config.KeyServer]; got != "https://hub.example:8443" {
		t.Errorf("server = %q", got)
	}
	if r.pair.n() != 0 {
		t.Error("without a terminal the wizard must not start pairing")
	}
	if _, ok := r.enrollment(); ok {
		t.Error("no pairing may be recorded")
	}
}

// The same through the environment, with an explicit Solo from the flag.
func TestWorkerSetupWithEnvironmentAsksNothing(t *testing.T) {
	r := newSetupRig(t, false)
	r.env["MINION_SHARE_EXISTING"] = "later"
	if err := r.run(context.Background(), readFailer{t}); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	if sel := r.share(); !sel.Configured || sel.Mode != store.ShareLater || sel.Includes("anything") {
		t.Errorf("share = %+v", sel)
	}
	if _, ok := r.file()[config.KeyServer]; ok {
		t.Error("no Hub address was given: the Worker runs alone")
	}
	if !strings.Contains(r.stdout.String(), "runs alone (Solo)") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

func TestWorkerSetupExplicitSoloFlagRemovesASavedAddress(t *testing.T) {
	r := newSetupRig(t, false)
	if err := os.WriteFile(filepath.Join(r.state, workerConfigName), []byte(`{"server":"https://old.example:8443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.run(context.Background(), readFailer{t}, "--share-existing", "all", "--server", ""); err != nil {
		t.Fatalf("worker setup: %v\n%s", err, r.stdout)
	}
	if _, ok := r.file()[config.KeyServer]; ok {
		t.Errorf("the saved address must be removed by an explicit Solo: %v", r.file())
	}
}

// Without a terminal and without the sharing choice nothing is read, saved,
// created or started.
func TestWorkerSetupWithoutTerminalOrFlagsRefuses(t *testing.T) {
	r := newSetupRig(t, false, "build")
	err := r.run(context.Background(), readFailer{t})
	if err == nil {
		t.Fatalf("worker setup succeeded without a terminal and without flags:\n%s", r.stdout)
	}
	for _, want := range []string{"no terminal", "--share-existing", "minion worker setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if r.dbExists() {
		t.Error("a refused wizard must not create the Worker state")
	}
	if r.pair.n() != 0 {
		t.Error("pairing started")
	}
}

// An invalid value is an error before anything is saved.
func TestWorkerSetupRejectsAnInvalidAddress(t *testing.T) {
	r := newSetupRig(t, false)
	err := r.run(context.Background(), readFailer{t}, "--share-existing", "all", "--server", "ftp://hub")
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("err = %v", err)
	}
	if r.dbExists() {
		t.Error("state was created for an invalid address")
	}
}

// Ctrl-C while the settings are not saved yet: nothing was changed.
func TestWorkerSetupCancelBeforeSaveChangesNothing(t *testing.T) {
	r := newSetupRig(t, true, "build")
	k := newKeys(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, k.r) }()
	waitForOutput(t, r.stdout, "Your choice [")
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, setup.ErrCancelled) {
			t.Errorf("err = %v, want ErrCancelled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker setup did not end")
	}
	if sel := r.share(); sel.Configured {
		t.Errorf("a cancelled wizard saved %+v", sel)
	}
	if _, ok := r.file()[config.KeyServer]; ok {
		t.Error("a cancelled wizard saved a Hub address")
	}
}

// Enter during the pairing wait finishes setup with the settings saved and no
// pairing; the Worker can pair later.
func TestWorkerSetupEnterSkipsThePairingWait(t *testing.T) {
	r := newSetupRig(t, true, "build")
	r.pair.block = true
	k := newKeys(t)
	k.typeLines("1", "https://hub.example:8443")
	done := make(chan error, 1)
	go func() { done <- r.run(context.Background(), k.r) }()
	waitForOutput(t, r.stdout, "Pairing code: MN-K7Q4-XY12")
	k.enter(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker setup: %v\n%s", err, r.stdout)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker setup did not end")
	}
	if !strings.Contains(r.stdout.String(), "Pairing skipped") {
		t.Errorf("output:\n%s", r.stdout)
	}
	if _, ok := r.enrollment(); ok {
		t.Error("skipping the wait must not record a pairing")
	}
	if sel := r.share(); !sel.Configured || sel.Mode != store.ShareAll {
		t.Errorf("share = %+v", sel)
	}
}

// Ctrl-C during the pairing wait of "setup" ends it cleanly: the settings were
// saved before pairing started.
func TestWorkerSetupInterruptWhilePairingKeepsTheSettings(t *testing.T) {
	r := newSetupRig(t, true, "build")
	r.pair.block = true
	k := newKeys(t)
	k.typeLines("3", "https://hub.example:8443")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, k.r) }()
	waitForOutput(t, r.stdout, "Pairing code: MN-K7Q4-XY12")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an interrupted setup after the save must end cleanly: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker setup did not end")
	}
	if sel := r.share(); !sel.Configured || sel.Mode != store.ShareLater || sel.Includes("build") {
		t.Errorf("share = %+v, want later with nothing shared", sel)
	}
	if got := r.file()[config.KeyServer]; got != "https://hub.example:8443" {
		t.Errorf("server = %q", got)
	}
}

// waitForOutput blocks until the output contains want. It wakes on every
// write, so it neither polls nor sleeps; the deadline only ends a hung test.
func waitForOutput(t *testing.T, b *syncBuf, want string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		b.mu.Lock()
		if strings.Contains(b.b.String(), want) {
			b.mu.Unlock()
			return
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("the output never contained %q:\n%s", want, b)
		}
	}
}

// A share_existing in the config file is applied to the database at every
// start: the wizard keeps it in step, or the next start would undo the choice.
func TestWorkerSetupKeepsShareExistingInTheFileInStep(t *testing.T) {
	r := newSetupRig(t, false)
	if err := os.WriteFile(filepath.Join(r.state, workerConfigName), []byte(`{"share_existing":"all"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The flag wins for this run; the saved file must follow it.
	if err := r.run(context.Background(), readFailer{t}, "--share-existing", "none"); err != nil {
		t.Fatal(err)
	}
	if got := r.file()[config.KeyShareExisting]; got != "none" {
		t.Errorf("share_existing in the file = %q, want none", got)
	}
	if sel := r.share(); sel.Mode != store.ShareNone {
		t.Errorf("share = %+v", sel)
	}
}

// ---- the persister ----

// failingShareStore is the real store whose share write fails.
type failingShareStore struct {
	*store.Store
	err error
}

func (f failingShareStore) SaveShareContext(context.Context, store.ShareSelection) error {
	return f.err
}

func TestSetupPersisterPutsTheFileBackWhenTheDatabaseRefuses(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, workerConfigName)
	before := `{"server":"https://old.example:8443","share_existing":"all"}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenContext(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p := &setupPersister{stateDir: state, st: failingShareStore{Store: st, err: errors.New("database is locked")}}
	err = p.Save(context.Background(), setup.Settings{
		Share:  store.ShareSelection{Mode: store.ShareNone, Configured: true},
		Server: "https://new.example:8443",
	})
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("err = %v", err)
	}
	got := readWorkerUpConfig(t, path)
	if got[config.KeyServer] != "https://old.example:8443" || got[config.KeyShareExisting] != "all" {
		t.Errorf("the config file was not put back: %v", got)
	}
}

func TestSetupPersisterCurrentReadsTheSavedState(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, workerConfigName), []byte(`{"server":"https://hub.example:8443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenContext(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SaveShareContext(context.Background(), store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	cur, err := (&setupPersister{stateDir: state, st: st}).Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cur.Server != "https://hub.example:8443" || cur.Share.Mode != store.ShareSelect || !cur.Share.Configured {
		t.Errorf("current = %+v", cur)
	}
}

func TestIsTerminalStreamRecognisesOnlyTerminals(t *testing.T) {
	if isTerminalStream(strings.NewReader("")) {
		t.Error("a string reader is not a terminal")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	if isTerminalStream(pr) {
		t.Error("a pipe is not a terminal")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if isTerminalStream(null) {
		t.Error("the null device is not a terminal")
	}
}

// ---- "worker up": the first-run gate ----

// upGate is "worker up --foreground" (or the service path) without the shortcut
// that the other tests of "up" use: the first-run gate is real.
func foregroundGate(t *testing.T, tty bool, env map[string]string, sessions ...string) (workerUpDeps, *probe, *setupRig) {
	t.Helper()
	deps, p, _, rig := foregroundGateEvents(t, tty, env, sessions...)
	return deps, p, rig
}

// foregroundGateEvents also returns the log of the registered pre-start hook.
func foregroundGateEvents(t *testing.T, tty bool, env map[string]string, sessions ...string) (workerUpDeps, *probe, *[]string, *setupRig) {
	t.Helper()
	deps, p, events := foregroundFixture(t, env)
	rig := newSetupRig(t, tty, sessions...)
	rig.state = env["MINION_STATE_DIR"]
	if rig.state == "" {
		t.Fatal("the fixture has no state directory")
	}
	deps.setup = rig.deps
	deps.setup.lookupEnv = nil // the run dependencies carry the environment
	return deps, p, events, rig
}

func runUp(deps workerUpDeps, ctx context.Context, in io.Reader, args ...string) (stdout string, err error) {
	var out syncBuf
	err = runUpOut(deps, ctx, in, &out, args...)
	return out.String(), err
}

// runUpOut is runUp with an output that the test can watch while the command runs.
func runUpOut(deps workerUpDeps, ctx context.Context, in io.Reader, out *syncBuf, args ...string) error {
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerUpCmd(deps))
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"worker", "up"}, args...))
	return root.ExecuteContext(ctx)
}

// Unconfigured, no terminal, no flags: "up" exits with an actionable message
// and starts nothing: no hook, no module, no service command, no stdin read.
func TestWorkerUpUnconfiguredWithoutTerminalExitsWithAnActionableMessage(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	deps, p, events, rig := foregroundGateEvents(t, false, env)
	_, err := runUp(deps, context.Background(), readFailer{t}, "--foreground")
	if err == nil {
		t.Fatal("worker up started an unconfigured Worker without a terminal")
	}
	for _, want := range []string{"not set up yet", "no terminal", "--share-existing all|none|later", "MINION_SHARE_EXISTING", "minion worker setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if len(*events) != 0 {
		t.Errorf("hooks ran: %v", *events)
	}
	if got := p.log(); len(got) != 0 {
		t.Errorf("the Worker started: %v", got)
	}
	if rig.dbExists() {
		t.Error("the refusal created the Worker state")
	}
}

// Fully provided by flags or environment: no gate, no question, the Worker starts.
func TestWorkerUpWithTheShareChoiceGivenStartsWithoutAnyQuestion(t *testing.T) {
	for name, cfg := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"flag":        {map[string]string{}, []string{"--share-existing", "later"}},
		"environment": {map[string]string{"MINION_SHARE_EXISTING": "none"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.env["MINION_STATE_DIR"] = t.TempDir()
			deps, p, _ := foregroundGate(t, false, cfg.env)
			deps.setup.interactive = func(io.Reader, io.Writer) bool {
				t.Error("the TTY check ran, but nothing had to be asked")
				return false
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := runUp(deps, ctx, readFailer{t}, append([]string{"--foreground"}, cfg.args...)...)
				done <- err
			}()
			select {
			case <-p.ready:
			case err := <-done:
				t.Fatalf("worker up ended before the Worker started: %v", err)
			case <-time.After(30 * time.Second):
				t.Fatal("worker up did not start")
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("worker up: %v", err)
			}
		})
	}
}

// A choice saved before (by the wizard or by "worker config share") counts as set up.
func TestWorkerUpWithASavedShareChoiceStartsWithoutAQuestion(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	deps, p, rig := foregroundGate(t, false, env)
	st, err := store.OpenContext(context.Background(), rig.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveShareContext(context.Background(), store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := runUp(deps, ctx, readFailer{t}, "--foreground"); done <- err }()
	select {
	case <-p.ready:
	case err := <-done:
		t.Fatalf("worker up ended before the Worker started: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker up did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("worker up: %v", err)
	}
}

// Unconfigured on a terminal: the wizard runs first, its answers are saved,
// then the Worker starts with them.
func TestWorkerUpUnconfiguredOnATerminalRunsTheWizardThenStarts(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	deps, p, rig := foregroundGate(t, true, env, "build", "docs")
	k := newKeys(t)
	k.typeLines("2", "1", "", "") // choose; tick build; done; Hub: Solo
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := runUp(deps, ctx, k.r, "--foreground"); done <- result{out, err} }()
	select {
	case <-p.ready:
	case r := <-done:
		t.Fatalf("worker up ended before the Worker started: %v\n%s", r.err, r.out)
	case <-time.After(30 * time.Second):
		t.Fatal("worker up did not start")
	}
	sel := rig.share()
	if !sel.Configured || sel.Mode != store.ShareSelect || len(sel.Sessions) != 1 || sel.Sessions[0] != "build" {
		t.Errorf("saved share = %+v, want select [build]", sel)
	}
	cancel()
	r := <-done
	if r.err != nil {
		t.Errorf("worker up: %v", r.err)
	}
	if !strings.Contains(r.out, "Step 1 of 3") || !strings.Contains(r.out, "I see 2 tmux sessions:") {
		t.Errorf("output:\n%s", r.out)
	}
}

// Cancelling the wizard of "up" starts nothing.
func TestWorkerUpWizardCancelledStartsNothing(t *testing.T) {
	env := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	deps, p, rig := foregroundGate(t, true, env, "a")
	_, err := runUp(deps, context.Background(), strings.NewReader(""), "--foreground") // Ctrl-D at the first question
	if !errors.Is(err, setup.ErrCancelled) {
		t.Fatalf("err = %v", err)
	}
	if got := p.log(); len(got) != 0 {
		t.Errorf("the Worker started: %v", got)
	}
	if sel := rig.share(); sel.Configured {
		t.Errorf("a cancelled wizard saved %+v", sel)
	}
}

// ---- the service path ----

func serviceGate(t *testing.T, root, tty bool, sessions ...string) (*upFixture, *setupRig) {
	t.Helper()
	f := newUpFixture(t, root)
	rig := newSetupRig(t, tty, sessions...)
	rig.state = f.stateDir()
	f.deps.setup = rig.deps
	f.deps.setup.lookupEnv = nil // the run dependencies carry the environment
	f.deps.setup.assumeSetUp = false
	return f, rig
}

func (f *upFixture) runIn(in io.Reader, args ...string) (string, error) {
	f.t.Helper()
	return runUp(f.deps, context.Background(), in, args...)
}

func TestWorkerUpServiceUnconfiguredWithoutTerminalChangesNothing(t *testing.T) {
	f, rig := serviceGate(t, false, false)
	_, err := f.runIn(readFailer{t})
	if err == nil || !strings.Contains(err.Error(), "not set up yet") || !strings.Contains(err.Error(), "--share-existing") {
		t.Fatalf("err = %v", err)
	}
	if calls := f.runCalls(); len(calls) != 0 {
		t.Errorf("service commands ran: %v", calls)
	}
	if rig.dbExists() {
		t.Error("the refusal created the Worker state")
	}
}

// Root cannot ask about another user's tmux or save into their state: the
// message names the user and the way out, also on a terminal.
func TestWorkerUpServiceUnderSudoNeverAsksAndNamesTheUser(t *testing.T) {
	for _, tty := range []bool{false, true} {
		f, _ := serviceGate(t, true, tty, "build")
		_, err := f.runIn(readFailer{t})
		if err == nil || !strings.Contains(err.Error(), "runs as root for ann") || !strings.Contains(err.Error(), "--share-existing") {
			t.Fatalf("tty=%v: err = %v", tty, err)
		}
		if calls := f.runCalls(); len(calls) != 0 {
			t.Errorf("tty=%v: service commands ran: %v", tty, calls)
		}
	}
}

func TestWorkerUpServiceUnderSudoWithTheChoiceGivenInstalls(t *testing.T) {
	f, _ := serviceGate(t, true, false)
	if _, err := f.runIn(readFailer{t}, "--share-existing", "all"); err != nil {
		t.Fatalf("worker up: %v", err)
	}
	if len(f.runCalls()) == 0 {
		t.Error("the service was not installed")
	}
}

// Unconfigured on a terminal: the wizard runs before the installation, saves
// into the state directory, and the installation then uses what it saved.
func TestWorkerUpServiceOnATerminalRunsTheWizardBeforeInstalling(t *testing.T) {
	f, rig := serviceGate(t, false, true, "build", "docs")
	rig.pair.block = true
	k := newKeys(t)
	k.typeLines("1", "https://hub.example:8443")
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- runUpOut(f.deps, context.Background(), k.r, &out) }()
	// The wizard waits for the pairing; nothing may be installed before it ends.
	waitForOutput(t, &out, "Pairing code: MN-K7Q4-XY12")
	if calls := f.runCalls(); len(calls) != 0 {
		t.Errorf("service commands ran while the wizard was still open: %v", calls)
	}
	k.enter(t)
	if err := <-done; err != nil {
		t.Fatalf("worker up: %v\n%s", err, &out)
	}
	if len(f.runCalls()) == 0 {
		t.Error("the service was not installed after the wizard")
	}
	if sel := rig.share(); !sel.Configured || sel.Mode != store.ShareAll {
		t.Errorf("share = %+v", sel)
	}
	if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), workerConfigName))[config.KeyServer]; got != "https://hub.example:8443" {
		t.Errorf("the saved server = %q", got)
	}
	if !strings.Contains(out.String(), "Settings saved to") {
		t.Errorf("the installation did not report its save:\n%s", &out)
	}
}

// An invitation key enrolls the Worker by itself: "up" must not offer a code.
func TestWorkerUpWithAnInvitationKeyOffersNoPairingCode(t *testing.T) {
	for name, cfg := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"flag":             {map[string]string{}, []string{"--invite-key", "mnk_abc.def"}},
		"environment file": {map[string]string{"MINION_INVITE_KEY_FILE": "/run/secrets/never-read"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.env["MINION_STATE_DIR"] = t.TempDir()
			deps, p, rig := foregroundGate(t, true, cfg.env, "build")
			k := newKeys(t)
			k.typeLines("1") // share all; the Hub address comes from the flag below
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out syncBuf
			done := make(chan error, 1)
			args := append([]string{"--foreground", "--server", "https://hub.example:8443"}, cfg.args...)
			go func() { done <- runUpOut(deps, ctx, k.r, &out, args...) }()
			select {
			case <-p.ready:
			case err := <-done:
				t.Fatalf("worker up ended before the Worker started: %v\n%s", err, &out)
			case <-time.After(30 * time.Second):
				t.Fatal("worker up did not start")
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("worker up: %v", err)
			}
			if strings.Contains(out.String(), "Pairing code:") || !strings.Contains(out.String(), "An invitation key was given") {
				t.Errorf("output:\n%s", &out)
			}
			if rig.pair.n() != 0 {
				t.Error("pairing started although an invitation key was given")
			}
		})
	}
}
