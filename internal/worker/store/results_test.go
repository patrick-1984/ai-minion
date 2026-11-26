package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// fakeClock is a Clock whose time the test moves.
type fakeClock struct{ ms atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ms.Store(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixMilli())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

func TestResultCacheRoundTripAndTTL(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))

	if _, ok, err := s.GetResult(ctx, "r1"); err != nil || ok {
		t.Fatalf("empty cache: ok=%v err=%v", ok, err)
	}
	rec := ResultRecord{RequestID: "r1", Op: "send_text", Session: "work", Force: true, Result: []byte(`{"outcome":"delivered"}`)}
	if err := s.PutResult(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetResult(ctx, "r1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Op != "send_text" || got.Session != "work" || !got.Force || string(got.Result) != `{"outcome":"delivered"}` || !got.CreatedAt.Equal(clk.Now()) {
		t.Fatalf("got %+v", got)
	}

	clk.Advance(ResultTTL - time.Second)
	if _, ok, _ := s.GetResult(ctx, "r1"); !ok {
		t.Fatal("entry must still be live just before the TTL")
	}
	clk.Advance(2 * time.Second)
	if _, ok, _ := s.GetResult(ctx, "r1"); ok {
		t.Fatal("entry older than the TTL must count as absent")
	}
}

func TestResultCacheFirstWriteWinsAndExpiredIDIsReusable(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))

	if err := s.PutResult(ctx, ResultRecord{RequestID: "r1", Op: "a", Result: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutResult(ctx, ResultRecord{RequestID: "r1", Op: "b", Result: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetResult(ctx, "r1")
	if got.Op != "a" || string(got.Result) != "1" {
		t.Fatalf("second put replaced the first: %+v", got)
	}

	clk.Advance(ResultTTL + time.Minute)
	if err := s.PutResult(ctx, ResultRecord{RequestID: "r1", Op: "c", Result: []byte("3")}); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.GetResult(ctx, "r1")
	if !ok || got.Op != "c" {
		t.Fatalf("expired id must be reusable, got %+v ok=%v", got, ok)
	}
}

func TestResultCachePrunes(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	count := func() int {
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM op_results`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, id := range []string{"a", "b"} {
		if err := s.PutResult(ctx, ResultRecord{RequestID: id, Op: "x", Result: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(ResultTTL + time.Second)
	// A Put prunes the expired rows.
	if err := s.PutResult(ctx, ResultRecord{RequestID: "c", Op: "x", Result: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("rows after Put = %d, want 1 (a and b pruned)", n)
	}
	clk.Advance(ResultTTL + time.Second)
	n, err := s.PruneResults(ctx)
	if err != nil || n != 1 || count() != 0 {
		t.Fatalf("PruneResults = %d, %v; rows left %d", n, err, count())
	}
}

func TestPutResultRejectsEmptyID(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.PutResult(context.Background(), ResultRecord{Op: "x"}); err == nil {
		t.Fatal("empty request id accepted")
	}
}

func TestReplaceResultKeepsCreationTimeAndDeleteResultRemoves(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))

	if err := s.PutResult(ctx, ResultRecord{RequestID: "r1", Op: "a", Result: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	first := clk.Now()
	clk.Advance(time.Minute)
	if err := s.ReplaceResult(ctx, ResultRecord{RequestID: "r1", Op: "a", Result: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetResult(ctx, "r1")
	if err != nil || !ok || string(got.Result) != "2" || !got.CreatedAt.Equal(first) {
		t.Fatalf("after replace: %+v ok=%v err=%v", got, ok, err)
	}
	// An id that was pruned meanwhile is inserted again.
	clk.Advance(ResultTTL)
	if err := s.ReplaceResult(ctx, ResultRecord{RequestID: "r1", Op: "a", Result: []byte("3")}); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.GetResult(ctx, "r1"); !ok || string(got.Result) != "3" {
		t.Fatalf("replace of an expired id: %+v ok=%v", got, ok)
	}
	if err := s.DeleteResult(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetResult(ctx, "r1"); ok {
		t.Fatal("entry survived DeleteResult")
	}
	if err := s.DeleteResult(ctx, "nope"); err != nil {
		t.Fatalf("deleting an absent id: %v", err)
	}
	if err := s.ReplaceResult(ctx, ResultRecord{Op: "x"}); err == nil {
		t.Fatal("empty request id accepted")
	}
}

func TestResultCacheKeepsCallerAndPayloadHash(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	rec := ResultRecord{RequestID: "r1", Op: "send_text", Session: "work", Caller: "user user=a actor= key= worker=", CallerSession: "ctl 1", PayloadHash: "abc", Result: []byte("1")}
	if err := s.PutResult(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetResult(ctx, "r1")
	if err != nil || !ok || got.Caller != rec.Caller || got.CallerSession != "ctl 1" || got.PayloadHash != "abc" {
		t.Fatalf("after put: %+v, %v, %v", got, ok, err)
	}
	// The completed result keeps who sent the request and what it carried.
	rec.Result = []byte("2")
	if err := s.ReplaceResult(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.GetResult(ctx, "r1")
	if err != nil || !ok || got.Caller != rec.Caller || got.CallerSession != "ctl 1" || got.PayloadHash != "abc" || string(got.Result) != "2" {
		t.Fatalf("after replace: %+v, %v, %v", got, ok, err)
	}
	// A replace that inserts (the id was pruned meanwhile) stores them too.
	if err := s.ReplaceResult(ctx, ResultRecord{RequestID: "r2", Op: "x", Caller: "local", PayloadHash: "def", Result: []byte("3")}); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.GetResult(ctx, "r2"); !ok || got.Caller != "local" || got.PayloadHash != "def" {
		t.Fatalf("inserted by replace: %+v, %v", got, ok)
	}
}

// A row of a Worker that did not store the caller and the payload (v0.21.0 and
// before) has no op_result_callers row, reads as empty members and compares as
// before.
func TestResultRowFromBeforeTheCallerColumnsComparesAsBefore(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at) VALUES ('old', 'send_text', 'work', 0, '{}', ?)`,
		clk.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	old, ok, err := s.GetResult(ctx, "old")
	if err != nil || !ok || old.Caller != "" || old.PayloadHash != "" {
		t.Fatalf("old row: %+v, %v, %v", old, ok, err)
	}
	next := ResultRecord{Op: "send_text", Session: "work", Caller: "local", PayloadHash: "abc"}
	if !old.SameRequest(next) {
		t.Fatal("an old row must answer a request with the same op, session and force")
	}
	next.Force = true
	if old.SameRequest(next) {
		t.Fatal("an old row must still tell another force apart")
	}
}

func TestResultSameRequest(t *testing.T) {
	base := ResultRecord{Op: "send_text", Session: "work", Caller: "local", CallerSession: "ctl-1", PayloadHash: "abc"}
	for _, tc := range []struct {
		name string
		mut  func(*ResultRecord)
		want bool
	}{
		{"identical", func(*ResultRecord) {}, true},
		{"other op", func(r *ResultRecord) { r.Op = "send_keys" }, false},
		{"other session", func(r *ResultRecord) { r.Session = "other" }, false},
		{"other force", func(r *ResultRecord) { r.Force = true }, false},
		{"other caller", func(r *ResultRecord) { r.Caller = "user user=b actor= key= worker=" }, false},
		{"other payload", func(r *ResultRecord) { r.PayloadHash = "def" }, false},
		{"request without a payload digest", func(r *ResultRecord) { r.PayloadHash = "" }, true},
		{"other source session", func(r *ResultRecord) { r.CallerSession = "other" }, false},
		{"request without a source session (older Hub)", func(r *ResultRecord) { r.CallerSession = "" }, true},
	} {
		next := base
		tc.mut(&next)
		if got := base.SameRequest(next); got != tc.want {
			t.Errorf("%s: SameRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// v0210Migrations is the Worker migration set of v0.21.0 (0001 to 0014), the
// release a v0.22.0 self-update starts from and rolls back to.
func v0210Migrations(t *testing.T) fstest.MapFS {
	t.Helper()
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(workerMigrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range entries {
		if file.IsDir() || file.Name() >= "0015_" {
			continue
		}
		path := migrationsDir + "/" + file.Name()
		body, err := fs.ReadFile(workerMigrations, path)
		if err != nil {
			t.Fatal(err)
		}
		old[path] = &fstest.MapFile{Data: body}
	}
	return old
}

// TestResultCallersNeedNoMigrationSoARollbackKeepsReceipts: this release opens
// a v0.21.0 database without a numbered migration, so a self-update writes no
// pre-migration backup and a rollback has nothing to restore (selfupdate
// restoreStateDB). The receipts of writes the new binary ran stay, the
// v0.21.0 binary accepts the database (no schema_too_new), reads the receipt
// with its own statements and still answers the duplicate from it.
func TestResultCallersNeedNoMigrationSoARollbackKeepsReceipts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := openStore(t, dir, WithMigrations(v0210Migrations(t), migrationsDir))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir)
	if entries, err := os.ReadDir(filepath.Join(dir, backupDirName)); err == nil && len(entries) > 0 {
		t.Fatalf("opening a v0.21.0 database wrote a pre-migration backup %v; a rollback would restore it and lose receipts", entries)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	rec := ResultRecord{RequestID: "sent", Op: "send_text", Session: "work", Caller: "local", CallerSession: "ctl", PayloadHash: "abc", Result: []byte(`{"outcome":"delivered"}`)}
	if err := s.PutResult(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceResult(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back, err := Open(dir, WithMigrations(v0210Migrations(t), migrationsDir))
	if err != nil {
		t.Fatalf("the v0.21.0 migration set refuses the database: %v", err)
	}
	t.Cleanup(func() { _ = back.Close() })
	// The v0.21.0 read of the cache (its column list).
	var op, result string
	if err := back.db.QueryRowContext(ctx,
		`SELECT op, result FROM op_results WHERE request_id = 'sent'`).Scan(&op, &result); err != nil || op != "send_text" || result != string(rec.Result) {
		t.Fatalf("receipt as v0.21.0 reads it: %q %q %v", op, result, err)
	}
	// The v0.21.0 write of the cache (its column list) still works.
	if _, err := back.db.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at) VALUES ('other', 'x', 'w', 0, '{}', 1)`); err != nil {
		t.Fatalf("v0.21.0 insert: %v", err)
	}
}

// An op_results row that a release without op_result_callers inserted again
// after it pruned the earlier one is not described by the caller row it left
// behind: that row has another created_at and is not read.
func TestResultCallerRowOfAnEarlierRequestIsIgnored(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	if err := s.PutResult(ctx, ResultRecord{RequestID: "r", Op: "send_text", Session: "work", Caller: "local", PayloadHash: "abc", Result: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	// What an older binary does: it prunes and inserts op_results only.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM op_results WHERE request_id = 'r'`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO op_results (request_id, op, session_name, force, result, created_at) VALUES ('r', 'send_text', 'work', 0, '2', ?)`,
		clk.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetResult(ctx, "r")
	if err != nil || !ok || got.Caller != "" || got.PayloadHash != "" || string(got.Result) != "2" {
		t.Fatalf("row of the older binary: %+v, %v, %v", got, ok, err)
	}
	// A delete removes both rows.
	if err := s.DeleteResult(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM op_result_callers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("caller rows after delete = %d, %v", n, err)
	}
}

// The first Put of a live id names its caller; a second Put does not replace
// it, and pruning removes the caller rows with their results.
func TestResultCallerFirstPutWinsAndIsPruned(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	s := openStore(t, t.TempDir(), WithClock(clk.Now))
	if err := s.PutResult(ctx, ResultRecord{RequestID: "r", Op: "x", Caller: "a", Result: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutResult(ctx, ResultRecord{RequestID: "r", Op: "x", Caller: "b", Result: []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetResult(ctx, "r"); got.Caller != "a" {
		t.Fatalf("caller = %q, want the first", got.Caller)
	}
	clk.Advance(ResultTTL + time.Second)
	if _, err := s.PruneResults(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM op_result_callers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("caller rows after prune = %d, %v", n, err)
	}
}
