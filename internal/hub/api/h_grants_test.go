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
	// The real audit log over the store, as on the Hub: it sees the rows the
	// store writes inside a grant transaction, so the dispatcher adds none.
	lg := audit.New(k.st)
	if err := RegisterGrants(handlers, k.st, list, k.az, lg); err != nil {
		t.Fatal(err)
	}
	k.mux = http.NewServeMux()
	d := &Dispatcher{Catalog: cat, Handlers: handlers, Auth: k.authn, Authz: k.az, Audit: lg, Log: testLogger(&strings.Builder{})}
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
	// The level message is granted like the others; a second grant of the
	// pair conflicts.
	if m := k.setGrant(grantArgs(a2, a1, ops.GrantLevelMessage)); m.Level != ops.GrantLevelMessage || m.State != ops.GrantStateActive {
		t.Fatalf("message level = %+v", m)
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
	if l := k.listGrants(); len(l) != 2 {
		t.Fatalf("an administrator lists %+v", l)
	}
	// grant_set is audited by the handler, refusals included.
	if set := len(k.grantEvents(AuditActionGrantCreate)) + len(k.grantEvents(AuditActionGrantUpdate)); set != 5 {
		t.Fatalf("grant_set audit events = %d, want 5", set)
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
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != http.StatusOK {
		t.Fatalf("revoke by the target's owner = %d %s", code, body)
	}
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); kindErrCode(t, body) != proto.CodeNotFound {
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
		Level: 1, Authority: store.GrantAuthorityAdmin, SourceOwnerID: k.u1, TargetOwnerID: k.u2}, 0, store.GrantWrite{ActorID: admin2.ID})
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
	// The grant is not suspended any more: a second approval is a conflict,
	// changes nothing (revision, updated_at) and is audited as failed.
	code, body = k.call(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": res.Grant.Revision})
	if code != http.StatusConflict || kindErrCode(t, body) != proto.CodeConflict || !strings.Contains(body, "this grant is not suspended") {
		t.Fatalf("approve of a grant that is not suspended = %d %s", code, body)
	}
	if row, err := k.st.SessionGrant(ctx, g.ID); err != nil || row.GrantedBy != k.admin || row.Revision != res.Grant.Revision ||
		row.UpdatedAt != res.Grant.UpdatedAt.Time().UnixMilli() {
		t.Fatalf("after the refused approval = %+v, %v", row, err)
	}
	evs := k.grantEvents(AuditActionGrantApprove)
	if len(evs) != 2 || evs[0].Outcome != AuditOutcomeSuccess || evs[1].Outcome != "failed" || evs[1].GrantID != g.ID {
		t.Fatalf("approve audit = %+v", evs)
	}
}

// An end in the past is refused on a create and on an update: nothing is
// stored or changed.
func TestGrantSetRefusesAnExpiryInThePast(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	stamp := func(d time.Duration) string { return time.Now().Add(d).UTC().Format("2006-01-02T15:04:05.000Z") }
	refused := func(what string, code int, body string) {
		t.Helper()
		if code != http.StatusBadRequest || kindErrCode(t, body) != proto.CodeInvalidArgument || !strings.Contains(body, "expires_at") {
			t.Fatalf("%s = %d %s, want invalid_argument naming expires_at", what, code, body)
		}
	}
	args := grantArgs(a1, a2, ops.GrantLevelView)
	args["expires_at"] = stamp(-time.Minute)
	code, body := k.call(ops.OpGrantSet, args)
	refused("create with a past expiry", code, body)
	if rows, _ := k.st.ListSessionGrants(context.Background(), store.SessionGrantFilter{}); len(rows) != 0 {
		t.Fatalf("stored %+v", rows)
	}
	// An end to come is accepted; changing it to the past is refused.
	args["expires_at"] = stamp(time.Hour)
	g := k.setGrant(args)
	args["id"], args["if_revision"], args["expires_at"] = g.ID, g.Revision, stamp(-time.Hour)
	code, body = k.call(ops.OpGrantSet, args)
	refused("update to a past expiry", code, body)
	row, err := k.st.SessionGrant(context.Background(), g.ID)
	if err != nil || row.Revision != g.Revision || row.ExpiresAt == nil || *row.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("after the refused update = %+v, %v", row, err)
	}
}

// grantEvents returns the stored audit events of one action, oldest first.
func (k *kindStack) grantEvents(action string) []audit.Event {
	k.t.Helper()
	rows, err := k.st.DB().QueryContext(context.Background(),
		`SELECT actor, acting_as, action, target, outcome, source, grant_id, detail FROM audit_events WHERE action = ? ORDER BY rowid`, action)
	if err != nil {
		k.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []audit.Event
	for rows.Next() {
		var ev audit.Event
		if err := rows.Scan(&ev.Actor, &ev.ActingAs, &ev.Action, &ev.Target, &ev.Outcome, &ev.Source, &ev.GrantID, &ev.Detail); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// An administrator grants between two Workers of ONE other user: the
// authority is owner (D4), so the owner can edit it and it does not hang on
// the administrator's role. The audit rows carry target, source, grant id and
// a detail without names; the grantor revokes it.
func TestGrantAdministratorBetweenWorkersOfOneOtherUserAndTheAudit(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.admin)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	if g.Authority != ops.GrantAuthorityOwner || g.GrantedBy != k.admin {
		t.Fatalf("grant = %+v", g)
	}
	evs := k.grantEvents(AuditActionGrantCreate)
	if len(evs) != 1 || evs[0].Target != a2.ID+":build" || evs[0].Source != a1.ID+":agent" || evs[0].GrantID != g.ID ||
		evs[0].Detail != "level=view authority=owner expires=none" || evs[0].Outcome != AuditOutcomeSuccess || evs[0].Actor == "" {
		t.Fatalf("create audit = %+v", evs)
	}
	// The owner edits the grant an administrator made for them, and it is
	// not suspended when that administrator loses the role.
	k.as(k.u1)
	up := grantArgs(a1, a2, ops.GrantLevelControl)
	up["id"], up["if_revision"] = g.ID, g.Revision
	g2 := k.setGrant(up)
	if _, err := k.st.DB().ExecContext(context.Background(), `DELETE FROM user_roles WHERE user_id = ? AND role = 'admin'`, k.admin); err != nil {
		t.Fatal(err)
	}
	if cur, err := k.st.SessionGrant(context.Background(), g.ID); err != nil || cur.SuspendedAt != nil || cur.Revision != g2.Revision {
		t.Fatalf("after the administrator's demotion: %+v (%v)", cur, err)
	}
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
	// A revoke at an old revision leaves the grant and is audited as failed.
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("revoke at an old revision = %d %s", code, body)
	}
	if _, err := k.st.SessionGrant(context.Background(), g.ID); err != nil {
		t.Fatalf("after the stale revoke: %v", err)
	}
	// Revoke with the revision; exactly one more row, written with the delete.
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g2.Revision}); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	evs = k.grantEvents(AuditActionGrantRevoke)
	want := "level=control authority=owner rev=" + strconv.FormatUint(g2.Revision, 10)
	if len(evs) != 2 || evs[0].Outcome == AuditOutcomeSuccess || evs[0].GrantID != g.ID || evs[1].Outcome != AuditOutcomeSuccess ||
		evs[1].GrantID != g.ID || evs[1].Source != a1.ID+":agent" || evs[1].Target != a2.ID+":build" || evs[1].Detail != want {
		t.Fatalf("revoke audit = %+v, want detail %q", evs, want)
	}
}

// D4: an administrator acting as a user in a browser session creates owner
// grants between that user's own Workers; the grant is the user's and the
// audit row names the administrator as acting. Between users it is refused,
// as are the other SessionOnly grant operations and an act-as API key.
func TestGrantActAsCreatesOwnerGrantsOnly(t *testing.T) {
	k := newGrantStack(t)
	ctx := context.Background()
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b := k.worker(k.u2, "b", store.WorkerKindPrivate)
	actAs := func(via string) {
		k.authn.mu.Lock()
		k.authn.id = Identity{Principal: authz.NewActAsPrincipal(k.admin, k.u1, time.Now().Add(time.Hour)), Actor: k.admin, ActingAs: k.u1, Via: via}
		k.authn.mu.Unlock()
	}
	actAs(ViaSession)
	if code, body := k.call(ops.OpGrantSet, grantArgs(a1, b, ops.GrantLevelView)); code != http.StatusForbidden {
		t.Fatalf("act-as grant_set between users = %d %s, want forbidden", code, body)
	}
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	if g.Authority != ops.GrantAuthorityOwner || g.GrantedBy != k.u1 {
		t.Fatalf("act-as grant = %+v", g)
	}
	var ok *audit.Event
	for _, ev := range k.grantEvents(AuditActionGrantCreate) {
		if ev.Outcome == AuditOutcomeSuccess {
			ok = &ev
		}
	}
	if ok == nil || ok.Actor != k.admin || ok.ActingAs != k.u1 || ok.GrantID != g.ID {
		t.Fatalf("act-as audit = %+v", k.grantEvents(AuditActionGrantCreate))
	}
	// grant_approve stays refused to an act-as session; grant_set to an
	// act-as identity that did not come through a browser session.
	if code, body := k.call(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != http.StatusForbidden {
		t.Fatalf("act-as grant_approve = %d %s, want forbidden", code, body)
	}
	actAs(ViaAPIKey)
	if code, body := k.call(ops.OpGrantSet, grantArgs(a2, a1, ops.GrantLevelView)); code != http.StatusForbidden {
		t.Fatalf("act-as grant_set not via a session = %d %s, want forbidden", code, body)
	}
	if rows, _ := k.st.ListSessionGrants(ctx, store.SessionGrantFilter{}); len(rows) != 1 || rows[0].ID != g.ID {
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

// The administrator loses the role between the handler's check and the store
// write (the authorizer of this stack keeps saying administrator): the store
// reads the role in its own transaction and nothing commits.
func TestGrantDemotedAdministratorCommitsNothing(t *testing.T) {
	k := newGrantStack(t)
	ctx := context.Background()
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u2, "b", store.WorkerKindPrivate)
	k.as(k.admin)
	g := k.setGrant(grantArgs(a, b, ops.GrantLevelView))
	if _, err := k.st.DB().ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND role = 'admin'`, k.admin); err != nil {
		t.Fatal(err)
	}
	cur, err := k.st.SessionGrant(ctx, g.ID)
	if err != nil || cur.SuspendedAt == nil {
		t.Fatalf("after the demotion: %+v (%v)", cur, err)
	}
	if code, body := k.call(ops.OpGrantSet, grantArgs(b, a, ops.GrantLevelControl)); code != http.StatusForbidden {
		t.Fatalf("create by a demoted administrator = %d %s", code, body)
	}
	if code, body := k.call(ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": cur.Revision}); code != http.StatusForbidden {
		t.Fatalf("approve by a demoted administrator = %d %s", code, body)
	}
	rows, _ := k.st.ListSessionGrants(ctx, store.SessionGrantFilter{})
	if len(rows) != 1 || rows[0].Revision != cur.Revision || rows[0].SuspendedAt == nil || rows[0].Level != 1 {
		t.Fatalf("stored = %+v", rows)
	}
	// The refusals are audited as denied; no success row was written.
	if evs := k.grantEvents(AuditActionGrantApprove); len(evs) != 1 || evs[0].Outcome != AuditOutcomeDenied || evs[0].GrantID != g.ID {
		t.Fatalf("approve audit = %+v", evs)
	}
	if evs := k.grantEvents(AuditActionGrantCreate); len(evs) != 2 || evs[1].Outcome != AuditOutcomeDenied {
		t.Fatalf("create audit = %+v", evs)
	}
	// The former grantor, who owns neither Worker, still withdraws the grant.
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": cur.Revision}); code != http.StatusOK {
		t.Fatalf("revoke by the demoted grantor = %d %s", code, body)
	}
	if _, err := k.st.SessionGrant(ctx, g.ID); err == nil {
		t.Fatal("the grant is still stored")
	}
}

// grantKeyScope is an API key scope with read and operate on some Workers.
type grantKeyScope map[string]bool

func (s grantKeyScope) Actions(res authz.ResolvedResource) []ops.Action {
	if res.Kind == authz.ResourceWorker && s[res.WorkerID] {
		return []ops.Action{ops.ActionRead, ops.ActionOperate}
	}
	return nil
}

// Revoke (D4): an API key limited to the target Worker and the owner of the
// source may revoke; a user who owns neither Worker and did not decide the
// grant may not, and neither may a key of the grantor without the Workers.
func TestGrantRevokeByScopedKeyAndRefusals(t *testing.T) {
	k := newGrantStack(t)
	ctx := context.Background()
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u2, "b", store.WorkerKindPrivate)
	c := k.worker(k.u2, "c", store.WorkerKindPrivate)
	k.as(k.admin)
	g := k.setGrant(grantArgs(a, b, ops.GrantLevelView))
	key := func(user string, scope authz.Scope) {
		k.authn.mu.Lock()
		k.authn.id = Identity{Principal: authz.NewAPIKeyPrincipal(user, scope, ""), Actor: "key:k1", Via: ViaAPIKey}
		k.authn.mu.Unlock()
	}
	// A key of the administrator limited to another Worker is refused.
	key(k.admin, grantKeyScope{c.ID: true})
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code == http.StatusOK {
		t.Fatalf("revoke by a key of another Worker = %d %s", code, body)
	}
	if _, err := k.st.SessionGrant(ctx, g.ID); err != nil {
		t.Fatalf("the grant is gone: %v", err)
	}
	// The same key limited to the target Worker alone revokes.
	key(k.admin, grantKeyScope{b.ID: true})
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != http.StatusOK {
		t.Fatalf("revoke by a key of the target Worker = %d %s", code, body)
	}
	// The owner of the source revokes a grant on another owner's Worker.
	k.as(k.admin)
	g = k.setGrant(grantArgs(a, b, ops.GrantLevelView))
	k.as(k.u1)
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != http.StatusOK {
		t.Fatalf("revoke by the source's owner = %d %s", code, body)
	}
	// A user who owns neither Worker of a grant is refused.
	k.as(k.admin)
	g = k.setGrant(grantArgs(b, c, ops.GrantLevelView)) // both bob's
	k.as(k.u1)
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code == http.StatusOK {
		t.Fatalf("revoke by a stranger = %d %s", code, body)
	}
	if _, err := k.st.SessionGrant(ctx, g.ID); err != nil {
		t.Fatalf("the grant is gone: %v", err)
	}
	// The person who decided the grant withdraws it although they own
	// neither Worker and are no administrator; their API key does not.
	if _, err := k.st.DB().ExecContext(ctx, `UPDATE session_grant SET granted_by = ? WHERE id = ?`, k.u1, g.ID); err != nil {
		t.Fatal(err)
	}
	key(k.u1, grantKeyScope{})
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code == http.StatusOK {
		t.Fatalf("revoke by a key of the grantor = %d %s", code, body)
	}
	k.as(k.u1)
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != http.StatusOK {
		t.Fatalf("revoke by the grantor = %d %s", code, body)
	}
	if _, err := k.st.SessionGrant(ctx, g.ID); err == nil {
		t.Fatal("the grant is still stored")
	}
	evs := k.grantEvents(AuditActionGrantRevoke)
	if last := evs[len(evs)-1]; last.Outcome != AuditOutcomeSuccess || last.Actor != k.u1 || last.GrantID != g.ID || last.Target != c.ID+":build" {
		t.Fatalf("revoke audit = %+v", evs)
	}
}

// grant_list masks, for a viewer who is no administrator, the session name on
// the side they do not own; the id, the Workers and the level stay.
func TestGrantListMasksTheOtherOwnersSessionName(t *testing.T) {
	k := newGrantStackWith(t, func(w *GrantWorker) { w.Owner = "owner-" + w.Name })
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u2, "b", store.WorkerKindPrivate)
	k.as(k.admin)
	g := k.setGrant(grantArgs(a, b, ops.GrantLevelControl))
	whole := grantArgs(b, a, ops.GrantLevelView)
	whole["target_session"] = ""
	k.setGrant(whole)
	if l := k.listGrants(); len(l) != 2 || l[0].SourceSession != "agent" || l[0].TargetSession != "build" || l[0].SourceMasked || l[0].TargetMasked {
		t.Fatalf("an administrator lists %+v", l)
	}
	k.as(k.u1) // owns a, the source of the first grant
	l := k.listGrants()
	if len(l) != 2 || l[0].ID != g.ID || l[0].SourceSession != "agent" || l[0].SourceMasked ||
		l[0].TargetSession != "a session of owner-b" || !l[0].TargetMasked || l[0].Level != ops.GrantLevelControl ||
		l[0].TargetWorkerID != b.ID || l[0].TargetWorkerName != "b" {
		t.Fatalf("the source's owner lists %+v", l)
	}
	// The whole-Worker target names no session; its source is bob's.
	if l[1].SourceSession != "a session of owner-b" || !l[1].SourceMasked || l[1].TargetSession != "" || l[1].TargetMasked {
		t.Fatalf("the target's owner lists %+v", l[1])
	}
	k.as(k.u2) // owns b
	l = k.listGrants()
	if len(l) != 2 || l[0].SourceSession != "a session of owner-a" || !l[0].SourceMasked || l[0].TargetSession != "build" || l[0].TargetMasked ||
		l[1].SourceSession != "agent" || l[1].SourceMasked {
		t.Fatalf("the target's owner lists %+v", l)
	}
	for _, gr := range l {
		if strings.Contains(gr.SourceSession+gr.TargetSession, "agent") && gr.SourceMasked {
			t.Fatalf("a masked row carries the name: %+v", gr)
		}
	}
}

// grant_revoke carries the revision the caller saw (D2): without one it is
// refused, with a stale one it conflicts, and the grant stays either way.
func TestGrantRevokeNeedsTheRevision(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelView))
	up := grantArgs(a1, a2, ops.GrantLevelControl)
	up["id"], up["if_revision"] = g.ID, g.Revision
	g2 := k.setGrant(up)
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID}); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("revoke without a revision = %d %s", code, body)
	}
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); kindErrCode(t, body) != proto.CodeConflict {
		t.Fatalf("revoke at a stale revision = %d %s", code, body)
	}
	if cur, err := k.st.SessionGrant(context.Background(), g.ID); err != nil || cur.Revision != g2.Revision {
		t.Fatalf("after the refusals: %+v (%v)", cur, err)
	}
	if code, body := k.call(ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g2.Revision}); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
}
