package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// promptArgs are send_text arguments of a submitted prompt.
func promptArgs() map[string]any {
	a := scArgs(ops.OpSendText, grantedBuild)
	a["submit"] = true
	return a
}

// targetOnline puts the target Worker of aliceGrant online with the features.
func (r *scRig) targetOnline(features ...string) {
	r.be.mu.Lock()
	defer r.be.mu.Unlock()
	r.be.online[wAlicePi2.ID] = Online{WorkerID: wAlicePi2.ID, Features: features}
}

// A Message grant sends send_text as a prompt only: agent-only and submitted,
// with operate for the Worker; force and an unsubmitted text are refused, and
// so are send_keys, set_task and answer. Nothing refused reaches a Worker.
func TestSessionGrantMessageSendsPromptsOnly(t *testing.T) {
	r := newSCRig(t)
	r.targetOnline(proto.FeatureSendTextAgentOnly)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelMessage))

	args := promptArgs()
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, args, "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	req := sent.request(t)
	var got ops.SendTextArgs
	if err := json.Unmarshal(req.Args, &got); err != nil {
		t.Fatal(err)
	}
	if !got.AgentOnly || !got.Submit || got.Force || got.Text != "hi" || got.RequestID != args["request_id"] || req.RequestID != got.RequestID {
		t.Fatalf("prompt = %+v (request id %q)", got, req.RequestID)
	}
	if !req.Actions.Permits(ops.ActionOperate) || !req.Origin.Grant || req.Origin.SourceWorkerID != wAlicePi1.ID {
		t.Fatalf("request = %+v", req)
	}

	opens := r.be.openCount()
	forced := promptArgs()
	forced["force"] = true
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, forced, "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("forced prompt: %q, want forbidden", c)
	}
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeInvalidArgument {
		t.Fatalf("unsubmitted text: %q, want invalid_argument", c)
	}
	for _, op := range []string{ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer} {
		args := scArgs(op, grantedBuild)
		if op == ops.OpAnswer {
			args["expect"] = ops.ReviewedDialog{PaneID: "%1", PID: 7, Command: "claude", SessionCreated: 1,
				DialogDigest: strings.Repeat("a", ops.DialogDigestLen)}
		}
		frames, _ = r.call(t, wAlicePi1.ID, scRequest(op, args, "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
		if c := errCodeOf(t, frames); c != proto.CodeForbidden {
			t.Errorf("%s with a Message grant: %q, want forbidden", op, c)
		}
	}
	if r.be.openCount() != opens {
		t.Fatal("a refused request reached a Worker")
	}
}

// A target Worker that does not announce send_text.agent_only gets nothing
// from a Message grant.
func TestSessionGrantMessageNeedsTheWorkerFeature(t *testing.T) {
	r := newSCRig(t)
	r.targetOnline()
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelMessage))
	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, promptArgs(), "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeUnsupportedOp {
		t.Fatalf("old Worker: %q, want unsupported_op", c)
	}
	if r.be.openCount() != 0 {
		t.Fatal("the request reached the Worker")
	}
}

// A View grant cannot send a prompt either, and a Control grant sends
// send_text as it was asked for.
func TestSessionGrantPromptPathIsForMessageOnly(t *testing.T) {
	r := newSCRig(t)
	r.targetOnline(proto.FeatureSendTextAgentOnly)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, promptArgs(), "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("View grant: %q, want forbidden", c)
	}
	if r.be.openCount() != 0 {
		t.Fatal("a refused request reached a Worker")
	}

	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	var got ops.SendTextArgs
	if err := json.Unmarshal(sent.request(t).Args, &got); err != nil {
		t.Fatal(err)
	}
	if got.AgentOnly || got.Submit || got.Force {
		t.Fatalf("Control send_text was changed: %+v", got)
	}
}

// PromptOnly is for an agent-only, submitted send_text and nothing else.
func TestRouterPromptOnlyRefusesAnyOtherRequest(t *testing.T) {
	r := newRig(t, nil)
	id := ops.NewUUIDv7()
	tgt := ops.Target{WorkerID: wAlicePi2.ID, SessionName: "build"}
	for name, req := range map[string]CallRequest{
		"send_keys": {Op: ops.OpSendKeys, Args: ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: tgt, Keys: []string{"Enter"}}},
		"plain":     {Op: ops.OpSendText, Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: tgt, Text: "hi", Submit: true}},
		"forced": {Op: ops.OpSendText, Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id, Force: true}, Target: tgt, Text: "hi",
			Submit: true, AgentOnly: true}},
	} {
		req.Principal, req.WorkerID, req.Session, req.PromptOnly = routeAlice(), tgt.WorkerID, tgt.SessionName, true
		if _, err := r.rt.Call(context.Background(), req); code(err) != proto.CodeInternal {
			t.Errorf("%s: %v, want internal", name, err)
		}
	}
	if r.be.openCount() != 0 {
		t.Fatal("a refused request reached a Worker")
	}
}

// A grant revoked after the decision and before the dispatch is refused by
// the fence: the Worker's stream is opened but no request is written to it.
func TestSessionGrantRevokedBeforeDispatchIsRefused(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	r.be.beforeOpen = func(*fakeBackend) { r.grants.setDirect(wAlicePi1.ID, "agent") }
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("revoked before dispatch: %q, want forbidden", c)
	}
	sent.mu.Lock()
	defer sent.mu.Unlock()
	if len(sent.written) != 0 {
		t.Fatalf("%d frames reached the Worker", len(sent.written))
	}
}

// The fence compares the key the call was authorized with: an unchanged grant
// passes, a changed one and a failing store are refused.
func TestSessionGrantAdmitComparesTheGrantKey(t *testing.T) {
	g := &fakeGrants{}
	g.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	cs := NewCallServer(CallServerConfig{Grants: g, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()
	_, _, key, err := cs.authenticate(ctx, wAlicePi1.ID, "", "agent")
	if err != nil || key == "" {
		t.Fatalf("key %q %v", key, err)
	}
	admit := cs.admitGrant(wAlicePi1.ID, "agent", key)
	if err := admit(ctx, Online{}); err != nil {
		t.Fatalf("unchanged grant: %v", err)
	}
	changed := aliceGrant(authz.LevelControl)
	changed.Revision = 2
	g.setDirect(wAlicePi1.ID, "agent", changed)
	if err := admit(ctx, Online{}); code(err) != proto.CodeForbidden {
		t.Fatalf("changed grant: %v, want forbidden", err)
	}
	g.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	g.failDirect(errors.New("store down"))
	if err := admit(ctx, Online{}); err == nil {
		t.Fatal("a failing store admitted the dispatch")
	}
}

// One failed re-check of a running call is passed over, the second in a row
// ends it with forbidden.
func TestSessionGrantTwoFailedRechecksEndARunningCall(t *testing.T) {
	g := &fakeGrants{}
	g.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	cs := NewCallServer(CallServerConfig{Grants: g, ReauthInterval: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	pr, _, key, err := cs.authenticate(context.Background(), wAlicePi1.ID, "", "agent")
	if err != nil || key == "" {
		t.Fatalf("key %q %v", key, err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.reauthorizeWorker(ctx, cancel, pr, "agent", key)
	}()
	time.Sleep(20 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("ended while the store answered")
	}
	g.failDirect(errors.New("store down"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call was not withdrawn")
	}
	if c := proto.CodeOf(context.Cause(ctx)); c != proto.CodeForbidden {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
}
