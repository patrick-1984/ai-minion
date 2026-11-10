package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func newPairing(id string) PairingRow {
	return PairingRow{ID: id, Status: PairingPending, CreatedAt: 1000, ExpiresAt: 1000 + 600_000}
}

func TestPairingRoundTripAndReplace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.Pairing(ctx, "K7Q4"); !errors.Is(err, ErrPairingNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	if err := s.PutPairing(ctx, newPairing("K7Q4")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Pairing(ctx, "K7Q4")
	if err != nil || got != newPairing("K7Q4") {
		t.Fatalf("got %+v err=%v", got, err)
	}
	// A new code over an expired row replaces it, attempts included.
	if _, err := s.AddPairingFailure(ctx, "K7Q4", 5); err != nil {
		t.Fatal(err)
	}
	fresh := newPairing("K7Q4")
	fresh.CreatedAt, fresh.ExpiresAt = 9000, 9000+600_000
	if err := s.PutPairing(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Pairing(ctx, "K7Q4"); got != fresh {
		t.Fatalf("not replaced: %+v", got)
	}
}

func TestAddPairingFailureCountsAndDropsAtTheLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutPairing(ctx, newPairing("AAAA")); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		row, err := s.AddPairingFailure(ctx, "AAAA", 5)
		if err != nil || row.Attempts != i || row.Status != PairingPending {
			t.Fatalf("failure %d: %+v err=%v", i, row, err)
		}
	}
	row, err := s.AddPairingFailure(ctx, "AAAA", 5)
	if err != nil || row.Attempts != 5 || row.Status != PairingDropped {
		t.Fatalf("fifth failure: %+v err=%v", row, err)
	}
	if _, err := s.AddPairingFailure(ctx, "AAAA", 5); !errors.Is(err, ErrPairingNotPending) {
		t.Errorf("a dropped pairing was charged again: %v", err)
	}
	if _, err := s.AddPairingFailure(ctx, "ZZZZ", 5); !errors.Is(err, ErrPairingNotPending) {
		t.Errorf("an unknown pairing was charged: %v", err)
	}
}

func TestAddPairingFailureLosesNoCountUnderConcurrency(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutPairing(ctx, newPairing("BBBB")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AddPairingFailure(ctx, "BBBB", 100); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got, _ := s.Pairing(ctx, "BBBB"); got.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", got.Attempts)
	}
}

func TestCompletePairingCreatesTheWorkerOnceAndMarksTheCodeUsed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutPairing(ctx, newPairing("CCCC")); err != nil {
		t.Fatal(err)
	}
	w := worker(NewID(), 1, "u1", "pi")
	if err := s.CompletePairing(ctx, "CCCC", "digest-"+"CCCC", w); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Pairing(ctx, "CCCC")
	if row.Status != PairingUsed || row.OwnerID != "u1" || row.WorkerID != w.ID {
		t.Fatalf("row = %+v", row)
	}
	if got, err := s.WorkerByPub(ctx, w.Pub); err != nil || got.ID != w.ID {
		t.Fatalf("worker = %+v err=%v", got, err)
	}
	// A used code cannot create a second Worker.
	if err := s.CompletePairing(ctx, "CCCC", "digest-"+"CCCC", worker(NewID(), 2, "u1", "other")); !errors.Is(err, ErrPairingNotPending) {
		t.Fatalf("second completion: %v", err)
	}
	if _, err := s.WorkerByPub(ctx, pub(2)); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("the second Worker exists: %v", err)
	}
}

func TestCompletePairingRollsBackOnWorkerConflicts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.InsertWorker(ctx, nil, worker(NewID(), 1, "u1", "pi")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPairing(ctx, newPairing("DDDD")); err != nil {
		t.Fatal(err)
	}
	if err := s.CompletePairing(ctx, "DDDD", "digest-"+"DDDD", worker(NewID(), 2, "u1", "pi")); !errors.Is(err, ErrWorkerNameTaken) {
		t.Fatalf("name taken: %v", err)
	}
	if err := s.CompletePairing(ctx, "DDDD", "digest-"+"DDDD", worker(NewID(), 1, "u2", "x")); !errors.Is(err, ErrWorkerExists) {
		t.Fatalf("key exists: %v", err)
	}
	if row, _ := s.Pairing(ctx, "DDDD"); row.Status != PairingPending {
		t.Fatalf("the code was consumed by a failed completion: %+v", row)
	}
	if err := s.CompletePairing(ctx, "NONE", "digest-"+"NONE", worker(NewID(), 3, "u1", "n")); !errors.Is(err, ErrPairingNotPending) {
		t.Fatalf("unknown pairing: %v", err)
	}
}

func TestPurgePairingsDeletesOnlyExpiredRows(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old, live := newPairing("OLD1"), newPairing("LIV1")
	old.ExpiresAt, live.ExpiresAt = 5000, 50_000
	for _, p := range []PairingRow{old, live} {
		if err := s.PutPairing(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PurgePairings(ctx, 10_000)
	if err != nil || n != 1 {
		t.Fatalf("purged %d err=%v", n, err)
	}
	if _, err := s.Pairing(ctx, "OLD1"); !errors.Is(err, ErrPairingNotFound) {
		t.Errorf("expired row survived: %v", err)
	}
	if _, err := s.Pairing(ctx, "LIV1"); err != nil {
		t.Errorf("live row purged: %v", err)
	}
}
