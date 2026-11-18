//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// A40: a pairing that was cancelled (Ctrl+C) and started again shows a code
// at once, within the first code's lifetime and after it, and never a lone
// "The pairing code expired." without the box of the new code.
func TestACancelledPairingStartedAgainShowsANewCodeAtOnce(t *testing.T) {
	h, ps := newPairingConnHub(t)
	w := newCompWorkerState(t)
	for round := 0; round < 3; round++ {
		var out syncLog
		code := identity.NewPairingCode()
		p := newCompPairer(t, h, w, &out, func() identity.PairingCode { return code })
		run, cancel := runPairer(p)
		eventually(t, "the Hub to hold the Worker as pending", func() bool { return ps().IsPending(code.PairingID()) })
		eventually(t, "the code box", func() bool { return strings.Contains(out.String(), code.String()) })
		if strings.Contains(out.String(), "expired") {
			t.Fatalf("round %d: the box says expired: %s", round, out.String())
		}
		cancel()
		run.wait(t)
		w.restart()
		time.Sleep(50 * time.Millisecond)
	}
}

// A41 through the real Hub and the production dialer: a Worker that has the
// expected Hub fingerprint waits with a code, shows the Hub's words and the
// pairing request; the Hub lists it with the Worker's words, an administrator
// admits it, and the Worker is paired, approved, without the code.
func TestARealWorkerIsAdmittedFromPairingRequests(t *testing.T) {
	h, ps := newPairingConnHub(t)
	w := newCompWorkerState(t)
	out := &syncLog{}
	code := identity.NewPairingCode()
	hubFP := h.service().Identity().Fingerprint()
	run, cancel := runPairer(expectPairer(t, h, w, out, code, matcher(t, string(hubFP))))
	defer cancel()
	eventually(t, "the request to be listed", func() bool { return len(ps().Requests()) == 1 })
	eventually(t, "the box", func() bool { return strings.Contains(out.String(), "Pairing requests") })
	key, err := w.enr.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	q := ps().Requests()[0]
	if q.WorkerFingerprint != key.Fingerprint() || !strings.Contains(out.String(), key.Fingerprint().Words()) {
		t.Fatalf("the Hub lists %s, the Worker shows:\n%s", q.WorkerFingerprint, out.String())
	}
	if _, err := ps().Admit(context.Background(), workers.AdmitRequest{
		UserID: "alice", PairingID: q.PairingID, Fingerprint: string(q.WorkerFingerprint), Approve: true, Actor: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	run.wait(t)
	if run.err != nil || run.enr.Owner != "alice" {
		t.Fatalf("pairer = %+v, %v", run.enr, run.err)
	}
	row, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil || row.Pending {
		t.Fatalf("row = %+v, %v (an admitted Worker is approved)", row, err)
	}
	// The root was checked against the expectation before anything: the
	// Worker records that check, as for a code.
	if run.enr.HubCheck != wstore.HubCheckFingerprint || !strings.Contains(out.String(), "It matched the fingerprint you gave.") {
		t.Fatalf("hub_check %q, console:\n%s", run.enr.HubCheck, out.String())
	}
}

// Without an expected fingerprint and with a certificate this computer does not
// trust, the Worker does not offer an admission: only its code pairs.
func TestARealWorkerWithoutATrustedHubIsNotAdmittable(t *testing.T) {
	h, ps := newPairingConnHub(t)
	w := newCompWorkerState(t)
	out := &syncLog{}
	code := identity.NewPairingCode()
	run, cancel := runPairer(newCompPairer(t, h, w, out, func() identity.PairingCode { return code }))
	defer cancel()
	eventually(t, "the Hub to hold the Worker", func() bool { return ps().IsPending(code.PairingID()) })
	eventually(t, "the box with the Hub's words", func() bool {
		return strings.Contains(out.String(), "confirmed by the pairing code")
	})
	if strings.Contains(out.String(), "Pairing requests") || len(ps().Requests()) != 0 {
		t.Fatalf("an untrusted Hub may admit:\n%s", out.String())
	}
	if !strings.Contains(out.String(), h.service().Identity().Fingerprint().Words()) {
		t.Fatalf("the box lacks the Hub's words:\n%s", out.String())
	}
	if _, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: code.String(), IP: claimIP()}); err != nil {
		t.Fatal(err)
	}
	run.wait(t)
	if run.err != nil {
		t.Fatal(run.err)
	}
}
