package api

// Story 5.3: GET /api/v1/events, the live feed of session changes as
// Server-Sent Events (AD-18), re-authorised every 30 s (AD-11).
//
// The stream starts with a "snapshot" event that holds the sessions the caller
// may read (the same entries as GET /api/v1/sessions), taken after the
// subscription is made so that no change falls between the two. After that,
// every change published by the Hub's event bus is sent as an SSE event named
// by its type (session_added, session_removed, state_changed, screen_changed,
// worker_online, worker_offline, worker_resynced) with the JSON of the event as
// data. A subscriber receives an event only when its principal may read the
// resource the event is about; a Worker-level event is sent when the principal
// may read the Worker or any of its known sessions, and a worker_resynced is
// also sent once for a Worker whose sessions the stream delivered before, so
// that the client drops the ones the replacement state no longer shows.
//
// Every ReauthInterval the credentials of the request are authenticated again
// (a revoked key, a logged-out or expired session and a changed credential all
// end the stream) and the permission cache is dropped, so that a lost or
// changed permission takes effect within one interval. The re-check runs on its
// own goroutine, so a write blocked on a client that does not read neither
// delays it nor survives a revocation. A stream narrowed with ?worker= also
// ends when the principal can no longer read that Worker (for example it was
// reassigned). When access is lost the stream sends an "access_lost" event with
// the API error and ends. A consumer that is too slow, and a Hub that stops,
// end the stream with a "reset" event;
// the browser then reconnects and gets a fresh snapshot.
//
// ?all=true lifts the default "mine" filter (an admin sees every Worker);
// ?worker=<name or id> narrows the stream to one Worker.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// EventsPath is the route of the SSE feed.
const EventsPath = "/api/v1/events"

// Defaults of the event stream.
const (
	// DefaultHeartbeat is the interval of the keep-alive comment that stops
	// proxies from closing an idle stream.
	DefaultHeartbeat = 15 * time.Second
	// DefaultMaxEventStreams caps the open streams of the Hub.
	DefaultMaxEventStreams = 1024
	// DefaultMaxEventStreamsPerUser caps the open streams of one user.
	DefaultMaxEventStreamsPerUser = 16
	// DefaultEventBuffer is the queue of one stream.
	DefaultEventBuffer = workers.DefaultSubscriberBuffer
	// retryHint is what the browser waits before it reconnects.
	retryHint = 3 * time.Second
	// maxDecisionCache bounds the cache of read decisions of one stream.
	maxDecisionCache = 4096
)

// EventBus is the Hub's event bus; *workers.EventHub implements it.
type EventBus interface {
	LiveState
	Subscribe(buffer int) *workers.Subscription
}

var _ EventBus = (*workers.EventHub)(nil)

// Clock is the time source of the stream's timers; tests inject a fake.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// EventsConfig configures [NewEventsHandler].
type EventsConfig struct {
	// Auth authenticates the request, at the start and at every re-check.
	// Required.
	Auth Authn
	// Authz decides what a principal may read. Required.
	Authz *authz.Authorizer
	// Bus delivers the events. Required.
	Bus EventBus
	// Lister builds the snapshot. Required.
	Lister *SessionLister
	// Owners answers who owns a Worker, for the "mine" filter. Required.
	Owners authz.WorkerLookup
	// Clock drives the re-authorisation and the heartbeat; nil is the
	// system clock.
	Clock Clock
	// ReauthInterval is the re-check period; zero means
	// [DefaultReauthInterval], and it never exceeds it.
	ReauthInterval time.Duration
	// Heartbeat is the keep-alive period; zero means [DefaultHeartbeat].
	Heartbeat time.Duration
	// WriteTimeout bounds one write; zero means [DefaultStreamWriteTimeout].
	WriteTimeout time.Duration
	// MaxStreams and MaxStreamsPerUser cap the open streams; zero means the
	// defaults. A request over a cap is rate_limited.
	MaxStreams        int
	MaxStreamsPerUser int
	// Buffer is the queue of one stream; zero means [DefaultEventBuffer].
	Buffer int
	// BodyTimeout bounds the time a client may take to send a request body
	// that it announced; zero means [DefaultBodyTimeout].
	BodyTimeout time.Duration
	// Log receives internal failures; nil means slog.Default.
	Log *slog.Logger
}

// EventsHandler serves GET /api/v1/events.
type EventsHandler struct {
	cfg  EventsConfig
	clk  Clock
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	total   int
	perUser map[string]int
}

// NewEventsHandler validates cfg and returns the handler.
func NewEventsHandler(cfg EventsConfig) (*EventsHandler, error) {
	if cfg.Auth == nil || cfg.Authz == nil || cfg.Bus == nil || cfg.Lister == nil || cfg.Owners == nil {
		return nil, errors.New("api: the events handler needs Auth, Authz, Bus, Lister and Owners")
	}
	if cfg.ReauthInterval <= 0 || cfg.ReauthInterval > DefaultReauthInterval {
		cfg.ReauthInterval = DefaultReauthInterval
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultStreamWriteTimeout
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultMaxEventStreams
	}
	if cfg.MaxStreamsPerUser <= 0 {
		cfg.MaxStreamsPerUser = DefaultMaxEventStreamsPerUser
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultEventBuffer
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	h := &EventsHandler{cfg: cfg, clk: cfg.Clock, done: make(chan struct{}), perUser: map[string]int{}}
	if h.clk == nil {
		h.clk = systemClock{}
	}
	return h, nil
}

// Close ends every open stream (the Hub stops).
func (h *EventsHandler) Close() { h.once.Do(func() { close(h.done) }) }

// Streams returns the number of open streams.
func (h *EventsHandler) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func (h *EventsHandler) acquire(user string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= h.cfg.MaxStreams || h.perUser[user] >= h.cfg.MaxStreamsPerUser {
		return false
	}
	h.total++
	h.perUser[user]++
	return true
}

func (h *EventsHandler) release(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total--
	if h.perUser[user]--; h.perUser[user] <= 0 {
		delete(h.perUser, user)
	}
}

var readOnly = ops.MustActionSet(ops.ActionRead)

// ServeHTTP implements http.Handler.
func (h *EventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The route is mounted on the mux, outside the dispatcher: an announced
	// request body is bounded here, before any answer, as the dispatcher does.
	if err := absorbBody(w, r, h.cfg.BodyTimeout); err != nil {
		WriteError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteError(w, NewError(proto.CodeMethodNotAllowed, "use GET"))
		return
	}
	id, err := h.cfg.Auth.Authenticate(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	q, err := ParseSessionQuery(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	q.Kind, q.State, q.Online = "", "", nil // the stream filters by Worker only
	s := &eventStream{h: h, r: r, id: id, q: q, cache: map[string]bool{}, seen: map[string]struct{}{}}
	if err := s.checkPrincipal(r.Context(), id.Principal); err != nil {
		WriteError(w, err)
		return
	}
	user := id.Principal.UserID
	if user == "" {
		user = "worker:" + id.Principal.WorkerID
	}
	if !h.acquire(user) {
		e := NewError(proto.CodeRateLimited, "too many open event streams")
		e.RetryAfter = retryHint
		WriteError(w, e)
		return
	}
	defer h.release(user)

	// Subscribe before the snapshot is read: a change between the two is then
	// delivered twice at worst, never lost.
	sub := h.cfg.Bus.Subscribe(h.cfg.Buffer)
	defer sub.Close()
	snapshot, err := h.cfg.Lister.List(r.Context(), id.Principal, q)
	if err != nil {
		h.cfg.Log.Error("event stream: cannot build the snapshot", "error", err.Error())
		WriteError(w, internalError())
		return
	}
	if snapshot == nil {
		snapshot = []SessionInfo{}
	}
	for _, e := range snapshot {
		s.seen[e.WorkerID] = struct{}{}
	}
	s.selectWorker(r.Context())
	out, err := newSSE(w, h.cfg.WriteTimeout)
	if err != nil {
		WriteError(w, internalError())
		return
	}
	defer out.finish()
	s.run(r.Context(), sub, out, snapshot)
}

// eventStream is the state of one open stream.
type eventStream struct {
	h *EventsHandler
	r *http.Request
	q SessionQuery

	// mu guards id and cache, which the re-check (its own goroutine) replaces
	// while the event loop reads them.
	mu sync.Mutex
	id Identity
	// cache holds the read decisions of session events since the last
	// re-check. It is dropped at every re-check, so a decision is at most one
	// interval old. gen counts the re-checks: a decision made with an older
	// principal is not stored.
	cache map[string]bool
	gen   uint64

	// The two fields below belong to the event loop alone.
	//
	// seen holds the Workers for which the stream delivered sessions. A
	// worker_resynced for such a Worker is delivered once even when the
	// replacement state holds nothing the principal may read now: that is how
	// the client learns that the sessions it holds are gone.
	seen map[string]struct{}
	// worker is the id of the Worker selected with ?worker= while the
	// principal could read it; the re-check ends the stream when it cannot.
	worker string
}

// selectWorker resolves ?worker= to a Worker id and remembers it when the
// principal may read that Worker (or one of its sessions) now.
func (s *eventStream) selectWorker(ctx context.Context) {
	if s.q.Worker == "" {
		return
	}
	recs, err := s.h.cfg.Lister.Directory.Workers(ctx)
	if err != nil {
		return
	}
	for _, w := range recs {
		if w.ID == s.q.Worker || w.Name == s.q.Worker {
			if s.mayReadWorker(ctx, s.principal(), w.ID) {
				s.worker = w.ID
			}
			return
		}
	}
}

func (s *eventStream) principal() authz.Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id.Principal
}

// run is the event loop. The re-check runs on a goroutine of its own, so a
// write that is blocked on a client that does not read cannot delay it, and a
// revocation interrupts that write (the same design as Dispatcher.serveStream).
func (s *eventStream) run(ctx context.Context, sub *workers.Subscription, out *sseWriter, snapshot []SessionInfo) {
	h := s.h
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go s.reauthLoop(rctx, cancel, out)

	if out.retry(retryHint) != nil || out.send("snapshot", SessionsResult{Sessions: snapshot}) != nil {
		s.endRevoked(ctx, rctx, out)
		return
	}
	beat := h.clk.After(h.cfg.Heartbeat)
	for {
		select {
		case <-rctx.Done():
			// The client is gone, or the re-check revoked the stream.
			s.endRevoked(ctx, rctx, out)
			return
		case <-h.done:
			_ = out.sendFinal("reset", resetData("shutdown"))
			return
		case ev, ok := <-sub.C():
			if !ok {
				reason := "closed"
				if errors.Is(sub.Err(), workers.ErrSlowSubscriber) {
					reason = "slow_consumer"
				}
				_ = out.sendFinal("reset", resetData(reason))
				return
			}
			if !s.visible(rctx, ev) {
				continue
			}
			if out.send(ev.Type, ev) != nil {
				s.endRevoked(ctx, rctx, out)
				return
			}
		case <-beat:
			beat = h.clk.After(h.cfg.Heartbeat)
			if out.comment("keep-alive") != nil {
				s.endRevoked(ctx, rctx, out)
				return
			}
		}
	}
}

// endRevoked sends access_lost when the re-check revoked the stream. It is
// called whenever the loop ends: a write that failed because the revocation
// interrupted it is the ordinary way to get here.
func (s *eventStream) endRevoked(ctx, rctx context.Context, out *sseWriter) {
	if cause := context.Cause(rctx); ctx.Err() == nil && cause != nil && !errors.Is(cause, context.Canceled) {
		_ = out.sendFinal("access_lost", Envelope(ErrorOf(cause)))
	}
}

// reauthLoop re-checks the stream every ReauthInterval. When the check fails
// it interrupts a blocked write and ends the loop with the error as cause.
func (s *eventStream) reauthLoop(ctx context.Context, cancel context.CancelCauseFunc, out *sseWriter) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.h.clk.After(s.h.cfg.ReauthInterval):
		}
		if err := s.recheck(ctx); err != nil {
			if ctx.Err() == nil {
				// The cause comes first, so that a loop whose write fails
				// from the interrupt finds it.
				cancel(err)
				out.interrupt()
			}
			return
		}
	}
}

// recheck authenticates the request again, refreshes the principal and drops
// the cached decisions. A changed credential, a lost subject, a failed check
// or a selected Worker that the principal can no longer read is an error.
func (s *eventStream) recheck(ctx context.Context) error {
	id, err := s.h.cfg.Auth.Authenticate(s.r.WithContext(ctx))
	if err != nil {
		return err
	}
	s.mu.Lock()
	same := id.Actor == s.id.Actor && id.ActingAs == s.id.ActingAs && id.Principal.UserID == s.id.Principal.UserID &&
		id.Principal.Kind == s.id.Principal.Kind
	if same {
		s.id = id
		s.cache = map[string]bool{}
		s.gen++
	}
	s.mu.Unlock()
	if !same {
		return NewError(proto.CodeUnauthenticated, "the credentials changed")
	}
	if err := s.checkPrincipal(ctx, id.Principal); err != nil {
		return err
	}
	if s.worker != "" && !s.mayReadWorker(ctx, id.Principal, s.worker) {
		return NewError(proto.CodeForbidden, "access to the selected Worker was lost")
	}
	return nil
}

// checkPrincipal tells whether the principal still exists at all: an active
// user, an unexpired act-as session, a present Worker owner. It asks about the
// principal's own user resource: a principal that holds nothing there (a key
// limited to some Workers) gets not_found, which is fine, while a principal
// without any permission gets forbidden.
func (s *eventStream) checkPrincipal(ctx context.Context, p authz.Principal) error {
	if p.Kind == authz.KindWorker || p.UserID == "" {
		return NewError(proto.CodeForbidden, "this credential cannot read the event feed")
	}
	err := s.h.cfg.Authz.Decide(ctx, p, readOnly, authz.UserResource{ID: p.UserID})
	if err == nil {
		return nil
	}
	switch proto.CodeOf(err) {
	case proto.CodeNotFound:
		return nil
	case proto.CodeForbidden:
		return NewError(proto.CodeForbidden, "access to the event feed was lost")
	}
	s.h.cfg.Log.Error("event stream: permission check failed", "error", err.Error())
	return internalError()
}

// visible tells whether the principal may receive ev.
func (s *eventStream) visible(ctx context.Context, ev workers.Event) bool {
	if w := s.q.Worker; w != "" && w != ev.WorkerID && w != ev.Worker {
		return false
	}
	if ev.Type == workers.EventPipeChanged {
		return s.visiblePipeChanged(ctx, ev)
	}
	if ev.Session != "" && ev.Type != workers.EventSessionRemoved && s.fenced(ev.WorkerID, ev.Session) {
		// share_set took the session away from the share after this event was
		// queued for the stream: nothing of it is delivered any more. This is
		// asked at the delivery, not cached, so it holds for events that were
		// queued before share_set returned.
		return false
	}
	if ev.Session == "" {
		// Worker-level events are not cached: what a principal may read of a
		// Worker changes when a snapshot replaces its sessions, and a stale
		// denial would hide the resync that announces them.
		return s.visibleWorkerEvent(ctx, ev)
	}
	key := ev.WorkerID + "\x00" + ev.Session
	s.mu.Lock()
	v, ok := s.cache[key]
	p, gen := s.id.Principal, s.gen
	s.mu.Unlock()
	if ok {
		if v {
			s.seen[ev.WorkerID] = struct{}{}
		}
		return v
	}
	v = s.decide(ctx, p, ev)
	s.mu.Lock()
	if s.gen == gen {
		if len(s.cache) >= maxDecisionCache {
			s.cache = map[string]bool{}
		}
		s.cache[key] = v
	}
	s.mu.Unlock()
	if v {
		s.seen[ev.WorkerID] = struct{}{}
	}
	return v
}

// visiblePipeChanged decides pipe_changed at the delivery, from current state
// only: the stored history fence (not the live-only one: the event is about the
// stored conversation), then the "mine" filter and a fresh read decision, then
// the fresh current history policy of the Worker. Neither
// the fence nor the decision is cached, so an unshare, a lost permission or a
// reassignment that happens while the event is queued drops it. An unknown
// policy hides (the fence is strict) and a failed check hides.
func (s *eventStream) visiblePipeChanged(ctx context.Context, ev workers.Event) bool {
	if ev.Session == "" {
		return false
	}
	f, ok := s.h.cfg.Bus.(interface {
		Fenced(workerID, session string) bool
	})
	if !ok || f.Fenced(ev.WorkerID, ev.Session) {
		return false // no fence to ask: fail closed
	}
	// Ordinary authorisation first: a principal that may not read the target
	// never causes a probe of the Worker.
	if !s.decide(ctx, s.principal(), ev) {
		return false
	}
	// Then the current history policy, asked fresh at the delivery: a local
	// unshare on a connected Worker whose event has not arrived yet drops the
	// event queued before it. No port, an unavailable probe or a replaced
	// connection hides (fail closed). The probe is bounded; the event carries
	// no content either way.
	h, ok := s.h.cfg.Bus.(interface {
		HistoryVisible(ctx context.Context, workerID, session string) bool
	})
	if !ok {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, pipeChangedProbeTimeout)
	defer cancel()
	return h.HistoryVisible(pctx, ev.WorkerID, ev.Session)
}

// pipeChangedProbeTimeout bounds the fresh history-policy check of one
// pipe_changed delivery.
const pipeChangedProbeTimeout = 5 * time.Second

// fenced tells whether the Hub stopped showing the session (see
// [workers.EventHub.ApplyShare]); a bus without a fence shows everything it
// was given.
func (s *eventStream) fenced(workerID, session string) bool {
	if f, ok := s.h.cfg.Bus.(interface {
		LiveFenced(workerID, session string) bool
	}); ok {
		return f.LiveFenced(workerID, session)
	}
	f, ok := s.h.cfg.Bus.(interface {
		Fenced(workerID, session string) bool
	})
	return ok && f.Fenced(workerID, session)
}

func (s *eventStream) visibleWorkerEvent(ctx context.Context, ev workers.Event) bool {
	if s.decide(ctx, s.principal(), ev) {
		return true
	}
	if _, ok := s.seen[ev.WorkerID]; ok && ev.Type == workers.EventWorkerResynced {
		// The replacement state holds nothing the principal may read, but it
		// did read sessions of this Worker before: tell it once, so that it
		// drops them.
		delete(s.seen, ev.WorkerID)
		return true
	}
	return false
}

func (s *eventStream) decide(ctx context.Context, p authz.Principal, ev workers.Event) bool {
	if !s.passesMine(ctx, p, ev.WorkerID) {
		return false
	}
	if ev.Session != "" {
		return s.canRead(ctx, p, authz.SessionResource{WorkerID: ev.WorkerID, Name: ev.Session})
	}
	return s.canReadWorker(ctx, p, ev.WorkerID)
}

// passesMine applies the "mine" filter: unless ?all=true, only the Workers
// the principal's user owns.
func (s *eventStream) passesMine(ctx context.Context, p authz.Principal, workerID string) bool {
	if s.q.All {
		return true
	}
	owner, err := s.h.cfg.Owners.WorkerOwner(ctx, workerID)
	return err == nil && p.UserID != "" && owner == p.UserID
}

// canReadWorker tells whether p may read the Worker or, when it cannot (a key
// limited to some sessions), any of the sessions the hub knows of it.
func (s *eventStream) canReadWorker(ctx context.Context, p authz.Principal, workerID string) bool {
	if s.canRead(ctx, p, authz.WorkerResource{ID: workerID}) {
		return true
	}
	if ws, ok := s.h.cfg.Bus.Sessions(workerID); ok {
		for _, sess := range ws.Sessions {
			if s.canRead(ctx, p, authz.SessionResource{WorkerID: workerID, Name: sess.Name}) {
				return true
			}
		}
	}
	return false
}

// mayReadWorker is the check of a selected Worker: the "mine" filter and
// then [eventStream.canReadWorker].
func (s *eventStream) mayReadWorker(ctx context.Context, p authz.Principal, workerID string) bool {
	return s.passesMine(ctx, p, workerID) && s.canReadWorker(ctx, p, workerID)
}

func (s *eventStream) canRead(ctx context.Context, p authz.Principal, res authz.Resource) bool {
	err := s.h.cfg.Authz.Decide(ctx, p, readOnly, res)
	if err == nil {
		return true
	}
	if c := proto.CodeOf(err); c != proto.CodeForbidden && c != proto.CodeNotFound {
		s.h.cfg.Log.Error("event stream: permission check failed", "error", err.Error())
	}
	return false // fail closed
}

// ---------------------------------------------------------------------------
// SSE framing. internal/hub/web has the same helper for pages, but it imports
// this package, so the few lines exist here too.

type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration

	// mu orders the write deadlines. write arms one before each write; the
	// re-check, when it revokes the stream, calls interrupt to shorten it
	// and so to end a write that is blocked on a client that does not read.
	// After that, write refuses until sendFinal arms a fresh deadline for the
	// last event; after finish it refuses for good.
	mu          sync.Mutex
	interrupted bool
	final       bool // the last event was started: interrupt changes nothing
	done        bool
}

var (
	errSSEUnsupported = errors.New("api: the response writer cannot stream")
	errSSEInterrupted = errors.New("api: the event stream was interrupted")
)

func newSSE(w http.ResponseWriter, timeout time.Duration) (*sseWriter, error) {
	s := &sseWriter{w: w, rc: http.NewResponseController(w), timeout: timeout}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // proxies must not buffer the stream
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := s.rc.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			return nil, errSSEUnsupported
		}
		return nil, err
	}
	return s, nil
}

// finish clears the write deadline, which would fail the next request that
// uses the connection, and keeps a late interrupt from setting one again.
func (s *sseWriter) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	_ = s.rc.SetWriteDeadline(time.Time{})
}

// interruptGrace is how long a write that is in flight when the stream is
// revoked may still take. A write that is merely in progress completes within
// it, and the last event can then follow; a write that is blocked on a client
// that does not read is cut. (A write that fails at the connection leaves
// net/http's buffered writer broken for good, so cutting at once would also
// lose the last event of a healthy connection.)
const interruptGrace = time.Second

// interrupt ends a blocked write within [interruptGrace] and makes the
// following writes fail.
func (s *sseWriter) interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.final {
		return
	}
	s.interrupted = true
	_ = s.rc.SetWriteDeadline(time.Now().Add(interruptGrace))
}

func (s *sseWriter) write(text string) error {
	s.mu.Lock()
	if s.done || s.interrupted {
		s.mu.Unlock()
		return errSSEInterrupted
	}
	// A writer without deadlines (a recorder) cannot block.
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	s.mu.Unlock()
	return s.put(text)
}

func (s *sseWriter) put(text string) error {
	if _, err := io.WriteString(s.w, text); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) retry(d time.Duration) error {
	return s.write("retry: " + strconv.FormatInt(d.Milliseconds(), 10) + "\n\n")
}

func (s *sseWriter) comment(text string) error { return s.write(": " + text + "\n\n") }

// sendFinal writes the last event of a stream, also after an interrupt, under
// a fresh write deadline.
func (s *sseWriter) sendFinal(event string, data []byte) error {
	if strings.ContainsAny(event, "\r\n") {
		return errors.New("api: an SSE event name must not contain a line break")
	}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errSSEInterrupted
	}
	s.interrupted, s.final = false, true
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	s.mu.Unlock()
	return s.put(frame(event, data))
}

// send writes one event with v as its JSON data.
func (s *sseWriter) send(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.sendRaw(event, b)
}

// sendRaw writes one event whose data is a single line of JSON. An event name
// with a line break is refused: it would let a value forge further fields.
func (s *sseWriter) sendRaw(event string, data []byte) error {
	if strings.ContainsAny(event, "\r\n") {
		return errors.New("api: an SSE event name must not contain a line break")
	}
	return s.write(frame(event, data))
}

func resetData(reason string) []byte {
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return b
}

func frame(event string, data []byte) string {
	return "event: " + event + "\ndata: " + strings.TrimRight(string(data), "\n") + "\n\n"
}
