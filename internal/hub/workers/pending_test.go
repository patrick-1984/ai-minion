package workers

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// F-02: enrollment is not authorization. These tests run against the real
// endpoint, registry and store.

// enrolPending inserts a Worker row that waits for approval.
func (h *testHub) enrolPending(key *identity.Key, owner, name string) store.WorkerRow {
	h.t.Helper()
	w := store.WorkerRow{
		ID: store.NewID(), Pub: key.Public(), Name: name, OwnerID: owner, Kind: store.WorkerKindPrivate,
		CreatedAt: time.Now().UnixMilli(), Pending: true, EnrollAddr: "192.0.2.50",
	}
	if err := h.st.InsertWorker(context.Background(), nil, w); err != nil {
		h.t.Fatal(err)
	}
	return w
}

func answerCode(t *testing.T, typ string, body json.RawMessage) proto.Code {
	t.Helper()
	if typ != proto.TypeError {
		t.Fatalf("answer = %s, want an error frame", body)
	}
	var f proto.ErrorFrame
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	return f.Code
}

func callRequestOf(op string) map[string]any {
	id := ops.NewUUIDv7()
	return map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": id, "args": map[string]any{}}
}

func TestPendingWorkerConnectsButCannotCallOrBeOperated(t *testing.T) {
	h := newTestHub(t, nil)
	var served, hooked atomic.Int32
	h.svc.HandleCallOp(ops.OpPipeRead, func(_ context.Context, c Call) {
		served.Add(1)
		_ = c.Stream.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": map[string]any{}})
	})
	h.svc.Registry().OnAdd(func(Online, OpSession) { hooked.Add(1) })

	key := newKey(t)
	w := h.enrolPending(key, "owner-1", "stolen")
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	if welcome := decodeWelcome(t, res); !welcome.Pending || welcome.WorkerID != w.ID {
		t.Fatalf("welcome = %+v, want pending", welcome)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the pending Worker to be connected", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	// Connected, but no op stream is ever opened towards it, and no module hook
	// takes its session.
	on, _ := h.svc.Registry().Get(w.ID)
	if !on.Pending {
		t.Errorf("online = %+v, want pending", on)
	}
	if _, err := h.svc.Registry().OpenOp(context.Background(), w.ID, "send_text"); proto.CodeOf(err) != proto.CodeWorkerPending {
		t.Errorf("OpenOp on a pending Worker: %v", err)
	}
	if hooked.Load() != 0 {
		t.Errorf("a registry hook saw the pending Worker (%d)", hooked.Load())
	}

	// Its call-stream requests are refused, whatever the op is.
	for _, op := range []string{ops.OpPipeRead, ops.OpPipeSend, "sessions_list", "anything"} {
		typ, body := callOnce(t, sess, callRequestOf(op))
		if got := answerCode(t, typ, body); got != proto.CodeWorkerPending {
			t.Errorf("call %s: code = %s, want worker_pending", op, got)
		}
	}
	if served.Load() != 0 {
		t.Errorf("an op was served for a pending Worker (%d)", served.Load())
	}
}

func TestPendingWorkerCanRetireItself(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrolPending(key, "owner-1", "pi")
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	eventually(t, "the pending Worker to be connected", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	typ, body := callOnce(t, sess, retireRequest())
	if typ != proto.TypeOpResult {
		t.Fatalf("self-retirement of a pending Worker was refused: %s", body)
	}
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); err == nil {
		t.Error("the pending Worker is still enrolled after it retired itself")
	}
	if _, err := h.st.TombstoneByPub(context.Background(), key.Public()); err != nil {
		t.Errorf("no tombstone: %v", err)
	}
}

func TestApprovalEnablesOpsAndCallsAfterTheReconnect(t *testing.T) {
	h := newTestHub(t, nil)
	var served atomic.Int32
	h.svc.HandleCallOp(ops.OpPipeRead, func(_ context.Context, c Call) {
		served.Add(1)
		_ = c.Stream.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": map[string]any{}})
	})
	key := newKey(t)
	w := h.enrolPending(key, "owner-1", "pi")
	sess := h.admittedPending(key)
	eventually(t, "the pending Worker to be connected", func() bool { return h.svc.Registry().IsOnline(w.ID) })

	ap, err := NewApprover(ApproverConfig{Store: h.st, Connections: h.svc.Registry()})
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: "owner-1"}

	// A wrong fingerprint approves nothing.
	if _, err := ap.Approve(context.Background(), actor, w.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("wrong fingerprint: %v", err)
	}
	if row, _ := h.st.WorkerByID(context.Background(), w.ID); !row.Pending {
		t.Fatal("a wrong fingerprint approved the Worker")
	}

	fp, _ := identity.FingerprintOf(key.Public())
	r, err := ap.Approve(context.Background(), actor, w.ID, fp.String())
	if err != nil || !r.Changed || !r.Disconnected {
		t.Fatalf("approve = %+v err=%v", r, err)
	}
	// The pending connection is ended; the Worker reconnects as an approved Worker.
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the pending connection was not ended by the approval")
	}
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	if welcome := decodeWelcome(t, res); welcome.Pending {
		t.Fatalf("welcome after approval = %+v", welcome)
	}
	sess2, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	eventually(t, "the approved Worker to be online", func() bool {
		on, ok := h.svc.Registry().Get(w.ID)
		return ok && !on.Pending
	})
	st, err := h.svc.Registry().OpenOp(context.Background(), w.ID, "send_text")
	if err != nil {
		t.Fatalf("OpenOp after approval: %v", err)
	}
	st.Reset()
	typ, body := callOnce(t, sess2, callRequestOf(ops.OpPipeRead))
	if typ != proto.TypeOpResult || served.Load() != 1 {
		t.Errorf("call after approval: %s (served %d)", body, served.Load())
	}

	// The approval is audited once, with the actor; a repeat changes nothing.
	r, err = ap.Approve(context.Background(), actor, w.ID, "")
	if err != nil || r.Changed {
		t.Errorf("second approve = %+v err=%v", r, err)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: AuditWorkerApprove})
	if err != nil || len(page.Events) != 1 || page.Events[0].Actor != "owner-1" || page.Events[0].Target != w.ID {
		t.Errorf("approve audit = %+v err=%v", page, err)
	}
	if _, err := ap.Approve(context.Background(), actor, store.NewID(), ""); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("unknown Worker: %v", err)
	}
}

// racyRepo approves the Worker right after the first read of its row, as an
// approval that commits between the connect and the registration would.
type racyRepo struct {
	Repository
	st   *store.Store
	once atomic.Bool
}

func (r *racyRepo) WorkerByPub(ctx context.Context, pub []byte) (store.WorkerRow, error) {
	w, err := r.Repository.WorkerByPub(ctx, pub)
	if err == nil && w.Pending && r.once.CompareAndSwap(false, true) {
		if _, aerr := r.st.ApproveWorker(ctx, w.ID, "owner-1", time.Now().UnixMilli(), nil); aerr != nil {
			panic(aerr)
		}
	}
	return w, err
}

// An approval that commits between the read of the Worker row and its
// registration is not lost: the connection that was welcomed as pending ends
// itself when it sees the row is approved, and the reconnect is approved.
func TestApprovalDuringTheConnectEndsThePendingConnection(t *testing.T) {
	h := newTestHub(t, nil)
	h.svc.handler.cfg.Repository = &racyRepo{Repository: h.svc.handler.cfg.Repository, st: h.st}
	key := newKey(t)
	h.enrolPending(key, "owner-1", "pi")
	sess := h.admittedPending(key)
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stale pending connection stayed open after an approval that committed during the connect")
	}
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		t.Fatal(err)
	}
	if decodeWelcome(t, res).Pending {
		t.Error("the reconnect is still pending")
	}
}

// Self-approval is not possible through the authorizer: a Worker identity holds
// neither operate nor manage.
func TestAWorkerPrincipalCannotApproveItself(t *testing.T) {
	az := authz.New(fakeUsers{"owner-1": {Role: authz.RoleUser, Active: true}}, &fakeDir{recs: []WorkerRecord{{ID: "w1", OwnerID: "owner-1"}}})
	p := authz.Principal{Kind: authz.KindWorker, WorkerID: "w1", UserID: "owner-1"}
	err := az.Decide(context.Background(), p, ops.MustActionSet(ops.ActionOperate, ops.ActionManage), authz.WorkerResource{ID: "w1"})
	if err == nil {
		t.Fatal("a Worker identity may approve (operate/manage) its own Worker")
	}
}

// The directory used for routing lists approved Workers only; the full list is
// for the Workers list and the approval.
func TestDirectoryHidesPendingWorkersFromRouting(t *testing.T) {
	h := newTestHub(t, nil)
	ok := h.enrol(newKey(t), "owner-1", "approved")
	pend := h.enrolPending(newKey(t), "owner-1", "pending")
	d := StoreDirectory{Store: h.st}
	routable, err := d.Workers(context.Background())
	if err != nil || len(routable) != 1 || routable[0].ID != ok.ID {
		t.Fatalf("Workers = %+v err=%v", routable, err)
	}
	all, err := d.AllWorkers(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("AllWorkers = %+v err=%v", all, err)
	}
	for _, w := range all {
		if w.ID == pend.ID && (!w.Pending || w.Fingerprint == "" || w.EnrollAddr != "192.0.2.50") {
			t.Errorf("pending record = %+v", w)
		}
	}
}

// workers_list shows a pending Worker with its fingerprint (the owner and
// admins need it to approve); resolving it as a target finds nothing.
type allDir struct{ *fakeDir }

func (d allDir) Workers(ctx context.Context) ([]WorkerRecord, error) {
	all, err := d.fakeDir.Workers(ctx)
	var out []WorkerRecord
	for _, w := range all {
		if !w.Pending {
			out = append(out, w)
		}
	}
	return out, err
}

func (d allDir) AllWorkers(ctx context.Context) ([]WorkerRecord, error) {
	return d.fakeDir.Workers(ctx)
}

func TestWorkersListShowsPendingButTargetsDoNotResolve(t *testing.T) {
	pending := WorkerRecord{ID: ops.NewUUIDv7(), Name: "fresh", OwnerID: idAlice, Kind: "private", Fingerprint: "fp-fresh", Pending: true, EnrollAddr: "198.51.100.4"}
	fd := &fakeDir{recs: []WorkerRecord{wAlicePi1, pending}}
	az := authz.New(fakeUsers{idAlice: {Role: authz.RoleUser, Active: true}}, fd)
	rt, err := NewRouter(RouterConfig{Backend: &fakeBackend{online: map[string]Online{}}, Directory: allDir{fd}, Authz: az})
	if err != nil {
		t.Fatal(err)
	}
	alice := authz.Principal{Kind: authz.KindBrowserSession, UserID: idAlice}
	list, err := rt.ListWorkers(context.Background(), alice, false)
	if err != nil {
		t.Fatal(err)
	}
	var found *ops.WorkerInfo
	for i := range list {
		if list[i].ID == pending.ID {
			found = &list[i]
		}
	}
	if found == nil || !found.Pending || found.Fingerprint != "fp-fresh" || found.EnrollAddr != "198.51.100.4" {
		t.Fatalf("pending Worker in the list = %+v", found)
	}
	if _, err := rt.Resolve(context.Background(), alice, "fresh:main", false); proto.CodeOf(err) != proto.CodeTargetNotFound {
		t.Errorf("Resolve of a pending Worker: %v", err)
	}
}

// admittedPending dials as a pending Worker and returns the Worker side.
func (h *testHub) admittedPending(key *identity.Key) *transport.Session {
	h.t.Helper()
	res, err := h.dial(h.workerConfig(key, "send_text"))
	if err != nil {
		h.t.Fatal(err)
	}
	if !decodeWelcome(h.t, res).Pending {
		h.t.Fatal("the welcome did not say pending")
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = sess.Close() })
	return sess
}
