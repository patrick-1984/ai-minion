package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// markRepo wraps the repository of the handler so that a test can inject the
// persistence of the first connect.
type markRepo struct {
	Repository
	mark func(ctx context.Context, workerID string, at int64) (bool, error)
}

func (m markRepo) MarkFirstConnected(ctx context.Context, workerID string, at int64) (bool, error) {
	return m.mark(ctx, workerID, at)
}

// The first connect is persisted as part of the admission: when the write
// fails the connection is closed and the Worker is never published.
func TestAFailedFirstConnectRecordClosesTheConnectionAndNeverPublishesTheWorker(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "o", "pi")
	h.svc.handler.cfg.Repository = markRepo{Repository: h.svc.handler.cfg.Repository,
		mark: func(context.Context, string, int64) (bool, error) { return false, errors.New("disk full") }}
	_, s := connectWelcomed(t, h, key)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open without a recorded first connect")
	}
	if h.svc.Registry().IsOnline(w.ID) {
		t.Error("the Worker was published although the first connect was not recorded")
	}
	cur, _ := h.st.WorkerByID(context.Background(), w.ID)
	if cur.FirstConnectedAt != 0 {
		t.Errorf("first_connected_at = %d", cur.FirstConnectedAt)
	}
}

// The Worker is not online until the first connect is persisted, and a repair
// refuses from the moment the Worker is online.
func TestTheWorkerIsPublishedOnlyAfterTheFirstConnectIsPersisted(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "o", "pi")
	st := h.st
	entered, release := make(chan struct{}), make(chan struct{})
	h.svc.handler.cfg.Repository = markRepo{Repository: h.svc.handler.cfg.Repository,
		mark: func(ctx context.Context, id string, at int64) (bool, error) {
			close(entered)
			<-release
			return st.MarkFirstConnected(ctx, id, at)
		}}
	connectWelcomed(t, h, key)
	<-entered
	if h.svc.Registry().IsOnline(w.ID) {
		t.Fatal("the Worker was online before its first connect was persisted")
	}
	close(release)
	eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
	cur, _ := st.WorkerByID(context.Background(), w.ID)
	if cur.FirstConnectedAt == 0 {
		t.Fatal("online without a recorded first connect")
	}
	if err := st.SetRepairWindow(context.Background(), w.ID, 1<<40); !errors.Is(err, store.ErrRepairNotAllowed) {
		t.Errorf("a repair window on a connected Worker: %v", err)
	}
}

// A connect racing the completion of an owner's repair: whatever the order,
// the Worker is never online without its first connect recorded, and a repair
// that finds the first connect recorded is refused.
func TestAConnectRacingARepairCompletionHasOneWinner(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		h := newTestHub(t, nil)
		key := newKey(t)
		w := h.enrol(key, "o", "pi")
		if err := h.st.SetRepairWindow(ctx, w.ID, time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.PutPairing(ctx, store.PairingRow{ID: "AB" + string(rune('A'+i/10)) + string(rune('A'+i%10)), Status: store.PairingPending, CreatedAt: 1, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
		row, _ := h.st.WorkerByID(ctx, w.ID)
		var wg sync.WaitGroup
		var repairErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			repairErr = h.st.RepairPairing(ctx, "AB"+string(rune('A'+i/10))+string(rune('A'+i%10)), "digest", row, time.Now().UnixMilli())
		}()
		go func() { defer wg.Done(); connectWelcomed(t, h, key) }()
		wg.Wait()
		eventually(t, "the Worker to be online", func() bool { return h.svc.Registry().IsOnline(w.ID) })
		cur, _ := h.st.WorkerByID(ctx, w.ID)
		if cur.FirstConnectedAt == 0 {
			t.Fatalf("round %d: online without a recorded first connect", i)
		}
		if repairErr != nil && !errors.Is(repairErr, store.ErrRepairNotAllowed) {
			t.Fatalf("round %d: repair: %v", i, repairErr)
		}
		// Once connected, a repair never succeeds again.
		if err := h.st.RepairPairing(ctx, "ZZZZ", "d2", cur, time.Now().UnixMilli()); !errors.Is(err, store.ErrRepairNotAllowed) {
			t.Fatalf("round %d: repair after the connect = %v", i, err)
		}
	}
}
