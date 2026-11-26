package workers

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
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

// admittedOriginOfCall routes a sessions_list of p to worker as the grant
// call paths do (the admission reads the grants that authorize it, for the
// caller's audit) with the calling session src, and returns the origin sent.
func admittedOriginOfCall(t *testing.T, p authz.Principal, worker WorkerRecord, src originSource) (proto.Origin, error) {
	t.Helper()
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionsList, WorkerID: worker.ID,
		Args: ops.SessionsListArgs{}, granted: func([]string) {}, source: src})
	if err != nil {
		return proto.Origin{}, err
	}
	return st.request(t).Origin, nil
}

// The Hub names the calling session and the grant in the origin of a call that
// a session principal makes (story W1). Both come from the principal and the
// admission.
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
			got, err := admittedOriginOfCall(t, tc.p, tc.target, originSource{})
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

// The calling session and its proof come from the admission path, whatever
// authorizes the call: a bound master lease is verified, a session named on a
// call stream (legacy session control included) is claimed (review r1).
func TestOriginNamesTheSourceTheAdmissionEstablished(t *testing.T) {
	g1 := ops.NewUUIDv7()
	src := proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: wAlicePi2.ID, SourceWorkerFP: wAlicePi2.Fingerprint, SourceWorkerOwnerID: idAlice}
	with := func(f func(*proto.Origin)) proto.Origin { o := src; f(&o); return o }
	tests := []struct {
		name string
		p    authz.Principal
		src  originSource
		want proto.Origin
	}{
		{"a nomination-only master: the bound master principal keeps no session",
			authz.NewBoundMasterPrincipal(wAlicePi2.ID, allScope()), verifiedSource("planner"),
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof = true, "planner", proto.OriginProofVerified
			})},
		{"a master under a verified grant",
			authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "planner", nil, []authz.DirectGrant{originGrant(g1, wAlicePi1, authz.LevelView)}),
			verifiedSource("planner"),
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof, o.GrantID = true, "planner", proto.OriginProofVerified, g1
			})},
		{"legacy session control: the grant principal keeps no session",
			authz.NewWorkerGrantPrincipal(wAlicePi2.ID, allScope()), claimedSource("boss"),
			with(func(o *proto.Origin) {
				o.Grant, o.SourceSession, o.SourceProof = true, "boss", proto.OriginProofClaimed
			})},
		{"the own Worker under the own-Worker rule, not a grant",
			authz.NewWorkerPrincipal(wAlicePi2.ID), claimedSource("boss"),
			with(func(o *proto.Origin) { o.SourceSession, o.SourceProof = "boss", proto.OriginProofClaimed })},
		{"a source that is no session name is left out",
			authz.NewBoundMasterPrincipal(wAlicePi2.ID, allScope()), verifiedSource("a:b"),
			with(func(o *proto.Origin) { o.Grant = true })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := wAlicePi1
			if tc.p.Kind == authz.KindWorker && tc.p.Scope == nil && len(tc.p.DirectGrants()) == 0 {
				target = wAlicePi2 // a plain Worker identity reaches its own Worker only
			}
			got, err := admittedOriginOfCall(t, tc.p, target, tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("origin = %+v\nwant     %+v", got, tc.want)
			}
		})
	}
}

// countingUsers is a user lookup that counts its reads; once fail is set,
// every read fails.
type countingUsers struct {
	fakeUsers
	reads atomic.Int64
	fail  atomic.Bool
}

func (u *countingUsers) User(ctx context.Context, id string) (authz.UserState, error) {
	u.reads.Add(1)
	if u.fail.Load() {
		return authz.UserState{}, errors.New("the store is gone")
	}
	return u.fakeUsers.User(ctx, id)
}

// withUsers decides with the user lookup users.
func withUsers(users authz.UserLookup) func(*RouterConfig) {
	return func(c *RouterConfig) { c.Authz = authz.New(users, c.Directory.(*fakeDir)) }
}

// The grant id of the origin is the one the admission read for the caller's
// audit: naming it reads nothing more, so a failing read cannot refuse a call
// that the admission allowed (review r1).
func TestOriginGrantNeedsNoLookupOfItsOwn(t *testing.T) {
	g1 := ops.NewUUIDv7()
	reads := func(p authz.Principal, granted func([]string)) (int64, proto.Origin) {
		t.Helper()
		users := &countingUsers{fakeUsers: fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}}
		r := newRig(t, withUsers(users))
		st := newFakeStream(resultFrame(t, sessionsResult()))
		r.script(st)
		if _, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID,
			Args: ops.SessionsListArgs{}, granted: granted}); err != nil {
			t.Fatal(err)
		}
		return users.reads.Load(), st.request(t).Origin
	}
	direct := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "planner", nil, []authz.DirectGrant{originGrant(g1, wAlicePi1, authz.LevelView)})
	legacy := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "planner", allScope(), nil)
	nDirect, o := reads(direct, nil)
	nLegacy, _ := reads(legacy, nil)
	if nDirect != nLegacy || o.GrantID != "" || !o.Grant {
		t.Fatalf("without the admission's grants: %d reads (legacy %d), origin %+v; want no lookup and no grant id", nDirect, nLegacy, o)
	}
	var told []string
	nAdmitted, o := reads(direct, func(ids []string) { told = ids })
	if nAdmitted != nDirect+1 || o.GrantID != g1 || len(told) != 1 || told[0] != g1 {
		t.Fatalf("with the admission's grants: %d reads (want %d), origin %+v, told %v", nAdmitted, nDirect+1, o, told)
	}
}

// At the final admission the origin names the grant that admission found, and
// a read that fails after it changes nothing of the request.
func TestOriginNamesTheGrantOfTheFinalAdmission(t *testing.T) {
	g1, g2 := ops.NewUUIDv7(), ops.NewUUIDv7()
	users := &countingUsers{fakeUsers: fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}}
	r := newRig(t, withUsers(users))
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	first := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "planner", nil, []authz.DirectGrant{originGrant(g1, wAlicePi1, authz.LevelView)})
	final := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "planner", nil, []authz.DirectGrant{originGrant(g2, wAlicePi1, authz.LevelView)})
	var told []string
	req := CallRequest{Principal: first, Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{},
		granted: func(ids []string) { told = ids }, source: verifiedSource("planner"),
		admit: func(context.Context, Online) (authz.Principal, error) { return final, nil }}
	if _, err := r.rt.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := st.request(t)
	if got.Origin.GrantID != g2 || len(told) != 1 || told[0] != g2 || got.Origin.SourceProof != proto.OriginProofVerified {
		t.Fatalf("origin = %+v, told %v; want the grant of the final admission", got.Origin, told)
	}
	if err := got.Origin.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The Router names the Worker it authorized and routes to in the op.request
// frame, outside the arguments: a sessions_list or session_create whose
// arguments name no Worker still lets the Worker check it (review r1).
func TestRouterFrameNamesTheDestinationOutsideTheArguments(t *testing.T) {
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, sessionsResult()))
	r.script(st)
	if _, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID,
		Args: ops.SessionsListArgs{}}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	raw := append(json.RawMessage(nil), st.written[0]...)
	st.mu.Unlock()
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	var dest string
	if err := json.Unmarshal(frame["target_worker_id"], &dest); err != nil || dest != wAlicePi1.ID {
		t.Fatalf("target_worker_id = %s, %v; want the routed Worker", frame["target_worker_id"], err)
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(frame["args"], &args); err != nil {
		t.Fatal(err)
	}
	if _, ok := args["worker_id"]; ok {
		t.Fatalf("the destination was put into the arguments: %s", frame["args"])
	}
}
