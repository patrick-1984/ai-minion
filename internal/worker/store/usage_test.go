package store

import (
	"context"
	"errors"
	"testing"
)

func TestUsageGrantBoundToEnrollment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	grant := func(e Enrollment) {
		t.Helper()
		if err := s.AcceptUsageRegistration(ctx, e.HubPub, e.WorkerID, e.Owner); err != nil {
			t.Fatal(err)
		}
	}
	check := func(id string, want bool) {
		t.Helper()
		ok, err := s.UsageRegistration(ctx, id)
		if err != nil || ok != want {
			t.Fatalf("usage registration = %t, %v; want %t", ok, err, want)
		}
	}
	e := Enrollment{HubPub: pubKey(1), WorkerID: "worker-1", Owner: "alice"}
	if err := s.SaveEnrollment(ctx, e); err != nil {
		t.Fatal(err)
	}
	check(e.WorkerID, false)
	grant(e)
	check(e.WorkerID, true)
	if err := s.SaveEnrollment(ctx, e); err != nil {
		t.Fatal(err)
	}
	check(e.WorkerID, true)
	old := e
	e.Owner = "bob"
	if err := s.SaveEnrollment(ctx, e); err != nil {
		t.Fatal(err)
	}
	check(e.WorkerID, false)
	if err := s.AcceptUsageRegistration(ctx, old.HubPub, old.WorkerID, old.Owner); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("old approval: %v", err)
	}
	grant(e)
	e.HubPub = pubKey(2)
	if err := s.SaveEnrollment(ctx, e); err != nil {
		t.Fatal(err)
	}
	check(e.WorkerID, false)
}
