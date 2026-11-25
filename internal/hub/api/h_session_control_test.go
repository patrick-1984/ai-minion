package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// newSessionControlStack is the kind stack with only the session-control
// operations registered over the real store and authorizer.
func newSessionControlStack(t *testing.T) *kindStack {
	t.Helper()
	return newControlStack(t, func(st *store.Store) MinionControlStore { return st })
}

// newControlStack is newSessionControlStack with the store of the Minion
// control handlers wrapped (a test injects a failing read there).
func newControlStack(t *testing.T, wrap func(*store.Store) MinionControlStore) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpSessionControlList, ops.OpSessionControlSet, ops.OpControlList, ops.OpControlSetWorker, ops.OpControlSetMaster, ops.OpControlSetAccept, ops.OpControlCheck} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	handlers := NewRegistry(cat)
	if err := RegisterSessionControl(handlers, k.st); err != nil {
		t.Fatal(err)
	}
	list := func(ctx context.Context) ([]ControlWorkerBase, error) {
		rows, err := k.st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ControlWorkerBase, 0, len(rows))
		for _, w := range rows {
			out = append(out, ControlWorkerBase{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, CanBeMaster: true})
		}
		return out, nil
	}
	if err := RegisterMinionControl(handlers, wrap(k.st), list); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *kindStack) grants() []ops.SessionControlInfo {
	k.t.Helper()
	code, body := k.call(ops.OpSessionControlList, map[string]any{})
	if code != http.StatusOK {
		k.t.Fatalf("session_control_list = %d %s", code, body)
	}
	var res ops.SessionControlListResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		k.t.Fatal(err)
	}
	return res.Grants
}

func TestSessionControlAdminSetsListsAndRemovesAGrant(t *testing.T) {
	k := newSessionControlStack(t)
	w1, w2 := k.worker(k.u1, "w1", store.WorkerKindPrivate), k.worker(k.u2, "w2", store.WorkerKindPrivate)
	k.as(k.admin)
	if code, body := k.call(ops.OpSessionControlList, map[string]any{}); code != http.StatusOK || !strings.Contains(body, `"grants":[]`) {
		t.Fatalf("a fresh Hub lists %d %s; want no grants (default deny)", code, body)
	}
	// v0.9.0: a grant with targets is made in Minion control.
	targets := []ops.KeyResource{{Kind: ops.KeyResourceWorker, WorkerID: w2.ID}}
	if code, body := k.call(ops.OpSessionControlSet, map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": targets}); code != http.StatusConflict || !strings.Contains(body, "Minion control") {
		t.Fatalf("set with targets = %d %s, want 409 pointing to Minion control", code, body)
	}
	if code, body := k.call(ops.OpControlSetMaster, map[string]any{"worker_id": w1.ID, "session_name": "boss", "master": true}); code != http.StatusOK {
		t.Fatalf("set master = %d %s", code, body)
	}
	if code, body := k.call(ops.OpControlSetAccept, map[string]any{"worker_id": w2.ID, "source_worker_id": w1.ID, "accept": true}); code != http.StatusOK {
		t.Fatalf("set accept = %d %s", code, body)
	}
	g := k.grants()
	if len(g) != 1 || g[0].WorkerID != w1.ID || g[0].SessionName != "boss" || len(g[0].Targets) != 1 || g[0].Targets[0] != targets[0] {
		t.Fatalf("grants = %+v", g)
	}
	// Empty targets still remove the master session.
	if code, body := k.call(ops.OpSessionControlSet, map[string]any{"worker_id": w1.ID, "session_name": "boss"}); code != http.StatusOK {
		t.Fatalf("remove = %d %s", code, body)
	}
	if g := k.grants(); len(g) != 0 {
		t.Fatalf("grants after remove = %+v", g)
	}
	// Every change is audited with the changed Worker (or session) as target.
	var n int
	for _, ev := range k.audit.all() {
		switch ev.Action {
		case ops.OpControlSetMaster:
			n++
			if ev.Target != "worker:"+w1.ID && !strings.Contains(ev.Target, w1.ID) {
				t.Errorf("audit target %q", ev.Target)
			}
		case ops.OpControlSetAccept:
			n++
			if !strings.Contains(ev.Target, w2.ID) {
				t.Errorf("audit target %q", ev.Target)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d audit events for the control setters, want 2", n)
	}
}

// A session limit on an acceptance is kept, and an empty one is refused.
func TestControlAcceptKeepsItsSessionLimit(t *testing.T) {
	k := newSessionControlStack(t)
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u1, "b", store.WorkerKindPrivate)
	k.as(k.admin)
	k.call(ops.OpControlSetMaster, map[string]any{"worker_id": a.ID, "session_name": "boss", "master": true})
	if code, body := k.call(ops.OpControlSetAccept, map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": true, "sessions": []string{}}); code != http.StatusBadRequest {
		t.Fatalf("empty sessions = %d %s", code, body)
	}
	if code, body := k.call(ops.OpControlSetAccept, map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": true, "sessions": []string{"ci"}}); code != http.StatusOK {
		t.Fatalf("limited accept = %d %s", code, body)
	}
	// The audit row says what was set: the source Worker and the reach, and
	// the master row names its session in the source, never in the detail.
	var accept, master audit.Event
	for _, ev := range k.audit.all() {
		switch {
		case ev.Action == ops.OpControlSetAccept && ev.Outcome == AuditOutcomeSuccess:
			accept = ev
		case ev.Action == ops.OpControlSetMaster:
			master = ev
		}
	}
	if want := "source_worker=" + a.ID + " accept=true sessions=limited"; accept.Detail != want || accept.Target != b.ID {
		t.Fatalf("accept audit = %+v, want detail %q", accept, want)
	}
	if master.Source != a.ID+":boss" || master.Detail != "session_set=true" {
		t.Fatalf("master audit = %+v", master)
	}
	g := k.grants()
	if len(g) != 1 || len(g[0].Targets) != 1 || g[0].Targets[0].Kind != ops.KeyResourceSession || g[0].Targets[0].SessionName != "ci" {
		t.Fatalf("the session limit was lost: %+v", g)
	}
}

func TestSessionControlRefusesNonAdmins(t *testing.T) {
	k := newSessionControlStack(t)
	w1 := k.worker(k.u1, "w1", store.WorkerKindPrivate)
	k.as(k.u1) // the owner of the Worker, but no admin
	if code, body := k.call(ops.OpSessionControlList, map[string]any{}); code != http.StatusForbidden {
		t.Errorf("list by a user = %d %s, want 403", code, body)
	}
	set := map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}
	if code, body := k.call(ops.OpSessionControlSet, set); code != http.StatusForbidden {
		t.Errorf("set by the owner = %d %s, want 403", code, body)
	}
	if rows, _ := k.st.ListSessionControl(context.Background()); len(rows) != 0 {
		t.Errorf("a refused set stored %+v", rows)
	}
}

// workerOnlyScope grants manage on one Worker and nothing else.
type workerOnlyScope string

func (s workerOnlyScope) Actions(res authz.ResolvedResource) []ops.Action {
	if res.WorkerID == string(s) {
		return []ops.Action{ops.ActionManage}
	}
	return nil
}

func TestSessionControlRefusesAnAdminKeyLimitedToSomeResources(t *testing.T) {
	k := newSessionControlStack(t)
	w1 := k.worker(k.u1, "w1", store.WorkerKindPrivate)
	k.authn.mu.Lock()
	k.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal(k.admin, workerOnlyScope(w1.ID), ""), Actor: "key:k1", Via: ViaAPIKey, IP: netip.MustParseAddr("192.0.2.7")}
	k.authn.mu.Unlock()
	set := map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}
	if code, body := k.call(ops.OpSessionControlSet, set); code != http.StatusForbidden {
		t.Errorf("set by a limited admin key = %d %s, want 403", code, body)
	}
}

func TestSessionControlRefusesInvalidRequests(t *testing.T) {
	k := newSessionControlStack(t)
	w1 := k.worker(k.u1, "w1", store.WorkerKindPrivate)
	k.as(k.admin)
	for _, tc := range []struct {
		name string
		args map[string]any
		code int
	}{
		{"everything combined", map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}, {Kind: ops.KeyResourceWorker, WorkerID: w1.ID}}}, http.StatusBadRequest},
		{"user target", map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceUser, UserID: k.u1}}}, http.StatusBadRequest},
		{"bad session name", map[string]any{"worker_id": w1.ID, "session_name": "a:b", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}, http.StatusBadRequest},
		{"unknown Worker", map[string]any{"worker_id": ops.NewUUIDv7(), "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}, http.StatusNotFound},
	} {
		if code, body := k.call(ops.OpSessionControlSet, tc.args); code != tc.code {
			t.Errorf("%s: %d %s, want %d", tc.name, code, body, tc.code)
		}
	}
	if rows, _ := k.st.ListSessionControl(context.Background()); len(rows) != 0 {
		t.Errorf("a refused set stored %+v", rows)
	}
}

// A Minion control change that was authorized before a reassignment or
// removal of a Worker must not bring control back after that transaction
// deleted the Worker's rows.
func TestSessionControlSetCannotOutliveAReassignmentOrRemovalMidRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		land func(k *kindStack, w store.WorkerRow)
		code int
	}{
		{"reassignment", func(k *kindStack, w store.WorkerRow) { k.reassignDirect(w, k.u2) }, http.StatusConflict},
		{"removal", func(k *kindStack, w store.WorkerRow) {
			ctx := context.Background()
			err := k.st.Tx(ctx, func(tx *sql.Tx) error {
				if _, err := k.st.DeleteSessionControlOfWorker(ctx, tx, w.ID); err != nil {
					return err
				}
				return k.st.TombstoneWorkerTx(ctx, tx, w.ID, k.admin, 1)
			})
			if err != nil {
				k.t.Fatalf("remove: %v", err)
			}
		}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newSessionControlStack(t)
			w := k.worker(k.u1, "pi", store.WorkerKindInfrastructure)
			src := k.worker(k.u1, "boss", store.WorkerKindInfrastructure)
			k.as(k.admin)
			k.call(ops.OpControlSetMaster, map[string]any{"worker_id": src.ID, "session_name": "boss", "master": true})
			k.az.mu.Lock()
			k.az.n = 0 // count the decisions of the next request only
			k.az.mu.Unlock()
			k.az.after = func(n int) {
				if n == 1 { // after the dispatcher's decision, before the handler stores
					tc.land(k, w)
				}
			}
			set := map[string]any{"worker_id": w.ID, "source_worker_id": src.ID, "accept": true}
			if code, body := k.call(ops.OpControlSetAccept, set); code != tc.code {
				t.Errorf("set = %d %s, want %d", code, body, tc.code)
			}
			if rows, _ := k.st.ListSessionControl(context.Background()); len(rows) != 0 {
				t.Errorf("control survived the %s: %+v", tc.name, rows)
			}
		})
	}
}

// "Who can control this session?" answers from the computed grant: a migrated
// v0.8 grant on the session's own Worker is named; a new nomination there is not.
func TestControlCheckNamesMigratedSelfGrants(t *testing.T) {
	k := newSessionControlStack(t)
	w1 := k.worker(k.u1, "w1", store.WorkerKindPrivate)
	ctx := context.Background()
	if err := k.st.PutSessionControl(ctx, store.SessionControlRow{WorkerID: w1.ID, SessionName: "boss",
		TargetsJSON: `[{"kind":"session","worker_id":"` + w1.ID + `","session_name":"ci"}]`, CreatedBy: k.admin, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := k.st.PutMasterSession(ctx, store.MasterRow{WorkerID: w1.ID, SessionName: "fresh", CreatedBy: k.admin}); err != nil {
		t.Fatal(err)
	}
	k.as(k.admin)
	code, body := k.call(ops.OpControlCheck, map[string]any{"worker_id": w1.ID, "session_name": "ci"})
	if code != http.StatusOK || !strings.Contains(body, `"session_name":"boss"`) || strings.Contains(body, `"fresh"`) {
		t.Fatalf("check = %d %s", code, body)
	}
}

// A session-bound master is not in master_session: the name-only revoke must
// refuse instead of answering delivered while nothing is revoked.
func TestNameOnlyRevokeRefusesABoundMaster(t *testing.T) {
	k := newSessionControlStack(t)
	a := k.worker(k.u1, "a", store.WorkerKindPrivate)
	ctx := context.Background()
	if _, err := k.st.DB().ExecContext(ctx, `INSERT INTO bound_master_session
		(worker_id,session_name,incarnation_id,owner_id,worker_pub,generation,active,changed_at) VALUES (?,?,?,?,?,1,1,1)`,
		a.ID, "boss", "incarnation-one", a.OwnerID, []byte{1}); err != nil {
		t.Fatal(err)
	}
	k.as(k.admin)
	for op, args := range map[string]map[string]any{
		ops.OpControlSetMaster:  {"worker_id": a.ID, "session_name": "boss", "master": false},
		ops.OpSessionControlSet: {"worker_id": a.ID, "session_name": "boss", "targets": []any{}},
	} {
		if code, body := k.call(op, args); code != http.StatusConflict || !strings.Contains(body, "session-bound master action") {
			t.Errorf("%s on a bound master = %d %s; want conflict", op, code, body)
		}
	}
	if ok, err := k.st.BoundMasterActive(ctx, a.ID, "boss"); err != nil || !ok {
		t.Fatalf("bound master after the refused revokes = %t, %v", ok, err)
	}
	// An inactive bound nomination and a plain name are revoked as before.
	if _, err := k.st.DB().ExecContext(ctx, `UPDATE bound_master_session SET active=0 WHERE worker_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boss", "plain"} {
		if code, body := k.call(ops.OpControlSetMaster, map[string]any{"worker_id": a.ID, "session_name": name, "master": false}); code != http.StatusOK {
			t.Errorf("revoke of %s = %d %s", name, code, body)
		}
	}
}

// failingGrantStore fails the read of a master session's computed grant.
type failingGrantStore struct{ *store.Store }

func (failingGrantStore) SessionControl(context.Context, string, string) (store.SessionControlRow, error) {
	return store.SessionControlRow{}, errors.New("the database is gone")
}

// control_check answers an error when the Hub cannot read a grant: skipping
// the master would say "nobody can control this session" while one can.
func TestControlCheckReportsAStoreError(t *testing.T) {
	k := newControlStack(t, func(st *store.Store) MinionControlStore { return failingGrantStore{st} })
	w1 := k.worker(k.u1, "w1", store.WorkerKindPrivate)
	if err := k.st.PutMasterSession(context.Background(), store.MasterRow{WorkerID: w1.ID, SessionName: "boss", CreatedBy: k.admin}); err != nil {
		t.Fatal(err)
	}
	k.as(k.admin)
	code, body := k.call(ops.OpControlCheck, map[string]any{"worker_id": w1.ID, "session_name": "ci"})
	if code < 500 || strings.Contains(body, "No master session can control") {
		t.Fatalf("check with a failing store = %d %s; want an error, not an empty answer", code, body)
	}
}

// The setters refuse a change made on a view someone else changed meanwhile
// (if_state, if_master, if_user_managed, if_accept_all) and store nothing.
func TestControlSettersRefuseAStaleView(t *testing.T) {
	k := newSessionControlStack(t)
	a := k.worker(k.u1, "a", store.WorkerKindPrivate)
	b := k.worker(k.u1, "b", store.WorkerKindPrivate)
	ctx := context.Background()
	k.as(k.admin)
	accept := func(on bool, seen string) (int, string) {
		return k.call(ops.OpControlSetAccept, map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": on, "if_state": seen})
	}
	if code, body := accept(true, "absent"); code != http.StatusOK {
		t.Fatalf("tick on no entry = %d %s", code, body)
	}
	// Another administrator limits the acceptance.
	if err := k.st.PutAccept(ctx, store.AcceptRow{TargetWorkerID: b.ID, SourceWorkerID: a.ID, Sessions: []string{"ci"}, CreatedBy: k.admin}); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		for _, seen := range []string{"absent", "all", "none"} {
			if code, body := accept(on, seen); code != http.StatusConflict || !strings.Contains(body, "changed by someone else") {
				t.Errorf("accept=%t seen %s on a limited entry = %d %s; want conflict", on, seen, code, body)
			}
		}
	}
	if code, body := accept(true, "maybe"); code != http.StatusBadRequest {
		t.Errorf("unknown if_state = %d %s", code, body)
	}
	rows, err := k.st.ListAccepts(ctx)
	if err != nil || len(rows) != 1 || len(rows[0].Sessions) != 1 || rows[0].Sessions[0] != "ci" {
		t.Fatalf("acceptance after the refused changes = %+v, %v; want the limit of the other administrator", rows, err)
	}
	if code, body := accept(false, "limited"); code != http.StatusOK {
		t.Errorf("untick on the seen state = %d %s", code, body)
	}
	// A nomination and the Worker switches.
	master := func(on, seen bool) int {
		code, _ := k.call(ops.OpControlSetMaster, map[string]any{"worker_id": a.ID, "session_name": "boss", "master": on, "if_master": seen})
		return code
	}
	if got := []int{master(false, true), master(true, false), master(true, false), master(false, false), master(false, true)}; !slices.Equal(got,
		[]int{http.StatusConflict, http.StatusOK, http.StatusConflict, http.StatusConflict, http.StatusOK}) {
		t.Errorf("control_set_master with if_master = %v", got)
	}
	sw := func(args map[string]any) int {
		args["worker_id"] = b.ID
		code, _ := k.call(ops.OpControlSetWorker, args)
		return code
	}
	if got := []int{sw(map[string]any{"accept_all": true, "if_accept_all": true}), sw(map[string]any{"accept_all": true, "if_accept_all": false}),
		sw(map[string]any{"user_managed": false, "if_user_managed": false}), sw(map[string]any{"user_managed": false, "if_user_managed": true})}; !slices.Equal(got,
		[]int{http.StatusConflict, http.StatusOK, http.StatusConflict, http.StatusOK}) {
		t.Errorf("control_set_worker with what was seen = %v", got)
	}
}
