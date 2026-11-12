package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto/tls"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// --- a code whose confirmation was exposed is never reused -----------------

// pairExposedStep sends the CPace confirmation (the dialer reports it through
// OnExposed) and then fails with err: whatever the peer answered proves
// nothing.
func pairExposedStep(err error) pairStep {
	return func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		p.OnExposed()
		return PairResult{}, err
	}
}

func TestExposedCodeIsRetiredAfterAnUnauthenticatedErrorAndTheNextCodeWaitsForTheBackoff(t *testing.T) {
	errs := map[string]error{
		"pairing_code_invalid":      proto.NewError(proto.CodePairingCodeInvalid, "the pairing code is wrong"),
		"pairing_attempts_exceeded": proto.NewError(proto.CodePairingAttemptsExceeded, "dropped"),
		"rate_limited":              proto.NewError(proto.CodeRateLimited, "slow down"),
		"disconnect":                io.ErrUnexpectedEOF,
	}
	for name, e := range errs {
		t.Run(name, func(t *testing.T) {
			f := newPairFixture(t, pairExposedStep(e), pairWaitStep(nil))
			f.codeSeq("K7Q4XY12", "B2C3D4E5")
			f.run(f.pairer())
			eventually(t, func() bool { return f.dial.nCalls() == 1 && len(f.shown()) == 1 })
			// The second code is not even attempted before the retry delay.
			eventually(t, func() bool { return len(f.delays()) == 1 })
			time.Sleep(50 * time.Millisecond)
			if f.dial.nCalls() != 1 {
				t.Fatal("a code that was exposed must be followed by a delay, not an immediate retry")
			}
			f.backoffStep()
			eventually(t, func() bool { return f.dial.nCalls() == 2 && len(f.shown()) == 2 })
			if got := f.dial.call(1).Code.String(); got != "MN-B2C3-D4E5" {
				t.Fatalf("the retry used %s, want the fresh code", got)
			}
			if f.dial.call(1).Code.PairingID() == f.dial.call(0).Code.PairingID() {
				t.Fatal("the fresh code must have another pairing id")
			}
		})
	}
}

func TestExposureNeverResetsTheBackoffForAnUnauthenticatedErrorOrDisconnect(t *testing.T) {
	// An interceptor that answers every guess with pairing_code_invalid gets
	// one guess per code and a delay that grows with each of them.
	invalid := pairExposedStep(proto.NewError(proto.CodePairingCodeInvalid, "wrong"))
	f := newPairFixture(t, invalid, invalid, invalid, invalid, pairWaitStep(nil))
	f.run(f.pairer())
	got := f.waitDelays(4)
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 6 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("delays = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want strictly growing %v", got, want)
		}
	}
}

func TestExposedCodeConnectionHeldForALongTimeDoesNotRestartTheBackoff(t *testing.T) {
	var f *pairFixture
	held := func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		f.clk.Advance(pairHeldReset) // the user took a while to type the code
		p.OnExposed()
		return PairResult{}, proto.NewError(proto.CodePairingCodeInvalid, "typo")
	}
	invalid := pairExposedStep(proto.NewError(proto.CodePairingCodeInvalid, "wrong"))
	f = newPairFixture(t, invalid, invalid, held, pairWaitStep(nil))
	f.run(f.pairer())
	got := f.waitDelays(3)
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3000 * time.Millisecond}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("delays = %v, want %v (a long hold must not restart the backoff)", got, want)
		}
	}
}

func TestUnexposedPairingCodeInvalidKeepsTheCode(t *testing.T) {
	// Nothing was exposed (the Hub refused before the confirmation step), so
	// the same code is still safe and stays until it is too old.
	f := newPairFixture(t, pairWaitStep(proto.NewError(proto.CodePairingCodeInvalid, "unknown id")), pairWaitStep(nil))
	f.codeSeq("K7Q4XY12", "B2C3D4E5")
	f.run(f.pairer())
	f.backoffStep()
	eventually(t, func() bool { return f.dial.nCalls() == 2 })
	if f.dial.call(1).Code.String() != "MN-K7Q4-XY12" {
		t.Fatal("an unexposed code is kept")
	}
}

// A hostile Hub on the production transport. It runs CPace with a guess and
// answers with the frame an honest Hub sends for a wrong code. A Worker that
// retried the same code after that would let it guess once per connection,
// without ever reaching the legitimate Hub's attempt limit.
func TestAdversarialHubGuessingTheCodeGetsOneGuessPerCodeAndNeverALongRunOfConnections(t *testing.T) {
	h := newStubPairHub(t)
	h.guess = []byte("not the code")
	clk := newConnClock()
	hp := newHubPairer(t, h.url(), func(c *PairerConfig) {
		c.Clock = clk
		c.Backoff = &transport.Backoff{Rand: func() float64 { return 0.5 }}
	})
	r := hp.start(t)

	firstCode := hp.code(t, 1)
	eventually(t, func() bool { return h.connects.Load() == 1 && clk.pending() > 0 })
	// The connection ended with pairing_code_invalid after the Worker's
	// confirmation. The Worker retired the code and waits for the retry delay.
	time.Sleep(100 * time.Millisecond)
	if h.connects.Load() != 1 {
		t.Fatalf("the Worker connected %d times before any delay passed", h.connects.Load())
	}
	// Every retry delay that passes gives the hostile Hub one more guess, each
	// against a different code, and each delay is longer than the one before.
	var delays []time.Duration
	codes := []string{firstCode}
	seen := 0
	for i := 2; i <= 4; i++ {
		var d time.Duration
		eventually(t, func() bool {
			rec := clk.recorded()
			for ; seen < len(rec); seen++ {
				if rec[seen] <= time.Minute {
					d = rec[seen]
					seen++
					return true
				}
			}
			return false
		})
		delays = append(delays, d)
		clk.Advance(d)
		codes = append(codes, hp.code(t, i))
		n := int32(i)
		eventually(t, func() bool { return h.connects.Load() == n })
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] <= delays[i-1] {
			t.Fatalf("delays = %v, want strictly growing", delays)
		}
	}
	uniq := map[string]bool{}
	for _, c := range codes {
		uniq[c] = true
	}
	if len(uniq) != len(codes) {
		t.Fatalf("a code was shown twice: %v", codes)
	}
	ids := h.allIDs()
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("two consecutive connections used the pairing id %s: %v", ids[i], ids)
		}
	}
	select {
	case <-r.done:
		t.Fatalf("the run ended: %v", r.err)
	default:
	}
	if _, ok, _ := hp.enr.Load(context.Background()); ok {
		t.Fatal("nothing may be stored")
	}
	if !strings.Contains(hp.out.String(), "The Hub did not accept the code. A new code replaces it.") {
		t.Fatalf("console:\n%s", hp.out.String())
	}
}

// --- revoking the old identity ---------------------------------------------

// retireHub is an in-process stand-in for the Hub, for the failure modes the
// real Hub never produces (silent, unsupported, offline). It serves the connect
// mode like the real one (welcome for an enrolled key, a signed revoke for a
// tombstoned key, not_enrolled for an unknown one) and answers worker_retire on
// a call stream by removing the key's enrollment and leaving a tombstone
// (AD-9). The contract with the REAL Hub is tested in
// test/e2e/worker_retire_test.go (go test -tags e2e ./test/e2e).
type retireHub struct {
	t *testing.T
	// key is the root (the pin); hub holds the operational key and certificate.
	key  *identity.Key
	hub  *hubcert.Hub
	cert tls.Certificate

	mu          sync.Mutex
	srv         *httptest.Server
	addr        string
	known       map[string]bool
	tomb        map[string]bool
	unsupported bool
	silent      bool          // reads worker_retire and never answers
	wrongSigner *identity.Key // signs revokes with another key
	retired     int

	connects atomic.Int32
	live     atomic.Int32
}

func newRetireHub(t *testing.T) *retireHub {
	t.Helper()
	hub := hubcert.New(t)
	h := &retireHub{t: t, key: hub.Root, hub: hub, cert: wrongNameCert(t), known: map[string]bool{}, tomb: map[string]bool{}}
	h.start("127.0.0.1:0")
	t.Cleanup(h.stop)
	return h
}

func (h *retireHub) start(addr string) {
	h.t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		h.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+transport.ConnectPath, h.serve)
	srv := httptest.NewUnstartedServer(mux)
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	h.mu.Lock()
	h.srv, h.addr = srv, l.Addr().String()
	h.mu.Unlock()
}

func (h *retireHub) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv = nil
	h.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (h *retireHub) url() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "https://" + h.addr
}

func (h *retireHub) pin() identity.Pin { return identity.Pin{Pub: h.key.Public()} }

func (h *retireHub) enrol(pub []byte) {
	h.mu.Lock()
	h.known[string(pub)] = true
	h.mu.Unlock()
}

func (h *retireHub) state(pub []byte) (known, tombstoned bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.known[string(pub)], h.tomb[string(pub)]
}

func (h *retireHub) setSilent(v bool) {
	h.mu.Lock()
	h.silent = v
	h.mu.Unlock()
}

func (h *retireHub) setUnsupported(v bool) {
	h.mu.Lock()
	h.unsupported = v
	h.mu.Unlock()
}

func (h *retireHub) serve(w http.ResponseWriter, r *http.Request) {
	h.connects.Add(1)
	conn, err := transport.Accept(w, r)
	if err != nil {
		return
	}
	h.mu.Lock()
	hc := h.hub.Config()
	h.mu.Unlock()
	hc.Timeout = 10 * time.Second
	res, err := transport.HubHandshake(conn, hc)
	if err != nil {
		return
	}
	write := func(v any) error { return proto.WriteFrame(res.Channel, proto.StreamFrameLimit, v) }
	pub := []byte(res.WorkerPub)
	known, tomb := h.state(pub)
	switch {
	case tomb:
		signer := hc.Key
		h.mu.Lock()
		if h.wrongSigner != nil {
			signer = h.wrongSigner
		}
		h.mu.Unlock()
		sig, _ := signer.Sign(identity.PurposeRevoke, res.TranscriptHash[:])
		_ = write(map[string]any{"type": proto.TypeRevoke, "sig": identity.B64(sig)})
		_ = res.Channel.Close()
	case !known:
		_ = write(map[string]string{"type": proto.TypeNotEnrolled})
		_ = res.Channel.Close()
	default:
		if err := write(map[string]any{
			"type": proto.TypeWelcome, "worker_id": "w-" + res.Worker.Name, "name": res.Worker.Name, "owner": "alice",
			"kind": "private", "policy": map[string]bool{"preview_only": false}, "proto_min": "1.0", "proto_max": "1.0",
		}); err != nil {
			return
		}
		sess, err := transport.NewSession(res.Channel, transport.RoleHub, transport.SessionOptions{})
		if err != nil {
			return
		}
		h.live.Add(1)
		defer h.live.Add(-1)
		go h.serveCalls(sess, pub)
		<-sess.Done()
	}
}

func (h *retireHub) serveCalls(sess *transport.Session, pub []byte) {
	for {
		st, err := sess.Accept(context.Background())
		if err != nil {
			return
		}
		body, err := st.ReadFrame()
		if err != nil || !strings.Contains(string(body), RetireOp) {
			st.Reset()
			continue
		}
		h.mu.Lock()
		unsupported, silent := h.unsupported, h.silent
		if silent {
			h.mu.Unlock()
			continue // hold the stream open and never answer
		}
		if !unsupported {
			h.known[string(pub)] = false
			h.tomb[string(pub)] = true
			h.retired++
		}
		h.mu.Unlock()
		if unsupported {
			_ = st.WriteFrame(proto.NewError(proto.CodeUnsupportedOp, "worker_retire is not implemented").Frame())
		} else {
			_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}})
		}
		_ = st.Close()
	}
}

func fastBackoff() *transport.Backoff {
	return &transport.Backoff{Initial: 5 * time.Millisecond, Max: 30 * time.Millisecond}
}

func newRetirer(t *testing.T, store *RetiredStore, hubURL string, opts ...func(*RetirerConfig)) *Retirer {
	t.Helper()
	cfg := RetirerConfig{
		Store: store, Dialer: NewHubDialer(), Name: "pi", Backoff: fastBackoff(), Poll: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}
	r, err := NewRetirer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func retiredFor(t *testing.T, h *retireHub, url string) (RetiredIdentity, *identity.Key) {
	t.Helper()
	old, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return RetiredIdentity{Key: old, HubURL: url, Pin: h.pin(), WorkerID: "w-old", RetiredAt: time.Now()}, old
}

func TestRetirerRevokesTheOldEnrollmentAndTheOldKeyIsRejectedOnTheNextConnect(t *testing.T) {
	h := newRetireHub(t)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}

	remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background())
	if err != nil || remaining != 0 {
		t.Fatalf("RunOnce = %d, %v", remaining, err)
	}
	if known, tomb := h.state(old.Public()); known || !tomb {
		t.Fatalf("the Hub must have removed the old enrollment: known=%v tombstoned=%v", known, tomb)
	}
	if p, _ := rs.List(); len(p) != 0 {
		t.Fatalf("the confirmed record must be deleted: %d", len(p))
	}
	// The old key (a retained copy, say) is now refused: its connect is answered
	// with a revoke that the Hub operational key signed for that connection.
	att, err := NewHubDialer().Connect(context.Background(), ConnectParams{
		HubURL: h.url(), Key: old, Name: "copy", Pin: h.pin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if att.Reply.Type != proto.TypeRevoke || att.Session != nil {
		t.Fatalf("the old key must be rejected, got %q", att.Reply.Type)
	}
	if err := identity.Verify(att.OpPub, identity.PurposeRevoke, att.TranscriptHash, att.Reply.RevokeSig); err != nil {
		t.Fatalf("the revoke must be signed by the Hub: %v", err)
	}
}

func TestPendingRevocationSurvivesAnOfflineHubAndAFreshProcess(t *testing.T) {
	h := newRetireHub(t)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	dir := t.TempDir()
	if err := NewRetiredStore(dir).Add(rec); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	addr := h.addr
	h.mu.Unlock()
	h.stop() // the Hub is offline

	remaining, err := newRetirer(t, NewRetiredStore(dir), h.url(), func(c *RetirerConfig) { c.Dialer = hubDialerWithTimeout(time.Second) }).RunOnce(context.Background())
	if err != nil || remaining != 1 {
		t.Fatalf("RunOnce = %d, %v; the revocation must stay pending", remaining, err)
	}
	// A new store value over the same directory is a restarted process: the
	// record, key included, is still there.
	pending, err := NewRetiredStore(dir).List()
	if err != nil || len(pending) != 1 || string(pending[0].Key.Public()) != string(old.Public()) {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	if known, tomb := h.state(old.Public()); !known || tomb {
		t.Fatal("the old identity is still enrolled while the Hub is away")
	}

	// The Hub comes back; the running retirer (the service) finishes the job.
	h.start(addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); newRetirer(t, NewRetiredStore(dir), h.url()).Run(ctx) }()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { cancel(); <-done }) }
	defer stop()
	eventually(t, func() bool { _, tomb := h.state(old.Public()); return tomb })
	// List is empty as soon as RetiredStore.Remove has deleted the metadata,
	// which it does first on purpose (a crash never leaves a record without its
	// key); the retirer is then still about to delete the key file. Only the
	// end of the retirer proves that Remove has returned: Run checks its
	// context between passes, never inside Remove.
	eventually(t, func() bool { p, _ := NewRetiredStore(dir).List(); return len(p) == 0 })
	stop()
	if _, err := os.Stat(filepath.Join(dir, retiredDirName, rec.Fingerprint()+".key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old key file must be deleted with the record: %v", err)
	}
}

func hubDialerWithTimeout(d time.Duration) Dialer {
	return hubDialer{dialTimeout: d, handshakeTimeout: d}
}

func TestPendingRevocationStaysWhenTheHubDoesNotSupportRetiring(t *testing.T) {
	h := newRetireHub(t)
	h.setUnsupported(true)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background())
	if err != nil || remaining != 1 {
		t.Fatalf("RunOnce = %d, %v", remaining, err)
	}
	if p, _ := rs.List(); len(p) != 1 {
		t.Fatal("an unsupported Hub must not clear the record")
	}
	h.setUnsupported(false) // upgraded Hub
	if remaining, _ := newRetirer(t, rs, h.url()).RunOnce(context.Background()); remaining != 0 {
		t.Fatal("the retry against an upgraded Hub must succeed")
	}
	if _, tomb := h.state(old.Public()); !tomb {
		t.Fatal("the old key must be revoked")
	}
}

func TestRetirerTreatsAnAlreadyRevokedOrUnknownKeyAsDone(t *testing.T) {
	h := newRetireHub(t)
	rs := NewRetiredStore(t.TempDir())
	tombed, tk := retiredFor(t, h, h.url())
	h.mu.Lock()
	h.tomb[string(tk.Public())] = true
	h.mu.Unlock()
	unknown, _ := retiredFor(t, h, h.url())
	for _, r := range []RetiredIdentity{tombed, unknown} {
		if err := rs.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if remaining, err := newRetirer(t, rs, h.url()).RunOnce(context.Background()); err != nil || remaining != 0 {
		t.Fatalf("RunOnce = %d, %v", remaining, err)
	}
}

func TestRetirerIgnoresARevokeThatTheHubKeyDidNotSign(t *testing.T) {
	h := newRetireHub(t)
	other, _ := identity.GenerateKey()
	h.wrongSigner = other
	rec, old := retiredFor(t, h, h.url())
	h.mu.Lock()
	h.tomb[string(old.Public())] = true
	h.mu.Unlock()
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := newRetirer(t, rs, h.url()).RunOnce(context.Background()); remaining != 1 {
		t.Fatal("an unauthenticated revoke must not clear the record")
	}
}

func TestRetirerNeverConnectsToAHubThatDoesNotMatchThePin(t *testing.T) {
	h := newRetireHub(t)
	rec, old := retiredFor(t, h, h.url())
	impostor, _ := identity.GenerateKey()
	rec.Pin = identity.Pin{Pub: impostor.Public()}
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := newRetirer(t, rs, h.url()).RunOnce(context.Background()); remaining != 1 {
		t.Fatal("a Hub with another key must not be asked to retire anything")
	}
	if h.retired != 0 {
		t.Fatal("the retire request reached a Hub that is not the pinned one")
	}
}

func TestRetiredStoreKeepsTheSecretKeyPrivate(t *testing.T) {
	h := newRetireHub(t)
	rec, _ := retiredFor(t, h, h.url())
	dir := t.TempDir()
	rs := NewRetiredStore(dir)
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	keyPath, metaPath := rs.paths(rec.Fingerprint())
	for _, p := range []string{keyPath, metaPath} {
		if err := identity.CheckPrivate(p); err != nil {
			t.Fatalf("%s must be private: %v", p, err)
		}
	}
	if err := rs.Add(RetiredIdentity{}); err == nil {
		t.Fatal("an incomplete record must be refused")
	}
}

// --- the enrollment generation is shared between processes ------------------

// twoProcesses opens the state directory twice, as the service and the CLI do.
func twoProcesses(t *testing.T) (service, cli *Enrollments, dir string) {
	t.Helper()
	dir = t.TempDir()
	open := func() *Enrollments {
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return NewEnrollments(st, dir)
	}
	return open(), open(), dir
}

func TestGenerationIsSharedAcrossEnrollmentsOfOneStateDirectory(t *testing.T) {
	svc, cli, _ := twoProcesses(t)
	ctx := context.Background()
	gen, changed := svc.Watch()
	if err := cli.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}, Owner: "bob"}); err != nil {
		t.Fatal(err)
	}
	if !svc.Moved(gen) {
		t.Fatal("the service must see that the CLI committed")
	}
	select {
	case <-changed:
	default:
		t.Fatal("the service's change channel must be closed by a change from another process")
	}
	// Every conditional writer of the service refuses the old generation.
	if err := svc.ApplyWelcomeIf(ctx, gen, HubWelcome{WorkerID: "w-old", Owner: "alice"}); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("ApplyWelcomeIf = %v", err)
	}
	if err := svc.RewritePinIf(ctx, gen, identity.Pin{Pub: make([]byte, 32), Seq: 9}); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("RewritePinIf = %v", err)
	}
	if _, err := svc.WipeIf(ctx, gen); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("WipeIf = %v", err)
	}
	if got, ok, _ := svc.Load(ctx); !ok || got.Owner != "bob" {
		t.Fatalf("the replacement enrollment must survive: %+v %v", got, ok)
	}
	// With the current generation the service may write again.
	gen2, _ := svc.Watch()
	if gen2 == gen {
		t.Fatal("the generation must have moved")
	}
	if err := svc.ApplyWelcomeIf(ctx, gen2, HubWelcome{WorkerID: "w-new", Owner: "bob"}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotIsConsistentWithConcurrentWritersOfAnotherProcess(t *testing.T) {
	svc, cli, _ := twoProcesses(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			owner := "owner-" + string(rune('a'+i%26))
			_ = cli.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}, Owner: owner, WorkerID: owner})
			_, _ = cli.Wipe(ctx)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for i := 0; i < 100; i++ {
		gen, _, enr, ok, err := svc.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok && enr.Owner != enr.WorkerID {
			t.Fatalf("torn snapshot: %+v", enr)
		}
		// A write that is conditional on the snapshot either lands on that very
		// enrollment or is refused because the other process moved on.
		err = svc.ApplyWelcomeIf(ctx, gen, HubWelcome{WorkerID: enr.WorkerID, Owner: enr.Owner})
		if err != nil && !errors.Is(err, ErrEnrollmentChanged) && (ok || !errors.Is(err, store.ErrNotEnrolled)) {
			t.Fatal(err)
		}
	}
}

func TestEnrollmentsWithoutAStateDirectoryKeepTheGenerationInMemory(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := NewEnrollments(st, "")
	gen, _ := e.Watch()
	if err := e.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}}); err != nil {
		t.Fatal(err)
	}
	if !e.Moved(gen) {
		t.Fatal("the generation moves in memory")
	}
}

func TestCorruptGenerationFileIsAnErrorNotAReset(t *testing.T) {
	svc, _, dir := twoProcesses(t)
	if err := os.WriteFile(filepath.Join(dir, enrollmentEpochFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}}); err == nil {
		t.Fatal("a corrupt generation file must not be silently reset")
	}
}

// The scenario of the review: the Worker service holds a connection to the old
// Hub, `minion worker pair --new-code` runs as another process, and the service
// must not stay connected, nor may the old connection's revoke delete the
// replacement enrollment.
func TestRepairingByAnotherProcessDropsTheServicesConnectionAndProtectsTheReplacement(t *testing.T) {
	h := newRetireHub(t)
	svc, cli, dir := twoProcesses(t)
	ctx := context.Background()
	key, err := svc.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Commit(ctx, Enrollment{Pin: h.pin(), WorkerID: "w-old", Owner: "alice", HubURL: h.url()}); err != nil {
		t.Fatal(err)
	}
	h.enrol(key.Public())

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	conn, err := NewConnector(ConnectorConfig{
		HubURL: h.url(), Name: "pi", Key: svc.LoadKey, Enrollment: svc, Policy: NewPolicy(st), Dialer: NewHubDialer(),
		Backoff: fastBackoff(), IdlePoll: 20 * time.Millisecond, EpochPoll: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, func() bool { return conn.Status().State == ConnConnected })
	oldGen, _ := svc.Watch()

	// The CLI process: its own Enrollments value over the same directory.
	if _, err := RenewWorkerIdentity(ctx, cli); err != nil {
		t.Fatal(err)
	}
	// The running service drops the old Hub's connection and goes idle.
	eventually(t, func() bool { return conn.Status().State != ConnConnected })
	eventually(t, func() bool { return h.live.Load() == 0 })
	eventually(t, func() bool { st := conn.Status(); return st.State == ConnIdle && st.Reason == "not enrolled" })

	// The CLI pairs again (another owner). Whatever the old connection still
	// had in flight is refused: its revoke cannot delete the replacement.
	if err := cli.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: make([]byte, 32), Seq: 1}, WorkerID: "w-new", Owner: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WipeIf(ctx, oldGen); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("a stale revoke must be refused, got %v", err)
	}
	if got, ok, _ := svc.Load(ctx); !ok || got.Owner != "bob" {
		t.Fatalf("the replacement was deleted: %+v %v", got, ok)
	}

	// The old identity is revoked at the old Hub from the pending record.
	pending, _ := cli.RetiredStore().List()
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	if remaining, err := newRetirer(t, cli.RetiredStore(), h.url()).RunOnce(ctx); err != nil || remaining != 0 {
		t.Fatalf("RunOnce = %d, %v", remaining, err)
	}
	if _, tomb := h.state(key.Public()); !tomb {
		t.Fatal("the old key must be revoked at the old Hub")
	}
	_ = dir
}

func TestEnrollmentLockExcludesAnotherHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, enrollmentLockFile)
	unlock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); err == nil {
		t.Fatal("a second holder must wait")
	}
	unlock()
	unlock2, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatalf("the lock must be free after unlock: %v", err)
	}
	unlock2()
}

// --- stopping the retirer ---------------------------------------------------

// waitForAdmitted waits until the Hub holds an admitted connection.
func waitForAdmitted(t *testing.T, h *retireHub) {
	t.Helper()
	eventually(t, func() bool { return h.live.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // the request is on its way
}

func TestRetirerStopsInTheMiddleOfACallWithinTheStopBound(t *testing.T) {
	h := newRetireHub(t)
	h.setSilent(true)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	r := newRetirer(t, rs, h.url())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan time.Duration, 1)
	go func() {
		r.Run(ctx)
		done <- 0
	}()
	waitForAdmitted(t, h)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(DefaultStopTimeout):
		t.Fatalf("the retirer did not stop within the stop bound %s", DefaultStopTimeout)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the retirer took %s to stop; the call must end with the context, not with its own %s timeout", took, retireCallTimeout)
	}
	if p, _ := rs.List(); len(p) != 1 {
		t.Fatal("an aborted attempt must keep the pending revocation")
	}
	eventually(t, func() bool { return h.live.Load() == 0 }) // the connection was closed
}

func TestRunOnceWithADeadlineReturnsAtTheDeadlineNotAtTheCallTimeout(t *testing.T) {
	h := newRetireHub(t)
	h.setSilent(true)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	remaining, _ := newRetirer(t, rs, h.url()).RunOnce(ctx)
	if took := time.Since(start); took > 3*time.Second || remaining != 1 {
		t.Fatalf("RunOnce returned after %s with %d remaining; it must honour a 400ms deadline", took, remaining)
	}
}

func TestAdmittedButNotRetiredWaitsTheLongRetryFloor(t *testing.T) {
	h := newRetireHub(t)
	h.setUnsupported(true) // the Hub admits the old key but cannot retire it
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	r := newRetirer(t, rs, h.url(), func(c *RetirerConfig) { c.AdmittedRetry = 600 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, func() bool { return h.connects.Load() >= 1 })
	time.Sleep(300 * time.Millisecond) // many fast-backoff periods (5-30 ms)
	if n := h.connects.Load(); n != 1 {
		t.Fatalf("%d connects as the old identity within the floor, want exactly 1", n)
	}
	eventually(t, func() bool { return h.connects.Load() >= 2 }) // it does retry after the floor
}

type pairCountingDialer struct {
	Dialer
	n atomic.Int32
}

func (d *pairCountingDialer) Connect(ctx context.Context, p ConnectParams) (*Attempt, error) {
	d.n.Add(1)
	return d.Dialer.Connect(ctx, p)
}

func TestUnreachableHubIsRetriedWithTheNormalBackoffNotTheLongFloor(t *testing.T) {
	h := newRetireHub(t)
	rec, old := retiredFor(t, h, h.url())
	h.enrol(old.Public())
	rs := NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	h.stop()
	cd := &pairCountingDialer{Dialer: hubDialerWithTimeout(500 * time.Millisecond)}
	r := newRetirer(t, rs, h.url(), func(c *RetirerConfig) {
		c.AdmittedRetry = time.Hour
		c.Dialer = cd
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, func() bool { return cd.n.Load() >= 3 })
	if p, _ := rs.List(); len(p) != 1 {
		t.Fatal("the record must stay")
	}
}
