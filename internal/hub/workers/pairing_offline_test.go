package workers

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// RC1 release gate, finding 2: a pair_code session is online while
// first_connected_at is still empty, so the never-connected condition alone
// does not keep a re-issue away from a live Worker. The re-issue is refused
// while the Worker has a live session, and allowed once it is offline.
func TestReissueIsRefusedWhileTheWorkerIsOnline(t *testing.T) {
	h := newPairHub(t, nil, nil)
	row := enrolUnconnected(t, h, newKey(t), "u-alice")
	sess := newFakeSession()
	h.svc.Registry().Add(Online{WorkerID: row.ID, OwnerID: "u-alice"}, sess)

	if err := reissue(h, row.ID, "u-alice", false); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("re-issue of an online Worker = %v", err)
	}
	if cur, _ := h.st.WorkerByID(context.Background(), row.ID); cur.RepairUntil != 0 {
		t.Fatal("the repair window was opened for an online Worker")
	}
	_ = sess.Close()
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(row.ID) })
	if err := reissue(h, row.ID, "u-alice", false); err != nil {
		t.Fatalf("re-issue of the offline Worker = %v", err)
	}
	if n := countAudit(t, h, AuditPairingReissue); n != 1 {
		t.Errorf("%d pairing_reissue events, want 1", n)
	}
}

// The repair completion is refused too while the Worker is online (a window
// opened while it was offline does not let a new code take over a live
// Worker); the window stays open and works once the Worker is offline.
func TestRepairCompletionIsRefusedWhileTheWorkerIsOnline(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	row := enrolUnconnected(t, h, key, "u-alice")
	if err := reissue(h, row.ID, "u-alice", false); err != nil {
		t.Fatal(err)
	}
	sess := newFakeSession()
	h.svc.Registry().Add(Online{WorkerID: row.ID, OwnerID: "u-alice"}, sess)

	code := identity.NewPairingCode()
	w := h.start(t, key, code, "stuck")
	if _, err := h.claim("u-alice", code.String(), ""); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("repair of an online Worker = %v", err)
	}
	w.wait(t)
	if cur, _ := h.st.WorkerByID(context.Background(), row.ID); cur.RepairUntil == 0 {
		t.Fatal("the refused repair closed the window")
	}

	_ = sess.Close()
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(row.ID) })
	code2 := identity.NewPairingCode()
	w2 := h.start(t, key, code2, "stuck")
	res, err := h.claim("u-alice", code2.String(), "")
	if err != nil || res.WorkerID != row.ID {
		t.Fatalf("repair of the offline Worker = %+v %v", res, err)
	}
	w2.wait(t)
}

// WhileOffline holds the add lock: a registration that starts while the
// guarded write runs waits for it, so a Worker cannot become online between
// the online check and the write.
func TestWhileOfflineKeepsARegistrationWaitingUntilTheWriteIsDone(t *testing.T) {
	r := NewRegistry()
	added := make(chan struct{})
	online, err := r.WhileOffline("w1", func() error {
		go func() {
			r.Add(Online{WorkerID: "w1"}, newFakeSession())
			close(added)
		}()
		select {
		case <-added:
			t.Error("a Worker registered while the guarded write ran")
		case <-time.After(50 * time.Millisecond):
		}
		if r.IsOnline("w1") {
			t.Error("the Worker is online inside the guarded write")
		}
		return nil
	})
	if online || err != nil {
		t.Fatalf("WhileOffline = %v, %v", online, err)
	}
	<-added
	ran := false
	online, err = r.WhileOffline("w1", func() error { ran = true; return nil })
	if !online || err != nil || ran {
		t.Fatalf("WhileOffline of an online Worker = %v, %v (ran %v)", online, err, ran)
	}
}
