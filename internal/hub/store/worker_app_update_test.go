package store

import (
	"context"
	"errors"
	"testing"
)

// The Hub's update policy of a Worker: manual by default, auto and off
// stored with the administrator, manual removes the row, an unknown Worker
// and an unknown policy are refused.
func TestWorkerAppUpdate(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "tv", WorkerKindPrivate)
	other := f.worker(t, 2, f.u1, "phone", WorkerKindPrivate)
	if p, err := f.s.WorkerAppUpdatePolicy(ctx, w.ID); err != nil || p != "" {
		t.Fatalf("default = %q, %v", p, err)
	}
	if err := f.s.SetWorkerAppUpdate(ctx, w.ID, WorkerAppUpdateAuto, f.admin); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetWorkerAppUpdate(ctx, other.ID, WorkerAppUpdateOff, f.admin); err != nil {
		t.Fatal(err)
	}
	all, err := f.s.ListWorkerAppUpdates(ctx)
	if err != nil || len(all) != 2 || all[w.ID].Policy != "auto" || all[w.ID].UpdatedBy != f.admin || all[other.ID].Policy != "off" {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if p, _ := f.s.WorkerAppUpdatePolicy(ctx, other.ID); p != "off" {
		t.Fatalf("off = %q", p)
	}
	if err := f.s.SetWorkerAppUpdate(ctx, other.ID, "manual", f.admin); err != nil {
		t.Fatal(err)
	}
	if pols, _ := f.s.WorkerAppUpdatePolicies(ctx); len(pols) != 1 || pols[w.ID] != "auto" {
		t.Fatalf("after manual = %v", pols)
	}
	if err := f.s.SetWorkerAppUpdate(ctx, NewID(), "auto", f.admin); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("unknown Worker: %v", err)
	}
	if err := f.s.SetWorkerAppUpdate(ctx, w.ID, "sometimes", f.admin); err == nil {
		t.Fatal("unknown policy accepted")
	}
}
