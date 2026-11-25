package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func newGrantHistoryStack(t *testing.T) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove, ops.OpGrantHistory} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	handlers := NewRegistry(cat)
	list := func(ctx context.Context) ([]GrantWorker, error) {
		rows, err := k.st.ListWorkers(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]GrantWorker, 0, len(rows))
		for _, w := range rows {
			out = append(out, GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Approved: true, CanBeSource: true})
		}
		return out, nil
	}
	// The grant handlers write to the real audit log, which the history reads.
	alog := audit.New(k.st)
	if err := RegisterGrants(handlers, k.st, list, k.az, alog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterGrantHistory(handlers, k.st, alog, list, k.az); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *kindStack) grantHistory(id string) (int, []ops.AuditEvent) {
	k.t.Helper()
	code, body := k.call(ops.OpGrantHistory, map[string]any{"id": id})
	var res ops.GrantHistoryResult
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			k.t.Fatalf("grant_history = %s (%v)", body, err)
		}
	}
	return code, res.Events
}

// The owner of a Worker of the grant and an administrator read its history,
// also after the grant has ended; another user does not.
func TestGrantHistoryForOwnerAndAdministrator(t *testing.T) {
	k := newGrantHistoryStack(t)
	ctx := t.Context()
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b1 := k.worker(k.u2, "b1", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	k.as(k.u1)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	other := k.setGrant(map[string]any{"source_worker_id": a2.ID, "source_session": "agent", "target_worker_id": a1.ID, "level": ops.GrantLevelView})

	code, evs := k.grantHistory(g.ID)
	if code != http.StatusOK || len(evs) != 1 || evs[0].Action != AuditActionGrantCreate || evs[0].GrantID != g.ID {
		t.Fatalf("owner history = %d %+v", code, evs)
	}
	k.as(k.u2)
	if code, evs := k.grantHistory(g.ID); code == http.StatusOK {
		t.Fatalf("another user reads the history: %+v", evs)
	}
	k.as(k.admin)
	if code, evs := k.grantHistory(g.ID); code != http.StatusOK || len(evs) != 1 {
		t.Fatalf("administrator history = %d %+v", code, evs)
	}

	// The session is gone: the grant ends, its history stays readable.
	if n, err := k.st.EndGoneSessionGrants(ctx, a1.ID, []string{"agent"}, 1<<60); err != nil || n != 1 {
		t.Fatalf("end = %d, %v", n, err)
	}
	k.as(k.u1)
	code, evs = k.grantHistory(g.ID)
	if code != http.StatusOK || len(evs) != 2 || evs[0].Action != store.AuditActionGrantEnd ||
		evs[0].Actor != store.AuditActorSystem || evs[0].Detail != "cause=session_gone" || evs[1].Action != AuditActionGrantCreate {
		t.Fatalf("history of an ended grant = %d %+v", code, evs)
	}
	k.as(k.u2)
	if code, evs := k.grantHistory(g.ID); code == http.StatusOK {
		t.Fatalf("another user reads the history of an ended grant: %+v", evs)
	}
	k.as(k.admin)
	if code, evs := k.grantHistory(g.ID); code != http.StatusOK || len(evs) != 2 {
		t.Fatalf("administrator, ended grant = %d %+v", code, evs)
	}
	// The other grant's rows are not part of this history.
	if code, evs := k.grantHistory(other.ID); code != http.StatusOK || len(evs) != 1 || evs[0].GrantID != other.ID {
		t.Fatalf("other grant = %d %+v", code, evs)
	}
	// A grant nobody recorded, and an id that is none.
	k.as(k.u1)
	if code, _ := k.grantHistory(ops.NewUUIDv7()); code != http.StatusNotFound {
		t.Fatalf("unknown grant = %d", code)
	}
	if code, _ := k.grantHistory("nope"); code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", code)
	}
	_ = b1
}
