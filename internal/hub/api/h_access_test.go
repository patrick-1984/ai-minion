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
	return AccessFacts{
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
}

func accessCells(res ops.AccessMatrixResult) map[string]string {
	out := map[string]string{}
	for _, c := range res.Cells {
		s, t := res.Sources[c.Source], res.Targets[c.Target]
		v := c.InEffect
		if c.BlockedBy != "" {
			v = "blocked:" + c.BlockedBy
		}
		if c.Granted != ops.AccessLevelControl || c.Via != ops.AccessViaMaster {
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
	f.Bound = []store.BoundMasterRow{{WorkerID: "B", SessionName: "lead", Active: true}, {WorkerID: "B", SessionName: "old"}}
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
	res := ask(k.u1) // alice: her master B:lead and her Worker C only
	got := accessCells(res)
	if len(res.Sources) != 1 || res.Sources[0].WorkerID != "B" || len(got) != 3 || got["B:lead>C:c1"] != "control" {
		t.Errorf("alice sees sources %+v cells %v", res.Sources, got)
	}
	for _, tg := range res.Targets {
		if tg.Owner != "alice" {
			t.Errorf("alice sees a target of %s", tg.Owner)
		}
	}
	if res := ask(k.u2); len(res.Sources) != 0 || len(res.Cells) != 0 || res.Sources == nil {
		t.Errorf("bob sees %+v", res)
	}
}
