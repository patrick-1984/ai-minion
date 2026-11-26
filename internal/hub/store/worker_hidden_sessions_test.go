package store

import (
	"context"
	"errors"
	"testing"
)

// A Worker's own hidden-sessions choice: none by default, set, cleared,
// refused for an unknown Worker, dropped on reassignment and on removal.
func TestWorkerHiddenSessions(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	other := f.worker(t, 2, f.u1, "box", WorkerKindPrivate)
	if v, err := f.s.WorkerHiddenSessions(ctx, w.ID); err != nil || v != nil {
		t.Fatalf("default = %v, %v", v, err)
	}
	yes, no := true, false
	if err := f.s.SetWorkerHiddenSessions(ctx, w.ID, &yes, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetWorkerHiddenSessions(ctx, other.ID, &no, f.u1); err != nil {
		t.Fatal(err)
	}
	if v, err := f.s.WorkerHiddenSessions(ctx, w.ID); err != nil || v == nil || !*v {
		t.Fatalf("after allow = %v, %v", v, err)
	}
	all, err := f.s.ListWorkerHiddenSessions(ctx)
	if err != nil || len(all) != 2 || !all[w.ID] || all[other.ID] {
		t.Fatalf("list = %v, %v", all, err)
	}
	if err := f.s.SetWorkerHiddenSessions(ctx, other.ID, nil, f.u1); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.s.WorkerHiddenSessions(ctx, other.ID); v != nil {
		t.Fatalf("after default = %v", *v)
	}
	if err := f.s.SetWorkerHiddenSessions(ctx, NewID(), &yes, f.admin); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("unknown Worker: %v", err)
	}
	// A reassignment drops the old owner's choice.
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 1}); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.s.WorkerHiddenSessions(ctx, w.ID); v != nil {
		t.Fatalf("after reassignment = %v", *v)
	}
	// The removal takes the row with the Worker.
	if err := f.s.SetWorkerHiddenSessions(ctx, other.ID, &yes, f.u1); err != nil {
		t.Fatal(err)
	}
	if err := f.s.TombstoneWorker(ctx, other.ID, f.admin, 2); err != nil {
		t.Fatal(err)
	}
	if all, _ := f.s.ListWorkerHiddenSessions(ctx); len(all) != 0 {
		t.Fatalf("after removal = %v", all)
	}
}
