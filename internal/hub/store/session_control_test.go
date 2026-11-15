package store

import (
	"context"
	"errors"
	"testing"
)

func TestSessionControlStartsEmpty(t *testing.T) {
	s := openTest(t)
	rows, err := s.ListSessionControl(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("a fresh store lists %+v, err=%v; want no grants (default deny)", rows, err)
	}
	if _, err := s.SessionControl(context.Background(), "w1", "boss"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get on an empty store = %v, want ErrNotFound", err)
	}
}

func TestSessionControlCRUD(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := SessionControlRow{WorkerID: "w1", SessionName: "boss", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: "u1", CreatedAt: 10}
	b := SessionControlRow{WorkerID: "w1", SessionName: "alpha", TargetsJSON: `[{"kind":"worker","worker_id":"w2"}]`, CreatedBy: "u1", CreatedAt: 20}
	for _, r := range []SessionControlRow{a, b} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListSessionControl(ctx)
	if err != nil || len(rows) != 2 || rows[0] != b || rows[1] != a {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	// An upsert replaces targets, creator and time.
	a2 := a
	a2.TargetsJSON, a2.CreatedBy, a2.CreatedAt = `[{"kind":"session","worker_id":"w3","session_name":"build"}]`, "u2", 30
	if err := s.PutSessionControl(ctx, a2); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionControl(ctx, "w1", "boss"); err != nil || got != a2 {
		t.Errorf("get = %+v err=%v, want %+v", got, err, a2)
	}
	if ok, err := s.DeleteSessionControl(ctx, "w1", "boss"); err != nil || !ok {
		t.Errorf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.DeleteSessionControl(ctx, "w1", "boss"); ok {
		t.Error("a second delete reported a row")
	}
	if rows, _ := s.ListSessionControl(ctx); len(rows) != 1 || rows[0] != b {
		t.Errorf("rows after delete = %+v", rows)
	}
	if err := s.PutSessionControl(ctx, SessionControlRow{WorkerID: "w1", SessionName: "x"}); err == nil {
		t.Error("a row without targets was stored")
	}
}

func TestSessionControlOfAWorkerIsDeletedOnReassignment(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	other := f.worker(t, 2, f.u1, "other", WorkerKindInfrastructure)
	for _, r := range []SessionControlRow{
		{WorkerID: w.ID, SessionName: "boss", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: f.admin, CreatedAt: 1},
		{WorkerID: w.ID, SessionName: "two", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: f.admin, CreatedAt: 1},
		// Another controller that names w as a target stays (stale targets are not cleaned).
		{WorkerID: other.ID, SessionName: "boss", TargetsJSON: `[{"kind":"worker","worker_id":"` + w.ID + `"}]`, CreatedBy: f.admin, CreatedAt: 1},
	} {
		if err := f.s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u2, AtMS: 5000}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.ListSessionControl(ctx)
	if len(rows) != 1 || rows[0].WorkerID != other.ID {
		t.Errorf("rows after reassignment = %+v, want only the other controller", rows)
	}
}

func TestSessionControlSurvivesARefusedReassignment(t *testing.T) {
	f := newKindFixture(t)
	ctx := context.Background()
	w := f.worker(t, 1, f.u1, "pi", WorkerKindInfrastructure)
	if err := f.s.PutSessionControl(ctx, SessionControlRow{WorkerID: w.ID, SessionName: "boss", TargetsJSON: `[{"kind":"everything"}]`, CreatedBy: f.admin, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReassignWorker(ctx, ReassignParams{WorkerID: w.ID, NewOwnerID: f.u1, AtMS: 5000}); !errors.Is(err, ErrWorkerSameOwner) {
		t.Fatalf("err = %v", err)
	}
	if rows, _ := f.s.ListSessionControl(ctx); len(rows) != 1 {
		t.Errorf("a refused reassignment changed the grants: %+v", rows)
	}
}

func TestDeleteSessionControlOfWorker(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, r := range []SessionControlRow{
		{WorkerID: "w1", SessionName: "a", TargetsJSON: "[]", CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w1", SessionName: "b", TargetsJSON: "[]", CreatedBy: "u", CreatedAt: 1},
		{WorkerID: "w2", SessionName: "a", TargetsJSON: "[]", CreatedBy: "u", CreatedAt: 1},
	} {
		if err := s.PutSessionControl(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.DeleteSessionControlOfWorker(ctx, nil, "w1"); err != nil || n != 2 {
		t.Errorf("deleted %d, err=%v; want 2", n, err)
	}
	if rows, _ := s.ListSessionControl(ctx); len(rows) != 1 || rows[0].WorkerID != "w2" {
		t.Errorf("rows = %+v", rows)
	}
}
