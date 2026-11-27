package workers

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// IsPending is what tests wait on before they touch the pairing row. Await
// registers the entry before it reads or writes the row, so an IsPending that
// only looked at the entry could answer true while the row did not exist yet
// (the flake of TestAPendingWorkerThatClosesItsConnectionIsDroppedAndItsIdIsFree:
// "store: pairing is not pending"). An entry is pending only once its row has
// been read.
func TestIsPendingWaitsForThePairingRow(t *testing.T) {
	th := newTestHub(t, nil)
	clk := newFakeClock()
	gs := &gatedPairingStore{PairingStore: th.st}
	ps, err := th.svc.EnablePairing(PairingOptions{Store: gs, Limiter: allowAll{}, Clock: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := &pairHub{testHub: th, ps: ps, clk: clk}

	reading, release := gs.arm()
	defer release()
	code := identity.NewPairingCode()
	_, _ = h.rawWorker(t, newKey(t), code, "pi")
	<-reading // the entry is registered, its row read is held

	ps.mu.Lock()
	registered := ps.live[code.PairingID()] != nil
	ps.mu.Unlock()
	if !registered {
		t.Fatal("the entry is not registered while its row is read")
	}
	if ps.IsPending(code.PairingID()) {
		t.Fatal("IsPending is true before the pairing row was read")
	}

	release()
	eventually(t, "the Worker to be pending", func() bool { return ps.IsPending(code.PairingID()) })
	row, err := th.st.Pairing(context.Background(), code.PairingID())
	if err != nil || row.Status != store.PairingPending {
		t.Fatalf("row = %+v err = %v once pending", row, err)
	}
}
