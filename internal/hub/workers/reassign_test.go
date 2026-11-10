package workers

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// kindRig is a store with an admin and two users, a registry with fake online
// sessions and the services of Story 6.6 on top of them.
type kindRig struct {
	t             *testing.T
	st            *store.Store
	reg           *Registry
	audit         *audit.Log
	re            *Reassigner
	rs            *Restrictions
	admin, u1, u2 string
	adminActor    Actor
}

func newKindRig(t *testing.T) *kindRig {
	t.Helper()
	st := openStore(t, t.TempDir())
	r := &kindRig{t: t, st: st, reg: NewRegistry(), audit: audit.New(st)}
	ctx := context.Background()
	add := func(name, role string) string {
		u := store.UserRow{ID: store.NewID(), Username: name, PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
		if err := st.CreateUser(ctx, store.CreateUserParams{User: u, Role: role}); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	r.admin, r.u1, r.u2 = add("root", "admin"), add("alice", "user"), add("bob", "user")
	r.adminActor = Actor{ID: r.admin}
	var err error
	if r.re, err = NewReassigner(ReassignConfig{Store: st, Connections: r.reg, Auditor: r.audit}); err != nil {
		t.Fatal(err)
	}
	if r.rs, err = NewRestrictions(RestrictionsConfig{Store: st, Connections: r.reg, Auditor: r.audit}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *kindRig) worker(owner, name, kind string) store.WorkerRow {
	r.t.Helper()
	w := store.WorkerRow{
		ID: store.NewID(), Pub: newKey(r.t).Public(), Name: name, OwnerID: owner, Kind: kind, CreatedAt: time.Now().UnixMilli(),
	}
	if err := r.st.InsertWorker(context.Background(), nil, w); err != nil {
		r.t.Fatal(err)
	}
	return w
}

// online puts the Worker online with a fake session.
func (r *kindRig) online(w store.WorkerRow) *fakeSession {
	s := newFakeSession()
	r.reg.Add(Online{WorkerID: w.ID, Name: w.Name, OwnerID: w.OwnerID, Kind: w.Kind}, s)
	return s
}

func (r *kindRig) auditRows(action string) []store.AuditRow {
	r.t.Helper()
	rows, err := r.st.ListAudit(context.Background(), store.AuditQuery{Action: action, Limit: 20})
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

func wantCode(t *testing.T, err error, want proto.Code) {
	t.Helper()
	if got := proto.CodeOf(err); got != want {
		t.Fatalf("error = %v (code %q), want code %q", err, got, want)
	}
}

func TestReassignChangesOwnerDropsTrustRevokesBoundKeysInOneStep(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	for _, tr := range []store.TrustRow{
		{ReceiverID: r.u1, WorkerID: w.ID, CreatedAt: 1},
		{ReceiverID: r.u2, WorkerID: w.ID, SessionName: "minion-1", CreatedAt: 1},
		{ReceiverID: r.admin, WorkerID: w.ID, CreatedAt: 1}, // the admin's own trust goes too
	} {
		if err := r.st.PutTrust(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	key := store.APIKeyRow{
		ID: store.NewID(), OwnerID: r.u1, Name: "bound", Hash: bytes.Repeat([]byte{7}, 32), Hint: "abcdefgh",
		Actions: []string{"message"}, BoundWorkerID: w.ID, CreatedAt: 1,
		Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: w.ID}},
	}
	if err := r.st.CreateAPIKey(ctx, key, nil); err != nil {
		t.Fatal(err)
	}
	sess := r.online(w)

	res, err := r.re.Reassign(ctx, r.adminActor, w.ID, r.u2)
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustDeleted != 3 || res.KeysRevoked != 1 || !res.Disconnected {
		t.Errorf("result = %+v", res)
	}
	got, _ := r.st.WorkerByID(ctx, w.ID)
	if got.OwnerID != r.u2 || got.Kind != store.WorkerKindInfrastructure || got.Name != "pi" {
		t.Errorf("worker = %+v", got)
	}
	for _, rcv := range []string{r.u1, r.u2, r.admin} {
		if rows, _ := r.st.ListTrust(ctx, rcv); len(rows) != 0 {
			t.Errorf("trust of %s survived: %+v", rcv, rows)
		}
	}
	if k, _ := r.st.APIKeyByID(ctx, key.ID); k.RevokedAt == 0 {
		t.Error("the bound key is still live")
	}
	if !sess.isClosed() {
		t.Error("the online Worker was not disconnected, so it would keep serving under the old owner")
	}
}

func TestReassignIsAuditedWithTheKind(t *testing.T) {
	r := newKindRig(t)
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	actor := Actor{ID: r.admin, ActingAs: r.u1}
	if _, err := r.re.Reassign(context.Background(), actor, w.ID, r.u2); err != nil {
		t.Fatal(err)
	}
	rows := r.auditRows(AuditWorkerReassign)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	row := rows[0]
	if row.Actor != r.admin || row.ActingAs != r.u1 || row.Outcome != "success" ||
		!strings.HasPrefix(row.Target, w.ID) || !strings.Contains(row.Target, "kind=infrastructure") || !strings.Contains(row.Target, r.u2) {
		t.Errorf("audit row = %+v", row)
	}
	if ev := (audit.Event{Actor: row.Actor, Action: row.Action, Target: row.Target, Outcome: row.Outcome}); ev.Validate() != nil {
		t.Errorf("the audit row is not a valid audit event: %v", ev.Validate())
	}
}

func TestReassignOfAPrivateWorkerIsForbiddenForEveryone(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	w := r.worker(r.u1, "mine", store.WorkerKindPrivate)
	if err := r.st.PutTrust(ctx, store.TrustRow{ReceiverID: r.u1, WorkerID: w.ID, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sess := r.online(w)

	// Admin, owner and the would-be new owner: the service never looks at who
	// asks, a private Worker is not reassigned.
	for _, actor := range []Actor{r.adminActor, {ID: r.u1}, {ID: r.u2}} {
		_, err := r.re.Reassign(ctx, actor, w.ID, r.u2)
		wantCode(t, err, proto.CodeForbidden)
	}
	got, _ := r.st.WorkerByID(ctx, w.ID)
	if got.OwnerID != r.u1 {
		t.Errorf("owner = %s", got.OwnerID)
	}
	if ok, _ := r.st.HasTrust(ctx, r.u1, w.ID, ""); !ok {
		t.Error("trust was dropped by a refused reassignment")
	}
	if sess.isClosed() {
		t.Error("a refused reassignment disconnected the Worker")
	}
	rows := r.auditRows(AuditWorkerReassign)
	if len(rows) != 3 || rows[0].Outcome != string(proto.CodeForbidden) || !strings.Contains(rows[0].Target, "kind=private") {
		t.Errorf("refusals are audited with the kind: %+v", rows)
	}
}

func TestReassignNameCollisionIsAConflictAndChangesNothing(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	r.worker(r.u2, "pi", store.WorkerKindPrivate) // V already has a Worker called pi
	if err := r.st.PutTrust(ctx, store.TrustRow{ReceiverID: r.admin, WorkerID: w.ID, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sess := r.online(w)

	_, err := r.re.Reassign(ctx, r.adminActor, w.ID, r.u2)
	wantCode(t, err, proto.CodeConflict)
	got, _ := r.st.WorkerByID(ctx, w.ID)
	if got.OwnerID != r.u1 {
		t.Errorf("owner = %s after a refused reassignment", got.OwnerID)
	}
	if ok, _ := r.st.HasTrust(ctx, r.admin, w.ID, ""); !ok {
		t.Error("trust was dropped by a refused reassignment")
	}
	if sess.isClosed() {
		t.Error("a refused reassignment disconnected the Worker")
	}
	if rows := r.auditRows(AuditWorkerReassign); len(rows) != 1 || rows[0].Outcome != string(proto.CodeConflict) {
		t.Errorf("audit = %+v", rows)
	}
}

func TestReassignRefusals(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	gone := store.NewID()
	if _, err := r.st.DeactivateUser(ctx, store.DeactivateUserParams{UserID: r.u2, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		actor        Actor
		worker, user string
		want         proto.Code
	}{
		{"unknown worker", r.adminActor, store.NewID(), r.u1, proto.CodeNotFound},
		{"unknown user", r.adminActor, w.ID, gone, proto.CodeNotFound},
		{"deactivated user", r.adminActor, w.ID, r.u2, proto.CodeConflict},
		{"same owner", r.adminActor, w.ID, r.u1, proto.CodeInvalidArgument},
		{"no actor", Actor{}, w.ID, r.admin, proto.CodeForbidden},
	}
	for _, tc := range tests {
		_, err := r.re.Reassign(ctx, tc.actor, tc.worker, tc.user)
		if got := proto.CodeOf(err); got != tc.want {
			t.Errorf("%s: code = %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
	if got, _ := r.st.WorkerByID(ctx, w.ID); got.OwnerID != r.u1 {
		t.Errorf("owner = %s", got.OwnerID)
	}
}

func TestReassignOfAnOfflineWorkerDoesNotDisconnectAnything(t *testing.T) {
	r := newKindRig(t)
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	bystander := r.online(r.worker(r.u1, "other", store.WorkerKindInfrastructure))
	res, err := r.re.Reassign(context.Background(), r.adminActor, w.ID, r.u2)
	if err != nil || res.Disconnected {
		t.Fatalf("result = %+v err=%v", res, err)
	}
	if bystander.isClosed() {
		t.Error("another Worker was disconnected")
	}
}

func TestRenameUniqueWithinTheOwnersSpace(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	a := r.worker(r.u1, "pi", store.WorkerKindPrivate)
	r.worker(r.u1, "nuc", store.WorkerKindPrivate)
	r.worker(r.u2, "lab", store.WorkerKindPrivate)
	sess := r.online(a)

	wantCode(t, r.re.Rename(ctx, Actor{ID: r.u1}, a.ID, "nuc"), proto.CodeConflict)
	if sess.isClosed() {
		t.Error("a refused rename disconnected the Worker")
	}
	if err := r.re.Rename(ctx, Actor{ID: r.u1}, a.ID, "lab"); err != nil { // free in this owner's space
		t.Fatal(err)
	}
	if got, _ := r.st.WorkerByID(ctx, a.ID); got.Name != "lab" {
		t.Errorf("name = %q", got.Name)
	}
	if !sess.isClosed() {
		t.Error("the renamed Worker keeps an old welcome: it was not disconnected")
	}
	if rows := r.auditRows(AuditWorkerRename); len(rows) != 2 || rows[0].Outcome != "success" || rows[1].Outcome != string(proto.CodeConflict) {
		t.Errorf("audit = %+v", rows)
	}
	wantCode(t, r.re.Rename(ctx, Actor{ID: r.u1}, store.NewID(), "x"), proto.CodeNotFound)
	wantCode(t, r.re.Rename(ctx, Actor{}, a.ID, "y"), proto.CodeForbidden)
}

func TestRenameToTheCurrentNameChangesNothing(t *testing.T) {
	r := newKindRig(t)
	a := r.worker(r.u1, "pi", store.WorkerKindPrivate)
	sess := r.online(a)
	if err := r.re.Rename(context.Background(), Actor{ID: r.u1}, a.ID, "pi"); err != nil {
		t.Fatal(err)
	}
	if sess.isClosed() || len(r.auditRows(AuditWorkerRename)) != 0 {
		t.Error("a no-op rename disconnected the Worker or wrote an audit row")
	}
}

func TestNewReassignerAndRestrictionsNeedAStore(t *testing.T) {
	if _, err := NewReassigner(ReassignConfig{}); err == nil {
		t.Error("NewReassigner accepted no store")
	}
	if _, err := NewRestrictions(RestrictionsConfig{}); err == nil {
		t.Error("NewRestrictions accepted no store")
	}
}

// Reassignments racing for the same Worker end with one
// consistent owner.
func TestConcurrentReassignmentsEndWithOneOwner(t *testing.T) {
	r := newKindRig(t)
	ctx := context.Background()
	w := r.worker(r.u1, "pi", store.WorkerKindInfrastructure)
	var wg sync.WaitGroup
	for _, to := range []string{r.u2, r.admin, r.u2, r.admin} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.re.Reassign(ctx, r.adminActor, w.ID, to)
		}()
	}
	wg.Wait()
	got, _ := r.st.WorkerByID(ctx, w.ID)
	if got.OwnerID != r.u2 && got.OwnerID != r.admin {
		t.Errorf("owner = %s", got.OwnerID)
	}
}
