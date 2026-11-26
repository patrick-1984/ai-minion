package workers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func keepArgs(target ops.Target, keep bool) ops.SessionKeepSetArgs {
	return ops.SessionKeepSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Keep: keep}
}

// session_keep_set is a session write like set_task: the owner and an admin
// reach it with operate, another user gets not_found and a read-only key
// forbidden, and nothing refused reaches a Worker.
func TestSessionKeepSetOwnerAndAdminOnly(t *testing.T) {
	r := newRig(t, nil)
	own := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	call := func(p authz.Principal, tg ops.Target, keep bool) error {
		_, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionKeepSet,
			WorkerID: tg.WorkerID, Session: tg.SessionName, Args: keepArgs(tg, keep)})
		return err
	}

	st := newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered}))
	r.script(st)
	if err := call(routeAlice(), own, true); err != nil {
		t.Fatalf("owner: %v", err)
	}
	req := st.request(t)
	var got ops.SessionKeepSetArgs
	if err := json.Unmarshal(req.Args, &got); err != nil {
		t.Fatal(err)
	}
	if req.Op != ops.OpSessionKeepSet || !got.Keep || got.Target.SessionName != "minion-1" || !req.Actions.Permits(ops.ActionOperate) ||
		req.Origin.Kind != proto.OriginUser || req.Origin.UserID != idAlice {
		t.Fatalf("owner request = %+v, args %+v", req, got)
	}

	foreign := ops.Target{WorkerID: wBobBox.ID, SessionName: "minion-1"}
	st = newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered}))
	r.script(st)
	if err := call(routeRoot(), foreign, false); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := json.Unmarshal(st.request(t).Args, &got); err != nil || got.Keep {
		t.Fatalf("admin args = %+v, %v", got, err)
	}

	opens := r.be.openCount()
	if err := call(routeAlice(), foreign, true); code(err) != proto.CodeNotFound {
		t.Errorf("another user's session: %v, want not_found", err)
	}
	readOnly := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(authz.ResolvedResource) []ops.Action {
		return []ops.Action{ops.ActionRead, ops.ActionMessage}
	}), "")
	if err := call(readOnly, own, true); code(err) != proto.CodeForbidden {
		t.Errorf("read and message key: %v, want forbidden", err)
	}
	if r.be.openCount() != opens {
		t.Fatal("a refused keep reached a Worker")
	}
}

// On call streams a direct grant reaches session_keep_set at Control only:
// View and Message are refused before anything reaches a Worker.
func TestSessionKeepSetNeedsAControlGrant(t *testing.T) {
	r := newSCRig(t)
	r.targetOnline(proto.FeatureSendTextAgentOnly, proto.FeatureSessionKeepV1)
	for _, level := range []int{authz.LevelView, authz.LevelMessage} {
		r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(level))
		frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionKeepSet, scArgs(ops.OpSessionKeepSet, grantedBuild), "", "agent"),
			ops.WriteResult{Outcome: ops.OutcomeDelivered})
		if c := errCodeOf(t, frames); c != proto.CodeForbidden {
			t.Errorf("level %d: %q, want forbidden", level, c)
		}
	}
	if r.be.openCount() != 0 {
		t.Fatal("a refused keep reached a Worker")
	}

	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSessionKeepSet, scArgs(ops.OpSessionKeepSet, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	req := sent.request(t)
	var got ops.SessionKeepSetArgs
	if err := json.Unmarshal(req.Args, &got); err != nil {
		t.Fatal(err)
	}
	if wr.Outcome != ops.OutcomeDelivered || !got.Keep || got.Target.SessionName != "build" || !req.Origin.Grant {
		t.Fatalf("Control keep: result %+v, request %+v, args %+v", wr, req, got)
	}
}
