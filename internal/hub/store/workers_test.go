package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func pub(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func worker(id string, key byte, owner, name string) WorkerRow {
	return WorkerRow{ID: id, Pub: pub(key), Name: name, OwnerID: owner, Kind: WorkerKindPrivate, CreatedAt: 1000}
}

func TestHubIdentityIsRecordedOnceAndNeverReplaced(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.HubIdentity(ctx); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	row := HubIdentityRow{Pub: pub(1), Fingerprint: "fp", CreatedAt: 42}
	if err := s.PutHubIdentity(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.HubIdentity(ctx)
	if err != nil || !ok || !bytes.Equal(got.Pub, row.Pub) || got.Fingerprint != "fp" || got.CreatedAt != 42 {
		t.Fatalf("got %+v ok=%v err=%v", got, ok, err)
	}
	if err := s.PutHubIdentity(ctx, HubIdentityRow{Pub: pub(2), Fingerprint: "other", CreatedAt: 43}); err == nil {
		t.Fatal("the recorded Hub identity was replaced")
	}
	if got, _, _ := s.HubIdentity(ctx); !bytes.Equal(got.Pub, pub(1)) {
		t.Fatal("the recorded key changed")
	}
}

func TestInsertAndReadWorkers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	w := worker("w1", 1, "u1", "pi")
	if err := s.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func() (WorkerRow, error){
		"by pub": func() (WorkerRow, error) { return s.WorkerByPub(ctx, w.Pub) },
		"by id":  func() (WorkerRow, error) { return s.WorkerByID(ctx, "w1") },
	} {
		got, err := get()
		if err != nil || got.ID != "w1" || got.Name != "pi" || got.OwnerID != "u1" || got.Kind != WorkerKindPrivate || got.CreatedAt != 1000 {
			t.Errorf("%s: got %+v err=%v", name, got, err)
		}
	}
	if owner, err := s.WorkerOwner(ctx, "w1"); err != nil || owner != "u1" {
		t.Errorf("owner = %q err=%v", owner, err)
	}
	if _, err := s.WorkerByPub(ctx, pub(9)); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown pub: %v", err)
	}
	if _, err := s.WorkerOwner(ctx, "nope"); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestInsertWorkerConstraints(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.InsertWorker(ctx, nil, worker("w1", 1, "u1", "pi")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertWorker(ctx, nil, worker("w2", 1, "u2", "other")); !errors.Is(err, ErrWorkerExists) {
		t.Errorf("same key: %v", err)
	}
	if err := s.InsertWorker(ctx, nil, worker("w3", 3, "u1", "pi")); !errors.Is(err, ErrWorkerNameTaken) {
		t.Errorf("same owner and name: %v", err)
	}
	// The same name under another owner is fine (names are unique per owner).
	if err := s.InsertWorker(ctx, nil, worker("w4", 4, "u2", "pi")); err != nil {
		t.Errorf("same name, other owner: %v", err)
	}
	bad := worker("w5", 5, "u1", "x")
	bad.Kind = "bogus"
	if err := s.InsertWorker(ctx, nil, bad); err == nil {
		t.Error("an unknown kind was accepted")
	}
	short := worker("w6", 6, "u1", "y")
	short.Pub = []byte{1, 2, 3}
	if err := s.InsertWorker(ctx, nil, short); err == nil {
		t.Error("a short public key was accepted")
	}
}

func TestTombstoneWorkerKeepsTheKeyAndRemovesTheRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	w := worker("w1", 1, "u1", "pi")
	w.Kind = WorkerKindInfrastructure
	if err := s.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TombstoneByPub(ctx, w.Pub); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("before removal: %v", err)
	}
	if err := s.TombstoneWorker(ctx, "w1", "admin1", 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkerByPub(ctx, w.Pub); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("row still there: %v", err)
	}
	if _, err := s.WorkerOwner(ctx, "w1"); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("owner of a removed Worker: %v", err)
	}
	tb, err := s.TombstoneByPub(ctx, w.Pub)
	if err != nil {
		t.Fatal(err)
	}
	if tb.WorkerID != "w1" || tb.Name != "pi" || tb.OwnerID != "u1" || tb.Kind != WorkerKindInfrastructure ||
		tb.CreatedAt != 1000 || tb.RemovedAt != 2000 || tb.RemovedBy != "admin1" {
		t.Errorf("tombstone = %+v", tb)
	}
	if err := s.TombstoneWorker(ctx, "w1", "admin1", 3000); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("second removal: %v", err)
	}
}

func TestMigrationFourCreatesTheWorkersTables(t *testing.T) {
	s := openTest(t)
	for _, table := range []string{"hub_identity", "workers", "worker_tombstones"} {
		var n int
		if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
}
