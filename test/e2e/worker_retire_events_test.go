//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// The production Hub attaches the event ingest to every session (mod_events:
// Registry().OnAdd(ing.Attach)). The real Retirer must still get its
// worker_retire call served: the session has one reader of its streams, the
// connect handler's host, which gives each kind to its owner.
func TestRealRetirerWorksWithTheEventIngestAttached(t *testing.T) {
	h := newConnHub(t)
	ing, err := workers.NewIngest(workers.IngestOptions{Hub: workers.NewEventHub(workers.EventHubOptions{})})
	if err != nil {
		t.Fatal(err)
	}
	h.service().Registry().OnAdd(ing.Attach)
	for i := 0; i < 20; i++ {
		oldKey, err := identity.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		row := h.enrol(oldKey.Public(), fmt.Sprintf("pi-old-%d", i))
		rs := wagent.NewRetiredStore(t.TempDir())
		if err := rs.Add(wagent.RetiredIdentity{Key: oldKey, HubURL: h.url(), Pin: h.pin(), WorkerID: row.ID, RetiredAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		ret, err := wagent.NewRetirer(wagent.RetirerConfig{Store: rs, Dialer: wagent.NewHubDialer(), Name: "pi"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		remaining, err := ret.RunOnce(ctx)
		cancel()
		if err != nil || remaining != 0 {
			t.Fatalf("attempt %d: RunOnce = %d, %v", i, remaining, err)
		}
	}
}
