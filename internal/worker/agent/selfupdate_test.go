package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/version"
)

type fakeInstaller struct {
	mu   sync.Mutex
	opts []version.Options
	res  version.Result
	err  error
}

func (f *fakeInstaller) Run(_ context.Context, o version.Options) (version.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = append(f.opts, o)
	return f.res, f.err
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

func newTestUpdate(t *testing.T, inst *fakeInstaller) (*selfUpdate, chan struct{}) {
	t.Helper()
	restarted := make(chan struct{}, 1)
	rng := &version.ProtoRange{Min: version.ProtoVersion{Major: 3}, Max: version.ProtoVersion{Major: 3}}
	u := &selfUpdate{
		log: slog.New(slog.NewTextHandler(&logBuf{}, nil)), stateDir: t.TempDir(), exe: "/x/minion", now: time.Now,
		installer:    func() (releaseInstaller, error) { return inst, nil },
		hubRange:     func() *version.ProtoRange { return rng },
		restart:      func() { restarted <- struct{}{} },
		workerID:     func() string { return "w-self" },
		holdSessions: func(string) {},
	}
	return u, restarted
}

// updateRequest is how the op server hands over a Hub request: the origin is
// set and the Hub computed the caller's actions.
func updateRequest(op string, args any) Request {
	raw, _ := json.Marshal(args)
	return Request{Op: op, Args: raw, Actions: []ops.Action{ops.ActionManage},
		Origin: &proto.Origin{Kind: proto.OriginUser, UserID: "admin"}}
}

func callUpdate(t *testing.T, u *selfUpdate, args ops.WorkerUpdateArgs) (ops.WorkerUpdateResult, error) {
	t.Helper()
	if args.WorkerID == "" {
		args.WorkerID = "w-self"
	}
	res, err := u.handleUpdate(context.Background(), updateRequest(ops.OpWorkerUpdate, args))
	if err != nil {
		return ops.WorkerUpdateResult{}, err
	}
	return res.(ops.WorkerUpdateResult), nil
}

// Channels, garbage and non-canonical versions are refused before anything runs.
func TestWorkerUpdateAcceptsOnlyAnExactVersion(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{}
	u, _ := newTestUpdate(t, inst)
	for _, v := range []string{"latest", "0.9", "v0.9", "../x", "0.9.1", strings.Repeat("9", 300), ""} {
		if _, err := callUpdate(t, u, ops.WorkerUpdateArgs{Version: v}); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	if len(inst.opts) != 0 {
		t.Fatal("the installer ran")
	}
}

// The local veto, a downgrade, the same version and lost ConPTY sessions are
// answered with a reason; force never allows a downgrade.
func TestWorkerUpdateRefusals(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{}
	u, _ := newTestUpdate(t, inst)
	cases := []struct {
		name  string
		setup func()
		args  ops.WorkerUpdateArgs
		want  string
	}{
		{"downgrade", func() {}, ops.WorkerUpdateArgs{Version: "v0.8.2"}, "downgrade"},
		{"downgrade with force", func() {}, ops.WorkerUpdateArgs{Version: "v0.8.2", Force: true}, "downgrade"},
		{"same", func() {}, ops.WorkerUpdateArgs{Version: "v0.9.0"}, "already"},
		{"blocked", func() { u.block = "remote updates are turned off on this Worker (remote_update=off)" }, ops.WorkerUpdateArgs{Version: "v0.9.1"}, "remote_update=off"},
		{"sessions", func() {
			u.block = ""
			u.sessionsLost = func(context.Context) int { return 2 }
		}, ops.WorkerUpdateArgs{Version: "v0.9.1"}, "2 sessions"},
	}
	for _, tc := range cases {
		tc.setup()
		res, err := callUpdate(t, u, tc.args)
		if err != nil || res.Accepted || !strings.Contains(res.Reason, tc.want) {
			t.Errorf("%s: %+v, %v", tc.name, res, err)
		}
	}
	if len(inst.opts) != 0 {
		t.Fatal("the installer ran")
	}
}

func TestWorkerUpdateInstallsVerifiesAndRestarts(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{res: version.Result{Tag: "v0.9.1"}}
	u, restarted := newTestUpdate(t, inst)
	res, err := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"})
	if err != nil || !res.Accepted {
		t.Fatalf("%+v, %v", res, err)
	}
	if again, _ := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"}); again.Accepted {
		t.Error("a second update was accepted while one runs")
	}
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("no restart")
	}
	o := inst.opts[0]
	if o.AllowDowngrade || o.Selector.IsChannel() || o.Current != "v0.9.0" || o.ExecPath != "/x/minion" || o.HubRange == nil {
		t.Errorf("updater options %+v", o)
	}
	if st := u.status(); st.State != ops.UpdateStateRestarting || st.To != "v0.9.1" {
		t.Errorf("status %+v", st)
	}
	if r, ok := readPending(u.stateDir); !ok || r.From != "v0.9.0" || r.To != "v0.9.1" {
		t.Errorf("guard %+v %v", r, ok)
	}
}

func TestWorkerUpdateFailureIsReported(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{err: errors.New("signature does not verify")}
	u, restarted := newTestUpdate(t, inst)
	if res, _ := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"}); !res.Accepted {
		t.Fatal("not accepted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.status().State != ops.UpdateStateFailed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := u.status(); st.State != ops.UpdateStateFailed || !strings.Contains(st.Error, "signature") {
		t.Fatalf("status %+v", st)
	}
	select {
	case <-restarted:
		t.Fatal("restarted after a failure")
	default:
	}
}

// A new binary that starts three times within the window without committing
// is replaced by <exe>.old, and the old binary then reports the rollback.
func TestCrashGuardRestoresThePreviousBinary(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	exe := filepath.Join(bin, "minion")
	if err := os.WriteFile(exe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".old", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(&logBuf{}, nil))
	now := time.Now()
	if err := writePending(dir, pendingRecord{From: "v0.9.0", To: "v0.9.1", At: now}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < guardMaxStarts; i++ {
		if _, restart := guardOnStart(log, dir, exe, "v0.9.1", now.Add(time.Duration(i)*time.Second)); restart {
			t.Fatalf("rolled back on start %d, before %d failed starts", i+1, guardMaxStarts)
		}
	}
	if _, restart := guardOnStart(log, dir, exe, "v0.9.1", now.Add(5*time.Second)); !restart {
		t.Fatalf("no rollback after %d failed starts", guardMaxStarts)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("exe = %q, want the previous binary", b)
	}
	rolled, restart := guardOnStart(log, dir, exe, "v0.9.0", now.Add(6*time.Second))
	if restart || rolled == nil || rolled.To != "v0.9.1" {
		t.Fatalf("old binary: %+v %v", rolled, restart)
	}
	if _, ok := readPending(dir); ok {
		t.Fatal("guard not cleared")
	}
}

func TestCrashGuardCommitsAfterAStableConnection(t *testing.T) {
	dir := t.TempDir()
	if err := writePending(dir, pendingRecord{From: "v0.9.0", To: "v0.9.1"}); err != nil {
		t.Fatal(err)
	}
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		commitAfter(context.Background(), dir, "v0.9.1", func() bool { return true }, tick)
		close(done)
	}()
	start := time.Now()
	tick <- start
	tick <- start.Add(updateCommitAfter)
	<-done
	if _, ok := readPending(dir); ok {
		t.Fatal("not committed")
	}
}

func noFile(string) bool { return false }

func TestUpdateBlockReasons(t *testing.T) {
	withVersion(t, "v0.9.0")
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	dir := t.TempDir()
	exe := filepath.Join(dir, "minion")
	for _, tc := range []struct {
		exe, remote string
		env         map[string]string
		want        string
	}{
		{exe, "off", nil, "remote_update=off"},
		{exe, "on", map[string]string{"MINION_IN_CONTAINER": "1"}, "container"},
		{"/usr/bin/minion", "on", nil, "package"},
		{"", "on", nil, "unknown"},
		{filepath.Join(dir, "missing-dir", "minion"), "on", nil, "not writable"},
	} {
		if got := updateBlock(tc.exe, tc.remote, env(tc.env), noFile); !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s: %q, want %q", tc.exe, tc.remote, got, tc.want)
		}
	}
	if got := updateBlock(exe, "on", env(nil), func(p string) bool { return p == "/.dockerenv" }); !strings.Contains(got, "container") {
		t.Errorf("/.dockerenv: %q", got)
	}
	if got := updateBlock(exe, "on", env(nil), noFile); got != "" {
		t.Errorf("own writable dir: %q", got)
	}
}

// Only the Hub reaches the update: the local socket (no origin), a request
// for another Worker, a caller without manage, and a Worker without a Hub
// connection are refused before anything runs.
func TestWorkerUpdateIsHubOnlyAndForThisWorker(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{}
	u, _ := newTestUpdate(t, inst)
	args := ops.WorkerUpdateArgs{WorkerID: "w-self", Version: "v0.9.1"}
	local := updateRequest(ops.OpWorkerUpdate, args)
	local.Origin = nil
	if _, err := u.handleUpdate(context.Background(), local); err == nil {
		t.Error("a local request reached the update")
	}
	other := updateRequest(ops.OpWorkerUpdate, ops.WorkerUpdateArgs{WorkerID: "w-other", Version: "v0.9.1"})
	if _, err := u.handleUpdate(context.Background(), other); err == nil {
		t.Error("a request for another Worker was accepted")
	}
	weak := updateRequest(ops.OpWorkerUpdate, args)
	weak.Actions = []ops.Action{ops.ActionOperate}
	if _, err := u.handleUpdate(context.Background(), weak); err == nil {
		t.Error("operate was enough")
	}
	u.hubRange = func() *version.ProtoRange { return nil }
	if res, _ := callUpdate(t, u, args); res.Accepted || res.Reason != "not connected to a Hub" {
		t.Errorf("no Hub range: %+v", res)
	}
	if len(inst.opts) != 0 {
		t.Fatal("the installer ran")
	}
}

// New sessions are held from the acceptance; a refusal or failure lets them
// be created again.
func TestWorkerUpdateHoldsSessionCreation(t *testing.T) {
	withVersion(t, "v0.9.0")
	var mu sync.Mutex
	var holds []string
	inst := &fakeInstaller{err: errors.New("network down")}
	u, _ := newTestUpdate(t, inst)
	u.holdSessions = func(r string) { mu.Lock(); holds = append(holds, r); mu.Unlock() }
	u.sessionsLost = func(context.Context) int { return 1 }
	if res, _ := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"}); res.Accepted {
		t.Fatal("accepted with a session to lose")
	}
	mu.Lock()
	if len(holds) != 2 || holds[0] == "" || holds[1] != "" {
		t.Fatalf("holds = %q, want hold then release", holds)
	}
	holds = nil
	mu.Unlock()
	u.sessionsLost = nil
	if res, _ := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"}); !res.Accepted {
		t.Fatal("not accepted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.status().State != ops.UpdateStateFailed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(holds) != 2 || holds[1] != "" {
		t.Fatalf("holds = %q, want released after the failure", holds)
	}
	if _, ok := readPending(u.stateDir); ok {
		t.Fatal("the guard of a failed install was left behind")
	}
}

// The rollback restores the Worker database from the pre-migration backup
// written after the update began.
func TestRestoreStateDBUsesThePreMigrationBackup(t *testing.T) {
	dir := t.TempDir()
	since := time.Now().Add(-time.Minute)
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.db"), []byte("migrated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.db-wal"), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backups", "worker-20991231T000000.000Z.db"), []byte("old schema"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreStateDB(dir, since); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "worker.db")); string(b) != "old schema" {
		t.Fatalf("db = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "worker.db-wal")); err == nil {
		t.Fatal("the WAL of the migrated database survived")
	}
}

// Setting the hold waits for a session creation in flight: after it returns,
// no creation that started before can still finish unseen.
func TestSessionCreateHoldWaitsForCreationsInFlight(t *testing.T) {
	e := &Executor{}
	e.createMu.RLock() // a session_create in flight
	set := make(chan struct{})
	go func() { e.SetSessionCreateHold("updating"); close(set) }()
	select {
	case <-set:
		t.Fatal("the hold did not wait for the creation in flight")
	case <-time.After(100 * time.Millisecond):
	}
	e.createMu.RUnlock()
	select {
	case <-set:
	case <-time.After(5 * time.Second):
		t.Fatal("the hold never took effect")
	}
	if e.createHold.Load() == nil {
		t.Fatal("hold not set")
	}
}

// A rollback whose database restore fails keeps the guard and the new
// binary: the old binary never meets the newer schema.
func TestRollbackStopsWhenTheDatabaseCannotBeRestored(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	exe := filepath.Join(bin, "minion")
	_ = os.WriteFile(exe, []byte("new"), 0o755)
	_ = os.WriteFile(exe+".old", []byte("old"), 0o755)
	// A backup newer than the update that cannot be copied over the database:
	// the database path is a directory.
	_ = os.MkdirAll(filepath.Join(dir, "backups"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "backups", "worker-x.db"), []byte("old schema"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "worker.db", "blocker"), 0o700)
	now := time.Now()
	starts := []int64{now.Add(-3 * time.Second).Unix(), now.Add(-2 * time.Second).Unix(), now.Add(-time.Second).Unix()}
	_ = writePending(dir, pendingRecord{From: "v0.9.0", To: "v0.9.1", At: now.Add(-time.Minute), Starts: starts})
	log := slog.New(slog.NewTextHandler(&logBuf{}, nil))
	if _, restart := guardOnStart(log, dir, exe, "v0.9.1", now); restart {
		t.Fatal("restarted without the database")
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("the binary was rolled back without the database")
	}
	if _, ok := readPending(dir); !ok {
		t.Fatal("the guard was dropped")
	}
}

// A second Worker on the same state never reaches the crash guard: the
// lifecycle lock refuses it first.
func TestRecoveryRunsOnlyUnderTheLifecycleLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := LockLifecycle(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	_ = writePending(dir, pendingRecord{From: "v0.9.0", To: version.Version})
	before, _ := os.ReadFile(filepath.Join(dir, pendingFile))
	if _, err := LockLifecycle(dir); err == nil {
		t.Fatal("second lock taken")
	}
	after, _ := os.ReadFile(filepath.Join(dir, pendingFile))
	if string(before) != string(after) {
		t.Fatal("the guard changed")
	}
}
