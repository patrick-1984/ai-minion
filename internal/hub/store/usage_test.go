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
	"strings"
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
	r.Accounts[0].Source = "codex-account"
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

// BenchmarkUsageMixedSessions covers active sessions without writing a row
// per poll. For the 30-minute trace use -benchtime=360x at five-second ticks.
func BenchmarkUsageMixedSessions(b *testing.B) {
	for _, count := range []int{20, 50} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			workers, r, start := usageFixture(b, s, count)
			var worst time.Duration
			b.ResetTimer()
			for tick := 0; tick < b.N; tick++ {
				at := start.Add(time.Duration(tick) * 5 * time.Second)
				r.ObservedAt = at.UnixMilli()
				for i := range r.Sessions {
					r.Sessions[i].ObservedAt = r.ObservedAt
					if i%3 == tick%3 {
						v := int64((tick+1)*100 + i)
						r.Sessions[i].InputTokens = &v
					}
				}
				begin := time.Now()
				if err := s.SaveUsage(b.Context(), "w1", "alice", r, at); err != nil {
					b.Fatal(err)
				}
				worst = max(worst, time.Since(begin))
				if tick%4 == 0 {
					page, err := s.ReadUsage(b.Context(), workers, "alice", ops.UsageListArgs{Section: "sessions"}, at)
					if err != nil || len(page.Sessions) != count {
						b.Fatal("incomplete usage read", err)
					}
				}
			}
			b.StopTimer()
			var hours, events int
			if err := s.db.QueryRow(`SELECT count(*) FROM usage_hours`).Scan(&hours); err != nil {
				b.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&events); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(hours), "hourly_rows")
			b.ReportMetric(float64(events), "host_events")
			b.ReportMetric(float64(worst.Microseconds())/1000, "worst_write_ms")
		})
	}
}

func TestUsageRetentionDrainsBusyMinuteInBoundedBatches(t *testing.T) {
	s := openTest(t)
	workers, _, now := usageFixture(t, s, 0)
	// Two minutes at ten events per second exceed one SQLite batch.
	old := now.Add(-8 * 24 * time.Hour).UnixMilli()
	_, err := s.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1200)
	 INSERT INTO usage_events(worker_id,seq,session_id,observed_at,body) SELECT ?,i,'',?,'{}' FROM n`, workers[0].ID, old)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO usage_events(worker_id,seq,session_id,observed_at,body) VALUES(?,1201,'',?,'{}')`, workers[0].ID, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TrimUsage(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events after retention: %d, %v", n, err)
	}
}

func TestUsageAccountsTotalsSumApiKeySessionsOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	api := ops.UsageAccount{ID: ops.UsageID("account", "api", "k1"), Provider: "Anthropic", AuthMode: "api", ObservedAt: now.UnixMilli(), Source: "test"}
	sub := ops.UsageAccount{ID: ops.UsageID("account", "subscription", "s1"), Provider: "Anthropic", AuthMode: "subscription", ObservedAt: now.UnixMilli(), Source: "test"}
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{api, sub}}
	for i, in := range []int64{100, 250} {
		out, cost := in/10, float64(i+1)/2
		r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("session", "api", fmt.Sprint(i)), Name: "api", AuthMode: "api", AccountID: api.ID, ObservedAt: r.ObservedAt, Source: "test",
			UsageCounters: ops.UsageCounters{InputTokens: &in, OutputTokens: &out, CostUSD: &cost}})
	}
	n := int64(7)
	r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("session", "sub"), Name: "sub", AuthMode: "subscription", AccountID: sub.ID, ObservedAt: r.ObservedAt, Source: "test", UsageCounters: ops.UsageCounters{InputTokens: &n}})
	if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, r, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, now)
	if err != nil || len(got.Accounts) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, a := range got.Accounts {
		switch a.AuthMode {
		case "subscription":
			if a.Totals != nil {
				t.Fatalf("subscription account got totals: %+v", a.Totals)
			}
		case "api":
			tt := a.Totals
			if tt == nil || tt.Sessions != 2 || tt.InputTokens == nil || *tt.InputTokens != 350 || tt.OutputTokens == nil || *tt.OutputTokens != 35 ||
				tt.CostUSD == nil || *tt.CostUSD != 1.5 || tt.EstimatedCostUSD != nil {
				t.Fatalf("totals: %+v", tt)
			}
		}
	}
	// A Worker outside the caller's scope adds nothing.
	got, err = s.ReadUsage(ctx, workers[1:], "bob", ops.UsageListArgs{Section: "accounts"}, now)
	if err != nil || len(got.Accounts) != 0 {
		t.Fatalf("scope: %+v %v", got, err)
	}
}

func TestUsageAccountsTotalsHonourWorkerFilter(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	api := ops.UsageAccount{ID: ops.UsageID("account", "api", "shared"), Provider: "Anthropic", AuthMode: "api", ObservedAt: now.UnixMilli(), Source: "test"}
	for i, w := range workers[:2] {
		in := int64(100 * (i + 1))
		r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{api}, Sessions: []ops.UsageSession{{
			ID: ops.UsageID("session", "shared", fmt.Sprint(i)), Name: "api", AuthMode: "api", AccountID: api.ID, ObservedAt: now.UnixMilli(), Source: "test",
			UsageCounters: ops.UsageCounters{InputTokens: &in}}}}
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		worker string
		want   int64
	}{{"", 300}, {workers[0].ID, 100}, {workers[1].ID, 200}} {
		got, err := s.ReadUsage(ctx, workers[:2], "alice", ops.UsageListArgs{Section: "accounts", WorkerID: c.worker}, now)
		if err != nil || len(got.Accounts) != 1 || got.Accounts[0].Totals == nil || got.Accounts[0].Totals.InputTokens == nil || *got.Accounts[0].Totals.InputTokens != c.want {
			t.Fatalf("worker %q: %+v %v", c.worker, got.Accounts, err)
		}
	}
}

func TestUsageSessionsKeepNaturalOrderAcrossPages(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true}
	for _, n := range []int{10, 2, 11, 1, 9, 3} {
		r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("session", "nat", fmt.Sprint(n)), Name: fmt.Sprintf("wb-%d", n), AuthMode: "unknown", ObservedAt: r.ObservedAt, Source: "test"})
	}
	if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, r, now); err != nil {
		t.Fatal(err)
	}
	var names []string
	after := ""
	for range 10 {
		page, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "sessions", Limit: 2, After: after}, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Sessions {
			names = append(names, row.Name)
		}
		if after = page.Next; after == "" {
			break
		}
	}
	if got := strings.Join(names, ","); got != "wb-1,wb-2,wb-3,wb-9,wb-10,wb-11" {
		t.Fatalf("order across pages: %s", got)
	}
}

func TestUsageAccountKeepsNativeLoginLabelAcrossQuotaSamples(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	const email = "someone.long.address@example.com"
	id := ops.UsageID("account", "claude-login")
	save := func(step int, source, label string) {
		at := now.Add(time.Duration(step) * time.Second)
		a := ops.UsageAccount{ID: id, Provider: "Anthropic", Label: label, AuthMode: "subscription", ObservedAt: at.UnixMilli(), Source: source}
		r := ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Complete: true, Accounts: []ops.UsageAccount{a}}
		if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, r, at); err != nil {
			t.Fatal(err)
		}
	}
	label := func(step int) string {
		got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, now.Add(time.Duration(step)*time.Second))
		if err != nil || len(got.Accounts) != 1 {
			t.Fatalf("%+v %v", got, err)
		}
		return got.Accounts[0].Label
	}
	save(1, "native-test", email)
	save(2, "claude-statusline", "Anthropic subscription")
	if got := label(2); got != email {
		t.Fatalf("label after quota sample: %q", got)
	}
	save(3, "native-test", email)
	if got := label(3); got != email {
		t.Fatalf("label after refresh: %q", got)
	}
}

func TestUsageAccountKeepsLoginLabelThroughConsecutiveQuotaSamples(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	const email = "someone.long.address@example.com"
	id := ops.UsageID("account", "claude-login-2")
	save := func(step int, source, label string) {
		at := now.Add(time.Duration(step) * time.Second)
		a := ops.UsageAccount{ID: id, Provider: "Anthropic", Label: label, AuthMode: "subscription", ObservedAt: at.UnixMilli(), Source: source}
		if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, ops.UsageReport{Version: 1, ObservedAt: at.UnixMilli(), Accounts: []ops.UsageAccount{a}}, at); err != nil {
			t.Fatal(err)
		}
	}
	save(1, "native-test", email)
	for step := 2; step <= 4; step++ {
		save(step, "claude-statusline", fmt.Sprintf("Anthropic subscription %d", step))
		got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "accounts"}, now.Add(10*time.Second))
		if err != nil || len(got.Accounts) != 1 || got.Accounts[0].Label != email {
			t.Fatalf("label after quota sample %d: %+v %v", step, got.Accounts, err)
		}
	}
}

func TestUsageSessionCursorSurvivesADisappearingRowAndDuplicateWorkerNames(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	if _, err := s.db.Exec(`UPDATE workers SET name='shared'`); err != nil {
		t.Skip("worker names must be unique: ", err)
	}
	for _, w := range workers {
		r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true}
		for _, n := range []int{1, 2, 3, 10, 11} {
			r.Sessions = append(r.Sessions, ops.UsageSession{ID: ops.UsageID("session", w.ID, fmt.Sprint(n)), Name: fmt.Sprintf("wb-%d", n), AuthMode: "unknown", ObservedAt: r.ObservedAt, Source: "test"})
		}
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	page := func(after string) ops.UsageListResult {
		got, err := s.ReadUsage(ctx, workers, "alice", ops.UsageListArgs{Section: "sessions", Limit: 3, After: after}, now)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	var want []string
	for after := ""; ; {
		got := page(after)
		for _, r := range got.Sessions {
			want = append(want, r.ID)
		}
		if after = got.Next; after == "" {
			break
		}
	}
	if len(want) != 10 {
		t.Fatalf("rows: %d", len(want))
	}
	first := page("")
	var gotIDs []string
	for _, r := range first.Sessions {
		gotIDs = append(gotIDs, r.ID)
	}
	// The row that the cursor names is gone before the next request.
	if _, err := s.db.Exec(`DELETE FROM usage_sessions WHERE id=?`, first.Sessions[len(first.Sessions)-1].ID); err != nil {
		t.Fatal(err)
	}
	for after := first.Next; after != ""; {
		got := page(after)
		for _, r := range got.Sessions {
			gotIDs = append(gotIDs, r.ID)
		}
		after = got.Next
	}
	if len(gotIDs) != len(want) || fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("pages changed after the cursor row disappeared:\n got %v\nwant %v", gotIDs, want)
	}
	if _, err := s.ReadUsage(ctx, workers, "alice", ops.UsageListArgs{Section: "sessions", After: "not-a-cursor"}, now); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Fatalf("invalid cursor: %v", err)
	}
}

func TestUsageInventoryOfAnOlderSampleIsStillRecorded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	w := workers[0]
	save := func(observed time.Time, inventoryAt *time.Time, exporter string) {
		r := ops.UsageReport{Version: 1, ObservedAt: observed.UnixMilli(), Complete: true}
		if inventoryAt != nil {
			ms := inventoryAt.UnixMilli()
			r.InventoryAt = &ms
			r.Inventory = []ops.UsageHarness{{Name: "claude", Status: "installed", Exporter: exporter}}
		}
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	scan := now.Add(10 * time.Second)
	save(now.Add(20*time.Second), nil, "") // background sample commits first
	save(now.Add(15*time.Second), &scan, "installed")
	got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "workers"}, now)
	if err != nil || len(got.Workers) != 1 || got.Workers[0].InventoryAt == nil || *got.Workers[0].InventoryAt != scan.UnixMilli() || len(got.Workers[0].Inventory) != 1 || got.Workers[0].Inventory[0].Exporter != "installed" {
		t.Fatalf("inventory not recorded: %+v %v", got.Workers, err)
	}
}

func TestUsageNeverStoresAnEndpointHostFromAnOlderWorker(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	workers, _, now := usageFixture(t, s, 0)
	r := ops.UsageReport{Version: 1, ObservedAt: now.UnixMilli(), Complete: true, Sessions: []ops.UsageSession{{
		ID: ops.UsageID("session", "gw"), Name: "gw", AuthMode: "api", Endpoint: "cpa.internal.example:8443", ObservedAt: now.UnixMilli(), Source: "test"}}}
	if err := s.SaveUsage(ctx, workers[0].ID, workers[0].OwnerID, r, now); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := s.db.QueryRow(`SELECT body FROM usage_sessions`).Scan(&body); err != nil || strings.Contains(body, "cpa.internal") {
		t.Fatalf("host stored: %s %v", body, err)
	}
	got, err := s.ReadUsage(ctx, workers[:1], "alice", ops.UsageListArgs{Section: "sessions"}, now)
	if err != nil || len(got.Sessions) != 1 || !got.Sessions[0].CustomEndpoint || got.Sessions[0].Endpoint != "" {
		t.Fatalf("%+v %v", got.Sessions, err)
	}
}
