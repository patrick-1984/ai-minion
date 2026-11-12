package web

// Design 12.3: the "Re-issue pairing" button of a Worker that never connected.

import (
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestWorkerPageOffersReissuePairingOnlyWhenEligible(t *testing.T) {
	never := WorkerRow{ID: "w7", Name: "lost-welcome", Owner: "alice", Kind: ops.WorkerKindPrivate, NeverConnected: true}
	seen := WorkerRow{ID: "w8", Name: "seen", Owner: "alice", Kind: ops.WorkerKindPrivate}
	online := WorkerRow{ID: "w6", Name: "online-now", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, NeverConnected: true}
	d := Dashboard{Workers: []WorkerRow{never, seen, online}}
	for _, tc := range []struct {
		id      string
		operate bool
		want    bool
	}{
		{"w7", true, true},   // owner or admin, never connected
		{"w7", false, false}, // somebody who may not operate the Worker
		{"w8", true, false},  // connected before
		{"w6", true, false},  // connected right now
	} {
		out, err := renderWorkerFragment(newWorkerView(d, false, tc.id, tc.operate))
		if err != nil {
			t.Fatal(err)
		}
		path := `hx-post="/workers/` + tc.id + `/repair-pairing"`
		if tc.want {
			mustHave(t, out, path, `data-control="repair-pairing"`, ">Re-issue pairing<")
		} else {
			mustNotHave(t, out, path, `data-control="repair-pairing"`)
		}
	}
	// The Workers list itself does not carry the button.
	v := newDashboardView(d, false)
	for _, w := range v.Workers {
		if w.RepairURL != "" {
			t.Errorf("list row %s has a repair URL", w.ID)
		}
	}
}

func TestReissuePairingButtonCallsTheCatalogOperation(t *testing.T) {
	f := newFakeOps().reply(ops.OpWorkerPairingReissue, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	s := onbUser(t)
	id := ops.NewUUIDv7()
	rec := onbPost(t, s, WorkerRepairPairingPath(id), url.Values{})
	if rec.Code != 200 {
		t.Fatalf("repair-pairing = %d %s", rec.Code, rec.Body.String())
	}
	c := f.called(ops.OpWorkerPairingReissue)
	if len(c) != 1 || c[0].Args["worker_id"] != id || c[0].Args["request_id"] == "" {
		t.Fatalf("worker_pairing_reissue calls = %+v", c)
	}
}
