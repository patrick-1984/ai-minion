package store

import (
	"context"
	"testing"
)

func TestListWorkersOrdersByNameThenID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if got, err := s.ListWorkers(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %v %v", got, err)
	}
	for _, w := range []WorkerRow{
		worker("w3", 1, "u1", "zeta"),
		worker("w2", 2, "u2", "alpha"),
		worker("w1", 3, "u1", "alpha"),
	} {
		if err := s.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, w := range got {
		ids = append(ids, w.ID)
	}
	if len(ids) != 3 || ids[0] != "w1" || ids[1] != "w2" || ids[2] != "w3" {
		t.Fatalf("order = %v", ids)
	}
	if got[0].OwnerID != "u1" || got[0].Kind != WorkerKindPrivate || got[0].CreatedAt != 1000 || len(got[0].Pub) != 32 {
		t.Errorf("row = %+v", got[0])
	}
}

func TestListWorkersOnAClosedStoreIsAnError(t *testing.T) {
	s := openTest(t)
	_ = s.Close()
	if _, err := s.ListWorkers(context.Background()); err == nil {
		t.Error("a closed store must be an error")
	}
}
