package agent

// Story 7.4: emitting turns to the Hub (AD-23).
//
// Turn extraction (Story 7.2) hands each agent reply to a [TurnEmitter]. The
// emitter keeps it in an outbox until the Hub has answered with turn.ack, so
// that no turn is lost across a reconnect and, because the Hub commits by
// turn_id, none is stored twice:
//
//   - [TurnEmitter.Emit] validates the turn, refuses the turn of a session that
//     the share selection does not include (local policy is enforced on the
//     Worker, AD-12, A-9: the Hub never sees the text of an unshared session),
//     cuts it so that its encoded frame stays under proto.StreamFrameLimit
//     (see [MaxTurnEncodedBytes]), stores it in the [TurnOutbox] and wakes the
//     sender. It never blocks on the network and works while no Hub is
//     connected;
//   - for every welcomed [Link] the emitter opens the turn channel, an "ev"
//     stream whose first frame is {"type":"turns.open"} (the Hub routes an ev
//     stream by its first frame, the event stream starts with events.snapshot),
//     sends every pending turn, oldest first, and removes a turn from the
//     outbox when the Hub answers turn.ack with status "stored" or "rejected"
//     (a rejected turn is final: sending it again cannot help). An answer
//     "busy" with retry_after_ms is not: the Hub stored nothing because it is
//     out of capacity for the turn now, and the turn stays in the outbox and is
//     sent again not before the time the Hub names (kept between [MinBusyWait]
//     and [MaxBusyWait]). A turn the Hub has not taken within
//     [TurnConfig.MaxAge] is dropped, so that a Hub that stays busy does not
//     keep a turn for ever;
//   - a turn without an ack after [TurnConfig.AckTimeout] is sent again on the
//     same stream, and a new stream (after a reconnect) starts by sending all
//     pending turns again. The Hub answers a repeated turn_id with the same
//     "stored" ack and stores nothing, so repeating is always safe;
//   - before every send the sender asks the share selection again. A turn
//     whose session is no longer shared is removed from the outbox and never
//     sent; a failing lookup sends nothing (fail closed) and the turn stays;
//   - a frame the transport refuses as frame_too_large is final for that turn:
//     it is removed from the outbox (only its turn_id is logged) and the
//     stream is kept, so that one turn cannot block the ones behind it.
//
// Frames of the turn channel (the Hub's view is in internal/hub/workers/pipe.go):
//
//	{"type":"turns.open"}
//	{"type":"turn","turn_id":"<uuidv7>","session":"minion-1","message_id":42,"text":"...","time":"<RFC 3339 ms>"}
//	{"type":"turn.ack","turn_id":"<uuidv7>","status":"stored|rejected|busy","reason":"...","retry_after_ms":1000}
//
// message_id is the id the Hub gave the delivery that the turn answers (it is
// in the pipe_send the Hub routed to this Worker): the Hub stores a turn only as
// the reply to a message it delivered to this Worker and session, once. A turn
// without one cannot be stored, so [TurnEmitter.Emit] refuses it.
//
// The default outbox is in memory ([NewMemOutbox]); a turn is then lost only
// when the Worker process stops before the Hub acknowledged it. A durable
// outbox in worker.db implements the same [TurnOutbox] port.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Frame types of the turn channel; the names are declared in internal/proto.
const (
	// FrameTurnsOpen is the first frame of the turn channel.
	FrameTurnsOpen = proto.TypeTurnsOpen
	// FrameTurn carries one turn to the Hub.
	FrameTurn = proto.TypeTurn
)

// Limits of the turn emitter.
const (
	// MaxTurnBytes is the largest turn text sent to the Hub; a longer reply is
	// cut at a character boundary and ends with [TruncatedMarker].
	MaxTurnBytes = 256 << 10
	// MaxTurnEncodedBytes is the largest size of the turn text once it is JSON
	// encoded in the frame (< > & U+2028 and control characters take six
	// bytes each). It leaves 16 KiB of the 1 MiB stream frame for the rest of
	// the frame and for the Hub's pipe_wait op.progress envelope; a turn is
	// cut to fit both limits.
	MaxTurnEncodedBytes = proto.StreamFrameLimit - 16<<10
	// TruncatedMarker ends a turn that was cut.
	TruncatedMarker = "\n[truncated]"
	// DefaultAckTimeout is how long the emitter waits for a turn.ack before it
	// sends the turn again on the same stream.
	DefaultAckTimeout = 10 * time.Second
	// DefaultOutboxLimit is the number of unacknowledged turns the in-memory
	// outbox holds.
	DefaultOutboxLimit = 1000
	// DefaultTurnMaxAge is how long a turn is kept for the Hub before it is
	// dropped unacknowledged.
	DefaultTurnMaxAge = 24 * time.Hour
	// MinBusyWait and MaxBusyWait bound the retry_after_ms of a busy answer.
	MinBusyWait = time.Second
	MaxBusyWait = 10 * time.Minute
	// DefaultMaxInFlight is the number of turns sent without an answer on one
	// stream. A backlog is sent a window at a time, so that a Hub that is out
	// of capacity answers busy to at most this many turns before the whole
	// channel is held back (see [TurnEmitter]).
	DefaultMaxInFlight = 8

	shareLookupTimeout = 5 * time.Second
)

// ErrOutboxFull is returned by [TurnOutbox.Add] when the outbox cannot take
// another turn.
var ErrOutboxFull = errors.New("agent: the turn outbox is full")

// PendingTurn is a turn that the Hub has not acknowledged.
type PendingTurn struct {
	// TurnID is the UUIDv7 that makes the Hub's commit idempotent.
	TurnID  string
	Session string
	// MessageID is the id the Hub gave the delivery that the turn answers.
	MessageID int64
	Text      string
	At        time.Time
}

// TurnOutbox keeps the turns until the Hub acknowledged them. It must be safe
// for concurrent use.
type TurnOutbox interface {
	// Add stores t. Adding a turn_id that is already there changes nothing.
	Add(t PendingTurn) error
	// Pending returns the turns without an acknowledgement, oldest first.
	Pending() ([]PendingTurn, error)
	// Remove forgets a turn; an unknown turn_id is not an error.
	Remove(turnID string) error
}

// MemOutbox is an in-memory [TurnOutbox].
type MemOutbox struct {
	mu    sync.Mutex
	limit int
	order []string
	byID  map[string]PendingTurn
}

// NewMemOutbox returns an outbox that holds at most limit turns (zero or less
// selects [DefaultOutboxLimit]).
func NewMemOutbox(limit int) *MemOutbox {
	if limit <= 0 {
		limit = DefaultOutboxLimit
	}
	return &MemOutbox{limit: limit, byID: map[string]PendingTurn{}}
}

// Add implements [TurnOutbox].
func (o *MemOutbox) Add(t PendingTurn) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.byID[t.TurnID]; ok {
		return nil
	}
	if len(o.order) >= o.limit {
		return ErrOutboxFull
	}
	o.byID[t.TurnID] = t
	o.order = append(o.order, t.TurnID)
	return nil
}

// Pending implements [TurnOutbox].
func (o *MemOutbox) Pending() ([]PendingTurn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]PendingTurn, 0, len(o.order))
	for _, id := range o.order {
		out = append(out, o.byID[id])
	}
	return out, nil
}

// Remove implements [TurnOutbox].
func (o *MemOutbox) Remove(turnID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.byID[turnID]; !ok {
		return nil
	}
	delete(o.byID, turnID)
	for i, id := range o.order {
		if id == turnID {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	return nil
}

// ErrNotShared is returned by [TurnEmitter.Emit] for a turn of a session that
// the share selection does not include.
var ErrNotShared = errors.New("agent: the session is not shared; its turns are not sent to the Hub")

// ShareChecker tells whether a session is shared with the Hub. *Policy
// implements it.
type ShareChecker interface {
	Shared(ctx context.Context, session string) (bool, error)
}

var _ ShareChecker = (*Policy)(nil)

// TurnConfig configures a [TurnEmitter].
type TurnConfig struct {
	// Shares applies the share selection to the turn channel. Required: the
	// turn channel bypasses the event publisher's observer, so the emitter
	// itself keeps the turns of unshared sessions on the Worker.
	Shares ShareChecker
	// Outbox keeps unacknowledged turns; nil selects a new [MemOutbox].
	Outbox TurnOutbox
	// Clock is the time source of the ack timeout and the reopen wait; nil
	// means the system clock.
	Clock transport.Clock
	// AckTimeout, ReopenDelay and WriteTimeout default to
	// [DefaultAckTimeout], [DefaultReopenDelay] and [DefaultWriteTimeout].
	AckTimeout   time.Duration
	ReopenDelay  time.Duration
	WriteTimeout time.Duration
	// MaxAge is how long a turn waits for the Hub's acknowledgement before it
	// is dropped; zero selects [DefaultTurnMaxAge].
	MaxAge time.Duration
	// MaxInFlight bounds the turns sent and not yet answered on one stream;
	// zero selects [DefaultMaxInFlight].
	MaxInFlight int
	// Logger receives operational messages: never turn text (AD-20). Nil
	// discards them.
	Logger *slog.Logger
}

// TurnEmitter sends turns to the Hub and retries them until acknowledged.
type TurnEmitter struct {
	cfg  TurnConfig
	out  TurnOutbox
	clk  transport.Clock
	log  *slog.Logger
	kick chan struct{}

	// The busy holds belong to the emitter, not to a stream: the Hub tells when
	// it is out of capacity, and a stream that is reset or a link that is
	// opened again (a reconnect) must not forget it, or the backlog would be
	// sent again at once and read as a Worker that ignores busy answers.
	// holdMu guards both. busyUntil is bounded by the outbox: a hold is dropped
	// with its turn.
	holdMu    sync.Mutex
	chanHold  time.Time            // no turn is sent before this time
	busyUntil map[string]time.Time // by turn_id: this turn is not sent before
	// cwnd is the window after a busy answer: one turn, to find out whether
	// there is room again, and one more for every turn the Hub takes, until it
	// is the full window again (zero). The Hub's limits are not known to the
	// Worker, so a full window after every single success would send a window
	// of turns into a limit that has room for one.
	cwnd int
}

// NewTurnEmitter returns a turn emitter. Register [TurnEmitter.Serve] with
// [Connector.OnLink]. It fails without cfg.Shares.
func NewTurnEmitter(cfg TurnConfig) (*TurnEmitter, error) {
	if cfg.Shares == nil {
		return nil, errors.New("agent: a TurnEmitter needs a Shares port (the share selection)")
	}
	e := &TurnEmitter{cfg: cfg, out: cfg.Outbox, clk: cfg.Clock, log: cfg.Logger, kick: make(chan struct{}, 1), busyUntil: map[string]time.Time{}}
	if e.out == nil {
		e.out = NewMemOutbox(0)
	}
	if e.clk == nil {
		e.clk = transport.SystemClock
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if e.cfg.AckTimeout <= 0 {
		e.cfg.AckTimeout = DefaultAckTimeout
	}
	if e.cfg.ReopenDelay <= 0 {
		e.cfg.ReopenDelay = DefaultReopenDelay
	}
	if e.cfg.WriteTimeout <= 0 {
		e.cfg.WriteTimeout = DefaultWriteTimeout
	}
	if e.cfg.MaxAge <= 0 {
		e.cfg.MaxAge = DefaultTurnMaxAge
	}
	if e.cfg.MaxInFlight <= 0 {
		e.cfg.MaxInFlight = DefaultMaxInFlight
	}
	return e, nil
}

// encodedRuneLen is the largest number of bytes the rune takes inside a JSON
// string written by encoding/json (which escapes <, >, & and U+2028/2029).
func encodedRuneLen(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
		return 6
	}
	return utf8.RuneLen(r)
}

// jsonStringLen is the size of text as a JSON string value, without quotes.
func jsonStringLen(text string) int {
	b, err := json.Marshal(text)
	if err != nil {
		return 6 * len(text)
	}
	return len(b) - 2
}

// truncateTurn cuts text at a character boundary so that it is at most
// MaxTurnBytes long and its JSON encoding at most MaxTurnEncodedBytes, and
// marks the cut. text must be valid UTF-8.
func truncateTurn(text string) string {
	if len(text) <= MaxTurnBytes && jsonStringLen(text) <= MaxTurnEncodedBytes {
		return text
	}
	rawBudget := MaxTurnBytes - len(TruncatedMarker)
	encBudget := MaxTurnEncodedBytes - jsonStringLen(TruncatedMarker)
	cut, enc := 0, 0
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		enc += encodedRuneLen(r)
		if i+n > rawBudget || enc > encBudget {
			break
		}
		i += n
		cut = i
	}
	return text[:cut] + TruncatedMarker
}

// Emit queues a turn for the Hub. An empty TurnID gets a new UUIDv7 and a
// zero At the current time; the (possibly new) turn_id is returned. It does
// not wait for the Hub.
func (e *TurnEmitter) Emit(t PendingTurn) (string, error) {
	if err := (ops.Target{SessionName: t.Session}).Validate(); err != nil {
		return "", err
	}
	if t.Text == "" || !utf8.ValidString(t.Text) {
		return "", errors.New("agent: a turn needs non-empty UTF-8 text")
	}
	if t.MessageID <= 0 {
		return "", errors.New("agent: a turn needs the message_id of the delivery it answers")
	}
	if t.TurnID == "" {
		t.TurnID = ops.NewUUIDv7()
	} else if !ops.ValidUUIDv7(t.TurnID) {
		return "", errors.New("agent: turn_id must be a UUIDv7")
	}
	ctx, cancel := context.WithTimeout(context.Background(), shareLookupTimeout)
	ok, err := e.cfg.Shares.Shared(ctx, t.Session)
	cancel()
	switch {
	case err != nil:
		return "", fmt.Errorf("agent: cannot read the share selection; the turn is not kept: %w", err)
	case !ok:
		return "", ErrNotShared
	}
	if t.At.IsZero() {
		t.At = e.clk.Now()
	}
	t.Text = truncateTurn(t.Text)
	if err := e.out.Add(t); err != nil {
		return "", err
	}
	select {
	case e.kick <- struct{}{}:
	default:
	}
	return t.TurnID, nil
}

// turnFrame is the "turn" frame.
type turnFrame struct {
	Type      string        `json:"type"`
	TurnID    string        `json:"turn_id"`
	Session   string        `json:"session"`
	MessageID int64         `json:"message_id"`
	Text      string        `json:"text"`
	Time      ops.Timestamp `json:"time"`
}

// Serve sends the turns over one link until it ends. It is a [LinkHandler].
func (e *TurnEmitter) Serve(l *Link) { e.serve(l.Context(), openerOf(l.Session)) }

// ServeStreams is [TurnEmitter.Serve] with the turn channels opened by open
// instead of by the session of a link. It returns when ctx ends. It is the seam
// of the composition tests that run the emitter against the Hub's turn server
// without a yamux session in between.
func (e *TurnEmitter) ServeStreams(ctx context.Context, open EventOpener) { e.serve(ctx, open) }

func (e *TurnEmitter) serve(ctx context.Context, open EventOpener) {
	for ctx.Err() == nil {
		st, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.log.Debug("cannot open the turn channel", "error", err.Error())
		} else if e.run(ctx, st) {
			return
		}
		select {
		case <-e.clk.After(e.cfg.ReopenDelay):
		case <-ctx.Done():
			return
		}
	}
}

func (e *TurnEmitter) write(st EventStream, v any) error {
	_ = st.SetWriteDeadline(time.Now().Add(e.cfg.WriteTimeout))
	return st.WriteFrame(v)
}

// holdFor records a busy answer: the Hub is out of capacity, so no turn goes
// out before now+wait (every limit it names is the channel's, not the turn's),
// and this turn is held until then in any case.
func (e *TurnEmitter) holdFor(turnID string, wait time.Duration) {
	until := e.clk.Now().Add(wait)
	e.holdMu.Lock()
	defer e.holdMu.Unlock()
	e.chanHold = laterOf(e.chanHold, until)
	e.busyUntil[turnID] = laterOf(e.busyUntil[turnID], until)
	e.cwnd = 1
}

// window returns how many turns may be unanswered at once.
func (e *TurnEmitter) window() int {
	e.holdMu.Lock()
	defer e.holdMu.Unlock()
	if e.cwnd > 0 {
		return min(e.cwnd, e.cfg.MaxInFlight)
	}
	return e.cfg.MaxInFlight
}

// openWindow widens the window by one: the Hub took a turn.
func (e *TurnEmitter) openWindow() {
	e.holdMu.Lock()
	if e.cwnd > 0 {
		if e.cwnd++; e.cwnd >= e.cfg.MaxInFlight {
			e.cwnd = 0
		}
	}
	e.holdMu.Unlock()
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// holdLeft returns how long the channel is held and when the turn is, as
// remaining times at now; zero means not held.
func (e *TurnEmitter) holdLeft(turnID string, now time.Time) (channel, turn time.Duration) {
	e.holdMu.Lock()
	defer e.holdMu.Unlock()
	if d := e.chanHold.Sub(now); d > 0 {
		channel = d
	}
	if u, ok := e.busyUntil[turnID]; ok {
		if d := u.Sub(now); d > 0 {
			turn = d
		}
	}
	return channel, turn
}

// releaseHold forgets the hold of a turn that left the outbox or was sent.
func (e *TurnEmitter) releaseHold(turnID string) {
	e.holdMu.Lock()
	delete(e.busyUntil, turnID)
	e.holdMu.Unlock()
}

// pruneHolds drops the holds of turns that are not pending any more, so the map
// stays as small as the outbox.
func (e *TurnEmitter) pruneHolds(pending map[string]struct{}) {
	e.holdMu.Lock()
	for id := range e.busyUntil {
		if _, ok := pending[id]; !ok {
			delete(e.busyUntil, id)
		}
	}
	e.holdMu.Unlock()
}

// run serves one stream and reports whether the link is over.
//
// Sending is paced by the Hub. At most [TurnConfig.MaxInFlight] turns are sent
// without an answer, so a backlog goes out a window at a time and a refusal is
// learnt after a window, not after the whole outbox; after a busy answer the
// window is one turn, widened by one for every turn the Hub takes. A busy answer holds the
// whole channel until its retry_after (the limits behind it, rates, stored
// volume and database pressure, are the channel's) and the held turn is not
// sent before it either; both holds live in the emitter, so they survive a
// stream that is reset and a link that is opened again. Only a turn that is
// still pending is sent, an acknowledged turn is never sent again, and a turn
// that stays unanswered is sent again after the ack timeout with the same id.
func (e *TurnEmitter) run(ctx context.Context, st EventStream) (linkDone bool) {
	if err := e.write(st, map[string]string{"type": FrameTurnsOpen}); err != nil {
		st.Reset()
		return ctx.Err() != nil
	}
	var (
		// acked and sentAt belong to this stream and are guarded by mu: the
		// reader sets them, the sender reads them. sentAt is the time of the
		// last send of a turn on this stream.
		mu     sync.Mutex
		acked  = map[string]struct{}{}
		sentAt = map[string]time.Time{}
	)
	wakeSender := func() {
		select {
		case e.kick <- struct{}{}:
		default:
		}
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			if typ, err := proto.FrameType(body); err != nil || typ != proto.TypeTurnAck {
				continue
			}
			var a struct {
				TurnID       string `json:"turn_id"`
				Status       string `json:"status"`
				Reason       string `json:"reason"`
				RetryAfterMS int64  `json:"retry_after_ms"`
			}
			if json.Unmarshal(body, &a) != nil {
				continue
			}
			if a.Status == "busy" {
				// Not an acknowledgement: keep the turn, and hold the channel
				// until the time the Hub names, within bounds.
				wait := min(max(time.Duration(a.RetryAfterMS)*time.Millisecond, MinBusyWait), MaxBusyWait)
				e.holdFor(a.TurnID, wait)
				mu.Lock()
				delete(sentAt, a.TurnID)
				mu.Unlock()
				e.log.Info("the Hub is out of capacity for a turn; the turn channel is held back", "turn_id", a.TurnID, "wait", wait.String())
				wakeSender()
				continue
			}
			if a.Status != "stored" && a.Status != "rejected" {
				continue
			}
			mu.Lock()
			acked[a.TurnID] = struct{}{}
			delete(sentAt, a.TurnID)
			mu.Unlock()
			if err := e.out.Remove(a.TurnID); err != nil {
				e.log.Warn("cannot remove an acknowledged turn", "turn_id", a.TurnID, "error", err.Error())
			}
			e.releaseHold(a.TurnID)
			e.openWindow()
			if a.Status == "rejected" {
				e.log.Warn("the Hub rejected a turn", "turn_id", a.TurnID, "reason", a.Reason)
			}
			wakeSender() // the window has room again
		}
	}()

	for {
		pending, err := e.out.Pending()
		if err != nil {
			e.log.Warn("cannot read the turn outbox", "error", err.Error())
		}
		now := e.clk.Now()
		ids := make(map[string]struct{}, len(pending))
		for _, t := range pending {
			ids[t.TurnID] = struct{}{}
		}
		e.pruneHolds(ids)
		mu.Lock()
		for id := range acked {
			if _, ok := ids[id]; !ok {
				delete(acked, id)
			}
		}
		for id := range sentAt {
			if _, ok := ids[id]; !ok {
				delete(sentAt, id)
			}
		}
		inflight := 0
		for id, at := range sentAt {
			if _, done := acked[id]; !done && now.Sub(at) < e.cfg.AckTimeout {
				inflight++
			}
		}
		mu.Unlock()

		wake := e.cfg.AckTimeout // until the next turn is due
		window := e.window()
		for _, t := range pending {
			if age := now.Sub(t.At); age > e.cfg.MaxAge {
				e.log.Warn("a turn the Hub did not take in time is dropped", "turn_id", t.TurnID, "age", age.Round(time.Second).String())
				e.forget(t.TurnID)
				continue
			}
			mu.Lock()
			_, done := acked[t.TurnID]
			at, sent := sentAt[t.TurnID]
			mu.Unlock()
			if done {
				continue
			}
			channel, held := e.holdLeft(t.TurnID, now)
			if left := max(channel, held); left > 0 {
				wake = min(wake, left)
				continue
			}
			if sent && now.Sub(at) < e.cfg.AckTimeout {
				wake = min(wake, e.cfg.AckTimeout-now.Sub(at))
				continue
			}
			if inflight >= window {
				continue // an answer wakes the sender
			}
			if !e.mayPass(ctx, t) {
				continue
			}
			f := turnFrame{Type: FrameTurn, TurnID: t.TurnID, Session: t.Session, MessageID: t.MessageID, Text: t.Text, Time: ops.Timestamp(t.At)}
			if err := e.write(st, f); err != nil {
				if errors.Is(err, proto.ErrFrameTooLarge) {
					// Final for this turn: sending it again cannot help and
					// would block every turn behind it. Nothing was written,
					// so the stream stays.
					e.log.Warn("a turn is too large for one frame and is dropped", "turn_id", t.TurnID)
					e.forget(t.TurnID)
					continue
				}
				st.Reset()
				return ctx.Err() != nil
			}
			mu.Lock()
			inflight++ // a send that is due again was not counted: its time had run out
			sentAt[t.TurnID] = now
			mu.Unlock()
			e.releaseHold(t.TurnID)
		}
		select {
		case <-ctx.Done():
			_ = st.Close()
			return true
		case <-gone:
			st.Reset()
			return ctx.Err() != nil
		case <-e.kick:
		case <-e.clk.After(max(wake, 10*time.Millisecond)):
		}
	}
}

// mayPass reports whether t may be sent now: its session is shared. The share
// selection is asked again immediately before every turn, never cached across
// a batch, so a revocation takes effect for the very next send. A turn of a
// session that is no longer shared is removed from the outbox and never sent;
// a lookup that fails sends nothing and keeps the turn (fail closed).
func (e *TurnEmitter) mayPass(ctx context.Context, t PendingTurn) bool {
	lctx, cancel := context.WithTimeout(ctx, shareLookupTimeout)
	ok, err := e.cfg.Shares.Shared(lctx, t.Session)
	cancel()
	if err != nil {
		e.log.Warn("cannot read the share selection; the turn is held back", "turn_id", t.TurnID, "error", err.Error())
		return false
	}
	if !ok {
		e.log.Info("a turn of a session that is no longer shared is dropped", "turn_id", t.TurnID)
		e.forget(t.TurnID)
	}
	return ok
}

func (e *TurnEmitter) forget(turnID string) {
	e.releaseHold(turnID)
	if err := e.out.Remove(turnID); err != nil {
		e.log.Warn("cannot remove a dropped turn", "turn_id", turnID, "error", err.Error())
	}
}
