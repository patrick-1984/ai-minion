package workers

import (
	"testing"
	"time"
)

// A replaced connection's removal does not delete the new entry: the online
// count (the minion.worker.online gauge) stays right.
func TestReplacedConnectionKeepsTheOnlineCount(t *testing.T) {
	reg := NewRegistry()
	s1, s2 := newFakeSession(), newFakeSession()
	reg.Add(Online{WorkerID: "w1"}, s1)
	reg.Add(Online{WorkerID: "w1"}, s2) // closes s1; its removal runs
	time.Sleep(50 * time.Millisecond)
	if n := reg.Len(); n != 1 {
		t.Fatalf("online = %d after a replacement", n)
	}
	_ = s2.Close()
	deadline := time.Now().Add(2 * time.Second)
	for reg.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the last connection's end did not remove the Worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
