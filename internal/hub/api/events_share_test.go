package api

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// An event that was queued for an open stream before share_set returned is not
// delivered afterwards: the decision is made when the event is delivered, from
// the Hub's current state, and a cached "readable" never lets it through. The
// removal of the session, which names it and holds nothing of it, still is.
func TestEventsQueuedBeforeShareSetAreNotDeliveredAfterIt(t *testing.T) {
	r := newEvRig(t, nil)
	_, f := r.feed(wAlice, ss("a1", "agent:claude", "stable", "t1"), ss("a2", "agent:claude", "busy", "secret task"))
	s := &eventStream{h: r.events, id: Identity{Principal: authz.NewSessionPrincipal(evAlice)},
		cache: map[string]bool{}, seen: map[string]struct{}{}}
	ctx := context.Background()
	state := func(session string) workers.Event {
		return workers.Event{Type: workers.EventStateChanged, WorkerID: wAlice, Worker: "pi-1", Session: session, State: "busy"}
	}
	if !s.visible(ctx, state("a2")) || !s.visible(ctx, state("a1")) { // fills the decision cache
		t.Fatal("a readable event was hidden before share_set")
	}

	r.hub.ApplyShare(wAlice, []string{"a2"})

	for _, typ := range []string{workers.EventStateChanged, workers.EventSessionAdded, workers.EventScreenChanged} {
		ev := state("a2")
		ev.Type = typ
		if s.visible(ctx, ev) {
			t.Fatalf("a queued %s of an unshared session was delivered", typ)
		}
	}
	if !s.visible(ctx, state("a1")) {
		t.Fatal("the share change hid a session that is still shared")
	}
	if rm := (workers.Event{Type: workers.EventSessionRemoved, WorkerID: wAlice, Worker: "pi-1", Session: "a2"}); !s.visible(ctx, rm) {
		t.Fatal("the removal of the session was not delivered: the client would keep it")
	}
	_ = f

	// The list and a new stream's snapshot do not name it either.
	if got := r.sessions("alice", ""); strings.Contains(sessionNames(got), "a2") || !strings.Contains(sessionNames(got), "a1") {
		t.Fatalf("sessions = %q", sessionNames(got))
	}
	c := r.open("alice", "")
	snap := decodeInfo(t, c.expect("snapshot"))
	if len(snap.Sessions) != 1 || snap.Sessions[0].Session != "a1" {
		t.Fatalf("a new stream's snapshot = %+v", snap)
	}
}

// The same holds when the Worker's rule (and not a list of unshared names) took
// the session away: an event queued for an open stream before the answer
// returned is not delivered after it, and the removal of the session still is.
// A Worker that has declared nothing is not hidden for its live events: it only
// ever reports sessions it shares; it is the stored history that waits for the
// rule.
func TestEventsQueuedBeforeAShareRuleAreNotDeliveredAfterIt(t *testing.T) {
	r := newEvRig(t, nil)
	_, f := r.feed(wAlice, ss("a1", "agent:claude", "stable", "t1"), ss("a2", "agent:claude", "busy", "secret task"))
	s := &eventStream{h: r.events, id: Identity{Principal: authz.NewSessionPrincipal(evAlice)},
		cache: map[string]bool{}, seen: map[string]struct{}{}}
	ctx := context.Background()
	state := func(session string) workers.Event {
		return workers.Event{Type: workers.EventStateChanged, WorkerID: wAlice, Worker: "pi-1", Session: session, State: "busy"}
	}
	if !s.visible(ctx, state("a2")) || !s.visible(ctx, state("a1")) {
		t.Fatal("a Worker that declared no rule had its events hidden")
	}
	_ = f

	next := ops.ShareRule{Mode: "select", Names: []string{"a1"}}
	r.hub.BeginShare(wAlice, &next).Applied(ops.ShareResult{Rule: next, Unshared: []string{"a2"}, Revision: "2"})

	if s.visible(ctx, state("a2")) {
		t.Fatal("a queued event of a session the rule does not share was delivered")
	}
	if !s.visible(ctx, state("a1")) {
		t.Fatal("the rule hid a session it shares")
	}
	if rm := (workers.Event{Type: workers.EventSessionRemoved, WorkerID: wAlice, Worker: "pi-1", Session: "a2"}); !s.visible(ctx, rm) {
		t.Fatal("the removal of the session was not delivered: the client would keep it")
	}
	if got := r.sessions("alice", ""); strings.Contains(sessionNames(got), "a2") || !strings.Contains(sessionNames(got), "a1") {
		t.Fatalf("sessions = %q", sessionNames(got))
	}
}
