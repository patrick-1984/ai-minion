package server

// Verification of "a lost session loses its permission" (owner question,
// v0.22.0), on the session purge module. A test named ...Gap asserts the
// current, unwanted behaviour so that a fix makes it fail and is noticed.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A new terminal that takes the name of a gone master session (its
// session_added is the first sighting, after the nomination was made) loses
// the name-only master nomination and is taken out of a session-limited
// acceptance, even when the session_removed of the old one was missed.
func TestVerifyLossReplacedSessionLosesItsClaimedMasterNomination(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u", CreatedAt: 1_000}); err != nil {
		t.Fatal(err)
	}
	// wB accepts control from wA for its session "ci" only.
	if err := st.PutAccept(ctx, store.AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "u", CreatedAt: 1_000}); err != nil {
		t.Fatal(err)
	}
	p := &sessionPurger{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: fakeSessions{ws: workers.WorkerSessions{WorkerID: "wA"}},
		seen:     fakeSeen{"boss": 5_000, "ci": 5_000}}
	added := func(w, name string) workers.Event {
		return workers.Event{Type: workers.EventSessionAdded, WorkerID: w, Session: name, Time: ops.Timestamp(time.UnixMilli(5_000))}
	}
	if err := p.handle(ctx, added("wA", "boss")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MasterSession(ctx, "wA", "boss"); err == nil {
		t.Fatal("the new terminal under the name boss inherited the master nomination")
	}
	if err := p.handle(ctx, added("wB", "ci")); err != nil {
		t.Fatal(err)
	}
	acc, err := st.ListAccepts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc) != 1 || acc[0].Sessions == nil || len(acc[0].Sessions) != 0 {
		t.Fatalf("the acceptance still names the replaced target session (or widened): %+v", acc)
	}
}

// A Worker that was offline while its master session was killed and a new
// one was created under the same name (or its tmux server restarted and the
// name was recreated) reconnects with a snapshot that lists the name again.
// The Hub keeps the first-seen time of a name it listed before the
// disconnect, so the purge sees nothing to do: the name-only master
// nomination stays and serves the new terminal (the name-only grant: see
// workers.TestVerifyLossReconnectSnapshotKeepsTheOldTerminalsGrantGap).
func TestVerifyLossOfflineRecreateKeepsClaimedMasterGap(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.UnixMilli(10_000)
	hub := workers.NewEventHub(workers.EventHubOptions{Now: func() time.Time { return now }})
	snapshot := func(names ...string) *workers.Link {
		t.Helper()
		l := hub.Connect("wA", "a")
		f, err := l.NewFeed()
		if err != nil {
			t.Fatal(err)
		}
		var s []workers.SessionState
		for _, n := range names {
			s = append(s, workers.SessionState{Name: n})
		}
		if err := f.Snapshot(0, s); err != nil {
			t.Fatal(err)
		}
		return l
	}
	snapshot()
	now = now.Add(time.Second)
	l := snapshot("boss") // first seen now: a real generation
	if at, _ := hub.FirstSeen("wA", "boss"); at != now.UnixMilli() {
		t.Fatalf("setup: first seen %d", at)
	}
	created := now.Add(time.Second).UnixMilli()
	if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	p := &sessionPurger{st: st, hub: hub, sessions: hub, seen: hub, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	now = now.Add(time.Minute)
	l.Disconnect()
	// Offline: "boss" is killed, a new "boss" is created. Reconnect.
	now = now.Add(time.Hour)
	sub := hub.Subscribe(16)
	defer sub.Close()
	snapshot("boss")
	var resync workers.Event
	for resync.Type != workers.EventWorkerResynced {
		select {
		case resync = <-sub.C():
		case <-time.After(time.Second):
			t.Fatal("no resync event")
		}
	}
	if err := p.handle(ctx, resync); err != nil {
		t.Fatal(err)
	}
	p.reconcile(ctx)
	if _, err := st.MasterSession(ctx, "wA", "boss"); err != nil {
		t.Fatalf("the nomination was purged (gap fixed?): %v", err)
	}
	// GAP: the nomination made for the old terminal stands for the new one.
}

// Removing a Worker ends every permission held by or given to its sessions:
// name-only master nominations and acceptances (DeleteSessionControlOfWorker
// in the removal transaction), session-bound nominations and session grants
// as source and as target (ON DELETE CASCADE on workers). A Worker enrolled
// again gets a new id and starts with nothing.
func TestVerifyLossWorkerRemovalDropsEveryPermissionOfItsSessions(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	db := st.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users(id,username,created_at,updated_at) VALUES('alice','alice',1,1)`)
	exec(`INSERT INTO user_roles(user_id,role,source) VALUES('alice','user','local')`)
	wa, wb := ops.NewUUIDv7(), ops.NewUUIDv7()
	for i, id := range []string{wa, wb} {
		w := store.WorkerRow{ID: id, Pub: []byte{byte(i + 1), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
			Name: string(rune('a' + i)), OwnerID: "alice", Kind: store.WorkerKindPrivate, CreatedAt: 1000}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE workers SET approval='approved'`)
	put := func(src, srcSess, dst, dstSess string) {
		t.Helper()
		if _, err := st.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: src, SourceSession: srcSess, TargetWorkerID: dst,
			TargetSession: dstSess, Level: 3, Authority: store.GrantAuthorityOwner, SourceOwnerID: "alice", TargetOwnerID: "alice",
			UpdatedAt: 100}, 0, store.GrantWrite{ActorID: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	put(wa, "boss", wb, "build") // wa as grant source
	put(wb, "ci", wa, "boss")    // wa as grant target
	if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: wa, SessionName: "claimed", CreatedBy: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAccept(ctx, store.AcceptRow{TargetWorkerID: wb, SourceWorkerID: wa, CreatedBy: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAccept(ctx, store.AcceptRow{TargetWorkerID: wa, SourceWorkerID: wb, CreatedBy: "alice", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	var pub []byte
	if err := db.QueryRowContext(ctx, `SELECT worker_pub FROM workers WHERE id=?`, wa).Scan(&pub); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO bound_master_session(worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,changed_at)
		VALUES(?,?,?,?,?,1,1,1)`, wa, "alice", "verified", ops.NewUUIDv7(), pub)

	if err := st.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := st.DeleteSessionControlOfWorker(ctx, tx, wa); err != nil {
			return err
		}
		return st.TombstoneWorkerTx(ctx, tx, wa, "alice", 2)
	}); err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q, wa).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for q, what := range map[string]string{
		`SELECT COUNT(*) FROM session_grant WHERE source_worker_id=?1 OR target_worker_id=?1`:  "session grants",
		`SELECT COUNT(*) FROM master_session WHERE worker_id=?1`:                                "name-only masters",
		`SELECT COUNT(*) FROM bound_master_session WHERE worker_id=?1`:                          "bound masters",
		`SELECT COUNT(*) FROM control_accept WHERE source_worker_id=?1 OR target_worker_id=?1`: "acceptances",
	} {
		if n := count(q); n != 0 {
			t.Errorf("%d %s survive the removal of their Worker", n, what)
		}
	}
}
