package agent

// Story 7.3: the Solo broker and the local entry of the pipe (AD-23, AD-22).
//
// [PipeService] serves pipe_send, pipe_read and pipe_wait for calls from this
// host (the local socket, which MCP and `mn` use) and owns the reply
// extraction (pipe_follow.go). Its mode comes from the enrollment of the
// Worker, read for every call:
//
//   - Solo (no enrollment): the Worker is the broker. pipe_send passes the
//     limits of internal/pipe (8 KiB, 1 per second with a burst of 5, 120 per
//     hour, the task budget of 200 per 24 hours), stores the message in the
//     Worker store (migration 0004), runs the guarded delivery of exec_pipe.go
//     and retracts the message again when nothing reached the pane
//     (failed_not_sent or an error), so that it neither shows in the pipe nor
//     uses budget. A request_id is one send: a repeat inside the 10 minutes of
//     the result cache is answered from it and stores and charges nothing.
//     pipe_read and pipe_wait read the same store. The local owner trusts
//     every peer (mcp.TrustAll): nothing is withheld.
//   - Hub mode (an enrollment exists): the Hub is the broker and the store.
//     The three ops are forwarded unchanged to the Hub with the [CallClient]:
//     the Worker's own broker and store are not used. With no Hub connection
//     the call fails at once with worker_offline and nothing is queued, and
//     there is no fallback to Solo. The answer of the Hub, withheld, reason and
//     notice included, is returned as it is. A write whose connection was lost
//     after it was sent has the outcome unknown (the Hub may have applied it)
//     and is never repeated here.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Defaults of the Solo pipe reads.
const (
	// DefaultPipeReadLimit is the number of entries pipe_read returns when the
	// request gives no limit.
	DefaultPipeReadLimit = 50
	// DefaultPipeWait is how long pipe_wait waits when the request gives no
	// timeout.
	DefaultPipeWait = 30 * time.Second
	// DefaultPipePoll is the fallback poll of a waiting pipe_wait; a turn
	// committed on this Worker wakes it at once.
	DefaultPipePoll = time.Second
	// DefaultPipePruneInterval is how often the retention (7 days) is applied.
	DefaultPipePruneInterval = time.Hour
	// PipePageBytes is the budget of the entries of one pipe_read page, JSON
	// encoded: the result always fits one frame (AD-3).
	PipePageBytes = proto.StreamFrameLimit - 16<<10
)

// PipeStore is the persistence of the Solo pipe. *store.Store implements it.
type PipeStore interface {
	pipe.Store
	DeleteMessage(ctx context.Context, id int64) error
	Entries(ctx context.Context, peer pipe.Peer, afterID string, limit int) ([]store.PipeEntry, error)
	LatestTurnID(ctx context.Context, peer pipe.Peer) (string, error)
}

var _ PipeStore = (*store.Store)(nil)

// PipeCaller forwards a call to the Hub. *CallClient implements it.
type PipeCaller interface {
	Do(ctx context.Context, op string, args, out any) error
	Stream(ctx context.Context, op string, args any, onElement func(json.RawMessage) error) error
}

// TurnSink takes the reply of a message that the Hub brokered. *TurnEmitter
// implements it.
type TurnSink interface {
	Emit(t PendingTurn) (string, error)
}

var (
	_ PipeCaller = (*CallClient)(nil)
	_ TurnSink   = (*TurnEmitter)(nil)
)

// PipeHub is what the pipe needs of the Hub mode. It is a seam because the
// services of the Hub links start after the pipe module: the real one looks
// them up for every call, so no start order matters ([NewRuntimePipeHub]).
type PipeHub interface {
	// Enrolled reports whether this Worker is enrolled with a Hub (Hub mode).
	Enrolled(ctx context.Context) (bool, error)
	// Calls returns the call client, or nil when there is none.
	Calls() PipeCaller
	// Turns returns the turn emitter, or nil when there is none.
	Turns() TurnSink
}

// PipeConfig configures a [PipeService]. Executor, Store and Hub are required.
type PipeConfig struct {
	Executor *Executor
	Store    PipeStore
	Hub      PipeHub
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// BudgetPerDay is the task budget of the Solo broker; zero selects the
	// default of 200 messages per peer per 24 hours.
	BudgetPerDay int
	// Poll, Settle, MaxFollow and MaxReadFailures tune the reply extraction;
	// zero selects the defaults above.
	Poll            time.Duration
	Settle          int
	MaxFollow       time.Duration
	MaxReadFailures int
	// WaitPoll is the fallback poll of pipe_wait; zero selects
	// [DefaultPipePoll].
	WaitPoll time.Duration
	// Logger receives operational messages, never the text of a message or of
	// a turn. Nil discards them.
	Logger *slog.Logger
}

// PipeService is the pipe of one Worker.
type PipeService struct {
	cfg    PipeConfig
	ex     *Executor
	broker *pipe.Broker
	store  PipeStore
	log    *slog.Logger
	now    func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	stopped bool
	follows map[string]*follow
	subs    map[pipe.Peer]map[chan struct{}]struct{}
}

// NewPipeService builds the service and attaches it to the executor.
func NewPipeService(cfg PipeConfig) (*PipeService, error) {
	switch {
	case cfg.Executor == nil:
		return nil, errors.New("agent: the pipe needs the executor")
	case cfg.Store == nil:
		return nil, errors.New("agent: the pipe needs a store")
	case cfg.Hub == nil:
		return nil, errors.New("agent: the pipe needs its Hub seam")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Poll <= 0 {
		cfg.Poll = DefaultFollowPoll
	}
	if cfg.Settle <= 0 {
		cfg.Settle = DefaultFollowSettle
	}
	if cfg.MaxFollow <= 0 {
		cfg.MaxFollow = DefaultMaxFollow
	}
	if cfg.MaxReadFailures <= 0 {
		cfg.MaxReadFailures = DefaultMaxReadFailures
	}
	if cfg.WaitPoll <= 0 {
		cfg.WaitPoll = DefaultPipePoll
	}
	b, err := pipe.NewBroker(pipe.Config{Store: cfg.Store, Now: cfg.Now, BudgetPerDay: cfg.BudgetPerDay})
	if err != nil {
		return nil, err
	}
	s := &PipeService{
		cfg: cfg, ex: cfg.Executor, broker: b, store: cfg.Store, log: cfg.Logger, now: cfg.Now,
		follows: map[string]*follow{}, subs: map[pipe.Peer]map[chan struct{}]struct{}{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.ex.AttachPipe(s)
	return s, nil
}

// Broker returns the Solo broker.
func (s *PipeService) Broker() *pipe.Broker { return s.broker }

// Stop ends every reply extraction and waits for them. A reply that was not
// finished is dropped: it cannot be read once the Worker is gone.
func (s *PipeService) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("the pipe did not stop in time")
	}
}

// RunPrune applies the retention (7 days) to the Solo store every interval
// until ctx ends.
func (s *PipeService) RunPrune(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPipePruneInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if res, err := s.broker.Prune(ctx, s.now()); err != nil {
				s.log.Warn("cannot prune the pipe", "error", err.Error())
			} else if res.Messages+res.Turns > 0 {
				s.log.Debug("pruned the pipe", "messages", res.Messages, "turns", res.Turns)
			}
		}
	}
}

func soloPeer(session string) pipe.Peer {
	return pipe.Peer{WorkerID: SoloWorkerID, SessionName: session}
}

// IsPipeOp reports whether op is served by [PipeService.Local]: the pipe ops
// of a call from this host.
func IsPipeOp(op string) bool {
	switch op {
	case ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait:
		return true
	}
	return false
}

// Local serves one pipe op of a call from this host. The caller is the local
// owner (the local socket); the request is never a Hub request, and this
// method is not reachable from the op server. ctx may carry [WithProgress]
// for pipe_wait.
func (s *PipeService) Local(ctx context.Context, rq Request) (json.RawMessage, error) {
	if !IsPipeOp(rq.Op) {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "not a pipe operation")
	}
	hubMode, err := s.cfg.Hub.Enrolled(ctx)
	if err != nil {
		// Never fall back to Solo when the mode cannot be told: that would
		// store a Hub pipe's message in the wrong place.
		return nil, internalErr("read the Hub enrollment", err)
	}
	if !hubMode {
		// Solo: the pipe serves this Worker's own sessions. A target that names a
		// Worker would be taken for a same-named local session, so it is refused
		// before anything is read or written (the Hub is the only router to another
		// Worker, and there is none).
		if err := refuseSoloWorkerTarget(rq.Args); err != nil {
			return nil, err
		}
	}
	var res any
	switch {
	case hubMode:
		return s.forward(ctx, rq)
	case rq.Op == ops.OpPipeSend:
		res, err = s.soloSend(ctx, rq)
	case rq.Op == ops.OpPipeRead:
		res, err = s.soloRead(ctx, rq)
	default:
		res, err = s.soloWait(ctx, rq)
	}
	if err != nil {
		return nil, err
	}
	b, merr := json.Marshal(res)
	if merr != nil {
		return nil, internalErr("encode result", merr)
	}
	return b, nil
}

// refuseSoloWorkerTarget refuses a pipe request whose target names a Worker.
func refuseSoloWorkerTarget(args json.RawMessage) error {
	var a struct {
		Target *ops.Target `json:"target"`
	}
	if json.Unmarshal(args, &a) != nil || a.Target == nil || a.Target.WorkerID == "" {
		return nil
	}
	return apiErr(proto.CodeInvalidArgument, "", "the target names a Worker, but this Worker runs without a Hub: the pipe can only reach its own sessions; give just the session name")
}

// pipeAPIError maps an error of the broker to the API error of the same code.
func pipeAPIError(what string, err error) error {
	var pe *pipe.Error
	if !errors.As(err, &pe) {
		return internalErr(what, err)
	}
	ae := apiErr(proto.Code(pe.Code), "", pe.Message)
	if pe.Code == pipe.CodePipeRateLimited && pe.RetryAfter > 0 {
		ae.Details, _ = json.Marshal(map[string]int64{"retry_after_ms": pe.RetryAfter.Milliseconds()})
	}
	return ae
}

// soloSend is pipe_send of the Solo broker: one write frame (policy, request
// id, cache, audit) around the broker's limits and store and the delivery.
func (s *PipeService) soloSend(ctx context.Context, rq Request) (any, error) {
	e := s.ex
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.PipeSendArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	peer := soloPeer(args.Target.SessionName)
	return e.write(ctx, op, args.WriteArgs, args.Target, true, func(ctx context.Context) (ops.WriteResult, error) {
		msg, err := s.broker.Send(ctx, peer, args.Text)
		if err != nil {
			return ops.WriteResult{}, pipeAPIError("store the message", err)
		}
		res, err := e.pipeRun(ctx, rq, op, args, routeSolo)
		if err != nil || res.Outcome == ops.OutcomeFailedNotSent {
			// Nothing reached the session: the message neither shows in the
			// pipe nor uses budget. Any other outcome (unknown included) keeps
			// it, because the text may have gone out.
			s.retract(ctx, msg)
		}
		return res, err
	})
}

func (s *PipeService) retract(ctx context.Context, m pipe.Message) {
	if err := s.store.DeleteMessage(context.WithoutCancel(ctx), m.ID); err != nil {
		s.log.Warn("cannot retract an undelivered pipe message", "session", m.Peer.SessionName, "error", err.Error())
	}
}

// soloRead is pipe_read of the Solo broker: the messages sent and the replies
// received, in commit order, after the cursor.
func (s *PipeService) soloRead(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.PipeReadArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := s.ex.requireShared(ctx, args.Target.SessionName); err != nil {
		return nil, err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = DefaultPipeReadLimit
	}
	entries, err := s.store.Entries(ctx, soloPeer(args.Target.SessionName), args.AfterTurnID, limit)
	if err != nil {
		return nil, pipeAPIError("read the pipe", err)
	}
	all := make([]ops.PipeTurn, len(entries))
	for i, e := range entries {
		all[i] = pipeTurnOf(e)
	}
	return ops.PipeReadResult{Turns: cutPipePage(all)}, nil
}

func pipeTurnOf(e store.PipeEntry) ops.PipeTurn {
	return ops.PipeTurn{TurnID: e.EntryID, Session: e.Peer.SessionName, Direction: e.Kind, Text: e.Text, At: ops.Timestamp(e.At)}
}

// cutPipePage keeps the entries that fit into PipePageBytes once encoded, at
// least one. The caller continues with the turn_id of the last entry.
func cutPipePage(all []ops.PipeTurn) []ops.PipeTurn {
	size := 0
	for i, e := range all {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		size += len(b) + 1
		if i > 0 && size > PipePageBytes {
			return all[:i]
		}
	}
	return all
}

// soloWait is pipe_wait of the Solo broker: it streams the turns committed
// after the cursor (after the latest turn when there is none) as soon as there
// are some, and ends without one when the timeout passes.
func (s *PipeService) soloWait(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.PipeWaitArgs](rq)
	if err != nil {
		return nil, err
	}
	session := args.Target.SessionName
	peer := soloPeer(session)
	if err := s.ex.requireShared(ctx, session); err != nil {
		return nil, err
	}
	cursor := args.AfterTurnID
	if cursor == "" {
		if cursor, err = s.store.LatestTurnID(ctx, peer); err != nil {
			return nil, pipeAPIError("read the pipe", err)
		}
	}
	wait := DefaultPipeWait
	if args.TimeoutMS > 0 {
		wait = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	wake, unsubscribe := s.subscribe(peer)
	defer unsubscribe()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(s.cfg.WaitPoll)
	defer tick.Stop()

	for first := true; ; first = false {
		if !first {
			// A session that stopped being shared ends the wait.
			if err := s.ex.requireShared(ctx, session); err != nil {
				return nil, err
			}
		}
		turns, err := s.store.Turns(ctx, peer, cursor, DefaultPipeReadLimit)
		if err != nil {
			return nil, pipeAPIError("read the pipe", err)
		}
		if len(turns) > 0 {
			for _, t := range turns {
				pt := ops.PipeTurn{TurnID: t.TurnID, Session: session, Direction: ops.PipeReceived, Text: t.Text, At: ops.Timestamp(t.At)}
				if err := EmitProgress(ctx, pt); err != nil {
					return nil, internalErr("encode a turn", err)
				}
			}
			return struct{}{}, nil
		}
		select {
		case <-wake:
		case <-tick.C:
		case <-deadline.C:
			return struct{}{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// subscribe returns a channel that receives a signal when a turn of peer is
// committed here, and the function that ends the subscription.
func (s *PipeService) subscribe(peer pipe.Peer) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs[peer] == nil {
		s.subs[peer] = map[chan struct{}]struct{}{}
	}
	s.subs[peer][ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs[peer], ch)
		if len(s.subs[peer]) == 0 {
			delete(s.subs, peer)
		}
		s.mu.Unlock()
	}
}

func (s *PipeService) notify(peer pipe.Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs[peer] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// forward is the Hub mode of the three ops: the request goes to the Hub as it
// is. The caller's actions are checked here against the catalog like for any
// local call; the Hub decides what the Worker identity may do. Arguments are
// not decoded: the target may be written in the human form that the Hub
// resolves, and a request_id of the caller stays the one the Hub records.
func (s *PipeService) forward(ctx context.Context, rq Request) (json.RawMessage, error) {
	op, err := lookupOp(rq.Op)
	if err != nil {
		return nil, err
	}
	if err := authorize(op.Actions, rq.Actions); err != nil {
		return nil, err
	}
	cc := s.cfg.Hub.Calls()
	if cc == nil {
		return nil, apiErr(proto.CodeWorkerOffline, "", "this Worker is not connected to a Hub")
	}
	var args any
	if len(rq.Args) > 0 {
		args = rq.Args
	}
	if rq.Op == ops.OpPipeWait {
		err := cc.Stream(ctx, rq.Op, args, func(el json.RawMessage) error { return EmitProgress(ctx, el) })
		if err != nil {
			return nil, forwardError(err)
		}
		return json.RawMessage(`{}`), nil
	}
	var raw json.RawMessage
	if err := cc.Do(ctx, rq.Op, args, &raw); err != nil {
		var lost *CallLostError
		if rq.Op == ops.OpPipeSend && errors.As(err, &lost) {
			// The Hub may have stored and routed the message: say so, and do
			// not repeat it (AD-15). A repeat with the same request_id is the
			// Hub's to answer.
			b, merr := json.Marshal(ops.WriteResult{
				Outcome: ops.OutcomeUnknown, Reason: proto.CodeWorkerOffline.String(),
				Warnings: []ops.Warning{{Code: "hub_call_lost", Message: "the connection to the Hub ended before it answered; the message may have been sent and is not sent again"}},
			})
			if merr != nil {
				return nil, internalErr("encode result", merr)
			}
			return b, nil
		}
		return nil, forwardError(err)
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`null`)
	}
	return raw, nil
}

// forwardError passes the error of the Hub on to the local caller: a
// *proto.Error keeps its code and message, anything else is a plain failure.
func forwardError(err error) error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return proto.NewError(proto.CodeInternal, "the call to the Hub failed")
}
