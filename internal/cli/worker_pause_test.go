package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

type pauseRig struct {
	dir     string
	env     map[string]string
	deps    workerPauseDeps
	calls   []string
	locked  bool // a Worker holds the lifecycle lock
	reply   func(op string) (json.RawMessage, error)
	cfgLock int
}

func newPauseRig(t *testing.T) *pauseRig {
	t.Helper()
	r := &pauseRig{dir: t.TempDir()}
	r.env = map[string]string{"MINION_STATE_DIR": r.dir, "MINION_SERVER": "https://hub.example"}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	r.deps = workerPauseDeps{
		lookupEnv:  func(k string) (string, bool) { v, ok := r.env[k]; return v, ok },
		loadConfig: loadWorkerConfig,
		lockState: func(dir string) (io.Closer, error) {
			if r.locked {
				return nil, agent.ErrStateInUse
			}
			return agent.LockLifecycle(dir)
		},
		lockConfig: func(path string) (func(), error) {
			if filepath.Base(path) != configLockName {
				t.Errorf("config lock %s", path)
			}
			r.cfgLock++
			return lockConfigFile(path)
		},
		setPause: agent.SetPause,
		call: func(_ context.Context, op string, _ any) (json.RawMessage, error) {
			r.calls = append(r.calls, op)
			if op == agent.OpLocalStatus {
				return json.Marshal(agent.LiveStatusReply{StateDir: r.dir, Instance: "inst-1", State: "connected"})
			}
			if r.reply != nil {
				return r.reply(op)
			}
			return nil, errors.New("no reply")
		},
		now: func() time.Time { return now },
	}
	return r
}

func (r *pauseRig) run(t *testing.T, pause bool, args ...string) (map[string]any, int, string) {
	t.Helper()
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	workerCommand(root).AddCommand(newWorkerPauseCmd(r.deps, true), newWorkerPauseCmd(r.deps, false))
	name := "resume"
	if pause {
		name = "pause"
	}
	var out, errOut bytes.Buffer
	code := execute(context.Background(), root, append([]string{"worker", name, "--json"}, args...), &out, &errOut)
	var obj map[string]any
	if err := json.Unmarshal(out.Bytes(), &obj); err != nil {
		t.Fatalf("stdout %q: %v (stderr %q)", out.String(), err, errOut.String())
	}
	return obj, code, errOut.String()
}

// Offline: the CLI holds the lifecycle lock and the config lock while it
// writes the pause file; no control socket is used.
func TestPauseOfflineUnderLifecycleLock(t *testing.T) {
	r := newPauseRig(t)
	var heldDuringWrite bool
	r.deps.setPause = func(dir string, paused bool, until, now time.Time) (agent.PauseState, error) {
		_, err := agent.LockLifecycle(dir)
		heldDuringWrite = errors.Is(err, agent.ErrStateInUse)
		return agent.SetPause(dir, paused, until, now)
	}
	obj, code, _ := r.run(t, true, "--for", "30m")
	if code != ExitOK || obj["ok"] != true || obj["paused"] != true || obj["until"] != "2026-11-18T12:30:00Z" || obj["applied"] != "offline" {
		t.Fatalf("pause = %v, exit %d", obj, code)
	}
	if !heldDuringWrite || r.cfgLock != 1 || len(r.calls) != 0 {
		t.Fatalf("lifecycle lock held %t, config lock %d, socket calls %v", heldDuringWrite, r.cfgLock, r.calls)
	}
	p, err := agent.ReadPause(r.dir)
	if err != nil || !p.Paused || p.Generation != 1 {
		t.Fatalf("pause file = %+v, %v", p, err)
	}
	obj, code, _ = r.run(t, false)
	if code != ExitOK || obj["paused"] != false || obj["generation"] != float64(2) {
		t.Fatalf("resume = %v, exit %d", obj, code)
	}
	// The status reports what was saved.
	if p, _ := agent.ReadPause(r.dir); p.Paused {
		t.Fatalf("still paused: %+v", p)
	}
}

func TestPauseSoloRefused(t *testing.T) {
	r := newPauseRig(t)
	delete(r.env, "MINION_SERVER")
	obj, code, _ := r.run(t, true)
	if code != ExitError || !strings.Contains(obj["message"].(string), "no Hub configured") {
		t.Fatalf("pause on Solo = %v, exit %d", obj, code)
	}
	if _, err := os.Stat(filepath.Join(r.dir, agent.PauseFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pause file was written: %v", err)
	}
}

// A running Worker without pause.v1 refuses the qualified op: exit 20 and
// the file is not written.
func TestPauseLiveWorkerWithoutFeatureIs20(t *testing.T) {
	r := newPauseRig(t)
	r.locked = true
	r.reply = func(op string) (json.RawMessage, error) {
		if op != "worker_pause+pause.v1" {
			t.Errorf("op %q, want the qualified worker_pause+pause.v1", op)
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, "unknown op")
	}
	obj, code, _ := r.run(t, true)
	if code != ExitWorkerTooOld || obj["code"] != "worker_too_old" {
		t.Fatalf("pause = %v, exit %d", obj, code)
	}
	if _, err := os.Stat(filepath.Join(r.dir, agent.PauseFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pause file was written: %v", err)
	}
	if r.cfgLock != 0 {
		t.Fatal("the CLI wrote while a Worker owns the state")
	}
}

// A running Worker with pause support applies it and acknowledges it.
func TestPauseLiveWorkerApplies(t *testing.T) {
	r := newPauseRig(t)
	r.locked = true
	r.reply = func(string) (json.RawMessage, error) {
		return json.Marshal(agent.PauseState{Paused: true, Generation: 7})
	}
	obj, code, _ := r.run(t, true)
	if code != ExitOK || obj["applied"] != "live" || obj["generation"] != float64(7) {
		t.Fatalf("pause = %v, exit %d", obj, code)
	}
}

// A write or fsync error is a non-zero exit without an acknowledgement.
func TestPauseWriteErrorNoAck(t *testing.T) {
	r := newPauseRig(t)
	r.deps.setPause = func(string, bool, time.Time, time.Time) (agent.PauseState, error) {
		return agent.PauseState{}, errors.New("write the pause file (fsync): disk full")
	}
	obj, code, _ := r.run(t, true)
	if code == ExitOK || obj["ok"] != false || obj["paused"] == true {
		t.Fatalf("pause = %v, exit %d", obj, code)
	}
}

func TestPauseForMustBePositive(t *testing.T) {
	r := newPauseRig(t)
	obj, code, _ := r.run(t, true, "--for", "-5m")
	if code != ExitUsage || obj["code"] != "usage" {
		t.Fatalf("pause --for -5m = %v, exit %d", obj, code)
	}
}
