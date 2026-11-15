package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/netip"
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
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpSessionControlList, ops.OpSessionControlSet} {
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
	targets := []ops.KeyResource{
		{Kind: ops.KeyResourceWorker, WorkerID: w2.ID},
		{Kind: ops.KeyResourceSession, WorkerID: w1.ID, SessionName: "build"},
	}
	if code, body := k.call(ops.OpSessionControlSet, map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": targets}); code != http.StatusOK {
		t.Fatalf("set = %d %s", code, body)
	}
	g := k.grants()
	if len(g) != 1 || g[0].WorkerID != w1.ID || g[0].SessionName != "boss" || g[0].CreatedBy != k.admin ||
		len(g[0].Targets) != 2 || g[0].Targets[0] != targets[0] || g[0].Targets[1] != targets[1] {
		t.Fatalf("grants = %+v", g)
	}
	// An upsert replaces the targets.
	if code, body := k.call(ops.OpSessionControlSet, map[string]any{"worker_id": w1.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}); code != http.StatusOK {
		t.Fatalf("set = %d %s", code, body)
	}
	if g := k.grants(); len(g) != 1 || len(g[0].Targets) != 1 || g[0].Targets[0].Kind != ops.KeyResourceEverything {
		t.Fatalf("grants after upsert = %+v", g)
	}
	// Empty targets delete the row.
	if code, body := k.call(ops.OpSessionControlSet, map[string]any{"worker_id": w1.ID, "session_name": "boss"}); code != http.StatusOK {
		t.Fatalf("remove = %d %s", code, body)
	}
	if g := k.grants(); len(g) != 0 {
		t.Fatalf("grants after remove = %+v", g)
	}
	// Every set is audited with the controller session as its target.
	var n int
	for _, ev := range k.audit.all() {
		if ev.Action == ops.OpSessionControlSet {
			n++
			if ev.Target != w1.ID+":boss" || ev.Actor != k.admin || ev.Outcome != AuditOutcomeSuccess {
				t.Errorf("audit event = %+v", ev)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d audit events for session_control_set, want 3", n)
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

// A session_control_set that was authorized before a reassignment or removal
// of the controller Worker must not bring a grant back after that transaction
// deleted the Worker's grants.
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
			k.as(k.admin)
			k.az.after = func(n int) {
				if n == 1 { // after the dispatcher's decision, before the handler stores
					tc.land(k, w)
				}
			}
			set := map[string]any{"worker_id": w.ID, "session_name": "boss", "targets": []ops.KeyResource{{Kind: ops.KeyResourceEverything}}}
			if code, body := k.call(ops.OpSessionControlSet, set); code != tc.code {
				t.Errorf("set = %d %s, want %d", code, body, tc.code)
			}
			if rows, _ := k.st.ListSessionControl(context.Background()); len(rows) != 0 {
				t.Errorf("a grant survived the %s: %+v", tc.name, rows)
			}
		})
	}
}
