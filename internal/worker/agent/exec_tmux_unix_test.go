//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// realExecutor wires the executor to a real tmux server on a private socket,
// the real classifier and a real store.
func realExecutor(t *testing.T) (*Executor, *tmuxtest.Server) {
	t.Helper()
	srv := tmuxtest.New(t)
	host, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	ex, err := NewExecutor(Config{Host: host, Classifier: agentdetect.New(), State: st, Policy: NewPolicy(st), LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return ex, srv
}

func realWrite(t *testing.T, ex *Executor, op string, args any) ops.WriteResult {
	t.Helper()
	b, _ := json.Marshal(args)
	raw, err := ex.Execute(context.Background(), Request{Op: op, Actions: []ops.Action{ops.ActionOperate, ops.ActionRead}, Args: b})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return res
}

func TestTmuxSendTextToACatPaneIsDelivered(t *testing.T) {
	ex, srv := realExecutor(t)
	target := srv.NewSession("catwork", "cat")

	res := realWrite(t, ex, ops.OpSendText, sendText("catwork", "hello-from-minion", false))
	if res.Outcome != ops.OutcomeDelivered || res.Forced || res.Pane == nil || res.Pane.Kind != ops.PaneKindOther {
		t.Fatalf("result = %+v pane %+v", res, res.Pane)
	}
	// The pane's state is unknown, which warns and never blocks.
	if !hasWarning(res, "readiness_unknown") {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	srv.WaitFor(target, "hello-from-minion", 10*time.Second)

	keys := realWrite(t, ex, ops.OpSendKeys, sendKeys("catwork", false, "Enter"))
	if keys.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_keys = %+v", keys)
	}
}

func TestTmuxSendTextToAShellPaneIsRefusedThenForced(t *testing.T) {
	ex, srv := realExecutor(t)
	target := srv.NewSession("shellwork") // the default shell
	// Let the shell draw its prompt, so that the paste is not racing startup.
	time.Sleep(500 * time.Millisecond)

	refused := realWrite(t, ex, ops.OpSendText, sendText("shellwork", "typed-into-shell", false))
	if refused.Outcome != ops.OutcomeFailedNotSent || refused.Reason != "shell_pane" || !refused.Forceable {
		t.Fatalf("refusal = %+v pane %+v", refused, refused.Pane)
	}
	if got := srv.Capture(target); strings.Contains(got, "typed-into-shell") {
		t.Fatalf("a refused write reached the shell:\n%s", got)
	}

	forced := realWrite(t, ex, ops.OpSendText, sendText("shellwork", "typed-into-shell", true))
	if forced.Outcome != ops.OutcomeDelivered || !forced.Forced {
		t.Fatalf("forced = %+v", forced)
	}
	srv.WaitFor(target, "typed-into-shell", 10*time.Second)
}

func TestTmuxSessionCreateSnapshotAndTask(t *testing.T) {
	ex, srv := realExecutor(t)
	srv.NewSession("adopted", "cat") // an adopted session stays as it is

	create := realWrite(t, ex, ops.OpSessionCreate, ops.SessionCreateArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Argv: []string{"cat"},
	})
	if create.Outcome != ops.OutcomeDelivered || create.Pane == nil || create.Pane.Session != "minion-1" {
		t.Fatalf("create = %+v pane %+v", create, create.Pane)
	}
	task := realWrite(t, ex, ops.OpSetTask, ops.SetTaskArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "minion-1"}, Task: "demo task",
	})
	if task.Outcome != ops.OutcomeDelivered {
		t.Fatalf("set_task = %+v", task)
	}

	raw, err := ex.Execute(context.Background(), Request{Op: ops.OpSessionsList, Actions: []ops.Action{ops.ActionRead}})
	if err != nil {
		t.Fatal(err)
	}
	var list ops.SessionsListResult
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	names := map[string]ops.SessionInfo{}
	for _, s := range list.Sessions {
		names[s.Name] = s
	}
	if names["adopted"].Name == "" || names["minion-1"].Task != "demo task" {
		t.Fatalf("sessions = %+v", list.Sessions)
	}
}

// A real tmux, the real classifier and a program that is named claude and
// shows a real permission dialog: the snapshot carries the reviewed metadata,
// an answer bound to it is delivered, and the same binding no longer works
// once the screen shows something else.
func TestTmuxAnswerIsBoundToTheReviewedDialog(t *testing.T) {
	ex, srv := realExecutor(t)
	// A shell script named claude: the classifier finds the agent by the
	// script name in the arguments of the pane's process, and the script draws
	// a real permission dialog and then echoes what it reads.
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\ncat \"$1\"\nwhile read -r line; do printf '%s\\n' \"$line\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dialog, err := filepath.Abs("../agentdetect/testdata/claude_dialog.txt")
	if err != nil {
		t.Fatal(err)
	}
	target := srv.NewSession("dlg", claude, dialog)
	srv.WaitFor(target, "Do you want to proceed?", 10*time.Second)

	snap := func() ops.PaneInfo {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"target": map[string]any{"session_name": "dlg"}})
		raw, err := ex.Execute(context.Background(), Request{Op: ops.OpSessionSnapshot, Actions: []ops.Action{ops.ActionRead}, Args: b})
		if err != nil {
			t.Fatal(err)
		}
		var r ops.SessionSnapshotResult
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r.Pane
	}
	var pi ops.PaneInfo
	for deadline := time.Now().Add(10 * time.Second); ; {
		if pi = snap(); pi.State == ops.PaneStateDialog || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pi.State != ops.PaneStateDialog || pi.Kind != "agent:claude" || pi.PID <= 0 || pi.SessionCreated <= 0 || !ops.ValidDialogDigest(pi.DialogDigest) {
		t.Fatalf("pane = %+v", pi)
	}
	exp := ops.ReviewedDialog{PaneID: pi.ID, PID: pi.PID, Command: pi.Command, SessionCreated: pi.SessionCreated, DialogDigest: pi.DialogDigest}
	answer := func(force bool) ops.WriteResult {
		return realWrite(t, ex, ops.OpAnswer, ops.AnswerArgs{
			WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force}, Target: ops.Target{SessionName: "dlg"}, Choice: "3", Expect: exp,
		})
	}

	// The same dialog twice: the digest is stable across reads.
	if again := snap(); again.DialogDigest != pi.DialogDigest {
		t.Fatalf("the digest of an unchanged dialog moved: %s -> %s", pi.DialogDigest, again.DialogDigest)
	}

	// The binding holds: the answer is delivered, once.
	if res := answer(false); res.Outcome != ops.OutcomeDelivered || res.Forced {
		t.Fatalf("bound answer = %+v", res)
	}
	// What the answer typed shows under the dialog, so the view the caller
	// reviewed is gone: the same binding delivers nothing, forced or not.
	deadline := time.Now().Add(10 * time.Second)
	for snap().DialogDigest == pi.DialogDigest {
		if time.Now().After(deadline) {
			t.Fatalf("the screen never changed:\n%s", srv.Capture(target))
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, force := range []bool{false, true} {
		res := answer(force)
		if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || res.Forceable || res.Forced {
			t.Fatalf("force=%v: stale answer = %+v", force, res)
		}
	}
}

// The text and its Enter are one request: cat shows the line twice only if the
// Enter reached it, and one request id is one audit row.
func TestTmuxSendTextSubmitPressesEnter(t *testing.T) {
	ex, srv := realExecutor(t)
	target := srv.NewSession("catsubmit", "cat")
	res := realWrite(t, ex, ops.OpSendText, ops.SendTextArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "catsubmit"}, Text: "submitted-by-minion", Submit: true,
	})
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text = %+v", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(srv.Capture(target), "submitted-by-minion") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the Enter never reached the pane:\n%s", srv.Capture(target))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
