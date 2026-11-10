package workers

// Admission of the inbound streams of the Workers' sessions (AD-3).
//
// A multiplexed session lets its Worker open as many streams as it likes; the
// transport bounds only the streams whose kind is still being read. Everything
// after that is bounded here, in the one place that sees every admitted
// stream, the accept loop of the session's [hostedSession]:
//
//   - every stream of a handled kind takes a slot of its lane (calls, or the
//     ev streams: the ones that still wait for their first frame, the event
//     stream, the turn channel and the replaced ones that are closing), first
//     of the authenticated Worker and then of the Hub as a whole. The lanes are
//     independent, so a Worker that parks idle calls cannot take the capacity
//     of its own event and turn channels, and a crowd of Workers that park
//     calls cannot take the ev capacity of the others;
//   - a slot is held until the stream is really released: the handler is done
//     with it and yamux has dropped it (see [transport.Stream.Release]), not
//     when the handler function returns. A Worker that never completes the
//     close of a stream keeps its slot, not the Hub's capacity of everyone else;
//   - a stream that does not get a slot is refused. The refusal is itself
//     bounded: a few writers per Worker and in total answer a refused call, any
//     other refusal just closes the stream, and every refused stream counts as
//     held until yamux has surely dropped it (it cannot be reset at once:
//     yamux has no public RST). A connection that keeps opening streams while
//     it holds that many already is abusive and is closed.
//
// Admission starts before the kind of a stream is known. The transport asks
// the Worker's admission ([workerAdmission] is a [transport.StreamBudget])
// for a unit of the pending lane for every stream yamux accepts, before it reads
// a byte: a stream that never sends its two-byte prefix, a prefix that is not
// one, and a stream that is rejected and still draining all hold capacity of the
// Worker and of the Hub from the start, and a stream that does not fit is
// rejected through the bounded refusal accounting below (a session that exceeds
// it is closed). The unit is held while the stream is classified and while it
// waits for the accept loop, and is given back only after the stream has taken
// its slot of the call or event lane (or its refusal slot): the lane is taken
// first, so there is no moment at which a live stream holds nothing.
//
// The limits are keyed by the Worker id of the handshake, not by connection, so
// a Worker that reconnects does not start with a fresh budget while the streams
// of its old connection are still closing.

import (
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Default stream limits (see [StreamLimits]).
const (
	// DefaultWorkerCalls is the call streams one Worker may have open.
	DefaultWorkerCalls = maxConcurrentCalls
	// DefaultWorkerEvents is the ev streams one Worker may have in any stage:
	// waiting for the first frame, the event stream, the turn channel and the
	// replaced streams that are still closing. A Worker needs two (events and
	// turns); the rest is room for a stream that is replaced by its successor.
	DefaultWorkerEvents = 8
	// DefaultWorkerRefusals is the refused streams of one Worker that may be
	// held at once (closing or being answered) before the connection is ended.
	DefaultWorkerRefusals = 64
	// DefaultWorkerRefusalWriters is how many refusals of one Worker are
	// answered (written and drained) at the same time.
	DefaultWorkerRefusalWriters = 4

	// DefaultWorkerPending is the streams of one Worker that may be accepted by
	// the session and not yet handed to a lane: waiting for their kind prefix,
	// or for the accept loop. A healthy Worker has a handful at any moment.
	DefaultWorkerPending = 64

	// DefaultGlobalCalls, DefaultGlobalEvents, DefaultGlobalRefusals,
	// DefaultGlobalRefusalWriters and DefaultGlobalPending bound the same things
	// for all Workers.
	DefaultGlobalCalls          = 1024
	DefaultGlobalEvents         = 2048
	DefaultGlobalRefusals       = 1024
	DefaultGlobalRefusalWriters = 64
	DefaultGlobalPending        = 2048
)

// StreamLimits bounds the inbound streams. A zero member selects its default.
type StreamLimits struct {
	// Per authenticated Worker.
	WorkerCalls          int
	WorkerEvents         int
	WorkerRefusals       int
	WorkerRefusalWriters int
	WorkerPending        int
	// For the whole Hub.
	GlobalCalls          int
	GlobalEvents         int
	GlobalRefusals       int
	GlobalRefusalWriters int
	GlobalPending        int
}

func (l StreamLimits) withDefaults() StreamLimits {
	set := func(p *int, d int) {
		if *p <= 0 {
			*p = d
		}
	}
	set(&l.WorkerCalls, DefaultWorkerCalls)
	set(&l.WorkerEvents, DefaultWorkerEvents)
	set(&l.WorkerRefusals, DefaultWorkerRefusals)
	set(&l.WorkerRefusalWriters, DefaultWorkerRefusalWriters)
	set(&l.GlobalCalls, DefaultGlobalCalls)
	set(&l.GlobalEvents, DefaultGlobalEvents)
	set(&l.GlobalRefusals, DefaultGlobalRefusals)
	set(&l.GlobalRefusalWriters, DefaultGlobalRefusalWriters)
	set(&l.WorkerPending, DefaultWorkerPending)
	set(&l.GlobalPending, DefaultGlobalPending)
	return l
}

type lane int

const (
	laneCall lane = iota
	laneEvent
	numLanes
)

// streamAdmission holds the counters of all Workers. It is safe for
// concurrent use.
type streamAdmission struct {
	lim StreamLimits

	mu      sync.Mutex
	used    [numLanes]int
	pending int
	refused int
	writers int
	workers map[string]*workerCounters
	// heldFor is how long a refusal that could not be waited for counts as
	// held; it is transport.ReleaseBound, a variable for tests.
	heldFor time.Duration
}

func newStreamAdmission(l StreamLimits, heldFor time.Duration) *streamAdmission {
	return &streamAdmission{lim: l.withDefaults(), workers: map[string]*workerCounters{}, heldFor: heldFor}
}

// workerCounters is the streams one Worker holds, over all its connections.
type workerCounters struct {
	id      string
	conns   int
	used    [numLanes]int
	pending int
	refused int
	writers int
}

// workerAdmission is one connection of a Worker: the counters of the Worker
// and the refusals that only this connection holds.
type workerAdmission struct {
	*workerCounters
	a *streamAdmission

	qmu   sync.Mutex
	quiet map[*time.Timer]func()
	ended bool
}

// attach registers a connection of the Worker; detach is its counterpart. The
// counters of a Worker live while it has a connection or a slot.
func (a *streamAdmission) attach(workerID string) *workerAdmission {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.workers[workerID]
	if c == nil {
		c = &workerCounters{id: workerID}
		a.workers[workerID] = c
	}
	c.conns++
	return &workerAdmission{workerCounters: c, a: a, quiet: map[*time.Timer]func(){}}
}

// detach ends the connection: the streams it refused without waiting for them
// are released with the session, which yamux has just dropped.
func (w *workerAdmission) detach() {
	w.qmu.Lock()
	w.ended = true
	held := w.quiet
	w.quiet = nil
	w.qmu.Unlock()
	for t, release := range held {
		t.Stop()
		release()
	}
	w.a.mu.Lock()
	defer w.a.mu.Unlock()
	w.conns--
	w.a.forgetLocked(w.workerCounters)
}

func (a *streamAdmission) forgetLocked(w *workerCounters) {
	if w.conns > 0 || w.refused > 0 || w.writers > 0 || w.pending > 0 {
		return
	}
	for _, n := range w.used {
		if n > 0 {
			return
		}
	}
	delete(a.workers, w.id)
}

func (w *workerAdmission) laneLimits(l lane) (perWorker, global int) {
	if l == laneCall {
		return w.a.lim.WorkerCalls, w.a.lim.GlobalCalls
	}
	return w.a.lim.WorkerEvents, w.a.lim.GlobalEvents
}

// acquire takes a slot of lane l; the returned function gives it back and must
// be called once, when the stream is really released.
func (w *workerAdmission) acquire(l lane) (release func(), ok bool) {
	a := w.a
	perWorker, global := w.laneLimits(l)
	a.mu.Lock()
	defer a.mu.Unlock()
	if w.used[l] >= perWorker || a.used[l] >= global {
		return nil, false
	}
	w.used[l]++
	a.used[l]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			w.used[l]--
			a.used[l]--
			a.forgetLocked(w.workerCounters)
		})
	}, true
}

// Reserve is the pending lane: a unit for a stream that yamux accepted and whose
// kind is not yet known or not yet handed over (transport.StreamBudget).
func (w *workerAdmission) Reserve() (release func(), ok bool) {
	a := w.a
	a.mu.Lock()
	defer a.mu.Unlock()
	if w.pending >= a.lim.WorkerPending || a.pending >= a.lim.GlobalPending {
		return nil, false
	}
	w.pending++
	a.pending++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			w.pending--
			a.pending--
			a.forgetLocked(w.workerCounters)
		})
	}, true
}

// Refuse accounts for a rejected stream that is closed and drained by the
// transport: the unit of the refusal lane is held until the release the
// transport calls once yamux has released the stream. It is not ok when the
// Worker or the Hub holds too many refused streams: the session is abusive.
func (w *workerAdmission) Refuse() (release func(), ok bool) {
	a := w.a
	a.mu.Lock()
	defer a.mu.Unlock()
	if w.refused >= a.lim.WorkerRefusals || a.refused >= a.lim.GlobalRefusals {
		return nil, false
	}
	w.refused++
	a.refused++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			w.refused--
			a.refused--
			a.forgetLocked(w.workerCounters)
		})
	}, true
}

var _ transport.StreamBudget = (*workerAdmission)(nil)

// refusal is what the host does with a stream that got no slot.
type refusal int

const (
	// refuseAnswer: answer the stream on a goroutine of its own (a writer
	// slot is held until the stream is released) and release it.
	refuseAnswer refusal = iota
	// refuseClose: close the stream and nothing more; it is counted as held
	// until yamux has surely dropped it.
	refuseClose
	// refuseAbort: the connection holds too many refused streams: end it.
	refuseAbort
)

// refuse accounts for one refused stream and says how to refuse it. For
// refuseAnswer the caller answers the stream and calls release once it is
// released; for refuseClose the release is scheduled here, by a timer, and the
// returned function does nothing.
func (w *workerAdmission) refuse(canAnswer bool) (kind refusal, release func()) {
	a := w.a
	a.mu.Lock()
	defer a.mu.Unlock()
	if w.refused >= a.lim.WorkerRefusals || a.refused >= a.lim.GlobalRefusals {
		return refuseAbort, func() {}
	}
	w.refused++
	a.refused++
	answer := canAnswer && w.writers < a.lim.WorkerRefusalWriters && a.writers < a.lim.GlobalRefusalWriters
	if answer {
		w.writers++
		a.writers++
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			w.refused--
			a.refused--
			if answer {
				w.writers--
				a.writers--
			}
			a.forgetLocked(w.workerCounters)
		})
	}
	if answer {
		return refuseAnswer, release
	}
	w.holdQuietly(release)
	return refuseClose, func() {}
}

// holdQuietly counts a closed refused stream as held until yamux has surely
// dropped it, or until the connection ends, whichever is first. It is called
// with the admission lock held.
func (w *workerAdmission) holdQuietly(release func()) {
	w.qmu.Lock()
	defer w.qmu.Unlock()
	if w.ended {
		go release() // the lock order forbids calling it here
		return
	}
	var t *time.Timer
	t = time.AfterFunc(w.a.heldFor, func() {
		w.qmu.Lock()
		delete(w.quiet, t)
		w.qmu.Unlock()
		release()
	})
	w.quiet[t] = release
}

// admissionUsage is the streams held now (tests, diagnostics).
type admissionUsage struct {
	Calls, Events, Pending, Refused, Writers, Workers int
}

func (a *streamAdmission) usage() admissionUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return admissionUsage{Calls: a.used[laneCall], Events: a.used[laneEvent], Pending: a.pending, Refused: a.refused, Writers: a.writers, Workers: len(a.workers)}
}
