package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// newGrantStack is the kind stack with only the grant operations registered
// over the real store and authorizer.
func newGrantStack(t *testing.T) *kindStack { return newGrantStackWith(t, nil) }

// newGrantStackWith is newGrantStack; tweak may change what the handlers know
// of a Worker (approval, thin).
func newGrantStackWith(t *testing.T, tweak func(*GrantWorker)) *kindStack {
	t.Helper()
	k := newKindStack(t)
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpGrantList, ops.OpGrantSet, ops.OpGrantRevoke, ops.OpGrantApprove} {
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
			gw := GrantWorker{ID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Approved: true, CanBeSource: true}
			if tweak != nil {
				tweak(&gw)
			}
			out = append(out, gw)
		}
		return out, nil
	}
	if err := RegisterGrants(handlers, k.st, list, k.az, k.audit); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: k.audit, Log: testLogger(&strings.Builder{})}
	if _, err := NewRouter(d, k.mux); err != nil {
		t.Fatal(err)
	}
	return k
}

func grantArgs(src, tgt store.WorkerRow, level string) map[string]any {
	return map[string]any{"source_worker_id": src.ID, "source_session": "agent", "target_worker_id": tgt.ID,
		"target_session": "build", "level": level}
}

// setGrant calls grant_set and returns the stored grant.
func (k *kindStack) setGrant(args map[string]any) ops.Grant {
	k.t.Helper()
	code, body := k.call(ops.OpGrantSet, args)
	if code != http.StatusOK {
		k.t.Fatalf("grant_set = %d %s", code, body)
	}
	var res ops.GrantSetResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Grant.ID == "" {
		k.t.Fatalf("grant_set = %s (%v)", body, err)
	}
	return res.Grant
}

func (k *kindStack) listGrants() []ops.Grant {
	k.t.Helper()
	code, body := k.call(ops.OpGrantList, map[string]any{})
	if code != http.StatusOK {
		k.t.Fatalf("grant_list = %d %s", code, body)
	}
	var res ops.GrantListResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		k.t.Fatal(err)
	}
	return res.Grants
}

func TestGrantOwnerCreatesBetweenOwnWorkersAndOthersSeeNothing(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	if g.Authority != ops.GrantAuthorityOwner || g.GrantedBy != k.u1 || g.Level != ops.GrantLevelView || g.State != ops.GrantStateActive || g.Revision == 0 {
		t.Fatalf("grant = %+v", g)
	}
	if l := k.listGrants(); len(l) != 1 || l[0].ID != g.ID || l[0].SourceWorkerName != "a1" {
		t.Fatalf("owner lists %+v", l)
	}
	// level message is not available yet; a second grant of the pair conflicts.
	if code, body := k.call(ops.OpGrantSet, grantArgs(a2, a1, ops.GrantLevelMessage)); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("message level = %d %s", code, body)
	}
	if code, body := k.call(ops.OpGrantSet, grantArgs(a1, a2, ops.GrantLevelControl)); kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("duplicate = %d %s", code, body)
	}
	// Update with a stale revision conflicts, with the right one it changes.
	up := grantArgs(a1, a2, ops.GrantLevelControl)
	up["id"], up["if_revision"] = g.ID, g.Revision+7
	if code, body := k.call(ops.OpGrantSet, up); kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("stale revision = %d %s", code, body)
	}
	up = grantArgs(a1, a2, ops.GrantLevelControl)
	up["id"], up["if_revision"] = g.ID, g.Revision
	if g2 := k.setGrant(up); g2.Level != ops.GrantLevelControl || g2.Revision == g.Revision {
		t.Fatalf("update = %+v", g2)
	}
	// Another user sees nothing; an administrator sees it.
	k.as(k.u2)
	if l := k.listGrants(); len(l) != 0 {
		t.Fatalf("a non-owner lists %+v", l)
	}
	k.as(k.admin)
	if l := k.listGrants(); len(l) != 1 {
		t.Fatalf("an administrator lists %+v", l)
	}
	// grant_set is audited by the handler, refusals included.
	var set int
	for _, ev := range k.audit.all() {
		if ev.Action == AuditActionGrantCreate || ev.Action == AuditActionGrantUpdate {
			set++
		}
	}
	if set != 5 {
		t.Fatalf("grant_set audit events = %d, want 5: %+v", set, k.audit.all())
	}
}

func TestGrantBetweenUsersTakesAnAdministrator(t *testing.T) {
	k := newGrantStack(t)
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u2, "b", store.WorkerKindPrivate)
	k.as(k.u1)
	if code, body := k.call(ops.OpGrantSet, grantArgs(a, b, ops.GrantLevelView)); code != http.StatusForbidden {
		t.Fatalf("a plain user between users = %d %s, want forbidden", code, body)
	}
	if rows, _ := k.st.ListSessionGrants(context.Background(), store.SessionGrantFilter{}); len(rows) != 0 {
		t.Fatalf("stored %+v", rows)
	}
	k.as(k.admin)
	g := k.setGrant(grantArgs(a, b, ops.GrantLevelView))
	if g.Authority != ops.GrantAuthorityAdmin || g.GrantedBy != k.admin {
		t.Fatalf("grant = %+v", g)
	}
	// An API key cannot call grant_set (SessionOnly).
	k.authn.mu.Lock()
	k.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal(k.admin, nil, ""), Actor: "key:k1", Via: ViaAPIKey}
	k.authn.mu.Unlock()
	if code, body := k.call(ops.OpGrantSet, grantArgs(b, a, ops.GrantLevelView)); code != http.StatusForbidden {
		t.Fatalf("an API key = %d %s, want forbidden", code, body)
	}
	// The owner of the target revokes; a second revoke answers not_found.
	k.as(k.u2)
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID}); code != http.StatusOK {
		t.Fatalf("revoke by the target's owner = %d %s", code, body)
	}
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID}); kindErrCode(t, body) != proto.CodeNotFound {
		t.Fatalf("second revoke = %d %s", code, body)
	}
}

func TestGrantApproveAfterTheGrantorWasDemoted(t *testing.T) {
	k := newGrantStack(t)
	ctx := context.Background()
	admin2 := store.UserRow{ID: store.NewID(), Username: "root2", PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
	if err := k.st.CreateUser(ctx, store.CreateUserParams{User: admin2, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u2, "b", store.WorkerKindPrivate)
	g, err := k.st.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: a.ID, SourceSession: "agent", TargetWorkerID: b.ID,
		Level: 1, Authority: store.GrantAuthorityAdmin, GrantedBy: admin2.ID, SourceOwnerID: k.u1, TargetOwnerID: k.u2}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.st.DB().ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND role = 'admin'`, admin2.ID); err != nil {
		t.Fatal(err)
	}
	cur, err := k.st.SessionGrant(ctx, g.ID)
	if err != nil || cur.SuspendedAt == nil {
		t.Fatalf("after the demotion: %+v (%v), want suspended", cur, err)
	}
	k.as(k.u1)
	if code, body := k.call(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": cur.Revision}); code != http.StatusForbidden {
		t.Fatalf("approve by a user = %d %s", code, body)
	}
	k.as(k.admin)
	code, body := k.call(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": cur.Revision})
	var res ops.GrantSetResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || res.Grant.Suspended || res.Grant.GrantedBy != k.admin {
		t.Fatalf("approve = %d %s", code, body)
	}
}

// grantEvents returns the audit events of one action.
func (k *kindStack) grantEvents(action string) []audit.Event {
	var out []audit.Event
	for _, ev := range k.audit.all() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// An administrator grants between two Workers of ONE other user: the
// authority is admin. The audit rows carry target, source, grant id and a
// detail without names; the grantor revokes it.
func TestGrantAdministratorBetweenWorkersOfOneOtherUserAndTheAudit(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.admin)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	if g.Authority != ops.GrantAuthorityAdmin || g.GrantedBy != k.admin {
		t.Fatalf("grant = %+v", g)
	}
	evs := k.grantEvents(AuditActionGrantCreate)
	if len(evs) != 1 || evs[0].Target != a2.ID+":build" || evs[0].Source != a1.ID+":agent" || evs[0].GrantID != g.ID ||
		evs[0].Detail != "level=view authority=admin expires=none" || evs[0].Outcome != AuditOutcomeSuccess {
		t.Fatalf("create audit = %+v", evs)
	}
	up := grantArgs(a1, a2, ops.GrantLevelControl)
	up["id"], up["if_revision"] = g.ID, g.Revision
	g2 := k.setGrant(up)
	evs = k.grantEvents(AuditActionGrantUpdate)
	if want := "level_old=view level_new=control rev=" + strconv.FormatUint(g2.Revision, 10); len(evs) != 1 || evs[0].Detail != want || evs[0].GrantID != g.ID {
		t.Fatalf("update audit = %+v, want detail %q", evs, want)
	}
	// A refused update (stale revision) is audited as failed.
	if code, body := k.call(ops.OpGrantSet, up); kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("stale update = %d %s", code, body)
	}
	if evs = k.grantEvents(AuditActionGrantUpdate); len(evs) != 2 || evs[1].Outcome != "failed" || evs[1].Detail != "level_old=control level_new=control" {
		t.Fatalf("refused update audit = %+v", evs)
	}
	// Revoke by granted_by; the dispatcher's row names the grant.
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID}); code != http.StatusOK {
		t.Fatalf("revoke by the grantor = %d %s", code, body)
	}
	evs = k.grantEvents(ops.OpGrantRevoke)
	if len(evs) != 1 || evs[0].GrantID != g.ID || evs[0].Source != a1.ID+":agent" || evs[0].Detail != "level=control authority=admin" {
		t.Fatalf("revoke audit = %+v", evs)
	}
}

// grant_set is SessionOnly: the dispatcher refuses an act-as session before
// the handler runs, for the acted user's own Workers and between users alike
// (even when the actor is an administrator). Nothing is stored.
func TestGrantActAsIsRefusedBySessionOnly(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b := k.worker(k.u2, "b", store.WorkerKindPrivate)
	k.authn.mu.Lock()
	k.authn.id = Identity{Principal: authz.NewActAsPrincipal(k.admin, k.u1, time.Now().Add(time.Hour)), Actor: k.admin, ActingAs: k.u1, Via: ViaSession}
	k.authn.mu.Unlock()
	for _, args := range []map[string]any{grantArgs(a1, a2, ops.GrantLevelView), grantArgs(a1, b, ops.GrantLevelView)} {
		if code, body := k.call(ops.OpGrantSet, args); code != http.StatusForbidden {
			t.Fatalf("act-as grant_set = %d %s, want forbidden", code, body)
		}
	}
	if rows, _ := k.st.ListSessionGrants(context.Background(), store.SessionGrantFilter{}); len(rows) != 0 {
		t.Fatalf("stored %+v", rows)
	}
}

// A Worker that waits for approval cannot be the source or the target.
func TestGrantUnapprovedWorkerIsRefused(t *testing.T) {
	var pending string
	k := newGrantStackWith(t, func(w *GrantWorker) { w.Approved = w.ID != pending })
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	pending = a2.ID
	k.as(k.u1)
	for _, args := range []map[string]any{grantArgs(a1, a2, ops.GrantLevelView), grantArgs(a2, a1, ops.GrantLevelView)} {
		if code, body := k.call(ops.OpGrantSet, args); kindErrCode(t, body) != proto.CodeConflict {
			t.Fatalf("grant with an unapproved Worker = %d %s, want conflict", code, body)
		}
	}
	if rows, _ := k.st.ListSessionGrants(context.Background(), store.SessionGrantFilter{}); len(rows) != 0 {
		t.Fatalf("stored %+v", rows)
	}
}
