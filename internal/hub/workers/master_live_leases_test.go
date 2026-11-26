package workers

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// LiveLeases lists exactly the leases that authorization accepts: an eligible
// candidate whose lease is not issued yet (a share snapshot that arrived
// before the next report) is listed as a candidate but holds no lease.
func TestLiveLeasesListsLeasesNotCandidates(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	want := LiveLease{WorkerID: r.source.ID, SessionName: "prepared", IncarnationID: "first"}
	if got := m.LiveLeases(); len(got) != 1 || got[0] != want {
		t.Fatalf("live leases = %+v, want %+v", got, want)
	}

	// The candidate stays eligible, but its lease is gone until the next
	// report: authorization refuses it, so the read model must not list it.
	m.mu.Lock()
	clear(r.lane.leases)
	m.mu.Unlock()
	view, err := m.MasterView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view[r.source.ID].Candidates) != 1 {
		t.Fatalf("candidates = %+v", view[r.source.ID].Candidates)
	}
	if got := m.LiveLeases(); len(got) != 0 {
		t.Fatalf("a candidate without lease is listed as a live lease: %+v", got)
	}
	call := Call{Worker: r.source, TranscriptHash: r.lane.info.TranscriptHash, RequestID: ops.NewUUIDv7(),
		Master: &proto.MasterCallArgs{Generation: 1, IncarnationID: "first", LeaseID: ops.NewUUIDv7(),
			Operation: ops.OpWorkersList, Arguments: []byte(`{}`)}}
	m.mu.Lock()
	_, _, err = m.authorizeLocked(call)
	m.mu.Unlock()
	if err == nil {
		t.Fatal("authorization accepted an incarnation without lease")
	}

	// The refresh issues the lease again; an expired lane lists nothing.
	m.RefreshLeases(r.source.ID)
	if got := m.LiveLeases(); len(got) != 1 || got[0] != want {
		t.Fatalf("live leases after refresh = %+v", got)
	}
	m.mu.Lock()
	r.lane.expires = m.now()
	m.mu.Unlock()
	if got := m.LiveLeases(); len(got) != 0 {
		t.Fatalf("an expired lane lists leases: %+v", got)
	}
}
