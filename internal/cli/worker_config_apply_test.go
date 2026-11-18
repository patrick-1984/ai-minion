package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// liveWorker is a fake running Worker on the control socket for the live
// apply of allow_new_sessions.
type liveWorker struct {
	mu        sync.Mutex
	dir       string
	instances []string // the instance status answers, one per call (last repeats)
	statusN   int
	applied   []agent.LocalConfigApplyArgs
	answer    func(args agent.LocalConfigApplyArgs) (json.RawMessage, error)
	silent    bool // never answers status
}

func (w *liveWorker) deps(cd workerConfigDeps) workerConfigDeps {
	cd.tighten = tightenApplier{
		lockState: func(string) (io.Closer, error) { return nil, agent.ErrStateInUse },
		call:      w.call,
		deadline:  300 * time.Millisecond,
		poll:      10 * time.Millisecond,
	}
	return cd
}

func (w *liveWorker) call(_ context.Context, op string, args any) (json.RawMessage, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if op == agent.OpLocalStatus {
		if w.silent {
			return nil, errors.New("no Worker answers")
		}
		i := min(w.statusN, len(w.instances)-1)
		w.statusN++
		return json.Marshal(agent.LiveStatusReply{StateDir: w.dir, Instance: w.instances[i]})
	}
	if op != "worker_config_apply+config_apply.v1" {
		return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op "+op)
	}
	a := args.(agent.LocalConfigApplyArgs)
	w.applied = append(w.applied, a)
	return w.answer(a)
}

func applyAnswer(effective string, sources ...string) func(agent.LocalConfigApplyArgs) (json.RawMessage, error) {
	return func(a agent.LocalConfigApplyArgs) (json.RawMessage, error) {
		if sources == nil {
			sources = []string{}
		}
		return json.Marshal(agent.ConfigApplyResult{Key: a.Key, Effective: effective, Sources: sources})
	}
}

func exitOf(err error) int {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	if err != nil {
		return ExitError
	}
	return ExitOK
}

// A running Worker confirms the block: exit 0 and it says so.
func TestLiveApplyBlockConfirmed(t *testing.T) {
	dir := t.TempDir()
	w := &liveWorker{dir: dir, instances: []string{"i1"}, answer: applyAnswer("off", "file")}
	out, _, err := configCmd(t, w.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "off")
	if err != nil || !strings.Contains(out, "running Worker applied it") {
		t.Fatalf("set off = %q, %v", out, err)
	}
	if len(w.applied) != 1 || w.applied[0].Instance != "i1" || w.applied[0].Key != "allow_new_sessions" {
		t.Fatalf("applied = %+v", w.applied)
	}
}

// No Worker answers in time: the value is saved, exit 22.
func TestApplyDeadlineExit22(t *testing.T) {
	dir := t.TempDir()
	w := &liveWorker{dir: dir, silent: true}
	_, errOut, err := configCmd(t, w.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "off")
	if exitOf(err) != ExitNotInForce || !strings.Contains(errOut, "did not confirm") {
		t.Fatalf("exit %d, stderr %q", exitOf(err), errOut)
	}
	if readConfig(t, dir)["allow_new_sessions"] != "off" {
		t.Fatal("the value was not saved")
	}
}

// The Worker's environment says off: "set on" is saved but not in force.
func TestEnvOffSetOnIs22WithSources(t *testing.T) {
	dir := t.TempDir()
	w := &liveWorker{dir: dir, instances: []string{"i1"}, answer: applyAnswer("off", "env")}
	_, errOut, err := configCmd(t, w.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "on")
	if exitOf(err) != ExitNotInForce || !strings.Contains(errOut, "by env") {
		t.Fatalf("exit %d, stderr %q", exitOf(err), errOut)
	}
}

// A Worker that restarted between status and apply is retried once with
// its new instance; a second restart is 27.
func TestStaleInstanceRetriedOnceThen27(t *testing.T) {
	dir := t.TempDir()
	stale := func(a agent.LocalConfigApplyArgs) (json.RawMessage, error) {
		if a.Instance != "i2" {
			return nil, proto.NewError(proto.CodeConflict, "restarted")
		}
		return applyAnswer("off")(a)
	}
	w := &liveWorker{dir: dir, instances: []string{"i1", "i2"}, answer: stale}
	if _, _, err := configCmd(t, w.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "off"); err != nil {
		t.Fatalf("one restart: %v", err)
	}
	w2 := &liveWorker{dir: dir, instances: []string{"i1"}, answer: func(agent.LocalConfigApplyArgs) (json.RawMessage, error) {
		return nil, proto.NewError(proto.CodeConflict, "restarted")
	}}
	if _, _, err := configCmd(t, w2.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "off"); exitOf(err) != ExitStateChanged {
		t.Fatalf("two restarts: exit %d (%v), want 27", exitOf(err), err)
	}
	if len(w2.applied) != 2 {
		t.Fatalf("tries = %d, want 2", len(w2.applied))
	}
}

// An older Worker without config_apply.v1: saved, not in force, 22.
func TestOldWorkerCannotApplyIs22(t *testing.T) {
	dir := t.TempDir()
	w := &liveWorker{dir: dir, instances: []string{"i1"}, answer: func(agent.LocalConfigApplyArgs) (json.RawMessage, error) {
		return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op")
	}}
	_, errOut, err := configCmd(t, w.deps(testConfigDeps(nil)), dir, "set", "allow_new_sessions", "off")
	if exitOf(err) != ExitNotInForce || !strings.Contains(errOut, "too old") {
		t.Fatalf("exit %d, stderr %q", exitOf(err), errOut)
	}
}

// An unset of an absent key still reconciles the running Worker.
func TestRepeatedUnsetReconciles(t *testing.T) {
	dir := t.TempDir()
	w := &liveWorker{dir: dir, instances: []string{"i1"}, answer: applyAnswer("on")}
	cd := w.deps(testConfigDeps(nil))
	for i := 0; i < 2; i++ {
		if _, _, err := configCmd(t, cd, dir, "unset", "allow_new_sessions"); err != nil {
			t.Fatalf("unset %d: %v", i, err)
		}
	}
	if len(w.applied) != 2 {
		t.Fatalf("applies = %d, want 2 (the second unset reconciles too)", len(w.applied))
	}
}

// No Worker runs: exit 0, the value is used at the next start; an
// environment off in this shell gives 22 for "set on".
func TestOfflineApplyNextStart(t *testing.T) {
	dir := t.TempDir()
	out, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "allow_new_sessions", "off")
	if err != nil || !strings.Contains(out, "reads allow_new_sessions when it starts") {
		t.Fatalf("offline set = %q, %v", out, err)
	}
	_, errOut, err := configCmd(t, testConfigDeps(map[string]string{"MINION_ALLOW_NEW_SESSIONS": "off"}), dir, "set", "allow_new_sessions", "on")
	if exitOf(err) != ExitNotInForce || !strings.Contains(errOut, "by env") {
		t.Fatalf("offline set on with env off: exit %d, stderr %q", exitOf(err), errOut)
	}
	if _, _, err := configCmd(t, testConfigDeps(nil), dir, "set", "allow_new_sessions", "maybe"); err == nil {
		t.Fatal("an invalid value was saved")
	}
}
