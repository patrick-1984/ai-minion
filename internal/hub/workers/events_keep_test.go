package workers

import (
	"encoding/json"
	"testing"
	"time"
)

// session.keep.v1: the keep flag of the state stream reaches the Hub's
// session state and its event feed; keeping or forgetting a session is a
// state change, and a Worker that omits the flag reports the session as not
// kept.
func TestFeedCarriesTheKeepFlag(t *testing.T) {
	h := NewEventHub(EventHubOptions{})
	sub := h.Subscribe(16)
	defer sub.Close()
	_, f := newFeed(t, h, "w1")
	snapshot := `{"type":"events.snapshot","seq":0,"sessions":[{"name":"tv","kind":"agent:claude","state":"busy","task":"","keep":true},{"name":"old","kind":"shell","state":"stable","task":""}]}`
	if err := feedFrame(f, json.RawMessage(snapshot), nil); err != nil {
		t.Fatal(err)
	}
	kept := func() map[string]bool {
		ws, _ := h.Sessions("w1")
		out := map[string]bool{}
		for _, s := range ws.Sessions {
			out[s.Name] = s.Keep
		}
		return out
	}
	if got := kept(); !got["tv"] || got["old"] {
		t.Fatalf("keep after the snapshot = %v", got)
	}
	drain(sub)
	const keepOld = `{"type":"event","seq":1,"event":"state_changed","session":"old","kind":"shell","state":"stable","task":"","keep":true}`
	if err := feedFrame(f, json.RawMessage(keepOld), nil); err != nil {
		t.Fatal(err)
	}
	if got := kept(); !got["old"] {
		t.Fatalf("keep after the change = %v", got)
	}
	select {
	case ev := <-sub.C():
		if ev.Type != EventStateChanged || ev.Session != "old" || !ev.Keep {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("keeping a session published nothing")
	}
	const forgetTV = `{"type":"event","seq":2,"event":"state_changed","session":"tv","kind":"agent:claude","state":"busy","task":""}`
	if err := feedFrame(f, json.RawMessage(forgetTV), nil); err != nil {
		t.Fatal(err)
	}
	if got := kept(); got["tv"] {
		t.Fatalf("keep after forgetting = %v", got)
	}
}

// drain empties a subscription's queue.
func drain(s *Subscription) {
	for {
		select {
		case <-s.C():
		default:
			return
		}
	}
}
