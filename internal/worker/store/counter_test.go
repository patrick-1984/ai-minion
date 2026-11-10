package store

import (
	"sort"
	"sync"
	"testing"
)

func TestNextSessionNumberStartsAtOneAndIncreases(t *testing.T) {
	s := openStore(t, t.TempDir())
	for want := int64(1); want <= 5; want++ {
		got, err := s.NextSessionNumber()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("NextSessionNumber = %d, want %d", got, want)
		}
	}
}

func TestNextSessionNumberConcurrent(t *testing.T) {
	s := openStore(t, t.TempDir())
	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  []int64
		errs []error
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.NextSessionNumber()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got = append(got, v)
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != n {
		t.Fatalf("got %d numbers, want %d", len(got), n)
	}
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("numbers = %v, want 1..%d each once", got, n)
		}
	}
}

func TestNextSessionNumberPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.NextSessionNumber(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	got, err := s.NextSessionNumber()
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Fatalf("after reopen NextSessionNumber = %d, want 4", got)
	}
}

func TestNextSessionNumberNeverReusedAfterSessionsGone(t *testing.T) {
	// The counter knows nothing about live sessions: handing out a number is
	// all that matters, so a number whose session was killed is not offered
	// again. Simulate by taking numbers and "forgetting" them.
	s := openStore(t, t.TempDir())
	first, _ := s.NextSessionNumber()
	second, _ := s.NextSessionNumber()
	// Both sessions are now gone (nothing to delete: the counter has no list).
	third, err := s.NextSessionNumber()
	if err != nil {
		t.Fatal(err)
	}
	if third <= second || second <= first {
		t.Fatalf("numbers %d, %d, %d are not strictly increasing", first, second, third)
	}
}

func TestCounterSurvivesAMigrationUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	// Reopen with the embedded set plus a later migration.
	files := map[string]string{
		"0099_extra.sql": "CREATE TABLE extra (id INTEGER);",
	}
	entries, err := workerMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := workerMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	s = openStore(t, dir, testMigrations(files))
	got, err := s.NextSessionNumber()
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("NextSessionNumber = %d, want 2", got)
	}
	if len(backups(t, dir)) != 1 {
		t.Fatalf("expected one pre-migration backup, got %v", backups(t, dir))
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName(7); got != "minion-7" {
		t.Fatalf("SessionName(7) = %q", got)
	}
}
