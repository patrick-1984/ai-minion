package workers

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// originGrant is a direct grant of a session on wAlicePi2 that reaches every
// session of target (both Workers belong to alice).
func originGrant(id string, target WorkerRecord, level int) authz.DirectGrant {
	return authz.DirectGrant{
		ID: id, TargetWorkerID: target.ID, Level: level, Authority: authz.AuthorityOwner,
		GrantedBy: idAlice, SourceOwnerID: idAlice, TargetOwnerID: idAlice,
	}
}

// The Hub names the calling session and the grant in the origin of a call that
// a session principal makes (story W1). Both come from the principal.
func TestRouterFillsTheAttributionMembersOfTheOrigin(t *testing.T) {
	g1, g2 := ops.NewUUIDv7(), ops.NewUUIDv7()
	src := proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: wAlicePi2.ID, SourceWorkerFP: wAlicePi2.Fingerprint, SourceWorkerOwnerID: idAlice}
	with := func(f func(*proto.Origin)) proto.Origin { o := src; f(&o); return o }
	session := func(name string, legacy authz.Scope, direct ...authz.DirectGrant) authz.Principal {
		return authz.NewWorkerSessionPrincipal(wAlicePi2.ID, name, legacy, direct)
	}
	tests := []struct {
		name   string
		p      authz.Principal
		target WorkerRecord
		want   proto.Origin
	}{
		{"one direct grant permits the call", session("planner", nil, originGrant(g1, wAlicePi1, authz.LevelView)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof, o.GrantID = true, "planner", proto.OriginProofClaimed, g1
			})},
		{"a session name with a space", session("my planner", nil, originGrant(g1, wAlicePi1, authz.LevelControl)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof, o.GrantID = true, "my planner", proto.OriginProofClaimed, g1
			})},
		{"two direct grants permit the call: no grant is named",
			session("planner", nil, originGrant(g1, wAlicePi1, authz.LevelView), originGrant(g2, wAlicePi1, authz.LevelMessage)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof = true, "planner", proto.OriginProofClaimed
			})},
		{"of two direct grants only one reaches the target",
			session("planner", nil, originGrant(g1, wRootLab, authz.LevelControl), originGrant(g2, wAlicePi1, authz.LevelView)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof, o.GrantID = true, "planner", proto.OriginProofClaimed, g2
			})},
		{"the own Worker needs no grant: the session is named, no grant",
			session("planner", nil, originGrant(g1, wAlicePi1, authz.LevelView)), wAlicePi2,
			with(func(o *proto.Origin) { o.SourceSession, o.SourceProof = "planner", proto.OriginProofClaimed })},
		{"the session-control grant permits the call, no direct grant does",
			session("planner", allScope(), originGrant(g1, wRootLab, authz.LevelView)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof = true, "planner", proto.OriginProofClaimed
			})},
		{"a name that is no session name is left out, the call is sent",
			session("a:b", nil, originGrant(g1, wAlicePi1, authz.LevelView)), wAlicePi1,
			with(func(o *proto.Origin) { o.Grant, o.GrantID = true, g1 })},
		{"a grant id that is no UUIDv7 is left out, the call is sent",
			session("planner", nil, originGrant("grant-1", wAlicePi1, authz.LevelView)), wAlicePi1,
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof = true, "planner", proto.OriginProofClaimed
			})},
		{"a Worker identity without a calling session", authz.NewWorkerGrantPrincipal(wAlicePi2.ID, allScope()), wAlicePi1,
			with(func(o *proto.Origin) { o.Grant = true })},
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

// A user, an API key and a key bound to a Worker name no calling session and
// no grant, and no argument of the call reaches these members.
func TestAttributionMembersAreNeverTakenFromTheArguments(t *testing.T) {
	forged := map[string]any{
		"source_session": "planner", "source_proof": proto.OriginProofVerified, "grant_id": ops.NewUUIDv7(),
		"origin": map[string]any{"source_session": "planner", "source_proof": proto.OriginProofVerified, "grant_id": ops.NewUUIDv7()},
	}
	for name, p := range map[string]authz.Principal{
		"user":            routeAlice(),
		"bound key":       authz.NewAPIKeyPrincipal(idAlice, allScope(), wAlicePi2.ID).WithKeyID("k-2"),
		"Worker identity": authz.NewWorkerPrincipal(wAlicePi1.ID),
		"session":         authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "real", nil, []authz.DirectGrant{originGrant("grant-1", wAlicePi1, authz.LevelView)}),
	} {
		r := newRig(t, nil)
		st := newFakeStream(resultFrame(t, sessionsResult()))
		r.script(st)
		if _, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: forged}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := st.request(t).Origin
		wantSession, wantProof := "", ""
		if name == "session" {
			wantSession, wantProof = "real", proto.OriginProofClaimed
		}
		if got.SourceSession != wantSession || got.SourceProof != wantProof || got.GrantID != "" {
			t.Errorf("%s: origin = %+v", name, got)
		}
	}
}
