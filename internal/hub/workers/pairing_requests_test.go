package workers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// startRequester runs a Worker that waits with a code and accepts an
// admission (it announces proto.FeaturePairAdmit). It records the Hub root its
// hello showed.
func (h *pairHub) startRequester(t *testing.T, key *identity.Key, code identity.PairingCode, name string, admit bool) (*pairingWorker, func() []byte) {
	t.Helper()
	w := &pairingWorker{done: make(chan struct{})}
	var shown []byte
	shownCh := make(chan struct{})
	cfg := transport.WorkerConfig{
		Key: key, Name: name, Ops: []string{"send_text"}, Mode: transport.ModePairCode,
		PairingCode: code, Timeout: 30 * time.Second, AllowAdmit: admit,
		OnHubHello: func(root ed25519.PublicKey) { shown = root; close(shownCh) },
	}
	if admit {
		cfg.Features = []string{proto.FeaturePairAdmit}
	}
	go func() {
		defer close(w.done)
		w.res, w.err = h.dial(cfg)
	}()
	t.Cleanup(func() {
		h.ps.Close() // ends a Worker that still waits
		<-w.done
		if w.res != nil {
			_ = w.res.Channel.Close()
		}
	})
	eventually(t, "the Hub to hold the Worker as pending and its row read", func() bool {
		h.ps.mu.Lock()
		defer h.ps.mu.Unlock()
		p := h.ps.live[code.PairingID()]
		return p != nil && isReady(p)
	})
	recv(t, shownCh, "the Hub hello")
	return w, func() []byte { return shown }
}

func fingerprintText(t *testing.T, key *identity.Key) string {
	t.Helper()
	return key.Fingerprint().String()
}

func TestAWaitingWorkerIsListedWithItsFactsAndTheHubHelloArrivesBeforeAnyClaim(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	_, shown := h.startRequester(t, key, code, "pi 4", true)
	if !bytes.Equal(shown(), h.svc.Identity().Public()) {
		t.Fatal("the waiting Worker did not get the Hub's root before the claim")
	}
	reqs := h.ps.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	q := reqs[0]
	if q.PairingID != code.PairingID() || q.Name != "pi-4" || q.Addr == "" || q.WorkerFingerprint != key.Fingerprint() ||
		!q.Since.Equal(h.clk.Now()) || !q.Expires.Equal(h.clk.Now().Add(PairingTTL)) || q.Resume {
		t.Errorf("request = %+v", q)
	}
	if strings.Contains(h.logs.String(), code.Canonical()[4:]) {
		t.Error("the secret part of the code reached the log")
	}
}

func TestAWorkerWithoutTheFeatureIsNotListedAndCannotBeAdmitted(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w, _ := h.startRequester(t, key, code, "old", false)
	if n := len(h.ps.Requests()); n != 0 {
		t.Fatalf("an old Worker is listed (%d)", n)
	}
	_, err := h.ps.Admit(context.Background(), AdmitRequest{UserID: "u-admin", PairingID: code.PairingID(), Fingerprint: fingerprintText(t, key), Approve: true})
	_ = wantPairingCode(t, err, proto.CodeNotFound)
	// Its code still pairs.
	if _, err := h.claim("u-alice", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil || w.res.Admitted {
		t.Fatalf("worker = %+v, %v", w.res, w.err)
	}
}

func TestAdmittingARequestEnrollsExactlyThePresentedKeyApproved(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w, _ := h.startRequester(t, key, code, "pi", true)

	res, err := h.ps.Admit(context.Background(), AdmitRequest{
		UserID: "u-admin", PairingID: strings.ToLower(code.PairingID()), Fingerprint: strings.ToUpper(fingerprintText(t, key)),
		Name: "kitchen", Approve: true, Actor: "u-admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil || !w.res.Admitted {
		t.Fatalf("worker = %v, admitted %v", w.err, w.res != nil && w.res.Admitted)
	}
	if !bytes.Equal(w.res.Pin.Pub, h.svc.Identity().Public()) {
		t.Error("the Worker did not pin the Hub root")
	}
	row, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != res.WorkerID || row.OwnerID != "u-admin" || row.Name != "kitchen" || row.Pending || row.ApprovedBy != "u-admin" || row.ApprovedAt == 0 {
		t.Errorf("worker row = %+v", row)
	}
	welcome := decodeWelcome(t, w.res)
	if welcome.WorkerID != row.ID || welcome.Pending {
		t.Errorf("welcome = %+v", welcome)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: AuditPairAdmit})
	if err != nil || len(page.Events) != 1 || page.Events[0].Outcome != "approved" || page.Events[0].Target != row.ID {
		t.Errorf("audit = %+v err=%v", page.Events, err)
	}
	if len(h.ps.Requests()) != 0 || h.ps.Pending() != 0 {
		t.Error("the request is still listed")
	}
	// The code of an admitted pairing is used: it does not pair another time.
	if _, err := h.claim("u-alice", code.String(), ""); err == nil {
		t.Fatal("the code paired after the admission")
	}
}

func TestAnAdmissionThatNamesAnotherKeyAdmitsNothing(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w, _ := h.startRequester(t, key, code, "pi", true)
	other := newKey(t)
	_, err := h.ps.Admit(context.Background(), AdmitRequest{UserID: "u-admin", PairingID: code.PairingID(), Fingerprint: fingerprintText(t, other), Approve: true})
	_ = wantPairingCode(t, err, proto.CodeConflict)
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); err == nil {
		t.Fatal("a Worker was created")
	}
	if len(h.ps.Requests()) != 1 {
		t.Fatal("the request is gone after a refused admission")
	}
	// Unknown id.
	_, err = h.ps.Admit(context.Background(), AdmitRequest{UserID: "u-admin", PairingID: "ZZZZ", Fingerprint: fingerprintText(t, key), Approve: true})
	_ = wantPairingCode(t, err, proto.CodeNotFound)
	_ = w
}

func TestRejectingARequestTellsTheWorkerAndHidesItsKeyForALifetime(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w, _ := h.startRequester(t, key, code, "pi", true)
	if err := h.ps.Reject(context.Background(), RejectRequest{PairingID: code.PairingID(), Fingerprint: fingerprintText(t, key), Actor: "u-admin"}); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if transport.PeerReason(w.err) != "rejected" {
		t.Fatalf("the Worker saw %v", w.err)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: AuditPairingReject})
	if err != nil || len(page.Events) != 1 || page.Events[0].Target != code.PairingID() {
		t.Errorf("audit = %+v err=%v", page.Events, err)
	}
	// The same key comes back with a new code: not listed, but its code works.
	code2 := identity.NewPairingCode()
	w2, _ := h.startRequester(t, key, code2, "pi", true)
	if n := len(h.ps.Requests()); n != 0 {
		t.Fatalf("a rejected key is listed again at once (%d)", n)
	}
	if _, err := h.claim("u-alice", code2.String(), ""); err != nil {
		t.Fatal(err)
	}
	w2.wait(t)
	if w2.err != nil {
		t.Fatal(w2.err)
	}
}

func TestARejectedKeyIsListedAgainAfterOneLifetime(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w, _ := h.startRequester(t, key, code, "pi", true)
	if err := h.ps.Reject(context.Background(), RejectRequest{PairingID: code.PairingID(), Fingerprint: fingerprintText(t, key)}); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	h.clk.Advance(PairingTTL + time.Second)
	h.ps.Sweep(context.Background())
	_, _ = h.startRequester(t, key, identity.NewPairingCode(), "pi", true)
	if n := len(h.ps.Requests()); n != 1 {
		t.Fatalf("requests = %d after the hiding ended", n)
	}
}

func TestARequestBeingClaimedIsNotListedAndCannotBeAdmitted(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	_, _ = h.startRequester(t, key, code, "pi", true)
	h.ps.mu.Lock()
	p := h.ps.live[code.PairingID()]
	p.cur = &claim{done: make(chan claimOutcome, 1)} // a claim in flight
	h.ps.mu.Unlock()
	if len(h.ps.Requests()) != 0 {
		t.Fatal("a request in a claim is listed")
	}
	_, err := h.ps.Admit(context.Background(), AdmitRequest{UserID: "u-admin", PairingID: code.PairingID(), Fingerprint: fingerprintText(t, key)})
	_ = wantPairingCode(t, err, proto.CodeConflict)
	h.ps.mu.Lock()
	p.cur = nil
	h.ps.mu.Unlock()
}

func TestAdmitDigestsAreUniqueAndMarked(t *testing.T) {
	a, b := admitDigest("ABCD", []byte("k")), admitDigest("ABCD", []byte("k"))
	if a == b || !isAdmitDigest(a) || isAdmitDigest(strings.Repeat("a", 64)) {
		t.Fatalf("%s %s", a, b)
	}
}
