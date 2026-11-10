package audit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// stepClock returns a clock that starts at base and advances by step on every
// call.
func stepClock(base time.Time, step time.Duration) store.Clock {
	var mu sync.Mutex
	cur := base
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := cur
		cur = cur.Add(step)
		return t
	}
}

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func good(action string) audit.Event {
	return audit.Event{Actor: "user-1", Action: action, Target: "worker-1", Outcome: "success"}
}

func TestRecordWritesRowWithUUIDv7AndUnixMS(t *testing.T) {
	s := openStore(t)
	now := t0.Add(123 * time.Millisecond)
	l := audit.New(s, audit.WithClock(func() time.Time { return now }))
	ev := audit.Event{Actor: "admin-1", ActingAs: "user-2", Action: "worker.remove", Target: "worker-9", Outcome: "success"}
	if err := l.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	var (
		id                                       string
		ts                                       int64
		actor, actingAs, action, target, outcome string
	)
	err := s.DB().QueryRow(`SELECT id, ts, actor, acting_as, action, target, outcome FROM audit_events`).
		Scan(&id, &ts, &actor, &actingAs, &action, &target, &outcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ParseID(id); err != nil {
		t.Errorf("id is not a UUIDv7: %v", err)
	}
	if ts != now.UnixMilli() {
		t.Errorf("ts = %d, want unix ms %d", ts, now.UnixMilli())
	}
	if actor != "admin-1" || actingAs != "user-2" || action != "worker.remove" || target != "worker-9" || outcome != "success" {
		t.Errorf("row = %q %q %q %q %q", actor, actingAs, action, target, outcome)
	}
}

func TestRecordKeepsEventsDistinctWithOwnIDs(t *testing.T) {
	s := openStore(t)
	l := audit.New(s)
	for range 20 {
		if err := l.Record(context.Background(), good("login")); err != nil {
			t.Fatal(err)
		}
	}
	var n, distinct int
	if err := s.DB().QueryRow(`SELECT count(*), count(DISTINCT id) FROM audit_events`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != 20 || distinct != 20 {
		t.Errorf("rows = %d, distinct ids = %d", n, distinct)
	}
}

func TestOversizeFieldIsRejectedAndNotWritten(t *testing.T) {
	s := openStore(t)
	l := audit.New(s)
	long := strings.Repeat("x", audit.MaxFieldLen+1)
	fields := map[string]func(*audit.Event, string){
		"actor":     func(e *audit.Event, v string) { e.Actor = v },
		"acting_as": func(e *audit.Event, v string) { e.ActingAs = v },
		"action":    func(e *audit.Event, v string) { e.Action = v },
		"target":    func(e *audit.Event, v string) { e.Target = v },
		"outcome":   func(e *audit.Event, v string) { e.Outcome = v },
	}
	for name, set := range fields {
		t.Run(name, func(t *testing.T) {
			ev := good("login")
			set(&ev, long)
			err := l.Record(context.Background(), ev)
			if !errors.Is(err, audit.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			var fe *audit.FieldError
			if !errors.As(err, &fe) || fe.Field != name {
				t.Errorf("err = %v, want a FieldError for %s", err, name)
			}
			if strings.Contains(err.Error(), "xxxx") {
				t.Errorf("the error leaks the value: %v", err)
			}
			// Exactly the limit is fine.
			ev = good("login")
			set(&ev, strings.Repeat("y", audit.MaxFieldLen))
			if err := l.Record(context.Background(), ev); err != nil {
				t.Errorf("a value of %d characters was rejected: %v", audit.MaxFieldLen, err)
			}
		})
	}
	// Only the five exactly-at-the-limit events were written.
	var n, longest int
	if err := s.DB().QueryRow(`SELECT count(*), max(length(actor), length(acting_as), length(action), length(target), length(outcome)) FROM audit_events`).Scan(&n, &longest); err != nil {
		t.Fatal(err)
	}
	if n != len(fields) || longest > audit.MaxFieldLen {
		t.Errorf("rows = %d (want %d), longest value = %d", n, len(fields), longest)
	}
}

func TestLimitCountsCharactersNotBytes(t *testing.T) {
	l := audit.New(openStore(t))
	ev := good("login")
	ev.Target = strings.Repeat("\u00e9", audit.MaxFieldLen) // 512 bytes, 256 characters
	if err := l.Record(context.Background(), ev); err != nil {
		t.Errorf("256 characters were rejected: %v", err)
	}
	ev.Target = strings.Repeat("\u00e9", audit.MaxFieldLen+1)
	if err := l.Record(context.Background(), ev); !errors.Is(err, audit.ErrInvalid) {
		t.Errorf("257 characters accepted: %v", err)
	}
}

func TestRequiredFieldsAndContentGuards(t *testing.T) {
	l := audit.New(openStore(t))
	tests := []struct {
		name string
		mod  func(*audit.Event)
	}{
		{"empty actor", func(e *audit.Event) { e.Actor = "" }},
		{"empty action", func(e *audit.Event) { e.Action = "" }},
		{"empty outcome", func(e *audit.Event) { e.Outcome = "" }},
		{"newline in target", func(e *audit.Event) { e.Target = "line one\nline two" }},
		{"escape in target", func(e *audit.Event) { e.Target = "\x1b[31mred" }},
		{"invalid utf-8", func(e *audit.Event) { e.Action = "a\xffb" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := good("login")
			tc.mod(&ev)
			if err := l.Record(context.Background(), ev); !errors.Is(err, audit.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
	if err := l.Record(context.Background(), audit.Event{Actor: "a", Action: "b", Outcome: "c"}); err != nil {
		t.Errorf("empty acting_as and target must be allowed: %v", err)
	}
}

// seed records n events with the given actor/action pattern, one second apart.
func seed(t *testing.T, s *store.Store, clock store.Clock, evs ...audit.Event) *audit.Log {
	t.Helper()
	l := audit.New(s, audit.WithClock(clock))
	for _, e := range evs {
		if err := l.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func actions(p audit.Page) string {
	var out []string
	for _, e := range p.Events {
		out = append(out, e.Action)
	}
	return strings.Join(out, ",")
}

func TestListNewestFirstWithFilters(t *testing.T) {
	s := openStore(t)
	l := seed(t, s, stepClock(t0, time.Second),
		audit.Event{Actor: "alice", Action: "login", Outcome: "success"},      // t0+0
		audit.Event{Actor: "bob", Action: "login", Outcome: "denied"},         // +1
		audit.Event{Actor: "alice", Action: "key.create", Outcome: "success"}, // +2
		audit.Event{Actor: "bob", Action: "role.set", Outcome: "success"},     // +3
		audit.Event{Actor: "alice", Action: "logout", Outcome: "success"},     // +4
	)
	ctx := context.Background()
	tests := []struct {
		name string
		f    audit.Filter
		want string
	}{
		{"all", audit.Filter{}, "logout,role.set,key.create,login,login"},
		{"actor", audit.Filter{Actor: "alice"}, "logout,key.create,login"},
		{"action", audit.Filter{Action: "login"}, "login,login"},
		{"actor and action", audit.Filter{Actor: "bob", Action: "login"}, "login"},
		{"from inclusive", audit.Filter{From: t0.Add(2 * time.Second)}, "logout,role.set,key.create"},
		{"to exclusive", audit.Filter{To: t0.Add(2 * time.Second)}, "login,login"},
		{"range", audit.Filter{From: t0.Add(time.Second), To: t0.Add(4 * time.Second)}, "role.set,key.create,login"},
		{"no match", audit.Filter{Actor: "carol"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := l.List(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := actions(p); got != tc.want {
				t.Errorf("events = %s, want %s", got, tc.want)
			}
			if p.NextCursor != "" {
				t.Errorf("unexpected next cursor %q", p.NextCursor)
			}
		})
	}
	p, _ := l.List(ctx, audit.Filter{Limit: 1})
	e := p.Events[0]
	if !e.Time.Equal(t0.Add(4*time.Second)) || e.Time.Location() != time.UTC {
		t.Errorf("time = %v", e.Time)
	}
	if e.Actor != "alice" || e.Outcome != "success" || e.Target != "" || e.ActingAs != "" {
		t.Errorf("event = %+v", e)
	}
}

func TestListPaginatesWithCursorIncludingEqualTimestamps(t *testing.T) {
	s := openStore(t)
	// A frozen clock puts every event in the same millisecond, so the order
	// and the cursor depend on the id tie-break.
	l := audit.New(s, audit.WithClock(func() time.Time { return t0 }))
	for i := range 7 {
		if err := l.Record(context.Background(), good(fmt.Sprintf("a%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var all []string
	cursor := ""
	pages := 0
	for {
		p, err := l.List(ctx, audit.Filter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(p.Events) > 3 {
			t.Fatalf("page has %d events", len(p.Events))
		}
		for _, e := range p.Events {
			all = append(all, e.Action)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(all) != 7 {
		t.Fatalf("got %d events: %v", len(all), all)
	}
	seen := map[string]bool{}
	for _, a := range all {
		if seen[a] {
			t.Errorf("event %s listed twice", a)
		}
		seen[a] = true
	}
	// Newest first: ids are time ordered, so the order is a6..a0.
	if strings.Join(all, ",") != "a6,a5,a4,a3,a2,a1,a0" {
		t.Errorf("order = %v", all)
	}
}

func TestListExactPageBoundaryHasNoNextCursor(t *testing.T) {
	s := openStore(t)
	l := seed(t, s, stepClock(t0, time.Second), good("a"), good("b"), good("c"), good("d"))
	p, err := l.List(context.Background(), audit.Filter{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 4 || p.NextCursor != "" {
		t.Errorf("events = %d, cursor = %q", len(p.Events), p.NextCursor)
	}
}

func TestCursorKeepsFiltersConsistent(t *testing.T) {
	s := openStore(t)
	var evs []audit.Event
	for i := range 6 {
		who := "alice"
		if i%2 == 1 {
			who = "bob"
		}
		evs = append(evs, audit.Event{Actor: who, Action: fmt.Sprintf("n%d", i), Outcome: "success"})
	}
	l := seed(t, s, stepClock(t0, time.Second), evs...)
	ctx := context.Background()
	p1, err := l.List(ctx, audit.Filter{Actor: "alice", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := l.List(ctx, audit.Filter{Actor: "alice", Limit: 2, Cursor: p1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if actions(p1) != "n4,n2" || actions(p2) != "n0" || p2.NextCursor != "" {
		t.Errorf("pages = %s | %s", actions(p1), actions(p2))
	}
}

func TestListRejectsBadArguments(t *testing.T) {
	l := audit.New(openStore(t))
	ctx := context.Background()
	tests := []struct {
		name string
		f    audit.Filter
	}{
		{"negative limit", audit.Filter{Limit: -1}},
		{"limit too big", audit.Filter{Limit: ops.MaxAuditListLimit + 1}},
		{"cursor garbage", audit.Filter{Cursor: "!!!"}},
		{"cursor wrong shape", audit.Filter{Cursor: "bm90LWEtY3Vyc29y"}},
		{"from after to", audit.Filter{From: t0.Add(time.Hour), To: t0}},
		{"oversize actor", audit.Filter{Actor: strings.Repeat("x", audit.MaxFieldLen+1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.List(ctx, tc.f); !errors.Is(err, audit.ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRecordTxCommitsAndRollsBackWithTheCaller(t *testing.T) {
	s := openStore(t)
	l := audit.New(s)
	ctx := context.Background()
	count := func() int {
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM audit_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := l.RecordTx(ctx, tx, good("role.set")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || count() != 0 {
		t.Fatalf("rollback: err = %v, rows = %d", err, count())
	}
	if err := s.Tx(ctx, func(tx *sql.Tx) error { return l.RecordTx(ctx, tx, good("role.set")) }); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Errorf("commit: rows = %d", count())
	}
	if err := l.RecordTx(ctx, nil, good("x")); err == nil {
		t.Error("RecordTx accepted a nil transaction")
	}
	// An invalid event fails before touching the transaction.
	err = s.Tx(ctx, func(tx *sql.Tx) error { return l.RecordTx(ctx, tx, audit.Event{}) })
	if !errors.Is(err, audit.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestConcurrentRecordsAreAllStored(t *testing.T) {
	s := openStore(t)
	l := audit.New(s)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Record(context.Background(), good(fmt.Sprintf("c%d", i)))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := l.List(context.Background(), audit.Filter{Limit: ops.MaxAuditListLimit})
	if err != nil || len(p.Events) != 40 {
		t.Fatalf("listed %d events, err %v", len(p.Events), err)
	}
}

func TestListOpMapsArgumentsAndEncodesRFC3339Milliseconds(t *testing.T) {
	s := openStore(t)
	base := t0.Add(7 * time.Millisecond)
	l := seed(t, s, stepClock(base, 1500*time.Millisecond),
		audit.Event{Actor: "alice", Action: "login", Outcome: "success"},
		audit.Event{Actor: "admin", ActingAs: "alice", Action: "act_as.start", Target: "alice", Outcome: "success"},
		audit.Event{Actor: "alice", Action: "logout", Outcome: "success"},
	)
	ctx := context.Background()
	from := ops.Timestamp(base.Add(time.Second))
	res, err := l.ListOp(ctx, ops.AuditListArgs{From: &from, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 || res.NextCursor != "" {
		t.Fatalf("result = %+v", res)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	first, second := got.Events[0], got.Events[1]
	if first["action"] != "logout" || first["ts"] != "2026-11-09T12:00:03.007Z" {
		t.Errorf("first = %v", first)
	}
	if second["acting_as"] != "alice" || second["target"] != "alice" || second["ts"] != "2026-11-09T12:00:01.507Z" {
		t.Errorf("second = %v", second)
	}
	if _, has := first["acting_as"]; has {
		t.Error("an empty acting_as must be omitted")
	}
	if err := store.ParseID(first["id"].(string)); err != nil {
		t.Errorf("event id: %v", err)
	}

	// Pagination through the op: the cursor round-trips.
	p1, err := l.ListOp(ctx, ops.AuditListArgs{Limit: 2})
	if err != nil || p1.NextCursor == "" {
		t.Fatalf("page 1 = %+v, err %v", p1, err)
	}
	p2, err := l.ListOp(ctx, ops.AuditListArgs{Limit: 2, Cursor: p1.NextCursor})
	if err != nil || len(p2.Events) != 1 || p2.Events[0].Action != "login" {
		t.Fatalf("page 2 = %+v, err %v", p2, err)
	}

	// Invalid arguments are invalid_argument errors of the ops package.
	if _, err := l.ListOp(ctx, ops.AuditListArgs{Limit: -3}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("err = %v, want ops.ErrInvalidArgument", err)
	}
	if _, err := l.ListOp(ctx, ops.AuditListArgs{Cursor: "AAAA"}); !errors.Is(err, audit.ErrInvalid) || !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("err = %v, want audit.ErrInvalid and ops.ErrInvalidArgument", err)
	}
}

func TestEmptyListEncodesAnEmptyArray(t *testing.T) {
	l := audit.New(openStore(t))
	res, err := l.ListOp(context.Background(), ops.AuditListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if string(b) != `{"events":[]}` {
		t.Errorf("json = %s", b)
	}
}

// fakeRepo shows that the package works against the port alone.
type fakeRepo struct {
	rows    []store.AuditRow
	lastQ   store.AuditQuery
	failing error
}

func (f *fakeRepo) InsertAudit(_ context.Context, _ store.Execer, r store.AuditRow) error {
	if f.failing != nil {
		return f.failing
	}
	f.rows = append(f.rows, r)
	return nil
}

func (f *fakeRepo) ListAudit(_ context.Context, q store.AuditQuery) ([]store.AuditRow, error) {
	f.lastQ = q
	return f.rows, f.failing
}

func TestLogUsesOnlyThePortAndPropagatesErrors(t *testing.T) {
	repo := &fakeRepo{}
	n := 0
	l := audit.New(repo,
		audit.WithClock(func() time.Time { return t0 }),
		audit.WithIDFunc(func() string { n++; return fmt.Sprintf("id-%d", n) }))
	ctx := context.Background()
	if err := l.Record(ctx, good("login")); err != nil {
		t.Fatal(err)
	}
	if len(repo.rows) != 1 || repo.rows[0].ID != "id-1" || repo.rows[0].TS != t0.UnixMilli() {
		t.Fatalf("rows = %+v", repo.rows)
	}
	if _, err := l.List(ctx, audit.Filter{}); err != nil {
		t.Fatal(err)
	}
	if repo.lastQ.Limit != ops.DefaultAuditListLimit+1 {
		t.Errorf("the port got limit %d, want default+1", repo.lastQ.Limit)
	}
	repo.failing = errors.New("disk full")
	if err := l.Record(ctx, good("login")); err == nil || errors.Is(err, audit.ErrInvalid) {
		t.Errorf("storage error = %v", err)
	}
	if _, err := l.List(ctx, audit.Filter{}); err == nil {
		t.Error("List swallowed the storage error")
	}
}
