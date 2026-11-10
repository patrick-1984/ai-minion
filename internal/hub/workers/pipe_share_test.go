package workers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func wantNotShared(t *testing.T, err error, what string) {
	t.Helper()
	pe, ok := err.(*proto.Error)
	if !ok || pe.Code != proto.CodeForbidden || string(pe.Details) != `{"reason":"not_shared"}` {
		t.Fatalf("%s: err = %#v, want forbidden/not_shared", what, err)
	}
}

// A history stored while a session was shared is not served once the session is
// unshared: Read, a waiting Wait and a Wait that starts later all refuse, for
// every kind of caller, with an answer that names nothing of the history; and
// the same session is served again when it is shared again. Stored history is
// hidden, not erased.
func TestPipeHistoryOfAnUnsharedSessionIsNotServed(t *testing.T) {
	ctx := context.Background()
	var fenced atomic.Bool
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Shared = func(workerID, session string) bool {
			return !fenced.Load() || workerID != wAlicePi1.ID || session != pipeSess
		}
	})
	if _, err := r.p.Send(ctx, authz.NewSessionPrincipal(idAlice), sendArgs(wAlicePi1, pipeSess, "the question")); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Second)
	r.commit(t, wAlicePi1, pipeSess, "the private answer")
	r.commit(t, wAlicePi1, "minion-2", "another session")
	args := ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)}
	owner, admin := authz.NewSessionPrincipal(idAlice), authz.NewSessionPrincipal(idRoot)

	view, err := r.p.Read(ctx, owner, args)
	if err != nil || len(view.Turns) != 2 {
		t.Fatalf("shared read = %+v, %v", view, err)
	}

	// A wait that is open while the session is unshared ends with the refusal when the next turn arrives...
	waiting := make(chan error, 1)
	go func() {
		waiting <- r.p.Wait(ctx, owner, ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), TimeoutMS: 5000}, func(ops.PipeTurn) error {
			t.Error("a turn of an unshared session was delivered to a wait")
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond) // the wait is parked; the poll below wakes it either way
	fenced.Store(true)
	r.commit(t, wAlicePi1, pipeSess, "a turn after the unshare")
	select {
	case err := <-waiting:
		wantNotShared(t, err, "a wait open across the unshare")
	case <-time.After(3 * time.Second):
		t.Fatal("the wait was not ended by the unshare")
	}

	// ...and every new read or wait is refused, for the owner, an admin and a model.
	for name, p := range map[string]authz.Principal{"owner": owner, "admin": admin, "model": modelAlice()} {
		_, err := r.p.Read(ctx, p, args)
		wantNotShared(t, err, name+" read")
		err = r.p.Wait(ctx, p, ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), TimeoutMS: 50}, func(ops.PipeTurn) error { return nil })
		wantNotShared(t, err, name+" wait")
	}
	// A session that is still shared is unaffected.
	if v, err := r.p.Read(ctx, owner, ops.PipeReadArgs{Target: target(wAlicePi1, "minion-2")}); err != nil || len(v.Turns) != 1 {
		t.Fatalf("another session = %+v, %v", v, err)
	}
	// Sharing it again serves the history that was kept.
	fenced.Store(false)
	if v, err := r.p.Read(ctx, owner, args); err != nil || len(v.Turns) != 3 {
		t.Fatalf("after sharing again = %+v, %v", v, err)
	}
}
