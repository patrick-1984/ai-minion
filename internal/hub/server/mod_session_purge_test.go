package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

type fakeSessions struct{ ws workers.WorkerSessions }

func (f fakeSessions) Sessions(string) (workers.WorkerSessions, bool) { return f.ws, true }
func (f fakeSessions) All() []workers.WorkerSessions                  { return []workers.WorkerSessions{f.ws} }

// A20: a removed session and, after a resync, every session the Worker no
// longer shares lose their master nomination; a name that comes back is a
// slave session.
func TestGoneSessionsLoseTheirMasterNomination(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, n := range []string{"boss", "ci", "live"} {
		if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: "wA", SessionName: n, CreatedBy: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	p := &sessionPurger{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: fakeSessions{ws: workers.WorkerSessions{WorkerID: "wA", Online: true, Synced: false, Sessions: []workers.SessionState{{Name: "live"}}}}}
	p.handle(ctx, workers.Event{Type: workers.EventSessionRemoved, WorkerID: "wA", Session: "boss", Time: ops.Timestamp(time.Now())})
	if _, err := st.MasterSession(ctx, "wA", "boss"); err == nil {
		t.Fatal("boss is still a master after it was removed")
	}
	// Before the snapshot of this connection nothing is purged.
	p.handle(ctx, workers.Event{Type: workers.EventWorkerResynced, WorkerID: "wA", Time: ops.Timestamp(time.Now())})
	if ms, _ := st.ListMasterSessions(ctx); len(ms) != 2 {
		t.Fatalf("purged before the snapshot: %+v", ms)
	}
	p.sessions = fakeSessions{ws: workers.WorkerSessions{WorkerID: "wA", Online: true, Synced: true, UpdatedAt: time.Now(), Sessions: []workers.SessionState{{Name: "live"}}}}
	p.reconcile(ctx) // as after a missed event
	ms, _ := st.ListMasterSessions(ctx)
	if len(ms) != 1 || ms[0].SessionName != "live" {
		t.Fatalf("masters after the check = %+v (ci is gone)", ms)
	}
}

// A removed session ends the grants that name it in the one audited purge
// transaction: a grant edited after its creation still ends with its audit
// row, and while the audit insert fails the grant is not deleted at all.
func TestGoneSessionEndsItsGrantsWithAudit(t *testing.T) {
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
		w := store.WorkerRow{ID: id, Pub: bytes.Repeat([]byte{byte(i + 1)}, 32), Name: string(rune('a' + i)), OwnerID: "alice",
			Kind: store.WorkerKindPrivate, CreatedAt: 1000}
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE workers SET approval='approved'`)
	g, err := st.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: wa, SourceSession: "boss", TargetWorkerID: wb,
		Level: 1, Authority: store.GrantAuthorityOwner, SourceOwnerID: "alice", TargetOwnerID: "alice", UpdatedAt: 100},
		0, store.GrantWrite{ActorID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	g.Level = 2
	g.UpdatedAt = time.Now().Add(time.Hour).UnixMilli() // edited after the removal evidence
	if _, err := st.PutSessionGrant(ctx, g, g.Revision, store.GrantWrite{ActorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	p := &sessionPurger{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: fakeSessions{ws: workers.WorkerSessions{WorkerID: wa}}}
	removed := workers.Event{Type: workers.EventSessionRemoved, WorkerID: wa, Session: "boss", Time: ops.Timestamp(time.Now())}

	exec(`CREATE TRIGGER test_audit_down BEFORE INSERT ON audit_events WHEN NEW.action = 'grant.end'
		BEGIN SELECT RAISE(ABORT, 'audit down'); END`)
	p.handle(ctx, removed)
	if _, err := st.SessionGrant(ctx, g.ID); err != nil {
		t.Fatalf("grant deleted although its audit row failed: %v", err)
	}
	exec(`DROP TRIGGER test_audit_down`)
	p.handle(ctx, removed)
	if _, err := st.SessionGrant(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("grant of a removed session survived: %v", err)
	}
	rows, err := st.ListAudit(ctx, store.AuditQuery{GrantID: g.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var ends int
	for _, r := range rows {
		if r.Action == store.AuditActionGrantEnd && r.Detail == "cause=session_gone" {
			ends++
		}
	}
	if ends != 1 {
		t.Fatalf("grant.end rows = %d, want 1: %+v", ends, rows)
	}
}
