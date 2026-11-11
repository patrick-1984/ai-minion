package workers

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// originOfCall routes one sessions_list call for p to worker and returns the
// origin of the op.request that the Worker was sent.
func originOfCall(t *testing.T, p authz.Principal, worker WorkerRecord) (proto.Origin, error) {
	t.Helper()
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionsList, WorkerID: worker.ID, Args: ops.SessionsListArgs{}})
	if err != nil {
		return proto.Origin{}, err
	}
	return st.request(t).Origin, nil
}

func allScope() authz.Scope {
	return scopeFunc(func(authz.ResolvedResource) []ops.Action {
		return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}
	})
}

// The Hub derives the origin from the authenticated principal (F-01).
func TestRouterDerivesTheOriginFromThePrincipal(t *testing.T) {
	tests := []struct {
		name   string
		p      authz.Principal
		target WorkerRecord
		want   proto.Origin
	}{
		{"browser session", authz.NewSessionPrincipal(idAlice), wAlicePi1,
			proto.Origin{Kind: proto.OriginUser, UserID: idAlice}},
		{"act-as", authz.NewActAsPrincipal(idRoot, idAlice, time.Now().Add(time.Hour)), wAlicePi1,
			proto.Origin{Kind: proto.OriginUser, UserID: idAlice, ActorID: idRoot}},
		{"unbound API key", authz.NewAPIKeyPrincipal(idAlice, allScope(), "").WithKeyID("k-1"), wAlicePi1,
			proto.Origin{Kind: proto.OriginAPIKey, UserID: idAlice, KeyID: "k-1"}},
		{"Worker identity on its own Worker", authz.NewWorkerPrincipal(wAlicePi1.ID), wAlicePi1,
			proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: wAlicePi1.ID, SourceWorkerFP: wAlicePi1.Fingerprint, SourceWorkerOwnerID: idAlice}},
		{"key bound to a Worker", authz.NewAPIKeyPrincipal(idAlice, allScope(), wAlicePi2.ID).WithKeyID("k-2"), wAlicePi1,
			proto.Origin{Kind: proto.OriginAPIKey, UserID: idAlice, KeyID: "k-2", KeyBoundWorkerID: wAlicePi2.ID,
				SourceWorkerID: wAlicePi2.ID, SourceWorkerFP: wAlicePi2.Fingerprint, SourceWorkerOwnerID: idAlice}},
		// The key owner (an admin) differs from the owner of the bound Worker:
		// the source Worker's owner is the bound Worker's owner, the user is the
		// key owner (N5).
		{"key owner differs from the owner of the bound Worker", authz.NewAPIKeyPrincipal(idRoot, allScope(), wAlicePi2.ID).WithKeyID("k-3"), wAlicePi1,
			proto.Origin{Kind: proto.OriginAPIKey, UserID: idRoot, KeyID: "k-3", KeyBoundWorkerID: wAlicePi2.ID,
				SourceWorkerID: wAlicePi2.ID, SourceWorkerFP: wAlicePi2.Fingerprint, SourceWorkerOwnerID: idAlice}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := originOfCall(t, tc.p, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("origin = %+v\nwant     %+v", got, tc.want)
			}
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A caller cannot write the origin: no argument reaches it.
func TestOriginIsNeverTakenFromTheArguments(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	forged := map[string]any{
		"origin": map[string]any{"kind": "worker", "source_worker_id": wBobPi1.ID},
		"kind":   "worker", "source_worker_id": wBobPi1.ID, "user_id": idBob,
	}
	if _, err := r.rt.Call(context.Background(), CallRequest{
		Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: forged,
	}); err != nil {
		t.Fatal(err)
	}
	want := proto.Origin{Kind: proto.OriginUser, UserID: idAlice}
	if got := st.request(t).Origin; got != want {
		t.Fatalf("origin = %+v, want %+v", got, want)
	}
}

// A principal whose origin cannot be derived sends nothing (fail closed).
func TestRequestWithoutADerivableOriginIsNotSent(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	for name, p := range map[string]authz.Principal{
		"key without an id":    authz.NewAPIKeyPrincipal(idAlice, allScope(), ""),
		"unknown source":       authz.NewAPIKeyPrincipal(idAlice, allScope(), "no-such-worker").WithKeyID("k"),
		"empty principal kind": {},
	} {
		if _, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}}); err == nil {
			t.Errorf("%s: the call succeeded", name)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.written) != 0 {
		t.Fatalf("%d frames were sent for requests without an origin", len(st.written))
	}
}
