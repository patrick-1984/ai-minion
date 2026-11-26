package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Verification of "a lost session loses its permission" (owner question,
// v0.22.0). A tmux session that is killed and recreated under the same name
// between two polls (default 1s, configurable up to 1m), including a tmux
// server restart that recreates the name, has another instance
// (term.Session.Instance: boot id, server pid and start time, session id).
// The Worker reports it as a session_removed / session_added pair, so the
// Hub sees a new generation of the name and its first-seen fence and its
// purge of name-only (Claimed) master nominations and grants fire.
func TestVerifyLossRecreateWithinOnePollIsReported(t *testing.T) {
	old := View{"boss": {Name: "boss", Kind: "agent:claude", State: "idle", Task: "t", InstanceID: "i1"}}
	recreated := View{"boss": {Name: "boss", Kind: "agent:claude", State: "idle", Task: "t", InstanceID: "i2"}}
	want := []Change{
		{EventSessionRemoved, SessionState{Name: "boss"}},
		{EventSessionAdded, recreated["boss"]},
	}
	if ch := Diff(old, recreated); !reflect.DeepEqual(ch, want) {
		t.Fatalf("a recreated session: %+v, want %+v", ch, want)
	}
	// The added event carries the instance on the wire.
	if f := eventFrameOf(2, time.Time{}, want[1]); f.InstanceID != "i2" {
		t.Fatalf("frame instance = %q", f.InstanceID)
	}
}

// A host that reports no instance (psmux, an old tmux) keeps the name-only
// behaviour: no replacement can be seen. An instance that becomes known is a
// state change only, never a replacement.
func TestDiffInstanceUnknownIsNoReplacement(t *testing.T) {
	none := View{"boss": {Name: "boss", Kind: "shell", State: "idle"}}
	if ch := Diff(none, none); len(ch) != 0 {
		t.Fatalf("no instance: %+v", ch)
	}
	known := View{"boss": {Name: "boss", Kind: "shell", State: "idle", InstanceID: "i1"}}
	ch := Diff(none, known)
	if len(ch) != 1 || ch[0].Event != EventStateChanged || ch[0].Session.InstanceID != "i1" {
		t.Fatalf("instance became known: %+v", ch)
	}
	if ch := Diff(known, none); len(ch) != 1 || ch[0].Event != EventStateChanged {
		t.Fatalf("instance became unknown: %+v", ch)
	}
}

// The production observer reports the host's instance of every shared session
// (the same value as the instance_id of sessions_list), so a kill and create
// of the name between two looks is published as removed and added, with the
// new instance on the wire.
func TestHostObserverReportsTheInstanceAndARecreateBetweenLooks(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("boss", "claude")
	e.host.setScreen(ref, "same")
	e.host.mu.Lock()
	e.host.sessions[0].Instance = "i1"
	e.host.mu.Unlock()
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	obs, err := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	clk := newManualClock()
	pub, _ := NewEventPublisher(EventConfig{Observer: obs, Interval: time.Second, Clock: clk})
	st := newFakeEvStream()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.serve(ctx, func(context.Context) (EventStream, error) { return st, nil })
		close(done)
	}()
	defer func() { cancel(); <-done }()
	if snap := st.snapshot(t); len(snap.Sessions) != 1 || snap.Sessions[0].InstanceID != "i1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	// Killed and created again under the name within one poll: same screen,
	// same kind, another instance.
	e.host.mu.Lock()
	e.host.sessions[0].Instance = "i2"
	e.host.mu.Unlock()
	clk.tick(t, time.Second)
	if ev := st.event(t); ev.Event != EventSessionRemoved || ev.Session != "boss" || ev.InstanceID != "" {
		t.Fatalf("first event = %+v", ev)
	}
	if ev := st.event(t); ev.Event != EventSessionAdded || ev.Session != "boss" || ev.InstanceID != "i2" {
		t.Fatalf("second event = %+v", ev)
	}
}
