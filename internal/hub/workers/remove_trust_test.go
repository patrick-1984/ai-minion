package workers

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// trustRemover is a Remover whose trust step is the one the Hub composes
// (store.DeleteTrustOfWorker) over the real trust table; only the key step is
// left out.
func trustRemover(t *testing.T, st *store.Store, s RemoveStore) *Remover {
	t.Helper()
	rm, err := NewRemover(RemoveConfig{
		Store: s, Auditor: audit.New(st),
		Trust: CleanupFunc(func(ctx context.Context, tx *sql.Tx, workerID string) error {
			_, err := st.DeleteTrustOfWorker(ctx, tx, workerID)
			return err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rm
}

func seedTrust(t *testing.T, st *store.Store, workerID string) {
	t.Helper()
	for _, r := range []store.TrustRow{
		{ReceiverID: "u2", WorkerID: workerID, CreatedAt: 1},
		{ReceiverID: "u2", WorkerID: workerID, SessionName: "minion-1", CreatedAt: 1},
		{ReceiverID: "admin", WorkerID: workerID, CreatedAt: 1},
		{ReceiverID: "u2", WorkerID: "other-worker", CreatedAt: 1},
	} {
		if err := st.PutTrust(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}

func trustRows(t *testing.T, st *store.Store, workerID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM trust WHERE worker_id = ?`, workerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRemoveDeletesEveryTrustRowOfTheWorkerForEveryReceiver(t *testing.T) {
	r := newRemoveRig(t)
	_, w := r.enrol("owner-1", "pi")
	seedTrust(t, r.st, w.ID)
	if _, err := trustRemover(t, r.st, r.st).Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID); err != nil {
		t.Fatal(err)
	}
	if n := trustRows(t, r.st, w.ID); n != 0 {
		t.Errorf("%d trust rows of the removed Worker remain (Worker and session rows, every receiver)", n)
	}
	if n := trustRows(t, r.st, "other-worker"); n != 1 {
		t.Errorf("rows of another Worker = %d, want 1", n)
	}
}

func TestAFailedRemovalLeavesTheTrustRowsInPlace(t *testing.T) {
	r := newRemoveRig(t)
	_, w := r.enrol("owner-1", "pi")
	seedTrust(t, r.st, w.ID)
	boom := errors.New("boom")
	// The trust step runs first and succeeds inside the transaction; the
	// tombstone then fails, and the deletion must roll back with it.
	rm := trustRemover(t, r.st, failingTombstone{r.st, boom})
	if _, err := rm.Remove(context.Background(), RemoveActor{ID: "owner-1"}, w.ID); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the failure", err)
	}
	if n := trustRows(t, r.st, w.ID); n != 3 {
		t.Errorf("trust rows = %d, want the 3 of the Worker untouched", n)
	}
	if _, err := r.st.WorkerByID(context.Background(), w.ID); err != nil {
		t.Errorf("the Worker is gone after a failed removal: %v", err)
	}
}
