package workers

import "testing"

// ApplyShare drops the unshared sessions at once, with a session_removed event
// each, and the Worker's own late frames about them do not bring them back: an
// event the Worker had already sent (in flight, or queued) is a stale event.
func TestApplyShareDropsAndFencesUnsharedSessions(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(32)
	defer sub.Close()
	_, f := newFeed(t, h, "w1")
	recvEv(t, sub) // worker_online
	if err := f.Snapshot(0, []SessionState{sess("a", "shell", "stable", "ta"), sess("b", "agent:claude", "busy", "secret task")}); err != nil {
		t.Fatal(err)
	}
	recvEv(t, sub) // worker_resynced

	h.ApplyShare("w1", []string{"b", "nope"})
	if ev := recvEv(t, sub); ev.Type != EventSessionRemoved || ev.Session != "b" || ev.Task != nil || ev.State != "" || ev.ScreenHash != "" {
		t.Fatalf("removal event = %+v (it names the session and holds nothing of it)", ev)
	}
	noEvent(t, sub) // "nope" was not known: nothing to remove
	if ws, _ := h.Sessions("w1"); names(ws) != "a" {
		t.Fatalf("sessions = %q, want a", names(ws))
	}
	if !h.Fenced("w1", "b") || !h.Fenced("w1", "nope") || h.Fenced("w1", "a") || h.Fenced("w2", "b") {
		t.Fatal("the fence is wrong")
	}

	// Frames the Worker sent before it applied the change: nothing of b appears, seq still counts.
	for i, c := range []Change{
		{Seq: 1, Event: EventStateChanged, Session: sess("b", "agent:claude", "stable", "stale task"), HasTask: true},
		{Seq: 2, Event: EventSessionAdded, Session: sess("b", "agent:claude", "busy", "again")},
		{Seq: 3, Event: EventScreenChanged, Session: SessionState{Name: "b", ScreenHash: "h"}},
	} {
		if err := f.Apply(c); err != nil {
			t.Fatalf("stale frame %d: %v", i, err)
		}
	}
	if ws, _ := h.Sessions("w1"); names(ws) != "a" {
		t.Fatalf("a stale event brought an unshared session back: %q", names(ws))
	}
	noEvent(t, sub)

	// Others are not affected.
	if err := f.Apply(Change{Seq: 4, Event: EventStateChanged, Session: sess("a", "shell", "busy", ""), HasTask: false}); err != nil {
		t.Fatal(err)
	}
	if ev := recvEv(t, sub); ev.Type != EventStateChanged || ev.Session != "a" {
		t.Fatalf("event = %+v", ev)
	}

	// The Worker confirms the removal: the stream is ordered, so nothing stale of b can follow,
	// but b stays hidden (the history the Hub kept must not come back by itself)...
	if err := f.Apply(Change{Seq: 5, Event: EventSessionRemoved, Session: SessionState{Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	noEvent(t, sub)
	if !h.Fenced("w1", "b") || !h.Fenced("w1", "nope") {
		t.Fatal("the confirmation of the removal lifted the fence")
	}
	// ...until the Worker reports it shared again: a session_added after its own removal.
	if err := f.Apply(Change{Seq: 6, Event: EventSessionAdded, Session: sess("b", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	if ev := recvEv(t, sub); ev.Type != EventSessionAdded || ev.Session != "b" {
		t.Fatalf("event = %+v", ev)
	}
	if h.Fenced("w1", "b") || !h.Fenced("w1", "nope") {
		t.Fatal("the fence after the session was shared again is wrong")
	}
	if ws, _ := h.Sessions("w1"); names(ws) != "a,b" {
		t.Fatalf("sessions = %q", names(ws))
	}
}

// A later call replaces the fence: a session that is shared again leaves it, and a
// new snapshot (a new stream starts from what the Worker shares now) clears it; Forget too.
func TestApplyShareFenceIsReplacedByTheNextCallAndSnapshotAndForget(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	_, f := newFeed(t, h, "w1")
	if err := f.Snapshot(0, []SessionState{sess("a", "shell", "stable", ""), sess("b", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	h.ApplyShare("w1", []string{"a", "b"})
	h.ApplyShare("w1", []string{"b"}) // a is shared again
	if h.Fenced("w1", "a") || !h.Fenced("w1", "b") {
		t.Fatal("the next call did not replace the fence")
	}
	h.ApplyShare("w1", nil)
	if h.Fenced("w1", "b") {
		t.Fatal("share all left a fence")
	}

	h.ApplyShare("w1", []string{"b"})
	_, f2 := newFeed(t, h, "w1") // a new connection of the Worker
	if err := f2.Snapshot(0, []SessionState{sess("b", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	if h.Fenced("w1", "b") {
		t.Fatal("a new snapshot did not end the fence")
	}
	h.ApplyShare("w1", []string{"b"})
	h.Forget("w1")
	if h.Fenced("w1", "b") {
		t.Fatal("Forget left a fence")
	}
	h.ApplyShare("unknown-worker", []string{"x"}) // a Worker the Hub has no state of: fenced, nothing to drop
	if !h.Fenced("unknown-worker", "x") {
		t.Fatal("no fence for a Worker without state")
	}
}
