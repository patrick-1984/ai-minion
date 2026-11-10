//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// Integration: a real tmux session on a private socket, the real classifier
// and the real clock. A change in the pane produces the events.
func TestTmuxSessionChangeProducesEvents(t *testing.T) {
	srv := tmuxtest.New(t)
	host, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"watched"}}); err != nil {
		t.Fatal(err)
	}
	target := srv.NewSession("watched", "sh")
	srv.NewSession("hidden", "sh") // not shared: must never show up

	obs, err := NewHostObserver(HostObserverConfig{Host: host, Classifier: agentdetect.New(), Policy: NewPolicy(st)})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := NewEventPublisher(EventConfig{Observer: obs, Interval: MinPollInterval})
	if err != nil {
		t.Fatal(err)
	}
	ev := newFakeEvStream()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.serve(ctx, func(context.Context) (EventStream, error) { return ev, nil })
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })

	snap := ev.snapshot(t)
	if len(snap.Sessions) != 1 || snap.Sessions[0].Name != "watched" || snap.Sessions[0].Kind != "shell" {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Typing changes the screen.
	srv.SendKeys(target, "echo changed-by-test", "Enter")
	got := waitEvent(t, ev, EventScreenChanged)
	if got.Session != "watched" || got.ScreenHash == "" || got.ScreenHash == snap.Sessions[0].ScreenHash {
		t.Fatalf("screen_changed = %+v", got)
	}

	// A foreground program that is not a shell changes kind: state_changed.
	srv.SendKeys(target, "exec sleep 300", "Enter")
	got = waitEvent(t, ev, EventStateChanged)
	if got.Session != "watched" || got.Kind == "shell" {
		t.Fatalf("state_changed = %+v", got)
	}
}

// waitEvent returns the first event of the wanted kind within the timeout and
// checks that every frame on the way is well formed, in sequence and about the
// shared session.
func waitEvent(t *testing.T, ev *fakeEvStream, want string) EventFrame {
	t.Helper()
	deadline := time.After(20 * time.Second)
	var last uint64
	for {
		select {
		case f := <-ev.frames:
			var e EventFrame
			if err := json.Unmarshal(f, &e); err != nil {
				t.Fatalf("frame %s: %v", f, err)
			}
			if e.Session == "hidden" {
				t.Fatalf("an unshared session appeared: %s", f)
			}
			if e.Seq <= last {
				t.Fatalf("seq %d after %d", e.Seq, last)
			}
			last = e.Seq
			if e.Event == want {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s event within 20 s", want)
		}
	}
}
