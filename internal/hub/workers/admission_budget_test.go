package workers

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// budgetSession is a real Hub session (a real yamux server over a pipe) whose
// budget is the admission of worker id, served by the one accept loop of its
// host, and the transport session of its Worker, which opens raw streams
// (Session.OpenRaw) because a Worker that withholds or garbles the kind prefix
// is exactly what a real Worker library would not do.
type budgetSession struct {
	hub  *transport.Session
	peer *transport.Session
	host *hostedSession
}

func newBudgetSession(t *testing.T, adm *streamAdmission, id string, kindTimeout time.Duration, handlers func(*hostedSession)) *budgetSession {
	t.Helper()
	a, b := net.Pipe()
	w := adm.attach(id)
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{Budget: w, KindTimeout: kindTimeout})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := newHostedSession(hub, w, nil)
	if handlers != nil {
		handlers(host)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); host.serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = hub.Close(); _ = peer.Close(); <-done })
	return &budgetSession{hub: hub, peer: peer, host: host}
}

func eventuallyUsage(t *testing.T, adm *streamAdmission, what string, cond func(admissionUsage) bool) admissionUsage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u := adm.usage()
		if cond(u) {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: usage %+v", what, u)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStreamsWithoutAKindAreAdmittedBeforeTheyAreClassified is the audit's
// reproduction with the fix: three real yamux sessions of three Workers, each
// opening 16 streams that never send their two-byte prefix, against a global
// pending budget of 4 and a refusal budget of 3. Before the fix all 48 streams
// were allocated while every admission counter stayed at zero. Now they are
// counted from the moment yamux accepts them, the budget is never exceeded, the
// sessions that keep opening streams beyond it are closed, and every unit is
// given back when they are.
func TestStreamsWithoutAKindAreAdmittedBeforeTheyAreClassified(t *testing.T) {
	adm := newStreamAdmission(StreamLimits{GlobalPending: 4, WorkerPending: 3, GlobalRefusals: 3, WorkerRefusals: 2, GlobalCalls: 1, GlobalEvents: 1}, transport.ReleaseBound)
	var sessions []*budgetSession
	for i := 0; i < 3; i++ {
		sessions = append(sessions, newBudgetSession(t, adm, "worker-"+string(rune('a'+i)), time.Minute, nil))
	}
	var open sync.WaitGroup
	for _, s := range sessions {
		open.Add(1)
		go func() {
			defer open.Done()
			for j := 0; j < 16; j++ {
				if _, err := s.peer.OpenRaw(context.Background()); err != nil {
					return // the Hub closed the session: it is abusive
				}
			}
		}()
	}
	// The counters are not zero while raw streams are live, and the budget is a
	// bound: at no moment do more units sit in the pending and refusal lanes than
	// the limits allow.
	stop := make(chan struct{})
	var peakPending, peakRefused atomic.Int32
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			u := adm.usage()
			for {
				p := peakPending.Load()
				if int32(u.Pending) <= p || peakPending.CompareAndSwap(p, int32(u.Pending)) {
					break
				}
			}
			for {
				p := peakRefused.Load()
				if int32(u.Refused) <= p || peakRefused.CompareAndSwap(p, int32(u.Refused)) {
					break
				}
			}
		}
	}()
	open.Wait()
	for _, s := range sessions {
		select {
		case <-s.hub.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("a session that keeps opening streams beyond its limits stayed open (streams %d)", s.hub.NumStreams())
		}
	}
	close(stop)
	if p := peakPending.Load(); p < 1 || p > 4 {
		t.Fatalf("peak pending = %d, want between 1 and the global limit 4: the streams were not counted from the start", p)
	}
	if r := peakRefused.Load(); r < 1 || r > 3 {
		t.Fatalf("peak refused = %d, want between 1 and the global limit 3", r)
	}
	// Cancellation releases everything: the sessions are closed, nothing is held.
	u := eventuallyUsage(t, adm, "every unit to be given back", func(u admissionUsage) bool {
		return u.Pending == 0 && u.Refused == 0 && u.Calls == 0 && u.Events == 0 && u.Workers == 0
	})
	t.Logf("peak pending %d, peak refused %d, final usage %+v", peakPending.Load(), peakRefused.Load(), u)
}

// A prefix that is not one drains under the refusal accounting and releases
// when yamux has released the stream; a stream that never sends one gives up at
// the kind timeout and releases too.
func TestAnInvalidOrMissingPrefixHoldsCapacityUntilTheStreamIsGone(t *testing.T) {
	adm := newStreamAdmission(StreamLimits{GlobalPending: 8, WorkerPending: 8}, transport.ReleaseBound)
	s := newBudgetSession(t, adm, "worker-x", 100*time.Millisecond, nil)
	bad, err := s.peer.OpenRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Write([]byte("zz")); err != nil { // not a kind prefix
		t.Fatal(err)
	}
	silent, err := s.peer.OpenRaw(context.Background()) // never writes
	if err != nil {
		t.Fatal(err)
	}
	eventuallyUsage(t, adm, "the two streams to be counted", func(u admissionUsage) bool { return u.Pending >= 2 })
	// The kind timeout (100 ms) ends the wait of the silent stream and the prefix
	// is refused, but both stay counted while they close: the units are held
	// until the peer completes the close and yamux has released the streams.
	time.Sleep(300 * time.Millisecond)
	if u := adm.usage(); u.Pending < 2 {
		t.Fatalf("usage %+v: closing streams gave their capacity back before yamux released them", u)
	}
	_ = bad.Close()
	_ = silent.Close()
	eventuallyUsage(t, adm, "the units to come back", func(u admissionUsage) bool { return u.Pending == 0 && u.Refused == 0 })
}

// Healthy streams still work with the budget in place: a call and an event
// stream are admitted, take their lane before the pending unit goes back (the
// stream never holds nothing), and free every unit when they end. Each stream is
// opened alone, so that the units counted are the units of that one stream; for a
// moment it holds two (the lane and the pending unit), never none.
func TestHealthyLanesWorkWithTheBudgetAndTheHandoffHasNoGap(t *testing.T) {
	adm := newStreamAdmission(StreamLimits{GlobalPending: 2, WorkerPending: 2}, transport.ReleaseBound)
	served := make(chan struct{}, 4)
	release := make(chan struct{})
	h := handlerFunc(func(_ context.Context, st *transport.Stream, done func()) {
		served <- struct{}{}
		go func() { <-release; done() }()
	})
	s := newBudgetSession(t, adm, "worker-h", time.Minute, func(hs *hostedSession) {
		hs.Handle(transport.KindCall, h)
		hs.Handle(transport.KindEvents, h)
	})
	for _, tc := range []struct {
		prefix string
		kind   transport.Kind
		held   func(admissionUsage) bool
	}{
		{"ca", transport.KindCall, func(u admissionUsage) bool { return u.Calls == 1 && u.Pending == 0 }},
		{"ev", transport.KindEvents, func(u admissionUsage) bool { return u.Events == 1 && u.Pending == 0 }},
	} {
		stop := make(chan struct{})
		sampled := make(chan struct{})
		var gap, reached atomic.Bool
		go func() {
			defer close(sampled)
			for {
				select {
				case <-stop:
					return
				default:
				}
				u := adm.usage()
				switch total := u.Pending + u.Calls + u.Events; {
				case total >= 1:
					reached.Store(true)
				case reached.Load():
					gap.Store(true)
				}
			}
		}()
		st, err := s.peer.OpenStream(context.Background(), tc.kind)
		if err != nil {
			t.Fatal(err)
		}
		<-served
		u := eventuallyUsage(t, adm, tc.prefix+" stream to hold its lane", tc.held)
		t.Logf("usage while the %s stream is served: %+v", tc.prefix, u)
		time.Sleep(20 * time.Millisecond) // the sampler sees the settled state too
		close(stop)
		<-sampled
		if !reached.Load() {
			t.Fatalf("%s: the sampler never saw the stream counted", tc.prefix)
		}
		if gap.Load() {
			t.Fatalf("%s: the live stream held no capacity at some moment of the handoff", tc.prefix)
		}
		// End it: the handler's done releases the stream and frees the lane.
		release <- struct{}{}
		_ = st.Close()
		eventuallyUsage(t, adm, tc.prefix+" lane to be freed", func(u admissionUsage) bool { return u.Calls == 0 && u.Events == 0 && u.Pending == 0 })
	}
}

type handlerFunc func(ctx context.Context, st *transport.Stream, done func())

func (f handlerFunc) Serve(ctx context.Context, st *transport.Stream, done func()) { f(ctx, st, done) }
func (f handlerFunc) Close()                                                       {}
