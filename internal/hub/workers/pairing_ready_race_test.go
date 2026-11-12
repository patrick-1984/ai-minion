package workers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// gatedPairingStore holds the pairing-row read of Await while armed, and
// counts the resume failures charged against commit rows.
type gatedPairingStore struct {
	PairingStore
	mu       sync.Mutex
	hold     chan struct{} // non-nil: Pairing blocks until it is closed
	reading  chan struct{} // closed when a held read started
	resumeNo atomic.Int32
}

func (g *gatedPairingStore) arm() (reading <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hold, g.reading = make(chan struct{}), make(chan struct{})
	hold := g.hold
	var once sync.Once
	return g.reading, func() { once.Do(func() { close(hold) }) }
}

func (g *gatedPairingStore) Pairing(ctx context.Context, id string) (store.PairingRow, error) {
	g.mu.Lock()
	hold, reading := g.hold, g.reading
	g.hold, g.reading = nil, nil
	g.mu.Unlock()
	if hold != nil {
		close(reading)
		<-hold
	}
	return g.PairingStore.Pairing(ctx, id)
}

func (g *gatedPairingStore) AddResumeFailure(ctx context.Context, codeDigest string) (int, error) {
	g.resumeNo.Add(1)
	return g.PairingStore.AddResumeFailure(ctx, codeDigest)
}

// RC1 release gate, finding 5: a claim that meets an entry whose row is still
// being read (no resume ticket known yet) must not take the plain path and
// accept the entry after its unlocked read just because the pointer is the
// same: the entry got its resume ticket meanwhile, and a stranger's wrong
// code would then be charged against the owner's resume. The seam runs the
// rest of Await (ticket set, ready closed) exactly in the claim's unlocked
// window.
func TestAStrangersClaimDuringTheReadinessWindowChargesNothing(t *testing.T) {
	th := newTestHub(t, nil)
	clk := newFakeClock()
	gs := &gatedPairingStore{PairingStore: th.st}
	ps, err := th.svc.EnablePairing(PairingOptions{Store: gs, Limiter: allowAll{}, Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	th.svc.SetOwnerNames(func(_ context.Context, id string) string { return "user-" + id })
	h := &pairHub{testHub: th, ps: ps, clk: clk}

	key := newKey(t)
	code := identity.NewPairingCode()
	row := pairAndLoseTheWelcome(t, h, key, code, "u-alice")
	if n := gs.resumeNo.Load(); n != 0 {
		t.Fatalf("%d resume failures before the race", n)
	}

	// Right after the claim's readiness wait (no entry yet), the Worker
	// reconnects with the same code; its row read is held, so the entry is
	// reserved but not ready when the claim decides. Right after the decision
	// released the lock, the row read completes: the entry gets its resume
	// ticket and becomes ready inside the claim's unlocked window.
	reading, release := gs.arm()
	defer release()
	var w *pairingWorker
	var waited, decided sync.Once
	ps.testSeam = func(stage string) {
		switch stage {
		case "waited":
			waited.Do(func() {
				w = h.startWorker(t, key, code, "pi")
				<-reading
			})
		case "decided":
			decided.Do(func() {
				release()
				eventually(t, "the entry to become ready", func() bool {
					ps.mu.Lock()
					defer ps.mu.Unlock()
					p := ps.live[code.PairingID()]
					return p != nil && isReady(p)
				})
			})
		}
	}

	_, err = h.claim("u-mallory", wrongTail(code), "")
	if proto.CodeOf(err) != proto.CodePairingCodeInvalid {
		t.Fatalf("the stranger's claim = %v", err)
	}
	if n := gs.resumeNo.Load(); n != 0 {
		t.Fatalf("the stranger's wrong code charged %d resume attempts of the owner", n)
	}
	if n := countAudit(t, h, "pairing.attempt"); n != 0 {
		t.Errorf("%d counted pairing attempts", n)
	}
	if !ps.IsPending(code.PairingID()) {
		t.Fatal("the stranger's claim ended the owner's resume")
	}
	// The owner's resume still works.
	res, err := h.claim("u-alice", code.String(), "")
	if err != nil || res.WorkerID != row.ID {
		t.Fatalf("the owner's resume = %+v %v", res, err)
	}
	w.wait(t)
}
