package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// DefaultLockWait is how long a write waits for the writer lock of its pane
// before it ends as failed_not_sent/busy.
const DefaultLockWait = 5 * time.Second

// Classifier reports kind and state of a pane (AD-12). *agentdetect.Detector
// implements it; tests use a fake.
type Classifier interface {
	Classify(ctx context.Context, in agentdetect.Input) agentdetect.Result
}

// State is the Worker database as the executor uses it. *store.Store
// implements it.
type State interface {
	NextSessionNumberContext(ctx context.Context) (int64, error)
	GetResult(ctx context.Context, requestID string) (store.ResultRecord, bool, error)
	PutResult(ctx context.Context, rec store.ResultRecord) error
	ReplaceResult(ctx context.Context, rec store.ResultRecord) error
	DeleteResult(ctx context.Context, requestID string) error
	AppendAudit(ctx context.Context, rec store.AuditRecord) error
}

var _ State = (*store.Store)(nil)

// Config assembles an [Executor]. Host, Classifier, State and Policy are
// required.
type Config struct {
	Host       term.SessionHost
	Classifier Classifier
	State      State
	Policy     *Policy
	// Locks is the set of writer locks shared with every other entry point
	// that writes to panes. Nil creates a private set.
	Locks *Locks
	// LockWait bounds the wait for a writer lock; zero means
	// [DefaultLockWait].
	LockWait time.Duration
	// DefaultCwd is the starting directory of session_create when the
	// request names none; empty leaves it to the host.
	DefaultCwd string
	// Logger receives operational messages. It never gets terminal content,
	// text, keys or secrets (AD-20). Nil discards them.
	Logger *slog.Logger
}

// Request is one op request as the executor sees it: the op name, the
// caller's effective actions for the resolved target (computed by the Hub, or
// the full set on the local socket) and the JSON arguments.
type Request struct {
	Op      string
	Actions []ops.Action
	Args    json.RawMessage
}

// Handler executes one op. The executor passes the request through unchanged;
// the handler decodes the arguments and returns the typed result or an
// [*APIError].
type Handler func(ctx context.Context, rq Request) (any, error)

// Executor runs the Worker ops against a [term.SessionHost], enforcing the
// action contract of AD-15: truthful outcomes, KEEP/WARN/FORCE guards,
// duplicate request ids answered from a cache, per-pane writer locks and the
// local policy. It is safe for concurrent use.
type Executor struct {
	host     term.SessionHost
	classify Classifier
	state    State
	policy   *Policy
	locks    *Locks
	lockWait time.Duration
	cwd      string
	log      *slog.Logger

	mu       sync.RWMutex
	handlers map[string]Handler

	// shareMu serialises share_set: it reads the saved selection and writes it
	// back in one step, so that two calls cannot interleave.
	shareMu sync.Mutex
	// shareBarrier is the highest Hub sequence number of a share_set (and of a
	// probe) seen so far, guarded by shareMu: a share_set with an equal or
	// lower one is refused, so that the Hub can learn the rule in force with the
	// proof that no change it dispatched earlier can still be applied. It is in
	// memory on purpose: a request that was in flight in an earlier process
	// does not exist any more.
	shareBarrier uint64

	// pipe reads the replies of pipe messages (Story 7.3); nil until the pipe
	// module attaches it.
	pipe atomic.Pointer[PipeService]

	// mem keeps the completed write results that the database could not
	// store, so that a duplicate request id still finds the precise answer
	// (see write).
	memMu sync.Mutex
	mem   map[string]memEntry
}

// NewExecutor builds an executor with the release-1 session and write ops
// registered. Later stories add ops with [Executor.Handle].
func NewExecutor(c Config) (*Executor, error) {
	switch {
	case c.Host == nil:
		return nil, errors.New("agent: executor needs a session host")
	case c.Classifier == nil:
		return nil, errors.New("agent: executor needs a classifier")
	case c.State == nil:
		return nil, errors.New("agent: executor needs the Worker state")
	case c.Policy == nil:
		return nil, errors.New("agent: executor needs a policy")
	}
	e := &Executor{
		host: c.Host, classify: c.Classifier, state: c.State, policy: c.Policy,
		locks: c.Locks, lockWait: c.LockWait, cwd: c.DefaultCwd, log: c.Logger,
		handlers: map[string]Handler{},
	}
	if e.locks == nil {
		e.locks = NewLocks()
	}
	if e.lockWait <= 0 {
		e.lockWait = DefaultLockWait
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	e.Handle(ops.OpSessionsList, e.sessionsList)
	e.Handle(ops.OpSessionSnapshot, e.sessionSnapshot)
	e.Handle(ops.OpSessionCapture, e.sessionCapture)
	e.Handle(ops.OpSessionCreate, e.sessionCreate)
	e.Handle(ops.OpSendText, e.sendText)
	e.Handle(ops.OpSendKeys, e.sendKeys)
	e.Handle(ops.OpSetTask, e.setTask)
	e.Handle(ops.OpAnswer, e.answer)
	// pipe_send is announced from the first hello, whatever order the modules
	// start in (Story 7.3). pipe_read and pipe_wait are not executor ops.
	e.Handle(ops.OpPipeSend, e.pipeSend)
	// share_get and share_set (Story 4.15) are announced from the first hello
	// too: a Worker that has them is the one the Hub can ask about its sharing.
	e.Handle(ops.OpShareGet, e.shareGet)
	e.Handle(ops.OpShareSet, e.shareSet)
	return e, nil
}

// Handle registers the handler of an op, replacing an earlier one. It is how
// further stories (the pipe ops, share_get and share_set) join the executor.
func (e *Executor) Handle(op string, h Handler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[op] = h
}

// Locks returns the writer locks, so that other entry points writing to panes
// take the same ones (AD-15).
func (e *Executor) Locks() *Locks { return e.locks }

// Policy returns the local policy.
func (e *Executor) Policy() *Policy { return e.policy }

// Execute runs one op and returns its result as JSON. A refusal that is not
// an outcome (permission, not found, invalid argument, conflict, internal)
// comes back as an [*APIError]; everything that reached a pane is an outcome
// inside a ops.WriteResult.
func (e *Executor) Execute(ctx context.Context, rq Request) (json.RawMessage, error) {
	e.mu.RLock()
	h := e.handlers[rq.Op]
	e.mu.RUnlock()
	if h == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	res, err := h(ctx, rq)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, internalErr("encode result", err)
	}
	return b, nil
}

// lookupOp returns the catalog entry of a Worker op.
func lookupOp(name string) (ops.Op, error) {
	op, ok := ops.Lookup(name)
	if !ok || op.Scope != ops.ScopeWorker {
		return ops.Op{}, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	return op, nil
}

// decode parses the JSON arguments of a request into T and runs T's Validate.
// Unknown fields are ignored: a minor change of an op is additive and a
// receiver ignores what it does not know (AD-4). Invalid JSON, a wrong field
// type, a second JSON value and a failed Validate are invalid_argument errors.
func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return v, apiErr(proto.CodeInvalidArgument, "", "the arguments are not valid JSON for this operation")
	}
	if dec.More() {
		return v, apiErr(proto.CodeInvalidArgument, "", "the arguments hold more than one JSON value")
	}
	if val, ok := any(v).(ops.Validator); ok {
		if err := val.Validate(); err != nil {
			return v, apiErr(proto.CodeInvalidArgument, "", err.Error())
		}
	}
	return v, nil
}

// prepareWrite is prepare for a write op. A caller that is not allowed to use
// the op still leaves a local audit row (AD-20): the op, the target and the
// request id when the arguments are valid, never the content. meta returns the
// write arguments and the target of the decoded arguments.
func prepareWrite[T any](e *Executor, ctx context.Context, rq Request, meta func(T) (ops.WriteArgs, ops.Target)) (ops.Op, T, error) {
	op, args, err := prepare[T](rq)
	if err != nil {
		if ae, ok := AsAPIError(err); ok && ae.Code == proto.CodeForbidden {
			var wa ops.WriteArgs
			var t ops.Target
			if v, derr := decode[T](rq.Args); derr == nil {
				wa, t = meta(v)
			}
			e.record(ctx, rq.Op, wa, t, ops.WriteResult{}, err)
		}
		return ops.Op{}, args, err
	}
	return op, args, nil
}

// prepare looks up the op, checks that the caller's actions are accepted for
// it and decodes the arguments. Action checks come first: a caller that may
// not use the op learns nothing about its arguments.
func prepare[T any](rq Request) (ops.Op, T, error) {
	var zero T
	op, err := lookupOp(rq.Op)
	if err != nil {
		return ops.Op{}, zero, err
	}
	if err := authorize(op.Actions, rq.Actions); err != nil {
		return ops.Op{}, zero, err
	}
	args, err := decode[T](rq.Args)
	if err != nil {
		return ops.Op{}, zero, err
	}
	return op, args, nil
}

// APIError is a failure that is not an outcome (AD-15, AD-18): permission,
// not found, invalid argument, conflict or an internal error. It carries a
// code from internal/proto and an optional more specific reason code.
type APIError struct {
	Code    proto.Code
	Reason  proto.Code // for example preview_only under forbidden; empty when none
	Message string     // short English text, never terminal content
	// Details is the JSON object of more detail that reaches the caller as the
	// details of the error frame (for example retry_after_ms of a rate limit);
	// nil when there is none. It never holds terminal content.
	Details json.RawMessage
	Err     error // the cause, for logs; never sent to callers
}

// Error implements error.
func (e *APIError) Error() string {
	s := string(e.Code)
	if e.Reason != "" {
		s += " (" + string(e.Reason) + ")"
	}
	s += ": " + e.Message
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the cause.
func (e *APIError) Unwrap() error { return e.Err }

func apiErr(code, reason proto.Code, msg string) *APIError {
	return &APIError{Code: code, Reason: reason, Message: msg}
}

func internalErr(what string, err error) *APIError {
	return &APIError{Code: proto.CodeInternal, Message: what + " failed", Err: err}
}

// AsAPIError returns the [*APIError] in err's chain.
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// authorize checks the accepted action set of an op against the caller's
// actions. Failing it is a forbidden API error, never an outcome, and force
// does not change it.
func authorize(accepted ops.ActionSet, held []ops.Action) error {
	if !accepted.Permits(held...) {
		return apiErr(proto.CodeForbidden, "", fmt.Sprintf("this operation needs the action %s", accepted))
	}
	return nil
}

// authorizeWrite is the Worker's final check of a write once the pane kind is
// known (AD-12): the action `message` alone delivers only to an agent:* pane,
// every other pane needs `operate`.
func authorizeWrite(accepted ops.ActionSet, held []ops.Action, kind string) error {
	if err := authorize(accepted, held); err != nil {
		return err
	}
	operate := false
	for _, h := range held {
		operate = operate || (h == ops.ActionOperate && accepted.Permits(ops.ActionOperate))
	}
	if !operate && !isAgentKind(kind) {
		return apiErr(proto.CodeForbidden, "", "the action message delivers only to agent sessions; this pane needs operate")
	}
	return nil
}

func isAgentKind(kind string) bool {
	return len(kind) > len(ops.PaneKindAgentPrefix) && kind[:len(ops.PaneKindAgentPrefix)] == ops.PaneKindAgentPrefix
}

// hostErr maps an error of the session host to an API error. The cause stays
// in Err for the log; the message is generic.
func hostErr(what string, err error) error {
	switch {
	case errors.Is(err, term.ErrNotFound):
		return &APIError{Code: proto.CodeTargetNotFound, Message: "the session or pane does not exist", Err: err}
	case errors.Is(err, ops.ErrInvalidArgument):
		return &APIError{Code: proto.CodeInvalidArgument, Message: "the terminal host rejected an argument", Err: err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return internalErr(what, err)
}
