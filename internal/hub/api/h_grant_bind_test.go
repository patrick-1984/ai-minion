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
	// The real audit log over the store, as on the Hub: a bind that commits
	// writes its audit row in the store transaction, a refusal through the log.
	lg := audit.New(k.st)
	if err := RegisterGrantBind(handlers, k.st, lane, list, k.az, lg); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: lg, Log: testLogger(&strings.Builder{})}
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
	// Each change that committed has exactly one stored row, written with it:
	// the action, the grant, its source and target, and a detail without names.
	success := func(action string) []audit.Event {
		var out []audit.Event
		for _, ev := range k.grantEvents(action) {
			if ev.Outcome == AuditOutcomeSuccess {
				out = append(out, ev)
			}
		}
		return out
	}
	for _, want := range []audit.Event{
		{Action: AuditActionGrantBind, Detail: "bind=true level=control rev=2"},
		{Action: AuditActionGrantUnbind, Detail: "bind=false level=control rev=3"},
	} {
		evs := success(want.Action)
		if len(evs) != 1 || evs[0].GrantID != g.ID || evs[0].Source != a1.ID+":agent" || evs[0].Target != a2.ID+":build" ||
			evs[0].Detail != want.Detail || evs[0].Actor == "" {
			t.Fatalf("%s success rows = %+v", want.Action, evs)
		}
	}
	// The refusals are recorded too, never as a success: no terminal and the
	// stale revision for the bind, the stale revision for the unbind.
	if n := len(k.grantEvents(AuditActionGrantBind)); n != 3 {
		t.Fatalf("grant.bind rows = %d, want 3: %+v", n, k.grantEvents(AuditActionGrantBind))
	}
	if n := len(k.grantEvents(AuditActionGrantUnbind)); n != 2 {
		t.Fatalf("grant.unbind rows = %d, want 2: %+v", n, k.grantEvents(AuditActionGrantUnbind))
	}
}

// A user who lost the Workers between the handler's check and the write
// commits no bind and no success row (the store reads the authority again).
func TestGrantBindAuthorityIsReadInTheWritingTransaction(t *testing.T) {
	lane := &fakeBindLane{live: map[string]string{"agent": "inc-1"}}
	k := newGrantBindStack(t, lane)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(t.Context(), `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	g, err := k.st.PutSessionGrant(t.Context(), store.SessionGrantRow{SourceWorkerID: a1.ID, SourceSession: "agent",
		TargetWorkerID: a2.ID, TargetSession: "build", Level: 1, Authority: store.GrantAuthorityOwner, GrantedBy: k.u1,
		SourceOwnerID: k.u1, TargetOwnerID: k.u1}, 0, store.GrantWrite{ActorID: k.u1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.st.DB().ExecContext(t.Context(), `UPDATE users SET active=0 WHERE id=?`, k.u1); err != nil {
		t.Fatal(err)
	}
	_, err = k.st.BindSessionGrant(t.Context(), g.ID, "inc-1", g.Revision, store.GrantWrite{ActorID: k.u1,
		Audit: func(_ *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
			return store.AuditRow{Actor: "user:u1", Action: AuditActionGrantBind, Target: "grant", Outcome: AuditOutcomeSuccess, GrantID: now.ID}, nil
		}})
	if ErrorOf(grantStoreErr(err)).Code != proto.CodeForbidden {
		t.Fatalf("bind by a deactivated owner = %v", err)
	}
	if row, _ := k.st.SessionGrant(t.Context(), g.ID); row.SourceIncarnation != "" || row.Revision != g.Revision {
		t.Fatalf("the refused bind changed the grant: %+v", row)
	}
	if n := len(k.grantEvents(AuditActionGrantBind)); n != 0 {
		t.Fatalf("the refused bind left %d audit rows", n)
	}
}

// An omitted or null bind is refused before the handler runs: it would
// otherwise decode as false and turn a Verified grant into a Claimed one.
func TestGrantBindNeedsAnExplicitBind(t *testing.T) {
	lane := &fakeBindLane{live: map[string]string{"agent": "inc-1"}}
	k := newGrantBindStack(t, lane)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	if _, err := k.st.DB().ExecContext(t.Context(), `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	g, err := k.st.PutSessionGrant(t.Context(), store.SessionGrantRow{SourceWorkerID: a1.ID, SourceSession: "agent",
		TargetWorkerID: a2.ID, TargetSession: "build", Level: 3, Authority: store.GrantAuthorityOwner,
		SourceOwnerID: k.u1, TargetOwnerID: k.u1}, 0, store.GrantWrite{ActorID: k.u1})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = k.st.BindSessionGrant(t.Context(), g.ID, "inc-1", g.Revision, store.GrantWrite{ActorID: k.u1}); err != nil {
		t.Fatal(err)
	}
	k.as(k.u1)
	for name, args := range map[string]map[string]any{
		"omitted": {"id": g.ID, "if_revision": g.Revision},
		"null":    {"id": g.ID, "if_revision": g.Revision, "bind": nil},
	} {
		if code, body := k.call(ops.OpGrantBind, args); code == http.StatusOK || kindErrCode(t, body) != proto.CodeInvalidArgument {
			t.Fatalf("%s bind = %d %s, want invalid_argument", name, code, body)
		}
	}
	if row, _ := k.st.SessionGrant(t.Context(), g.ID); row.SourceIncarnation != "inc-1" || row.Revision != g.Revision {
		t.Fatalf("the Verified grant changed: %+v", row)
	}
	if len(lane.refreshed) != 0 {
		t.Fatalf("leases refreshed: %v", lane.refreshed)
	}
}
