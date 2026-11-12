package server

import (
	"context"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// worker_pairing_reissue (design 12.3) is wired end to end in the production
// module set: the owner and an administrator open the repair window of a
// Worker that never connected; a stranger does not see the Worker; a Worker
// that has connected (or an unknown one) is refused.
func TestWorkerPairingReissueIsServedForTheOwnerAndAdmins(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "reissue-integration-pw-1")
	var (
		st  *store.Store
		mgr *session.Manager
	)
	reg := productionRegistry(t)
	if err := reg.Register("grab", 90, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		mgr, err = Require[*session.Manager](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data := hubstore.Dir(t)
	hubidtest.Provision(t, data)
	h := startHub(t, &Config{DataDir: data}, reg)
	ah := &apiHub{h: h, hc: newHubClient(t, h, "https"), st: st, mgr: mgr}
	ctx := context.Background()

	alice := ah.user(t, "alice")
	ah.user(t, "mallory")
	newWorker := func(name string, connected bool) string {
		t.Helper()
		k, err := identity.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		w := store.WorkerRow{ID: store.NewID(), Pub: k.Public(), Name: name, OwnerID: alice, Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
		if connected {
			w.FirstConnectedAt = w.CreatedAt
		}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
		return w.ID
	}
	fresh, connected := newWorker("never", false), newWorker("seen", true)
	reissue := func(who, worker string) apiResp {
		t.Helper()
		tok, csrf := ah.login(t, who)
		return ah.call(t, "POST", "/api/v1/ops/"+ops.OpWorkerPairingReissue,
			ops.WorkerPairingReissueArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, WorkerID: worker}, asSession(tok, csrf))
	}

	if r := reissue("mallory", fresh); r.code < 400 || r.err.Code == proto.CodeConflict {
		t.Errorf("a stranger re-issued: %d %s", r.code, r.body)
	}
	if r := reissue("alice", fresh); r.code != 200 {
		t.Fatalf("owner re-issue = %d %s", r.code, r.body)
	}
	row, err := st.WorkerByID(ctx, fresh)
	if err != nil || row.RepairUntil <= time.Now().UnixMilli() || row.RepairUntil > time.Now().Add(workers.RepairWindow+time.Minute).UnixMilli() {
		t.Fatalf("repair window = %d (%v)", row.RepairUntil, err)
	}
	if r := reissue("root", fresh); r.code != 200 {
		t.Fatalf("admin re-issue = %d %s", r.code, r.body)
	}
	if r := reissue("alice", connected); r.code != 409 || r.err.Code != proto.CodeConflict {
		t.Errorf("re-issue of a connected Worker = %d %s", r.code, r.body)
	}
	if r := reissue("root", store.NewID()); r.code < 400 {
		t.Errorf("re-issue of an unknown Worker = %d %s", r.code, r.body)
	}
	rows, err := st.ListAudit(ctx, store.AuditQuery{Action: workers.AuditPairingReissue, Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit = %+v (%v)", rows, err)
	}
	for _, r := range rows {
		if r.Target != fresh || r.Outcome != "window_opened" {
			t.Errorf("audit row = %+v", r)
		}
	}
}
