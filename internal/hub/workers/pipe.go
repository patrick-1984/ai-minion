package workers

// Story 7.4: the Hub pipe (AD-23, AD-17, AD-22, R26-R29).
//
// In Hub mode the Hub is the pipe broker. [Pipe] wraps the one broker of
// internal/pipe (limits, task budget, idempotent turns) over the SQLite store
// and adds what only the Hub knows:
//
//   - pipe_send is checked with the op's accepted actions (message|operate)
//     on the session, passes the broker's limits (8 KiB, 1 per second with a
//     burst of 5 and 120 per hour per peer, the task budget) and is then
//     routed to the Worker as a write op through the [Router]. A message the
//     broker refuses is dropped, never queued. A message the Worker reports
//     as failed_not_sent is retracted again, so that something that never
//     reached the session neither shows in the pipe nor uses budget; any
//     other outcome, unknown included, stays stored and is recorded with it.
//     A request_id is one send for as long as the Hub keeps the message: a
//     retry is answered from the recorded outcome and is neither stored,
//     counted nor routed again (see [Pipe.Send]).
//   - Turns arrive from the Workers on the turn channel, an "ev" stream whose
//     first frame is turns.open (served through [Pipe.RegisterTurns] by the
//     event ingest, which tells an ev stream apart by its first frame). Each "turn" frame is
//     committed idempotently by turn_id and answered with turn.ack; a Worker
//     that never sees the ack sends the turn again, and the second commit
//     stores nothing and gets the same {status: "stored"}. The Hub stamps the
//     turn with its own clock (the "time" of the frame is not used): a Worker
//     whose clock is wrong cannot reorder the pipe or have its turns pruned.
//     A turn whose JSON-encoded text exceeds MaxTurnEncodedBytes is rejected,
//     so that every stored turn fits one stream frame (AD-3).
//   - pipe_read and pipe_wait withhold received turns from a model that has
//     not trusted the source (AD-17, AD-22, R29). A model is a caller that
//     authenticated with an API key or as a Worker identity; a person in the
//     browser (session, act-as) sees every turn they may read, trusted or not,
//     because trust never restricts a person. The receiver whose trust counts
//     is the key's owner, the acting admin for act-as, and the owner of the
//     Worker for a Worker identity. A pipe_read answers with the turns that
//     remain, the count of withheld turns and the reason "untrusted_peer"; a
//     pipe_wait on an untrusted source ends at once with the error
//     untrusted_peer, and one that loses trust while it waits ends the same
//     way instead of delivering.
//   - pipe_read answers entries in the order in which the Hub committed them
//     (one commit position shared by messages and turns, read as one snapshot,
//     so an entry committed after a reader's cursor is never skipped, in the
//     same millisecond too) and cuts a page to the entry limit and to ReadPageBytes of encoded entries, at least one
//     entry, so that the answer fits one stream frame. Every turn_id it
//     returns, the synthetic id of a sent message included, is a valid
//     after_turn_id for pipe_read and pipe_wait.
//
// Frames of the turn channel (JSON, framed as everywhere, AD-3):
//
//	{"type":"turns.open"}                                        // Worker, first frame
//	{"type":"turn","turn_id":"<uuidv7>","session":"minion-1","message_id":42,"text":"...","time":"<RFC 3339 ms>"}
//	{"type":"turn.ack","turn_id":"<uuidv7>","status":"stored|rejected|busy","reason":"...","retry_after_ms":1000}
//
// The Hub takes the Worker id from the authenticated connection, never from a
// frame, so a Worker can only write turns of its own sessions. A turn is the
// reply to a delivery: message_id is the id the Hub gave the message when it
// routed the pipe_send (the op's message_id), and the Hub stores a turn only
// for a message of that Worker and session that has no reply yet. Frame rate,
// byte and turn rates and the stored volume of the Worker, its owner and the Hub
// are bounded before anything is written (pipe_quota.go): "rejected" is final
// (the Worker drops the turn), "busy" is transient and stores nothing (the
// Worker sends the turn again after retry_after_ms), "stored" is only ever
// sent for a turn that is in the store.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Frame types of the turn channel; the names are declared in internal/proto.
const (
	// FrameTurnsOpen is the first frame of the turn channel.
	FrameTurnsOpen = proto.TypeTurnsOpen
	// FrameTurn carries one turn from a Worker.
	FrameTurn = proto.TypeTurn
)

// Limits of the Hub pipe service.
const (
	// MaxTurnBytes is the largest turn text the Hub stores. The Worker cuts
	// longer replies; a longer one that arrives anyway is rejected.
	MaxTurnBytes = 256 << 10
	// MaxTurnEncodedBytes is the largest size of a turn text once it is
	// JSON-encoded in a frame (< > & U+2028 and control characters take six
	// bytes). Leaving 16 KiB for the rest of the frame, a stored turn always
	// fits into the pipe_wait op.progress frame and into a page of pipe_read
	// (AD-3: a stream frame is at most proto.StreamFrameLimit).
	MaxTurnEncodedBytes = proto.StreamFrameLimit - 16<<10
	// ReadPageBytes is the budget of the entries of one pipe_read page, JSON
	// encoded. The page always has at least one entry; the caller continues
	// with after_turn_id.
	ReadPageBytes = proto.StreamFrameLimit - 16<<10
	// DefaultReadLimit is the number of entries a pipe_read returns when the
	// request gives no limit.
	DefaultReadLimit = 50
	// DefaultPipeWait is how long a pipe_wait waits when the request gives no
	// timeout.
	DefaultPipeWait = 30 * time.Second
	// DefaultPipePoll is the fallback poll of a waiting pipe_wait; a turn
	// committed on this Hub wakes it at once.
	DefaultPipePoll = time.Second
	// DefaultPruneInterval is how often [Pipe.RunPrune] deletes expired
	// messages and turns.
	DefaultPruneInterval = time.Hour

	ackWriteTimeout = 10 * time.Second
)

// ReasonUntrustedPeer is the reason of a withheld turn.
const ReasonUntrustedPeer = string(proto.CodeUntrustedPeer)

// PipeStore is the persistence the Hub pipe needs: the broker's port plus the
// reads by position, the request-id lookup and one delete. *store.PipeStore
// implements it.
type PipeStore interface {
	pipe.Store
	// TurnPos returns the position of a turn of peer or a *pipe.Error with
	// CodeNotFound.
	TurnPos(ctx context.Context, peer pipe.Peer, turnID string) (store.PipePos, error)
	// MessagePos returns the position of message id of peer when it exists
	// and was stored at atMS.
	MessagePos(ctx context.Context, peer pipe.Peer, id, atMS int64) (store.PipePos, bool, error)
	// LatestTurnPos returns the position of the last turn of peer, or the
	// zero position.
	LatestTurnPos(ctx context.Context, peer pipe.Peer) (store.PipePos, error)
	// EntriesAfterPos returns the merged entries after a position, oldest
	// first, at most limit, from one consistent snapshot; withTurns false
	// leaves the received turns out. TurnsAfterPos returns only turns.
	EntriesAfterPos(ctx context.Context, peer pipe.Peer, pos store.PipePos, limit int, withTurns bool) ([]store.PipeEntry, error)
	TurnsAfterPos(ctx context.Context, peer pipe.Peer, pos store.PipePos, limit int) ([]pipe.Turn, error)
	// CountTurnsAfterPos counts the turns after pos (and not after upTo).
	CountTurnsAfterPos(ctx context.Context, peer pipe.Peer, pos store.PipePos, upTo *store.PipePos) (int, error)
	// MessageByRequestID finds the message a pipe_send with requestID stored.
	MessageByRequestID(ctx context.Context, peer pipe.Peer, requestID string) (pipe.Message, bool, error)
	// SetMessageRequestID records the request_id of the pipe_send that
	// stored message id.
	SetMessageRequestID(ctx context.Context, id int64, requestID string) error
	// SetMessageResult and MessageResult keep the answer (a JSON WriteResult)
	// of the pipe_send that stored message id; MessageResult is nil while
	// there is none.
	SetMessageResult(ctx context.Context, id int64, result []byte) error
	MessageResult(ctx context.Context, id int64) ([]byte, error)
	// DeleteMessage deletes a message; a missing one is not an error.
	DeleteMessage(ctx context.Context, id int64) error
	// CheckTurn, PutDeliveredTurn and Pressure are the turn ingestion of a
	// Worker (migration 0016): the check that a turn answers a delivery of its
	// Worker, the insert that enforces the stored-volume quotas in its own
	// transaction, and the size of the database and its WAL.
	CheckTurn(ctx context.Context, peer pipe.Peer, turnID string, messageID int64) (store.TurnStatus, error)
	PutDeliveredTurn(ctx context.Context, in store.DeliveredTurn) (store.TurnStatus, error)
	Pressure(ctx context.Context, walLimit int64) (store.Pressure, error)
}

// PipeDecider is the one decision point; *authz.Authorizer implements it.
type PipeDecider interface {
	Decide(ctx context.Context, p authz.Principal, accepted ops.ActionSet, res authz.Resource) error
}

// PipeTrust is the trust lookup; *authz.Trust implements it.
type PipeTrust interface {
	Trusted(ctx context.Context, receiverID, workerID, sessionName string) (bool, error)
	Owner(ctx context.Context, workerID string) (string, error)
}

// PipeRouter routes the pipe_send write to the Worker; *Router implements it.
type PipeRouter interface {
	Call(ctx context.Context, req CallRequest) (any, error)
}

var (
	_ PipeDecider = (*authz.Authorizer)(nil)
	_ PipeTrust   = (*authz.Trust)(nil)
	_ PipeRouter  = (*Router)(nil)
)

// PipeConfig configures [NewPipe]. Store, Authz, Router and Trust are
// required.
type PipeConfig struct {
	Store  PipeStore
	Authz  PipeDecider
	Router PipeRouter
	Trust  PipeTrust
	// Catalog declares the ops; the default is ops.Default.
	Catalog *ops.Registry
	// BudgetPerDay is the task budget (setting pipe_budget_per_day); zero
	// selects the broker default.
	BudgetPerDay int
	// BudgetFunc is the live task budget: asked on every send, it replaces
	// BudgetPerDay ([pipe.Config.BudgetFunc]). The Hub passes the resolved
	// setting pipe_budget_per_day.
	BudgetFunc func() (int, error)
	// Now is the clock; the default is time.Now.
	Now func() time.Time
	// Poll is the fallback poll of pipe_wait; zero selects DefaultPipePoll.
	Poll time.Duration
	// Quotas bound what the Workers store through the turn channel; zero
	// members select the defaults (pipe_quota.go).
	Quotas TurnQuotas
	// Shared tells whether the Hub may still show what it stored of a session:
	// false for a session that the owner took away from the share (share_set,
	// [EventHub.Fenced]). It is asked on every read and on every wake-up of a
	// wait, so a history kept before the session was unshared is not served
	// afterwards. Nil shows everything.
	Shared func(workerID, session string) bool
	// Changed is told, with the Worker id and the session name only, after a
	// message or a reply of that session was durably committed (a message whose
	// delivery was refused before it reached the session was retracted and is
	// not announced; a replayed request_id or a duplicate turn is not announced
	// again). It is a wake-up and carries no text, no turn id and no policy: the
	// Hub passes the EventHub's pipe_changed publisher. It is called with no
	// lock of the Pipe held and must not block. Nil announces nothing.
	Changed func(workerID, session string)

	// SharedNow is Shared for a caller that can wait for an answer: it is asked
	// on every read and on every wake-up of a wait, and before every entry that
	// leaves, and it may ask the connected Worker for its share rule on its
	// current connection ([EventHub.HistoryVisible]), so history that a
	// connected Worker's own policy has just stopped sharing is not served
	// before the Worker's next event. When set it replaces Shared.
	SharedNow func(ctx context.Context, workerID, session string) bool
	// Abort closes the connection of a Worker that keeps sending turns after
	// the Hub told it to wait (the registry's Disconnect). Nil only resets the
	// stream.
	Abort func(workerID string)
	// Logger receives operational messages, never message or turn text. The
	// default is slog.Default().
	Logger *slog.Logger
}

// Pipe is the Hub's pipe service. It is safe for concurrent use.
type Pipe struct {
	broker *pipe.Broker
	store  PipeStore
	az     PipeDecider
	rt     PipeRouter
	trust  PipeTrust
	now    func() time.Time
	poll   time.Duration
	log    *slog.Logger

	sendActions ops.ActionSet
	readActions ops.ActionSet

	sendMu sync.Mutex // serialises the request_id lookup and the broker send

	quotas  TurnQuotas
	limiter *turnLimiter
	abort   func(workerID string)
	changed func(workerID, session string)
	shared  func(workerID, session string) bool
	// sharedNow is [PipeConfig.SharedNow].
	sharedNow func(ctx context.Context, workerID, session string) bool
	// pressureMu guards the cached reading of the database size.
	pressureMu   sync.Mutex
	pressureAt   time.Time
	pressureBusy bool

	// reqs holds the request_ids with an attempt in progress: the channel is
	// closed when the attempt ends ([Pipe.lockRequest]).
	reqMu sync.Mutex
	reqs  map[string]chan struct{}

	mu   sync.Mutex
	subs map[pipe.Peer]map[chan struct{}]struct{}
}

// NewPipe builds the Hub pipe service.
func NewPipe(c PipeConfig) (*Pipe, error) {
	if c.Store == nil || c.Authz == nil || c.Router == nil || c.Trust == nil {
		return nil, errors.New("workers: a Pipe needs a Store, an Authz, a Router and a Trust")
	}
	cat := c.Catalog
	if cat == nil {
		cat = ops.Default
	}
	send, ok1 := cat.Lookup(ops.OpPipeSend)
	read, ok2 := cat.Lookup(ops.OpPipeRead)
	if !ok1 || !ok2 {
		return nil, errors.New("workers: the catalog has no pipe ops")
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	broker, err := pipe.NewBroker(pipe.Config{Store: c.Store, Now: now, BudgetPerDay: c.BudgetPerDay, BudgetFunc: c.BudgetFunc})
	if err != nil {
		return nil, err
	}
	p := &Pipe{
		broker: broker, store: c.Store, az: c.Authz, rt: c.Router, trust: c.Trust,
		now: now, poll: c.Poll, log: c.Logger,
		sendActions: send.Actions, readActions: read.Actions,
		subs:   map[pipe.Peer]map[chan struct{}]struct{}{},
		reqs:   map[string]chan struct{}{},
		quotas: c.Quotas.withDefaults(), abort: c.Abort, shared: c.Shared, sharedNow: c.SharedNow, changed: c.Changed,
	}
	p.limiter = newTurnLimiter(p.quotas, now)
	if p.poll <= 0 {
		p.poll = DefaultPipePoll
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p, nil
}

// Broker returns the broker, for settings and tests.
func (p *Pipe) Broker() *pipe.Broker { return p.broker }

// errNotShared is the answer for a session that is no longer shared. It names
// nothing of the history.
func notShared() error {
	e := proto.NewError(proto.CodeForbidden, "the session is not shared")
	e.Details, _ = json.Marshal(map[string]string{"reason": proto.CodeNotShared.String()})
	return e
}

// visible reports whether the history of peer may be shown.
func (p *Pipe) visible(peer pipe.Peer) bool {
	return p.shared == nil || p.shared(peer.WorkerID, peer.SessionName)
}

// visibleNow reports whether the history of peer may be shown now: it asks
// the current authorization of the Worker (see [PipeConfig.SharedNow]) when the
// Pipe has one, and the cached rule otherwise.
func (p *Pipe) visibleNow(ctx context.Context, peer pipe.Peer) bool {
	if p.sharedNow != nil {
		return p.sharedNow(ctx, peer.WorkerID, peer.SessionName)
	}
	return p.visible(peer)
}

// pipeError turns a broker error into the wire error with the same code. Any
// other error is returned as is.
func pipeError(err error) error {
	var pe *pipe.Error
	if !errors.As(err, &pe) {
		return err
	}
	e := proto.NewError(proto.Code(pe.Code), pe.Message)
	if pe.Code == pipe.CodePipeRateLimited && pe.RetryAfter > 0 {
		e.Details, _ = json.Marshal(map[string]int64{"retry_after_ms": pe.RetryAfter.Milliseconds()})
	}
	return e
}

func invalidArgument(msg string) error { return proto.NewError(proto.CodeInvalidArgument, msg) }

// peerOf validates a target and returns the peer and its authz resource.
func peerOf(t ops.Target) (pipe.Peer, authz.SessionResource, error) {
	if t.WorkerID == "" {
		return pipe.Peer{}, authz.SessionResource{}, invalidArgument("target.worker_id is required")
	}
	peer := pipe.Peer{WorkerID: t.WorkerID, SessionName: t.SessionName}
	if !peer.Valid() {
		return pipe.Peer{}, authz.SessionResource{}, invalidArgument("target needs worker_id and session_name")
	}
	return peer, authz.SessionResource{WorkerID: t.WorkerID, Name: t.SessionName}, nil
}

// RerouteWindow is how long after it was stored a pipe_send whose outcome was
// never recorded may be routed to the Worker again. The Worker answers a
// repeated request_id from its result cache for ten minutes
// (internal/worker/store/results.go); a repeat inside this window, shorter
// than the cache, is therefore the same write. After it a second routing could
// write the text a second time, so the Hub refuses it with outcome unknown.
const RerouteWindow = 8 * time.Minute

// Send runs pipe_send for a principal: decide, broker limits, route to the
// Worker. See the file comment.
//
// A request_id is one send for as long as the Hub keeps the message (7 days),
// however often the caller asks again:
//
//   - the attempts of one request_id run one at a time (a later one waits for
//     the earlier one to end), so that one attempt cannot retract the message
//     another attempt delivered;
//   - the first attempt stores the message and routes it. The answer of the
//     Worker is kept with the message, and every later attempt gets that
//     answer back: nothing is stored, counted or routed again, however long
//     ago the Worker's own cache expired;
//   - a message without a recorded answer (the Hub stopped while the Worker
//     was writing, or the answer could not be stored) is routed again only
//     inside [RerouteWindow], where the Worker's cache makes it the same
//     write; later the Hub answers with outcome unknown;
//   - a send that failed before it reached the Worker, or that the Worker
//     reports as failed_not_sent, leaves nothing behind: the next attempt is a
//     new send with a new charge.
func (p *Pipe) Send(ctx context.Context, pr authz.Principal, args ops.PipeSendArgs) (ops.WriteResult, error) {
	peer, res, err := peerOf(args.Target)
	if err != nil {
		return ops.WriteResult{}, err
	}
	// Authorise before the broker, so that a caller who may not message the
	// session cannot use its rate limit or budget.
	if err := p.az.Decide(ctx, pr, p.sendActions, res); err != nil {
		return ops.WriteResult{}, err
	}
	if args.RequestID != "" {
		unlock, err := p.lockRequest(ctx, peer, args.RequestID)
		if err != nil {
			return ops.WriteResult{}, err
		}
		defer unlock()
	}
	msg, prior, created, err := p.storeMessage(ctx, peer, args)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if prior != nil {
		return *prior, nil
	}
	// The Worker names this id on the turn that answers the delivery. It is the
	// Hub's, never the caller's.
	args.MessageID = msg.ID
	out, err := p.rt.Call(ctx, CallRequest{
		Principal: pr, Op: ops.OpPipeSend, WorkerID: peer.WorkerID, Session: peer.SessionName, Args: args,
	})
	if err != nil {
		if created { // nothing reached the session as far as the router knows
			p.retract(ctx, msg)
		}
		return ops.WriteResult{}, err
	}
	wr, ok := out.(ops.WriteResult)
	if !ok {
		return ops.WriteResult{}, proto.NewError(proto.CodeInternal, "the Worker answered pipe_send with an unexpected result")
	}
	if wr.Outcome == ops.OutcomeFailedNotSent {
		p.retract(ctx, msg)
		return wr, nil
	}
	if created { // the message is retained: other readers of the session learn of it
		p.announce(peer)
	}
	if args.RequestID != "" {
		if b, err := json.Marshal(wr); err == nil {
			err = p.store.SetMessageResult(context.WithoutCancel(ctx), msg.ID, b)
			if err != nil {
				p.log.Warn("pipe: cannot record the answer of a send", "worker_id", peer.WorkerID, "error", err.Error())
			}
		}
	}
	return wr, nil
}

// lockRequest serialises the attempts of one request_id of a peer; the
// returned function ends the attempt.
func (p *Pipe) lockRequest(ctx context.Context, peer pipe.Peer, rid string) (func(), error) {
	key := peer.WorkerID + "\x00" + peer.SessionName + "\x00" + rid
	for {
		p.reqMu.Lock()
		held, busy := p.reqs[key]
		if !busy {
			ch := make(chan struct{})
			p.reqs[key] = ch
			p.reqMu.Unlock()
			return func() {
				p.reqMu.Lock()
				delete(p.reqs, key)
				p.reqMu.Unlock()
				close(ch)
			}, nil
		}
		p.reqMu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// storeMessage stores the message of a send through the broker, once per
// request_id and peer. A request_id the Hub knows yields the stored message
// and created false; prior is then the recorded answer, or the refusal of a
// message whose answer is unknown and too old to route again. A nil prior with
// created false means the message is routed again (inside [RerouteWindow]).
func (p *Pipe) storeMessage(ctx context.Context, peer pipe.Peer, args ops.PipeSendArgs) (msg pipe.Message, prior *ops.WriteResult, created bool, err error) {
	rid := args.RequestID
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if rid != "" {
		m, ok, err := p.store.MessageByRequestID(ctx, peer, rid)
		if err != nil {
			return pipe.Message{}, nil, false, err
		}
		if ok {
			raw, err := p.store.MessageResult(ctx, m.ID)
			if err != nil {
				return pipe.Message{}, nil, false, err
			}
			if len(raw) > 0 {
				var wr ops.WriteResult
				if json.Unmarshal(raw, &wr) == nil {
					return m, &wr, false, nil
				}
			}
			if p.now().Sub(m.At) >= RerouteWindow {
				wr := ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: string(proto.CodeInternal),
					Warnings: []ops.Warning{{Code: "send_outcome_unknown", Message: "this request_id was sent before and its outcome was not recorded; the Hub does not send it again"}}}
				return m, &wr, false, nil
			}
			return m, nil, false, nil
		}
	}
	m, err := p.broker.Send(ctx, peer, args.Text)
	if err != nil {
		return pipe.Message{}, nil, false, pipeError(err)
	}
	if rid != "" {
		if err := p.store.SetMessageRequestID(ctx, m.ID, rid); err != nil {
			p.retract(ctx, m)
			return pipe.Message{}, nil, false, err
		}
	}
	return m, nil, true, nil
}

// retract removes a message that was refused before it reached the session.
func (p *Pipe) retract(ctx context.Context, m pipe.Message) {
	if err := p.store.DeleteMessage(context.WithoutCancel(ctx), m.ID); err != nil {
		p.log.Warn("pipe: cannot retract an undelivered message", "worker_id", m.Peer.WorkerID, "error", err.Error())
	}
}

// PipeReadView is the result of pipe_read: the turns, and what was withheld
// from a model (R29). It is the shared result type of the catalog, so that a
// client decodes the same members the Hub sends.
type PipeReadView = ops.PipeReadResult

// readsAsModel reports whether a caller is treated as a model: one that came
// with an API key or as a Worker identity. A person in the browser is not.
func readsAsModel(pr authz.Principal) bool {
	return pr.Kind == authz.KindAPIKey || pr.Kind == authz.KindWorker
}

// trusted reports whether the principal may read the turns of the source as
// trusted. A lookup failure counts as not trusted (fail closed).
func (p *Pipe) trusted(ctx context.Context, pr authz.Principal, peer pipe.Peer) bool {
	receiver := authz.TrustReceiver(pr)
	if pr.Kind == authz.KindWorker {
		owner, err := p.trust.Owner(ctx, pr.WorkerID)
		if err != nil {
			return false
		}
		receiver = owner
	}
	ok, err := p.trust.Trusted(ctx, receiver, peer.WorkerID, peer.SessionName)
	if err != nil {
		p.log.Warn("pipe: trust lookup failed; treating the source as not trusted", "error", err.Error())
		return false
	}
	return ok
}

// sentID returns a stable UUIDv7 for a sent message, derived from its time
// and database id, so that every entry of a pipe_read has a turn_id.
func sentID(m pipe.Message) string {
	var r [10]byte
	r[0], r[1] = 'm', 's'
	binary.BigEndian.PutUint64(r[2:], uint64(m.ID))
	id, err := pipe.NewTurnIDFrom(m.At, bytes.NewReader(r[:]))
	if err != nil {
		return ""
	}
	return id
}

// decodeSentID reverses [sentID]: it returns the message id and the time in
// unix milliseconds that a synthetic turn_id of a sent message carries. ok is
// false for any other id. The caller must still check that the message
// exists for the peer.
func decodeSentID(s string) (id, atMS int64, ok bool) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return 0, 0, false
	}
	var b [16]byte
	if n, err := hex.Decode(b[:], []byte(s[0:8]+s[9:13]+s[14:18]+s[19:23]+s[24:])); err != nil || n != 16 {
		return 0, 0, false
	}
	if b[6]&0x0f != 'm'&0x0f || b[7] != 's' {
		return 0, 0, false
	}
	ms := uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 | uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
	raw := binary.BigEndian.Uint64(b[8:])
	raw &^= 0xc000000000000000 // the UUID variant bits replaced the top two bits of the id
	if raw == 0 || raw > 1<<62 || ms > 1<<47 {
		return 0, 0, false
	}
	return int64(raw), int64(ms), true
}

func receivedEntry(t pipe.Turn) ops.PipeTurn {
	return ops.PipeTurn{
		TurnID: t.TurnID, Session: t.Peer.SessionName, Direction: ops.PipeReceived,
		Text: t.Text, At: ops.Timestamp(t.At),
	}
}

func sentEntry(m pipe.Message) ops.PipeTurn {
	return ops.PipeTurn{
		TurnID: sentID(m), Session: m.Peer.SessionName, Direction: ops.PipeSent,
		Text: m.Text, At: ops.Timestamp(m.At),
	}
}

// position resolves a cursor (the after_turn_id of pipe_read and pipe_wait)
// to a position. Every turn_id that pipe_read returns is a valid cursor: the
// id of a received turn, and the synthetic id of a sent message (see
// [sentID]), which is checked against the stored message of this peer. Empty
// is the start of the pipe.
func (p *Pipe) position(ctx context.Context, peer pipe.Peer, id string) (store.PipePos, error) {
	if id == "" {
		return store.PipePos{}, nil
	}
	if !pipe.IsTurnID(id) {
		return store.PipePos{}, invalidArgument("after_turn_id is not a UUIDv7")
	}
	pos, err := p.store.TurnPos(ctx, peer, id)
	if err == nil {
		return pos, nil
	}
	if !errors.Is(err, pipe.ErrNotFound) {
		return store.PipePos{}, err
	}
	if mid, atMS, ok := decodeSentID(id); ok {
		pos, found, err := p.store.MessagePos(ctx, peer, mid, atMS)
		if err != nil {
			return store.PipePos{}, err
		}
		if found {
			return pos, nil
		}
	}
	return store.PipePos{}, pipeError(err)
}

// Read runs pipe_read: the entries after AfterTurnID, that is the received
// turns and the messages sent, in commit order, cut to the limit and
// to ReadPageBytes of encoded entries (at least one entry); the caller pages
// on with the turn_id of the last entry. A model gets no received turn of a
// source its user has not trusted.
func (p *Pipe) Read(ctx context.Context, pr authz.Principal, args ops.PipeReadArgs) (PipeReadView, error) {
	peer, res, err := peerOf(args.Target)
	if err != nil {
		return PipeReadView{}, err
	}
	if err := p.az.Decide(ctx, pr, p.readActions, res); err != nil {
		return PipeReadView{}, err
	}
	if !p.visibleNow(ctx, peer) {
		return PipeReadView{}, notShared()
	}
	limit := args.Limit
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	after, err := p.position(ctx, peer, args.AfterTurnID)
	if err != nil {
		return PipeReadView{}, err
	}
	withhold := readsAsModel(pr) && !p.trusted(ctx, pr, peer)
	entries, err := p.store.EntriesAfterPos(ctx, peer, after, limit, !withhold)
	if err != nil {
		return PipeReadView{}, err
	}
	all := make([]ops.PipeTurn, 0, len(entries))
	for _, e := range entries {
		if e.Message != nil {
			all = append(all, sentEntry(*e.Message))
		} else {
			all = append(all, receivedEntry(*e.Turn))
		}
	}
	if !p.visibleNow(ctx, peer) { // unshared while the page was read
		return PipeReadView{}, notShared()
	}
	kept, cut := cutPage(all)

	view := PipeReadView{Turns: append(make([]ops.PipeTurn, 0, len(kept)), kept...)}
	if withhold {
		// Count the received turns the page skipped: those up to its last
		// entry, or all that follow when the page reached the end.
		var upTo *store.PipePos
		if (cut || len(entries) == limit) && len(kept) > 0 {
			u := entries[len(kept)-1].Pos
			upTo = &u
		}
		n, err := p.store.CountTurnsAfterPos(ctx, peer, after, upTo)
		if err != nil {
			return PipeReadView{}, err
		}
		if n > 0 {
			view.Withheld = n
			view.Reason = ReasonUntrustedPeer
			view.Notice = fmt.Sprintf("%d pipe turn(s) of session %q were withheld because the source is not trusted. A person can read them in the web UI.",
				n, peer.SessionName)
		}
	}
	return view, nil
}

// cutPage keeps the entries that fit into ReadPageBytes once encoded, at
// least one, and reports whether it dropped any.
func cutPage(all []ops.PipeTurn) (kept []ops.PipeTurn, cut bool) {
	size := 0
	for i, e := range all {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		size += len(b) + 1
		if i > 0 && size > ReadPageBytes {
			return all[:i], true
		}
	}
	return all, false
}

// subscribe returns a channel that receives a signal when a turn of peer is
// committed on this Hub, and the function that ends the subscription.
func (p *Pipe) subscribe(peer pipe.Peer) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	if p.subs[peer] == nil {
		p.subs[peer] = map[chan struct{}]struct{}{}
	}
	p.subs[peer][ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs[peer], ch)
		if len(p.subs[peer]) == 0 {
			delete(p.subs, peer)
		}
		p.mu.Unlock()
	}
}

func (p *Pipe) notify(peer pipe.Peer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs[peer] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// announce tells the Changed hook that something committed for peer. The hook
// is outside every lock of the Pipe; it publishes metadata only.
func (p *Pipe) announce(peer pipe.Peer) {
	if p.changed != nil {
		p.changed(peer.WorkerID, peer.SessionName)
	}
}

// Wait runs pipe_wait: it calls emit for the turns committed after
// AfterTurnID (after the latest turn at the time of the call when it is
// empty) as soon as there are some, and returns. It returns without a turn
// when the timeout passes. The caller is re-authorised on every wake-up, and
// a model whose source is not trusted gets untrusted_peer instead of a turn.
func (p *Pipe) Wait(ctx context.Context, pr authz.Principal, args ops.PipeWaitArgs, emit func(ops.PipeTurn) error) error {
	peer, res, err := peerOf(args.Target)
	if err != nil {
		return err
	}
	if err := p.az.Decide(ctx, pr, p.readActions, res); err != nil {
		return err
	}
	if !p.visibleNow(ctx, peer) {
		return notShared()
	}
	model := readsAsModel(pr)
	if model && !p.trusted(ctx, pr, peer) {
		return proto.NewError(proto.CodeUntrustedPeer, "the source of this pipe is not trusted by the receiving user")
	}
	var cursor store.PipePos
	if args.AfterTurnID == "" {
		if cursor, err = p.store.LatestTurnPos(ctx, peer); err != nil {
			return err
		}
	} else if cursor, err = p.position(ctx, peer, args.AfterTurnID); err != nil {
		return err
	}
	wait := DefaultPipeWait
	if args.TimeoutMS > 0 {
		wait = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	wake, unsubscribe := p.subscribe(peer)
	defer unsubscribe()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(p.poll)
	defer tick.Stop()

	for first := true; ; first = false {
		if !first {
			if err := p.az.Decide(ctx, pr, p.readActions, res); err != nil {
				return err
			}
			if !p.visibleNow(ctx, peer) {
				return notShared()
			}
		}
		turns, err := p.store.TurnsAfterPos(ctx, peer, cursor, DefaultReadLimit)
		if err != nil {
			return err
		}
		if len(turns) > 0 {
			if !p.visibleNow(ctx, peer) { // unshared while the turns were read
				return notShared()
			}
			if model && !p.trusted(ctx, pr, peer) {
				return proto.NewError(proto.CodeUntrustedPeer, "the source of this pipe is not trusted by the receiving user")
			}
			for _, t := range turns {
				// The share rule is asked before every turn that leaves, not
				// once for the batch: the emitter of a turn can block (a slow
				// reader) while share_set completes, and the turns that were
				// already fetched are not delivered after that.
				if !p.visibleNow(ctx, peer) {
					return notShared()
				}
				if err := emit(receivedEntry(t)); err != nil {
					return err
				}
			}
			return nil
		}
		select {
		case <-wake:
		case <-tick.C:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// encodedLen is the size of text as a JSON string value.
func encodedLen(text string) int {
	b, err := json.Marshal(text)
	if err != nil {
		return len(text) * 6
	}
	return len(b) - 2
}

// TurnFrame is the "turn" frame a Worker sends on the turn channel.
type TurnFrame struct {
	Type    string `json:"type"`
	TurnID  string `json:"turn_id"`
	Session string `json:"session"`
	// MessageID is the message_id of the pipe_send the turn answers: the id
	// the Hub gave that delivery. A turn without one, or with one that is not
	// a message of this Worker and session without a reply yet, is rejected.
	MessageID int64         `json:"message_id"`
	Text      string        `json:"text"`
	Time      ops.Timestamp `json:"time"`
}

// TurnAckFrame is the "turn.ack" frame the Hub answers with.
type TurnAckFrame struct {
	Type   string         `json:"type"`
	TurnID string         `json:"turn_id"`
	Status pipe.AckStatus `json:"status"`
	Reason string         `json:"reason,omitempty"`
	// RetryAfterMS is, with status busy, the earliest time to send the turn
	// again.
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
}

// IngestTurn commits a turn of the authenticated Worker idempotently and
// returns the ack to send. The turn must answer a delivery of this Worker, and
// it must fit the quotas of the turn channel (pipe_quota.go); both are decided
// before anything is written.
//
//   - rejected is final (the Worker drops the turn): the frame is malformed or
//     too large, names no delivery of this Worker and session, answers a
//     delivery that has its reply already, or reuses another turn's id;
//   - busy is not final and stores nothing: a rate or storage limit, or
//     pressure on the database, with retry_after_ms; the Worker keeps the turn;
//   - stored is sent for a turn that is in the store, the first time and every
//     time it is sent again, at no cost to the quotas;
//   - an error (the store failed) sends no ack: the Worker sends the turn again.
func (p *Pipe) IngestTurn(ctx context.Context, workerID string, f TurnFrame) (pipe.Ack, error) {
	return p.ingestTurn(ctx, workerID, f, len(f.Text)+len(f.TurnID)+len(f.Session)+128)
}

// ingestTurn is IngestTurn for a frame of size bytes on the wire, which is
// charged to the frame budget of the Worker first.
func (p *Pipe) ingestTurn(ctx context.Context, workerID string, f TurnFrame, size int) (pipe.Ack, error) {
	// The ack echoes the turn_id; a frame can carry any string there.
	id := f.TurnID
	if len(id) > 64 {
		id = id[:64]
	}
	reject := func(reason string) (pipe.Ack, error) {
		return pipe.Ack{TurnID: id, Status: pipe.AckRejected, Reason: reason}, nil
	}
	busy := func(wait time.Duration, reason string) (pipe.Ack, error) {
		return pipe.Ack{TurnID: id, Status: pipe.AckBusy, Reason: reason, RetryAfterMS: retryAfter(wait)}, nil
	}
	// The cheapest check first: a Worker that floods the channel pays here, and
	// only here, for frames that go no further.
	if wait, ok := p.limiter.frame(workerID, size); !ok {
		return busy(wait, "too many turn frames; wait")
	}
	if err := (ops.Target{SessionName: f.Session}).Validate(); err != nil {
		return reject("invalid session name")
	}
	switch {
	case f.Text == "":
		return reject("the turn is empty")
	case len(f.Text) > MaxTurnBytes:
		return reject("the turn is too large")
	case !utf8.ValidString(f.Text):
		return reject("the turn is not valid UTF-8")
	case encodedLen(f.Text) > MaxTurnEncodedBytes:
		return reject("the turn is too large once encoded")
	case !pipe.IsTurnID(f.TurnID):
		return reject("turn_id is not a UUIDv7")
	case f.MessageID <= 0:
		return reject("the turn does not name the delivery it answers")
	}
	peer := pipe.Peer{WorkerID: workerID, SessionName: f.Session}
	st, err := p.store.CheckTurn(ctx, peer, f.TurnID, f.MessageID)
	if err != nil {
		return pipe.Ack{}, err
	}
	switch st {
	case store.TurnDuplicate:
		return pipe.Ack{TurnID: f.TurnID, Status: pipe.AckStored}, nil // the lost ack, again
	case store.TurnIDTaken:
		return reject("turn_id is already used by another turn")
	case store.TurnNoDelivery:
		return reject("the turn does not answer a message delivered to this session of this Worker")
	case store.TurnAnswered:
		return reject("the delivery has its reply already")
	}
	// The turn would be stored: now it is charged. The owner is the one of the
	// Worker the connection authenticated.
	owner, err := p.trust.Owner(ctx, workerID)
	if err != nil {
		return pipe.Ack{}, err
	}
	if p.underPressure(ctx) {
		return busy(quotaRetryAfter, "the Hub database is under pressure")
	}
	cost := int(store.TurnCost(len(f.Text)))
	refund, wait, ok := p.limiter.admit(workerID, owner, cost)
	if !ok {
		return busy(wait, "turn rate limit; wait")
	}
	// The Hub stamps the turn with its own clock, not the Worker's: a Worker
	// whose clock is behind (a Raspberry Pi before NTP sync) would otherwise
	// sort its replies before the messages they answer, break paging by time
	// and have its turns pruned at once.
	res, err := p.store.PutDeliveredTurn(ctx, store.DeliveredTurn{
		Turn:      pipe.Turn{TurnID: f.TurnID, Peer: peer, Text: f.Text, At: p.now()},
		MessageID: f.MessageID, OwnerID: owner, Limits: p.quotas.Stored,
	})
	if err != nil {
		refund()
		return pipe.Ack{}, err
	}
	switch res {
	case store.TurnStored:
		p.notify(peer)
		p.announce(peer)
		return pipe.Ack{TurnID: f.TurnID, Status: pipe.AckStored}, nil
	case store.TurnDuplicate:
		refund()
		return pipe.Ack{TurnID: f.TurnID, Status: pipe.AckStored}, nil
	case store.TurnQuotaWorker, store.TurnQuotaOwner, store.TurnQuotaGlobal:
		refund()
		return busy(quotaRetryAfter, "the stored volume of turns is at its limit")
	case store.TurnIDTaken:
		refund()
		return reject("turn_id is already used by another turn")
	case store.TurnAnswered:
		refund()
		return reject("the delivery has its reply already")
	}
	refund()
	return reject("the turn does not answer a message delivered to this session of this Worker")
}

// underPressure reports whether the database or its WAL is over its limit. The
// reading is kept for a second: a busy channel does not stat the WAL per frame.
// A reading that fails counts as not under pressure (the insert would fail too
// if the database were gone).
func (p *Pipe) underPressure(ctx context.Context) bool {
	p.pressureMu.Lock()
	defer p.pressureMu.Unlock()
	if now := p.now(); now.Sub(p.pressureAt) < pressureTTL && !p.pressureAt.IsZero() {
		return p.pressureBusy
	}
	pr, err := p.store.Pressure(ctx, p.quotas.MaxWALBytes)
	if err != nil {
		p.log.Warn("pipe: cannot read the database size", "error", err.Error())
		return false
	}
	p.pressureAt = p.now()
	p.pressureBusy = pr.DBBytes > p.quotas.MaxDBBytes || pr.WALBytes > p.quotas.MaxWALBytes
	return p.pressureBusy
}

// ServeTurns serves the turn channel of one Worker after its first frame
// (turns.open) was read: it commits every turn frame and answers it. It
// returns when the stream ends. A Worker that keeps sending before the time a
// busy answer told it to wait (an early frame, see pipe_quota.go) loses the
// stream after [maxBusyStrikes] of them, and its connection when that happens
// again and again. The hold belongs to the Worker, not to the stream.
func (p *Pipe) ServeTurns(ctx context.Context, workerID string, st FrameStream) {
	// A read that waits for the next turn must end with ctx (a Reset would not
	// wake it: see interruptRead), and the stream is then abandoned.
	defer interruptOnDone(ctx, st)()
	defer func() {
		if ctx.Err() != nil {
			st.Reset()
			return
		}
		_ = st.Close()
	}()
	junk := 0 // frames in a row that were no turn
	for ctx.Err() == nil {
		body, err := st.ReadFrame()
		if err != nil {
			return
		}
		early := p.limiter.arrive(workerID)
		var f TurnFrame
		if typ, err := proto.FrameType(body); err != nil || typ != FrameTurn || json.Unmarshal(body, &f) != nil {
			// Not a turn that can be read: an unknown type (additive protocol,
			// AD-4) or a frame of the wrong shape. It is ignored, but it is not
			// free: it is charged to the frame budget and the frame bytes of the
			// Worker like every frame, and a run of them ends the stream. No
			// ack can be sent (there is no turn_id to trust), and nothing of it
			// is logged.
			if wait, ok := p.limiter.frame(workerID, len(body)); !ok {
				n := p.limiter.busy(workerID, early, wait)
				if early && n >= maxBusyStrikes {
					p.cutOff(workerID, st, "a Worker keeps sending frames after it was told to wait")
					return
				}
			}
			if junk++; junk >= maxJunkFrames {
				p.cutOff(workerID, st, "a Worker sends frames of the turn channel that are no turns")
				return
			}
			continue
		}
		junk = 0
		ack, err := p.ingestTurn(ctx, workerID, f, len(body))
		if err != nil {
			p.log.Warn("pipe: cannot store a turn; no ack is sent", "worker_id", workerID, "turn_id", f.TurnID, "error", err.Error())
			continue
		}
		if ack.Status == pipe.AckBusy {
			n := p.limiter.busy(workerID, early, time.Duration(ack.RetryAfterMS)*time.Millisecond)
			if early && n >= maxBusyStrikes {
				p.cutOff(workerID, st, "a Worker keeps sending turns before the time it was told to wait")
				return
			}
		}
		_ = st.SetWriteDeadline(time.Now().Add(ackWriteTimeout))
		if err := st.WriteFrame(TurnAckFrame{Type: proto.TypeTurnAck, TurnID: ack.TurnID, Status: ack.Status, Reason: ack.Reason, RetryAfterMS: ack.RetryAfterMS}); err != nil {
			st.Reset()
			return
		}
	}
}

// cutOff resets the turn stream of a Worker that abuses it and, when that
// happens again and again, closes its connection. Nothing of the frames is
// logged, only the Worker and the reason.
func (p *Pipe) cutOff(workerID string, st FrameStream, why string) {
	p.log.Warn("pipe: the turn channel is reset: "+why, "worker_id", workerID)
	if p.limiter.strike(workerID) && p.abort != nil {
		p.log.Warn("pipe: the connection of a Worker that keeps flooding the turn channel is closed", "worker_id", workerID)
		p.abort(workerID)
	}
	st.Reset()
}

// RegisterTurns makes the pipe the server of the turn channel on the event
// ingest: an "ev" stream whose first frame is turns.open is served by
// [Pipe.ServeTurns] for the Worker the stream belongs to (the Worker of the
// authenticated connection, never one named in a frame). It reports false when
// the ingest already serves that first frame.
func (p *Pipe) RegisterTurns(in *Ingest) bool {
	return in.OnStream(FrameTurnsOpen, func(ctx context.Context, info Online, st *transport.Stream, _ json.RawMessage) {
		p.ServeTurns(ctx, info.WorkerID, st)
	})
}

// RunPrune deletes the messages and turns older than the retention of 7
// days now and then every interval (zero selects [DefaultPruneInterval])
// until ctx ends.
func (p *Pipe) RunPrune(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPruneInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := p.broker.Prune(ctx, p.now()); err != nil && ctx.Err() == nil {
			p.log.Warn("pipe: prune failed", "error", err.Error())
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

var _ FrameStream = (*transport.Stream)(nil)
