package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// fakePairDialer answers every Pair call with the same result; with block set
// it holds the connection until the context ends.
type fakePairDialer struct {
	mu     sync.Mutex
	calls  []agent.PairParams
	block  bool
	result agent.PairResult
}

func (d *fakePairDialer) Pair(ctx context.Context, p agent.PairParams) (agent.PairResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, p)
	d.mu.Unlock()
	p.OnWaiting()
	if d.block {
		<-ctx.Done()
		return agent.PairResult{}, ctx.Err()
	}
	return d.result, nil
}

func (d *fakePairDialer) n() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func okPairResult(owner string) agent.PairResult {
	return agent.PairResult{
		Pin:     identity.Pin{Pub: bytes.Repeat([]byte{9}, 32)},
		Welcome: agent.HubWelcome{Type: proto.TypeWelcome, WorkerID: "w-1", Name: "pi", Owner: owner, Kind: "private"},
		Version: proto.Current,
	}
}

// fakeHubDialer is the connect transport that revokes the old identity of
// --new-code: it records the keys it was asked to connect with and answers
// with reply, or fails with err when no reply is set (an unreachable Hub).
type fakeHubDialer struct {
	mu    sync.Mutex
	keys  [][]byte
	urls  []string
	pins  []identity.Pin
	reply *agent.Reply
	err   error
}

func (d *fakeHubDialer) Connect(_ context.Context, p agent.ConnectParams) (*agent.Attempt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, append([]byte(nil), p.Key.Public()...))
	d.urls = append(d.urls, p.HubURL)
	d.pins = append(d.pins, p.Pin)
	if d.reply == nil {
		return nil, d.err
	}
	return &agent.Attempt{Pin: p.Pin, Reply: *d.reply}, nil
}

func (d *fakeHubDialer) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.keys)
}

type pairCLI struct {
	t       *testing.T
	state   string
	deps    workerPairDeps
	dialer  *fakePairDialer
	hubConn *fakeHubDialer
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
}

func newPairCLI(t *testing.T, env map[string]string) *pairCLI {
	t.Helper()
	state := t.TempDir()
	e := map[string]string{"MINION_STATE_DIR": state, "MINION_SERVER": "https://hub.example:8443"}
	for k, v := range env {
		if v == "" {
			delete(e, k)
		} else {
			e[k] = v
		}
	}
	c := &pairCLI{t: t, state: state, dialer: &fakePairDialer{result: okPairResult("alice")}, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		hubConn: &fakeHubDialer{err: errors.New("dial the hub: connection refused")}}
	c.deps = workerPairDeps{
		stdin:      strings.NewReader(""),
		stdout:     c.stdout,
		stderr:     c.stderr,
		lookupEnv:  envFrom(e),
		loadConfig: loadWorkerConfig,
		dialer:     c.dialer,
		hubDialer:  c.hubConn,
		revokeWait: 200 * time.Millisecond,
		backoff:    &transport.Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond},
		newCode: func() identity.PairingCode {
			pc, _ := identity.ParsePairingCode("K7Q4XY12")
			return pc
		},
		saveServer: saveWorkerServer,
	}
	return c
}

func (c *pairCLI) exec(args ...string) error {
	c.t.Helper()
	return c.execCtx(context.Background(), args...)
}

func (c *pairCLI) execCtx(ctx context.Context, args ...string) error {
	c.t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerPairCmd(c.deps), newWorkerJoinCmd(c.deps))
	root.SetArgs(append([]string{"worker"}, args...))
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.ExecuteContext(ctx)
}

func (c *pairCLI) enrollment() (agent.Enrollment, bool) {
	c.t.Helper()
	st, err := store.Open(c.state)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	enr, ok, err := agent.NewEnrollments(st, c.state).Load(context.Background())
	if err != nil {
		c.t.Fatal(err)
	}
	return enr, ok
}

func (c *pairCLI) pair() {
	c.t.Helper()
	if err := c.exec("pair"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *pairCLI) keyPub() []byte {
	c.t.Helper()
	k, err := identity.LoadKey(filepath.Join(c.state, agent.WorkerKeyFile))
	if err != nil {
		c.t.Fatal(err)
	}
	return k.Public()
}

func TestWorkerPairAndJoinAreRegistered(t *testing.T) {
	root := NewRoot()
	for _, name := range []string{"pair", "join"} {
		cmd, _, err := root.Find([]string{"worker", name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("worker %s not found: %v", name, err)
		}
	}
	cmd, _, _ := root.Find([]string{"worker", "pair"})
	for _, f := range []string{"new-code", "yes", "server"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("worker pair has no --%s", f)
		}
	}
	// run still works next to them
	if c, _, err := root.Find([]string{"worker", "run"}); err != nil || c.Name() != "run" {
		t.Fatalf("worker run lost: %v", err)
	}
}

func TestWorkerPairShowsTheCodeAndStoresTheEnrollment(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	out := c.stdout.String()
	if !strings.Contains(out, "MN-K7Q4-XY12") || !strings.Contains(out, "paired to alice") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(c.stderr.String(), "MN-K7Q4-XY12") {
		t.Fatalf("the code is in the log (stderr) too:\n%s", c.stderr.String())
	}
	enr, ok := c.enrollment()
	if !ok || enr.Owner != "alice" || enr.WorkerID != "w-1" {
		t.Fatalf("enrollment = %+v %v", enr, ok)
	}
	if got := c.dialer.calls[0].HubURL; got != "https://hub.example:8443" {
		t.Fatalf("hub = %s", got)
	}
}

func TestWorkerPairRefusesAPairedWorker(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	before := c.dialer.n()
	err := c.exec("pair")
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeAlreadyPaired {
		t.Fatalf("err = %v, want already_paired", err)
	}
	if c.dialer.n() != before {
		t.Fatal("a paired Worker must not connect")
	}
	if _, ok := c.enrollment(); !ok {
		t.Fatal("the enrollment stays")
	}
}

func TestWorkerPairNewCodeAsksAndAborts(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	key := c.keyPub()
	for _, answer := range []string{"n\n", "\n", ""} {
		c.deps.stdin = strings.NewReader(answer)
		c.stdout.Reset()
		err := c.exec("pair", "--new-code")
		if err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("answer %q: err = %v", answer, err)
		}
		if !strings.Contains(c.stdout.String(), "[y/N]") {
			t.Fatalf("no question:\n%s", c.stdout.String())
		}
		if _, ok := c.enrollment(); !ok || string(c.keyPub()) != string(key) {
			t.Fatalf("answer %q changed something", answer)
		}
	}
}

func TestWorkerPairNewCodeAfterConfirmationCreatesANewIdentityAndPairsAgain(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	oldKey := c.keyPub()
	c.dialer.result = okPairResult("bob")
	c.deps.stdin = strings.NewReader("yes\n")
	c.stdout.Reset()
	if err := c.exec("pair", "--new-code"); err != nil {
		t.Fatal(err)
	}
	if string(c.keyPub()) == string(oldKey) {
		t.Fatal("a new Worker key is expected")
	}
	if got := string(c.dialer.calls[len(c.dialer.calls)-1].Key.Public()); got != string(c.keyPub()) {
		t.Fatal("pairing must use the new key")
	}
	enr, ok := c.enrollment()
	if !ok || enr.Owner != "bob" {
		t.Fatalf("enrollment = %+v %v", enr, ok)
	}
	if !strings.Contains(c.stdout.String(), "paired to bob") {
		t.Fatalf("stdout:\n%s", c.stdout.String())
	}
}

func TestWorkerPairNewCodeWithYesDoesNotAsk(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	c.deps.stdin = nil // would abort if asked
	c.stdout.Reset()
	if err := c.exec("pair", "--new-code", "--yes"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.stdout.String(), "[y/N]") {
		t.Fatalf("--yes must not ask:\n%s", c.stdout.String())
	}
}

func TestWorkerPairNewCodeOnAnUnpairedWorkerWithAKeyCreatesANewKeyWithoutAsking(t *testing.T) {
	c := newPairCLI(t, nil)
	// An unpaired Worker that already has a key, such as one whose Hub
	// removed it: pairing reaches the Hub, which answers revoked.
	st, err := store.Open(c.state)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := agent.NewEnrollments(st, c.state).LoadOrCreateKey()
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	c.deps.stdin = nil // would abort if asked
	if err := c.exec("pair", "--new-code"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.stdout.String(), "[y/N]") {
		t.Fatal("nothing to confirm on an unpaired Worker")
	}
	if string(c.keyPub()) == string(oldKey.Public()) {
		t.Fatal("--new-code must always create a new Worker key")
	}
	if got := string(c.dialer.calls[0].Key.Public()); got != string(c.keyPub()) {
		t.Fatal("pairing must use the new key")
	}
	if _, ok := c.enrollment(); !ok {
		t.Fatal("not paired")
	}
}

func TestWorkerPairNewCodeOnAnUnpairedWorkerWithoutAKeyJustPairs(t *testing.T) {
	c := newPairCLI(t, nil)
	if err := c.exec("pair", "--new-code"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.stdout.String(), "[y/N]") {
		t.Fatal("nothing to confirm on an unpaired Worker")
	}
	if _, ok := c.enrollment(); !ok {
		t.Fatal("not paired")
	}
}

func TestWorkerPairNewCodeWithoutAHubURLChangesNothing(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	oldKey := c.keyPub()
	oldEnr, _ := c.enrollment()
	before := c.dialer.n()
	// The Hub address is set only in the service's environment, not here.
	c.deps.lookupEnv = envFrom(map[string]string{"MINION_STATE_DIR": c.state})
	c.deps.stdin = nil
	err := c.exec("pair", "--new-code", "--yes")
	if !errors.Is(err, agent.ErrNoHubURL) {
		t.Fatalf("err = %v, want ErrNoHubURL", err)
	}
	if string(c.keyPub()) != string(oldKey) {
		t.Fatal("the key must be unchanged")
	}
	if enr, ok := c.enrollment(); !ok || enr.WorkerID != oldEnr.WorkerID || enr.Owner != oldEnr.Owner {
		t.Fatalf("the enrollment must be unchanged: %+v %v", enr, ok)
	}
	if c.dialer.n() != before {
		t.Fatal("no connection without a URL")
	}
}

func TestWorkerPairNeedsAHubURL(t *testing.T) {
	c := newPairCLI(t, map[string]string{"MINION_SERVER": ""})
	err := c.exec("pair")
	if !errors.Is(err, agent.ErrNoHubURL) {
		t.Fatalf("err = %v", err)
	}
	if c.dialer.n() != 0 {
		t.Fatal("no connection without a URL")
	}
}

func TestWorkerPairServerFlagOverridesTheEnvironment(t *testing.T) {
	c := newPairCLI(t, nil)
	if err := c.exec("pair", "--server", "https://other.example"); err != nil {
		t.Fatal(err)
	}
	if got := c.dialer.calls[0].HubURL; got != "https://other.example" {
		t.Fatalf("hub = %s", got)
	}
}

func TestWorkerJoinRemembersTheHubAndPairs(t *testing.T) {
	c := newPairCLI(t, map[string]string{"MINION_SERVER": ""})
	if err := c.exec("join", "https://hub.example:8443"); err != nil {
		t.Fatal(err)
	}
	if got := c.dialer.calls[0].HubURL; got != "https://hub.example:8443" {
		t.Fatalf("hub = %s", got)
	}
	data, err := os.ReadFile(filepath.Join(c.state, workerConfigName))
	if err != nil || !strings.Contains(string(data), "https://hub.example:8443") {
		t.Fatalf("config = %s %v", data, err)
	}
	if !strings.Contains(c.stdout.String(), "paired to alice") {
		t.Fatalf("stdout:\n%s", c.stdout.String())
	}
	// A Worker service that is already running read its Hub address when it
	// started (or ran alone without one); the command says so.
	if !strings.Contains(c.stdout.String(), "already running uses the address it started with: restart it") {
		t.Fatalf("join does not tell that a running service must be restarted:\n%s", c.stdout.String())
	}
}

func TestWorkerPairWithoutJoinDoesNotTalkAboutSavingTheAddress(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	if strings.Contains(c.stdout.String(), "restart it") {
		t.Fatalf("stdout:\n%s", c.stdout.String())
	}
}

func TestWorkerJoinOnAPairedWorkerRefusesAndKeepsTheAddress(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	err := c.exec("join", "https://elsewhere.example")
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeAlreadyPaired {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(c.state, workerConfigName)); statErr == nil {
		t.Fatal("the address of a refused join must not be saved")
	}
}

func TestWorkerJoinRejectsABadURL(t *testing.T) {
	c := newPairCLI(t, nil)
	for _, u := range []string{"hub.example", "ftp://hub.example", "https://user:secret@hub.example"} {
		err := c.exec("join", u)
		if err == nil {
			t.Fatalf("%q must be refused", u)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("the error repeats a credential: %v", err)
		}
	}
	if c.dialer.n() != 0 {
		t.Fatal("no connection for a bad URL")
	}
}

func TestWorkerPairStopsCleanlyOnCancel(t *testing.T) {
	c := newPairCLI(t, nil)
	c.dialer.block = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.execCtx(ctx, "pair") }()
	deadline := time.Now().Add(5 * time.Second)
	for c.dialer.n() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("pairing did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pair did not stop")
	}
	if _, ok := c.enrollment(); ok {
		t.Fatal("nothing may be stored")
	}
}

// The old identity is revoked at its Hub: the CLI connects with the OLD key,
// authenticated by the old pin, before it pairs again.
func TestWorkerPairNewCodeRevokesTheOldIdentityAtItsHub(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	oldKey := c.keyPub()
	oldEnr, _ := c.enrollment()
	c.hubConn.err, c.hubConn.reply = nil, &agent.Reply{Type: proto.TypeNotEnrolled}
	if err := c.exec("pair", "--new-code", "--yes"); err != nil {
		t.Fatal(err)
	}
	if c.hubConn.calls() != 1 || string(c.hubConn.keys[0]) != string(oldKey) {
		t.Fatalf("the revocation must connect once with the old key, keys = %x", c.hubConn.keys)
	}
	if c.hubConn.urls[0] != "https://hub.example:8443" || string(c.hubConn.pins[0].Pub) != string(oldEnr.Pin.Pub) {
		t.Fatalf("url %q pin %x", c.hubConn.urls[0], c.hubConn.pins[0].Pub)
	}
	if !strings.Contains(c.stdout.String(), "The old identity was revoked at the Hub.") {
		t.Fatalf("stdout:\n%s", c.stdout.String())
	}
	if p, _ := agent.NewRetiredStore(c.state).List(); len(p) != 0 {
		t.Fatalf("the confirmed revocation must not stay pending: %d", len(p))
	}
}

func TestWorkerPairNewCodeKeepsTheRevocationPendingWhileTheOldHubIsUnreachable(t *testing.T) {
	c := newPairCLI(t, nil) // the fake Hub connection fails
	c.pair()
	oldKey := c.keyPub()
	if err := c.exec("pair", "--new-code", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.stdout.String(), "could not revoke the old identity yet") || !strings.Contains(c.stdout.String(), "paired to alice") {
		t.Fatalf("stdout:\n%s", c.stdout.String())
	}
	pending, err := agent.NewRetiredStore(c.state).List()
	if err != nil || len(pending) != 1 || string(pending[0].Key.Public()) != string(oldKey) || pending[0].HubURL != "https://hub.example:8443" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

// The old Hub is the one the service uses (environment and config file), not a
// --server flag that names the Hub of the new pairing.
func TestWorkerPairNewCodeRevokesAtTheServicesHubNotAtTheNewServerFlag(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	if err := c.exec("pair", "--new-code", "--yes", "--server", "https://other-hub.example:9443"); err != nil {
		t.Fatal(err)
	}
	pending, _ := agent.NewRetiredStore(c.state).List()
	if len(pending) != 1 || pending[0].HubURL != "https://hub.example:8443" {
		t.Fatalf("pending = %+v", pending)
	}
	if got := c.dialer.calls[len(c.dialer.calls)-1].HubURL; got != "https://other-hub.example:9443" {
		t.Fatalf("pairing used %s", got)
	}
}

// The service is another process with its own Enrollments value: re-pairing in
// the CLI invalidates its generation, so an old connection's revoke or welcome
// can neither delete nor revive the replacement.
func TestWorkerPairNewCodeInvalidatesTheRunningServicesEnrollmentGeneration(t *testing.T) {
	c := newPairCLI(t, nil)
	c.pair()
	st, err := store.Open(c.state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	service := agent.NewEnrollments(st, c.state)
	gen, changed := service.Watch()
	c.dialer.result = okPairResult("bob")
	if err := c.exec("pair", "--new-code", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !service.Moved(gen) {
		t.Fatal("the service must see that the CLI replaced the enrollment")
	}
	select {
	case <-changed:
	default:
		t.Fatal("the service's connection is told to end")
	}
	if _, err := service.WipeIf(context.Background(), gen); !errors.Is(err, agent.ErrEnrollmentChanged) {
		t.Fatalf("the old connection's revoke must be refused, got %v", err)
	}
	if enr, ok := c.enrollment(); !ok || enr.Owner != "bob" {
		t.Fatalf("the replacement must survive: %+v %v", enr, ok)
	}
}

// The old identity is revoked at the Hub that authenticated the enrollment,
// even when the pairing used --server and nothing else names that Hub: here the
// new address is the join argument and neither the environment nor the config
// file knows the old Hub.
func TestWorkerPairNewCodeRevokesAtTheHubOfTheEnrollmentNotAtTheJoinArgument(t *testing.T) {
	c := newPairCLI(t, map[string]string{"MINION_SERVER": ""})
	if err := c.exec("pair", "--server", "https://hub-a.example:8443"); err != nil {
		t.Fatal(err)
	}
	if err := c.exec("join", "https://hub-b.example:8443", "--new-code", "--yes"); err != nil {
		t.Fatal(err)
	}
	pending, err := agent.NewRetiredStore(c.state).List()
	if err != nil || len(pending) != 1 || pending[0].HubURL != "https://hub-a.example:8443" {
		t.Fatalf("pending = %+v, %v; the old key must be retired at Hub A", pending, err)
	}
	if got := c.hubConn.urls[0]; got != "https://hub-a.example:8443" {
		t.Fatalf("the revocation went to %s", got)
	}
}

// Joining saves the address under the config lock of `worker config`: a change
// that another command makes while join waits for the lock is not lost.
func TestSaveWorkerServerKeepsAConcurrentConfigChange(t *testing.T) {
	state := filepath.Join(t.TempDir(), "not-created-yet")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := lockConfigFile(filepath.Join(state, configLockName))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- saveWorkerServer(state, "https://hub.example:8443") }()
	select {
	case err := <-done:
		t.Fatalf("saveWorkerServer ran without the config lock (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	// Another command changes the file while it holds the lock.
	f, err := config.LoadFile(filepath.Join(state, workerConfigName))
	if err != nil {
		t.Fatal(err)
	}
	f.Set(config.KeyLogLevel, "debug")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state, workerConfigName))
	if err != nil || !strings.Contains(string(data), "debug") || !strings.Contains(string(data), "https://hub.example:8443") {
		t.Fatalf("config = %s (%v): the concurrent change or the address is missing", data, err)
	}
}

func TestSaveWorkerServerCreatesTheStateDirectory(t *testing.T) {
	state := filepath.Join(t.TempDir(), "a", "state")
	if err := saveWorkerServer(state, "https://hub.example:8443"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, workerConfigName)); err != nil {
		t.Fatal(err)
	}
}
