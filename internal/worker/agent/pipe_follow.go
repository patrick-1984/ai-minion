package agent

// Story 7.3: the reply extraction of the pipe (Option B, spine AM-5, EPICS
// A-18).
//
// After a pipe message was pasted and submitted, one goroutine per session
// follows the delivery with a [agentdetect.TurnExtractor] (Story 7.2):
//
//   - it reads frames with Anchorer.ReadFrame(Need()) only while the extractor
//     is Armed, one per poll interval;
//   - a frame that completes the reply gives a turn, which goes to the sink of
//     the delivery (see [pipeRoute]) and ends the goroutine;
//   - term.ErrNotFound (the pane is gone) flushes the extractor, so the reply
//     read so far is kept, and ends the goroutine;
//   - any other read error, and any other error of Observe, is retried at the
//     next poll, at most [DefaultMaxReadFailures] in a row; then the reply read
//     so far is flushed and the goroutine ends;
//   - ErrTurnUnavailable (the evidence was invalidated, for example by a
//     resize or by tmux without the row counters) ends it without a turn: the
//     message was delivered and nothing is made up;
//   - a delivery that is not over after [DefaultMaxFollow] is flushed and ends.
//
// A message is never written again by this lifecycle, whatever happens to the
// reading. A newer delivery to the same session ends the older one first and
// keeps the reply the older one had read (Flush).

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Defaults of the reply extraction.
const (
	// DefaultFollowPoll is the interval between two frames of a delivery.
	DefaultFollowPoll = 500 * time.Millisecond
	// DefaultFollowSettle is the number of consecutive stable frames that
	// complete a reply: it rides over an agent that rests for one poll
	// between two tool calls.
	DefaultFollowSettle = 2
	// DefaultMaxFollow bounds how long one delivery is followed.
	DefaultMaxFollow = 30 * time.Minute
	// DefaultMaxReadFailures is the number of read errors in a row after
	// which the reply read so far is flushed and the delivery is dropped.
	DefaultMaxReadFailures = 20

	commitTimeout = 10 * time.Second
	commitTries   = 3
)

// followRequest is one delivered message to follow.
type followRequest struct {
	Session string
	Pane    term.PaneRef
	Agent   string
	Text    string
	Mark    term.DeliveryMark
	Route   pipeRoute
	// MessageID is the id the Hub gave the delivery (Hub mode): the reply
	// names it, which is how the Hub knows the turn answers something it sent.
	MessageID int64
}

// follow is the goroutine of one delivery.
type follow struct {
	session   string
	route     pipeRoute
	messageID int64
	ext       *agentdetect.TurnExtractor
	cancel    context.CancelFunc
	done      chan struct{}
}

// follow starts the extraction of a delivered message. The delivery was
// already checked: its mark can anchor a turn.
func (s *PipeService) follow(r followRequest) {
	anch, err := agentdetect.AnchorerOf(s.ex.host)
	if err != nil {
		return // cannot happen: a mark came from the host's Anchorer
	}
	ext, err := agentdetect.NewTurnExtractor(agentdetect.TurnConfig{Session: r.Session, Agent: r.Agent, Settle: s.cfg.Settle, Now: s.now})
	if err != nil {
		s.log.Warn("cannot follow a pipe delivery", "session", r.Session, "error", err.Error())
		return
	}
	ext.Delivered(r.Text, r.Mark)

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	f := &follow{session: r.Session, route: r.Route, messageID: r.MessageID, ext: ext, cancel: cancel, done: make(chan struct{})}
	s.follows[r.Session] = f
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runFollow(ctx, f, anch, r.Pane)
}

// supersede ends the extraction that follows session, if any, and delivers
// the reply it had read. It returns when the goroutine is gone.
func (s *PipeService) supersede(session string) {
	s.mu.Lock()
	f := s.follows[session]
	delete(s.follows, session)
	s.mu.Unlock()
	if f == nil {
		return
	}
	f.cancel()
	<-f.done
	s.flush(f)
}

// flush hands the reply read so far to the sink and disarms the extractor.
func (s *PipeService) flush(f *follow) {
	t, err := f.ext.Flush()
	switch {
	case err != nil && errors.Is(err, agentdetect.ErrTurnUnavailable):
		s.logUnavailable(f.session, err)
	case err != nil:
		s.log.Warn("cannot finish a pipe reply", "session", f.session, "error", err.Error())
	case t != nil:
		s.deliverTurn(f.route, f.messageID, t)
	}
}

func (s *PipeService) logUnavailable(session string, err error) {
	var ue *agentdetect.UnavailableError
	reason := "unknown"
	if errors.As(err, &ue) {
		reason = ue.Reason
	}
	s.log.Info("no pipe turn: the evidence of the delivery no longer holds", "session", session, "reason", reason)
}

func (s *PipeService) runFollow(ctx context.Context, f *follow, anch term.Anchorer, pane term.PaneRef) {
	defer s.wg.Done()
	defer close(f.done)
	defer func() {
		s.mu.Lock()
		if s.follows[f.session] == f {
			delete(s.follows, f.session)
		}
		s.mu.Unlock()
	}()

	tick := time.NewTicker(s.cfg.Poll)
	defer tick.Stop()
	limit := time.NewTimer(s.cfg.MaxFollow)
	defer limit.Stop()
	failures := 0
	retry := func(what string, err error) (giveUp bool) {
		failures++
		s.log.Debug("pipe reply read failed; retrying at the next poll", "session", f.session, "step", what, "failures", failures, "error", err.Error())
		if failures < s.cfg.MaxReadFailures {
			return false
		}
		s.log.Warn("giving up the reply of a pipe message after repeated read errors", "session", f.session, "failures", failures)
		s.flush(f)
		return true
	}

	// The extractor is read only while it is armed.
	for f.ext.Armed() {
		select {
		case <-ctx.Done():
			return // superseded (which flushes) or the Worker is stopping
		case <-limit.C:
			s.log.Warn("the reply to a pipe message did not finish in time", "session", f.session, "after", s.cfg.MaxFollow.String())
			s.flush(f)
			return
		case <-tick.C:
		}
		frame, err := anch.ReadFrame(ctx, pane, f.ext.Need())
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, term.ErrNotFound):
				// The pane is gone: keep the reply read so far.
				s.flush(f)
				return
			case retry("read", err):
				return
			}
			continue
		}
		turn, err := f.ext.Observe(frame)
		if err != nil {
			if errors.Is(err, agentdetect.ErrTurnUnavailable) {
				s.logUnavailable(f.session, err)
				return
			}
			if retry("observe", err) {
				return
			}
			continue
		}
		failures = 0
		if turn != nil {
			s.deliverTurn(f.route, f.messageID, turn)
			return
		}
	}
	s.log.Debug("the extractor ended a pipe delivery without a turn", "session", f.session)
}

// deliverTurn hands a finished reply to the sink of its delivery: the Worker's
// own store in Solo mode, the turn emitter in Hub mode.
func (s *PipeService) deliverTurn(route pipeRoute, messageID int64, t *agentdetect.Turn) {
	text := truncateTurn(t.Text)
	if text == "" {
		return
	}
	switch route {
	case routeSolo:
		s.commitSolo(t.Session, t.TurnID, text, t.At)
	default:
		s.emitHub(t.Session, t.TurnID, messageID, text, t.At)
	}
}

// commitSolo commits a turn to the Solo broker. The commit is idempotent by
// turn_id, so a store error is retried a few times without risk.
func (s *PipeService) commitSolo(session, turnID, text string, at time.Time) {
	peer := soloPeer(session)
	var err error
	for try := 0; try < commitTries; try++ {
		ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
		_, err = s.broker.CommitTurn(ctx, pipe.Turn{TurnID: turnID, Peer: peer, Text: text, At: at})
		cancel()
		if err == nil {
			s.notify(peer)
			return
		}
		if pipe.CodeOf(err) == pipe.CodeInvalidArgument {
			break // retrying cannot help
		}
		time.Sleep(time.Duration(try+1) * 100 * time.Millisecond)
	}
	s.log.Error("cannot store a pipe reply", "session", session, "turn_id", turnID, "error", err.Error())
}

// emitHub hands a turn to the turn emitter, which keeps it in the durable
// outbox until the Hub acknowledges it and asks the share selection before
// every send.
func (s *PipeService) emitHub(session, turnID string, messageID int64, text string, at time.Time) {
	var sink TurnSink
	if s.cfg.Hub != nil {
		sink = s.cfg.Hub.Turns()
	}
	if sink == nil {
		s.log.Warn("a pipe reply was dropped: this Worker has no turn emitter", "session", session, "turn_id", turnID)
		return
	}
	if _, err := sink.Emit(PendingTurn{TurnID: turnID, Session: session, MessageID: messageID, Text: text, At: at}); err != nil {
		if errors.Is(err, ErrNotShared) {
			s.log.Info("a pipe reply stays on the Worker: the session is not shared", "session", session)
			return
		}
		s.log.Warn("a pipe reply could not be queued for the Hub", "session", session, "turn_id", turnID, "error", err.Error())
	}
}
