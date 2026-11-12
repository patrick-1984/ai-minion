package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The restrictive share_set calls that were dispatched and are not settled are
// kept durably, read back in order, removed by settlement (one, or every one up
// to a sequence number) and survive a reopen.
func TestPendingShareRestrictionsAreDurable(t *testing.T) {
	dir := t.TempDir()
	s, err := openCached(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := PendingShare{Seq: 100, WorkerID: "w1", Rule: ops.ShareRule{Mode: "select", Names: []string{"a"}}, BaseRevision: 3}
	b := PendingShare{Seq: 200, WorkerID: "w1", Rule: ops.ShareRule{Mode: "none"}, BaseRevision: 4}
	c := PendingShare{Seq: 150, WorkerID: "w2", Rule: ops.ShareRule{Mode: "none"}}
	for _, p := range []PendingShare{b, a, c} {
		if err := s.SavePendingShare(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SavePendingShare(ctx, PendingShare{WorkerID: "w1"}); err == nil {
		t.Fatal("a pending restriction without a sequence number was stored")
	}
	if err := s.SavePendingShare(ctx, PendingShare{Seq: 5, WorkerID: "w1", Rule: ops.ShareRule{Mode: "sometimes"}}); err == nil {
		t.Fatal("an invalid mode was stored")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openCached(t, dir) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.LoadPendingShares(ctx)
	if err != nil || !reflect.DeepEqual(got, []PendingShare{a, c, b}) {
		t.Fatalf("after a restart: %+v %v", got, err)
	}
	if err := s.DeletePendingShare(ctx, "w1", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePendingShares(ctx, "w1", 250); err != nil { // everything of w1 up to 250
		t.Fatal(err)
	}
	if got, _ := s.LoadPendingShares(ctx); !reflect.DeepEqual(got, []PendingShare{c}) {
		t.Fatalf("after the settlements: %+v", got)
	}
}

// The migration that adds the table runs on a database made by the one before it.
func TestPendingShareMigrationUpgradesAnExistingDatabase(t *testing.T) {
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0018" {
			continue
		}
		b, err := hubMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	dir := t.TempDir()
	old, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := old.SaveWorkerShare(ctx, "w", ops.ShareDecl{Rule: ops.ShareRule{Mode: "all"}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	cur, err := openCached(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	if m, err := cur.LoadWorkerShares(ctx); err != nil || m["w"].Revision != 1 {
		t.Fatalf("the known rule did not survive the upgrade: %+v %v", m, err)
	}
	if p, err := cur.LoadPendingShares(ctx); err != nil || len(p) != 0 {
		t.Fatalf("an upgraded database has pending restrictions: %+v %v", p, err)
	}
	if err := cur.SavePendingShare(ctx, PendingShare{Seq: 1, WorkerID: "w", Rule: ops.ShareRule{Mode: "none"}}); err != nil {
		t.Fatal(err)
	}
}
