package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type accessUsers struct{ st *store.Store }

func (u accessUsers) User(ctx context.Context, id string) (authz.UserState, error) {
	row, err := u.st.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return authz.UserState{}, authz.ErrNotFound
	}
	return authz.UserState{Role: authz.Role(row.Role), Active: row.Active}, err
}

type accessOwners struct{ st *store.Store }

func (o accessOwners) WorkerOwner(ctx context.Context, id string) (string, error) {
	owner, err := o.st.WorkerOwner(ctx, id)
	if errors.Is(err, store.ErrWorkerNotFound) {
		return "", authz.ErrNotFound
	}
	return owner, err
}

// accessFleet is a fixture fleet in a temporary store.
type accessFleet struct {
	t    *testing.T
	st   *store.Store
	az   *authz.Authorizer
	id   map[string]string // letter -> Worker id
	own  map[string]string // letter -> owner
	live AccessLive
}

func (f *accessFleet) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.st.DB().ExecContext(f.t.Context(), q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *accessFleet) grant(source, session, target, name string, level int, by string) string {
	f.t.Helper()
	g := store.SessionGrantRow{SourceWorkerID: f.id[source], SourceSession: session, TargetWorkerID: f.id[target], TargetSession: name,
		Level: level, Authority: store.GrantAuthorityAdmin, GrantedBy: by, SourceOwnerID: f.own[source], TargetOwnerID: f.own[target]}
	if by == f.own[source] && by == f.own[target] {
		g.Authority = store.GrantAuthorityOwner
	}
	out, err := f.st.PutSessionGrant(f.t.Context(), g, 0, store.GrantWrite{ActorID: by})
	if err != nil {
		f.t.Fatalf("grant %s:%s > %s:%s: %v", source, session, target, name, err)
	}
	return out.ID
}

func newAccessFleet(t *testing.T) *accessFleet {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &accessFleet{t: t, st: st, az: authz.New(accessUsers{st}, accessOwners{st}), id: map[string]string{}, own: map[string]string{},
		live: AccessLive{Online: map[string]bool{}, AgentOnly: map[string]bool{}, Sessions: map[string][]string{}, Attached: map[[2]string]bool{}}}
	ctx := t.Context()
	for _, u := range []struct{ id, role string }{{"root", "admin"}, {"dave", "admin"}, {"alice", "user"}, {"bob", "user"}, {"carol", "user"}} {
		f.exec(`INSERT INTO users(id,username,created_at,updated_at) VALUES(?,?,1,1)`, u.id, u.id)
		f.exec(`INSERT INTO user_roles(user_id,role,source) VALUES(?,?,'local')`, u.id, u.role)
	}
	sessions := map[string][]string{"A": {"boss", "helper"}, "B": {"lead", "b1"}, "C": {"c1", "c2", "c3"}, "D": {"d1"},
		"E": {"e1", "e2"}, "F": {"f1"}, "G": {"g1"}, "H": {"hv", "h2"}}
	owners := map[string]string{"A": "root", "B": "alice", "C": "alice", "D": "bob", "E": "bob", "F": "carol", "G": "alice", "H": "alice"}
	for i, l := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		w := store.WorkerRow{ID: ops.NewUUIDv7(), Pub: bytes.Repeat([]byte{byte(i + 1)}, 32), Name: "w-" + l, OwnerID: owners[l],
			Kind: store.WorkerKindPrivate, CreatedAt: 1000}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		f.id[l], f.own[l] = w.ID, owners[l]
		f.live.Sessions[w.ID], f.live.Online[w.ID], f.live.AgentOnly[w.ID] = sessions[l], l != "D", true
		share := ops.ShareRule{Mode: ops.ShareModeAll}
		if l == "C" {
			share = ops.ShareRule{Mode: ops.ShareModeSelect, Names: []string{"c1", "c3"}}
		}
		if err := st.SaveWorkerShare(ctx, w.ID, ops.ShareDecl{Rule: share, Revision: 1}); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`UPDATE workers SET approval='approved'`)
	// Master sessions and what accepts them.
	for _, m := range [][2]string{{"A", "boss"}, {"B", "lead"}, {"F", "f1"}} {
		if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: f.id[m[0]], SessionName: m[1], CreatedBy: "root", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	accept := func(target, source string, names []string) {
		if err := st.PutAccept(ctx, store.AcceptRow{TargetWorkerID: f.id[target], SourceWorkerID: f.id[source], Sessions: names, CreatedBy: "root", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	accept("C", "A", []string{"c1", "c2"})
	accept("C", "B", nil)
	accept("E", "A", []string{})
	accept("C", "F", nil)
	accept("C", "H", nil)
	accept("B", "H", []string{"b1"})
	all := true
	for _, l := range []string{"D", "E"} {
		if err := st.SetWorkerControl(ctx, f.id[l], nil, &all); err != nil {
			t.Fatal(err)
		}
	}
	// Session-bound masters on H: hv holds a lease, h2 does not.
	for _, name := range []string{"hv", "h2"} {
		f.exec(`INSERT INTO bound_master_session(worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,changed_at)
			VALUES(?,?,?,?,?,1,1,1)`, f.id["H"], "alice", name, "inc-"+name, bytes.Repeat([]byte{8}, 32))
	}
	f.live.Attached[[2]string{f.id["H"], "hv"}] = true
	// Direct grants.
	f.grant("B", "b1", "C", "", 1, "alice")
	f.grant("A", "helper", "E", "e1", 2, "root")
	expired := f.grant("E", "e1", "B", "b1", 3, "root")
	suspended := f.grant("E", "e1", "B", "lead", 3, "root")
	f.grant("E", "e1", "C", "c1", 3, "dave")
	f.grant("F", "f1", "C", "c1", 1, "root")
	f.grant("B", "b1", "E", "", 3, "root")
	f.grant("B", "lead", "C", "c1", 1, "alice")
	f.grant("A", "helper", "D", "d1", 3, "root")
	bound := f.grant("B", "b1", "H", "h2", 1, "alice")
	f.grant("A", "boss", "B", "b1", 1, "root")
	f.grant("G", "g1", "C", "c1", 2, "alice")
	f.grant("C", "c1", "B", "", 2, "alice")
	f.exec(`UPDATE session_grant SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).UnixMilli(), expired)
	f.exec(`UPDATE session_grant SET suspended_at = 5, suspend_cause = 'test' WHERE id = ?`, suspended)
	f.exec(`UPDATE session_grant SET source_incarnation = 'inc-x' WHERE id = ?`, bound)
	// What changed after the grants were decided: dave is no administrator
	// any more, carol is deactivated, G waits for approval, E has dispatched
	// a restriction that drops e2.
	f.exec(`DELETE FROM user_roles WHERE user_id = 'dave' AND role = 'admin'`)
	f.exec(`INSERT INTO user_roles(user_id,role,source) VALUES('dave','user','local')`)
	// The trigger suspended dave's grant; without the suspension the lost
	// authority alone must still block it.
	f.exec(`UPDATE session_grant SET suspended_at = NULL, suspend_cause = '' WHERE granted_by = 'dave'`)
	f.exec(`UPDATE users SET active = 0 WHERE id = 'carol'`)
	f.exec(`UPDATE workers SET approval = 'pending' WHERE id = ?`, f.id["G"])
	if err := st.SavePendingShare(ctx, store.PendingShare{Seq: 9, WorkerID: f.id["E"], Rule: ops.ShareRule{Mode: ops.ShareModeSelect, Names: []string{"e1"}}, BaseRevision: 1}); err != nil {
		t.Fatal(err)
	}
	return f
}

// level is what authz.Decide answers for p on one session, as a level.
func (f *accessFleet) level(p authz.Principal, worker, session string) string {
	f.t.Helper()
	res := authz.SessionResource{WorkerID: worker, Name: session}
	allowed := func(a ops.Action) bool {
		err := f.az.Decide(f.t.Context(), p, ops.MustActionSet(a), res)
		var pe *proto.Error
		if err != nil && !errors.As(err, &pe) {
			f.t.Fatal(err)
		}
		return err == nil
	}
	read, message, operate := allowed(ops.ActionRead), allowed(ops.ActionMessage), allowed(ops.ActionOperate)
	switch {
	case read && message && operate:
		return ops.AccessLevelControl
	case read && message && !operate:
		return ops.AccessLevelMessage
	case read && !message && !operate:
		return ops.AccessLevelView
	case !read && !message && !operate:
		return ""
	}
	f.t.Errorf("%s:%s is no level: read %v message %v operate %v", worker, session, read, message, operate)
	return "?"
}

// principal builds the principal of a calling session as the call server
// does (workers.CallServer.authenticate): the session-control grant of a
// master session as scope, and the direct grants read by name.
func (f *accessFleet) principal(worker, session string) authz.Principal {
	f.t.Helper()
	ctx := f.t.Context()
	var legacy authz.Scope
	row, err := f.st.SessionControl(ctx, worker, session)
	switch {
	case err == nil:
		var targets []ops.KeyResource
		if err := json.Unmarshal([]byte(row.TargetsJSON), &targets); err != nil {
			f.t.Fatal(err)
		}
		legacy = apikey.NewScope(targets, authz.GrantActions())
	case !errors.Is(err, store.ErrNotFound):
		f.t.Fatal(err)
	}
	rows, err := f.st.SessionGrantsFor(ctx, worker, session)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(rows) == 0 {
		if legacy == nil {
			return authz.NewWorkerPrincipal(worker)
		}
		return authz.NewWorkerGrantPrincipal(worker, legacy)
	}
	direct := make([]authz.DirectGrant, 0, len(rows))
	for _, g := range rows {
		d := authz.DirectGrant{ID: g.ID, TargetWorkerID: g.TargetWorkerID, TargetSession: g.TargetSession, Level: g.Level,
			Authority: g.Authority, GrantedBy: g.GrantedBy, SourceOwnerID: g.SourceOwnerID, TargetOwnerID: g.TargetOwnerID,
			Suspended: g.SuspendedAt != nil}
		if g.ExpiresAt != nil {
			d.ExpiresAt = authz.ExpiryAt(time.UnixMilli(*g.ExpiresAt))
		}
		direct = append(direct, d)
	}
	return authz.NewWorkerSessionPrincipal(worker, session, legacy, direct)
}

// boundLevel is what the master broker gives a session-bound master that
// holds a lease: the store admission of the destination (BoundMasterTargets)
// and the decision on its scope, within the master catalog (Message).
func (f *accessFleet) boundLevel(b store.BoundMasterRow, target, owner, session string) string {
	f.t.Helper()
	targets, err := f.st.BoundMasterTargets(f.t.Context(), b, &store.MasterTarget{WorkerID: target, OwnerID: owner, Sessions: []string{session}})
	if errors.Is(err, store.ErrMasterCandidate) || errors.Is(err, store.ErrMasterRevision) || errors.Is(err, store.ErrWorkerOwnerChanged) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	l := f.level(authz.NewBoundMasterPrincipal(b.WorkerID, apikey.NewScope(targets, authz.GrantActions())), target, session)
	if accessRank(l) > accessRank(ops.AccessLevelMessage) {
		l = ops.AccessLevelMessage
	}
	return l
}

// The property of the read model: for every session of the fleet as a source
// and every session of every other Worker as a target, the level the matrix
// shows in effect is the level the Hub decides for the calling session.
// A layer of the Workers (approval, sharing, connection) is not part of that
// decision: such a cell is in effect for nothing, and the decision gives what
// the cell says is granted.
func TestAccessMatrixAgreesWithTheDecision(t *testing.T) {
	f := newAccessFleet(t)
	ctx := t.Context()
	snap, err := f.st.AccessSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facts := AccessFactsFrom(snap, f.live)
	res := AccessMatrix(facts, "root", true, "")
	cells := map[[4]string]ops.AccessCell{}
	for _, c := range res.Cells {
		s, tg := res.Sources[c.Source], res.Targets[c.Target]
		cells[[4]string{s.WorkerID, s.Session, tg.WorkerID, tg.Session}] = c
	}
	bound := map[[2]string]store.BoundMasterRow{}
	for _, b := range snap.Bound {
		bound[[2]string{b.WorkerID, b.SessionName}] = b
	}
	name := map[string]string{}
	for l, id := range f.id {
		name[id] = l
	}
	pairs, inEffect, blocked := 0, map[string]int{}, map[string]int{}
	for _, src := range snap.Workers {
		for _, session := range f.live.Sessions[src.ID] {
			p := f.principal(src.ID, session)
			b, verified := bound[[2]string{src.ID, session}]
			for _, tg := range snap.Workers {
				if tg.ID == src.ID {
					continue
				}
				for _, target := range f.live.Sessions[tg.ID] {
					pairs++
					pair := name[src.ID] + ":" + session + " > " + name[tg.ID] + ":" + target
					direct := f.level(p, tg.ID, target)
					want := direct
					if verified && f.live.Attached[[2]string{src.ID, session}] {
						if l := f.boundLevel(b, tg.ID, tg.OwnerID, target); accessRank(l) > accessRank(want) {
							want = l
						}
					}
					c, has := cells[[4]string{src.ID, session, tg.ID, target}]
					ex, ok := AccessExplain(facts, "root", true, ops.AccessExplainArgs{SourceWorkerID: src.ID, SourceSession: session,
						TargetWorkerID: tg.ID, TargetSession: target})
					if !ok || ex.InEffect != c.InEffect {
						t.Errorf("%s: explain says %q, the matrix %q", pair, ex.InEffect, c.InEffect)
					}
					switch c.BlockedBy {
					case ops.AccessBlockedWorkerPending, ops.AccessBlockedNotShared, ops.AccessBlockedWorkerOffline:
						blocked[c.BlockedBy]++
						if c.InEffect != "" {
							t.Errorf("%s: in effect %q although blocked by %s", pair, c.InEffect, c.BlockedBy)
						}
						if !verified && direct != c.Granted {
							t.Errorf("%s: blocked by %s with %q granted, the decision gives %q", pair, c.BlockedBy, c.Granted, direct)
						}
					default:
						if has {
							blocked[c.BlockedBy]++
						}
						if c.InEffect != want {
							t.Errorf("%s: the matrix shows %q in effect (cell %v, blocked by %q, via %q), the decision gives %q",
								pair, c.InEffect, has, c.BlockedBy, c.Via, want)
						}
						inEffect[c.InEffect]++
					}
				}
			}
		}
	}
	// The fixture must exercise every level and every layer, or the property
	// proves nothing.
	for _, l := range []string{ops.AccessLevelView, ops.AccessLevelMessage, ops.AccessLevelControl} {
		if inEffect[l] == 0 {
			t.Errorf("no pair is in effect at %s: %v", l, inEffect)
		}
	}
	for _, b := range []string{ops.AccessBlockedSuspended, ops.AccessBlockedExpired, ops.AccessBlockedAuthority, ops.AccessBlockedOwnerRule,
		ops.AccessBlockedOwnerInactive, ops.AccessBlockedSourceIncarnation, ops.AccessBlockedMasterNotAttached,
		ops.AccessBlockedWorkerPending, ops.AccessBlockedNotShared, ops.AccessBlockedWorkerOffline} {
		if blocked[b] == 0 {
			t.Errorf("no cell is blocked by %s: %v", b, blocked)
		}
	}
	if pairs < 150 {
		t.Errorf("only %d pairs compared", pairs)
	}
}

// A viewer who does not own a side learns nothing from a guessed session name
// there: a session that exists and one that does not are answered the same,
// and so is a guessed master session.
func TestAccessExplainAnswersForeignGuessesAlike(t *testing.T) {
	f := grantFixture()
	ask := func(viewer, sw, ss, tw, ts string) (ops.AccessExplainResult, bool) {
		return AccessExplain(f, viewer, false, ops.AccessExplainArgs{SourceWorkerID: sw, SourceSession: ss, TargetWorkerID: tw, TargetSession: ts})
	}
	// alice owns B and C; D and E are bob's, A is root's.
	for _, q := range [][4]string{
		{"B", "b1", "D", "d1"},     // exists, nothing covers it
		{"B", "b1", "D", "nope"},   // does not exist
		{"B", "b1", "E", "nope"},   // a grant names e1 only
		{"A", "boss", "B", "b1"},   // a master of another owner without reach here
		{"A", "helper", "B", "b1"}, // a session of another owner
		{"A", "nope", "B", "b1"},
	} {
		if res, ok := ask("u-alice", q[0], q[1], q[2], q[3]); ok {
			t.Errorf("alice asks %v and is answered: %+v", q, res)
		}
	}
	// A pair of her matrix is answered, the other side without its name.
	res, ok := ask("u-alice", "A", "boss", "C", "c1")
	if !ok || !res.Source.Masked || res.Source.Session != "a session of root" || res.InEffect != ops.AccessLevelControl {
		t.Errorf("alice asks about a pair of her matrix: %+v %v", res, ok)
	}
	// Between her own Workers every answer is given, with the reason.
	res, ok = ask("u-alice", "B", "b1", "C", "nope")
	if !ok || res.Checks[1].Code != ops.AccessCodeNoSuchSession || res.Checks[1].State != ops.AccessCheckFail {
		t.Errorf("alice asks about her own Workers: %+v %v", res, ok)
	}
	res, ok = ask("u-alice", "B", "b1", "C", "c2")
	if !ok || res.Checks[4].Code != ops.AccessBlockedNotShared || res.Checks[4].Fix != "/workers/C" {
		t.Errorf("an unshared session of her own Worker: %+v %v", res, ok)
	}
}

// A Message level needs a target that takes prompts for agent panes only.
func TestAccessMessageWorksAsViewWithoutAgentOnly(t *testing.T) {
	f := grantFixture()
	f.Workers[4].AgentOnly = false // E
	res, ok := AccessExplain(f, "u-admin", true, ops.AccessExplainArgs{SourceWorkerID: "A", SourceSession: "helper", TargetWorkerID: "E", TargetSession: "e1"})
	if !ok || res.InEffect != ops.AccessLevelView || res.Checks[5].Code != ops.AccessCodeMessageAsView || res.Checks[5].State != ops.AccessCheckPass {
		t.Errorf("message without agent_only: %+v", res)
	}
}
