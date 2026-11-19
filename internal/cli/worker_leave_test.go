package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// pinDialer enrolls every invitation at the Hub whose root key is pub.
type pinDialer struct{ pub []byte }

func (d pinDialer) Enroll(context.Context, agent.InviteParams) (*agent.InviteOutcome, error) {
	return &agent.InviteOutcome{Pin: identity.Pin{Pub: d.pub}, Welcome: agent.HubWelcome{
		Type: proto.TypeWelcome, WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private",
	}}, nil
}

// hubToken returns a join token of the Hub whose root key is pub.
func hubToken(t *testing.T, url string, pub []byte) string {
	t.Helper()
	fp, err := identity.FingerprintOf(pub)
	if err != nil {
		t.Fatal(err)
	}
	k, err := identity.NewInviteKey(fp)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := identity.NewJoinToken(url, k)
	if err != nil {
		t.Fatal(err)
	}
	return tok.Encode()
}

func fpOf(pub []byte) string {
	f, _ := identity.FingerprintOf(pub)
	return string(f)
}

// leaveWorker is a fake running Worker on one state directory: it holds the
// real lifecycle lock while it runs and lets go of it when it stops. lossy
// sessions end on a stop unless the caller accepts them; countFails makes
// the count fail. onStop runs before it stops.
type leaveWorker struct {
	mu         sync.Mutex
	state      string
	lock       io.Closer
	lossy      int
	countFails bool
	onStop     func()
	stops      int
}

func startLeaveWorker(t *testing.T, state string) *leaveWorker {
	t.Helper()
	l, err := agent.LockLifecycle(state)
	if err != nil {
		t.Fatal(err)
	}
	w := &leaveWorker{state: state, lock: l}
	t.Cleanup(func() { w.stop() })
	return w
}

func (w *leaveWorker) stop() {
	if w.lock != nil {
		_ = w.lock.Close()
		w.lock = nil
	}
}

func (w *leaveWorker) running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lock != nil
}

func (w *leaveWorker) call(_ context.Context, op string) (json.RawMessage, error) {
	if !w.running() {
		return nil, client.ErrWorkerNotRunning
	}
	if op == agent.OpLocalStatus {
		return json.Marshal(agent.LiveStatusReply{StateDir: w.state, Instance: "i1", SessionHost: "conpty"})
	}
	return nil, errors.New("unexpected op " + op)
}

func (w *leaveWorker) callArgs(_ context.Context, op string, args any) (json.RawMessage, error) {
	if op != "worker_lifecycle+lifecycle.v1+stop_guard.v1" {
		return nil, errors.New("unexpected op " + op)
	}
	a := args.(agent.LocalLifecycleArgs)
	if a.Instance != "i1" || a.EnrollmentEpoch == nil {
		return nil, errors.New("leave's stop is not bound to the instance and the enrollment generation")
	}
	refuse := func(reason string) error {
		det, _ := json.Marshal(map[string]any{"reason": reason, "sessions_lost": w.lossy})
		pe := proto.NewError(proto.CodeConflict, "nothing was done")
		pe.Details = det
		return pe
	}
	switch {
	case w.countFails && !a.AcceptUnknown:
		return nil, refuse(agent.ReasonCountUnavailable)
	case !w.countFails && w.lossy > a.AcceptLoss:
		return nil, refuse(agent.ReasonSessionsWouldEnd)
	}
	if w.onStop != nil {
		w.onStop()
	}
	w.mu.Lock()
	w.stops++
	w.stop()
	w.mu.Unlock()
	return json.Marshal(agent.LifecycleResult{Action: "stop", SessionsLost: w.lossy, Exiting: true})
}

func leaveTestDeps(w *leaveWorker, env map[string]string) workerLeaveDeps {
	d := defaultWorkerLeaveDeps()
	d.stdin = strings.NewReader("")
	d.lookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	d.down = baseDown()
	d.down.goos = "windows"
	d.down.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	if w != nil {
		d.down.call, d.down.callArgs = w.call, w.callArgs
	}
	d.sleep = func(time.Duration) {}
	d.stopWait = 2 * time.Second
	return d
}

// runLeave runs "minion worker leave --json args..." and returns the result
// object and the exit status.
func runLeave(t *testing.T, d workerLeaveDeps, args ...string) (map[string]any, int) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerLeaveCmd(d))
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", "leave", "--json"}, args...), &out, &errOut)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout %q: %v (stderr %q)", out.String(), err, errOut.String())
	}
	return m, code
}

// expectArgs are the --yes arguments for the Worker of dir as it is now.
func expectArgs(t *testing.T, dir string) []string {
	t.Helper()
	fp, epoch, err := leaveIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--yes", "--state-dir", dir, "--expect-worker-fingerprint", fp, "--expect-enrollment-epoch", strconv.FormatUint(epoch, 10)}
}

func configOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, workerConfigName))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func enrolled(t *testing.T, dir string) bool {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	_, ok, err := agent.NewEnrollments(st, dir).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func leaveStrs(v any) []string {
	var out []string
	for _, x := range anySlice(v) {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// Join Hub A with a token, leave, join Hub B with a token: the expectation
// that the join of A created is gone, so B's token is accepted.
func TestLeaveAThenJoinB(t *testing.T) {
	c := newJoinCLI(t)
	c.worker.running = false
	hubA, hubB := bytes.Repeat([]byte{9}, 32), bytes.Repeat([]byte{7}, 32)
	c.deps.inviteDialer = pinDialer{pub: hubA}
	m, code := c.run(hubToken(t, "https://a.example", hubA)+"\n", "--token-file", "-", "--share-existing", "later")
	if code != ExitNotRunning || m["enrollment"] != "committed" || m["config"] != "saved" {
		t.Fatalf("join A = %v, %d\n%s", m, code, c.stderr.String())
	}
	if cfg := c.config(); cfg[config.KeyHubFingerprint] != fpOf(hubA) {
		t.Fatalf("config after join A = %v", cfg)
	}
	if pin, ok, err := agent.ReadJoinPin(c.state); err != nil || !ok || pin.Source != agent.JoinPinSourceJoin || !pin.CreatedFingerprint(fpOf(hubA)) {
		t.Fatalf("join record = %+v, %v, %v", pin, ok, err)
	}
	if err := os.WriteFile(filepath.Join(c.state, workerConfigName), []byte(`{"server":"https://a.example","hub_fingerprint":"`+fpOf(hubA)+`","allow_new_sessions":"false"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, code = runLeave(t, leaveTestDeps(nil, nil), expectArgs(t, c.state)...)
	if code != ExitOK || m["left"] != true || slices.Contains(leaveStrs(m["kept"]), config.KeyHubFingerprint) {
		t.Fatalf("leave = %v, %d", m, code)
	}
	cfg := c.config()
	if _, ok := cfg[config.KeyHubFingerprint]; ok {
		t.Fatalf("hub_fingerprint survived the leave: %v", cfg)
	}
	if cfg[config.KeyAllowNewSessions] != "false" {
		t.Fatalf("allow_new_sessions was not kept: %v", cfg)
	}
	if _, ok, _ := agent.ReadJoinPin(c.state); ok {
		t.Fatal("the join record survived the leave")
	}

	c.deps.inviteDialer = pinDialer{pub: hubB}
	m, code = c.run(hubToken(t, "https://b.example", hubB)+"\n", "--token-file", "-", "--share-existing", "later")
	if code != ExitNotRunning || m["enrollment"] != "committed" {
		t.Fatalf("join B = %v, %d\n%s", m, code, c.stderr.String())
	}
	if cfg := c.config(); cfg[config.KeyHubFingerprint] != fpOf(hubB) || cfg[config.KeyServer] != "https://b.example" {
		t.Fatalf("config after join B = %v", cfg)
	}
}

// A hub_fingerprint provisioned before the join, with the same value as the
// token's, is not the join's: leave keeps it and says so.
func TestLeaveKeepsPreprovisionedMatchingPin(t *testing.T) {
	c := newJoinCLI(t)
	c.worker.running = false
	hub := bytes.Repeat([]byte{9}, 32)
	if err := os.WriteFile(filepath.Join(c.state, workerConfigName), []byte(`{"hub_fingerprint":"`+fpOf(hub)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.deps.inviteDialer = pinDialer{pub: hub}
	if m, code := c.run(hubToken(t, "https://a.example", hub)+"\n", "--token-file", "-", "--share-existing", "later"); m["enrollment"] != "committed" {
		t.Fatalf("join = %v, %d\n%s", m, code, c.stderr.String())
	}
	m, code := runLeave(t, leaveTestDeps(nil, nil), expectArgs(t, c.state)...)
	if code != ExitOK || !slices.Equal(leaveStrs(m["kept"]), []string{config.KeyHubFingerprint}) {
		t.Fatalf("leave = %v, %d", m, code)
	}
	if cfg := c.config(); cfg[config.KeyHubFingerprint] != fpOf(hub) {
		t.Fatalf("the provisioned pin was removed: %v", cfg)
	}
	if enrolled(t, c.state) || keyExists(c.state) {
		t.Fatal("the enrollment or the key survived")
	}
}

// The Hub rotated its key after the join: the pinned key is another one, but
// the config value is still the one the join created, so it goes.
func TestLeaveRemovesJoinPinAfterHubKeyRotation(t *testing.T) {
	c := newJoinCLI(t)
	c.worker.running = false
	hub := bytes.Repeat([]byte{9}, 32)
	c.deps.inviteDialer = pinDialer{pub: hub}
	if m, code := c.run(hubToken(t, "https://a.example", hub)+"\n", "--token-file", "-", "--share-existing", "later"); m["enrollment"] != "committed" {
		t.Fatalf("join = %v, %d\n%s", m, code, c.stderr.String())
	}
	// The rotation: the enrollment now pins the new Hub key.
	st, err := store.Open(c.state)
	if err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, c.state)
	cur, ok, err := enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	cur.Pin = identity.Pin{Pub: bytes.Repeat([]byte{5}, 32)}
	if err := enr.Commit(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	m, code := runLeave(t, leaveTestDeps(nil, nil), expectArgs(t, c.state)...)
	if code != ExitOK || len(leaveStrs(m["kept"])) != 0 || !slices.Contains(leaveStrs(m["removed"]), config.KeyHubFingerprint) {
		t.Fatalf("leave = %v, %d", m, code)
	}
	if _, ok := c.config()[config.KeyHubFingerprint]; ok {
		t.Fatal("hub_fingerprint survived")
	}
}

// A pin without a join record (an older join, or hand-written) is kept and
// reported as kept: ["hub_fingerprint"].
func TestLeaveLegacyPinKeptAndReported(t *testing.T) {
	dir := pairedStateDir(t)
	fp := testHubFingerprint()
	if err := os.WriteFile(filepath.Join(dir, workerConfigName), []byte(`{"server":"https://hub.example","hub_fingerprint":"`+fp+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.SetPause(dir, true, time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	m, code := runLeave(t, leaveTestDeps(nil, nil), expectArgs(t, dir)...)
	if code != ExitOK || !slices.Equal(leaveStrs(m["kept"]), []string{"hub_fingerprint"}) || m["pause_cleared"] != true {
		t.Fatalf("leave = %v, %d", m, code)
	}
	cfg := configOf(t, dir)
	if cfg[config.KeyHubFingerprint] != fp {
		t.Fatalf("the legacy pin was removed: %v", cfg)
	}
	if _, ok := cfg[config.KeyServer]; ok {
		t.Fatalf("server survived: %v", cfg)
	}
	if p, err := agent.ReadPause(dir); err != nil || p.Paused {
		t.Fatalf("pause = %+v, %v", p, err)
	}
}

// The Worker joined again while it was being stopped: the epoch moved, so
// leave exits 27 and deletes nothing.
func TestLeaveConcurrentReenrollment(t *testing.T) {
	dir := pairedStateDir(t)
	args := expectArgs(t, dir)
	w := startLeaveWorker(t, dir)
	w.onStop = func() {
		st, err := store.Open(dir)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = st.Close() }()
		hub, _ := identity.GenerateKey()
		if err := agent.NewEnrollments(st, dir).Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w2", Name: "pi", Owner: "bob", Kind: "private"}); err != nil {
			t.Error(err)
		}
	}
	m, code := runLeave(t, leaveTestDeps(w, nil), args...)
	if code != ExitStateChanged || m["code"] != "state_changed" {
		t.Fatalf("leave = %v, %d", m, code)
	}
	if !slices.Equal(leaveStrs(m["steps_done"]), []string{leaveStepVerified, leaveStepStopped}) {
		t.Fatalf("steps_done = %v", m["steps_done"])
	}
	if !keyExists(dir) || !enrolled(t, dir) || configOf(t, dir)[config.KeyServer] == "" {
		t.Fatal("something was deleted")
	}
	// A stale Worker fingerprint is refused before anything is stopped.
	w2 := startLeaveWorker(t, t.TempDir())
	bad := append([]string(nil), args...)
	bad[4] = strings.Repeat("a", 52)
	if m, code := runLeave(t, leaveTestDeps(w2, nil), bad...); code != ExitStateChanged || w2.stops != 0 {
		t.Fatalf("stale fingerprint = %v, %d (stops %d)", m, code, w2.stops)
	}
}

// --state-dir picks the profile: only that one is stopped and reset, even
// though the environment names another one.
func TestLeaveCustomStateDirResetsOnlyThatProfile(t *testing.T) {
	a, b := pairedStateDir(t), pairedStateDir(t)
	w := startLeaveWorker(t, a)
	m, code := runLeave(t, leaveTestDeps(w, map[string]string{"MINION_STATE_DIR": b}), append(expectArgs(t, a), "--accept-loss", "0")...)
	if code != ExitOK || m["state_dir"] != a || m["stopped"] != true {
		t.Fatalf("leave = %v, %d", m, code)
	}
	if keyExists(a) || enrolled(t, a) {
		t.Fatal("profile A was not reset")
	}
	if !keyExists(b) || !enrolled(t, b) || configOf(t, b)[config.KeyServer] == "" {
		t.Fatal("profile B was touched")
	}
}

// A Worker of this user that answers for another state directory is not
// stopped.
func TestLeaveDoesNotStopAnotherProfilesWorker(t *testing.T) {
	a := pairedStateDir(t)
	other := startLeaveWorker(t, t.TempDir())
	m, code := runLeave(t, leaveTestDeps(other, nil), expectArgs(t, a)...)
	if code != ExitOK || m["stopped"] != false || other.stops != 0 || !other.running() {
		t.Fatalf("leave = %v, %d (stops %d)", m, code, other.stops)
	}
}

// A failure half way lists the steps that were done; the same command then
// finishes the job.
func TestLeavePartialFailureListsStepsDone(t *testing.T) {
	dir := pairedStateDir(t)
	args := expectArgs(t, dir)
	// A pause "file" that cannot be removed.
	if err := os.MkdirAll(filepath.Join(dir, agent.PauseFile, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, code := runLeave(t, leaveTestDeps(nil, nil), args...)
	steps := leaveStrs(m["steps_done"])
	if code != ExitError || !slices.Contains(steps, agent.ResetStepPairing) || !slices.Contains(steps, agent.ResetStepConfig) ||
		slices.Contains(steps, agent.ResetStepKey) || !strings.Contains(m["message"].(string), "done: verified, stopped, pairing_deleted, config_cleared") {
		t.Fatalf("leave = %v, %d", m, code)
	}
	if !keyExists(dir) {
		t.Fatal("the key went although an earlier step failed")
	}
	if err := os.RemoveAll(filepath.Join(dir, agent.PauseFile)); err != nil {
		t.Fatal(err)
	}
	args = expectArgs(t, dir) // the reset moved the enrollment epoch
	if m, code := runLeave(t, leaveTestDeps(nil, nil), args...); code != ExitOK || keyExists(dir) {
		t.Fatalf("second leave = %v, %d", m, code)
	}
}

// ConPTY sessions would end: 23 with the count, 29 when they cannot be
// counted; nothing is stopped or deleted. --accept-loss N leaves.
func TestLeaveConPTYSessionLoss(t *testing.T) {
	dir := pairedStateDir(t)
	args := expectArgs(t, dir)
	w := startLeaveWorker(t, dir)
	w.lossy = 2
	m, code := runLeave(t, leaveTestDeps(w, nil), args...)
	if code != ExitSessionsWouldEnd || m["sessions_lost"] != float64(2) || !w.running() || !keyExists(dir) {
		t.Fatalf("23: %v, %d", m, code)
	}
	w.countFails = true
	m, code = runLeave(t, leaveTestDeps(w, nil), args...)
	if code != ExitCountUnavailable || !w.running() || !keyExists(dir) {
		t.Fatalf("29: %v, %d", m, code)
	}
	w.countFails = false
	m, code = runLeave(t, leaveTestDeps(w, nil), append(args, "--accept-loss", "2")...)
	if code != ExitOK || w.running() || keyExists(dir) || m["sessions_lost"] != float64(2) {
		t.Fatalf("accepted: %v, %d", m, code)
	}
}

// The output says that the Worker stays listed on the Hub, in the JSON
// result and in the text.
func TestLeaveSaysTheWorkerStaysListedOnTheHub(t *testing.T) {
	dir := pairedStateDir(t)
	args := expectArgs(t, dir)
	env := map[string]string{"MINION_SERVER": "https://env.example"}
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerLeaveCmd(leaveTestDeps(nil, env)))
	var out, errOut bytes.Buffer
	if code := execute(context.Background(), root, append([]string{"worker", "leave"}, args...), &out, &errOut); code != ExitOK {
		t.Fatalf("leave = %d: %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "stays listed on the Hub until you remove it there") || !strings.Contains(out.String(), "MINION_SERVER") {
		t.Fatalf("text = %q", out.String())
	}
	dir = pairedStateDir(t)
	m, code := runLeave(t, leaveTestDeps(nil, env), expectArgs(t, dir)...)
	left := anySlice(m["not_removed"])
	if code != ExitOK || m["still_listed_on_hub"] != true || !strings.Contains(m["note"].(string), "stays listed on the Hub") || len(left) != 1 {
		t.Fatalf("json = %v, %d", m, code)
	}
}

func TestLeaveUsage(t *testing.T) {
	dir := pairedStateDir(t)
	if m, code := runLeave(t, leaveTestDeps(nil, nil), "--yes", "--state-dir", dir); code != ExitUsage {
		t.Fatalf("--yes without expectations = %v, %d", m, code)
	}
	if m, code := runLeave(t, leaveTestDeps(nil, nil), "--state-dir", dir); code != ExitUsage {
		t.Fatalf("--json without --yes = %v, %d", m, code)
	}
	if !keyExists(dir) {
		t.Fatal("a refused leave deleted the key")
	}
}

// Leave moves the enrollment generation, so a foreground pairing that still
// waits for the Hub with the old key cannot commit after the reset.
func TestLeaveAdvancesEnrollmentEpoch(t *testing.T) {
	dir := pairedStateDir(t)
	_, before, err := leaveIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if m, code := runLeave(t, leaveTestDeps(nil, nil), expectArgs(t, dir)...); code != ExitOK {
		t.Fatalf("leave = %v, %d", m, code)
	}
	_, after, err := leaveIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("enrollment epoch stayed %d after leave", after)
	}
}
