package store

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"
	"time"
)

func newThingsStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), testMigrations(v1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func countThings(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM things`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTxCommits(t *testing.T) {
	s := newThingsStore(t)
	err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		for _, id := range []string{"a", "b"} {
			if _, err := tx.Exec(`INSERT INTO things (id) VALUES (?)`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countThings(t, s); n != 2 {
		t.Fatalf("rows = %d", n)
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	s := newThingsStore(t)
	sentinel := errors.New("boom")
	err := Tx(context.Background(), s.DB(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO things (id) VALUES ('a')`); err != nil {
			return err
		}
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("err = %v, want the callback error unchanged", err)
	}
	if n := countThings(t, s); n != 0 {
		t.Fatalf("rows = %d after rollback", n)
	}
}

func TestTxRollsBackOnPanicAndRepanics(t *testing.T) {
	s := newThingsStore(t)
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("recovered %v, want the original panic", r)
			}
		}()
		_ = s.Tx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO things (id) VALUES ('a')`); err != nil {
				return err
			}
			panic("kaboom")
		})
		t.Fatal("panic was swallowed")
	}()
	if n := countThings(t, s); n != 0 {
		t.Fatalf("rows = %d after panic", n)
	}
	// The connection is usable again: no leaked transaction holds the lock.
	if err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO things (id) VALUES ('b')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTxRollsBackOnConstraintError(t *testing.T) {
	s := newThingsStore(t)
	err := s.Tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO things (id) VALUES ('a')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO things (id) VALUES ('a')`) // primary key clash
		return err
	})
	if err == nil {
		t.Fatal("want constraint error")
	}
	if n := countThings(t, s); n != 0 {
		t.Fatalf("rows = %d, the first insert must roll back with the second", n)
	}
}

func TestTxBeginFailsOnCancelledContext(t *testing.T) {
	s := newThingsStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := s.Tx(ctx, func(*sql.Tx) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("err = %v, called = %v", err, called)
	}
}

func TestTxReleasesWriteLockOnGoexit(t *testing.T) {
	s := newThingsStore(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Tx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO things (id) VALUES ('aborted')`); err != nil {
				return err
			}
			runtime.Goexit() // what t.Fatal does inside a callback
			return nil
		})
	}()
	<-done

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO things (id) VALUES ('after')`)
		return err
	})
	if err != nil {
		t.Fatalf("write after an aborted Tx: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM things WHERE id = 'aborted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the row of the aborted transaction must be rolled back")
	}
	if got := countThings(t, s); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}
