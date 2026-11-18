package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// failingListHost is a session host whose listing fails.
type failingListHost struct{ *fakeHost }

func (failingListHost) ListSessions(context.Context) ([]term.Session, error) {
	return nil, errors.New("listing failed")
}

// A listing error is an unknown count, never 0.
func TestCountLostListingError(t *testing.T) {
	c := SessionCounter{Host: failingListHost{newFakeHost()}, EndsSessions: true}
	n, known, err := c.CountLost(context.Background())
	if known || err == nil || n != 0 {
		t.Fatalf("CountLost = %d, %t, %v; want unknown with the error", n, known, err)
	}
	if n, known, err := (SessionCounter{EndsSessions: true}).CountLost(context.Background()); known || err == nil || n != 0 {
		t.Fatalf("without a host = %d, %t, %v; want unknown", n, known, err)
	}
	h := newFakeHost()
	h.addSession("a", "claude")
	h.addSession("b", "claude")
	if n, known, err := (SessionCounter{Host: h, EndsSessions: true}).CountLost(context.Background()); !known || err != nil || n != 2 {
		t.Fatalf("ConPTY count = %d, %t, %v; want 2", n, known, err)
	}
	if n, known, err := (SessionCounter{Host: failingListHost{h}}).CountLost(context.Background()); !known || err != nil || n != 0 {
		t.Fatalf("tmux (sessions survive) = %d, %t, %v; want a known 0 without listing", n, known, err)
	}
}

// The updater refuses when the count is unknown, unless forced.
func TestUpdateRefusesOnUnknownCount(t *testing.T) {
	withVersion(t, "v0.9.0")
	inst := &fakeInstaller{}
	u, restarted := newTestUpdate(t, inst)
	var mu sync.Mutex
	var holds []string
	u.holdSessions = func(r string) { mu.Lock(); holds = append(holds, r); mu.Unlock() }
	u.sessionsLost = SessionCounter{Host: failingListHost{newFakeHost()}, EndsSessions: true}.CountLost
	res, err := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1"})
	if err != nil || res.Accepted || !strings.Contains(res.Reason, "cannot be counted") {
		t.Fatalf("update = %+v, %v; want a refusal for the unknown count", res, err)
	}
	mu.Lock()
	if len(holds) != 2 || holds[1] != "" {
		t.Fatalf("holds = %q, want hold then release", holds)
	}
	mu.Unlock()
	if len(inst.opts) != 0 {
		t.Fatal("the installer ran")
	}
	if res, err := callUpdate(t, u, ops.WorkerUpdateArgs{Version: "v0.9.1", Force: true}); err != nil || !res.Accepted {
		t.Fatalf("forced update = %+v, %v; want accepted", res, err)
	}
	// Let the forced run finish before the test (and its version) ends.
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the forced update did not finish")
	}
}

// The updater's release lifts only its own hold: a lifecycle hold taken
// before or during the update keeps session_create refused.
func TestUpdateReleaseKeepsLifecycleHold(t *testing.T) {
	e := newEnv(t)
	release := e.ex.HoldCreate(HoldOwnerLifecycle, "the Worker is stopping")
	e.ex.SetSessionCreateHold("the Worker is updating")
	e.ex.SetSessionCreateHold("") // the update failed or was refused
	_, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSessionCreate, Actions: []ops.Action{ops.ActionOperate}, Args: []byte(`{"name":"x"}`)})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeConflict || !strings.Contains(ae.Message, "stopping") {
		t.Fatalf("session_create under the lifecycle hold = %v", err)
	}
	if reason, held := e.ex.CreateHeld(); !held || reason != "the Worker is stopping" {
		t.Fatalf("CreateHeld = %q, %t", reason, held)
	}
	// The update's hold beside the lifecycle hold: releasing the lifecycle
	// keeps the update's.
	e.ex.SetSessionCreateHold("the Worker is updating")
	release()
	release() // idempotent
	if reason, held := e.ex.CreateHeld(); !held || reason != "the Worker is updating" {
		t.Fatalf("after the lifecycle release CreateHeld = %q, %t", reason, held)
	}
	e.ex.SetSessionCreateHold("")
	if _, held := e.ex.CreateHeld(); held {
		t.Fatal("no owner holds, but session_create is still held")
	}
}
