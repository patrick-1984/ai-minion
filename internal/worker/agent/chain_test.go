package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// These tests are the Worker side of the Hub key rotation (AD-5, Story 4.9),
// on the production path: the handshake (transport.resolvePin) verifies the
// chain the Hub sends, the connector rewrites the stored pin
// (Connector.attempt, Enrollments.RewritePinIf) and only then serves the
// session. The Hub is a stub that sends the chain in its hello; the Worker is
// the real connector with the production dialer over a real store.

// certHubs maps a root public key to its certified Hub, for chainHub.
var certHubs sync.Map

// hubChain returns n+1 Hub keys and the chain of n transitions between them.
func hubChain(t *testing.T, n int) ([]*identity.Key, []identity.Transition) {
	t.Helper()
	keys := make([]*identity.Key, n+1)
	for i := range keys {
		// Every root comes with an operational key and a certificate (protocol
		// 3.0); chainHub finds them by the root's public key.
		h := hubcert.New(t)
		certHubs.Store(string(h.RootPub()), h)
		keys[i] = h.Root
	}
	chain := make([]identity.Transition, n)
	for i := range chain {
		tr, err := identity.NewTransition(keys[i], keys[i+1].Public(), uint64(i+1), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		chain[i] = tr
	}
	return keys, chain
}

func flip(b identity.B64) identity.B64 {
	c := append(identity.B64(nil), b...)
	c[0] ^= 0xff
	return c
}

// chainHub is an in-process Hub that proves one key and sends one transition
// chain in every connect hello, then welcomes the Worker. key and chain can be
// changed while it runs (a rotation, or the Hub fixing a bad chain).
type chainHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	key      *identity.Key
	chain    []identity.Transition
	sessions []*transport.Session

	connects atomic.Int32 // attempts that reached the handler
	welcomes atomic.Int32 // sessions the Hub welcomed
}

func newChainHub(t *testing.T, key *identity.Key, chain []identity.Transition) *chainHub {
	t.Helper()
	h := &chainHub{t: t, key: key, chain: chain}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+transport.ConnectPath, h.serve)
	h.srv = httptest.NewTLSServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *chainHub) set(key *identity.Key, chain []identity.Transition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.key, h.chain = key, chain
}

func (h *chainHub) serve(w http.ResponseWriter, r *http.Request) {
	h.connects.Add(1)
	conn, err := transport.Accept(w, r)
	if err != nil {
		return
	}
	h.mu.Lock()
	key, chain := h.key, h.chain
	h.mu.Unlock()
	ch, ok := certHubs.Load(string(key.Public()))
	if !ok {
		h.t.Error("no certified hub for the chain hub's root")
		return
	}
	hc := ch.(*hubcert.Hub).Config()
	hc.Chain, hc.Timeout = chain, 10*time.Second
	res, err := transport.HubHandshake(conn, hc)
	if err != nil {
		return // the Worker did not accept this Hub
	}
	h.welcomes.Add(1)
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]any{
		"type": proto.TypeWelcome, "worker_id": "w-" + res.Worker.Name, "name": res.Worker.Name, "owner": "alice",
		"kind": "private", "policy": map[string]bool{"preview_only": false},
		"proto_min": "1.0", "proto_max": "1.0",
	}); err != nil {
		return
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		return
	}
	h.mu.Lock()
	h.sessions = append(h.sessions, sess)
	h.mu.Unlock()
	defer func() { _ = sess.Close() }()
	<-sess.Done()
}

// drop ends every Worker session, as a Hub restart does.
func (h *chainHub) drop() {
	h.mu.Lock()
	sessions := h.sessions
	h.sessions = nil
	h.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

// failingPinRepo is an enrollment repo whose pin update (the pin and the
// certificate ratchet are one transaction) can be made to fail.
type failingPinRepo struct {
	EnrollmentRepo
	fail     atomic.Bool
	attempts atomic.Int32
}

var errPinWrite = errors.New("disk full")

func (r *failingPinRepo) UpdatePinRatchet(ctx context.Context, pub ed25519.PublicKey, seq, maxSerial uint64, h []byte) error {
	r.attempts.Add(1)
	if r.fail.Load() {
		return errPinWrite
	}
	return r.EnrollmentRepo.UpdatePinRatchet(ctx, pub, seq, maxSerial, h)
}

// chainWorker is a real connector over a real store, pinned to a Hub key.
type chainWorker struct {
	dir    string
	st     *store.Store
	enr    *Enrollments
	conn   *Connector
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// startChainWorker enrolls a Worker pinned to pin in a fresh state directory
// and runs its connector against hubURL. wrap, when set, replaces the
// enrollment repo the connector writes through.
func startChainWorker(t *testing.T, hubURL string, pin identity.Pin, wrap func(EnrollmentRepo) EnrollmentRepo) *chainWorker {
	t.Helper()
	return startChainWorkerWith(t, hubURL, Enrollment{Pin: pin}, wrap)
}

// startChainWorkerWith is startChainWorker for a given stored enrollment (with
// a certificate ratchet, for example).
func startChainWorkerWith(t *testing.T, hubURL string, stored Enrollment, wrap func(EnrollmentRepo) EnrollmentRepo) *chainWorker {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var repo EnrollmentRepo = st
	if wrap != nil {
		repo = wrap(st)
	}
	enr := NewEnrollments(repo, dir)
	if _, err := enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	conn, err := NewConnector(ConnectorConfig{
		HubURL:     hubURL,
		Name:       "pi",
		Ops:        func() []string { return []string{"sessions_list"} },
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     NewPolicy(st),
		Dialer:     NewHubDialer(),
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll:   20 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(&logBuf{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &chainWorker{dir: dir, st: st, enr: enr, conn: conn, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(w.done); conn.Run(ctx) }()
	t.Cleanup(func() { w.stop(t) })
	return w
}

// stop ends the connector and closes the store; it is safe to call twice.
func (w *chainWorker) stop(t *testing.T) {
	t.Helper()
	w.once.Do(func() {
		w.cancel()
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			t.Error("the connector did not stop")
		}
		_ = w.st.Close()
	})
}

func (w *chainWorker) waitState(t *testing.T, want ConnState) {
	t.Helper()
	eventually(t, func() bool { return w.conn.Status().State == want })
}

// storedEnrollment reads the enrollment straight from the database.
func (w *chainWorker) storedEnrollment(t *testing.T) Enrollment {
	t.Helper()
	enr, ok, err := w.enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("stored enrollment = %+v, %v, %v", enr, ok, err)
	}
	return enr
}

func requirePin(t *testing.T, got identity.Pin, key *identity.Key, seq uint64) {
	t.Helper()
	if !bytes.Equal(got.Pub, key.Public()) || got.Seq != seq {
		t.Fatalf("stored pin = (%s, %d), want (%s, %d)", key2fp(got.Pub), got.Seq, key.Fingerprint(), seq)
	}
}

func key2fp(pub []byte) string {
	fp, err := identity.FingerprintOf(pub)
	if err != nil {
		return "invalid key"
	}
	return fp.String()
}

// AC2: a Worker pinned to an older key, offline across several rotations,
// follows the chain on its next connect, rewrites its pin and connects on the
// new key without any human action.
func TestAWorkerPinnedToAnOlderKeyFollowsTheChainOnTheProductionPath(t *testing.T) {
	keys, chain := hubChain(t, 3)
	for _, tc := range []struct {
		name    string
		pin     identity.Pin
		wantSeq uint64
	}{
		{"pinned to the first key, three rotations", identity.Pin{Pub: keys[0].Public(), Seq: 0}, 3},
		{"pinned after the first rotation, two more", identity.Pin{Pub: keys[1].Public(), Seq: 1}, 3},
		{"already on the current key", identity.Pin{Pub: keys[3].Public(), Seq: 3}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newChainHub(t, keys[3], chain) // the whole chain, as the Hub sends it
			w := startChainWorker(t, hub.srv.URL, tc.pin, nil)
			w.waitState(t, ConnConnected)
			requirePin(t, w.storedEnrollment(t).Pin, keys[3], tc.wantSeq)

			// The pin is in the database, not only in memory: it is still there
			// when the store is opened again.
			w.stop(t)
			st, err := store.Open(w.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			enr, ok, err := NewEnrollments(st, w.dir).Load(context.Background())
			if err != nil || !ok {
				t.Fatalf("reopened enrollment = %+v, %v, %v", enr, ok, err)
			}
			requirePin(t, enr.Pin, keys[3], tc.wantSeq)
		})
	}
}

// AC2 and AC3 over time: the Worker keeps following later rotations while it
// stays running.
func TestARunningWorkerFollowsALaterRotation(t *testing.T) {
	keys, chain := hubChain(t, 2)
	hub := newChainHub(t, keys[1], chain[:1])
	w := startChainWorker(t, hub.srv.URL, identity.Pin{Pub: keys[0].Public()}, nil)
	w.waitState(t, ConnConnected)
	requirePin(t, w.storedEnrollment(t).Pin, keys[1], 1)

	hub.set(keys[2], chain) // rotate again, then drop the Worker's session
	before := hub.welcomes.Load()
	hub.drop()
	eventually(t, func() bool { return hub.welcomes.Load() > before })
	eventually(t, func() bool {
		p := w.storedEnrollment(t).Pin
		return bytes.Equal(p.Pub, keys[2].Public()) && p.Seq == 2
	})
}

// AC3: a chain with a bad signature, or one whose seq decreases, is rejected:
// the Worker does not connect, its stored pin stays byte-identical, and it
// keeps retrying. When the Hub serves the honest chain the same running
// connector connects and moves its pin, with no restart.
func TestAWorkerRejectsABadChainKeepsItsPinAndRecovers(t *testing.T) {
	keys, honest := hubChain(t, 3)
	badSig := append([]identity.Transition(nil), honest...)
	badSig[1].SigByOld = flip(badSig[1].SigByOld)

	// Every link is properly signed, but the last seq goes back from 2 to 1.
	backwards := append([]identity.Transition(nil), honest[:2]...)
	last, err := identity.NewTransition(keys[2], keys[3].Public(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	backwards = append(backwards, last)

	for _, tc := range []struct {
		name  string
		chain []identity.Transition
	}{
		{"bad signature", badSig},
		{"decreasing seq", backwards},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newChainHub(t, keys[3], tc.chain)
			pin := identity.Pin{Pub: keys[0].Public(), Seq: 0}
			w := startChainWorker(t, hub.srv.URL, pin, nil)

			w.waitState(t, ConnBackoff)
			eventually(t, func() bool { return w.conn.Status().Attempts >= 3 && hub.connects.Load() >= 3 })
			if st := w.conn.Status().State; st == ConnConnected {
				t.Fatal("the Worker connected to a Hub whose chain does not verify")
			}
			if hub.welcomes.Load() != 0 {
				t.Fatalf("the Hub completed %d handshakes with a Worker that must reject its chain", hub.welcomes.Load())
			}
			enr := w.storedEnrollment(t)
			if !bytes.Equal(enr.Pin.Pub, pin.Pub) || enr.Pin.Seq != pin.Seq {
				t.Fatalf("the pin moved to (%s, %d)", key2fp(enr.Pin.Pub), enr.Pin.Seq)
			}
			if enr.WorkerID != "" {
				t.Fatalf("the Worker applied a welcome from a Hub it rejected: %+v", enr)
			}

			// The Hub serves the honest chain: the running connector recovers.
			hub.set(keys[3], honest)
			w.waitState(t, ConnConnected)
			requirePin(t, w.storedEnrollment(t).Pin, keys[3], 3)
		})
	}
}

// AC3: when the new pin cannot be stored, the connector keeps the old pin and
// does not serve the session, whatever the Hub proved. Once the store works
// again the same connector moves the pin and connects.
func TestAWorkerThatCannotStoreTheNewPinDoesNotServeTheSession(t *testing.T) {
	keys, chain := hubChain(t, 2)
	hub := newChainHub(t, keys[2], chain)
	var repo *failingPinRepo
	pin := identity.Pin{Pub: keys[0].Public(), Seq: 0}
	w := startChainWorker(t, hub.srv.URL, pin, func(r EnrollmentRepo) EnrollmentRepo {
		repo = &failingPinRepo{EnrollmentRepo: r}
		repo.fail.Store(true)
		return repo
	})

	eventually(t, func() bool { return repo.attempts.Load() >= 3 })
	if st := w.conn.Status().State; st == ConnConnected {
		t.Fatal("the Worker served a session although it could not store the new pin")
	}
	enr := w.storedEnrollment(t)
	if !bytes.Equal(enr.Pin.Pub, pin.Pub) || enr.Pin.Seq != pin.Seq {
		t.Fatalf("the pin moved to (%s, %d) although the write failed", key2fp(enr.Pin.Pub), enr.Pin.Seq)
	}
	if enr.WorkerID != "" {
		t.Fatalf("the welcome was applied although the pin was not stored: %+v", enr)
	}

	repo.fail.Store(false)
	w.waitState(t, ConnConnected)
	requirePin(t, w.storedEnrollment(t).Pin, keys[2], 2)
}
