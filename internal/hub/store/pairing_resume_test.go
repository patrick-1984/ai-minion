package store

import (
	"context"
	"testing"
)

// The forward migration 0023 on a 0.6.3-shaped database: existing Workers are
// backfilled with first_connected_at = created_at (so none is eligible for an
// owner re-issue), the repair window is closed, and old used-code rows keep
// working but never resume.
func TestMigration0023OnAV063Database(t *testing.T) {
	dir := t.TempDir()
	old := openBefore(t, dir, "0023")
	ctx := context.Background()
	if _, err := old.db.Exec(`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('w1', ?, 'n', 'o', 'private', 777)`, pub(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO used_pairing_codes (code_digest, used_at) VALUES ('d1', 5)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := openCached(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	w, err := s.WorkerByID(ctx, "w1")
	if err != nil || w.FirstConnectedAt != 777 || w.RepairUntil != 0 {
		t.Fatalf("worker = %+v %v", w, err)
	}
	if err := s.SetRepairWindow(ctx, "w1", 1<<40); err != ErrRepairNotAllowed {
		t.Errorf("a backfilled Worker is eligible: %v", err)
	}
	if used, err := s.PairingCodeUsed(ctx, "d1"); err != nil || !used {
		t.Errorf("the old used code was lost: %v %v", used, err)
	}
	if _, err := s.PairingCommitFor(ctx, "ABCD", pub(1), 0); err != ErrNoPairingCommit {
		t.Errorf("an old row resumes: %v", err)
	}
	// A new Worker has no first connect until a handshake registers.
	if err := s.InsertWorker(ctx, nil, WorkerRow{ID: "w2", Pub: pub(2), Name: "m", OwnerID: "o", Kind: WorkerKindPrivate, CreatedAt: 9}); err != nil {
		t.Fatal(err)
	}
	if w2, _ := s.WorkerByID(ctx, "w2"); w2.FirstConnectedAt != 0 {
		t.Errorf("new worker first_connected_at = %d", w2.FirstConnectedAt)
	}
}
