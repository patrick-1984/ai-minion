package agent

import (
	"reflect"
	"testing"
	"time"
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
