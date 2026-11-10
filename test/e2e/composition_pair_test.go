//go:build e2e

package e2e

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 9.4 composition cases: the real Hub (connect endpoint with pairing
// enabled, over TLS with a certificate for another host name), the production
// pair dialer, the real Pairer and the real connector, with the Worker state
// on disk. Nothing below fakes a transport, a handshake or a store.

// compWorkerState is one Worker state directory that can be closed and opened
// again, as a process restart does.
type compWorkerState struct {
	t   *testing.T
	dir string
	st  *wstore.Store
	enr *wagent.Enrollments
}

func newCompWorkerState(t *testing.T) *compWorkerState {
	t.Helper()
	w := &compWorkerState{t: t, dir: t.TempDir()}
	w.open()
	t.Cleanup(w.close)
	return w
}

func (w *compWorkerState) open() {
	w.t.Helper()
	st, err := wstore.Open(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	w.st, w.enr = st, wagent.NewEnrollments(st, w.dir)
}

func (w *compWorkerState) close() {
	if w.st != nil {
		_ = w.st.Close()
		w.st = nil
	}
}

// restart is the Worker process ending and starting again on the same disk.
func (w *compWorkerState) restart() { w.close(); w.open() }

func fastBackoff() *transport.Backoff {
	return &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond}
}

// connect runs the production connector on the state with the given protocol
// range (zero: the build's) and returns it with its stop function.
func (w *compWorkerState) connect(h *connHub, protocols proto.Range) (*wagent.Connector, *syncLog, func()) {
	w.t.Helper()
	logs := &syncLog{}
	conn, err := wagent.NewConnector(wagent.ConnectorConfig{
		HubURL: h.url(), Name: "pi", Ops: func() []string { return []string{"sessions_list"} },
		Key: w.enr.LoadKey, Enrollment: w.enr, Policy: wagent.NewPolicy(w.st), Dialer: wagent.NewHubDialer(),
		Backoff: fastBackoff(), IdlePoll: 20 * time.Millisecond, Protocols: protocols,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	var once atomic.Bool
	stop := func() {
		if once.Swap(true) {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			w.t.Error("the connector did not stop")
		}
	}
	w.t.Cleanup(stop)
	return conn, logs, stop
}

// pairHub is the real Hub with pairing enabled and the user alice.
func newPairingConnHub(t *testing.T) (*connHub, func() *workers.Pairings) {
	t.Helper()
	h := newConnHubWith(t, proto.Version{}, func(svc *workers.Service) {
		if _, err := svc.EnablePairing(workers.PairingOptions{Limiter: openLimiter{}}); err != nil {
			t.Error(err)
		}
	})
	if _, err := h.st.DB().Exec(
		`INSERT INTO users (id, username, active, created_at, updated_at) VALUES ('alice', 'alice', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	return h, func() *workers.Pairings { return h.service().Pairings() }
}

func newCompPairer(t *testing.T, h *connHub, w *compWorkerState, out io.Writer, codes func() identity.PairingCode) *wagent.Pairer {
	t.Helper()
	p, err := wagent.NewPairer(wagent.PairerConfig{
		HubURL: h.url(), Name: "pi", Ops: func() []string { return []string{"sessions_list"} },
		Enrollment: w.enr, Dialer: wagent.NewPairDialer(), Backoff: fastBackoff(), NewCode: codes,
		EpochPoll: 20 * time.Millisecond, Out: out,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type pairRun struct {
	enr  wagent.Enrollment
	err  error
	done chan struct{}
}

func runPairer(p *wagent.Pairer) (*pairRun, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	r := &pairRun{done: make(chan struct{})}
	go func() { defer close(r.done); r.enr, r.err = p.Run(ctx) }()
	return r, cancel
}

func (r *pairRun) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pairer did not finish")
	}
}

func claimIP() netip.Addr { return netip.MustParseAddr("192.0.2.10") }

func TestRealPairerAndHubPairWithTheRightCodeAndAWorkerRestartDoesNotPairAgain(t *testing.T) {
	h, ps := newPairingConnHub(t)
	w := newCompWorkerState(t)
	code := identity.NewPairingCode()
	p := newCompPairer(t, h, w, nil, func() identity.PairingCode { return code })
	run, cancel := runPairer(p)
	defer cancel()

	eventually(t, "the Hub to hold the Worker as pending", func() bool { return ps().IsPending(code.PairingID()) })
	res, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: code.String(), IP: claimIP()})
	if err != nil {
		t.Fatal(err)
	}
	run.wait(t)
	if run.err != nil {
		t.Fatalf("pairer: %v", run.err)
	}
	if run.enr.WorkerID != res.WorkerID || run.enr.Owner != "alice" || run.enr.Kind != "private" {
		t.Fatalf("enrollment = %+v, claim = %+v", run.enr, res)
	}
	ek, err := w.enr.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if row, err := h.st.WorkerByPub(context.Background(), ek.Public()); err != nil || row.ID != res.WorkerID {
		t.Fatalf("the Hub's row = %+v, %v", row, err)
	}

	// The paired Worker connects with the pin it stored.
	_, _, stop := w.connect(h, proto.Range{})
	eventually(t, "the paired Worker to be online", func() bool { return h.service().Registry().IsOnline(res.WorkerID) })
	stop()

	// A Worker restart: the state is reopened from disk. It is still paired,
	// the pairer refuses a new code, and the connector comes back with no code
	// and no pairing handshake at all.
	w.restart()
	again := newCompPairer(t, h, w, nil, identity.NewPairingCode)
	_, err = again.Run(context.Background())
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeAlreadyPaired {
		t.Fatalf("a paired Worker's pairer returned %v, want already_paired", err)
	}
	conn, logs, _ := w.connect(h, proto.Range{})
	eventually(t, "the restarted Worker to be online again", func() bool {
		return h.service().Registry().IsOnline(res.WorkerID) && conn.Status().State == wagent.ConnConnected
	})
	if ps().Pending() != 0 {
		t.Fatal("the restarted Worker opened a pairing")
	}
	if strings.Contains(strings.ToLower(logs.String()), "pairing code") {
		t.Fatalf("the restarted Worker talked about a pairing code: %s", logs.String())
	}
	rows, err := h.st.ListWorkers(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("workers at the Hub = %d, %v; the restart must not create a second one", len(rows), err)
	}
}

func TestARealPairerRetiresACodeAfterAWrongClaimAndTheNextCodePairs(t *testing.T) {
	h, ps := newPairingConnHub(t)
	w := newCompWorkerState(t)
	var out syncLog
	first, second := identity.NewPairingCode(), identity.NewPairingCode()
	var n atomic.Int32
	p := newCompPairer(t, h, w, &out, func() identity.PairingCode {
		if n.Add(1) == 1 {
			return first
		}
		return second
	})
	run, cancel := runPairer(p)
	defer cancel()

	eventually(t, "the first code to be pending", func() bool { return ps().IsPending(first.PairingID()) })
	_, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: wrongTail(first), IP: claimIP()})
	if proto.CodeOf(err) != proto.CodePairingCodeInvalid {
		t.Fatalf("a wrong code: %v", err)
	}
	// The Worker answered the check with its confirmation, so it does not
	// offer that code again: it shows a new one (a guess must not be free).
	eventually(t, "the Worker to wait with a new code", func() bool { return ps().IsPending(second.PairingID()) })
	if _, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: first.String(), IP: claimIP()}); err == nil {
		t.Fatal("the retired first code paired the Worker")
	}
	if _, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: second.String(), IP: claimIP()}); err != nil {
		t.Fatalf("the new code: %v", err)
	}
	run.wait(t)
	if run.err != nil || run.enr.Owner != "alice" {
		t.Fatalf("pairer = %+v, %v", run.enr, run.err)
	}
}

// The production pair dialer, used five times with one code, as a Worker does
// that offers a code again: the Hub counts the wrong claims, drops the code at
// the fifth, and tells the Worker that connects afterwards. (The real Pairer
// itself never offers an exposed code twice, see the test above; this is the
// Hub's own limit seen through the production dialer.)
func TestFiveWrongCodesAtTheRealHubExceedTheAttemptsAndTheWorkerIsToldSo(t *testing.T) {
	h, ps := newPairingConnHub(t)
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	code := identity.NewPairingCode()
	dial := func() <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := wagent.NewPairDialer().Pair(context.Background(), wagent.PairParams{
				HubURL: h.url(), Key: key, Name: "pi", Ops: []string{"sessions_list"}, Code: code, Timeout: 20 * time.Second,
			})
			ch <- err
		}()
		return ch
	}
	for attempt := 1; attempt <= workers.PairingMaxAttempts; attempt++ {
		ch := dial()
		eventually(t, "the Worker to be pending", func() bool { return ps().IsPending(code.PairingID()) })
		_, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: wrongTail(code), IP: claimIP()})
		want := proto.CodePairingCodeInvalid
		if attempt == workers.PairingMaxAttempts {
			want = proto.CodePairingAttemptsExceeded
		}
		if proto.CodeOf(err) != want {
			t.Fatalf("attempt %d: claim error %v, want %s", attempt, err, want)
		}
		select {
		case werr := <-ch:
			if proto.CodeOf(werr) != proto.CodePairingCodeInvalid {
				t.Fatalf("attempt %d: the Worker saw %v", attempt, werr)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the Worker's pairing did not end")
		}
	}
	select {
	case werr := <-dial():
		if proto.CodeOf(werr) != proto.CodePairingAttemptsExceeded {
			t.Fatalf("after the limit the Worker saw %v, want pairing_attempts_exceeded", werr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no answer")
	}
	if _, err := ps().Claim(context.Background(), workers.ClaimRequest{UserID: "alice", Code: code.String(), IP: claimIP()}); err == nil {
		t.Fatal("the dropped code was accepted")
	}
	if rows, _ := h.st.ListWorkers(context.Background()); len(rows) != 0 {
		t.Fatalf("a Worker was created by a dropped pairing: %d", len(rows))
	}
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
