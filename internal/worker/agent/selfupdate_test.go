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
	u := &selfUpdate{
		log: slog.New(slog.NewTextHandler(&logBuf{}, nil)), stateDir: t.TempDir(), exe: "/x/minion", now: time.Now,
		installer: func() (releaseInstaller, error) { return inst, nil },
		hubRange:  func() *version.ProtoRange { return nil },
		restart:   func() { restarted <- struct{}{} },
	}
	return u, restarted
}

func callUpdate(t *testing.T, u *selfUpdate, args ops.WorkerUpdateArgs) (ops.WorkerUpdateResult, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := u.handleUpdate(context.Background(), Request{Op: ops.OpWorkerUpdate, Args: raw})
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
	if o.AllowDowngrade || o.Selector.IsChannel() || o.Current != "v0.9.0" || o.ExecPath != "/x/minion" {
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
	for i := 0; i < guardMaxStarts-1; i++ {
		if _, restart := guardOnStart(log, dir, exe, "v0.9.1", now.Add(time.Duration(i)*time.Second)); restart {
			t.Fatalf("restart after start %d", i+1)
		}
	}
	if _, restart := guardOnStart(log, dir, exe, "v0.9.1", now.Add(5*time.Second)); !restart {
		t.Fatal("no rollback on the third start")
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
