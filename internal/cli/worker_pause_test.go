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

func TestStatusApplierConfirmsAllowNewSessions(t *testing.T) {
	dir := t.TempDir()
	reply := func(state, v string) func(context.Context, string) (json.RawMessage, error) {
		return func(context.Context, string) (json.RawMessage, error) {
			return json.Marshal(agent.LiveStatusReply{StateDir: state, AllowNewSessions: v})
		}
	}
	off := configChange{Key: "allow_new_sessions", Value: "off"}
	for _, tc := range []struct {
		name   string
		call   func(context.Context, string) (json.RawMessage, error)
		change configChange
		want   string // "" ok; else part of the error
	}{
		{"confirmed", reply(dir, "off"), off, ""},
		{"unset means on", reply(dir, "on"), configChange{Key: "allow_new_sessions", Unset: true}, ""},
		{"an older Worker cannot confirm", reply(dir, ""), off, "too old"},
		{"another source keeps it open", reply(dir, "on"), off, "enforces"},
		{"no Worker", func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }, off, "no running"},
		{"another key is applied at the next start", reply(dir, "on"), configChange{Key: "name", Value: "x"}, "no running"},
	} {
		err := statusApplier{call: tc.call}.ApplyConfig(context.Background(), dir, tc.change)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestConfigSetAllowNewSessionsSavesAndRefusesBadValues(t *testing.T) {
	dir := t.TempDir()
	cd := testConfigDeps(nil)
	rec := &recordingApplier{}
	cd.applier = rec
	if _, _, err := configCmd(t, cd, dir, "set", "allow_new_sessions", "maybe"); err == nil {
		t.Fatal("maybe was accepted")
	}
	out, _, err := configCmd(t, cd, dir, "set", "allow_new_sessions", "off")
	if err != nil || !strings.Contains(out, "applied") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if readConfig(t, dir)["allow_new_sessions"] != "off" || len(rec.changes) != 1 || rec.changes[0].Value != "off" {
		t.Fatalf("file %v changes %v", readConfig(t, dir), rec.changes)
	}
}
