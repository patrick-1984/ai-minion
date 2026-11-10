package workers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Inbound streams of a Worker's session (AD-3).
//
// A multiplexed session has exactly one reader of its inbound streams: the
// accept loop of its [streamHost], started by the connect handler. It looks at
// the kind of each stream and gives it to the handler that owns that kind:
//
//   - KindCall: the call handler of the connect handler ([Handler]); it is
//     registered before the Worker is added to the registry, so no module can
//     take the kind away;
//   - KindEvents: the event ingest of Story 5.3 ([Ingest.Attach]);
//   - any other kind a module serves (Story 7.4 and later): it joins the host
//     from its [Registry.OnAdd] hook with [StreamHost.Handle], exactly like the
//     ingest. It must not call Accept on the session itself: two readers of one
//     session each steal streams from the other and reset the ones they do not
//     know.
//
// A stream of a kind without a handler, and a stream that finds no free slot
// of its lane, is refused (admission.go): the streams of every Worker, and of
// all Workers together, are bounded, and a connection that keeps opening
// streams beyond that is closed.

// StreamHandler serves the inbound streams of one kind of one session.
type StreamHandler interface {
	// Serve takes over st. It runs on the accept loop and must not block: it
	// starts its own goroutine for any longer work. ctx ends when the session
	// is over or the Hub shuts down; a stream that waits (an idle read) must be
	// interrupted by it, with defer interruptOnDone(ctx, st)() (see [interruptRead]
	// and [interruptOnDone]). Native Reset also ends active reads, but the
	// context helper ties the interruption to the handler's lifetime.
	//
	// done must be called exactly once, when the handler has finished with st
	// and will not touch it again, whatever ended the stream (it is a nil
	// function for a session that has no admission and may be called all the
	// same: see [streamDone]). It closes the stream, waits until yamux has
	// released it and only then frees the slot of the stream: the handler calls
	// it on its own goroutine, last.
	Serve(ctx context.Context, st *transport.Stream, done func())
	// Close is called once, after the accept loop ended and ctx is done. It
	// returns after every goroutine of the handler has returned.
	Close()
}

// readInterrupter is the part of a stream that [interruptRead] needs.
type readInterrupter interface {
	SetReadDeadline(t time.Time) error
}

// interruptRead wakes a read that waits on st: it fails at once with a timeout.
// It is how a handler's idle read ends with the session or the Hub through
// the narrower readInterrupter interface. Native transport.Stream.Reset also
// interrupts active reads and refuses new ones; the context helper does not
// require that method. The handler resets the stream when its read fails.
func interruptRead(st readInterrupter) { _ = st.SetReadDeadline(time.Now()) }

// interruptEvery is how often [interruptOnDone] repeats the interrupt.
const interruptEvery = 5 * time.Millisecond

// interruptOnDone interrupts the read that waits on st (see [interruptRead])
// once ctx is done, and keeps interrupting it every [interruptEvery] until the
// returned stop function is called, which the handler defers so that it runs
// when the handler returns. The interrupt is repeated because a read can start
// to wait just after one, and a stream that sees a read of a reader that is not
// its handler's own (a test double, or a read of a stream whose owner is
// not a [transport.Stream]) may lose a wake-up. A [transport.Stream] itself
// never has the handler and its drain read at the same time: Reset wakes the
// waiting read and the drain starts only once the read has left, so the drain
// can neither take the wake-up nor move the deadline. stop waits for the
// goroutine, so nothing outlives the handler.
func interruptOnDone(ctx context.Context, st readInterrupter) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	cancel := context.AfterFunc(ctx, func() {
		defer close(exited)
		tick := time.NewTicker(interruptEvery)
		defer tick.Stop()
		for {
			interruptRead(st)
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	})
	return func() {
		close(done)
		if !cancel() { // the interrupt goroutine was started: wait for it
			<-exited
		}
	}
}

// StreamHost is the session of a connected Worker as a hook of
// [Registry.OnAdd] sees it, when the Hub's connect handler serves it. A hook
// tests for it with a type assertion (see [HostOf]).
type StreamHost interface {
	// Handle makes h the handler of the inbound streams of kind. It reports
	// false, and keeps the existing handler, when the kind already has one.
	// It is meant for the OnAdd hook; a handler added later only serves the
	// streams that arrive after it.
	Handle(kind transport.Kind, h StreamHandler) bool
}

// HostOf returns the stream host of sess, and false for a session that nobody
// serves (a test double, or a session whose streams the caller reads itself).
func HostOf(sess OpSession) (StreamHost, bool) {
	h, ok := sess.(StreamHost)
	return h, ok
}

// hostedSession is the session the connect handler puts in the registry: the
// transport session plus the dispatcher of its inbound streams.
type hostedSession struct {
	*transport.Session
	adm      *workerAdmission
	log      *slog.Logger
	mu       sync.Mutex
	handlers map[transport.Kind]StreamHandler
	closed   bool
	order    []StreamHandler // in the order of Handle, closed in reverse
}

// newHostedSession returns the host of s. adm holds the stream counters of the
// Worker the session belongs to; nil gives the session counters of its own with
// the default limits (a test double).
func newHostedSession(s *transport.Session, adm *workerAdmission, log *slog.Logger) *hostedSession {
	if adm == nil {
		adm = newStreamAdmission(StreamLimits{}, transport.ReleaseBound).attach("")
	}
	if log == nil {
		log = slog.Default()
	}
	return &hostedSession{Session: s, adm: adm, log: log, handlers: map[transport.Kind]StreamHandler{}}
}

// streamDone is the done function of a stream that is not admitted by a
// session host (the ingest of a session it reads itself): it just releases.
func streamDone(st *transport.Stream) func() { return st.Release }

// Handle implements [StreamHost].
func (s *hostedSession) Handle(kind transport.Kind, h StreamHandler) bool {
	if h == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, taken := s.handlers[kind]; taken {
		return false
	}
	s.handlers[kind] = h
	s.order = append(s.order, h)
	return true
}

func (s *hostedSession) handler(kind transport.Kind) StreamHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[kind]
}

// serve is the one accept loop of the session. It returns when the session
// ends or ctx is done, after the handlers have returned.
func (s *hostedSession) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		// The session is over, or the Hub is stopping: close the session, so
		// that every stream is released at once and the handlers, which wait
		// for the release of their stream, return (Close below waits for them).
		_ = s.Close()
		s.mu.Lock()
		s.closed = true
		hs := s.order
		s.mu.Unlock()
		for i := len(hs) - 1; i >= 0; i-- {
			hs[i].Close()
		}
	}()
	go func() {
		select {
		case <-s.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	defer s.adm.detach()
	for {
		st, err := s.Accept(ctx)
		if err != nil {
			return
		}
		s.admit(ctx, st)
	}
}

func laneOf(k transport.Kind) (lane, bool) {
	switch k {
	case transport.KindCall:
		return laneCall, true
	case transport.KindEvents:
		return laneEvent, true
	}
	return 0, false
}

// admit gives st to the handler of its kind when its lane has a free slot, and
// refuses it otherwise.
func (s *hostedSession) admit(ctx context.Context, st *transport.Stream) {
	h := s.handler(st.Kind())
	l, ok := laneOf(st.Kind())
	if h == nil || !ok {
		s.refuse(st)
		return
	}
	free, ok := s.adm.acquire(l)
	if !ok {
		s.refuse(st)
		return
	}
	// The stream holds its lane now: the unit it held while it was classified
	// can go back, with no moment at which it held nothing.
	st.DropReservation()
	var once sync.Once
	h.Serve(ctx, st, func() {
		once.Do(func() {
			st.Release()
			free()
		})
	})
}

// refuse refuses st within the bounds of the admission: a call is answered
// with rate_limited by at most a few writers, any other stream is closed, and a
// connection that holds too many refused streams is ended.
func (s *hostedSession) refuse(st *transport.Stream) {
	call := st.Kind() == transport.KindCall
	how, release := s.adm.refuse(call)
	// The refusal accounting holds the stream now; give back the unit it held
	// while it was classified. (An abort closes the whole session.)
	defer st.DropReservation()
	switch how {
	case refuseAnswer:
		go func() {
			defer release()
			_ = st.SetWriteDeadline(time.Now().Add(controlTimeout))
			_ = st.WriteFrame(proto.NewError(proto.CodeRateLimited, "too many calls in flight on this connection").Frame())
			st.Release()
		}()
	case refuseClose:
		_ = st.Close()
	default:
		s.log.Warn("worker connection closed: it keeps opening streams beyond its limits", "worker_id", s.adm.id)
		_ = st.Close()
		_ = s.Close()
	}
}
