package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// --- test doubles ----------------------------------------------------------

// connClock is a manually advanced transport.Clock. After registers a timer
// and Advance fires every timer that falls due. It records the delay of every
// After call, so tests can check the backoff.
type connClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []connTimer
	delays []time.Duration
}

type connTimer struct {
	at time.Time
	ch chan time.Time
}

func newConnClock() *connClock {
	return &connClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *connClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *connClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := connTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.delays = append(c.delays, d)
	return t.ch
}

func (c *connClock) NewTicker(time.Duration) transport.Ticker { return idleTicker{} }

type idleTicker struct{}

func (idleTicker) C() <-chan time.Time { return nil }
func (idleTicker) Stop()               {}

func (c *connClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *connClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

// Advance moves time forward by d and fires the timers that are due.
func (c *connClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
}

// fakeSession is a Session whose end the test controls.
type fakeSession struct {
	done   chan struct{}
	once   sync.Once
	err    error
	closed atomic.Bool
}

func newFakeSession() *fakeSession { return &fakeSession{done: make(chan struct{})} }

func (s *fakeSession) drop(err error) {
	s.once.Do(func() { s.err = err; close(s.done) })
}

func (s *fakeSession) Accept(ctx context.Context) (*transport.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *fakeSession) OpenCall(context.Context) (*transport.Stream, error) {
	return nil, errors.New("fake")
}
func (s *fakeSession) OpenEvents(context.Context) (*transport.Stream, error) {
	return nil, errors.New("fake")
}
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}
func (s *fakeSession) Close() error {
	s.closed.Store(true)
	s.drop(transport.ErrSessionClosed)
	return nil
}

type dialStep func(p ConnectParams) (*Attempt, error)

// scriptDialer is a Dialer that plays a script, one step per Connect call; the
// last step repeats.
type scriptDialer struct {
	mu    sync.Mutex
	steps []dialStep
	calls []ConnectParams
}

func (d *scriptDialer) script(steps ...dialStep) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steps = steps
}

func (d *scriptDialer) Connect(_ context.Context, p ConnectParams) (*Attempt, error) {
	d.mu.Lock()
	d.calls = append(d.calls, p)
	i := len(d.calls) - 1
	if i >= len(d.steps) {
		i = len(d.steps) - 1
	}
	var step dialStep
	if i >= 0 {
		step = d.steps[i]
	}
	d.mu.Unlock()
	if step == nil {
		return nil, errors.New("no script")
	}
	return step(p)
}

func (d *scriptDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *scriptDialer) call(i int) ConnectParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[i]
}

func failStep(msg string) dialStep {
	return func(ConnectParams) (*Attempt, error) { return nil, errors.New(msg) }
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// connFixture is a connector over a real store and a script dialer.
type connFixture struct {
	t      *testing.T
	st     *store.Store
	enr    *Enrollments
	policy *Policy
	clk    *connClock
	dial   *scriptDialer
	logs   *logBuf
	hub    *identity.Key
	worker *identity.Key
	conn   *Connector
	dir    string
}

type fixtureOpt func(*ConnectorConfig)

func newConnFixture(t *testing.T, opts ...fixtureOpt) *connFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &connFixture{
		t: t, st: st, dir: dir, enr: NewEnrollments(st, dir), policy: NewPolicy(st),
		clk: newConnClock(), dial: &scriptDialer{}, logs: &logBuf{},
	}
	if f.hub, err = identity.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	if f.worker, err = f.enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: f.hub.Public()}}); err != nil {
		t.Fatal(err)
	}
	f.drainChanged()
	f.conn = f.newConnector(opts...)
	return f
}

// drainChanged discards the wake-up that Commit left, as a new process would
// not have it.
func (f *connFixture) drainChanged() {
	select {
	case <-f.enr.Changed():
	default:
	}
}

func (f *connFixture) newConnector(opts ...fixtureOpt) *Connector {
	f.t.Helper()
	cfg := ConnectorConfig{
		HubURL:     "https://hub.example:8443",
		Name:       "test-worker",
		Ops:        func() []string { return []string{ops.OpSessionsList, ops.OpSendText} },
		Key:        f.enr.LoadKey,
		Enrollment: f.enr,
		Policy:     f.policy,
		Dialer:     f.dial,
		Clock:      f.clk,
		Backoff:    &transport.Backoff{Rand: func() float64 { return 0.5 }},
		Logger:     slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, o := range opts {
		o(&cfg)
	}
	c, err := NewConnector(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// run starts f.conn and stops it when the test ends.
func (f *connFixture) run() context.CancelFunc { return f.runConn(f.conn) }

func (f *connFixture) runConn(c *Connector) context.CancelFunc {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Error("the connector did not stop")
		}
	}
	f.t.Cleanup(stop)
	return stop
}

// step waits until the connector sleeps on the clock and advances it by d.
func (f *connFixture) step(d time.Duration) {
	f.t.Helper()
	eventually(f.t, func() bool { return f.clk.pending() > 0 })
	f.clk.Advance(d)
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second) // generous: race-enabled CI runners are slow
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *connFixture) waitIdle(reason string) {
	f.t.Helper()
	eventually(f.t, func() bool {
		st := f.conn.Status()
		return st.State == ConnIdle && st.Reason == reason
	})
}

func (f *connFixture) waitState(want ConnState) {
	f.t.Helper()
	eventually(f.t, func() bool { return f.conn.Status().State == want })
}

func (f *connFixture) welcomeStep(sess *fakeSession, w HubWelcome) dialStep {
	return func(p ConnectParams) (*Attempt, error) {
		if w.Type == "" {
			w.Type = proto.TypeWelcome
		}
		return &Attempt{
			Pin: p.Pin, TranscriptHash: randomHash(), Version: proto.Current,
			Reply: Reply{Type: proto.TypeWelcome, Welcome: w}, Session: sess,
		}, nil
	}
}

func randomHash() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

func okWelcome() HubWelcome {
	return HubWelcome{WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private", Range: proto.SupportedRange(proto.Current)}
}

func (f *connFixture) storedEnrollment() Enrollment {
	f.t.Helper()
	e, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok {
		f.t.Fatalf("enrollment = %+v, ok %v, err %v", e, ok, err)
	}
	return e
}

// --- AC: connect mode with the stored pin, retry forever --------------------

func TestConnectsInConnectModeWithTheStoredPin(t *testing.T) {
	f := newConnFixture(t)
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()))
	f.run()
	f.waitState(ConnConnected)

	p := f.dial.call(0)
	if p.HubURL != "https://hub.example:8443" || p.Name != "test-worker" {
		t.Fatalf("params = %+v", p)
	}
	if !bytes.Equal(p.Pin.Pub, f.hub.Public()) || p.Pin.Seq != 0 {
		t.Fatalf("the connector must present the stored pin, got %+v", p.Pin)
	}
	if !bytes.Equal(p.Key.Public(), f.worker.Public()) {
		t.Fatal("the connector must use the Worker's key")
	}
	if got := p.Ops; len(got) != 2 || got[0] != ops.OpSessionsList {
		t.Fatalf("ops = %v", got)
	}
	st := f.conn.Status()
	if st.Welcome.WorkerID != "w-1" || st.Attempts != 1 || st.LastError != "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestRetriesWithTheBackoffForeverAndResetsAfterAConnection(t *testing.T) {
	f := newConnFixture(t)
	sess := newFakeSession()
	steps := make([]dialStep, 0, 14)
	for i := 0; i < 12; i++ {
		steps = append(steps, failStep("connection refused"))
	}
	steps = append(steps, f.welcomeStep(sess, okWelcome()))
	f.dial.script(steps...)
	f.run()

	for i := 0; i < 12; i++ {
		f.step(time.Minute)
	}
	f.waitState(ConnConnected)
	if f.dial.count() != 13 {
		t.Fatalf("attempts = %d, want 13", f.dial.count())
	}
	delays := f.clk.recorded()
	if len(delays) != 12 {
		t.Fatalf("delays = %v", delays)
	}
	// AD-3: the delay grows from 1 s and is capped at 60 s; Rand 0.5 puts it
	// at three quarters of the base.
	for i, d := range delays {
		base := (&transport.Backoff{}).Base(i)
		if d < base/2 || d > base || d > transport.BackoffMax {
			t.Errorf("delay %d = %v, base %v", i, d, base)
		}
	}
	if last := delays[len(delays)-1]; last < 30*time.Second {
		t.Errorf("the delay never reached the cap region: %v", last)
	}

	// After a connection the sequence starts again from the bottom.
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	sess.drop(errors.New("hub went away"))
	f.step(time.Minute)
	eventually(t, func() bool { return f.dial.count() == 14 })
	delays = f.clk.recorded()
	if d := delays[len(delays)-1]; d > transport.BackoffInitial {
		t.Errorf("the first delay after a connection = %v, want at most %v", d, transport.BackoffInitial)
	}
}

func TestReconnectsAfterARestartFromTheStoredState(t *testing.T) {
	f := newConnFixture(t)
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()), f.welcomeStep(newFakeSession(), okWelcome()))
	stop := f.run()
	f.waitState(ConnConnected)
	stop()

	// A new connector over the same state, as after a restart: nothing but
	// the stored pin and the key file.
	c2 := f.newConnector()
	f.runConn(c2)
	eventually(t, func() bool { return c2.Status().State == ConnConnected })
	p := f.dial.call(f.dial.count() - 1)
	if !bytes.Equal(p.Pin.Pub, f.hub.Public()) || !bytes.Equal(p.Key.Public(), f.worker.Public()) {
		t.Fatalf("restart did not reuse the stored identity: %+v", p.Pin)
	}
}

func TestIdleWithoutAHubUrlOrWithoutEnrollment(t *testing.T) {
	// No Hub URL: Solo.
	f := newConnFixture(t, func(c *ConnectorConfig) { c.HubURL = "" })
	f.dial.script(failStep("must not be dialed"))
	f.run()
	f.waitIdle("no Hub configured")
	f.step(DefaultIdlePoll)
	f.step(DefaultIdlePoll)
	if f.dial.count() != 0 {
		t.Fatal("a Worker without a Hub URL must not dial")
	}
	if n := strings.Count(f.logs.String(), "hub connector idle"); n != 1 {
		t.Fatalf("the idle state was logged %d times, want once", n)
	}

	// A Hub URL but no enrollment: wait, and start as soon as it is committed.
	g := newConnFixture(t)
	if _, err := g.enr.Wipe(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.drainChanged()
	g.dial.script(g.welcomeStep(newFakeSession(), okWelcome()))
	g.run()
	g.waitIdle("not enrolled")
	if g.dial.count() != 0 {
		t.Fatalf("dials %d", g.dial.count())
	}
	if err := g.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: g.hub.Public()}}); err != nil {
		t.Fatal(err)
	}
	g.waitState(ConnConnected)
}

func TestMissingKeyIsRetriedAndNothingIsDeleted(t *testing.T) {
	f := newConnFixture(t, func(c *ConnectorConfig) {
		c.Key = func() (*identity.Key, error) { return nil, ErrNoWorkerKey }
	})
	f.dial.script(failStep("must not be dialed"))
	f.run()
	f.waitState(ConnBackoff)
	f.step(time.Minute)
	eventually(t, func() bool { return f.clk.pending() > 0 })
	if f.dial.count() != 0 {
		t.Fatal("a Worker without a key must not dial")
	}
	if !strings.Contains(f.conn.Status().LastError, "key") {
		t.Fatalf("status = %+v", f.conn.Status())
	}
	f.storedEnrollment()
}

// --- AD-3: no credential expires: a year offline ----------------------------

func TestReconnectsAfterASimulatedYearOfflineWithoutAnyRefresh(t *testing.T) {
	f := newConnFixture(t)
	start := f.clk.Now()
	enrolledAt := f.storedEnrollment().EnrolledAt
	steps := []dialStep{}
	for i := 0; i < 5; i++ {
		steps = append(steps, failStep("no route to host"))
	}
	steps = append(steps, f.welcomeStep(newFakeSession(), okWelcome()))
	f.dial.script(steps...)
	f.run()

	// 5 x 73 days = 365 days without a Hub.
	for i := 0; i < 5; i++ {
		f.step(73 * 24 * time.Hour)
	}
	f.waitState(ConnConnected)
	if got := f.clk.Now().Sub(start); got < 365*24*time.Hour {
		t.Fatalf("only %v passed", got)
	}
	for i, d := range f.clk.recorded() {
		if d > transport.BackoffMax {
			t.Errorf("delay %d = %v exceeds the cap", i, d)
		}
	}
	// The same pin and key as on day one: nothing was refreshed, re-issued or
	// re-enrolled, and the stored enrollment is untouched.
	last := f.dial.call(f.dial.count() - 1)
	if !bytes.Equal(last.Pin.Pub, f.hub.Public()) || last.Pin.Seq != 0 || !bytes.Equal(last.Key.Public(), f.worker.Public()) {
		t.Fatalf("the credentials changed: %+v", last.Pin)
	}
	if got := f.storedEnrollment(); !got.EnrolledAt.Equal(enrolledAt) || !bytes.Equal(got.Pin.Pub, f.hub.Public()) {
		t.Fatalf("enrollment changed: %+v", got)
	}
}

// --- AC: welcome ------------------------------------------------------------

func TestWelcomeStoresDataAppliesPolicyAndLogsWithoutSecrets(t *testing.T) {
	f := newConnFixture(t, func(c *ConnectorConfig) { c.HubURL = "https://user:hunter2@hub.example:8443/prefix?token=abc" })
	w := okWelcome()
	w.Policy = HubPolicy{PreviewOnly: true}
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.run()
	f.waitState(ConnConnected)

	got := f.storedEnrollment()
	if got.WorkerID != "w-1" || got.Name != "pi" || got.Owner != "alice" || got.Kind != "private" || !got.Policy.PreviewOnly {
		t.Fatalf("stored enrollment = %+v", got)
	}
	if !f.policy.PreviewOnly() || !f.policy.HubPreviewOnly() {
		t.Fatal("the Hub policy was not applied")
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "connected to hub") || !strings.Contains(logs, "w-1") || !strings.Contains(logs, "alice") {
		t.Fatalf("logs = %s", logs)
	}
	for _, secret := range []string{"hunter2", "token=abc", "prefix"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the log leaks %q: %s", secret, logs)
		}
	}
}

func TestLocalRestrictionsAlwaysWin(t *testing.T) {
	f := newConnFixture(t)
	f.policy.SetPreviewOnly(true) // local: --preview-only
	w := okWelcome()
	w.Policy = HubPolicy{PreviewOnly: false} // the Hub does not restrict
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	f.run()
	f.waitState(ConnConnected)
	if !f.policy.PreviewOnly() {
		t.Fatal("a Hub without restriction must not lift the local preview-only")
	}
	if f.policy.HubPreviewOnly() {
		t.Fatal("the Hub layer must stay off")
	}
}

func TestHubRestrictionCanBeLiftedByTheHubButNotTheLocalOne(t *testing.T) {
	p := NewPolicy(nil)
	p.SetHubPreviewOnly(true)
	if !p.PreviewOnly() {
		t.Fatal("hub restriction not applied")
	}
	p.SetHubPreviewOnly(false)
	if p.PreviewOnly() {
		t.Fatal("hub restriction not lifted")
	}
	p.SetPreviewOnly(true)
	p.SetHubPreviewOnly(true)
	p.SetHubPreviewOnly(false)
	if !p.PreviewOnly() {
		t.Fatal("lifting the hub layer must not lift the local one")
	}
}

func TestStoredHubPolicyAppliesBeforeTheFirstConnection(t *testing.T) {
	f := newConnFixture(t)
	if err := f.enr.Commit(context.Background(), Enrollment{
		Pin: identity.Pin{Pub: f.hub.Public()}, WorkerID: "w-1", Policy: HubPolicy{PreviewOnly: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.conn.applyStoredPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.policy.PreviewOnly() {
		t.Fatal("the restriction of the last welcome must hold while the Hub is unreachable")
	}
	// Without enrollment there is no Hub restriction.
	if _, err := f.enr.Wipe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.conn.applyStoredPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.policy.PreviewOnly() {
		t.Fatal("a wiped enrollment must not keep restricting")
	}
}

func TestWelcomeWithoutWorkerIdIsRefused(t *testing.T) {
	f := newConnFixture(t)
	sess := newFakeSession()
	w := okWelcome()
	w.WorkerID = ""
	f.dial.script(f.welcomeStep(sess, w))
	f.run()
	f.waitState(ConnBackoff)
	if !sess.closed.Load() {
		t.Fatal("the session of a refused welcome must be closed")
	}
}

// --- AC: not_enrolled keeps everything --------------------------------------

func TestNotEnrolledKeepsAllStateAndRetries(t *testing.T) {
	f := newConnFixture(t)
	if err := f.st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	before := f.storedEnrollment()
	notEnrolled := func(p ConnectParams) (*Attempt, error) {
		return &Attempt{Pin: p.Pin, TranscriptHash: randomHash(), Reply: Reply{Type: proto.TypeNotEnrolled}}, nil
	}
	f.dial.script(notEnrolled, notEnrolled, f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitState(ConnBackoff)
	if !strings.Contains(f.conn.Status().LastError, "not_enrolled") {
		t.Fatalf("status = %+v", f.conn.Status())
	}
	f.step(time.Minute)
	eventually(t, func() bool { return f.dial.count() == 2 })
	f.step(time.Minute)
	f.waitState(ConnConnected)

	after := f.storedEnrollment()
	if !bytes.Equal(after.Pin.Pub, before.Pin.Pub) || !after.EnrolledAt.Equal(before.EnrolledAt) {
		t.Fatalf("not_enrolled changed the enrollment: %+v -> %+v", before, after)
	}
	if _, err := f.enr.LoadKey(); err != nil {
		t.Fatalf("not_enrolled must not touch the key: %v", err)
	}
	if sel, err := f.st.LoadShare(); err != nil || sel.Mode != store.ShareAll {
		t.Fatalf("share selection = %+v, %v", sel, err)
	}
}

// --- AC: revoke -------------------------------------------------------------

func (f *connFixture) revokeStep(signer *identity.Key, signedHash, transcript []byte) dialStep {
	return func(p ConnectParams) (*Attempt, error) {
		var sig []byte
		if signer != nil {
			var err error
			if sig, err = signer.Sign(identity.PurposeRevoke, signedHash); err != nil {
				return nil, err
			}
		}
		return &Attempt{Pin: p.Pin, TranscriptHash: transcript, Reply: Reply{Type: proto.TypeRevoke, RevokeSig: sig}}, nil
	}
}

func TestValidRevokeWipesPinAndEnrollmentOnlyAndGoesIdle(t *testing.T) {
	f := newConnFixture(t)
	if err := f.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	w := okWelcome()
	w.Policy = HubPolicy{PreviewOnly: true}
	hash := randomHash()
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, w), f.revokeStep(f.hub, hash, hash))
	f.run()
	f.waitState(ConnConnected)
	if !f.policy.PreviewOnly() {
		t.Fatal("setup: the Hub policy should be on")
	}
	sess.drop(errors.New("hub restarted"))
	f.step(time.Minute)
	f.waitState(ConnIdle)

	if _, ok, err := f.enr.Load(context.Background()); err != nil || ok {
		t.Fatalf("the enrollment must be wiped: ok %v, err %v", ok, err)
	}
	if _, err := f.enr.LoadKey(); err != nil {
		t.Fatalf("a revoke wipes the pin and the enrollment, not the key: %v", err)
	}
	if sel, err := f.st.LoadShare(); err != nil || sel.Mode != store.ShareSelect || len(sel.Sessions) != 1 {
		t.Fatalf("the sessions' share selection must stay: %+v, %v", sel, err)
	}
	if n, err := f.st.NextSessionNumber(); err != nil || n != 2 {
		t.Fatalf("session counter = %d, %v", n, err)
	}
	if f.policy.PreviewOnly() {
		t.Fatal("the Hub restriction must end with the enrollment")
	}
	if !strings.Contains(f.logs.String(), "removed by the hub") {
		t.Fatalf("logs = %s", f.logs.String())
	}
	// Nothing to connect to any more: no further attempts.
	calls := f.dial.count()
	f.step(DefaultIdlePoll)
	f.step(DefaultIdlePoll)
	if f.dial.count() != calls {
		t.Fatal("the connector must not dial after it was removed")
	}
}

func TestInvalidRevokeIsIgnoredLoggedAndNothingIsDeleted(t *testing.T) {
	stranger, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	hash := randomHash()
	cases := []struct {
		name string
		step func(f *connFixture) dialStep
	}{
		{"unsigned", func(f *connFixture) dialStep { return f.revokeStep(nil, hash, hash) }},
		{"signed by another key", func(f *connFixture) dialStep { return f.revokeStep(stranger, hash, hash) }},
		{"signed over another transcript", func(f *connFixture) dialStep { return f.revokeStep(f.hub, randomHash(), hash) }},
		{"wrong purpose", func(f *connFixture) dialStep {
			return func(p ConnectParams) (*Attempt, error) {
				sig, _ := f.hub.Sign(identity.PurposeHandshakeHub, hash)
				return &Attempt{Pin: p.Pin, TranscriptHash: hash, Reply: Reply{Type: proto.TypeRevoke, RevokeSig: sig}}, nil
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newConnFixture(t)
			before := f.storedEnrollment()
			f.dial.script(c.step(f), f.welcomeStep(newFakeSession(), okWelcome()))
			f.run()
			f.waitState(ConnBackoff)
			if !strings.Contains(f.logs.String(), "ignored a revoke") {
				t.Fatalf("the ignored revoke was not logged: %s", f.logs.String())
			}
			after := f.storedEnrollment()
			if !bytes.Equal(after.Pin.Pub, before.Pin.Pub) {
				t.Fatal("an invalid revoke changed the enrollment")
			}
			// The Worker keeps retrying.
			f.step(time.Minute)
			f.waitState(ConnConnected)
		})
	}
}

// --- AC: lost connection ----------------------------------------------------

func TestLostConnectionEndsInFlightOpsAsUnknownAndReconnects(t *testing.T) {
	f := newConnFixture(t)
	sess1, sess2 := newFakeSession(), newFakeSession()
	f.dial.script(f.welcomeStep(sess1, okWelcome()), f.welcomeStep(sess2, okWelcome()))

	// An op server in flight: it ends as unknown when the link context ends
	// with ErrLinkLost (AD-12, AD-15).
	type result struct {
		outcome ops.Outcome
		cause   error
	}
	results := make(chan result, 4)
	inflight := make(chan struct{}, 4)
	f.conn.OnLink("opserver", func(l *Link) {
		inflight <- struct{}{}
		<-l.Context().Done()
		out := ops.OutcomeDelivered
		if errors.Is(context.Cause(l.Context()), ErrLinkLost) {
			out = ops.OutcomeUnknown
		}
		results <- result{out, context.Cause(l.Context())}
	})
	f.run()
	f.waitState(ConnConnected)
	<-inflight

	// The local side keeps serving while the Hub is away.
	local := newEnv(t)
	local.host.addSession("work", "bash")
	sess1.drop(errors.New("connection reset"))
	r := <-results
	if r.outcome != ops.OutcomeUnknown || !errors.Is(r.cause, ErrLinkLost) {
		t.Fatalf("in-flight op ended as %v (cause %v), want unknown", r.outcome, r.cause)
	}
	f.waitState(ConnBackoff)
	if _, err := local.call(ops.OpSessionsList, allActions, nil); err != nil {
		t.Fatalf("the local executor must keep serving while the Hub is away: %v", err)
	}
	if !strings.Contains(f.logs.String(), "lost the connection") {
		t.Fatalf("logs = %s", f.logs.String())
	}

	// The reconnect starts, and the handler serves the new link too.
	f.step(time.Minute)
	f.waitState(ConnConnected)
	<-inflight
	if f.dial.count() != 2 {
		t.Fatalf("dials = %d", f.dial.count())
	}
	if !sess1.closed.Load() {
		t.Fatal("the lost session must be closed")
	}
}

func TestHandlerRegisteredWhileConnectedServesTheCurrentLink(t *testing.T) {
	f := newConnFixture(t)
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	f.run()
	f.waitState(ConnConnected)
	got := make(chan string, 1)
	f.conn.OnLink("late", func(l *Link) { got <- l.Welcome.WorkerID })
	select {
	case id := <-got:
		if id != "w-1" {
			t.Fatalf("worker id %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a handler registered while connected must serve the current link")
	}
}

func TestPanickingHandlerDoesNotBreakTheConnection(t *testing.T) {
	f := newConnFixture(t)
	f.dial.script(f.welcomeStep(newFakeSession(), okWelcome()))
	f.conn.OnLink("bad", func(*Link) { panic("boom") })
	f.run()
	f.waitState(ConnConnected)
	eventually(t, func() bool { return strings.Contains(f.logs.String(), "panicked") })
	if f.conn.Status().State != ConnConnected {
		t.Fatal("the connection must survive a panicking handler")
	}
}

func TestStoppingClosesTheConnectionAndDoesNotReportALoss(t *testing.T) {
	f := newConnFixture(t)
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()))
	stop := f.run()
	f.waitState(ConnConnected)
	stop()
	if !sess.closed.Load() {
		t.Fatal("stopping must close the session")
	}
	if strings.Contains(f.logs.String(), "lost the connection") {
		t.Fatal("a stop is not a loss")
	}
}

func TestHandlersThatDoNotStopDelayTheReconnectOnlyByTheGrace(t *testing.T) {
	f := newConnFixture(t, func(c *ConnectorConfig) { c.HandlerGrace = 30 * time.Second })
	sess := newFakeSession()
	f.dial.script(f.welcomeStep(sess, okWelcome()), f.welcomeStep(newFakeSession(), okWelcome()))
	release := make(chan struct{})
	f.conn.OnLink("stuck", func(*Link) { <-release })
	f.run()
	t.Cleanup(func() { close(release) }) // runs before the stop that waits for the handler
	f.waitState(ConnConnected)
	sess.drop(errors.New("gone"))
	f.step(30 * time.Second) // the grace timer
	f.waitState(ConnBackoff)
	f.step(time.Minute)
	f.waitState(ConnConnected)
	if !strings.Contains(f.logs.String(), "did not stop in time") {
		t.Fatalf("logs = %s", f.logs.String())
	}
}

// --- pin rewrite ------------------------------------------------------------

func TestAdvancedPinIsStoredBeforeTheConnectionIsServed(t *testing.T) {
	f := newConnFixture(t)
	newHub, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan Enrollment, 1)
	f.conn.OnLink("check", func(*Link) {
		e, _, _ := f.enr.Load(context.Background())
		seen <- e
	})
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		return &Attempt{
			Pin: identity.Pin{Pub: newHub.Public(), Seq: 2}, PinChanged: true, TranscriptHash: randomHash(),
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: newFakeSession(),
		}, nil
	})
	f.run()
	f.waitState(ConnConnected)
	e := <-seen
	if !bytes.Equal(e.Pin.Pub, newHub.Public()) || e.Pin.Seq != 2 || e.WorkerID != "w-1" {
		t.Fatalf("the pin must be rewritten before the link is served: %+v", e)
	}
}

func TestPinThatMovesBackwardsIsRefused(t *testing.T) {
	f := newConnFixture(t)
	if err := f.enr.RewritePin(context.Background(), identity.Pin{Pub: f.hub.Public(), Seq: 3}); err != nil {
		t.Fatal(err)
	}
	other, _ := identity.GenerateKey()
	sess := newFakeSession()
	f.dial.script(func(p ConnectParams) (*Attempt, error) {
		return &Attempt{
			Pin: identity.Pin{Pub: other.Public(), Seq: 1}, PinChanged: true, TranscriptHash: randomHash(),
			Reply: Reply{Type: proto.TypeWelcome, Welcome: okWelcome()}, Session: sess,
		}, nil
	})
	f.run()
	f.waitState(ConnBackoff)
	if !sess.closed.Load() {
		t.Fatal("a connection with a refused pin must be closed")
	}
	if got := f.storedEnrollment(); got.Pin.Seq != 3 || !bytes.Equal(got.Pin.Pub, f.hub.Public()) {
		t.Fatalf("pin changed: %+v", got)
	}
}

func TestOpsAreAnnouncedFreshForEveryAttempt(t *testing.T) {
	var n atomic.Int32
	f := newConnFixture(t, func(c *ConnectorConfig) {
		c.Ops = func() []string {
			if n.Add(1) == 1 {
				return []string{ops.OpSessionsList}
			}
			return []string{ops.OpSessionsList, ops.OpSendText}
		}
	})
	f.dial.script(failStep("down"), failStep("down"))
	f.run()
	f.step(time.Minute)
	eventually(t, func() bool { return f.dial.count() == 2 })
	if a, b := len(f.dial.call(0).Ops), len(f.dial.call(1).Ops); a != 1 || b != 2 {
		t.Fatalf("announced ops: %d then %d", a, b)
	}
}

func TestRepeatedFailuresAreLoggedOnceAtWarn(t *testing.T) {
	f := newConnFixture(t)
	f.dial.script(failStep("connection refused"))
	f.run()
	for i := 0; i < 4; i++ {
		f.step(time.Minute)
	}
	eventually(t, func() bool { return f.dial.count() >= 5 })
	if n := strings.Count(f.logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warnings = %d, want 1 for a repeated failure:\n%s", n, f.logs.String())
	}
}

func TestNewConnectorNeedsItsPorts(t *testing.T) {
	good := ConnectorConfig{Dialer: &scriptDialer{}, Enrollment: NewEnrollments(nil, ""), Key: func() (*identity.Key, error) { return nil, nil }}
	if _, err := NewConnector(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*ConnectorConfig){
		"dialer":     func(c *ConnectorConfig) { c.Dialer = nil },
		"enrollment": func(c *ConnectorConfig) { c.Enrollment = nil },
		"key":        func(c *ConnectorConfig) { c.Key = nil },
	} {
		cfg := good
		mut(&cfg)
		if _, err := NewConnector(cfg); err == nil {
			t.Errorf("a connector without %s must be refused", name)
		}
	}
}

// F-02: for a Worker the Hub has not approved yet the event and turn channels
// are not opened (the Hub registers none for it); the others run.
func TestPendingWelcomeDoesNotStartTheEventAndTurnHandlers(t *testing.T) {
	f := newConnFixture(t)
	w := okWelcome()
	w.Pending = true
	f.dial.script(f.welcomeStep(newFakeSession(), w))
	started := make(chan string, 8)
	for _, name := range []string{"op-server", "events", "turns", "hub-call"} {
		f.conn.OnLink(name, func(*Link) { started <- name })
	}
	f.run()
	f.waitState(ConnConnected)
	got := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case n := <-started:
			got[n] = true
		case <-deadline:
			t.Fatalf("started = %v", got)
		}
	}
	time.Sleep(50 * time.Millisecond)
	for len(started) > 0 {
		got[<-started] = true
	}
	if !got["op-server"] || !got["hub-call"] || got["events"] || got["turns"] {
		t.Errorf("started = %v, want op-server and hub-call only", got)
	}
}
