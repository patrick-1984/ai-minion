package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// ---------------------------------------------------------------- helpers

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// allowAll is a limiter that never refuses.
type allowAll struct{}

func (allowAll) Allow(ratelimit.Policy, netip.Addr, string) (bool, time.Duration) { return true, 0 }

var testIP = netip.MustParseAddr("192.0.2.10")

// unitPairings is a pairing service on a real store and a fake clock, without
// the Hub around it.
func unitPairings(t *testing.T, mutate func(*PairingOptions)) (*Pairings, *store.Store, *fakeClock) {
	t.Helper()
	st := openStore(t, t.TempDir())
	clk := newFakeClock()
	opts := PairingOptions{Store: st, Limiter: allowAll{}, Clock: clk.Now, Logger: textLogger(&syncBuf{})}
	if mutate != nil {
		mutate(&opts)
	}
	ps, err := NewPairings(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ps.Close)
	return ps, st, clk
}

// awaitResult is the outcome of an Await running in its own goroutine.
type awaitResult struct {
	ticket   *PairingTicket
	password []byte
	failed   func(error)
	err      error
}

func startAwait(ctx context.Context, ps *Pairings, id string, ip netip.Addr) <-chan awaitResult {
	ch := make(chan awaitResult, 1)
	go func() {
		t, err := ps.Await(ctx, id, ip)
		r := awaitResult{ticket: t, err: err}
		if t != nil {
			r.password, r.failed = t.Password(), t.HandshakeFailed
		}
		ch <- r
	}()
	return ch
}

func startAwaitAndWait(t *testing.T, ps *Pairings, id string) <-chan awaitResult {
	t.Helper()
	ch := startAwait(context.Background(), ps, id, testIP)
	eventually(t, "the Worker to be pending", func() bool { return ps.IsPending(id) })
	return ch
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func notReady[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s finished too early", what)
	case <-time.After(50 * time.Millisecond):
	}
}

type claimOut struct {
	res ClaimResult
	err error
}

func startClaim(ps *Pairings, user, code string) <-chan claimOut {
	ch := make(chan claimOut, 1)
	go func() {
		res, err := ps.Claim(context.Background(), ClaimRequest{UserID: user, Code: code, IP: testIP})
		ch <- claimOut{res, err}
	}()
	return ch
}

func wantPairingCode(t *testing.T, err error, want proto.Code) *proto.Error {
	t.Helper()
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != want {
		t.Fatalf("err = %v, want code %s", err, want)
	}
	return pe
}

func attemptsLeft(t *testing.T, pe *proto.Error) int {
	t.Helper()
	var d struct {
		Left int `json:"attempts_left"`
	}
	if err := json.Unmarshal(pe.Details, &d); err != nil {
		t.Fatalf("details %q: %v", pe.Details, err)
	}
	return d.Left
}

// wrongTail returns a code with the public id of code and a different rest.
func wrongTail(code identity.PairingCode) string {
	c := code.Canonical()
	tail := "0000"
	if c[4:] == tail {
		tail = "1111"
	}
	return c[:4] + tail
}

// ------------------------------------------------------- pending table, unit

func TestPendingPairingLivesTenMinutesAndThenExpires(t *testing.T) {
	ps, st, clk := unitPairings(t, nil)
	res := startAwaitAndWait(t, ps, "K7Q4")

	clk.Advance(PairingTTL - time.Second)
	if n := ps.Sweep(context.Background()); n != 0 {
		t.Fatalf("a pairing was expired after 9m59s (%d)", n)
	}
	notReady(t, res, "the pending Await")
	if ps.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", ps.Pending())
	}

	clk.Advance(time.Second)
	if n := ps.Sweep(context.Background()); n != 1 {
		t.Fatalf("expired = %d after 10m, want 1", n)
	}
	got := recv(t, res, "the expired Await")
	pe := wantPairingCode(t, got.err, proto.CodePairingCodeInvalid)
	if !strings.Contains(pe.Message, "expired") {
		t.Errorf("message = %q", pe.Message)
	}
	if ps.Pending() != 0 {
		t.Errorf("pending = %d after the expiry", ps.Pending())
	}
	// The code is dead: nobody is waiting for it.
	_, err := ps.Claim(context.Background(), ClaimRequest{UserID: "u1", Code: "K7Q4" + "ABCD", IP: testIP})
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)

	// The expired row is deleted by the sweep once the clock is past it.
	clk.Advance(time.Minute)
	ps.Sweep(context.Background())
	if _, err := st.Pairing(context.Background(), "K7Q4"); !errors.Is(err, store.ErrPairingNotFound) {
		t.Errorf("the expired row survived the sweep: %v", err)
	}
}

func TestAClaimAfterTheCodeExpiredIsRefusedEvenBeforeTheSweepRuns(t *testing.T) {
	ps, _, clk := unitPairings(t, nil)
	res := startAwaitAndWait(t, ps, "K7Q4")
	clk.Advance(PairingTTL)
	_, err := ps.Claim(context.Background(), ClaimRequest{UserID: "u1", Code: "K7Q4ABCD", IP: testIP})
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	ps.Close()
	recv(t, res, "Await after Close")
}

func TestSecondLivePendingConnectionWithTheSameIdIsRejected(t *testing.T) {
	ps, _, clk := unitPairings(t, nil)
	first := startAwaitAndWait(t, ps, "K7Q4")

	_, err := ps.Await(context.Background(), "K7Q4", netip.MustParseAddr("192.0.2.99"))
	_ = wantPairingCode(t, err, proto.CodePairingIDCollision)
	notReady(t, first, "the first pending Await") // the first connection is unaffected
	if ps.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", ps.Pending())
	}

	// Another id is no collision.
	other := startAwaitAndWait(t, ps, "ZZZZ")
	notReady(t, other, "the other pending Await")

	// After the first expired the id is free again.
	clk.Advance(PairingTTL)
	ps.Sweep(context.Background())
	recv(t, first, "the first Await to expire")
	again := startAwaitAndWait(t, ps, "K7Q4")
	notReady(t, again, "the new pending Await")
}

func TestAnExpiredPendingIdIsReusedEvenBeforeTheSweepRuns(t *testing.T) {
	ps, _, clk := unitPairings(t, nil)
	first := startAwaitAndWait(t, ps, "K7Q4")
	clk.Advance(PairingTTL + time.Second)
	second := startAwait(context.Background(), ps, "K7Q4", testIP)
	pe := wantPairingCode(t, recv(t, first, "the stale Await").err, proto.CodePairingCodeInvalid)
	if !strings.Contains(pe.Message, "expired") {
		t.Errorf("message = %q", pe.Message)
	}
	notReady(t, second, "the replacement Await")
}

func TestFailedAttemptsAreCountedAndTheFifthDropsThePairing(t *testing.T) {
	ps, st, _ := unitPairings(t, nil)
	ctx := context.Background()
	good := identity.NewPairingCode()
	id := good.PairingID()

	for attempt := 1; attempt <= PairingMaxAttempts; attempt++ {
		await := startAwaitAndWait(t, ps, id)
		claim := startClaim(ps, "u1", wrongTail(good))
		got := recv(t, await, "Await")
		if got.err != nil {
			t.Fatalf("attempt %d: Await failed: %v", attempt, got.err)
		}
		if string(got.password) != wrongTail(good) {
			t.Fatalf("the CPace password is not the typed code")
		}
		got.failed(transport.ErrPairingConfirm) // the handshake rejected the confirmation

		out := recv(t, claim, "Claim")
		if attempt < PairingMaxAttempts {
			pe := wantPairingCode(t, out.err, proto.CodePairingCodeInvalid)
			if left := attemptsLeft(t, pe); left != PairingMaxAttempts-attempt {
				t.Fatalf("attempt %d: attempts_left = %d, want %d", attempt, left, PairingMaxAttempts-attempt)
			}
			if row, _ := st.Pairing(ctx, id); row.Status != store.PairingPending || row.Attempts != attempt {
				t.Fatalf("attempt %d: row = %+v", attempt, row)
			}
		} else {
			_ = wantPairingCode(t, out.err, proto.CodePairingAttemptsExceeded)
		}
		if ps.Pending() != 0 {
			t.Fatalf("attempt %d: the pending connection survived the failed handshake", attempt)
		}
	}

	if row, _ := st.Pairing(ctx, id); row.Status != store.PairingDropped || row.Attempts != PairingMaxAttempts {
		t.Fatalf("row = %+v, want dropped after %d attempts", row, PairingMaxAttempts)
	}
	// The dropped pairing stays dead: the Worker that reconnects is told, and
	// the right code finds nobody waiting.
	_, err := ps.Await(ctx, id, testIP)
	_ = wantPairingCode(t, err, proto.CodePairingAttemptsExceeded)
	_, err = ps.Claim(ctx, ClaimRequest{UserID: "u1", Code: good.String(), IP: testIP})
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
}

func TestFailedAttemptsSurviveAWorkerReconnectAndAHubRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := hubstore.Open(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	clk := newFakeClock()
	newSvc := func() *Pairings {
		ps, err := NewPairings(PairingOptions{Store: st, Limiter: allowAll{}, Clock: clk.Now, Logger: textLogger(&syncBuf{})})
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	good := identity.NewPairingCode()
	fail := func(ps *Pairings) {
		t.Helper()
		await := startAwaitAndWait(t, ps, good.PairingID())
		claim := startClaim(ps, "u1", wrongTail(good))
		recv(t, await, "Await").failed(transport.ErrPairingConfirm)
		recv(t, claim, "Claim")
	}
	first := newSvc()
	fail(first)
	fail(first)
	first.Close() // the Hub restarts
	second := newSvc()
	defer second.Close()
	fail(second)
	row, err := st.Pairing(context.Background(), good.PairingID())
	if err != nil || row.Attempts != 3 {
		t.Fatalf("row = %+v err=%v, want 3 attempts across the restart", row, err)
	}
}

func TestAHandshakeThatFailsForAnotherReasonIsNotAnAttempt(t *testing.T) {
	ps, st, _ := unitPairings(t, nil)
	good := identity.NewPairingCode()
	await := startAwaitAndWait(t, ps, good.PairingID())
	claim := startClaim(ps, "u1", good.String())
	recv(t, await, "Await").failed(io.ErrUnexpectedEOF) // the Worker left
	_ = wantPairingCode(t, recv(t, claim, "Claim").err, proto.CodeWorkerOffline)
	if row, _ := st.Pairing(context.Background(), good.PairingID()); row.Attempts != 0 || row.Status != store.PairingPending {
		t.Errorf("row = %+v, want no attempt charged", row)
	}
	if ps.Pending() != 0 {
		t.Error("the dead connection is still pending")
	}
}

func TestAUsedCodeIsNeverAcceptedAgain(t *testing.T) {
	ps, st, _ := unitPairings(t, nil)
	ctx := context.Background()
	now := newFakeClock().Now()
	if err := st.PutPairing(ctx, store.PairingRow{
		ID: "USED", Status: store.PairingUsed, CreatedAt: store.UnixMS(now), ExpiresAt: store.UnixMS(now.Add(PairingTTL)),
		OwnerID: "u1", WorkerID: "w1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := ps.Await(ctx, "USED", testIP)
	_ = wantPairingCode(t, err, proto.CodePairingIDCollision)
	if ps.Pending() != 0 {
		t.Error("a used id was held as pending")
	}
	_, err = ps.Claim(ctx, ClaimRequest{UserID: "u1", Code: "USED1234", IP: testIP})
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
}

func TestClaimRefusesWhatCannotMatchAWaitingWorker(t *testing.T) {
	ps, _, _ := unitPairings(t, nil)
	ctx := context.Background()
	cases := map[string]ClaimRequest{
		"malformed code":     {UserID: "u1", Code: "not a code", IP: testIP},
		"empty code":         {UserID: "u1", Code: "", IP: testIP},
		"nobody is waiting":  {UserID: "u1", Code: "MN-K7Q4-9XTB", IP: testIP},
		"U is not Crockford": {UserID: "u1", Code: "MN-UUUU-UUUU", IP: testIP},
	}
	for name, req := range cases {
		_, err := ps.Claim(ctx, req)
		pe := wantPairingCode(t, err, proto.CodePairingCodeInvalid)
		if strings.Contains(pe.Message, "9XTB") {
			t.Errorf("%s: the message repeats the code: %q", name, pe.Message)
		}
	}
	_, err := ps.Claim(ctx, ClaimRequest{Code: "MN-K7Q4-9XTB", IP: testIP})
	_ = wantPairingCode(t, err, proto.CodeForbidden)
}

func TestTwoClaimsOfOneCodeAtOnceAreRefusedWithConflict(t *testing.T) {
	ps, _, _ := unitPairings(t, nil)
	good := identity.NewPairingCode()
	await := startAwaitAndWait(t, ps, good.PairingID())
	first := startClaim(ps, "u1", good.String())
	got := recv(t, await, "Await") // the first claim supplied the password; its handshake is "running"
	_, err := ps.Claim(context.Background(), ClaimRequest{UserID: "u2", Code: good.String(), IP: testIP})
	_ = wantPairingCode(t, err, proto.CodeConflict)
	got.failed(io.EOF)
	recv(t, first, "the first claim")
}

func TestClosingEndsPendingConnectionsAndRefusesNewOnes(t *testing.T) {
	ps, _, _ := unitPairings(t, nil)
	res := startAwaitAndWait(t, ps, "K7Q4")
	ps.Close()
	_ = wantPairingCode(t, recv(t, res, "Await after Close").err, proto.CodeWorkerOffline)
	_, err := ps.Await(context.Background(), "ABCD", testIP)
	_ = wantPairingCode(t, err, proto.CodeWorkerOffline)
	ps.Close() // twice is fine
}

func TestACancelledWaitReleasesTheId(t *testing.T) {
	ps, _, _ := unitPairings(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := startAwait(ctx, ps, "K7Q4", testIP)
	eventually(t, "pending", func() bool { return ps.IsPending("K7Q4") })
	cancel()
	_ = wantPairingCode(t, recv(t, res, "the cancelled Await").err, proto.CodeWorkerOffline)
	if ps.IsPending("K7Q4") || ps.Pending() != 0 {
		t.Error("a cancelled connection is still pending")
	}
	again := startAwaitAndWait(t, ps, "K7Q4")
	notReady(t, again, "the new Await")
}

func TestPendingConnectionsAreBounded(t *testing.T) {
	ps, _, _ := unitPairings(t, func(o *PairingOptions) { o.MaxPending = 3; o.MaxPendingPerIP = 2 })
	ipA, ipB := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1")
	hold := func(id string, ip netip.Addr) {
		t.Helper()
		startAwait(context.Background(), ps, id, ip)
		eventually(t, id+" pending", func() bool { return ps.IsPending(id) })
	}
	hold("AAA1", ipA)
	hold("AAA2", ipA)
	_, err := ps.Await(context.Background(), "AAA3", ipA) // the third of one client network
	_ = wantPairingCode(t, err, proto.CodeRateLimited)
	hold("BBB1", ipB)
	_, err = ps.Await(context.Background(), "BBB2", ipB) // the table is full
	_ = wantPairingCode(t, err, proto.CodeRateLimited)
	if ps.Pending() != 3 {
		t.Errorf("pending = %d, want 3", ps.Pending())
	}
}

// ------------------------------------------------------------ rate limiting

type captureLimiter struct {
	mu     sync.Mutex
	calls  []string
	policy ratelimit.Policy
	ip     netip.Addr
	deny   bool
}

func (l *captureLimiter) Allow(p ratelimit.Policy, ip netip.Addr, target string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, target)
	l.policy, l.ip = p, ip
	if l.deny {
		return false, 42 * time.Second
	}
	return true, 0
}

func TestClaimsAreLimitedByThePairingPolicyPerUserAndIP(t *testing.T) {
	lim := &captureLimiter{deny: true}
	ps, _, _ := unitPairings(t, func(o *PairingOptions) { o.Limiter = lim })
	_, err := ps.Claim(context.Background(), ClaimRequest{UserID: "u1", Code: "MN-K7Q4-9XTB", IP: testIP})
	pe := wantPairingCode(t, err, proto.CodeRateLimited)
	var rl interface{ RetryAfter() time.Duration }
	if !errors.As(err, &rl) || rl.RetryAfter() != 42*time.Second {
		t.Errorf("no retry-after in %v", err)
	}
	if pe.Message == "" || lim.policy != ratelimit.PolicyPairing || lim.ip != testIP || len(lim.calls) != 1 || lim.calls[0] != "user:u1" {
		t.Errorf("limiter saw policy=%s ip=%s targets=%v", lim.policy, lim.ip, lim.calls)
	}
}

func TestTheRealPairingLimiterStopsOneUserAndOneClientNetwork(t *testing.T) {
	lim, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ps, _, _ := unitPairings(t, func(o *PairingOptions) { o.Limiter = lim })
	ctx := context.Background()
	claim := func(user string, ip netip.Addr) error {
		_, err := ps.Claim(ctx, ClaimRequest{UserID: user, Code: "MN-K7Q4-9XTB", IP: ip})
		return err
	}
	// One user: the target bucket of the pairing policy holds 5.
	for i := 0; i < 5; i++ {
		_ = wantPairingCode(t, claim("u1", netip.MustParseAddr("192.0.2.1")), proto.CodePairingCodeInvalid)
	}
	_ = wantPairingCode(t, claim("u1", netip.MustParseAddr("192.0.2.2")), proto.CodeRateLimited)
	// One client network: the IP bucket holds 10 (5 are spent above by 192.0.2.1).
	shared := netip.MustParseAddr("198.51.100.7")
	for i := 0; i < 10; i++ {
		_ = wantPairingCode(t, claim("user-"+string(rune('a'+i)), shared), proto.CodePairingCodeInvalid)
	}
	_ = wantPairingCode(t, claim("user-z", shared), proto.CodeRateLimited)
}

// --------------------------------------------------------------- name rules

func TestWorkerNamesAreCleanedAndSuffixed(t *testing.T) {
	long := strings.Repeat("\u00e9", 100) // 200 bytes
	for in, want := range map[string]string{
		"pi":              "pi",
		"my raspberry pi": "my-raspberry-pi",
		"a:b":             "a-b",
		"  pi  ":          "pi",
		"":                "worker",
		"::::":            "worker",
		"tab\there":       "tab-here",
		"bad\xffbyte":     "bad-byte",
	} {
		if got := CleanWorkerName(in); got != want || !ops.ValidWorkerName(got) {
			t.Errorf("CleanWorkerName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanWorkerName(long); !ops.ValidWorkerName(got) || len(got) > ops.MaxWorkerNameLen {
		t.Errorf("a long name was cleaned to %d bytes %q", len(got), got)
	}
	if suffixed("pi", 1) != "pi" || suffixed("pi", 2) != "pi-2" || suffixed("pi", 13) != "pi-13" {
		t.Error("suffixed")
	}
	if got := suffixed(strings.Repeat("n", ops.MaxWorkerNameLen), 12); len(got) != ops.MaxWorkerNameLen || !strings.HasSuffix(got, "-12") {
		t.Errorf("a suffixed long name is %d bytes: %q", len(got), got)
	}
	if got := suffixed(CleanWorkerName(long), 9); !ops.ValidWorkerName(got) {
		t.Errorf("cut inside a character: %q", got)
	}
}

// ------------------------------------------------- in-process Hub, real CPace

// pairHub is a test Hub with the pairing service enabled.
type pairHub struct {
	*testHub
	ps  *Pairings
	clk *fakeClock
}

func newPairHub(t *testing.T, mutateSvc func(*ServiceOptions), mutate func(*PairingOptions)) *pairHub {
	t.Helper()
	h := newTestHub(t, mutateSvc)
	clk := newFakeClock()
	opts := PairingOptions{Limiter: allowAll{}, Clock: clk.Now}
	if mutate != nil {
		mutate(&opts)
	}
	ps, err := h.svc.EnablePairing(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.SetOwnerNames(func(_ context.Context, id string) string { return "user-" + id })
	return &pairHub{testHub: h, ps: ps, clk: clk}
}

// pairingWorker is the Worker side of a pairing running in its own goroutine.
type pairingWorker struct {
	res  *transport.WorkerResult
	err  error
	done chan struct{}
}

func (h *pairHub) startWorker(t *testing.T, key *identity.Key, code identity.PairingCode, name string) *pairingWorker {
	t.Helper()
	w := &pairingWorker{done: make(chan struct{})}
	cfg := transport.WorkerConfig{
		Key: key, Name: name, Ops: []string{"send_text", "sessions_list"}, Mode: transport.ModePairCode,
		PairingCode: code, Timeout: 30 * time.Second,
	}
	go func() {
		defer close(w.done)
		w.res, w.err = h.dial(cfg)
	}()
	t.Cleanup(func() {
		<-w.done
		if w.res != nil {
			_ = w.res.Channel.Close()
		}
	})
	return w
}

// start runs a Worker and waits until the Hub holds its connection as pending.
func (h *pairHub) start(t *testing.T, key *identity.Key, code identity.PairingCode, name string) *pairingWorker {
	t.Helper()
	w := h.startWorker(t, key, code, name)
	eventually(t, "the Hub to hold the Worker as pending", func() bool { return h.ps.IsPending(code.PairingID()) })
	return w
}

func (w *pairingWorker) wait(t *testing.T) {
	t.Helper()
	recv(t, w.done, "the Worker handshake")
}

func (h *pairHub) claim(user, code, name string) (ClaimResult, error) {
	return h.ps.Claim(context.Background(), ClaimRequest{UserID: user, Code: code, Name: name, IP: testIP})
}

func TestPairingWithTheRightCodeCreatesAPrivateWorkerOwnedByTheClaimingUser(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()
	w := h.start(t, key, code, "raspberry")

	// The user types the code in lower case with spaces: any form is accepted.
	typed := strings.ToLower(strings.ReplaceAll(code.String(), "-", " "))
	res, err := h.claim("u-alice", typed, "")
	if err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil {
		t.Fatalf("the Worker handshake failed: %v", w.err)
	}
	if !w.res.PinChanged || string(w.res.HubPub) != string(h.svc.Identity().Public()) {
		t.Error("the Worker did not learn the Hub key")
	}

	welcome := decodeWelcome(t, w.res)
	if welcome.Owner != "user-u-alice" || welcome.Kind != store.WorkerKindPrivate || welcome.Name != "raspberry" || welcome.WorkerID != res.WorkerID {
		t.Errorf("welcome = %+v, claim = %+v", welcome, res)
	}
	if res.Name != "raspberry" || res.Renamed {
		t.Errorf("claim result = %+v", res)
	}

	row, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if row.OwnerID != "u-alice" || row.Kind != store.WorkerKindPrivate || row.Name != "raspberry" || row.ID != res.WorkerID {
		t.Errorf("worker row = %+v", row)
	}
	eventually(t, "the Worker to be online", func() bool {
		on, ok := h.svc.Registry().Get(row.ID)
		return ok && on.OwnerID == "u-alice" && on.Announces("send_text")
	})
	if h.ps.Pending() != 0 {
		t.Errorf("pending = %d after the pairing", h.ps.Pending())
	}
	if p, err := h.st.Pairing(context.Background(), code.PairingID()); err != nil || p.Status != store.PairingUsed || p.OwnerID != "u-alice" || p.WorkerID != row.ID {
		t.Errorf("pairing row = %+v err=%v", p, err)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: "worker.pair"})
	if err != nil || len(page.Events) != 1 || page.Events[0].Actor != "u-alice" || page.Events[0].ActingAs != "" || page.Events[0].Target != row.ID {
		t.Errorf("audit = %+v err=%v", page.Events, err)
	}
	if strings.Contains(h.logs.String(), code.Canonical()) || strings.Contains(h.logs.String(), code.Canonical()[4:]) {
		t.Errorf("the pairing code reached the log:\n%s", h.logs)
	}
}

func TestPairingNameCanBeChosenAndCollisionsGetASuffix(t *testing.T) {
	h := newPairHub(t, nil, nil)
	h.enrol(newKey(t), "u1", "pi")

	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	res, err := h.claim("u1", code.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if res.Name != "pi-2" || !res.Renamed {
		t.Errorf("claim result = %+v, want pi-2 renamed", res)
	}
	if got := decodeWelcome(t, w.res); got.Name != "pi-2" {
		t.Errorf("welcome name = %q", got.Name)
	}

	code2 := identity.NewPairingCode()
	w2 := h.start(t, newKey(t), code2, "ignored announced name")
	res2, err := h.claim("u1", code2.String(), "lab-box")
	if err != nil {
		t.Fatal(err)
	}
	w2.wait(t)
	if res2.Name != "lab-box" || res2.Renamed {
		t.Errorf("claim result = %+v", res2)
	}

	// A third Worker of another user may use the same name.
	code3 := identity.NewPairingCode()
	w3 := h.start(t, newKey(t), code3, "my raspberry pi: lab")
	res3, err := h.claim("u2", code3.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	w3.wait(t)
	if res3.Name != "my-raspberry-pi--lab" || res3.Renamed {
		t.Errorf("claim result = %+v, want the announced name cleaned", res3)
	}
}

func TestAWrongCodeCountsAnAttemptAndTheWorkerCanTryAgainWithTheSameCode(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()

	w := h.start(t, key, code, "pi")
	_, err := h.claim("u1", wrongTail(code), "")
	pe := wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	if left := attemptsLeft(t, pe); left != PairingMaxAttempts-1 {
		t.Errorf("attempts_left = %d", left)
	}
	w.wait(t)
	if proto.CodeOf(w.err) != proto.CodePairingCodeInvalid {
		t.Fatalf("the Worker saw %v, want pairing_code_invalid", w.err)
	}
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Fatalf("a Worker was created by a wrong code: %v", err)
	}
	if row, _ := h.st.Pairing(context.Background(), code.PairingID()); row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}

	// The Worker reconnects with the same code; the right code now pairs it.
	w2 := h.start(t, key, code, "pi")
	if _, err := h.claim("u1", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w2.wait(t)
	if w2.err != nil {
		t.Fatalf("second handshake: %v", w2.err)
	}
	_ = decodeWelcome(t, w2.res)
}

func TestFiveWrongCodesDropThePairing(t *testing.T) {
	h := newPairHub(t, nil, nil)
	key := newKey(t)
	code := identity.NewPairingCode()

	for attempt := 1; attempt <= PairingMaxAttempts; attempt++ {
		w := h.start(t, key, code, "pi")
		_, err := h.claim("u1", wrongTail(code), "")
		if attempt < PairingMaxAttempts {
			_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
		} else {
			_ = wantPairingCode(t, err, proto.CodePairingAttemptsExceeded)
		}
		w.wait(t)
		if proto.CodeOf(w.err) != proto.CodePairingCodeInvalid {
			t.Fatalf("attempt %d: the Worker saw %v", attempt, w.err)
		}
	}

	// The Worker that reconnects is told that the code is gone.
	w := h.startWorker(t, key, code, "pi")
	w.wait(t)
	if proto.CodeOf(w.err) != proto.CodePairingAttemptsExceeded {
		t.Fatalf("the Worker saw %v, want pairing_attempts_exceeded", w.err)
	}
	// Even the right code pairs nothing now.
	_, err := h.claim("u1", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	if _, err := h.st.WorkerByPub(context.Background(), key.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Fatalf("a Worker was created from a dropped pairing: %v", err)
	}
	page, err := h.audit.List(context.Background(), audit.Filter{Action: "pairing.attempt"})
	if err != nil || len(page.Events) != PairingMaxAttempts || page.Events[0].Outcome != "attempts_exceeded" {
		t.Errorf("audit = %+v err=%v", page.Events, err)
	}
}

func TestASecondLivePendingWorkerWithTheSameIdGetsPairingIDCollision(t *testing.T) {
	h := newPairHub(t, nil, nil)
	code := identity.NewPairingCode()
	first := h.start(t, newKey(t), code, "first")

	second := h.startWorker(t, newKey(t), code, "second")
	second.wait(t)
	if proto.CodeOf(second.err) != proto.CodePairingIDCollision {
		t.Fatalf("the second Worker saw %v, want pairing_id_collision", second.err)
	}

	// The first one is not disturbed and still pairs.
	if _, err := h.claim("u1", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	first.wait(t)
	if first.err != nil {
		t.Fatalf("the first Worker: %v", first.err)
	}
	if got := decodeWelcome(t, first.res); got.Name != "first" {
		t.Errorf("welcome = %+v", got)
	}
}

func TestACodeThatWasUsedOnceCannotBeUsedAgain(t *testing.T) {
	h := newPairHub(t, nil, nil)
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	if _, err := h.claim("u1", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	_ = decodeWelcome(t, w.res)

	// Entering it again finds nobody waiting.
	_, err := h.claim("u1", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	// Another Worker that announces the same id is refused before any exchange.
	again := h.startWorker(t, newKey(t), code, "pi2")
	again.wait(t)
	if proto.CodeOf(again.err) != proto.CodePairingIDCollision {
		t.Fatalf("the second Worker saw %v, want pairing_id_collision", again.err)
	}
	if n := len(h.svc.Registry().List()); n != 1 {
		t.Errorf("%d Workers online, want 1", n)
	}
	// It stays used after a Hub restart of the pairing service.
	h.ps.Close()
	fresh, err := NewPairings(PairingOptions{Store: h.st, Limiter: allowAll{}, Clock: h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	_, err = fresh.Await(context.Background(), code.PairingID(), testIP)
	_ = wantPairingCode(t, err, proto.CodePairingIDCollision)
}

func TestThePendingWaitIsNotLimitedByTheHandshakeTimeout(t *testing.T) {
	h := newPairHub(t, func(o *ServiceOptions) { o.HandshakeTimeout = 300 * time.Millisecond }, nil)
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	time.Sleep(900 * time.Millisecond) // three times the handshake budget
	if !h.ps.IsPending(code.PairingID()) {
		t.Fatal("the Hub dropped the pending connection when the handshake budget ran out")
	}
	if _, err := h.claim("u1", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil {
		t.Fatalf("the Worker handshake: %v", w.err)
	}
	_ = decodeWelcome(t, w.res)

	// A connect handshake that stalls is still cut by the budget.
	conn, err := transport.Dial(context.Background(), h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("a silent connection was never cut")
	}
}

func TestAPendingWorkerIsDroppedAfterTenMinutesAndToldWhy(t *testing.T) {
	h := newPairHub(t, nil, nil)
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")

	h.clk.Advance(PairingTTL - time.Second)
	h.ps.Sweep(context.Background())
	if !h.ps.IsPending(code.PairingID()) {
		t.Fatal("dropped before ten minutes")
	}
	h.clk.Advance(time.Second)
	h.ps.Sweep(context.Background())
	w.wait(t)
	if proto.CodeOf(w.err) != proto.CodePairingCodeInvalid || !strings.Contains(w.err.Error(), "expired") {
		t.Fatalf("the Worker saw %v, want an expiry", w.err)
	}
	_, err := h.claim("u1", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
}

func TestARemovedWorkerKeyAndAnEnrolledKeyCannotPair(t *testing.T) {
	h := newPairHub(t, nil, nil)
	ctx := context.Background()

	removed := newKey(t)
	row := h.enrol(removed, "u1", "gone")
	if err := h.st.TombstoneWorker(ctx, row.ID, "admin", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	code := identity.NewPairingCode()
	w := h.start(t, removed, code, "gone")
	_, err := h.claim("u1", code.String(), "")
	_ = wantPairingCode(t, err, proto.CodeRevoked)
	w.wait(t)
	expectRefusal(t, w, proto.CodeRevoked)

	enrolled := newKey(t)
	h.enrol(enrolled, "u1", "here")
	code2 := identity.NewPairingCode()
	w2 := h.start(t, enrolled, code2, "here")
	_, err = h.claim("u2", code2.String(), "")
	_ = wantPairingCode(t, err, proto.CodeAlreadyPaired)
	w2.wait(t)
	expectRefusal(t, w2, proto.CodeAlreadyPaired)
	if owner, _ := h.st.WorkerOwner(ctx, mustWorkerID(t, h, enrolled)); owner != "u1" {
		t.Errorf("the enrolled Worker changed owner to %q", owner)
	}
	if h.ps.Pending() != 0 {
		t.Error("a refused pairing is still pending")
	}
}

// expectRefusal checks that the handshake succeeded (the code was right) and
// that the Hub then answered with an error frame instead of welcome.
func expectRefusal(t *testing.T, w *pairingWorker, code proto.Code) {
	t.Helper()
	if w.err != nil {
		t.Fatalf("the handshake failed: %v", w.err)
	}
	m := readControl(t, w.res)
	if frameType(t, m) != proto.TypeError {
		t.Fatalf("the Worker got a %q frame, want error", frameType(t, m))
	}
	var got proto.Code
	if err := json.Unmarshal(m["code"], &got); err != nil || got != code {
		t.Fatalf("the Worker was refused with %q, want %s", m["code"], code)
	}
}

func mustWorkerID(t *testing.T, h *pairHub, key *identity.Key) string {
	t.Helper()
	w, err := h.st.WorkerByPub(context.Background(), key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func TestPairCodeModeIsOnlyServedAfterEnablePairing(t *testing.T) {
	h := newTestHub(t, nil)
	if h.svc.Pairings() != nil {
		t.Fatal("pairing is on before EnablePairing")
	}
	cfg := transport.WorkerConfig{Key: newKey(t), Name: "w", Mode: transport.ModePairCode, PairingCode: identity.NewPairingCode(), Timeout: 10 * time.Second}
	if _, err := h.dial(cfg); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("err = %v, want invalid_argument", err)
	}
	if _, err := h.svc.EnablePairing(PairingOptions{}); err != nil {
		t.Fatal(err)
	}
	if h.svc.Pairings() == nil {
		t.Fatal("no pairing service after EnablePairing")
	}
	if _, err := h.svc.EnablePairing(PairingOptions{}); err == nil {
		t.Error("pairing was enabled twice")
	}
}

func TestClosingTheServiceEndsPendingWorkers(t *testing.T) {
	h := newPairHub(t, nil, nil)
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	h.svc.Close()
	w.wait(t)
	if w.err == nil {
		t.Fatal("a pending Worker was admitted while the Hub shut down")
	}
}

func TestNewPairingsNeedsAStore(t *testing.T) {
	if _, err := NewPairings(PairingOptions{}); err == nil {
		t.Error("no error without a store")
	}
}

// ------------------------------------------ binding a claim to its connection

func TestAnExpiredEntryWhoseHandshakeIsRunningIsNeverDisplacedByAnotherAwait(t *testing.T) {
	ps, st, clk := unitPairings(t, nil)
	first := startAwaitAndWait(t, ps, "K7Q4")
	claimed := startClaim(ps, "u1", "K7Q4ABCD")
	a := recv(t, first, "the first Await")
	if a.err != nil || a.ticket == nil {
		t.Fatalf("the first Await failed: %v", a.err)
	}

	// The code expires while the first handshake is running.
	clk.Advance(PairingTTL + time.Minute)
	_, err := ps.Await(context.Background(), "K7Q4", netip.MustParseAddr("192.0.2.99"))
	_ = wantPairingCode(t, err, proto.CodePairingIDCollision)
	if !ps.IsPending("K7Q4") || ps.Pending() != 1 {
		t.Fatal("the entry whose handshake is running was ended")
	}
	notReady(t, claimed, "the claim")

	// The first handshake still owns the entry: its failure is charged to it.
	a.failed(transport.ErrPairingConfirm)
	pe := wantPairingCode(t, recv(t, claimed, "the claim").err, proto.CodePairingCodeInvalid)
	if left := attemptsLeft(t, pe); left != PairingMaxAttempts-1 {
		t.Errorf("attempts_left = %d", left)
	}
	if row, err := st.Pairing(context.Background(), "K7Q4"); err != nil || row.Attempts != 1 {
		t.Errorf("row = %+v err=%v", row, err)
	}
}

func TestAHandshakeFailureOfAnEndedEntryIsNotChargedToTheNewRowOfTheSameId(t *testing.T) {
	ps, st, _ := unitPairings(t, nil)
	first := startAwaitAndWait(t, ps, "K7Q4")
	startClaim(ps, "u1", "K7Q4ABCD")
	a := recv(t, first, "the first Await")
	// The first entry ends (here: the service forgets it), and another
	// Worker registers the same id and gets a claim of its own.
	ps.end(a.ticket.p, errExpired())
	second := startAwaitAndWait(t, ps, "K7Q4")
	claimed := startClaim(ps, "u2", "K7Q4WXYZ")
	b := recv(t, second, "the second Await")
	if b.err != nil {
		t.Fatal(b.err)
	}

	a.failed(transport.ErrPairingConfirm)
	if row, err := st.Pairing(context.Background(), "K7Q4"); err != nil || row.Attempts != 0 || row.Status != store.PairingPending {
		t.Errorf("the second row was charged: %+v err=%v", row, err)
	}
	if !ps.IsPending("K7Q4") {
		t.Error("the second entry was ended by the first handshake")
	}
	notReady(t, claimed, "the second claim")
}

// interpose runs before the pairing handler of every pair_code connection.
type interpose struct {
	ps     *Pairings
	before func(c *Conn)
}

func (i interpose) Handle(ctx context.Context, c *Conn) error {
	i.before(c)
	return i.ps.Handle(ctx, c)
}

func TestAHandshakeThatSucceedsAfterItsEntryWasReplacedNeverPairsWithTheNewClaim(t *testing.T) {
	h := newPairHub(t, nil, nil)
	code := identity.NewPairingCode()
	keyA := newKey(t)
	var claimU2 <-chan claimOut
	h.svc.handler.mu.Lock()
	h.svc.handler.modes[transport.ModePairCode] = interpose{ps: h.ps, before: func(c *Conn) {
		// Worker A's handshake succeeded with U1's claim. Before the Hub
		// acts on it, A's entry is gone and Worker B registered the same id
		// and was claimed by U2.
		h.ps.end(c.pairing.p, errExpired())
		go func() { _, _ = h.ps.Await(context.Background(), code.PairingID(), testIP) }()
		eventually(t, "worker B to be pending", func() bool { return h.ps.IsPending(code.PairingID()) })
		claimU2 = startClaim(h.ps, "u-bob", code.String())
		eventually(t, "U2's claim to be in flight", func() bool {
			h.ps.mu.Lock()
			defer h.ps.mu.Unlock()
			p := h.ps.live[code.PairingID()]
			return p != nil && p.cur != nil
		})
	}}
	h.svc.handler.mu.Unlock()

	w := h.start(t, keyA, code, "a")
	if _, err := h.claim("u-alice", code.String(), ""); err == nil {
		t.Fatal("U1's claim succeeded although its entry ended")
	}
	w.wait(t)
	expectRefusal(t, w, proto.CodePairingCodeInvalid)
	if _, err := h.st.WorkerByPub(context.Background(), keyA.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Fatalf("worker A was created: %v", err)
	}
	if row, err := h.st.Pairing(context.Background(), code.PairingID()); err != nil || row.Status != store.PairingPending || row.OwnerID != "" {
		t.Errorf("pairing row = %+v err=%v", row, err)
	}
	notReady(t, claimU2, "U2's claim")
	if n := len(h.svc.Registry().List()); n != 0 {
		t.Errorf("%d Workers online, want 0", n)
	}
}

// -------------------------------------------------------------- audit identity

func TestPairingAuditsTheActorAndActingAsOfTheClaimAndTheAttempt(t *testing.T) {
	h := newPairHub(t, nil, nil)
	ctx := context.Background()
	claimAs := func(text string) error {
		_, err := h.ps.Claim(ctx, ClaimRequest{UserID: "u-alice", Actor: "admin-1", ActingAs: "u-alice", Code: text, IP: testIP})
		return err
	}

	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	_ = wantPairingCode(t, claimAs(wrongTail(code)), proto.CodePairingCodeInvalid)
	w.wait(t)
	page, err := h.audit.List(ctx, audit.Filter{Action: "pairing.attempt"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v err=%v", page.Events, err)
	}
	if e := page.Events[0]; e.Actor != "admin-1" || e.ActingAs != "u-alice" || e.Target != code.PairingID() || e.Outcome != "wrong_code" {
		t.Errorf("pairing.attempt = %+v", e)
	}

	key2 := newKey(t)
	w2 := h.start(t, key2, code, "pi")
	if err := claimAs(code.String()); err != nil {
		t.Fatal(err)
	}
	w2.wait(t)
	page, err = h.audit.List(ctx, audit.Filter{Action: "worker.pair"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("audit = %+v err=%v", page.Events, err)
	}
	if e := page.Events[0]; e.Actor != "admin-1" || e.ActingAs != "u-alice" || e.Outcome != "pending" {
		t.Errorf("worker.pair = %+v", e)
	}
	// The owner is still the target user, not the admin.
	if row, err := h.st.WorkerByPub(ctx, key2.Public()); err != nil || row.OwnerID != "u-alice" {
		t.Errorf("worker row = %+v err=%v", row, err)
	}
}

// ------------------------------------------- a Worker that goes away, replays

// rawWorker is a Worker that dials in pair_code mode and keeps its connection,
// so a test can close it while the Hub holds it as pending.
func (h *pairHub) rawWorker(t *testing.T, key *identity.Key, code identity.PairingCode, name string) (net.Conn, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, h.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
			Key: key, Name: name, Ops: []string{"send_text"}, Mode: transport.ModePairCode,
			PairingCode: code, Timeout: 30 * time.Second,
		})
		done <- err
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn, done
}

func (ps *Pairings) ipSlots() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	n := 0
	for _, c := range ps.perIP {
		n += c
	}
	return n
}

func TestAPendingWorkerThatClosesItsConnectionIsDroppedAndItsIdIsFree(t *testing.T) {
	h := newPairHub(t, nil, nil)
	ctx := context.Background()
	key := newKey(t)
	code := identity.NewPairingCode()
	conn, done := h.rawWorker(t, key, code, "pi")
	eventually(t, "the Hub to hold the Worker as pending", func() bool { return h.ps.IsPending(code.PairingID()) })

	// A failed attempt is already on the row, and the clock moves on, so a
	// row that was recreated instead of kept would show a different expiry.
	before, err := h.st.AddPairingFailure(ctx, code.PairingID(), PairingMaxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Minute)

	_ = conn.Close() // the Worker goes away
	eventually(t, "the Hub to notice that the Worker is gone", func() bool { return !h.ps.IsPending(code.PairingID()) })
	if h.ps.Pending() != 0 || h.ps.ipSlots() != 0 {
		t.Errorf("pending = %d, per-IP slots = %d after the Worker left", h.ps.Pending(), h.ps.ipSlots())
	}
	_ = recv(t, done, "the Worker handshake to end")

	// The same key with the same code comes back: pending again, not a
	// collision, with the stored attempts and expiry.
	w := h.start(t, key, code, "pi")
	after, err := h.st.Pairing(ctx, code.PairingID())
	if err != nil || after.Attempts != before.Attempts || after.ExpiresAt != before.ExpiresAt || after.Status != store.PairingPending {
		t.Errorf("row before = %+v, after = %+v, err = %v", before, after, err)
	}
	res, err := h.claim("u1", code.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if w.err != nil {
		t.Fatalf("the Worker handshake: %v", w.err)
	}
	if got := decodeWelcome(t, w.res); got.WorkerID != res.WorkerID {
		t.Errorf("welcome = %+v, claim = %+v", got, res)
	}
}

func TestWatchConnHandsTheBackgroundReadToTheNextRead(t *testing.T) {
	read := func(t *testing.T, c net.Conn, n int) []byte {
		t.Helper()
		buf := make([]byte, n)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		return buf
	}

	t.Run("data out of turn ends the wait and is not lost", func(t *testing.T) {
		hub, worker := net.Pipe()
		defer func() { _ = hub.Close(); _ = worker.Close() }()
		wc := newWatchConn(hub)
		broke := make(chan struct{})
		wc.watch(func() { close(broke) })
		go func() { _, _ = worker.Write([]byte("abc")) }()
		recv(t, broke, "the watcher to report the byte")
		if got := string(read(t, wc, 3)); got != "abc" {
			t.Errorf("read %q, want abc", got)
		}
	})
	t.Run("a closed peer ends the wait and the read reports it", func(t *testing.T) {
		hub, worker := net.Pipe()
		defer func() { _ = hub.Close() }()
		wc := newWatchConn(hub)
		broke := make(chan struct{})
		wc.watch(func() { close(broke) })
		_ = worker.Close()
		recv(t, broke, "the watcher to report the close")
		if _, err := wc.Read(make([]byte, 1)); err == nil {
			t.Error("read succeeded on a closed peer")
		}
	})
	t.Run("a silent peer is not a break, and deadlines and close pass through", func(t *testing.T) {
		hub, worker := net.Pipe()
		defer func() { _ = worker.Close() }()
		wc := newWatchConn(hub)
		broke := make(chan struct{})
		wc.watch(func() { close(broke) })
		select {
		case <-broke:
			t.Fatal("reported a break on a quiet peer")
		case <-time.After(50 * time.Millisecond):
		}
		_ = wc.SetDeadline(time.Now().Add(20 * time.Millisecond)) // reaches the read in progress
		recv(t, broke, "the deadline to end the background read")
		if _, err := wc.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("read error = %v, want the deadline error", err)
		}
		_ = wc.Close()
		if _, err := worker.Write([]byte("x")); err == nil {
			t.Error("Close did not reach the connection")
		}
	})
}

func TestACodeThatWasUsedCannotBeReplayedAfterItsRowExpiredWasSweptAndTheHubRestarted(t *testing.T) {
	h := newPairHub(t, nil, nil)
	ctx := context.Background()
	code := identity.NewPairingCode()
	w := h.start(t, newKey(t), code, "pi")
	if _, err := h.claim("u1", code.String(), ""); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	_ = decodeWelcome(t, w.res)

	// Ten minutes later the used row is expired and swept; the public id is
	// free again, but the code is not.
	h.clk.Advance(PairingTTL + time.Second)
	h.ps.Sweep(ctx)
	if _, err := h.st.Pairing(ctx, code.PairingID()); !errors.Is(err, store.ErrPairingNotFound) {
		t.Fatalf("the used row survived the sweep: %v", err)
	}
	second := newKey(t)
	replay := h.start(t, second, code, "replay")
	_, err := h.claim("u2", code.String(), "")
	pe := wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	if !strings.Contains(pe.Message, "already used") {
		t.Errorf("message = %q", pe.Message)
	}
	if _, err := h.st.WorkerByPub(ctx, second.Public()); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the replayed code created a Worker: %v", err)
	}
	h.ps.Close() // ends the replaying Worker's wait
	replay.wait(t)

	// The digest is in the database: it survives a restart of the service,
	// and it is not the code.
	fresh, err := NewPairings(PairingOptions{Store: h.st, Limiter: allowAll{}, Clock: h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	_, err = fresh.Claim(ctx, ClaimRequest{UserID: "u3", Code: code.String(), IP: testIP})
	pe = wantPairingCode(t, err, proto.CodePairingCodeInvalid)
	if !strings.Contains(pe.Message, "already used") {
		t.Errorf("message after a restart = %q", pe.Message)
	}
}

// ------------------------------------------ round-2 fixes: sweep and replacement

func TestTheSweepKeepsTheRowOfAClaimWhoseHandshakeIsRunningUntilItEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail bool
	}{{"failed confirmation", true}, {"completion", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ps, st, clk := unitPairings(t, nil)
			ctx := context.Background()
			first := startAwaitAndWait(t, ps, "K7Q4")
			claimed := startClaim(ps, "u1", "K7Q4ABCD")
			a := recv(t, first, "the Await")
			if a.err != nil {
				t.Fatal(a.err)
			}
			// The code expires while the handshake runs, and the sweep runs.
			clk.Advance(PairingTTL + time.Minute)
			ps.Sweep(ctx)
			if _, err := st.Pairing(ctx, "K7Q4"); err != nil {
				t.Fatalf("the sweep deleted the row of a running handshake: %v", err)
			}
			if tc.fail {
				a.failed(transport.ErrPairingConfirm)
				pe := wantPairingCode(t, recv(t, claimed, "the claim").err, proto.CodePairingCodeInvalid)
				if left := attemptsLeft(t, pe); left != PairingMaxAttempts-1 {
					t.Errorf("attempts_left = %d", left)
				}
			} else {
				row := store.WorkerRow{ID: store.NewID(), Name: "w", Pub: bytes.Repeat([]byte{7}, 32), OwnerID: "u1", Kind: store.WorkerKindPrivate, CreatedAt: 1}
				if err := st.CompletePairing(ctx, "K7Q4", "digest", row); err != nil {
					t.Fatalf("CompletePairing after the sweep: %v", err)
				}
			}
			ps.end(a.ticket.p, errExpired())
			ps.Sweep(ctx)
			if _, err := st.Pairing(ctx, "K7Q4"); !errors.Is(err, store.ErrPairingNotFound) {
				t.Errorf("the expired row survived once the handshake ended: %v", err)
			}
		})
	}
}

func TestAnExpiredIdReplacedFromAnotherAddressStaysWithinThatAddressLimit(t *testing.T) {
	ipA, ipB := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1")
	ps, _, clk := unitPairings(t, func(o *PairingOptions) { o.MaxPendingPerIP = 1 })
	first := startAwait(context.Background(), ps, "K7Q4", ipA)
	eventually(t, "A to be pending", func() bool { return ps.IsPending("K7Q4") })
	other := startAwait(context.Background(), ps, "M2P9", ipB)
	eventually(t, "B to be pending", func() bool { return ps.IsPending("M2P9") })
	clk.Advance(PairingTTL + time.Second)

	// B is at its limit: taking over A's expired id must not give B a second slot.
	_, err := ps.Await(context.Background(), "K7Q4", ipB)
	_ = wantPairingCode(t, err, proto.CodeRateLimited)
	if !ps.IsPending("K7Q4") {
		t.Error("the refused replacement removed the old entry")
	}
	notReady(t, first, "A's Await")
	notReady(t, other, "B's Await")
}

func TestAnExpiredIdReplacedFromTheSameAddressReusesItsSlot(t *testing.T) {
	ps, _, clk := unitPairings(t, func(o *PairingOptions) { o.MaxPendingPerIP = 1 })
	first := startAwaitAndWait(t, ps, "K7Q4")
	clk.Advance(PairingTTL + time.Second)
	second := startAwait(context.Background(), ps, "K7Q4", testIP)
	pe := wantPairingCode(t, recv(t, first, "the stale Await").err, proto.CodePairingCodeInvalid)
	if !strings.Contains(pe.Message, "expired") {
		t.Errorf("message = %q", pe.Message)
	}
	notReady(t, second, "the replacement Await")
	if ps.Pending() != 1 {
		t.Errorf("pending = %d", ps.Pending())
	}
}
