package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// fakeBindLane reports one incarnation per session name.
type fakeBindLane struct {
	live      map[string]string
	refreshed []string
}

func (f *fakeBindLane) PreparedIncarnation(_ context.Context, _, session string) (string, bool) {
	inc, ok := f.live[session]
	return inc, ok
}

func (f *fakeBindLane) RefreshLeases(workerID string) { f.refreshed = append(f.refreshed, workerID) }

func newGrantBindStack(t *testing.T, lane GrantBindLane) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantSet, ops.OpGrantBind} {
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
	if err := RegisterGrantBind(handlers, k.st, lane, list, k.az, k.audit); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestGrantBindAndUnbindWithConflicts(t *testing.T) {
	lane := &fakeBindLane{live: map[string]string{}}
	k := newGrantBindStack(t, lane)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(t.Context(), `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	g, err := k.st.PutSessionGrant(t.Context(), store.SessionGrantRow{SourceWorkerID: a1.ID, SourceSession: "agent",
		TargetWorkerID: a2.ID, TargetSession: "build", Level: 3, Authority: store.GrantAuthorityOwner, GrantedBy: k.u1,
		SourceOwnerID: k.u1, TargetOwnerID: k.u1}, 0, store.GrantWrite{ActorID: k.u1})
	if err != nil {
		t.Fatal(err)
	}
	k.as(k.u1)
	bind := func(rev uint64, on bool) (int, ops.GrantSetResult, string) {
		code, body := k.call(ops.OpGrantBind, map[string]any{"id": g.ID, "if_revision": rev, "bind": on})
		var res ops.GrantSetResult
		_ = json.Unmarshal([]byte(body), &res)
		return code, res, body
	}
	// No prepared terminal: conflict, nothing changes.
	if code, _, body := bind(g.Revision, true); code == http.StatusOK || kindErrCode(t, body) != proto.CodeConflict ||
		!strings.Contains(body, "not a prepared terminal right now") {
		t.Fatalf("bind without a terminal = %d %s", code, body)
	}
	lane.live["agent"] = "inc-1"
	if code, _, body := bind(g.Revision+7, true); code == http.StatusOK || kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("bind at a stale revision = %d %s", code, body)
	}
	code, res, body := bind(g.Revision, true)
	if code != http.StatusOK || res.Grant.SourceProof != ops.GrantProofVerified || res.Note != ops.GrantBindNote ||
		res.Grant.Revision != g.Revision+1 || len(lane.refreshed) != 1 || lane.refreshed[0] != a1.ID {
		t.Fatalf("bind = %d %s (refreshed %v)", code, body, lane.refreshed)
	}
	if row, _ := k.st.SessionGrant(t.Context(), g.ID); row.SourceIncarnation != "inc-1" {
		t.Fatalf("stored incarnation = %q", row.SourceIncarnation)
	}
	// Another user is refused; the old revision conflicts; unbind works.
	k.as(k.u2)
	if code, _, body := bind(res.Grant.Revision, false); code == http.StatusOK {
		t.Fatalf("a stranger unbound the grant: %s", body)
	}
	k.as(k.u1)
	if code, _, body := bind(g.Revision, false); code == http.StatusOK || kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("unbind at a stale revision = %d %s", code, body)
	}
	code, res, body = bind(res.Grant.Revision, false)
	if code != http.StatusOK || res.Grant.SourceProof != ops.GrantProofClaimed || res.Note != "" {
		t.Fatalf("unbind = %d %s", code, body)
	}
	if n := len(k.grantEvents(AuditActionGrantBind)); n < 2 {
		t.Fatalf("grant.bind audit events = %d", n)
	}
}
