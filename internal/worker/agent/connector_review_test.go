package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// --- a stalled dial must not be the last attempt -----------------------------

// stallProxy is a TCP listener in front of a Hub. It swallows the first
// `stall` connections (accepted, then silent: a proxy that stalls the TLS
// handshake) and forwards every later one to target.
type stallProxy struct {
	l      net.Listener
	target string
	stall  int32
	seen   atomic.Int32
	mu     sync.Mutex
	held   []net.Conn
}

func newStallProxy(t *testing.T, target string, stall int32) *stallProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{l: l, target: target, stall: stall}
	go p.loop()
	t.Cleanup(func() {
		_ = l.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.held {
			_ = c.Close()
		}
	})
	return p
}

func (p *stallProxy) addr() string { return p.l.Addr().String() }

func (p *stallProxy) loop() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		if p.seen.Add(1) <= p.stall {
			p.mu.Lock()
			p.held = append(p.held, c)
			p.mu.Unlock()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}
}

func shortDialer() Dialer {
	return hubDialer{dialTimeout: 300 * time.Millisecond, handshakeTimeout: 600 * time.Millisecond}
}

func TestStalledTLSDialIsBoundedAndTheNextAttemptConnects(t *testing.T) {
	h := newStubHub(t)
	proxy := newStallProxy(t, h.addr, 2)
	w := newHubWorkerDialer(t, "https://"+proxy.addr(), identity.Pin{Pub: h.key.Public()}, shortDialer())
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)
	if n := proxy.seen.Load(); n < 3 {
		t.Fatalf("connections through the proxy = %d, want the two stalled ones and a good one", n)
	}
	if !strings.Contains(w.logs.String(), "no answer within") {
		t.Fatalf("a stalled dial must be reported as a timeout: %s", w.logs.String())
	}
}

func TestStalledUpgradeIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	key, _ := identity.GenerateKey()
	hub, _ := identity.GenerateKey()

	start := time.Now()
	_, err := shortDialer().Connect(context.Background(), ConnectParams{
		HubURL: srv.URL, Key: key, Name: "pi", Pin: identity.Pin{Pub: hub.Public()},
	})
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stalled upgrade held the attempt for %s", d)
	}
}

func TestStalledHandshakeIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := transport.Accept(w, r)
		if err != nil {
			return
		}
		<-release // the upgrade is accepted, the handshake never starts
		_ = conn.Close()
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	key, _ := identity.GenerateKey()
	hub, _ := identity.GenerateKey()

	start := time.Now()
	_, err := shortDialer().Connect(context.Background(), ConnectParams{
		HubURL: srv.URL, Key: key, Name: "pi", Pin: identity.Pin{Pub: hub.Public()},
	})
	if err == nil {
		t.Fatal("a Hub that never answers the handshake must fail the attempt")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stalled handshake held the attempt for %s", d)
	}
}

func TestDialTimeoutDoesNotCutTheSessionAfterAGoodDial(t *testing.T) {
	h := newStubHub(t)
	w := newHubWorkerDialer(t, h.url(), identity.Pin{Pub: h.key.Public()},
		hubDialer{dialTimeout: 100 * time.Millisecond, handshakeTimeout: 100 * time.Millisecond})
	h.enrol(w.key.Public())
	w.waitState(t, ConnConnected)
	time.Sleep(500 * time.Millisecond) // well past both limits
	if w.conn.Status().State != ConnConnected {
		t.Fatalf("the per-attempt limits must not end a healthy connection: %+v", w.conn.Status())
	}
}

// --- the enrollment generation -----------------------------------------------

func TestConditionalWritersRefuseAfterTheEnrollmentChanged(t *testing.T) {
	f := newConnFixture(t)
	ctx := context.Background()
	gen, _ := f.enr.Watch()
	other, _ := identity.GenerateKey()
	if err := f.enr.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: other.Public()}, WorkerID: "w-new"}); err != nil {
		t.Fatal(err)
	}
	if err := f.enr.RewritePinIf(ctx, gen, identity.Pin{Pub: f.hub.Public(), Seq: 5}); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("RewritePinIf = %v", err)
	}
	if err := f.enr.ApplyWelcomeIf(ctx, gen, HubWelcome{WorkerID: "w-old", Policy: HubPolicy{PreviewOnly: true}}); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("ApplyWelcomeIf = %v", err)
	}
	if _, err := f.enr.WipeIf(ctx, gen); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("WipeIf = %v", err)
	}
	got := f.storedEnrollment()
	if got.WorkerID != "w-new" || got.Policy.PreviewOnly || got.Pin.Seq != 0 || !got.Pin.Pub.Equal(ed25519.PublicKey(other.Public())) {
		t.Fatalf("the replacement enrollment was touched: %+v", got)
	}
	// With the current generation they write.
	gen, _ = f.enr.Watch()
	if err := f.enr.ApplyWelcomeIf(ctx, gen, HubWelcome{WorkerID: "w-new", Policy: HubPolicy{PreviewOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if !f.storedEnrollment().Policy.PreviewOnly {
		t.Fatal("ApplyWelcomeIf with the current generation must write")
	}
}

func TestResetDuringALiveConnectionDropsItAndGoesIdle(t *testing.T) {
	f := newConnFixture(t)
	w := okWelcome()
	w.Policy = HubPolicy{PreviewOnly: true}
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, w))
	f.run()
	f.waitState(ConnConnected)
	if !f.policy.PreviewOnly() {
		t.Fatal("setup: the Hub policy should be on")
	}

	// `minion worker reset` in this process.
	if _, err := f.enr.Wipe(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.waitIdle("not enrolled")
	if !sess.closed.Load() {
		t.Fatal("the connection of the wiped enrollment must be closed")
	}
	if f.policy.PreviewOnly() {
		t.Fatal("the restriction of the old Hub must end with its enrollment")
	}
	calls := f.dial.count()
	f.step(DefaultIdlePoll)
	if f.dial.count() != calls {
		t.Fatal("a wiped Worker must not dial")
	}
}

func TestReEnrollmentDuringALiveConnectionReconnectsWithTheNewPinAtOnce(t *testing.T) {
	f := newConnFixture(t)
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()))
	f.run()
	f.waitState(ConnConnected)

	newHub, _ := identity.GenerateKey()
	next := newFakeSession()
	f.dial.script(f.welcomeStep(next, okWelcome()))
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: newHub.Public()}, Policy: HubPolicy{PreviewOnly: true}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return sess.closed.Load() && f.dial.count() >= 2 })
	if got := f.dial.call(1).Pin.Pub; !got.Equal(ed25519.PublicKey(newHub.Public())) {
		t.Fatal("the reconnect must use the new pin")
	}
	f.waitState(ConnConnected)
	if next.closed.Load() {
		t.Fatal("the new connection must stay up")
	}
}

func TestRevokeOfTheOldHubDoesNotDeleteAReplacementEnrollment(t *testing.T) {
	f := newConnFixture(t)
	hash := randomHash()
	started, release := make(chan struct{}), make(chan struct{})
	revoke := f.revokeStep(f.hub, hash, hash)
	slowRevoke := func(p ConnectParams) (*Attempt, error) {
		close(started)
		<-release // the old Hub's revoke is still on its way
		return revoke(p)
	}
	f.dial.script(slowRevoke, failStep("down"))
	f.run()
	<-started

	newHub, _ := identity.GenerateKey()
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: newHub.Public()}, WorkerID: "w-new"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, func() bool { return f.dial.count() >= 2 })

	got := f.storedEnrollment()
	if got.WorkerID != "w-new" || !got.Pin.Pub.Equal(ed25519.PublicKey(newHub.Public())) {
		t.Fatalf("the replacement enrollment must survive the old Hub's revoke: %+v", got)
	}
	if !strings.Contains(f.logs.String(), "replaced meanwhile") {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

// --- the welcome must be stored before the connection counts as healthy ------

// failingWelcomeRepo is the store whose UpdateWelcome can be made to fail.
type failingWelcomeRepo struct {
	*store.Store
	fail atomic.Bool
}

func (r *failingWelcomeRepo) UpdateWelcome(ctx context.Context, w store.WelcomeInfo) error {
	if r.fail.Load() {
		return errors.New("database is locked")
	}
	return r.Store.UpdateWelcome(ctx, w)
}

func TestFailedWelcomePersistenceKeepsTheNewRestrictionAndRetries(t *testing.T) {
	f := newConnFixture(t)
	repo := &failingWelcomeRepo{Store: f.st}
	repo.fail.Store(true)
	f.enr = NewEnrollments(repo, f.dir)
	f.conn = f.newConnector()

	w := okWelcome()
	w.Policy = HubPolicy{PreviewOnly: true}
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, w))
	f.run()
	f.waitState(ConnBackoff)

	if !f.policy.PreviewOnly() {
		t.Fatal("a restriction the Hub imposed must be enforced although it could not be stored")
	}
	if !sess.closed.Load() {
		t.Fatal("a connection whose welcome was not stored must not be served")
	}
	if f.storedEnrollment().Policy.PreviewOnly {
		t.Fatal("setup: nothing must have been stored")
	}
	if !strings.Contains(f.conn.Status().LastError, "cannot store the welcome data") {
		t.Fatalf("status = %+v", f.conn.Status())
	}

	// The retry stores it and only then is the connection healthy.
	repo.fail.Store(false)
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.step(time.Minute)
	f.waitState(ConnConnected)
	if !f.storedEnrollment().Policy.PreviewOnly || !f.policy.PreviewOnly() {
		t.Fatal("the retried welcome must be stored and applied")
	}
}

func TestFailedWelcomePersistenceDoesNotLiftAStoredRestrictionInMemory(t *testing.T) {
	f := newConnFixture(t)
	if err := f.enr.Commit(context.Background(), Enrollment{
		Pin: identity.Pin{Pub: f.hub.Public()}, WorkerID: "w-1", Policy: HubPolicy{PreviewOnly: true},
	}); err != nil {
		t.Fatal(err)
	}
	repo := &failingWelcomeRepo{Store: f.st}
	repo.fail.Store(true)
	f.enr = NewEnrollments(repo, f.dir)
	f.conn = f.newConnector()
	if err := f.conn.applyStoredPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}

	w := okWelcome() // the Hub now lifts the restriction
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.run()
	f.waitState(ConnBackoff)
	if !f.policy.PreviewOnly() {
		t.Fatal("a lifted restriction must take effect only once it is stored: disk still says restricted")
	}
	repo.fail.Store(false)
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.step(time.Minute)
	f.waitState(ConnConnected)
	if f.policy.PreviewOnly() {
		t.Fatal("once stored, the lifted restriction applies")
	}
}

// --- key creation ------------------------------------------------------------

func TestConcurrentKeyCreationYieldsOneIdentity(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	enr := NewEnrollments(st, dir)

	const n = 64
	keys := make([]*identity.Key, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = enr.LoadOrCreateKey()
		}()
	}
	close(start)
	wg.Wait()

	disk, err := enr.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !keys[i].Public().Equal(disk.Public()) {
			t.Fatalf("caller %d got an identity that is not the one on disk", i)
		}
	}
}
