package workers

// Story 5.3: the Hub side of the Worker event stream (AD-3, AD-18).
//
// A Worker opens one long-lived "ev" stream per connection. It starts with an
// events.snapshot frame (seq 0), the shared sessions with kind, state, task and
// a hash of the screen, and goes on with "event" frames whose seq rises by one
// per frame. This file holds three small parts:
//
//   - [EventHub]: the in-memory latest state of every Worker (sessions, online
//     flag) and the fan-out bus. It knows nothing about the network and holds
//     no terminal content: only what the Worker reports, which is the shared
//     sessions (A-9: an unshared session is on no frame);
//   - [Link] and [Feed]: the handles a connection and one of its event streams
//     use to feed the hub. A new stream supersedes the previous one of the
//     same connection, a new connection supersedes the previous one of the
//     same Worker, and a superseded handle changes nothing. A snapshot
//     replaces the Worker's state atomically (one lock) and publishes
//     worker_resynced;
//   - [Ingest]: takes the inbound streams of a transport session, routes the
//     "ev" streams to a Feed (a gap in seq, a missing snapshot or a malformed
//     frame resets the stream so that the Worker opens a new one with a fresh
//     snapshot after two seconds) and hands every other stream kind to an
//     optional handler (the call streams of Epic 7) or resets it.
//
// The hub holds no owner: who may read an event is decided by the authorizer
// of the consumer on the live owner (AD-11), never by data kept here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Names of the events the hub publishes. The first four are the events a
// Worker sends; the others are made by the hub.
const (
	EventSessionAdded   = "session_added"
	EventSessionRemoved = "session_removed"
	EventStateChanged   = "state_changed"
	EventScreenChanged  = "screen_changed"
	// EventWorkerResynced is published after a Worker's snapshot replaced its
	// state: subscribers re-read the Worker's sessions.
	EventWorkerResynced = "worker_resynced"
	// EventWorkerOnline and EventWorkerOffline follow the connection.
	EventWorkerOnline  = "worker_online"
	EventWorkerOffline = "worker_offline"
	// EventPipeChanged is published after a message or a reply of a session's
	// pipe conversation was durably committed. It is a wake-up only: it names
	// the Worker and the session and nothing of the conversation. The reader
	// reads the conversation again through its own authorised read.
	EventPipeChanged = "pipe_changed"
	// EventShareChanged is the event a Worker sends when its share rule changed.
	// It is consumed by the hub and never published: it holds the rule, which
	// only the hub's own checks use.
	EventShareChanged = "share_changed"
)

// Limits on what a Worker reports. They keep the hub's memory bounded however
// the Worker behaves; a value over a limit is cut or dropped, never an error
// that would make the Worker reopen the stream for ever.
const (
	maxSessionsPerWorker = 4096
	maxSessionNameLen    = 256
	maxClassLen          = 64
	maxScreenHashLen     = 128
	// DefaultSubscriberBuffer is the queue of one subscription.
	DefaultSubscriberBuffer = 256
)

// SessionState is the latest known state of one session. It never holds
// screen content.
type SessionState struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Task       string `json:"task"`
	ScreenHash string `json:"screen_hash,omitempty"`
}

// WorkerSessions is the latest known state of one Worker.
type WorkerSessions struct {
	WorkerID string
	// Name is the Worker's name at its last connect.
	Name string
	// Online is true while the Worker is connected.
	Online bool
	// Sessions are ordered by name.
	Sessions []SessionState
	// UpdatedAt is the time of the last snapshot or event.
	UpdatedAt time.Time
}

// Event is one change published to the subscribers. Session is empty for the
// events about a Worker as a whole (worker_resynced, worker_online,
// worker_offline).
type Event struct {
	Type     string `json:"type"`
	WorkerID string `json:"worker_id"`
	Worker   string `json:"worker"`
	Session  string `json:"session,omitempty"`
	Kind     string `json:"kind,omitempty"`
	State    string `json:"state,omitempty"`
	// Task is present for session_added and state_changed (empty when the
	// session has no task).
	Task       *string       `json:"task,omitempty"`
	ScreenHash string        `json:"screen_hash,omitempty"`
	Time       ops.Timestamp `json:"time"`
}

// ErrSlowSubscriber is the reason of a subscription that was closed because
// its queue was full.
var ErrSlowSubscriber = errors.New("workers: subscriber too slow")

// ErrSuperseded is returned by a [Feed] or [Link] that a newer one replaced.
var ErrSuperseded = errors.New("workers: superseded by a newer connection or stream")

// EventHubOptions configures [NewEventHub].
type EventHubOptions struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Share keeps the share rule of every Worker durably. With it the hub is
	// strict: a session's stored history is hidden whenever the Worker's rule is
	// not known (see events_share.go). Nil keeps the rules in memory only and
	// treats a Worker that declared none as sharing what it lists, which is for
	// tests and for hubs that do not store history.
	Share ShareStore
	// Logger receives operational messages; nil discards them.
	Logger *slog.Logger
}

// EventHub keeps the latest state of every Worker and fans events out to
// subscribers. It is safe for concurrent use.
type EventHub struct {
	mu      sync.Mutex
	now     func() time.Time
	workers map[string]*workerState
	subs    map[*Subscription]struct{}
	nextGen uint64
	// fenced holds, per Worker, the sessions that the Worker reported as not
	// shared in an answer of share_get or share_set (see [EventHub.ApplyShare]).
	// The value is true while the Worker has not yet confirmed the removal with
	// its own session_removed event (events about the session may still be in
	// flight and are dropped) and false after that (the session stays hidden
	// until the Worker shares it again).
	fenced map[string]map[string]bool

	// The authoritative share rule of every Worker, durable when shareStore is
	// set (events_share.go).
	shareStore    ShareStore
	shares        map[string]*shareState
	sharesLoaded  bool
	sharesLoadErr error
	lastSeq       uint64
	log           *slog.Logger
}

type workerState struct {
	id       string
	name     string
	online   bool
	gen      uint64 // the connection that owns the state
	stream   uint64 // the event stream that owns it
	sessions map[string]SessionState
	updated  time.Time
	// probe asks this connection for the share rule it has now (see
	// [Link.SetShareProbe]); nil when the connection has no such port.
	probe shareProber
}

// NewEventHub returns an empty hub.
func NewEventHub(o EventHubOptions) *EventHub {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EventHub{now: now, workers: map[string]*workerState{}, subs: map[*Subscription]struct{}{}, fenced: map[string]map[string]bool{},
		shareStore: o.Share, shares: map[string]*shareState{}, log: log}
}

// Subscription is a stream of events for one consumer.
type Subscription struct {
	h  *EventHub
	ch chan Event

	// err is set, under the hub lock, before ch is closed.
	err error
}

// C delivers the events in order. It is closed when the subscription ends:
// after [Subscription.Close] or when the consumer is too slow; see
// [Subscription.Err].
func (s *Subscription) C() <-chan Event { return s.ch }

// Err tells why C was closed: nil after Close, [ErrSlowSubscriber] when the
// queue overflowed. It is meaningful once C is closed.
func (s *Subscription) Err() error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.err
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.h.dropLocked(s, nil)
}

// Subscribe returns a subscription with a queue of buffer events (a
// non-positive buffer means [DefaultSubscriberBuffer]). A consumer that does
// not keep up is closed with [ErrSlowSubscriber]: the hub never blocks on a
// consumer and never drops an event silently.
func (h *EventHub) Subscribe(buffer int) *Subscription {
	if buffer <= 0 {
		buffer = DefaultSubscriberBuffer
	}
	s := &Subscription{h: h, ch: make(chan Event, buffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *EventHub) dropLocked(s *Subscription, err error) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	s.err = err
	close(s.ch)
}

// publishLocked sends ev to every subscription without blocking.
func (h *EventHub) publishLocked(ev Event) {
	for s := range h.subs {
		select {
		case s.ch <- ev:
		default:
			h.dropLocked(s, ErrSlowSubscriber)
		}
	}
}

func (h *EventHub) eventLocked(w *workerState, typ, session string) Event {
	return Event{Type: typ, WorkerID: w.id, Worker: w.name, Session: session, Time: ops.Timestamp(h.now())}
}

// Sessions returns the latest state of one Worker.
func (h *EventHub) Sessions(workerID string) (WorkerSessions, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.workers[workerID]
	if w == nil {
		return WorkerSessions{}, false
	}
	return w.copyLocked(), true
}

// All returns the latest state of every Worker the hub knows, ordered by
// Worker id.
func (h *EventHub) All() []WorkerSessions {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]WorkerSessions, 0, len(h.workers))
	for _, w := range h.workers {
		out = append(out, w.copyLocked())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkerID < out[j].WorkerID })
	return out
}

func (w *workerState) copyLocked() WorkerSessions {
	ws := WorkerSessions{WorkerID: w.id, Name: w.name, Online: w.online, UpdatedAt: w.updated,
		Sessions: make([]SessionState, 0, len(w.sessions))}
	for _, s := range w.sessions {
		ws.Sessions = append(ws.Sessions, s)
	}
	sort.Slice(ws.Sessions, func(i, j int) bool { return ws.Sessions[i].Name < ws.Sessions[j].Name })
	return ws
}

// Forget drops everything the hub knows about a Worker (it was removed).
func (h *EventHub) Forget(workerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.workers, workerID)
	delete(h.fenced, workerID)
	delete(h.shares, workerID)
	if h.shareStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shareSaveTimeout)
		defer cancel()
		if err := h.shareStore.DeleteWorkerShare(ctx, workerID); err != nil {
			h.log.Warn("workers: the share rule of a forgotten Worker could not be deleted", "worker_id", workerID, "error", err.Error())
		}
		if err := h.shareStore.DeletePendingShares(ctx, workerID, math.MaxInt64); err != nil {
			h.log.Warn("workers: the pending share restrictions of a forgotten Worker could not be deleted", "worker_id", workerID, "error", err.Error())
		}
	}
}

// ApplyShare makes the Hub's view follow an answer of the Worker's share_get or
// share_set: unshared are the names of the sessions the Worker does not share.
// It runs before the answer is returned to its caller, so that nothing the Hub
// kept about those sessions is readable once share_set has returned:
//
//   - the sessions are dropped from the Worker's state at once, with a
//     session_removed event, without waiting for the Worker's own event;
//   - they are fenced: a session_added, state_changed or screen_changed that
//     the Worker had already sent (it is in flight, or queued in a
//     subscription) does not bring them back, and [EventHub.Fenced] tells the
//     readers of anything else the Hub stored about them (the pipe history,
//     the queued events of an open stream) to hide it.
//
// The fence is the set of the last answer: a session that a later answer does
// not list (it is shared again) leaves it, and so does a session that the
// Worker reports as shared after it confirmed the removal (a session_added
// that follows its own session_removed on the ordered stream), a new snapshot
// (a new event stream starts from what the Worker shares now) and Forget. The
// fence is in memory: a restarted Hub learns what is shared from the Worker's
// snapshot only. The stored history is not erased; it is hidden.
func (h *EventHub) ApplyShare(workerID string, unshared []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.applyShareNamesLocked(workerID, unshared)
}

// applyShareNamesLocked is [EventHub.ApplyShare] for a caller that holds the lock.
func (h *EventHub) applyShareNamesLocked(workerID string, unshared []string) {
	fence := make(map[string]bool, len(unshared))
	for _, n := range unshared {
		if len(fence) >= maxSessionsPerWorker {
			break
		}
		// Keep the state of a session that was fenced already: its removal may
		// have been confirmed.
		pending, was := h.fenced[workerID][n]
		fence[n] = !was || pending
	}
	if len(fence) == 0 {
		delete(h.fenced, workerID)
	} else {
		h.fenced[workerID] = fence
	}
	w := h.workers[workerID]
	if w == nil {
		return
	}
	names := make([]string, 0, len(fence))
	for n := range fence {
		if _, ok := w.sessions[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		delete(w.sessions, n)
		h.publishLocked(h.eventLocked(w, EventSessionRemoved, n))
	}
	if len(names) > 0 {
		w.updated = h.now()
	}
}

// PublishPipeChanged publishes pipe_changed for a session whose conversation
// changed. It is gated on the current share authority before it is published
// (the stored history is what the event is about, so the strict history rule
// applies: an unshared session, a restriction that is not settled and a rule
// that is not known publish nothing) and never blocks: a subscriber whose queue
// is full is closed, as for every event. The event carries no Worker name and
// nothing of the conversation.
func (h *EventHub) PublishPipeChanged(workerID, session string) {
	if workerID == "" || session == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureSharesLocked()
	if h.hiddenLocked(workerID, session, true) {
		return
	}
	h.publishLocked(Event{Type: EventPipeChanged, WorkerID: workerID, Session: session, Time: ops.Timestamp(h.now())})
}

// Fenced reports whether what the Hub holds about a session (the pipe history,
// the events queued for an open stream) must not be shown: the Worker's current
// share rule does not share the session, a restriction that was asked for is not
// settled yet, or the rule is not known. It answers for any session name, also
// one the Worker does not list (closed, offline): the rule decides, not the
// list. Before the rule-aware answers (an answer that names only unshared
// sessions) it also covers a session that such an answer fenced.
func (h *EventHub) Fenced(workerID, session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureSharesLocked()
	return h.hiddenLocked(workerID, session, true)
}

// LiveFenced is [EventHub.Fenced] for what the Worker itself reports, the events
// queued for an open stream: it applies the Worker's rule, a restriction that
// was asked for and whose outcome is not known, and the sessions fenced by an
// answer that names unshared sessions. It does not hide a Worker whose rule the
// hub has not learned yet: such a Worker (one that declares no rule) only ever
// reports sessions it shares, and it is the stored history that must wait for
// the rule.
func (h *EventHub) LiveFenced(workerID, session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureSharesLocked()
	return h.hiddenLocked(workerID, session, false)
}

// Link is the hub's handle for one connection of a Worker.
type Link struct {
	h   *EventHub
	id  string
	gen uint64
}

// Connect records that a Worker is online. The state it had before stays until
// the first snapshot of this connection replaces it, so a short reconnect does
// not blank the session list. The new Link supersedes the previous one of the
// Worker. worker_online is published.
func (h *EventHub) Connect(workerID, name string) *Link {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextGen++
	w := h.workers[workerID]
	if w == nil {
		w = &workerState{id: workerID, sessions: map[string]SessionState{}}
		h.workers[workerID] = w
	}
	w.name, w.online, w.gen, w.stream = name, true, h.nextGen, 0
	w.probe = nil // the probe of the connection before belongs to a session that is gone
	w.updated = h.now()
	h.publishLocked(h.eventLocked(w, EventWorkerOnline, ""))
	return &Link{h: h, id: workerID, gen: h.nextGen}
}

// Disconnect records that the connection ended. The sessions stay, with the
// Worker marked offline. A superseded Link changes nothing.
func (l *Link) Disconnect() {
	h := l.h
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.workers[l.id]
	if w == nil || w.gen != l.gen || !w.online {
		return
	}
	w.online = false
	w.updated = h.now()
	h.publishLocked(h.eventLocked(w, EventWorkerOffline, ""))
}

// Feed is the handle for one event stream. It enforces the stream protocol:
// a snapshot with seq 0 first, then events with seq rising by one.
type Feed struct {
	h      *EventHub
	id     string
	gen    uint64
	stream uint64

	mu      sync.Mutex // guards the fields below; one reader normally
	started bool
	seq     uint64
}

// NewFeed opens the Feed of a new event stream of this connection. It
// supersedes the previous stream's Feed.
func (l *Link) NewFeed() (*Feed, error) {
	h := l.h
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.workers[l.id]
	if w == nil || w.gen != l.gen {
		return nil, ErrSuperseded
	}
	w.stream++
	return &Feed{h: h, id: l.id, gen: l.gen, stream: w.stream}, nil
}

// owned returns the state when this Feed still owns it.
func (f *Feed) ownedLocked() *workerState {
	w := f.h.workers[f.id]
	if w == nil || w.gen != f.gen || w.stream != f.stream {
		return nil
	}
	return w
}

// Protocol errors of an event stream. They all mean: reset the stream; the
// Worker opens a new one with a fresh snapshot.
var (
	ErrNoSnapshot = errors.New("workers: event before events.snapshot")
	ErrSeqGap     = errors.New("workers: gap in the event sequence")
	ErrTooMany    = errors.New("workers: too many sessions")
)

// Snapshot replaces the Worker's whole state with sessions in one step and
// publishes worker_resynced. seq must be 0.
func (f *Feed) Snapshot(seq uint64, sessions []SessionState) error {
	return f.SnapshotShare(seq, sessions, nil)
}

// SnapshotShare is [Feed.Snapshot] with the share rule the Worker declared with
// it (nil from a Worker that declares none). The rule is applied before the
// sessions, and the sessions it hides are not taken.
func (f *Feed) SnapshotShare(seq uint64, sessions []SessionState, share *ops.ShareDecl) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq != 0 {
		return fmt.Errorf("%w: events.snapshot has seq %d, want 0", ErrSeqGap, seq)
	}
	if len(sessions) > maxSessionsPerWorker {
		return ErrTooMany
	}
	next := make(map[string]SessionState, len(sessions))
	for _, s := range sessions {
		if s, ok := sanitize(s); ok {
			next[s.Name] = s
		}
	}
	h := f.h
	h.mu.Lock()
	defer h.mu.Unlock()
	w := f.ownedLocked()
	if w == nil {
		return ErrSuperseded
	}
	h.ensureSharesLocked()
	if share != nil {
		h.adoptLocked(f.id, w, f.gen, *share, false)
	}
	delete(h.fenced, f.id) // a new stream starts from what the Worker shares now
	for n := range next {
		if h.hiddenLocked(f.id, n, false) {
			delete(next, n)
		}
	}
	w.sessions = next // atomic: readers hold the same lock
	w.updated = h.now()
	f.started, f.seq = true, 0
	h.publishLocked(h.eventLocked(w, EventWorkerResynced, ""))
	return nil
}

// Change is one decoded "event" frame.
type Change struct {
	Seq     uint64
	Event   string
	Session SessionState
	// HasTask is true when the frame carried a task (even an empty one).
	HasTask bool
	// Share is the rule of a share_changed event.
	Share *ops.ShareDecl
}

// Apply applies one event. A gap or a repeat in seq, or an event before the
// snapshot, is an error. An unknown event name is ignored (AD-4: additive).
func (f *Feed) Apply(c Change) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return ErrNoSnapshot
	}
	if c.Seq != f.seq+1 {
		return fmt.Errorf("%w: got %d after %d", ErrSeqGap, c.Seq, f.seq)
	}
	h := f.h
	h.mu.Lock()
	defer h.mu.Unlock()
	w := f.ownedLocked()
	if w == nil {
		return ErrSuperseded
	}
	f.seq = c.Seq
	h.ensureSharesLocked()
	if c.Share != nil {
		// The Worker's rule changed. It comes ahead of the event that carries it
		// and of every event the new rule shows, on this ordered stream, so the
		// rule is applied first.
		h.adoptLocked(f.id, w, f.gen, *c.Share, false)
	}
	if c.Event == EventShareChanged {
		return nil // nothing else: a share_changed event names no session
	}
	s, ok := sanitize(c.Session)
	if !ok {
		return nil // an event about a session the hub cannot name: skip it
	}
	if h.hiddenLocked(f.id, s.Name, false) && h.shares[f.id] != nil && h.shares[f.id].known {
		// The Worker's rule does not share this session (an event that was sent
		// before the Worker applied a change, or that the Hub's own rule is
		// ahead of): nothing of it is kept or published.
		return nil
	}
	cur, known := w.sessions[s.Name]
	ev := h.eventLocked(w, c.Event, s.Name)
	if pending, fenced := h.fenced[f.id][s.Name]; fenced {
		switch {
		case c.Event == EventSessionRemoved:
			// The Worker confirms the session is gone from its share: the
			// stream is ordered, so nothing stale of it can follow. The session
			// stays hidden until the Worker reports it shared again.
			h.fenced[f.id][s.Name] = false
			return nil
		case pending:
			return nil // sent before the Worker applied the change
		case c.Event == EventSessionAdded || c.Event == EventStateChanged || c.Event == EventScreenChanged:
			// After the confirmation: the Worker shares the session again.
			delete(h.fenced[f.id], s.Name)
			if len(h.fenced[f.id]) == 0 {
				delete(h.fenced, f.id)
			}
			known = false
			cur = SessionState{}
		}
	}
	switch c.Event {
	case EventSessionAdded, EventStateChanged:
		if !known && len(w.sessions) >= maxSessionsPerWorker {
			return ErrTooMany
		}
		if !c.HasTask && known {
			s.Task = cur.Task
		}
		if c.Event == EventStateChanged && known && s.ScreenHash == "" {
			s.ScreenHash = cur.ScreenHash
		}
		if known && cur == s {
			return nil
		}
		w.sessions[s.Name] = s
		ev.Kind, ev.State, ev.ScreenHash = s.Kind, s.State, s.ScreenHash
		task := s.Task
		ev.Task = &task
	case EventSessionRemoved:
		if !known {
			return nil
		}
		delete(w.sessions, s.Name)
	case EventScreenChanged:
		if !known || cur.ScreenHash == s.ScreenHash {
			return nil
		}
		cur.ScreenHash = s.ScreenHash
		w.sessions[s.Name] = cur
		ev.Kind, ev.State, ev.ScreenHash = cur.Kind, cur.State, cur.ScreenHash
	default:
		return nil
	}
	w.updated = h.now()
	h.publishLocked(ev)
	return nil
}

// sanitize makes a reported session safe to keep: valid UTF-8, bounded
// lengths. A session without a usable name is dropped.
func sanitize(s SessionState) (SessionState, bool) {
	s.Name = strings.ToValidUTF8(s.Name, "")
	if s.Name == "" || len(s.Name) > maxSessionNameLen {
		return SessionState{}, false
	}
	s.Kind = cut(s.Kind, maxClassLen)
	s.State = cut(s.State, maxClassLen)
	s.Task = cut(s.Task, ops.MaxTaskLen)
	s.ScreenHash = cut(s.ScreenHash, maxScreenHashLen)
	return s, true
}

// cut returns v as valid UTF-8 of at most n bytes.
func cut(v string, n int) string {
	v = strings.ToValidUTF8(v, "")
	if len(v) <= n {
		return v
	}
	v = v[:n]
	for !utf8.ValidString(v) {
		v = v[:len(v)-1]
	}
	return v
}

// ---------------------------------------------------------------------------
// Wire decoding

type snapshotWire struct {
	Seq      uint64         `json:"seq"`
	Sessions []SessionState `json:"sessions"`
	Share    *ops.ShareDecl `json:"share"`
}

type eventWire struct {
	Seq        uint64  `json:"seq"`
	Event      string  `json:"event"`
	Session    string  `json:"session"`
	Kind       string  `json:"kind"`
	State      string  `json:"state"`
	Task       *string `json:"task"`
	ScreenHash string  `json:"screen_hash"`
	// Share is the rule of a share_changed event.
	Share *ops.ShareDecl `json:"share"`
}

// FrameReader is the read side of an event stream; *transport.Stream
// implements it and tests use a fake.
type FrameReader interface {
	ReadFrame() (json.RawMessage, error)
	// Reset abandons the stream.
	Reset()
}

// ServeFeed reads frames from st into feed until the stream ends, ctx ends or
// a protocol error resets the stream. It returns nil when the Worker closed
// the stream cleanly or ctx ended, and the cause otherwise. Frames of other
// types are ignored (AD-4).
func ServeFeed(ctx context.Context, st FrameReader, feed *Feed) error {
	return serveFeed(ctx, st, feed, nil, nil)
}

// serveFeed is [ServeFeed] with a sink for the frames that are neither
// events.snapshot nor event. Those frames take no part in the seq check.
func serveFeed(ctx context.Context, st FrameReader, feed *Feed, first json.RawMessage, other func(frameType string, body json.RawMessage)) error {
	stop := context.AfterFunc(ctx, st.Reset)
	defer stop()
	for {
		body := first
		first = nil
		if body == nil {
			var err error
			if body, err = st.ReadFrame(); err != nil {
				if ctx.Err() != nil || errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
		if err := feedFrame(feed, body, other); err != nil {
			st.Reset()
			if errors.Is(err, ErrSuperseded) {
				return nil
			}
			return err
		}
	}
}

func feedFrame(feed *Feed, body json.RawMessage, other func(string, json.RawMessage)) error {
	typ, err := proto.FrameType(body)
	if err != nil {
		return err
	}
	switch typ {
	case proto.TypeEventsSnap:
		var f snapshotWire
		if err := proto.DecodeFrame(body, &f); err != nil {
			return err
		}
		return feed.SnapshotShare(f.Seq, f.Sessions, f.Share)
	case proto.TypeEvent:
		var f eventWire
		if err := proto.DecodeFrame(body, &f); err != nil {
			return err
		}
		c := Change{Seq: f.Seq, Event: f.Event, HasTask: f.Task != nil, Share: f.Share,
			Session: SessionState{Name: f.Session, Kind: f.Kind, State: f.State, ScreenHash: f.ScreenHash}}
		if f.Task != nil {
			c.Session.Task = *f.Task
		}
		return feed.Apply(c)
	}
	if other != nil {
		other(typ, body)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ingest

// Acceptor is the inbound side of a transport session; *transport.Session
// implements it.
type Acceptor interface {
	Accept(ctx context.Context) (*transport.Stream, error)
}

// IngestOptions configures [NewIngest].
type IngestOptions struct {
	Hub *EventHub
	// Other receives every inbound stream that is not an ev stream (the call
	// streams, Epic 7), with the Worker it came from. It owns the stream. Nil
	// resets such streams.
	Other func(info Online, st *transport.Stream)
	// ReplyTimeout bounds one reply frame written on an ev stream (see
	// [Ingest.OnFrame]); zero means [DefaultReplyTimeout].
	ReplyTimeout time.Duration
	// FirstFrameTimeout bounds the wait for the first frame of an ev stream;
	// zero means [DefaultFirstFrameTimeout].
	FirstFrameTimeout time.Duration
	Logger            *slog.Logger
}

// DefaultFirstFrameTimeout bounds the wait for the first frame of an ev
// stream: a stream that stays silent is reset.
const DefaultFirstFrameTimeout = 10 * time.Second

// DefaultReplyTimeout bounds one frame that the Hub writes on an ev stream.
const DefaultReplyTimeout = 10 * time.Second

// Ingest connects the sessions of the registry to the [EventHub].
type Ingest struct {
	hub *EventHub
	log *slog.Logger

	replyTimeout time.Duration
	firstTimeout time.Duration

	mu      sync.RWMutex
	other   func(Online, *transport.Stream)
	frames  map[string][]func(Online, json.RawMessage, func(any) error)
	streams map[string]StreamFunc
}

// NewIngest returns an Ingest. Hub is required.
func NewIngest(o IngestOptions) (*Ingest, error) {
	if o.Hub == nil {
		return nil, errors.New("workers: the event ingest needs an event hub")
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if o.ReplyTimeout <= 0 {
		o.ReplyTimeout = DefaultReplyTimeout
	}
	if o.FirstFrameTimeout <= 0 {
		o.FirstFrameTimeout = DefaultFirstFrameTimeout
	}
	return &Ingest{hub: o.Hub, other: o.Other, log: log, replyTimeout: o.ReplyTimeout, firstTimeout: o.FirstFrameTimeout}, nil
}

// StreamFunc serves an ev stream that starts with a frame other than the
// event protocol's (see [Ingest.OnStream]). It owns st and runs on its own
// goroutine, which the session waits for when it ends: it must return when
// st fails or ctx is done, and the ingest resets st when ctx is done, so that
// a read that waits wakes up. first is the first frame, already read.
type StreamFunc func(ctx context.Context, info Online, st *transport.Stream, first json.RawMessage)

// OnStream makes fn serve the ev streams whose first frame has type
// frameType, for example "turns.open", the turn channel of Story 7.4: a
// Worker opens it as a stream of its own, so it must not replace the event
// stream of the connection (a stream that starts with events.snapshot does).
// It reports false, and keeps what it had, for a nil fn, for the two types of
// the event protocol and for a type that already has a handler. It is safe to
// call before and while Workers connect; a stream already being served keeps
// its handler.
func (in *Ingest) OnStream(frameType string, fn StreamFunc) bool {
	if fn == nil || frameType == "" || frameType == proto.TypeEventsSnap || frameType == proto.TypeEvent {
		return false
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.streams == nil {
		in.streams = map[string]StreamFunc{}
	}
	if _, taken := in.streams[frameType]; taken {
		return false
	}
	in.streams[frameType] = fn
	return true
}

func (in *Ingest) streamFunc(frameType string) StreamFunc {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.streams[frameType]
}

// OnFrame registers fn for the frames of type frameType that a Worker sends
// on its ev stream besides events.snapshot and event (for example "turn", the
// turn ingestion of Story 7.4, which answers with turn.ack). It is how a later
// module takes over such frames without editing this file; the Worker side
// counterpart is the publisher's OnFrame. Several handlers for one type run in
// registration order. The two types of the event protocol cannot be
// registered; a nil fn is ignored.
//
// fn runs on the stream's reader goroutine, once per frame, with the Worker
// the stream belongs to and a copy of the whole frame. It must not block:
// hand longer work to a goroutine of its own. reply writes one frame on the
// same ev stream; writes are serialised and each is bounded by the reply
// timeout, so a Worker that stops reading makes reply fail (and resets the
// stream, which the Worker reopens with a fresh snapshot) instead of
// blocking for ever. reply may be kept and used after fn returned; it then
// fails once the stream is gone. These frames take no part in the seq check
// of the event protocol, and a type nobody registered is ignored (AD-4).
func (in *Ingest) OnFrame(frameType string, fn func(info Online, body json.RawMessage, reply func(v any) error)) {
	if fn == nil || frameType == "" || frameType == proto.TypeEventsSnap || frameType == proto.TypeEvent {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.frames == nil {
		in.frames = map[string][]func(Online, json.RawMessage, func(any) error){}
	}
	in.frames[frameType] = append(in.frames[frameType], fn)
}

// frameSink returns the sink of serveFeed for one ev stream.
func (in *Ingest) frameSink(info Online, st *transport.Stream) func(string, json.RawMessage) {
	var mu sync.Mutex // serialises the writes of this stream
	reply := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		_ = st.SetWriteDeadline(time.Now().Add(in.replyTimeout))
		err := st.WriteFrame(v)
		_ = st.SetWriteDeadline(time.Time{})
		if err != nil {
			st.Reset() // a half-written frame would corrupt the stream
		}
		return err
	}
	return func(typ string, body json.RawMessage) {
		in.mu.RLock()
		hs := slices.Clone(in.frames[typ])
		in.mu.RUnlock()
		for _, h := range hs {
			h(info, bytes.Clone(body), reply)
		}
	}
}

// SetOther sets (or, with nil, clears) the handler of the inbound streams that
// are not ev streams, for a session that [Ingest.Attach] reads itself (a session
// the connect handler does not serve: a test double). On a session of the
// connect handler the streams of the other kinds go to their own handlers
// (see [StreamHost]), never through the ingest.
func (in *Ingest) SetOther(fn func(info Online, st *transport.Stream)) {
	in.mu.Lock()
	in.other = fn
	in.mu.Unlock()
}

func (in *Ingest) otherHandler() func(Online, *transport.Stream) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.other
}

// Attach connects a Worker's session to the event hub. It is the hook to give
// [Registry.OnAdd].
//
// On a session that the connect handler serves ([StreamHost]) the ingest does
// not read the session: it joins the host as the handler of the ev streams,
// and the host's accept loop, the only one of the session, gives it those.
// On any other session that can accept streams (a test double) it reads the
// session itself, in its own goroutine, until the session ends; a session that
// cannot accept streams is ignored. It returns at once.
func (in *Ingest) Attach(info Online, sess OpSession) {
	if host, ok := HostOf(sess); ok {
		is := in.openSession(info, sess)
		if !host.Handle(transport.KindEvents, is) {
			is.link.Disconnect()
			in.log.Warn("worker events: the ev streams of this session already have a handler", "worker_id", info.WorkerID)
		}
		return
	}
	acc, ok := sess.(Acceptor)
	if !ok {
		return
	}
	is := in.openSession(info, sess)
	go in.run(info, sess, acc, is)
}

// ingestSession is the ingest of one session: the link of the Worker in the
// event hub and the ev streams being served. It is the [StreamHandler] of
// KindEvents.
type ingestSession struct {
	in   *Ingest
	info Online
	link *Link
	// serve runs on the accept loop only; wg is waited for by Close.
	wg sync.WaitGroup
	// last is closed when the stream accepted before the next one has been
	// told apart: see [ingestSession.serveStream].
	last chan struct{}

	// laneMu guards lanes: the live stream of each kind of ev stream (the event
	// stream, and each type of [Ingest.OnStream]). A Worker has one of each: a
	// newer stream replaces the older one, and ends it, its idle read included.
	laneMu sync.Mutex
	lanes  map[string]*laneEntry
}

// laneEntry is the stream that holds a lane of an ingest session.
type laneEntry struct{ end context.CancelFunc }

// takeLane makes the stream with the cancel function end the holder of lane,
// if there is one, and take its place. The returned function gives the lane up;
// it does nothing when a newer stream has replaced this one.
func (is *ingestSession) takeLane(lane string, end context.CancelFunc) (give func()) {
	e := &laneEntry{end: end}
	is.laneMu.Lock()
	old := is.lanes[lane]
	if is.lanes == nil {
		is.lanes = map[string]*laneEntry{}
	}
	is.lanes[lane] = e
	is.laneMu.Unlock()
	if old != nil {
		old.end()
	}
	return func() {
		is.laneMu.Lock()
		if is.lanes[lane] == e {
			delete(is.lanes, lane)
		}
		is.laneMu.Unlock()
	}
}

// eventLane is the lane of the event stream; the others are named by the type
// of their first frame, which can never be this.
const eventLane = "\x00events"

func (in *Ingest) openSession(info Online, sess OpSession) *ingestSession {
	is := &ingestSession{in: in, info: info, link: in.hub.Connect(info.WorkerID, info.Name)}
	if sess != nil {
		// The probe goes over this very session: a Worker that reconnects is
		// asked on its new connection, never on the old one.
		is.link.SetShareProbe(shareProbeOver(sess))
		go in.hub.confirmPendingOnConnect(info.WorkerID)
	}
	return is
}

// Serve implements [StreamHandler]: it takes one ev stream and returns at
// once. The stream is read on a goroutine of its own, because its first frame
// tells what it is: the event stream of the connection (events.snapshot) or a
// stream of another module ([Ingest.OnStream], the turn channel).
func (is *ingestSession) Serve(ctx context.Context, st *transport.Stream, done func()) {
	if done == nil {
		done = streamDone(st)
	}
	prev := is.last
	turn := make(chan struct{})
	is.last = turn
	is.wg.Add(1)
	go func() {
		defer is.wg.Done()
		defer done()
		is.serveStream(ctx, st, prev, turn)
	}()
}

// serveStream serves one ev stream. prev is closed when the stream accepted
// before this one has been told apart and done: a new event stream replaces
// the previous one of the connection, so the event streams take their turn in
// the order of their accept, however long the first frames take to arrive.
// done is closed when this stream is told apart (and has taken its turn), not
// when it ends. Whatever ends the stream before that, a first frame that never
// arrives included, still passes the turn on.
func (is *ingestSession) serveStream(ctx context.Context, st *transport.Stream, prev <-chan struct{}, done chan<- struct{}) {
	var once sync.Once
	pass := func() {
		once.Do(func() {
			if prev != nil {
				select {
				case <-prev:
				case <-ctx.Done():
				}
			}
			close(done)
		})
	}
	defer pass()
	// The stream ends with the session or the Hub, and with the newer stream
	// that replaces it: either way an idle read, of the first frame or of any
	// later one, must not outlive it. The read is interrupted (a Reset does not
	// wake it, see interruptRead) until the handler has returned, because one
	// interrupt can be lost to the drain that Reset starts (see interruptOnDone),
	// and the stream abandoned.
	ctx, end := context.WithCancel(ctx)
	defer end()
	defer interruptOnDone(ctx, st)()
	defer func() {
		if ctx.Err() != nil {
			st.Reset() // the session, the Hub or a newer stream ended it: abandon the stream
		}
	}()

	_ = st.SetReadDeadline(time.Now().Add(is.in.firstTimeout))
	first, err := st.ReadFrame()
	if err != nil {
		st.Reset()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	typ, _ := proto.FrameType(first)
	if typ != proto.TypeEventsSnap && typ != proto.TypeEvent {
		fn := is.in.streamFunc(typ)
		if fn == nil {
			is.in.log.Debug("worker events: an ev stream starts with an unknown frame; reset", "worker_id", is.info.WorkerID, "type", typ)
			st.Reset()
			return
		}
		pass()
		// One stream of this type at a time: this one ends the previous one.
		defer is.takeLane(typ, end)()
		fn(ctx, is.info, st, first)
		return
	}
	// The event stream: it takes over from the previous one.
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			st.Reset()
			return
		}
	}
	feed, err := is.link.NewFeed()
	if err != nil {
		st.Reset()
		return
	}
	defer is.takeLane(eventLane, end)()
	pass()
	if err := serveFeed(ctx, st, feed, first, is.in.frameSink(is.info, st)); err != nil {
		// No frame content in the log: only the Worker and the cause.
		is.in.log.Warn("worker event stream ended with an error", "worker_id", is.info.WorkerID, "error", err.Error())
	}
}

// Close implements [StreamHandler]: the session is over, ctx is done.
func (is *ingestSession) Close() {
	is.wg.Wait()
	is.link.Disconnect()
}

// run is the accept loop of a session that no connect handler serves.
func (in *Ingest) run(info Online, sess OpSession, acc Acceptor, is *ingestSession) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		is.Close()
	}()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		st, err := acc.Accept(ctx)
		if err != nil {
			return
		}
		if st.Kind() != transport.KindEvents {
			if fn := in.otherHandler(); fn != nil {
				fn(info, st)
			} else {
				st.Reset()
			}
			continue
		}
		is.Serve(ctx, st, nil)
	}
}

// shareProbeTimeout bounds one share probe.
const shareProbeTimeout = 5 * time.Second

// shareProbeOver is the Hub's internal share probe on one session: it opens an
// op stream, sends the probe frame (proto.TypeShareProbe) and reads the answer.
// It is not an op of the catalog and is never reachable by a caller.
func shareProbeOver(sess OpSession) func(ctx context.Context, barrier uint64) (ops.ShareProbe, error) {
	return func(ctx context.Context, barrier uint64) (ops.ShareProbe, error) {
		ctx, cancel := context.WithTimeout(ctx, shareProbeTimeout)
		defer cancel()
		st, err := sess.OpenOp(ctx)
		if err != nil {
			return ops.ShareProbe{}, fmt.Errorf("workers: open the share probe stream: %w", err)
		}
		stop := context.AfterFunc(ctx, st.Reset)
		defer stop()
		if err := st.WriteFrame(ops.ShareProbeRequest{Type: proto.TypeShareProbe, OpBarrier: barrier}); err != nil {
			st.Reset()
			return ops.ShareProbe{}, fmt.Errorf("workers: send the share probe: %w", err)
		}
		body, err := readFrame(ctx, st)
		if err != nil {
			st.Reset()
			return ops.ShareProbe{}, fmt.Errorf("workers: read the share probe: %w", err)
		}
		typ, _ := proto.FrameType(body)
		switch typ {
		case proto.TypeOpResult:
			var f OpResult
			if err := proto.DecodeFrame(body, &f); err != nil {
				st.Reset()
				return ops.ShareProbe{}, err
			}
			_ = st.Close()
			var res ops.ShareProbe
			if err := json.Unmarshal(f.Result, &res); err != nil {
				return ops.ShareProbe{}, fmt.Errorf("workers: the share probe answer is unusable: %w", err)
			}
			return res, nil
		case proto.TypeError:
			var f proto.ErrorFrame
			if err := proto.DecodeFrame(body, &f); err != nil {
				st.Reset()
				return ops.ShareProbe{}, err
			}
			_ = st.Close()
			return ops.ShareProbe{}, f.Err()
		}
		st.Reset()
		return ops.ShareProbe{}, errors.New("workers: the share probe was answered with an unexpected frame")
	}
}

// confirmPendingOnConnect asks a Worker that has just connected for its rule if
// the Hub holds restrictions of unknown outcome for it (after a restart or a
// lost answer); with none it does nothing: a read asks anyway.
func (h *EventHub) confirmPendingOnConnect(workerID string) {
	h.mu.Lock()
	h.ensureSharesLocked()
	pending := false
	if st := h.shares[workerID]; st != nil {
		pending = len(st.prov) > 0
	}
	h.mu.Unlock()
	if !pending {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*shareProbeTimeout)
	defer cancel()
	// The Worker's op server may not be serving yet; the next read asks again.
	for i := 0; i < 3; i++ {
		if h.ConfirmShare(ctx, workerID) == nil {
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}
