package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

type pauseRig struct {
	state  string
	live   bool
	opErr  error
	calls  []string
	args   []any
	wrote  []bool
	writeE error
}

func (r *pauseRig) deps() workerPauseDeps {
	return workerPauseDeps{
		lookupEnv: func(k string) (string, bool) {
			if k == "MINION_STATE_DIR" {
				return r.state, true
			}
			return "", false
		},
		call: func(_ context.Context, op string, args any) (json.RawMessage, error) {
			r.calls, r.args = append(r.calls, op), append(r.args, args)
			if !r.live {
				return nil, client.ErrWorkerNotRunning
			}
			if op == agent.OpLocalStatus {
				return json.Marshal(agent.LiveStatusReply{StateDir: r.state})
			}
			return json.RawMessage(`{}`), r.opErr
		},
		write: func(_ string, p bool) error { r.wrote = append(r.wrote, p); return r.writeE },
	}
}

func (r *pauseRig) run(t *testing.T, pause bool) (string, error) {
	t.Helper()
	cmd := newWorkerPauseCmd(r.deps(), pause)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	err := runWorkerPause(cmd, r.deps(), pause)
	return out.String(), err
}

func TestPauseAsksTheRunningWorker(t *testing.T) {
	r := &pauseRig{state: t.TempDir(), live: true}
	out, err := r.run(t, true)
	if err != nil || !strings.Contains(out, "is paused") || len(r.wrote) != 0 {
		t.Fatalf("out=%q err=%v wrote=%v", out, err, r.wrote)
	}
	if r.calls[len(r.calls)-1] != agent.OpLocalPause {
		t.Fatalf("calls %v", r.calls)
	}
	if m, _ := r.args[len(r.args)-1].(map[string]bool); !m["paused"] {
		t.Fatalf("args %v", r.args)
	}
	out, err = r.run(t, false)
	if err != nil || !strings.Contains(out, "resumed") {
		t.Fatalf("resume: %q %v", out, err)
	}
}

func TestPauseWithoutAWorkerSavesTheChoice(t *testing.T) {
	r := &pauseRig{state: t.TempDir()}
	if out, err := r.run(t, true); err != nil || !strings.Contains(out, "saved") || len(r.wrote) != 1 || !r.wrote[0] {
		t.Fatalf("out=%q err=%v wrote=%v", out, err, r.wrote)
	}
	r.writeE = errors.New("disk full")
	if _, err := r.run(t, true); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a save failure was reported as success: %v", err)
	}
}

func TestPauseRefusedByAnOlderWorkerAndFailureIsNotSuccess(t *testing.T) {
	r := &pauseRig{state: t.TempDir(), live: true, opErr: proto.NewError(proto.CodeUnsupportedOp, "no")}
	if out, err := r.run(t, true); err == nil || !strings.Contains(err.Error(), "older") || strings.Contains(out, "is paused") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	r.opErr = errors.New("timeout")
	if out, err := r.run(t, true); err == nil || strings.Contains(out, "is paused") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
