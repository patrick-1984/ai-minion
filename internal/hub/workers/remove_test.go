package workers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// removeRig is a real store and audit log with trust and bound-key stand-ins:
// two small tables that the removal transaction must empty together with the
// tombstone (the real tables belong to the authz and apikey domains).
type removeRig struct {
	t     *testing.T
	st    *store.Store
	audit *audit.Log
	rm    *Remover
	live  *fakeLive
}

type fakeLive struct {
	mu     sync.Mutex
	calls  []string
	online bool
	conf   bool
	err    error
	// check runs at the time of the call, to observe the database then.
	check func()
}

func (f *fakeLive) RevokeOnline(_ context.Context, id string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if f.check != nil {
		f.check()
	}
	return f.online, f.conf, f.err
}

func (f *fakeLive) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newRemoveRig(t *testing.T) *removeRig {
	t.Helper()
	st := openStore(t, t.TempDir())
	for _, ddl := range []string{
		`CREATE TABLE test_trust (receiver TEXT NOT NULL, worker_id TEXT NOT NULL)`,
		`CREATE TABLE test_keys (key_id TEXT NOT NULL, bound_worker_id TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err := st.DB().Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	r := &removeRig{t: t, st: st, audit: audit.New(st), live: &fakeLive{}}
	rm, err := NewRemover(RemoveConfig{
		Store: st, Auditor: r.audit, Live: r.live,
		Trust: CleanupFunc(func(ctx context.Context, tx *sql.Tx, id string) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM test_trust WHERE worker_id = ?`, id)
			return err
		}),
		Keys: CleanupFunc(func(ctx context.Context, tx *sql.Tx, id string) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM test_keys WHERE bound_worker_id = ?`, id)
			return err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.rm = rm
	return r
}

func (r *removeRig) enrol(owner, name string) (*identity.Key, store.WorkerRow) {
	r.t.Helper()
	key := newKey(r.t)
	w := store.WorkerRow{
		ID: store.NewID(), Pub: key.Public(), Name: name, OwnerID: owner,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	if err := r.st.InsertWorker(context.Background(), nil, w); err != nil {
		r.t.Fatal(err)
	}
	return key, w
}

func (r *removeRig) count(table string) int {
	r.t.Helper()
	var n int
	if err := r.st.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *removeRig) seed(workerID string) {
	r.t.Helper()
	for _, q := range []string{
		`INSERT INTO test_trust VALUES ('u2', '` + workerID + `'), ('admin', '` + workerID + `'), ('u2', 'other-worker')`,
		`INSERT INTO test_keys (key_id, bound_worker_id) VALUES ('k1', '` + workerID + `'), ('k2', 'other-worker')`,
	} {
		if _, err := r.st.DB().Exec(q); err != nil {
			r.t.Fatal(err)
		}
	}
}

func TestRemoveTurnsTheWorkerIntoATombstoneAndDropsTrustAndBoundKeysInOneTransaction(t *testing.T) {
	r := newRemoveRig(t)
	key, w := r.enrol("owner-1", "pi")
	_, other := r.enrol("owner-1", "kept")
	r.seed(w.ID)
	ctx := context.Background()

	// The commit is complete before the live revoke is attempted.
	r.live.check = func() {
		if _, err := r.st.WorkerByID(ctx, w.ID); !errors.Is(err, store.ErrWorkerNotFound) {
			t.Errorf("the Worker row still exists when the revoke is sent: %v", err)
		}
		if _, err := r.st.TombstoneByPub(ctx, key.Public()); err != nil {
			t.Errorf("no tombstone when the revoke is sent: %v", err)
		}
	}
	res, err := r.rm.Remove(ctx, RemoveActor{ID: "owner-1"}, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.WorkerID != w.ID || res.Name != "pi" {
		t.Errorf("result = %+v", res)
	}

	tomb, err := r.st.TombstoneByPub(ctx, key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tomb.Pub, key.Public()) || tomb.WorkerID != w.ID || tomb.OwnerID != "owner-1" ||
		tomb.RemovedBy != "owner-1" || tomb.CreatedAt != w.CreatedAt || tomb.RemovedAt < w.CreatedAt {
		t.Errorf("tombstone = %+v", tomb)
	}
	if _, err := r.st.WorkerByPub(ctx, key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the Worker row survived: %v", err)
	}
	if owner, err := r.st.WorkerOwner(ctx, w.ID); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("a removed Worker still has an owner %q: %v", owner, err)
	}
	// Only this Worker's trust rows and keys went; everything else stays.
	if n := r.count(`test_trust WHERE worker_id = '` + w.ID + `'`); n != 0 {
		t.Errorf("%d trust rows of the removed Worker remain", n)
	}
	if n := r.count(`test_keys WHERE bound_worker_id = '` + w.ID + `'`); n != 0 {
		t.Errorf("%d bound keys of the removed Worker remain", n)
	}
	if r.count("test_trust") != 1 || r.count("test_keys") != 1 {
		t.Error("the removal touched another Worker's trust or keys")
	}
	if _, err := r.st.WorkerByID(ctx, other.ID); err != nil {
		t.Errorf("another Worker was removed: %v", err)
	}
	if got := r.live.called(); len(got) != 1 || got[0] != w.ID {
		t.Errorf("live revoke calls = %v", got)
	}
}

func TestRemoveIsAllOrNothing(t *testing.T) {
	for _, failing := range []string{"trust", "keys", "tombstone"} {
		t.Run(failing, func(t *testing.T) {
			r := newRemoveRig(t)
			key, w := r.enrol("owner-1", "pi")
			r.seed(w.ID)
			boom := errors.New("boom")
			cfg := RemoveConfig{Store: r.st, Auditor: r.audit, Live: r.live,
				Trust: CleanupFunc(func(ctx context.Context, tx *sql.Tx, id string) error {
					if failing == "trust" {
						return boom
					}
					_, err := tx.ExecContext(ctx, `DELETE FROM test_trust WHERE worker_id = ?`, id)
					return err
				}),
				Keys: CleanupFunc(func(ctx context.Context, tx *sql.Tx, id string) error {
					if failing == "keys" {
						return boom
					}
					_, err := tx.ExecContext(ctx, `DELETE FROM test_keys WHERE bound_worker_id = ?`, id)
					return err
				}),
			}
			if failing == "tombstone" {
				cfg.Store = failingTombstone{r.st, boom}
			}
			rm, err := NewRemover(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the failure", err)
			}
			// Nothing changed: the Worker is enrolled, trust and keys are intact.
			if _, err := r.st.WorkerByPub(context.Background(), key.Public()); err != nil {
				t.Errorf("the Worker is gone after a failed removal: %v", err)
			}
			if _, err := r.st.TombstoneByPub(context.Background(), key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
				t.Errorf("a tombstone exists after a failed removal: %v", err)
			}
			if r.count("test_trust") != 3 || r.count("test_keys") != 2 {
				t.Errorf("trust=%d keys=%d: the failed removal was not rolled back", r.count("test_trust"), r.count("test_keys"))
			}
			if len(r.live.called()) != 0 {
				t.Error("a failed removal revoked the Worker")
			}
			// The audit log never claims a removal that did not happen: exactly
			// one record, and it says failed.
			page, _ := r.audit.List(context.Background(), audit.Filter{Action: AuditActionWorkerRemove})
			if len(page.Events) != 1 || page.Events[0].Outcome != AuditOutcomeFailed {
				t.Errorf("audit = %+v, want exactly one failed event", page.Events)
			}
		})
	}
}

type failingTombstone struct {
	*store.Store
	err error
}

func (f failingTombstone) TombstoneWorkerTx(context.Context, *sql.Tx, string, string, int64) error {
	return f.err
}

func TestRemoveIsAuditedWithActorAndActingAsAndNoContent(t *testing.T) {
	r := newRemoveRig(t)
	_, w := r.enrol("owner-1", "pi")
	if _, err := r.rm.Remove(context.Background(), RemoveActor{ID: "admin-1", ActingAs: "owner-1"}, w.ID); err != nil {
		t.Fatal(err)
	}
	page, err := r.audit.List(context.Background(), audit.Filter{Action: AuditActionWorkerRemove})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v, %v", page, err)
	}
	e := page.Events[0]
	if e.Actor != "admin-1" || e.ActingAs != "owner-1" || e.Target != w.ID || e.Outcome != AuditOutcomeOK {
		t.Errorf("event = %+v", e)
	}
	tomb, _ := r.st.TombstoneByPub(context.Background(), w.Pub)
	if tomb.RemovedBy != "admin-1" {
		t.Errorf("removed_by = %q", tomb.RemovedBy)
	}
}

// failingRecordTx is an audit log whose in-transaction record fails; the
// plain Record (the failed record of a rolled-back removal) still works.
type failingRecordTx struct{ *audit.Log }

func (failingRecordTx) RecordTx(context.Context, store.Execer, audit.Event) error {
	return errors.New("audit down")
}

func TestRemoveNeedsItsAuditRecord(t *testing.T) {
	r := newRemoveRig(t)
	key, w := r.enrol("owner-1", "pi")
	r.seed(w.ID)
	rm, err := NewRemover(RemoveConfig{Store: r.st, Auditor: failingRecordTx{r.audit}, Live: r.live,
		Keys: CleanupFunc(func(ctx context.Context, tx *sql.Tx, id string) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM test_keys WHERE bound_worker_id = ?`, id)
			return err
		})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID); err == nil {
		t.Fatal("a removal whose audit record failed went through")
	}
	if _, err := r.st.WorkerByPub(context.Background(), key.Public()); err != nil {
		t.Errorf("the Worker was removed without an audit record: %v", err)
	}
	if _, err := r.st.TombstoneByPub(context.Background(), key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("a tombstone exists without an audit record: %v", err)
	}
	if r.count("test_keys") != 2 {
		t.Error("the cleanup was not rolled back with the audit record")
	}
	if len(r.live.called()) != 0 {
		t.Error("a removal without its audit record revoked the Worker")
	}
	// The ok record is part of the rolled-back transaction: only the failure
	// is on record.
	page, _ := r.audit.List(context.Background(), audit.Filter{Action: AuditActionWorkerRemove})
	if len(page.Events) != 1 || page.Events[0].Outcome != AuditOutcomeFailed {
		t.Errorf("audit = %+v", page.Events)
	}
}

func TestRemoveOfAnUnknownOrRemovedWorkerIsNotFound(t *testing.T) {
	r := newRemoveRig(t)
	_, w := r.enrol("owner-1", "pi")
	ctx := context.Background()
	if _, err := r.rm.Remove(ctx, RemoveActor{ID: "u"}, store.NewID()); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("unknown Worker: %v", err)
	}
	if _, err := r.rm.Remove(ctx, RemoveActor{ID: "u"}, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.rm.Remove(ctx, RemoveActor{ID: "u"}, w.ID); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("second removal: %v", err)
	}
	if _, err := r.rm.Remove(ctx, RemoveActor{}, w.ID); err == nil {
		t.Error("a removal without an actor was accepted")
	}
}

func TestRemoveRaceWithAnotherRemovalIsNotFound(t *testing.T) {
	r := newRemoveRig(t)
	_, w := r.enrol("owner-1", "pi")
	// The row is gone between the read and the transaction.
	rm, err := NewRemover(RemoveConfig{Store: raceStore{r.st}, Auditor: r.audit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rm.Remove(context.Background(), RemoveActor{ID: "u"}, w.ID); proto.CodeOf(err) != proto.CodeNotFound {
		t.Errorf("err = %v", err)
	}
}

type raceStore struct{ *store.Store }

func (s raceStore) WorkerByID(ctx context.Context, id string) (store.WorkerRow, error) {
	w, err := s.Store.WorkerByID(ctx, id)
	if err == nil {
		_ = s.TombstoneWorker(ctx, id, "other", time.Now().UnixMilli())
	}
	return w, err
}

func TestLiveRevokeFailureNeverUndoesARemoval(t *testing.T) {
	r := newRemoveRig(t)
	key, w := r.enrol("owner-1", "pi")
	r.live.online, r.live.err = true, errors.New("stream failed")
	res, err := r.rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID)
	if err != nil {
		t.Fatalf("a live delivery failure failed the removal: %v", err)
	}
	if !res.Online || res.Revoked {
		t.Errorf("result = %+v", res)
	}
	if _, err := r.st.TombstoneByPub(context.Background(), key.Public()); err != nil {
		t.Errorf("tombstone: %v", err)
	}
}

func TestRemoveResultWarnings(t *testing.T) {
	cases := []struct {
		res  RemoveResult
		want string
	}{
		{RemoveResult{Online: false}, ops.WarnRevokeOnNextConnect},
		{RemoveResult{Online: true, Revoked: false}, ops.WarnRevokeNotConfirmed},
		{RemoveResult{Online: true, Revoked: true}, ""},
	}
	for _, c := range cases {
		got := c.res.Warnings()
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%+v: warnings %v", c.res, got)
		case c.want != "" && (len(got) != 1 || got[0].Code != c.want):
			t.Errorf("%+v: warnings %v, want %s", c.res, got, c.want)
		}
	}
}

func TestRemoverNeedsItsDependencies(t *testing.T) {
	st := openStore(t, t.TempDir())
	if _, err := NewRemover(RemoveConfig{Auditor: audit.New(st)}); err == nil {
		t.Error("no store")
	}
	if _, err := NewRemover(RemoveConfig{Store: st}); err == nil {
		t.Error("no audit log")
	}
}

// ---- live revoke over a real connection -----------------------------------------

// workerSide is a Worker connected to a test Hub, with the first op stream it
// is sent and the data of its handshake.
type workerSide struct {
	sess *transport.Session
	res  *transport.WorkerResult
	id   string
}

func connectWorker(t *testing.T, h *testHub, key *identity.Key, w store.WorkerRow) *workerSide {
	t.Helper()
	res, err := h.dial(h.workerConfig(key, "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res)
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	return &workerSide{sess: sess, res: res, id: w.ID}
}

func (w *workerSide) accept(t *testing.T) *transport.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := w.sess.Accept(ctx)
	if err != nil {
		t.Fatalf("the Worker was sent no control stream: %v", err)
	}
	return st
}

func revokerFor(h *testHub, grace time.Duration) *RegistryRevoker {
	return &RegistryRevoker{Registry: h.svc.Registry(), Identity: h.svc.Identity(), Grace: grace}
}

func TestRegistryRevokerSendsASignedRevokeOverTheLiveConnection(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	ws := connectWorker(t, h, key, w)

	type answer struct {
		online, confirmed bool
		err               error
	}
	done := make(chan answer, 1)
	go func() {
		o, c, err := revokerFor(h, 5*time.Second).RevokeOnline(context.Background(), w.ID)
		done <- answer{o, c, err}
	}()

	st := ws.accept(t)
	body, err := st.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var f RevokeFrame
	if err := json.Unmarshal(body, &f); err != nil || f.Type != proto.TypeRevoke {
		t.Fatalf("frame = %s (%v)", body, err)
	}
	// Signed by the Hub key, purpose revoke, over this connection's transcript.
	if err := VerifyRevoke(ws.res.HubPub, ws.res.TranscriptHash[:], f); err != nil {
		t.Fatalf("the live revoke does not verify: %v", err)
	}
	other := bytes.Clone(ws.res.TranscriptHash[:])
	other[1] ^= 0x55
	if VerifyRevoke(ws.res.HubPub, other, f) == nil {
		t.Error("the live revoke verifies over another transcript")
	}
	// The Worker confirms; the Hub then ends the connection.
	// The Hub may end the session as soon as it has read the answer, so a
	// write error here is not a failure; the Hub's verdict below is.
	_ = st.WriteFrame(map[string]string{"type": proto.TypeOpResult, "op": "revoke"})
	_ = st.Close()

	a := <-done
	if a.err != nil || !a.online || !a.confirmed {
		t.Fatalf("answer = %+v", a)
	}
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
}

func TestRegistryRevokerWithoutAnswerEndsTheConnectionAndReportsUnconfirmed(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	ws := connectWorker(t, h, key, w)
	_ = ws // the Worker never answers

	start := time.Now()
	online, confirmed, err := revokerFor(h, 150*time.Millisecond).RevokeOnline(context.Background(), w.ID)
	if !online || confirmed || err == nil {
		t.Fatalf("online=%v confirmed=%v err=%v; want online, unconfirmed, an error", online, confirmed, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the revoke waited %s", time.Since(start))
	}
	eventually(t, "the connection of the removed Worker to end", func() bool { return !h.svc.Registry().IsOnline(w.ID) })
}

func TestRegistryRevokerRejectsAnythingButTheAck(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	ws := connectWorker(t, h, key, w)
	done := make(chan bool, 1)
	go func() {
		_, c, _ := revokerFor(h, 5*time.Second).RevokeOnline(context.Background(), w.ID)
		done <- c
	}()
	st := ws.accept(t)
	if _, err := st.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	_ = st.WriteFrame(proto.NewError(proto.CodeForbidden, "no").Frame())
	_ = st.Close()
	if <-done {
		t.Error("an error frame counted as a confirmation")
	}
}

func TestRegistryRevokerForAnOfflineWorkerDoesNothing(t *testing.T) {
	h := newTestHub(t, nil)
	online, confirmed, err := revokerFor(h, time.Second).RevokeOnline(context.Background(), store.NewID())
	if online || confirmed || err != nil {
		t.Errorf("online=%v confirmed=%v err=%v", online, confirmed, err)
	}
}

func TestRemoveOnlineWorkerDeliversTheLiveRevokeThroughTheRegistry(t *testing.T) {
	h := newTestHub(t, nil)
	// Exercise the live revoke after admission has completed. The independent
	// post-welcome recheck may otherwise observe the tombstone and close the
	// connection before this test's live revoke reaches its acknowledgement.
	rechecked := make(chan struct{}, 1)
	h.svc.Handler().afterRecheck = func() { rechecked <- struct{}{} }
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	ws := connectWorker(t, h, key, w)
	select {
	case <-rechecked:
	case <-timeout(t):
		t.Fatal("the admission recheck did not finish")
	}
	rm, err := NewRemover(RemoveConfig{Store: h.st, Auditor: h.audit, Live: revokerFor(h, 5*time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	go func() { // the Worker side: verify and confirm
		st := ws.accept(t)
		body, err := st.ReadFrame()
		if err != nil {
			return
		}
		var f RevokeFrame
		_ = json.Unmarshal(body, &f)
		if VerifyRevoke(ws.res.HubPub, ws.res.TranscriptHash[:], f) == nil {
			_ = st.WriteFrame(map[string]string{"type": proto.TypeOpResult, "op": "revoke"})
		}
		_ = st.Close()
	}()
	res, err := rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Online || !res.Revoked || len(res.Warnings()) != 0 {
		t.Errorf("result = %+v", res)
	}
	// Disconnect closes the transport before it returns; the registry's Done
	// watcher removes the entry asynchronously.
	eventually(t, "the removed Worker's registry entry to be cleared", func() bool {
		return !h.svc.Registry().IsOnline(w.ID)
	})
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("the removed Worker is still online")
	}
}

// ---- offline removal and the permanence rules ----------------------------------

func TestOfflineWorkerIsRevokedOnItsNextConnect(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	rm, err := NewRemover(RemoveConfig{Store: h.st, Auditor: h.audit, Live: revokerFor(h, time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID)
	if err != nil || res.Online || res.Revoked {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := res.Warnings(); len(got) != 1 || got[0].Code != ops.WarnRevokeOnNextConnect {
		t.Errorf("warnings = %v", got)
	}

	conn, err := h.dial(h.workerConfig(key, "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	m := readControl(t, conn)
	if got := frameType(t, m); got != proto.TypeRevoke {
		t.Fatalf("the reconnect got %q, want a signed revoke", got)
	}
	var f RevokeFrame
	raw, _ := json.Marshal(m)
	_ = json.Unmarshal(raw, &f)
	if err := VerifyRevoke(conn.HubPub, conn.TranscriptHash[:], f); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	expectClosed(t, conn)
	// A key the Hub never knew keeps getting the non-destructive answer.
	stranger, err := h.dial(h.workerConfig(newKey(t), "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	if got := frameType(t, readControl(t, stranger)); got != proto.TypeNotEnrolled {
		t.Errorf("stranger got %q", got)
	}
	expectClosed(t, stranger)
}

// TestExpiredInvitationAndAPIKeysNeverAffectAnEnrolledWorker is the AD-9
// regression test with real rows: an API key bound to the Worker whose
// expiry is long past and an API key scoped to the Worker that was revoked.
// The connect decision reads only the workers and tombstones tables, so
// neither can unpair or disconnect the Worker, however old the enrollment is.
// Story 4.5 adds the expired and revoked invitation-key half once the
// invitation_keys table exists.
func TestExpiredInvitationAndAPIKeysNeverAffectAnEnrolledWorker(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	// Enrolled five years ago.
	w := store.WorkerRow{
		ID: store.NewID(), Pub: key.Public(), Name: "pi", OwnerID: "owner-1", Kind: store.WorkerKindPrivate,
		CreatedAt: time.Now().AddDate(-5, 0, 0).UnixMilli(),
	}
	ctx := context.Background()
	if err := h.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	insertUser(t, h.st, "owner-1")
	past := time.Now().AddDate(-1, 0, 0).UnixMilli()
	createKey(t, h.st, store.APIKeyRow{
		ID: store.NewID(), OwnerID: "owner-1", Name: "bound-expired", BoundWorkerID: w.ID,
		Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: w.ID}}, ExpiresAt: past,
	})
	createKey(t, h.st, store.APIKeyRow{
		ID: store.NewID(), OwnerID: "owner-1", Name: "scoped-revoked",
		Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: w.ID}}, RevokedAt: past,
	})

	res, err := h.dial(h.workerConfig(key, "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	if welcome := decodeWelcome(t, res); welcome.WorkerID != w.ID {
		t.Fatalf("welcome = %+v", welcome)
	}
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	_ = res.Channel.Close()
	if _, err := h.st.WorkerByPub(ctx, key.Public()); err != nil {
		t.Errorf("the Worker row went away: %v", err)
	}
	if _, err := h.st.TombstoneByPub(ctx, key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the Worker was tombstoned without a removal: %v", err)
	}
	// Neither key was touched by the connect: no revoke happened.
	keys, err := h.st.ListAPIKeys(ctx, "owner-1")
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %d, %v", len(keys), err)
	}
	for _, k := range keys {
		switch k.Name {
		case "bound-expired":
			if k.RevokedAt != 0 {
				t.Errorf("the connect revoked key %s", k.Name)
			}
		case "scoped-revoked":
			if k.RevokedAt != past {
				t.Errorf("revoked_at of %s changed to %d", k.Name, k.RevokedAt)
			}
		}
	}
}

// ---- real API keys ---------------------------------------------------------------

func insertUser(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, id, id); err != nil {
		t.Fatal(err)
	}
}

// createKey stores a key through the production store with fresh hash and hint.
func createKey(t *testing.T, st *store.Store, k store.APIKeyRow) {
	t.Helper()
	sum := sha256.Sum256([]byte(k.ID))
	k.Hash, k.Hint = sum[:], k.ID[len(k.ID)-8:]
	k.Actions = []string{"sessions_list"}
	k.CreatedAt = time.Now().UnixMilli()
	if err := st.CreateAPIKey(context.Background(), k, nil); err != nil {
		t.Fatal(err)
	}
}

// revokeBoundKeys is the production cleanup of the Hub module
// (mod_workers_remove.go), on the real api_keys table.
func revokeBoundKeys(st *store.Store, now func() int64) Cleanup {
	return CleanupFunc(func(ctx context.Context, tx *sql.Tx, workerID string) error {
		_, err := st.RevokeAPIKeysBoundToWorker(ctx, tx, workerID, now())
		return err
	})
}

func TestRemoveRevokesTheRealKeysBoundToTheWorkerAndNoOthers(t *testing.T) {
	for _, tombstoneFails := range []bool{false, true} {
		name := "removal commits"
		if tombstoneFails {
			name = "tombstone step fails and the revocation is rolled back"
		}
		t.Run(name, func(t *testing.T) {
			r := newRemoveRig(t)
			_, w := r.enrol("owner-1", "pi")
			_, other := r.enrol("owner-1", "kept")
			insertUser(t, r.st, "owner-1")
			bound := store.NewID()
			foreign := store.NewID()
			createKey(t, r.st, store.APIKeyRow{ID: bound, OwnerID: "owner-1", Name: "bound", BoundWorkerID: w.ID,
				Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: w.ID}}})
			createKey(t, r.st, store.APIKeyRow{ID: foreign, OwnerID: "owner-1", Name: "other", BoundWorkerID: other.ID,
				Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: other.ID}}})

			const at = int64(1_700_000_000_000)
			cfg := RemoveConfig{Store: r.st, Auditor: r.audit, Live: r.live,
				Keys: revokeBoundKeys(r.st, func() int64 { return at })}
			boom := errors.New("boom")
			if tombstoneFails {
				cfg.Store = failingTombstone{r.st, boom}
			}
			rm, err := NewRemover(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID)
			if tombstoneFails != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			kb, err := r.st.APIKeyByID(context.Background(), bound)
			if err != nil {
				t.Fatal(err)
			}
			ko, err := r.st.APIKeyByID(context.Background(), foreign)
			if err != nil {
				t.Fatal(err)
			}
			if tombstoneFails {
				if kb.RevokedAt != 0 {
					t.Errorf("revoked_at = %d after a failed removal: the revocation was not rolled back", kb.RevokedAt)
				}
			} else if kb.RevokedAt != at {
				t.Errorf("revoked_at of the removed Worker's key = %d, want %d", kb.RevokedAt, at)
			}
			if ko.RevokedAt != 0 {
				t.Errorf("the key of another Worker was revoked at %d", ko.RevokedAt)
			}
		})
	}
}

// ---- connect/remove race ---------------------------------------------------------

// tombstoneAfterRead tombstones the Worker between the read of its row and
// its registration online, as a removal that commits in that window does.
type tombstoneAfterRead struct {
	*store.Store
	once sync.Once
}

func (s *tombstoneAfterRead) WorkerByPub(ctx context.Context, pub []byte) (store.WorkerRow, error) {
	w, err := s.Store.WorkerByPub(ctx, pub)
	if err == nil {
		s.once.Do(func() { _ = s.TombstoneWorker(ctx, w.ID, "owner-1", time.Now().UnixMilli()) })
	}
	return w, err
}

func TestWorkerRemovedWhileItConnectsIsDisconnectedAndRevokedOnReconnect(t *testing.T) {
	var racing *tombstoneAfterRead
	h := newTestHub(t, func(o *ServiceOptions) {
		racing = &tombstoneAfterRead{Store: o.Store.(*store.Store)}
		o.Store = racing
	})
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")

	res, err := h.dial(h.workerConfig(key, "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	decodeWelcome(t, res) // the welcome was already on its way
	// The Hub ends this connection instead of leaving a removed Worker online.
	expectClosed(t, res)
	eventually(t, "the removed Worker to be offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })

	conn, err := h.dial(h.workerConfig(key, "sessions_list"))
	if err != nil {
		t.Fatal(err)
	}
	if got := frameType(t, readControl(t, conn)); got != proto.TypeRevoke {
		t.Fatalf("the reconnect got %q, want a signed revoke", got)
	}
}
