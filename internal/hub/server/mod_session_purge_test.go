package server

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

type fakeSessions struct {
	ws     workers.WorkerSessions
	fenced map[string]bool
}

func (f fakeSessions) Sessions(string) (workers.WorkerSessions, bool) { return f.ws, true }
func (f fakeSessions) Fenced(_, s string) bool                        { return f.fenced[s] }

// A20: a removed session and, after a resync, every session the Worker no
// longer shares lose their master nomination; a name that comes back is a
// slave session.
func TestGoneSessionsLoseTheirMasterNomination(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, n := range []string{"boss", "ci", "hidden", "live"} {
		if err := st.PutMasterSession(ctx, store.MasterRow{WorkerID: "wA", SessionName: n, CreatedBy: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	p := &sessionPurger{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: fakeSessions{
			ws:     workers.WorkerSessions{WorkerID: "wA", Online: true, Sessions: []workers.SessionState{{Name: "live"}, {Name: "hidden"}}},
			fenced: map[string]bool{"hidden": true},
		}}
	p.handle(ctx, workers.Event{Type: workers.EventSessionRemoved, WorkerID: "wA", Session: "boss"})
	if _, err := st.MasterSession(ctx, "wA", "boss"); err == nil {
		t.Fatal("boss is still a master after it was removed")
	}
	p.handle(ctx, workers.Event{Type: workers.EventWorkerResynced, WorkerID: "wA"})
	ms, _ := st.ListMasterSessions(ctx)
	if len(ms) != 1 || ms[0].SessionName != "live" {
		t.Fatalf("masters after the resync = %+v (ci is gone, hidden is not shared)", ms)
	}
}
