package audit_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// lockedBuffer is a log sink that the export goroutine and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns the "audit" log records written so far.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if m["msg"] == "audit" {
			out = append(out, m)
		}
	}
	return out
}

// sinkCount counts the audit log records of one grant event.
func sinkCount(t *testing.T, l *audit.Log, sink *lockedBuffer, grantID, action, detail string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range sink.records(t) {
		if r["grant_id"] == grantID && r["action"] == action && r["detail"] == detail && r["actor"] == "system" {
			n++
		}
	}
	return n
}

type grantRig struct {
	st        *store.Store
	a, b, c   store.WorkerRow
	adm, own  store.SessionGrantRow
	nowMillis int64
}

// newGrantRig stores users (two admins), Workers a (alice), b (alice,
// infrastructure) and c (bob), an owner grant a -> b and an admin grant of
// root a -> c.
func newGrantRig(t *testing.T, st *store.Store) grantRig {
	t.Helper()
	ctx := t.Context()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO users(id,username,created_at,updated_at) VALUES
		('root','root',1,1),('boss','boss',1,1),('alice','alice',1,1),('bob','bob',1,1);
		INSERT INTO user_roles(user_id,role,source) VALUES('root','admin','local'),('boss','admin','local'),
		('alice','user','local'),('bob','user','local');`); err != nil {
		t.Fatal(err)
	}
	r := grantRig{st: st, nowMillis: time.Now().UnixMilli(),
		a: store.WorkerRow{ID: ops.NewUUIDv7(), Pub: bytes.Repeat([]byte{1}, 32), Name: "a", OwnerID: "alice", Kind: store.WorkerKindPrivate, CreatedAt: 1000},
		b: store.WorkerRow{ID: ops.NewUUIDv7(), Pub: bytes.Repeat([]byte{2}, 32), Name: "b", OwnerID: "alice", Kind: store.WorkerKindInfrastructure, CreatedAt: 1000},
		c: store.WorkerRow{ID: ops.NewUUIDv7(), Pub: bytes.Repeat([]byte{3}, 32), Name: "c", OwnerID: "bob", Kind: store.WorkerKindPrivate, CreatedAt: 1000}}
	for _, w := range []store.WorkerRow{r.a, r.b, r.c} {
		if err := st.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE workers SET approval='approved'`); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.own, err = st.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: r.a.ID, SourceSession: "boss", TargetWorkerID: r.b.ID,
		Level: 1, Authority: store.GrantAuthorityOwner, GrantedBy: "alice", SourceOwnerID: "alice", TargetOwnerID: "alice", UpdatedAt: 100},
		0, store.GrantWrite{ActorID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if r.adm, err = st.PutSessionGrant(ctx, store.SessionGrantRow{SourceWorkerID: r.a.ID, SourceSession: "boss", TargetWorkerID: r.c.ID,
		TargetSession: "t", Level: 3, Authority: store.GrantAuthorityAdmin, GrantedBy: "root", SourceOwnerID: "alice", TargetOwnerID: "bob", UpdatedAt: 100},
		0, store.GrantWrite{ActorID: "root"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r grantRig) demote(t *testing.T) {
	t.Helper()
	if _, err := r.st.SetLocalRole(t.Context(), store.SetLocalRoleParams{UserID: "root", Role: "user", NowMS: r.nowMillis}); err != nil {
		t.Fatal(err)
	}
}

// The grant rows that the SQL triggers of migration 0034 write (a demotion, a
// Worker reassignment, a Worker removal) reach the configured log sink, each
// exactly once.
func TestTriggerGrantAuditReachesTheLogSink(t *testing.T) {
	st := openStore(t)
	r := newGrantRig(t, st)
	sink := &lockedBuffer{}
	l := audit.New(st, audit.WithLogger(slog.New(slog.NewJSONHandler(sink, nil))))
	ctx := t.Context()

	r.demote(t)
	if n := sinkCount(t, l, sink, r.adm.ID, "grant.suspend", "cause=grantor_not_admin"); n != 1 {
		t.Fatalf("demotion: %d sink records, want 1", n)
	}

	if _, err := st.ReassignWorker(ctx, store.ReassignParams{WorkerID: r.b.ID, NewOwnerID: "bob", AtMS: r.nowMillis}); err != nil {
		t.Fatal(err)
	}
	if n := sinkCount(t, l, sink, r.own.ID, "grant.end", "cause=owner_changed"); n != 1 {
		t.Fatalf("reassignment: %d sink records, want 1", n)
	}

	if err := st.Tx(ctx, func(tx *sql.Tx) error {
		return st.TombstoneWorkerTx(ctx, tx, r.c.ID, "boss", r.nowMillis)
	}); err != nil {
		t.Fatal(err)
	}
	if n := sinkCount(t, l, sink, r.adm.ID, "grant.end", "cause=worker_removed"); n != 1 {
		t.Fatalf("removal: %d sink records, want 1", n)
	}

	// Later commits and another observer pass export nothing again.
	if err := l.Record(ctx, audit.Event{Actor: "boss", Action: "noop", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	st.SetAuditObserver(func(context.Context, store.AuditRow) { t.Error("a forwarded row was forwarded again") })
	if err := st.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ id, action, detail string }{
		{r.adm.ID, "grant.suspend", "cause=grantor_not_admin"},
		{r.own.ID, "grant.end", "cause=owner_changed"},
		{r.adm.ID, "grant.end", "cause=worker_removed"},
	} {
		if n := sinkCount(t, l, sink, want.id, want.action, want.detail); n != 1 {
			t.Fatalf("%s %s: %d sink records, want 1", want.action, want.detail, n)
		}
	}
}

// A trigger row committed before the audit log observed the store (the
// start-up reconciliation runs inside Open) reaches the sink when the log is
// set up, before the Hub serves.
func TestTriggerGrantAuditBeforeTheLogReachesTheSinkAtStart(t *testing.T) {
	st := openStore(t)
	r := newGrantRig(t, st)
	r.demote(t)
	sink := &lockedBuffer{}
	l := audit.New(st, audit.WithLogger(slog.New(slog.NewJSONHandler(sink, nil))))
	if n := sinkCount(t, l, sink, r.adm.ID, "grant.suspend", "cause=grantor_not_admin"); n != 1 {
		t.Fatalf("%d sink records, want 1", n)
	}
	// The row is in the history once, as before.
	page, err := l.List(t.Context(), audit.Filter{GrantID: r.adm.ID})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("history = %+v, %v", page.Events, err)
	}
}
