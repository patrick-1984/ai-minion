package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func (w *hubWorker) stored(t *testing.T) Enrollment {
	t.Helper()
	e, ok, err := w.enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment = %+v ok %v err %v", e, ok, err)
	}
	return e
}

func certHash32(c *identity.HubCert) []byte { h := sha256.Sum256(c.Bytes()); return h[:] }

func enrolledAgainst(t *testing.T, serial uint64) (*stubHub, *hubWorker) {
	t.Helper()
	h := newStubHub(t)
	h.reissue(serial)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)
	return h, w
}

func TestConnectCommitsPinRatchetAtomicallyBeforeServing(t *testing.T) {
	h, w := enrolledAgainst(t, 5)
	e := w.stored(t)
	if e.Ratchet.MaxSerial != 5 || !bytes.Equal(e.Ratchet.CertSHA256, certHash32(h.hub.Cert)) || !bytes.Equal(e.Pin.Pub, h.key.Public()) {
		t.Fatalf("stored ratchet = %+v", e.Ratchet)
	}
}

func TestRiseOfTheSerialAloneIsCommittedOnReconnect(t *testing.T) {
	h, w := enrolledAgainst(t, 5)
	h.reissue(9)
	h.dropSessions()
	eventually(t, func() bool { return w.stored(t).Ratchet.MaxSerial == 9 })
	if e := w.stored(t); !bytes.Equal(e.Ratchet.CertSHA256, certHash32(h.hub.Cert)) {
		t.Fatalf("hash not updated: %x", e.Ratchet.CertSHA256)
	}
	w.waitState(t, ConnConnected)
}

func TestRollbackAndEqualSerialWithOtherBytesAreRefusedWithTheOperatorCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial uint64
		reason string
	}{
		{"older serial", 3, "cert_serial_too_low"},
		{"equal serial, other bytes", 5, "cert_serial_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, w := enrolledAgainst(t, 5)
			before := w.stored(t)
			hellos := h.helloCount()
			h.reissue(tc.serial) // a new operational key: other bytes
			h.dropSessions()
			eventually(t, func() bool {
				return strings.Contains(w.conn.Status().LastError, tc.reason)
			})
			st := w.conn.Status()
			for _, want := range []string{"serial " + strconv.FormatUint(tc.serial, 10), "serial 5", "minion hub identity reissue --serial-floor 5"} {
				if !strings.Contains(st.LastError, want) {
					t.Errorf("status %q lacks %q", st.LastError, want)
				}
			}
			if !strings.Contains(w.logs.String(), "command=\"minion hub identity reissue --serial-floor 5\"") {
				t.Errorf("no operator log line:\n%s", w.logs.String())
			}
			if st.State == ConnConnected {
				t.Fatal("connected to a rolled-back Hub")
			}
			// The Hub never saw sig_worker: its handshake did not complete.
			if h.helloCount() != hellos {
				t.Fatal("the Worker sent sig_worker to a refused Hub")
			}
			if got := w.stored(t); got.Ratchet.MaxSerial != 5 || !bytes.Equal(got.Ratchet.CertSHA256, before.Ratchet.CertSHA256) {
				t.Fatalf("a refused certificate changed the ratchet: %+v", got.Ratchet)
			}
			// The documented repair: a higher serial connects again.
			h.reissue(6)
			eventually(t, func() bool { return w.conn.Status().State == ConnConnected && w.stored(t).Ratchet.MaxSerial == 6 })
		})
	}
}

func TestCertificateWindowIsEnforcedByTheWorker(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		nb, na time.Time
		ok     bool
		reason string
	}{
		{"expired", now.Add(-48 * time.Hour), now.Add(-time.Minute), false, "cert_expired"},
		{"not yet valid beyond the tolerance", now.Add(2 * time.Hour), now.Add(48 * time.Hour), false, "cert_not_yet_valid"},
		{"inside the one hour tolerance", now.Add(30 * time.Minute), now.Add(48 * time.Hour), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStubHub(t)
			h.mu.Lock()
			h.hub = hubcert.New(t, hubcert.WithWindow(tc.nb.UnixMilli(), tc.na.UnixMilli()))
			h.key = h.hub.Root
			h.mu.Unlock()
			w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
			h.enrol(w.key.Public())
			if tc.ok {
				w.waitState(t, ConnConnected)
				return
			}
			eventually(t, func() bool { return strings.Contains(w.conn.Status().LastError, tc.reason) })
			if w.conn.Status().State == ConnConnected {
				t.Fatal("connected with a certificate outside its window")
			}
		})
	}
}

func TestRevokeIsAcceptedOnlyFromTheOperationalKeyOfThisConnection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signer func(h *stubHub, oldOp *identity.Key) *identity.Key
		wiped  bool
	}{
		{"operational key of this connection", func(h *stubHub, _ *identity.Key) *identity.Key { return nil }, true},
		{"root key", func(h *stubHub, _ *identity.Key) *identity.Key { return h.key }, false},
		{"previous operational key", func(h *stubHub, old *identity.Key) *identity.Key { return old }, false},
		{"another key", func(*stubHub, *identity.Key) *identity.Key { k, _ := identity.GenerateKey(); return k }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStubHub(t)
			oldOp := h.hub.Op
			h.reissue(2) // the current operational key is not the first one
			w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
			h.enrol(w.key.Public())
			w.waitState(t, ConnConnected)
			h.mu.Lock()
			h.revokeKey = tc.signer(h, oldOp)
			h.mu.Unlock()
			h.tombstone(w.key.Public())
			h.dropSessions()
			if tc.wiped {
				eventually(t, func() bool { _, ok, _ := w.enr.Load(context.Background()); return !ok })
				return
			}
			eventually(t, func() bool { return strings.Contains(w.logs.String(), "ignored a revoke") })
			time.Sleep(50 * time.Millisecond)
			if _, ok, err := w.enr.Load(context.Background()); err != nil || !ok {
				t.Fatalf("a revoke by the wrong key deleted the pairing: ok %v err %v", ok, err)
			}
		})
	}
}

func TestARootTransitionResetsTheSerialScope(t *testing.T) {
	keys, chain := hubChain(t, 1)
	// The old root's scope is far ahead; the new root starts again at 0.
	hub := newChainHub(t, keys[1], chain)
	old := Enrollment{Pin: identity.Pin{Pub: keys[0].Public()},
		Ratchet: transport.Ratchet{Root: keys[0].Public(), MaxSerial: 500, CertSHA256: bytes.Repeat([]byte{7}, 32)}}
	w := startChainWorkerWith(t, hub.srv.URL, old, nil)
	w.waitState(t, ConnConnected)
	e := w.storedEnrollment(t)
	requirePin(t, e.Pin, keys[1], 1)
	if e.Ratchet.MaxSerial != 1 {
		t.Fatalf("scope after the transition: max_serial %d, want 1", e.Ratchet.MaxSerial)
	}
}

func TestACandidateRootThatFailsLaterStepsLeavesTheRatchetUntouched(t *testing.T) {
	keys, chain := hubChain(t, 1)
	hub := newChainHub(t, keys[1], chain)
	// The Hub presents the right chain but its certificate is for another
	// root: the candidate is rejected and nothing is stored.
	other := hubcert.New(t)
	h, _ := certHubs.Load(string(keys[1].Public()))
	real := h.(*hubcert.Hub)
	bad := *real
	bad.Cert = other.Cert
	bad.Op = other.Op
	certHubs.Store(string(keys[1].Public()), &bad)
	old := Enrollment{Pin: identity.Pin{Pub: keys[0].Public()},
		Ratchet: transport.Ratchet{Root: keys[0].Public(), MaxSerial: 9, CertSHA256: bytes.Repeat([]byte{7}, 32)}}
	w := startChainWorkerWith(t, hub.srv.URL, old, nil)
	eventually(t, func() bool { return hub.connects.Load() >= 2 })
	e := w.storedEnrollment(t)
	if !bytes.Equal(e.Pin.Pub, keys[0].Public()) || e.Ratchet.MaxSerial != 9 {
		t.Fatalf("a rejected candidate changed the enrollment: pin %s ratchet %+v", key2fp(e.Pin.Pub), e.Ratchet)
	}
}

// The connect mode is not a pairing: it neither reads nor changes the
// pair_resume record, and the record's root and serial never scope its ratchet.
func TestConnectModeIgnoresThePairResumeRecord(t *testing.T) {
	h := newStubHub(t)
	h.reissue(4)
	w := newHubWorker(t, h.url(), identity.Pin{Pub: h.key.Public()})
	rec := store.PairResume{Code: "MN-AAAA-BBBB", PairingID: "AAAA", ExpiresAt: time.Now().Add(time.Hour),
		RootPub: bytes.Repeat([]byte{5}, 32), Serial: 900, CertSHA256: bytes.Repeat([]byte{6}, 32)}
	if err := w.st.SavePairResume(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)
	if e := w.stored(t); e.Ratchet.MaxSerial != 4 {
		t.Fatalf("the record's serial scoped the connect ratchet: %+v", e.Ratchet)
	}
	got, err := w.st.LoadPairResume(context.Background())
	if err != nil || got.Code != rec.Code || got.Serial != 900 {
		t.Fatalf("connect mode touched the record: %+v %v", got, err)
	}
}
