package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openCodesStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addCodesWorker(t *testing.T, s *Store, id string, created int64) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES (?, randomblob(32), ?, 'usr', 'private', ?)`, id, "n-"+id, created); err != nil {
		t.Fatal(err)
	}
}

func codesOf(t *testing.T, s *Store, worker string, now time.Time, seen ...SessionSighting) (int64, []int64) {
	t.Helper()
	w, c, err := s.SessionCodes(context.Background(), worker, seen, now)
	if err != nil {
		t.Fatal(err)
	}
	return w, c
}

func TestWorkerNumbersSequentialAndNeverReused(t *testing.T) {
	s := openCodesStore(t)
	addCodesWorker(t, s, "w1", 1)
	addCodesWorker(t, s, "w2", 2)
	if _, err := s.DB().Exec(`DELETE FROM workers WHERE id = 'w2'`); err != nil {
		t.Fatal(err)
	}
	addCodesWorker(t, s, "w3", 3)
	got, err := s.WorkerNumbers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["w1"] != 1 || got["w3"] != 3 {
		t.Fatalf("numbers = %v, want w1=1 w3=3 (2 is never given again)", got)
	}
}

func TestSessionNumbersNeverReusedAfterPrune(t *testing.T) {
	s := openCodesStore(t)
	addCodesWorker(t, s, "w1", 1)
	t0 := time.Unix(1_000_000, 0)
	w, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "a"}, SessionSighting{Name: "b"})
	if w != 1 || c[0] != 1 || c[1] != 2 {
		t.Fatalf("got %d %v", w, c)
	}
	// Seen again: the same numbers, nothing new.
	if _, c := codesOf(t, s, "w1", t0.Add(time.Minute), SessionSighting{Name: "b"}, SessionSighting{Name: "a"}); c[0] != 2 || c[1] != 1 {
		t.Fatalf("stable: %v", c)
	}
	// 31 days later both are pruned by the next write; a new session takes 3.
	late := t0.Add(SessionCodeRetention + 24*time.Hour)
	if _, c := codesOf(t, s, "w1", late, SessionSighting{Name: "c"}); c[0] != 3 {
		t.Fatalf("new after prune: %v", c)
	}
	if _, _, ok, _ := s.ResolveSessionCode(context.Background(), 1, 1); ok {
		t.Fatal("a pruned code still resolves")
	}
	if _, c := codesOf(t, s, "w1", late, SessionSighting{Name: "a"}); c[0] != 4 {
		t.Fatalf("a pruned session number was given again: %v", c)
	}
}

func TestSessionRenameKeepsNumberWithInstanceID(t *testing.T) {
	s := openCodesStore(t)
	addCodesWorker(t, s, "w1", 1)
	t0 := time.Unix(1_000_000, 0)
	codesOf(t, s, "w1", t0, SessionSighting{Name: "x"}, SessionSighting{Name: "a", InstanceID: "t1-a"})
	// The event stream saw the new name first (no instance id): a number is given.
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "x"}, SessionSighting{Name: "renamed"}); c[1] != 3 {
		t.Fatalf("name-only sighting: %v", c)
	}
	// The listing with instance ids says it is session 2 renamed.
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "renamed", InstanceID: "t1-a"}); c[0] != 2 {
		t.Fatalf("rename: %v", c)
	}
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "renamed"}); c[0] != 2 {
		t.Fatalf("by name after rename: %v", c)
	}
	id, name, ok, err := s.ResolveSessionCode(context.Background(), 1, 2)
	if err != nil || !ok || id != "w1" || name != "renamed" {
		t.Fatalf("resolve = %q %q %v %v", id, name, ok, err)
	}
	if _, _, ok, _ := s.ResolveSessionCode(context.Background(), 1, 3); ok {
		t.Fatal("the transient number still resolves")
	}
}

func TestSessionNameFallbackAndRecreate(t *testing.T) {
	s := openCodesStore(t)
	addCodesWorker(t, s, "w1", 1)
	t0 := time.Unix(1_000_000, 0)
	// Without instance ids a session created again under its name keeps its number.
	codesOf(t, s, "w1", t0, SessionSighting{Name: "a"})
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "a"}); c[0] != 1 {
		t.Fatalf("fallback: %v", c)
	}
	// The first instance id adopts the row of the name.
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "a", InstanceID: "t1-1"}); c[0] != 1 {
		t.Fatalf("adopt: %v", c)
	}
	// Another instance under the same name is another session.
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "a", InstanceID: "t1-2"}); c[0] != 2 {
		t.Fatalf("recreate: %v", c)
	}
	if _, _, ok, _ := s.ResolveSessionCode(context.Background(), 1, 1); ok {
		t.Fatal("the old code reaches the new session")
	}
	if _, c := codesOf(t, s, "w1", t0, SessionSighting{Name: "a"}); c[0] != 2 {
		t.Fatalf("by name after recreate: %v", c)
	}
}

func TestSessionCodesReadOnlyWhenNothingChanges(t *testing.T) {
	s := openCodesStore(t)
	addCodesWorker(t, s, "w1", 1)
	t0 := time.Unix(1_000_000, 0)
	codesOf(t, s, "w1", t0, SessionSighting{Name: "a"})
	var before int64
	_ = s.DB().QueryRow(`SELECT last_seen FROM session_codes`).Scan(&before)
	codesOf(t, s, "w1", t0.Add(time.Minute), SessionSighting{Name: "a"})
	var after int64
	_ = s.DB().QueryRow(`SELECT last_seen FROM session_codes`).Scan(&after)
	if after != before {
		t.Fatal("a sighting inside the touch interval wrote")
	}
	codesOf(t, s, "w1", t0.Add(2*SessionCodeTouchEvery), SessionSighting{Name: "a"})
	_ = s.DB().QueryRow(`SELECT last_seen FROM session_codes`).Scan(&after)
	if after == before {
		t.Fatal("a stale mapping was not touched")
	}
}
