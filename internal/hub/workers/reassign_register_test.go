package workers

import (
	"sync/atomic"
	"testing"
)

// A reassignment that commits after the connect read the Worker's row and
// before the Worker is registered finds no session to disconnect. The
// registration recheck compares the current owner and ends the connection, so
// the reconnect is welcomed with the new owner.
func TestReassignmentBetweenTheRowReadAndRegistrationEndsTheConnection(t *testing.T) {
	h := newTestHub(t, nil)
	key := newKey(t)
	w := h.enrol(key, "owner-1", "pi")
	var once atomic.Bool
	rechecked := make(chan struct{}, 4)
	h.svc.Handler().afterRecheck = func() { rechecked <- struct{}{} }
	h.svc.handler.beforeRegister = func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		if _, err := h.st.DB().Exec(`UPDATE workers SET owner_id = 'owner-2' WHERE id = ?`, w.ID); err != nil {
			t.Error(err)
		}
	}
	_, sess := connectOnline(t, h, key)
	select {
	case <-sess.Done():
	case <-timeout(t):
		t.Fatal("the connection welcomed for the previous owner was kept")
	}
	eventually(t, "the Worker to go offline", func() bool { return !h.svc.Registry().IsOnline(w.ID) })

	// The reconnect is welcomed for the new owner and stays.
	<-rechecked
	welcome, sess := connectOnline(t, h, key)
	if welcome.OwnerID != "owner-2" {
		t.Fatalf("welcome owner = %q", welcome.OwnerID)
	}
	<-rechecked
	select {
	case <-sess.Done():
		t.Fatal("the correctly welcomed connection was closed")
	default:
	}
}
