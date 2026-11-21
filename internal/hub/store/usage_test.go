package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func usageFixture(t testing.TB, s *Store, n int) ([]ops.WorkerInfo, ops.UsageReport, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	a := ops.UsageAccount{ID: ops.UsageID("account", "subscription", "synthetic"), Provider: "example", AuthMode: "subscription", ObservedAt: now.UnixMilli(), Source: "test"}
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{a}}
	for i := range n {
		count := int64(i)
		r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("session", fmt.Sprint(i)), Name: fmt.Sprintf("private-%d", i), AuthMode: "subscription", AccountID: a.ID, ObservedAt: r.ObservedAt, Source: "test", UsageCounters: ops.UsageCounters{InputTokens: &count}})
	}
	workers := []ops.WorkerInfo{{ID: "w1", OwnerID: "alice"}, {ID: "w2", OwnerID: "bob"}}
	for i, w := range workers {
		if err := s.InsertWorker(context.Background(), nil, worker(w.ID, byte(i+1), w.OwnerID, w.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO users(id,username,created_at,updated_at) VALUES(?,?,1,1)`, w.OwnerID, w.OwnerID); err != nil {
			t.Fatal(err)
		}
	}
	return workers, r, now
}

func TestUsageAuthorizationAndPrivateAssignment(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, r, now := usageFixture(t, s, 2)
	for _, w := range workers {
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	list := func(w []ops.WorkerInfo, owner, section, bucket string) ops.UsageListResult {
		t.Helper()
		out, err := s.ReadUsage(ctx, w, owner, ops.UsageListArgs{Section: section, BucketID: bucket}, now)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := list(workers, "alice", "accounts", ""); len(got.Accounts) != 1 {
		t.Fatalf("duplicate account rows: %+v", got)
	}
	if got := list(workers[:1], "alice", "sessions", ""); len(got.Sessions) != 2 {
		t.Fatalf("wrong scope: %+v", got)
	}
	move := func(args ops.UsageBucketSetArgs) ops.UsageBucketSetResult {
		t.Helper()
		res, err := s.SetUsageBucket(ctx, "alice", workers[:1], args)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	b := move(ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Work"})
	c := move(ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Private"})
	move(ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: b.ID, All: true})
	if got := list(workers, "bob", "sessions", b.ID); len(got.Sessions) != 0 {
		t.Fatal("private buckets crossed owners")
	}
	ids := list(workers[:1], "alice", "sessions", b.ID).Sessions
	if len(ids) != 2 {
		t.Fatal("bulk selection")
	}
	move(ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: c.ID, Members: []string{ids[0].ID}})
	if len(list(workers, "alice", "sessions", b.ID).Sessions) != 1 || len(list(workers, "alice", "sessions", c.ID).Sessions) != 1 {
		t.Fatal("exclusive move")
	}
	move(ops.UsageBucketSetArgs{Action: "assign", Kind: "session", Members: []string{ids[0].ID}})
	if len(list(workers[:1], "alice", "sessions", "unassigned").Sessions) != 1 {
		t.Fatal("unassigned")
	}
	if _, err := s.SetUsageBucket(ctx, "bob", workers, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: b.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted another owner's bucket: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE workers SET owner_id='bob' WHERE id='w1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsage(ctx, "w1", "alice", r, now); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("stale owner accepted: %v", err)
	}
	if got := list(workers[:1], "alice", "sessions", ""); len(got.Sessions) != 0 {
		t.Fatal("stale authorization")
	}
}

func TestUsageReplaysAndIncompleteEnumeration(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, r, now := usageFixture(t, s, 2)
	save := func(x ops.UsageReport) {
		t.Helper()
		if err := s.SaveUsage(ctx, "w1", "alice", x, now); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []ops.UsageSessionRow {
		t.Helper()
		v, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "sessions"}, now)
		if err != nil {
			t.Fatal(err)
		}
		return v.Sessions
	}
	save(r)
	replay := r
	r.ObservedAt++
	r.Complete = false
	r.Sessions = nil
	save(r)
	if row := read()[0]; row.StoppedAt != nil || row.LastSeen != replay.ObservedAt {
		t.Fatalf("incomplete observation changed liveness: %+v", row)
	}
	r.ObservedAt++
	r.Complete = true
	save(r)
	save(replay)
	if row := read()[0]; row.StoppedAt == nil || *row.StoppedAt != r.ObservedAt {
		t.Fatalf("replay reopened session: %+v", row)
	}
	if err := s.TrimUsage(ctx, now.Add(100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(read()) != 0 {
		t.Fatal("retention did not remove expired history")
	}
}

func BenchmarkUsage50Sessions(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	_, r, now := usageFixture(b, s, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		at := now.Add(time.Duration(i) * 5 * time.Second)
		r.ObservedAt = at.UnixMilli()
		for j := range r.Sessions {
			r.Sessions[j].ObservedAt = r.ObservedAt
		}
		if err := s.SaveUsage(context.Background(), "w1", "alice", r, at); err != nil {
			b.Fatal(err)
		}
	}
}

func TestUsageCollectorDedupAndBackoff(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, r, now := usageFixture(t, s, 1)
	r.Accounts[0].Provider = "OpenAI"
	for _, w := range workers {
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(worker ops.WorkerInfo, at time.Time) UsageLease {
		t.Helper()
		l, err := s.ClaimUsagePoll(ctx, worker.ID, worker.OwnerID, at)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	first := claim(workers[0], now)
	if first.ID == "" || claim(workers[1], now).ID != "" {
		t.Fatal("duplicated one account on multiple Workers")
	}
	if err := s.CompleteUsagePoll(ctx, first, nil, "rate_limited", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if l := claim(workers[1], now.Add(59*time.Minute)); l.ID != "" {
		t.Fatal("ignored Retry-After")
	}
	dest := ops.UsageCollectorSetArgs{AccountID: r.Accounts[0].ID, WorkerID: workers[1].ID}
	if _, err := s.SetUsageCollector(ctx, workers[:1], dest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("limited owner redirected shared polling: %v", err)
	}
	if _, err := s.SetUsageCollector(ctx, workers, dest); err != nil {
		t.Fatal(err)
	}
	later := now.Add(62 * time.Minute)
	if l := claim(workers[0], later); l.ID != "" {
		t.Fatal("wrong selected collector")
	}
	second := claim(workers[1], later)
	if second.ID == "" {
		t.Fatal("no selected collector")
	}
	a := r.Accounts[0]
	a.ObservedAt = later.UnixMilli()
	if err := s.CompleteUsagePoll(ctx, second, &a, "ready", 0, later); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteUsagePoll(ctx, first, &a, "ready", 0, later); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired lease accepted")
	}
	if l := claim(workers[1], later.Add(time.Minute)); l.ID != "" {
		t.Fatal("polled within five minutes")
	}
}

func TestUsagePersistenceHasNoCompleteKey(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	workers, r, now := usageFixture(t, s, 1)
	rawKey := "sk-synthetic-long-high-entropy-fixture-d3d3607f87f549f3"
	domainSecret := []byte("synthetic-domain-fingerprint-secret-9af96f87e6c84922")
	// Construct exactly the typed wire projection emitted by the local
	// credential adapter. Its complete credential/secret are never fields.
	h := hmac.New(sha256.New, domainSecret)
	h.Write([]byte(ops.UsageID("api-key", "test", "https://example.invalid", rawKey)))
	id := hex.EncodeToString(h.Sum(nil))
	a := ops.UsageAccount{ID: id, Label: rawKey[:10] + "... / " + id[:10], Provider: "test", AuthMode: "api", Source: "native-api-identity", ObservedAt: r.ObservedAt}
	r.Accounts = []ops.UsageAccount{a}
	frame, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	check := func(b []byte) {
		t.Helper()
		if bytes.Contains(b, []byte(rawKey)) || bytes.Contains(b, domainSecret) {
			t.Fatal("complete credential or private seed present")
		}
	}
	check(frame)
	if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, r, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadUsage(ctx, workers, "alice", ops.UsageListArgs{Section: "accounts"}, now)
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].ID != id {
		t.Fatalf("missing projected account: %v", err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	check(b)
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(FULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(s.Dir(), DBFile+"*"))
	if err != nil || len(files) == 0 {
		t.Fatal("missing fixture database files")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(data)
	}
}
