package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// accessFixture: root (admin) owns A, alice owns B and C, bob owns D.
// A:boss (name-only master) and B:lead (name-only master).
func accessFixture(admin, alice, bob string) AccessFacts {
	all := ops.ShareDecl{Rule: ops.ShareRule{Mode: ops.ShareModeAll}}
	f := AccessFacts{
		Workers: []AccessWorker{
			{ID: "A", Name: "a-admin", Owner: "root", OwnerID: admin, OwnerAdmin: true, Online: true, Sessions: []string{"boss"}},
			{ID: "B", Name: "b-alice", Owner: "alice", OwnerID: alice, Online: true, Sessions: []string{"lead", "b1"}},
			{ID: "C", Name: "c-alice", Owner: "alice", OwnerID: alice, Online: true, Sessions: []string{"c1", "c2", "c3"}},
			{ID: "D", Name: "d-bob", Owner: "bob", OwnerID: bob, Online: false, Sessions: []string{"d1"}},
			{ID: "E", Name: "e-bob", Owner: "bob", OwnerID: bob, Online: true, Sessions: []string{"e1"}},
		},
		Masters: []store.MasterRow{{WorkerID: "A", SessionName: "boss"}, {WorkerID: "B", SessionName: "lead"}},
		Accepts: []store.AcceptRow{
			{TargetWorkerID: "C", SourceWorkerID: "A", Sessions: []string{"c1", "c2"}}, // limited
			{TargetWorkerID: "C", SourceWorkerID: "B"},                                 // every session
			{TargetWorkerID: "E", SourceWorkerID: "A", Sessions: []string{}},           // explicit empty override
		},
		Control: map[string]store.WorkerControlRow{"D": {WorkerID: "D", AcceptAll: true}, "E": {WorkerID: "E", AcceptAll: true}},
		Shares: map[string]ops.ShareDecl{"A": all, "B": all, "D": all, "E": all,
			"C": {Rule: ops.ShareRule{Mode: ops.ShareModeSelect, Names: []string{"c1", "c3"}}}},
	}
	for i := range f.Workers {
		f.Workers[i].OwnerActive, f.Workers[i].AgentOnly = true, true
	}
	return f
}

func accessCells(res ops.AccessMatrixResult) map[string]string {
	out := map[string]string{}
	for _, c := range res.Cells {
		s, t := res.Sources[c.Source], res.Targets[c.Target]
		v := c.InEffect
		if c.BlockedBy != "" {
			v = "blocked:" + c.BlockedBy
		}
		granted := ops.AccessLevelControl
		if s.Proof == ops.AccessProofVerified {
			granted = ops.AccessLevelMessage // the catalog of a session-bound master
		}
		if c.Granted != granted || c.Via != ops.AccessViaMaster {
			v += " BAD"
		}
		out[s.WorkerID+":"+s.Session+">"+t.WorkerID+":"+t.Session] = v
	}
	return out
}

func TestAccessMatrixAppliesEveryLayer(t *testing.T) {
	got := accessCells(AccessMatrix(accessFixture("u-admin", "u-alice", "u-bob"), "u-admin", true, ""))
	want := map[string]string{
		"A:boss>C:c1": "control",                // limited acceptance, shared
		"A:boss>C:c2": "blocked:not_shared",     // accepted, not shared
		"A:boss>D:d1": "blocked:worker_offline", // accept_all, admin owner, offline
		"B:lead>C:c1": "control",                // same owner
		"B:lead>C:c2": "blocked:not_shared",
		"B:lead>C:c3": "control",
		"B:lead>D:d1": "blocked:owner_rule", // the owner rule comes first
		"B:lead>E:e1": "blocked:owner_rule",
	}
	if len(got) != len(want) {
		t.Errorf("cells = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// accept_all with an explicit empty list for A means nothing; c3 is not
	// in A's limit; a master never reaches its own Worker.
	for _, k := range []string{"A:boss>E:e1", "A:boss>C:c3", "B:lead>B:b1"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %q, want no cell", k, v)
		}
	}
}

func TestAccessMatrixPendingBoundAndFilter(t *testing.T) {
	f := accessFixture("u-admin", "u-alice", "u-bob")
	f.Workers[2].Pending = true
	f.Masters = f.Masters[:1]
	f.Bound = []store.BoundMasterRow{{WorkerID: "B", OwnerID: "u-alice", SessionName: "lead", Active: true}, {WorkerID: "B", SessionName: "old"}}
	// Without a lease for the nominated incarnation a bound master reaches nothing.
	if got := accessCells(AccessMatrix(f, "u-admin", true, "")); got["B:lead>C:c3"] != "blocked:master_not_attached" {
		t.Errorf("a bound master without a lease: %v", got)
	}
	f.Attached = map[[2]string]bool{{"B", "lead"}: true}
	res := AccessMatrix(f, "u-admin", true, "")
	got := accessCells(res)
	if got["A:boss>C:c1"] != "blocked:worker_pending" || got["B:lead>C:c3"] != "blocked:worker_pending" {
		t.Errorf("a pending target: %v", got)
	}
	var proofs []string
	for _, s := range res.Sources {
		proofs = append(proofs, s.Session+"="+s.Proof)
	}
	if strings.Join(proofs, ",") != "boss=claimed,lead=verified" {
		t.Errorf("sources = %v (an inactive bound row is no master)", proofs)
	}
	only := AccessMatrix(f, "u-admin", true, "D")
	for k := range accessCells(only) {
		if !strings.Contains(k, ">D:") {
			t.Errorf("filter on D kept %s", k)
		}
	}
	if len(only.Cells) != 2 {
		t.Errorf("filter on D: %d cells, want 2", len(only.Cells))
	}
}

func TestAccessMatrixOpShowsAUserOnlyTheirOwnWorkers(t *testing.T) {
	k := newKindStack(t)
	cat := ops.NewRegistry()
	op, ok := ops.Lookup(ops.OpAccessMatrix)
	if !ok {
		t.Fatal("ops.Default lacks access_matrix")
	}
	cat.Register(op)
	handlers := NewRegistry(cat)
	facts := accessFixture(k.admin, k.u1, k.u2)
	err := RegisterAccessMatrix(handlers, func(context.Context) (AccessFacts, error) { return facts, nil },
		func(_ context.Context, id string) (bool, error) { return id == k.admin, nil })
	if err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	ask := func(user string) ops.AccessMatrixResult {
		t.Helper()
		k.as(user)
		code, body := k.call(ops.OpAccessMatrix, map[string]any{})
		if code != http.StatusOK {
			t.Fatalf("access_matrix = %d %s", code, body)
		}
		var res ops.AccessMatrixResult
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := ask(k.admin); len(res.Sources) != 2 || len(res.Cells) != 8 {
		t.Errorf("an administrator sees %d sources, %d cells; want 2 and 8", len(res.Sources), len(res.Cells))
	}
	// alice: every pair with one of her Workers at either end; a session of
	// another owner has no name.
	res := ask(k.u1)
	if len(res.Sources) != 2 || len(res.Cells) != 7 {
		t.Errorf("alice sees sources %+v, %d cells; want 2 and 7", res.Sources, len(res.Cells))
	}
	for _, s := range res.Sources {
		if (s.WorkerID == "A") != s.Masked || s.WorkerID == "A" && s.Session != "a session of root" {
			t.Errorf("alice sees source %+v", s)
		}
	}
	for _, tg := range res.Targets {
		if (tg.Owner != "alice") != tg.Masked || tg.Masked && tg.Session != "a session of bob" {
			t.Errorf("alice sees target %+v", tg)
		}
	}
	if res := ask(k.u2); len(res.Sources) != 2 || len(res.Cells) != 3 || !res.Sources[0].Masked || !res.Sources[1].Masked {
		t.Errorf("bob sees %+v", res)
	}
}

// grantFixture adds direct grants to accessFixture: g-view (owner grant
// B:b1 > C, whole Worker), g-msg (admin grant A:helper > E:e1), g-exp
// (expired), g-sus (suspended), g-gone (granted by a former administrator),
// g-own (owner grant across owners) and g-lead (View for the master B:lead
// on C:c1, which the master reach outranks).
func grantFixture() AccessFacts {
	f := accessFixture("u-admin", "u-alice", "u-bob")
	past, at := int64(500), int64(900)
	f.Now = 1000
	f.Admins = map[string]bool{"u-admin": true}
	f.Workers[0].Sessions = []string{"boss", "helper"}
	admin := func(id, sw, ss, tw, ts string, level int) store.SessionGrantRow {
		owner := map[string]string{"A": "u-admin", "B": "u-alice", "C": "u-alice", "D": "u-bob", "E": "u-bob"}
		return store.SessionGrantRow{ID: id, SourceWorkerID: sw, SourceSession: ss, TargetWorkerID: tw, TargetSession: ts, Level: level,
			Authority: store.GrantAuthorityAdmin, GrantedBy: "u-admin", SourceOwnerID: owner[sw], TargetOwnerID: owner[tw]}
	}
	exp, sus, gone := admin("g-exp", "E", "e1", "B", "b1", 3), admin("g-sus", "E", "e1", "B", "lead", 3), admin("g-gone", "E", "e1", "C", "c1", 3)
	exp.ExpiresAt, sus.SuspendedAt, gone.GrantedBy = &past, &at, "u-former"
	owner := store.SessionGrantRow{ID: "g-view", SourceWorkerID: "B", SourceSession: "b1", TargetWorkerID: "C", Level: 1,
		Authority: store.GrantAuthorityOwner, GrantedBy: "u-alice", SourceOwnerID: "u-alice", TargetOwnerID: "u-alice"}
	cross := owner
	cross.ID, cross.TargetWorkerID, cross.TargetSession, cross.Level = "g-own", "E", "e1", 3
	lead := owner
	lead.ID, lead.SourceSession, lead.TargetSession = "g-lead", "lead", "c1"
	f.Grants = []store.SessionGrantRow{owner, admin("g-msg", "A", "helper", "E", "e1", 2), exp, sus, gone, cross, lead,
		admin("g-off", "A", "helper", "D", "", 3)}
	return f
}

func TestAccessMatrixDirectGrants(t *testing.T) {
	res := AccessMatrix(grantFixture(), "u-admin", true, "")
	got := map[string]string{}
	for _, c := range res.Cells {
		s, tg := res.Sources[c.Source], res.Targets[c.Target]
		got[s.WorkerID+":"+s.Session+">"+tg.WorkerID+":"+tg.Session] = c.Granted + "/" + c.InEffect + "/" + c.Via + "/" + c.BlockedBy
	}
	want := map[string]string{
		"B:b1>C:c1":     "view/view/g-view/",
		"B:b1>C:c2":     "view//g-view/not_shared",
		"B:b1>C:c3":     "view/view/g-view/",
		"A:helper>E:e1": "message/message/g-msg/",
		"A:helper>D:d1": "control//g-off/worker_offline",
		"E:e1>B:b1":     "control//g-exp/expired",
		"E:e1>B:lead":   "control//g-sus/suspended",
		"E:e1>C:c1":     "control//g-gone/authority",
		"B:b1>E:e1":     "control//g-own/authority",
		"B:lead>C:c1":   "control/control/master/", // the master outranks g-lead
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, s := range res.Sources {
		if s.Proof != ops.AccessProofClaimed || s.Master != (s.Session == "boss" || s.Session == "lead") {
			t.Errorf("source %+v", s)
		}
	}
	// bob owns E and D: he sees the pairs that touch them, the other side's
	// session without its name.
	for _, s := range AccessMatrix(grantFixture(), "u-bob", false, "").Sources {
		if s.Owner != "bob" && (!s.Masked || strings.Contains(s.Session, "helper") || strings.Contains(s.Session, "b1")) {
			t.Errorf("bob sees source %+v", s)
		}
	}
}

// The matrix and the explainer are one evaluator: for every source and every
// live session of every Worker they agree, also where there is no cell.
func TestAccessExplainAgreesWithTheMatrix(t *testing.T) {
	f := grantFixture()
	res := AccessMatrix(f, "u-admin", true, "")
	cells := map[[4]string]ops.AccessCell{}
	for _, c := range res.Cells {
		s, tg := res.Sources[c.Source], res.Targets[c.Target]
		cells[[4]string{s.WorkerID, s.Session, tg.WorkerID, tg.Session}] = c
	}
	if len(cells) < 15 {
		t.Fatalf("the fixture has %d cells", len(cells))
	}
	for _, s := range res.Sources {
		for _, w := range f.Workers {
			for _, name := range w.Sessions {
				a := ops.AccessExplainArgs{SourceWorkerID: s.WorkerID, SourceSession: s.Session, TargetWorkerID: w.ID, TargetSession: name}
				ex, ok := AccessExplain(f, "u-admin", true, a)
				c := cells[[4]string{s.WorkerID, s.Session, w.ID, name}]
				if !ok || ex.InEffect != c.InEffect || ex.Via != c.Via {
					t.Errorf("%+v: explain %q via %q, matrix %q via %q", a, ex.InEffect, ex.Via, c.InEffect, c.Via)
				}
				fails, skipped := 0, false
				for i, ch := range ex.Checks {
					switch ch.State {
					case ops.AccessCheckFail:
						fails++
					case ops.AccessCheckSkip:
						skipped = true
					case ops.AccessCheckPass:
						if fails > 0 {
							t.Errorf("%+v: check %s passes after a fail", a, ch.ID)
						}
					}
					if i == len(ex.Checks)-1 && (ch.ID != ops.AccessCheckPreviewOnly || ch.State != ops.AccessCheckUnknown) {
						t.Errorf("%+v: last check %+v", a, ch)
					}
				}
				for _, ch := range ex.Checks {
					if ch.Code == "" || ch.Fix != "" && (ch.State != ops.AccessCheckFail || !strings.HasPrefix(ch.Fix, "/")) {
						t.Errorf("%+v: check without a code or with a fix it must not have: %+v", a, ch)
					}
				}
				if len(ex.Checks) != 7 || fails > 1 || (fails == 0) != (ex.InEffect != "") || skipped && fails == 0 {
					t.Errorf("%+v: in effect %q with checks %+v", a, ex.InEffect, ex.Checks)
				}
			}
		}
	}
	ex, _ := AccessExplain(f, "u-admin", true, ops.AccessExplainArgs{SourceWorkerID: "E", SourceSession: "e1", TargetWorkerID: "B", TargetSession: "b1"})
	if ex.Checks[3].ID != ops.AccessCheckTime || ex.Checks[3].Text != "the grant has expired" || ex.Checks[4].State != ops.AccessCheckSkip {
		t.Errorf("an expired grant: %+v", ex.Checks)
	}
	if ex.Checks[3].Code != ops.AccessBlockedExpired || ex.Checks[3].Fix != "/access/grants" || ex.Checks[4].Code != ops.AccessCodeSkipped {
		t.Errorf("codes of an expired grant: %+v", ex.Checks)
	}
	if _, ok := AccessExplain(f, "u-bob", false, ops.AccessExplainArgs{SourceWorkerID: "B", SourceSession: "b1", TargetWorkerID: "C", TargetSession: "c1"}); ok {
		t.Error("bob is told about a pair between alice's Workers")
	}
	// Bob owns the target only: the covered pair is not explained to him
	// (its source is masked in his matrix), the same as a guessed name.
	for _, ss := range []string{"helper", "nope"} {
		if ex, ok := AccessExplain(f, "u-bob", false, ops.AccessExplainArgs{SourceWorkerID: "A", SourceSession: ss, TargetWorkerID: "E", TargetSession: "e1"}); ok {
			t.Errorf("bob asks about %s -> his own target: %+v", ss, ex)
		}
	}
}

// The read model takes proof and level from the route that authorizes a
// pair: a grant bound to one terminal incarnation is served by the master
// broker while that incarnation holds a lease (at most Message), and a grant
// by name stays Claimed although its source session is a session-bound
// master.
func TestAccessRouteDecidesProofAndLevel(t *testing.T) {
	f := accessFixture("u-admin", "u-alice", "u-bob")
	f.Now = 1000
	f.Admins = map[string]bool{"u-admin": true}
	f.Masters = f.Masters[:1] // B:lead becomes a session-bound master
	f.Bound = []store.BoundMasterRow{{WorkerID: "B", OwnerID: "u-alice", SessionName: "lead", Active: true}}
	f.Attached = map[[2]string]bool{{"B", "lead"}: true}
	verified := store.SessionGrantRow{ID: "g-ver", SourceWorkerID: "B", SourceSession: "b1", SourceIncarnation: "inc-1",
		TargetWorkerID: "C", TargetSession: "c1", Level: 1, Authority: store.GrantAuthorityOwner, GrantedBy: "u-alice",
		SourceOwnerID: "u-alice", TargetOwnerID: "u-alice"}
	wide := verified
	wide.ID, wide.TargetSession, wide.Level = "g-ver-c3", "c3", 3
	claimed := store.SessionGrantRow{ID: "g-claim", SourceWorkerID: "B", SourceSession: "lead", TargetWorkerID: "E", TargetSession: "e1",
		Level: 1, Authority: store.GrantAuthorityAdmin, GrantedBy: "u-admin", SourceOwnerID: "u-alice", TargetOwnerID: "u-bob"}
	f.Grants = []store.SessionGrantRow{verified, wide, claimed}

	cells := func(f AccessFacts) (map[string]string, map[string]string) {
		res := AccessMatrix(f, "u-admin", true, "")
		got, proofs := map[string]string{}, map[string]string{}
		for _, c := range res.Cells {
			s, tg := res.Sources[c.Source], res.Targets[c.Target]
			got[s.WorkerID+":"+s.Session+">"+tg.WorkerID+":"+tg.Session] = c.Granted + "/" + c.InEffect + "/" + c.Via + "/" + c.BlockedBy
		}
		for _, s := range res.Sources {
			proofs[s.WorkerID+":"+s.Session] = s.Proof
		}
		return got, proofs
	}
	explain := func(f AccessFacts, ss, tw, ts string) ops.AccessExplainResult {
		t.Helper()
		ex, ok := AccessExplain(f, "u-admin", true, ops.AccessExplainArgs{SourceWorkerID: "B", SourceSession: ss, TargetWorkerID: tw, TargetSession: ts})
		if !ok {
			t.Fatalf("explain B:%s > %s:%s: no answer", ss, tw, ts)
		}
		return ex
	}
	callerCode := func(ex ops.AccessExplainResult) string {
		for _, c := range ex.Checks {
			if c.ID == ops.AccessCheckCaller {
				return c.Code
			}
		}
		return ""
	}

	// No lease for the incarnation: the verified grant is not in effect.
	got, _ := cells(f)
	if got["B:b1>C:c1"] != "view//g-ver/source_incarnation" {
		t.Errorf("verified grant without a lease: %v", got)
	}
	// The incarnation holds a lease: View in effect, Verified, and a Control
	// grant gives what the master catalog serves (Message).
	f.Leases = map[[3]string]bool{{"B", "b1", "inc-1"}: true}
	got, proofs := cells(f)
	if got["B:b1>C:c1"] != "view/view/g-ver/" || got["B:b1>C:c3"] != "message/message/g-ver-c3/" || proofs["B:b1"] != ops.AccessProofVerified {
		t.Errorf("verified grant with a lease: %v %v", got, proofs)
	}
	if ex := explain(f, "b1", "C", "c1"); ex.InEffect != ops.AccessLevelView || ex.Source.Proof != ops.AccessProofVerified ||
		callerCode(ex) != ops.AccessCodeCallerVerified {
		t.Errorf("explain of a verified grant: %+v", ex)
	}
	// A lease of another incarnation of the same session does not help.
	f.Leases = map[[3]string]bool{{"B", "b1", "inc-2"}: true}
	if got, _ := cells(f); got["B:b1>C:c1"] != "view//g-ver/source_incarnation" {
		t.Errorf("verified grant with another incarnation's lease: %v", got)
	}

	// The grant by name from the session-bound master B:lead to E (whose
	// owner rule refuses master reach) is Claimed; the master's own reach
	// to C stays Verified; the row holds both, so it is Claimed.
	got, proofs = cells(f)
	if got["B:lead>E:e1"] != "view/view/g-claim/" || got["B:lead>C:c1"] != "message/message/master/" {
		t.Errorf("claimed grant from a bound master: %v", got)
	}
	if proofs["B:lead"] != ops.AccessProofClaimed {
		t.Errorf("row of a source with a claimed route: %v", proofs)
	}
	if ex := explain(f, "lead", "E", "e1"); ex.Via != "g-claim" || ex.Source.Proof != ops.AccessProofClaimed ||
		callerCode(ex) != ops.AccessCodeCallerClaimed {
		t.Errorf("explain of a claimed grant from a bound master: %+v", ex)
	}
	if ex := explain(f, "lead", "C", "c1"); ex.Via != ops.AccessViaMaster || ex.Source.Proof != ops.AccessProofVerified ||
		callerCode(ex) != ops.AccessCodeCallerVerified {
		t.Errorf("explain of the bound master's reach: %+v", ex)
	}
}
