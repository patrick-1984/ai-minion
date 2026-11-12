package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

var codeRE = regexp.MustCompile(`MN-[0-9A-Z]{4}-[0-9A-Z]{4}`)

// --- fake dialer -----------------------------------------------------------

// pairStep is one scripted Pair call. wait reports when the Hub holds the
// connection (OnWaiting) before the step returns its result.
type pairStep func(ctx context.Context, p PairParams) (PairResult, error)

type pairScript struct {
	mu    sync.Mutex
	steps []pairStep
	calls []PairParams
}

func (d *pairScript) Pair(ctx context.Context, p PairParams) (PairResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, p)
	i := len(d.calls) - 1
	if i >= len(d.steps) {
		i = len(d.steps) - 1
	}
	step := d.steps[i]
	d.mu.Unlock()
	return step(ctx, p)
}

func (d *pairScript) nCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *pairScript) call(i int) PairParams {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[i]
}

// pairFailStep fails before the Hub is reached: nothing is waiting, nothing shown.
func pairFailStep(err error) pairStep {
	return func(context.Context, PairParams) (PairResult, error) { return PairResult{}, err }
}

// pairWaitStep reports the Hub as holding the connection, then fails with err
// (or, when err is nil, waits for the end of the context).
func pairWaitStep(err error) pairStep {
	return func(ctx context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		if err != nil {
			return PairResult{}, err
		}
		<-ctx.Done()
		return PairResult{}, context.Cause(ctx)
	}
}

func pairOkStep(hubPub []byte, owner string) pairStep {
	return func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		return PairResult{
			Pin: identity.Pin{Pub: hubPub},
			Welcome: HubWelcome{
				Type: proto.TypeWelcome, WorkerID: "w-1", Name: p.Name, Owner: owner, Kind: "private",
				Policy: HubPolicy{PreviewOnly: true},
			},
			Version: proto.Current,
		}, nil
	}
}

// pairFixture is a pairer over a real store, a script dialer and a fake clock.
type pairFixture struct {
	t     *testing.T
	st    *store.Store
	enr   *Enrollments
	clk   *connClock
	dial  *pairScript
	out   *logBuf
	logs  *logBuf
	codes []identity.PairingCode
	next  int
	// backoffSeen is the number of recorded timer delays backoffStep went by.
	backoffSeen int
}

func newPairFixture(t *testing.T, steps ...pairStep) *pairFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &pairFixture{
		t: t, st: st, enr: NewEnrollments(st, dir), clk: newConnClock(),
		dial: &pairScript{steps: steps}, out: &logBuf{}, logs: &logBuf{},
	}
	return f
}

// codeSeq makes the pairer use these codes in order.
func (f *pairFixture) codeSeq(canon ...string) {
	for _, c := range canon {
		pc, err := identity.ParsePairingCode(c)
		if err != nil {
			f.t.Fatal(err)
		}
		f.codes = append(f.codes, pc)
	}
}

func (f *pairFixture) newCode() identity.PairingCode {
	if f.next < len(f.codes) {
		c := f.codes[f.next]
		f.next++
		return c
	}
	return identity.NewPairingCode()
}

func (f *pairFixture) pairer(opts ...func(*PairerConfig)) *Pairer {
	f.t.Helper()
	cfg := PairerConfig{
		HubURL:     "https://hub.example:8443",
		Name:       "pi",
		Enrollment: f.enr,
		Dialer:     f.dial,
		Clock:      f.clk,
		Backoff:    &transport.Backoff{Rand: func() float64 { return 0.5 }},
		NewCode:    f.newCode,
		Out:        f.out,
		Logger:     slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, o := range opts {
		o(&cfg)
	}
	p, err := NewPairer(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

type pairRun struct {
	enr  Enrollment
	err  error
	done chan struct{}
}

// run starts p.Run in the background; cancel ends it.
func (f *pairFixture) run(p *Pairer) (*pairRun, context.CancelFunc) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &pairRun{done: make(chan struct{})}
	go func() { defer close(r.done); r.enr, r.err = p.Run(ctx) }()
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			f.t.Error("the pairer did not stop")
		}
	})
	return r, cancel
}

func (r *pairRun) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pairer did not finish")
	}
}

func (f *pairFixture) shown() []string { return codeRE.FindAllString(f.out.String(), -1) }

// --- AC 1: generate, show, connect in pair_code mode -----------------------

func TestPairerShowsTheCodeOnConsoleAndInTheLogAndSendsItToTheDialer(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	r, cancel := f.run(f.pairer())
	eventually(t, func() bool { return len(f.shown()) == 1 })

	if got := f.shown()[0]; got != "MN-K7Q4-XY12" {
		t.Fatalf("console code = %q", got)
	}
	if !strings.Contains(f.logs.String(), "MN-K7Q4-XY12") {
		t.Fatalf("the code must be in the log too:\n%s", f.logs.String())
	}
	p := f.dial.call(0)
	if p.Code.PairingID() != "K7Q4" || p.Code.String() != "MN-K7Q4-XY12" {
		t.Fatalf("the dialer got code %v", p.Code)
	}
	if p.HubURL != "https://hub.example:8443" || p.Name != "pi" || p.Key == nil {
		t.Fatalf("params = %+v", p)
	}
	if p.Timeout <= PairCodeTTL {
		t.Fatalf("the handshake must outlive the code, Timeout = %s", p.Timeout)
	}
	if _, err := f.enr.LoadKey(); err != nil {
		t.Fatalf("the key pair is persisted before connecting: %v", err)
	}
	cancel()
	r.wait(t)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v", r.err)
	}
}

func TestCodeAppearsOnlyAfterTheFirstSuccessfulConnection(t *testing.T) {
	f := newPairFixture(t,
		pairFailStep(errors.New("dial the hub: connection refused")),
		pairFailStep(errors.New("dial the hub: connection refused")),
		pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	r, _ := f.run(f.pairer())

	f.backoffStep() // after the first failure
	f.backoffStep() // after the second one
	eventually(t, func() bool { return f.dial.nCalls() == 3 && len(f.shown()) == 1 })
	_ = r
	// The same code was used on all three attempts: it was never shown, so it
	// has no age and no reason to change.
	for i := range 3 {
		if got := f.dial.call(i).Code.String(); got != "MN-K7Q4-XY12" {
			t.Fatalf("attempt %d used %s", i, got)
		}
	}
	if strings.Count(f.out.String(), "MN-") != 1 {
		t.Fatalf("the code is shown once:\n%s", f.out.String())
	}
}

// step advances the clock by d once any timer is pending.
func (f *pairFixture) step(d time.Duration) {
	f.t.Helper()
	eventually(f.t, func() bool { return f.clk.pending() > 0 })
	f.clk.Advance(d)
}

// backoffStep waits until the pairer sleeps for a retry delay (a wait of at
// most a minute, as opposed to the lifetime timer of a code) that it has not
// served yet, and lets it pass.
func (f *pairFixture) backoffStep() {
	f.t.Helper()
	f.clk.Advance(f.waitBackoff())
}

// waitBackoff waits for the retry timer, ignoring a code lifetime timer left
// behind by an ended attempt. It returns the registered retry delay.
func (f *pairFixture) waitBackoff() time.Duration {
	f.t.Helper()
	var d time.Duration
	eventually(f.t, func() bool {
		rec := f.clk.recorded()
		for ; f.backoffSeen < len(rec); f.backoffSeen++ {
			if rec[f.backoffSeen] <= time.Minute {
				d = rec[f.backoffSeen]
				f.backoffSeen++
				return true
			}
		}
		return false
	})
	return d
}

func TestFailureLogsNeverContainTheCode(t *testing.T) {
	f := newPairFixture(t,
		pairFailStep(errors.New("dial the hub: connection refused")),
		pairWaitStep(proto.NewError(proto.CodePairingCodeInvalid, "the pairing code is wrong")),
		pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	f.run(f.pairer())
	f.backoffStep()
	f.backoffStep()
	eventually(t, func() bool { return f.dial.nCalls() == 3 })
	var shownLines int
	for _, l := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(l, "XY12") {
			shownLines++
			if !strings.Contains(l, `msg="pairing code"`) {
				t.Fatalf("the code leaked into a log line other than the display: %s", l)
			}
		}
	}
	if shownLines != 1 {
		t.Fatalf("the code is logged once, in %d lines", shownLines)
	}
}

// --- AC 2: CPace confirmed, pin and enrollment, "paired to <user>" ---------

func TestPairingCommitsPinAndEnrollmentAndReportsTheOwner(t *testing.T) {
	hubPub := make([]byte, 32)
	hubPub[0] = 7
	f := newPairFixture(t, pairOkStep(hubPub, "alice"))
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(f.out.String(), "paired to alice\n") {
		t.Fatalf("console:\n%s", f.out.String())
	}
	enr, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment not stored: %v %v", ok, err)
	}
	if string(enr.Pin.Pub) != string(hubPub) || enr.WorkerID != "w-1" || enr.Owner != "alice" || enr.Kind != "private" || !enr.Policy.PreviewOnly {
		t.Fatalf("enrollment = %+v", enr)
	}
	if r.enr.Owner != "alice" {
		t.Fatalf("Run returned %+v", r.enr)
	}
}

func TestEnrollmentIsCommittedBeforeSuccessIsPrinted(t *testing.T) {
	f := newPairFixture(t, pairOkStep(make([]byte, 32), "alice"))
	r, _ := f.run(f.pairer())
	eventually(t, func() bool { return strings.Contains(f.out.String(), "paired to") })
	if _, ok, _ := f.enr.Load(context.Background()); !ok {
		t.Fatal("paired to ... was printed before the enrollment was stored")
	}
	r.wait(t)
}

func TestWelcomeWithoutWorkerIDIsNotAPairing(t *testing.T) {
	bad := func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		return PairResult{Pin: identity.Pin{Pub: make([]byte, 32)}, Welcome: HubWelcome{Owner: "alice"}}, nil
	}
	f := newPairFixture(t, bad)
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err == nil {
		t.Fatal("a welcome without a worker id must fail")
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("nothing may be stored")
	}
	if strings.Contains(f.out.String(), "paired to") {
		t.Fatal("success must not be printed")
	}
}

// --- AC 3: old or rejected codes are replaced ------------------------------

func TestCodeOlderThanTenMinutesIsReplaced(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	f.codeSeq("K7Q4XY12", "B2C3D4E5")
	f.run(f.pairer())
	eventually(t, func() bool { return len(f.shown()) == 1 })

	f.step(PairCodeTTL - time.Second)
	time.Sleep(20 * time.Millisecond)
	if len(f.shown()) != 1 {
		t.Fatal("the code is still valid one second before its end")
	}
	f.clk.Advance(time.Second) // the code is now exactly 10 minutes old
	eventually(t, func() bool { return len(f.shown()) == 2 })
	if got := f.shown(); got[0] != "MN-K7Q4-XY12" || got[1] != "MN-B2C3-D4E5" {
		t.Fatalf("shown = %v", got)
	}
	if !strings.Contains(f.out.String(), "expired") {
		t.Fatalf("the new code says why:\n%s", f.out.String())
	}
	if f.dial.call(1).Code.String() != "MN-B2C3-D4E5" {
		t.Fatalf("the next connection uses the new code: %v", f.dial.call(1).Code)
	}
}

func TestExpiredCodeIsReplacedWhenTheHubWasUnreachableMeanwhile(t *testing.T) {
	// The code was shown, the connection broke, and the Hub stayed away longer
	// than the code lives: the retry must not reuse the old code.
	f := newPairFixture(t, pairWaitStep(errors.New("connection lost")), pairWaitStep(nil))
	f.codeSeq("K7Q4XY12", "B2C3D4E5")
	f.run(f.pairer())
	// OnWaiting also arms a lifetime timer, possibly after the attempt ends.
	// Only the backoff timer proves the pairer is ready to retry.
	f.waitBackoff()
	f.clk.Advance(PairCodeTTL + time.Second)
	eventually(t, func() bool { return len(f.shown()) == 2 })
	if f.dial.call(1).Code.String() != "MN-B2C3-D4E5" {
		t.Fatalf("code of the retry = %v", f.dial.call(1).Code)
	}
}

func TestRejectedCodesAreReplacedByAFreshOne(t *testing.T) {
	for _, c := range []proto.Code{proto.CodePairingAttemptsExceeded, proto.CodePairingIDCollision} {
		t.Run(string(c), func(t *testing.T) {
			f := newPairFixture(t, pairWaitStep(proto.NewError(c, "rejected")), pairWaitStep(nil))
			f.codeSeq("K7Q4XY12", "B2C3D4E5")
			f.run(f.pairer())
			eventually(t, func() bool { return len(f.shown()) == 2 })
			if got := f.shown(); got[0] == got[1] {
				t.Fatalf("the same code was shown twice: %v", got)
			}
			if got := f.dial.call(1).Code; got.String() != "MN-B2C3-D4E5" {
				t.Fatalf("second connection used %v", got)
			}
			// No backoff wait: the user is waiting for a code.
			if len(f.clk.recorded()) > 1 { // only the lifetime timers
				for _, d := range f.clk.recorded() {
					if d < PairCodeTTL {
						t.Fatalf("unexpected wait %s after a rejected code", d)
					}
				}
			}
		})
	}
}

func TestFreshCodeNeverReusesThePreviousPairingID(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(proto.NewError(proto.CodePairingIDCollision, "x")), pairWaitStep(nil))
	f.codeSeq("K7Q4XY12", "K7Q4ZZZZ", "B2C3D4E5") // the second code has the same id
	f.run(f.pairer())
	eventually(t, func() bool { return len(f.shown()) == 2 })
	if got := f.shown()[1]; got != "MN-B2C3-D4E5" {
		t.Fatalf("second code = %s, want one with another pairing id", got)
	}
}

func TestWrongCodeAtTheHubKeepsTheSameCodeUntilItIsDropped(t *testing.T) {
	wrong := pairWaitStep(proto.NewError(proto.CodePairingCodeInvalid, "the pairing code is wrong"))
	f := newPairFixture(t, wrong, wrong, pairWaitStep(proto.NewError(proto.CodePairingAttemptsExceeded, "dropped")), pairWaitStep(nil))
	f.codeSeq("K7Q4XY12", "B2C3D4E5")
	f.run(f.pairer())

	f.backoffStep()
	f.backoffStep()
	eventually(t, func() bool { return f.dial.nCalls() == 4 && len(f.shown()) == 2 })
	for i := range 3 {
		if got := f.dial.call(i).Code.String(); got != "MN-K7Q4-XY12" {
			t.Fatalf("connection %d used %s: a wrong typed code does not change the code", i, got)
		}
	}
	if f.dial.call(3).Code.String() != "MN-B2C3-D4E5" {
		t.Fatalf("after the Hub dropped the code: %v", f.dial.call(3).Code)
	}
	if strings.Count(f.out.String(), "MN-K7Q4-XY12") != 1 {
		t.Fatalf("the unchanged code is not shown again:\n%s", f.out.String())
	}
}

func TestRemovedWorkerKeyEndsPairingWithAClearError(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(proto.NewError(proto.CodeRevoked, "this Worker key was removed")))
	r, _ := f.run(f.pairer())
	r.wait(t)
	var pe *proto.Error
	if !errors.As(r.err, &pe) || pe.Code != proto.CodeRevoked {
		t.Fatalf("err = %v", r.err)
	}
	if !strings.Contains(r.err.Error(), "--new-code") {
		t.Fatalf("the error says what to do: %v", r.err)
	}
}

func TestTransientErrorsRetryWithTheSameCodeAndBackoff(t *testing.T) {
	f := newPairFixture(t,
		pairWaitStep(proto.NewError(proto.CodeWorkerOffline, "the Hub is shutting down")),
		pairWaitStep(proto.NewError(proto.CodeRateLimited, "too many")),
		pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	f.run(f.pairer())
	f.backoffStep()
	f.backoffStep()
	eventually(t, func() bool { return f.dial.nCalls() == 3 })
	for i := range 3 {
		if f.dial.call(i).Code.String() != "MN-K7Q4-XY12" {
			t.Fatalf("attempt %d changed the code", i)
		}
	}
}

// --- AC 4: a paired Worker refuses -----------------------------------------

func TestPairedWorkerRefusesWithAlreadyPaired(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}, Owner: "alice", Name: "pi"}); err != nil {
		t.Fatal(err)
	}
	r, _ := f.run(f.pairer())
	r.wait(t)
	var pe *proto.Error
	if !errors.As(r.err, &pe) || pe.Code != proto.CodeAlreadyPaired {
		t.Fatalf("err = %v, want already_paired", r.err)
	}
	if !strings.Contains(r.err.Error(), "alice") || !strings.Contains(r.err.Error(), "--new-code") {
		t.Fatalf("message = %v", r.err)
	}
	if f.dial.nCalls() != 0 || len(f.shown()) != 0 {
		t.Fatal("a paired Worker must not connect or show a code")
	}
	if _, err := f.enr.LoadKey(); !errors.Is(err, ErrNoWorkerKey) {
		t.Fatalf("a refused pairing must not touch the key: %v", err)
	}
}

func TestRenewWorkerIdentityCreatesANewKeyWipesTheEnrollmentAndKeepsTheOldIdentityForRevocation(t *testing.T) {
	f := newPairFixture(t)
	old, err := f.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	hubPub := make([]byte, 32)
	hubPub[0] = 3
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: hubPub, Seq: 2}, WorkerID: "w-old", Owner: "alice", HubURL: "https://old-hub.example"}); err != nil {
		t.Fatal(err)
	}
	k, err := RenewWorkerIdentity(context.Background(), f.enr)
	if err != nil {
		t.Fatal(err)
	}
	if string(k.Public()) == string(old.Public()) {
		t.Fatal("the key must change")
	}
	loaded, err := f.enr.LoadKey()
	if err != nil || string(loaded.Public()) != string(k.Public()) {
		t.Fatalf("the new key is the stored one: %v", err)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("the old enrollment must be gone")
	}
	// The old identity is kept, durably and privately, until its Hub confirmed.
	pending, err := f.enr.RetiredStore().List()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	p := pending[0]
	if string(p.Key.Public()) != string(old.Public()) || p.HubURL != "https://old-hub.example" ||
		string(p.Pin.Pub) != string(hubPub) || p.Pin.Seq != 2 || p.WorkerID != "w-old" {
		t.Fatalf("pending revocation = %+v", p)
	}
	// Pairing is possible again, with the new identity.
	f.dial.steps = []pairStep{pairOkStep(make([]byte, 32), "bob")}
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err != nil || r.enr.Owner != "bob" {
		t.Fatalf("pairing after the renewal: %v %+v", r.err, r.enr)
	}
	if string(f.dial.call(0).Key.Public()) != string(k.Public()) {
		t.Fatal("pairing must use the new key")
	}
	if again, _ := f.enr.RetiredStore().List(); len(again) != 1 {
		t.Fatalf("pairing must not touch the pending revocation: %d", len(again))
	}
}

func TestRenewWorkerIdentityOfAnEnrollmentWithoutAHubAddressIsRefusedAndChangesNothing(t *testing.T) {
	f := newPairFixture(t)
	old, _ := f.enr.LoadOrCreateKey()
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); !errors.Is(err, ErrUnknownEnrollmentHub) {
		t.Fatalf("err = %v", err)
	}
	if k, err := f.enr.LoadKey(); err != nil || string(k.Public()) != string(old.Public()) {
		t.Fatal("the key must be unchanged")
	}
	if _, ok, _ := f.enr.Load(context.Background()); !ok {
		t.Fatal("the enrollment must be unchanged")
	}
	if p, _ := f.enr.RetiredStore().List(); len(p) != 0 {
		t.Fatal("nothing may be recorded")
	}
}

func TestRenewWorkerIdentityOfAnUnpairedWorkerRecordsNothingAndKeepsEarlierRevocations(t *testing.T) {
	f := newPairFixture(t)
	if _, err := f.enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := f.enr.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: make([]byte, 32)}, Owner: "alice", HubURL: "https://old-hub.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	// Running it again on the now unpaired Worker (a repair after a crash, or
	// a second --new-code) must not lose the pending revocation.
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.enr.RetiredStore().List(); len(p) != 1 {
		t.Fatalf("pending revocations = %d, want the one of the old enrollment", len(p))
	}
}

func TestPairerNeedsAHubURL(t *testing.T) {
	f := newPairFixture(t)
	_, err := NewPairer(PairerConfig{Enrollment: f.enr, Dialer: f.dial})
	if !errors.Is(err, ErrNoHubURL) {
		t.Fatalf("err = %v", err)
	}
}

func TestTTLText(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Minute: "1 minute", 10 * time.Minute: "10 minutes", 90 * time.Second: "1m30s"} {
		if got := ttlText(d); got != want {
			t.Errorf("ttlText(%s) = %q, want %q", d, got, want)
		}
	}
}

// --- integration: in-process Hub, wrong-name certificate, no TLS flags ------

// stubPairHub is an in-process Hub made of the shared transport package as the
// Hub's connect endpoint uses it for pair_code (Story 4.8 provides the real
// responder): the connection is held after the Worker hello until the test
// "types" a code, which becomes the CPace password. Its self-signed certificate
// is issued for another host name, and the Worker side under test is the
// production pair dialer with no TLS option at all (A-15).
type stubPairHub struct {
	t *testing.T
	// key is the root (what a Worker pins); hub holds the operational key and
	// certificate the stub presents (protocol 3.0). Replace hub (under mu) to
	// answer with another root or serial.
	key  *identity.Key
	hub  *hubcert.Hub
	cert tls.Certificate

	mu     sync.Mutex
	srv    *httptest.Server
	addr   string
	ids    []string
	reject []error // errors for the next connections, consumed in order
	claims chan []byte
	// guess, when set, is the CPace password the Hub uses for every connection
	// at once (a hostile Hub testing a guess): it never waits for a claim.
	guess []byte

	connects atomic.Int32
	welcomed atomic.Int32
	// afterAuth runs once the handshake succeeded (the Hub received sig_worker),
	// before the welcome. dropBeforeWelcome closes that many authenticated
	// connections without a welcome (a lost welcome).
	afterAuth         func()
	dropBeforeWelcome atomic.Int32
}

func newStubPairHub(t *testing.T) *stubPairHub {
	t.Helper()
	hub := hubcert.New(t)
	h := &stubPairHub{t: t, key: hub.Root, hub: hub, cert: wrongNameCert(t), claims: make(chan []byte, 8)}
	h.start("127.0.0.1:0")
	t.Cleanup(h.stop)
	return h
}

func (h *stubPairHub) start(addr string) {
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

func (h *stubPairHub) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv = nil
	h.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (h *stubPairHub) url() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return "https://" + h.addr
}

func (h *stubPairHub) lastID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ids) == 0 {
		return ""
	}
	return h.ids[len(h.ids)-1]
}

func (h *stubPairHub) allIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ids...)
}

func (h *stubPairHub) idCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

func (h *stubPairHub) rejectNext(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reject = append(h.reject, err)
}

// claim types a code into the Hub: the CPace password of the waiting Worker.
func (h *stubPairHub) claim(code string) {
	pc, err := identity.ParsePairingCode(code)
	if err != nil {
		h.t.Fatal(err)
	}
	h.claims <- pc.Password()
}

// stubPairConn watches for a peer close while the password callback waits,
// just as the real Hub does. A hijacked HTTP request context does not report a
// WebSocket close. Peek preserves any byte for the next handshake Read, which
// waits for Peek to finish before using the reader.
type stubPairConn struct {
	net.Conn
	reader   *bufio.Reader
	peekDone chan struct{}
}

func (c *stubPairConn) watch() <-chan struct{} {
	c.peekDone = make(chan struct{})
	go func() {
		_, _ = c.reader.Peek(1)
		close(c.peekDone)
	}()
	return c.peekDone
}

func (c *stubPairConn) Read(p []byte) (int, error) {
	if c.peekDone != nil {
		<-c.peekDone
	}
	return c.reader.Read(p)
}

func (h *stubPairHub) serve(w http.ResponseWriter, r *http.Request) {
	h.connects.Add(1)
	raw, err := transport.Accept(w, r)
	if err != nil {
		return
	}
	conn := &stubPairConn{Conn: raw, reader: bufio.NewReader(raw)}
	h.mu.Lock()
	hc := h.hub.Config()
	h.mu.Unlock()
	hc.Timeout = 30 * time.Second
	hc.PairingPassword = func(id string) ([]byte, error) {
		h.mu.Lock()
		h.ids = append(h.ids, id)
		var rej error
		if len(h.reject) > 0 {
			rej, h.reject = h.reject[0], h.reject[1:]
		}
		guess := h.guess
		h.mu.Unlock()
		if rej != nil {
			return nil, rej
		}
		if guess != nil {
			return guess, nil
		}
		select {
		case pw := <-h.claims:
			return pw, nil
		case <-conn.watch():
			return nil, errors.New("worker gone")
		case <-r.Context().Done():
			return nil, errors.New("request ended")
		}
	}
	res, err := transport.HubHandshake(conn, hc)
	if err != nil {
		return // a wrong code: the transport sent pairing_code_invalid
	}
	h.mu.Lock()
	after := h.afterAuth
	h.mu.Unlock()
	if after != nil {
		after()
	}
	if h.dropBeforeWelcome.Load() > 0 {
		h.dropBeforeWelcome.Add(-1)
		_ = res.Channel.Close()
		return
	}
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit, map[string]any{
		"type": proto.TypeWelcome, "worker_id": "w-" + res.Worker.Name, "name": res.Worker.Name, "owner": "alice",
		"kind": "private", "policy": map[string]bool{"preview_only": false}, "proto_min": "1.0", "proto_max": "1.0",
	}); err != nil {
		return
	}
	h.welcomed.Add(1)
	sess, err := transport.NewSession(res.Channel, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		return
	}
	<-sess.Done()
}

type hubPairer struct {
	*Pairer
	enr  *Enrollments
	out  *logBuf
	logs *logBuf
	dir  string
}

func newHubPairer(t *testing.T, hubURL string, opts ...func(*PairerConfig)) *hubPairer {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hp := &hubPairer{enr: NewEnrollments(st, dir), out: &logBuf{}, logs: &logBuf{}, dir: dir}
	cfg := PairerConfig{
		HubURL:     hubURL,
		Name:       "pi",
		Enrollment: hp.enr,
		Dialer:     NewPairDialer(), // production: no TLS option exists, the certificate is never consulted
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		Out:        hp.out,
		Logger:     slog.New(slog.NewTextHandler(hp.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, o := range opts {
		o(&cfg)
	}
	if hp.Pairer, err = NewPairer(cfg); err != nil {
		t.Fatal(err)
	}
	return hp
}

func (hp *hubPairer) start(t *testing.T) *pairRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &pairRun{done: make(chan struct{})}
	go func() { defer close(r.done); r.enr, r.err = hp.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("the pairer did not stop")
		}
	})
	return r
}

func (hp *hubPairer) code(t *testing.T, n int) string {
	t.Helper()
	eventually(t, func() bool { return len(codeRE.FindAllString(hp.out.String(), -1)) >= n })
	return codeRE.FindAllString(hp.out.String(), -1)[n-1]
}

func TestPairsWithAWrongNameSelfSignedHubAndNoTLSFlag(t *testing.T) {
	h := newStubPairHub(t)
	hp := newHubPairer(t, h.url())
	r := hp.start(t)

	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code) // the user types the code into the Hub
	r.wait(t)

	if r.err != nil {
		t.Fatalf("pairing failed: %v\nlogs:\n%s", r.err, hp.logs.String())
	}
	if !strings.Contains(hp.out.String(), "paired to alice") {
		t.Fatalf("console:\n%s", hp.out.String())
	}
	enr, ok, err := hp.enr.Load(context.Background())
	if err != nil || !ok {
		t.Fatalf("enrollment: %v %v", ok, err)
	}
	if string(enr.Pin.Pub) != string(h.key.Public()) {
		t.Fatal("the Worker must pin the Hub key that CPace confirmed")
	}
	if enr.Owner != "alice" || enr.WorkerID != "w-pi" {
		t.Fatalf("enrollment = %+v", enr)
	}
	if h.welcomed.Load() != 1 {
		t.Fatalf("welcomed = %d", h.welcomed.Load())
	}
	if strings.Contains(hp.logs.String(), "hub_ca") || strings.Contains(hp.logs.String(), "insecure") {
		t.Fatal("pairing needs no TLS setting")
	}
}

func TestWrongCodeTypedInTheHubRetiresTheCodeAndTheFreshOneWorks(t *testing.T) {
	h := newStubPairHub(t)
	hp := newHubPairer(t, h.url())
	r := hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })

	wrong := code[:8] + otherChar(code[8]) + code[9:] // keep the id, change the rest
	h.claim(wrong)
	// The Hub answers the wrong code with pairing_code_invalid. The Worker has
	// sent its confirmation by then, so the code is retired: a fresh one is
	// shown, and it works.
	fresh := hp.code(t, 2)
	if fresh == code {
		t.Fatal("the code must be replaced")
	}
	eventually(t, func() bool { return h.idCount() >= 2 && h.lastID() == fresh[3:7] })
	h.claim(fresh)
	r.wait(t)
	if r.err != nil {
		t.Fatalf("err = %v\nlogs:\n%s", r.err, hp.logs.String())
	}
	if !strings.Contains(hp.out.String(), "A new code replaces it") {
		t.Fatalf("the console says why the code changed:\n%s", hp.out.String())
	}
}

func otherChar(c byte) string {
	if c == 'A' {
		return "B"
	}
	return "A"
}

func TestHubRejectingTheIDMakesTheWorkerShowAFreshCode(t *testing.T) {
	h := newStubPairHub(t)
	h.rejectNext(proto.NewError(proto.CodePairingIDCollision, "a Worker is already waiting with this pairing id"))
	hp := newHubPairer(t, h.url())
	r := hp.start(t)

	first := hp.code(t, 1)
	second := hp.code(t, 2)
	if first == second {
		t.Fatal("a fresh code was expected")
	}
	eventually(t, func() bool { return h.lastID() == second[3:7] })
	h.claim(second)
	r.wait(t)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
}

func TestCodeAppearsAfterTheHubBecomesReachable(t *testing.T) {
	// Reserve an address with nothing listening on it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	h := newStubPairHub(t)
	h.stop()
	hp := newHubPairer(t, "https://"+addr)
	r := hp.start(t)

	time.Sleep(150 * time.Millisecond) // several failed attempts
	if strings.Contains(hp.out.String(), "MN-") || strings.Contains(hp.logs.String(), "pairing_code") {
		t.Fatalf("no code may be shown before the first successful connection:\n%s", hp.out.String())
	}
	h.start(addr)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	r.wait(t)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
}

func TestPairerStopsOnContextCancelWhileWaiting(t *testing.T) {
	h := newStubPairHub(t)
	hp := newHubPairer(t, h.url())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := hp.Run(ctx); done <- err }()
	hp.code(t, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pairer did not stop")
	}
	if _, ok, _ := hp.enr.Load(context.Background()); ok {
		t.Fatal("nothing may be stored")
	}
}

// --- AD-8: a bad Hub confirmation ends the run ------------------------------

func TestBadHubConfirmationEndsPairingAndIsNeverRetriedWithTheSameCode(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(fmt.Errorf("pair: %w", transport.ErrPairingConfirm)), pairWaitStep(nil))
	f.codeSeq("K7Q4XY12")
	r, _ := f.run(f.pairer())
	r.wait(t)
	if !errors.Is(r.err, transport.ErrPairingConfirm) {
		t.Fatalf("err = %v, want ErrPairingConfirm", r.err)
	}
	for _, want := range []string{"could not prove", "intercepted", "nothing was stored"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Fatalf("the error must say %q: %v", want, r.err)
		}
	}
	if f.dial.nCalls() != 1 {
		t.Fatalf("Pair was called %d times, want exactly one", f.dial.nCalls())
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("nothing may be committed")
	}
	if strings.Contains(f.out.String(), "paired to") {
		t.Fatalf("success must not be printed:\n%s", f.out.String())
	}
	if strings.Contains(r.err.Error(), "XY12") {
		t.Fatalf("the error contains the code: %v", r.err)
	}
	for _, l := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(l, "XY12") && !strings.Contains(l, `msg="pairing code"`) {
			t.Fatalf("the code leaked into a log line: %s", l)
		}
	}
	if strings.Count(f.logs.String(), "level=WARN") != 1 {
		t.Fatalf("one Warn line expected:\n%s", f.logs.String())
	}
}

// --- AD-3/AD-4: backoff grows unless there was progress ---------------------

// delays returns the retry delays (at most a minute) recorded so far.
func (f *pairFixture) delays() []time.Duration {
	var out []time.Duration
	for _, d := range f.clk.recorded() {
		if d <= time.Minute {
			out = append(out, d)
		}
	}
	return out
}

func (f *pairFixture) waitDelays(n int) []time.Duration {
	f.t.Helper()
	for i := 0; i < n; i++ {
		f.backoffStep()
	}
	return f.delays()
}

func TestRefusalsAfterTheHelloGrowTheBackoff(t *testing.T) {
	for _, code := range []proto.Code{proto.CodeRateLimited, proto.CodeProtoIncompatible} {
		t.Run(string(code), func(t *testing.T) {
			e := proto.NewError(code, "refused after the hello")
			f := newPairFixture(t, pairWaitStep(e), pairWaitStep(e), pairWaitStep(e), pairWaitStep(nil))
			f.run(f.pairer())
			got := f.waitDelays(3)
			want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("delays = %v, want strictly growing %v", got, want)
			}
		})
	}
}

func TestWrongCodeAtTheHubRestartsTheBackoff(t *testing.T) {
	limited := pairWaitStep(proto.NewError(proto.CodeRateLimited, "slow down"))
	wrong := pairWaitStep(proto.NewError(proto.CodePairingCodeInvalid, "wrong code"))
	f := newPairFixture(t, limited, limited, wrong, limited, pairWaitStep(nil))
	f.run(f.pairer())
	got := f.waitDelays(4)
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v (a typed wrong code is followed by the initial delay)", got, want)
	}
}

func TestLongHeldConnectionRestartsTheBackoff(t *testing.T) {
	var f *pairFixture
	held := func(ctx context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		f.clk.Advance(pairHeldReset)
		return PairResult{}, proto.NewError(proto.CodeRateLimited, "idle timeout")
	}
	short := pairWaitStep(proto.NewError(proto.CodeRateLimited, "slow down"))
	f = newPairFixture(t, short, held, short, pairWaitStep(nil))
	f.run(f.pairer())
	got := f.waitDelays(3)
	want := []time.Duration{750 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}
