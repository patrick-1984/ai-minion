package workers

import (
	"errors"
	"testing"
	"time"
)

// PublishPipeChanged is a wake-up with no content: it names the Worker and the session, follows the current share state at the
// publish, and never blocks on a subscriber.
func TestPublishPipeChangedIsGatedAndNeverBlocks(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(4)
	defer sub.Close()

	h.PublishPipeChanged("w1", "s1")
	select {
	case ev := <-sub.C():
		if ev.Type != EventPipeChanged || ev.WorkerID != "w1" || ev.Session != "s1" || ev.Worker != "" || ev.Task != nil || ev.State != "" || ev.ScreenHash != "" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was published")
	}
	// Without a Worker or a session there is nothing to name.
	h.PublishPipeChanged("", "s1")
	h.PublishPipeChanged("w1", "")
	// A session that was taken away from the share is not announced.
	h.ApplyShare("w1", []string{"s1"})
	for len(sub.C()) > 0 {
		<-sub.C() // the session_removed bookkeeping of ApplyShare, not a wake-up
	}
	h.PublishPipeChanged("w1", "s1")
	select {
	case ev := <-sub.C():
		t.Fatalf("an unshared session was announced: %+v", ev)
	default:
	}
	// A subscriber that does not read is closed, the publisher does not wait.
	slow := h.Subscribe(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			h.PublishPipeChanged("w2", "s")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the publisher blocked on a slow subscriber")
	}
	if !errors.Is(slow.Err(), ErrSlowSubscriber) {
		t.Fatalf("slow subscriber: %v", slow.Err())
	}
}
