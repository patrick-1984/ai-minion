package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// DefaultReauthInterval is how often a streaming response re-checks its
// caller's permission (AD-11: at least every 30 s).
const DefaultReauthInterval = 30 * time.Second

// DefaultBodyTimeout bounds the time a client may take to send a request body.
// The Hub's server timeouts cover the headers and idle connections only, so
// a client that stops sending would otherwise hold its goroutine for ever.
const DefaultBodyTimeout = 30 * time.Second

// DefaultStreamWriteTimeout bounds one write of a streaming response. A
// client that stops reading would otherwise block the write, and with it the
// handler and its resources, until it disconnects.
const DefaultStreamWriteTimeout = 30 * time.Second

// Decider is the single authorisation point; *authz.Authorizer implements it.
type Decider interface {
	Decide(ctx context.Context, p authz.Principal, accepted ops.ActionSet, res authz.Resource) error
}

var _ Decider = (*authz.Authorizer)(nil)

// Authn authenticates a request; *Authenticator implements it.
type Authn interface {
	Authenticate(r *http.Request) (Identity, error)
}

var _ Authn = (*Authenticator)(nil)

// RequestStore keeps the answers of request ids for the de-duplication of
// Hub-scope writes; *store.Store implements it.
type RequestStore interface {
	GetOpRequest(ctx context.Context, requestID string, now int64) (store.OpRequestRow, bool, error)
	PutOpRequest(ctx context.Context, r store.OpRequestRow) (bool, error)
}

var _ RequestStore = (*store.Store)(nil)

// Audit vocabulary of write operations (AD-20). The action is the operation
// name and the target is a resource id; no event holds arguments or content.
const (
	AuditOutcomeSuccess = "success"
	AuditOutcomeDenied  = "denied"
	// A write that ends in another outcome of the action contract
	// (failed_not_sent, partial, unknown) is audited under that outcome, and
	// one that returns an error as "failed".
	auditOutcomeError = "failed"
)

// Dispatcher runs catalog operations over HTTP. Build one with the fields
// set; the zero value of the optional fields is usable.
type Dispatcher struct {
	// Catalog declares the operations. Required.
	Catalog *ops.Registry
	// Handlers executes them. Required.
	Handlers *Registry
	// Auth authenticates the caller. Required.
	Auth Authn
	// Authz decides every call. Required.
	Authz Decider
	// Audit records every write operation. Nil disables it.
	Audit Auditor
	// Requests de-duplicates the Hub-scope writes of the strict operations
	// (ops.StrictArgs) by their request_id: a repeated request is answered
	// from the stored result and the handler does not run again. Nil
	// disables it: every request runs.
	Requests RequestStore
	// Log receives internal failures; nil means slog.Default.
	Log *slog.Logger
	// ReauthInterval is the credential and permission re-check period of
	// streams; zero means DefaultReauthInterval.
	ReauthInterval time.Duration
	// BodyTimeout bounds how long a request body may take to arrive; zero
	// means DefaultBodyTimeout.
	BodyTimeout time.Duration
	// StreamWriteTimeout bounds each write of a streaming response; zero
	// means DefaultStreamWriteTimeout.
	StreamWriteTimeout time.Duration

	// inFlight orders the requests that carry the same request_id.
	inFlight requestLocks
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// route says how a request reached an operation.
type route struct {
	// op is the operation name.
	op string
	// path holds the path parameters of an alias; they override body members.
	path url.Values
	// methods are the allowed HTTP methods; nil means the operation's own
	// (POST, and GET for reads).
	methods []string
}

// refused writes err as the answer and, at debug level, one line with the
// operation, the stage that refused it, the kind of the caller (empty before
// authentication) and the error code. The message is not logged: it can name
// what the caller asked for.
func (d *Dispatcher) refused(w http.ResponseWriter, op, stage string, kind authz.Kind, err error) {
	e := ErrorOf(err)
	d.logger().Debug("api request refused", "op", op, "stage", stage, "principal_kind", string(kind), "code", string(e.Code))
	WriteError(w, e)
}

// serve runs the operation of rt for r: authenticate, find the handler,
// decode and validate the arguments, derive the resource, authz.Decide, run
// the handler. Every failure is written as an AD-18 envelope.
func (d *Dispatcher) serve(w http.ResponseWriter, r *http.Request, rt route) {
	var (
		call    *Call // set once the caller is authorised
		pending bool  // a write handler is running and its audit row is due
	)
	defer func() {
		if p := recover(); p != nil {
			// The panic value can hold anything; only its type is logged.
			d.logger().Error("panic in API handler", "op", rt.op, "panic_type", fmt.Sprintf("%T", p))
			if pending {
				// The handler may have changed state before it panicked:
				// the write is audited with the outcome "unknown" (AD-20).
				d.audit(r.Context(), call, string(ops.OutcomeUnknown))
			}
			WriteError(w, internalError())
		}
	}()

	// The body deadline runs from here, so that every early answer (an
	// authentication failure, an unsupported operation, a method error) is
	// covered too: net/http reads the unread rest of a body before it
	// answers, and that read must not wait for a stalled client.
	d.armBody(w, r)
	start := time.Now()
	id, err := d.Auth.Authenticate(r)
	if err != nil {
		d.refused(w, rt.op, "authenticate", "", err)
		return
	}
	op, ent, ok := d.find(rt.op)
	if !ok {
		WriteError(w, NewError(proto.CodeUnsupportedOp, "the operation is not available on this Hub").
			WithDetails(map[string]any{"op": rt.op}))
		return
	}
	if !methodAllowed(op, rt, r.Method) {
		w.Header().Set("Allow", strings.Join(allowedMethods(op, rt), ", "))
		WriteError(w, NewError(proto.CodeMethodNotAllowed, "use "+strings.Join(allowedMethods(op, rt), " or ")+" for this route"))
		return
	}

	if op.SessionOnly && !sessionOnlyAllows(op, id) {
		// Only a browser session acting as itself may run these (the Hub
		// identity operations); an API key may not, and an act-as session only
		// where sessionOnlyActAs says so.
		d.refused(w, op.Name, "session_only", id.Principal.Kind, NewError(proto.CodeForbidden, "this operation is available only to a signed-in browser session"))
		return
	}

	args, requestWorker, err := d.decode(w, r, op, rt, id.Principal)
	if err != nil {
		d.refused(w, op.Name, "arguments", id.Principal.Kind, err)
		return
	}
	// A repeated request of a de-duplicated write gets the answer it was
	// given then, and nothing runs again. The caller is authenticated as the
	// one that was authorised for exactly this request, but that authority
	// may be gone since: a stored answer that carries data (a grant with its
	// session names) is given only after the same decision as a new request
	// allows it now. Only the data-free acknowledgement is answered at once,
	// before the resource is derived (a repeated revoke names a grant that is
	// gone). An answer given here adds no audit row.
	dedup, deduped := d.dedupKeyOf(op, ent, args, id)
	reused := false
	var replay *storedAnswer
	if deduped {
		// Two requests with one id run one after the other, so the second
		// finds the answer of the first instead of running beside it.
		unlock := d.inFlight.lock(dedup.id)
		defer unlock()
		answer, state, err := d.recall(r.Context(), op, dedup)
		if err != nil {
			// Fail closed: the Hub cannot tell whether the request ran.
			d.logger().Error("request de-duplication failed", "op", op.Name, "error", err)
			d.refused(w, op.Name, "request_id", id.Principal.Kind, internalError())
			return
		}
		switch state {
		case requestRepeated:
			if !dataFreeAnswer(answer) {
				replay = &answer
				break
			}
			writeStored(w, answer)
			d.logger().Debug("api request done", "request_id", audit.RequestID(r.Context()), "op", op.Name, "principal_kind", string(id.Principal.Kind),
				"outcome", "replayed", "duration", time.Since(start).String())
			return
		case requestReused:
			reused = true
		}
	}
	var res authz.Resource
	if ent.resource != nil {
		res, err = ent.resource(id.Principal, args)
	} else {
		res, err = defaultResource(id.Principal, op, args, requestWorker)
	}
	if err != nil {
		d.refused(w, op.Name, "resource", id.Principal.Kind, err)
		return
	}
	// A handler that records its own, more precise audit event (settings,
	// invitations, Worker approval) leaves the dispatcher no row to add.
	tctx, tracker := audit.WithTracker(r.Context())
	r = r.WithContext(tctx)
	call = &Call{Op: op, Args: args, Principal: id.Principal, Identity: id, Resource: res, IP: id.IP, Request: r}

	if err := d.Authz.Decide(r.Context(), id.Principal, op.Actions, res); err != nil {
		e := ErrorOf(err)
		if e.Code == proto.CodeInternal {
			d.logger().Error("authorisation failed", "op", op.Name, "error", err)
		}
		d.audit(r.Context(), call, AuditOutcomeDenied)
		if e.Code == proto.CodeForbidden && e.Message == "not allowed" && id.Principal.Kind == authz.KindAPIKey {
			// The accepted actions are public catalog data: name them. The
			// key's effective actions are its configured ones limited by its
			// owner's current role, so either can be the cause.
			e = NewError(proto.CodeForbidden, "not allowed: "+op.Name+" needs one of the actions ("+op.Actions.String()+") in this API key's effective permissions, which are the key's actions limited by its owner's current role and its resources; check them in the Hub")
		}
		d.refused(w, op.Name, "authorize", id.Principal.Kind, e)
		return
	}

	if replay != nil {
		writeStored(w, *replay)
		d.logger().Debug("api request done", "request_id", audit.RequestID(r.Context()), "op", op.Name, "principal_kind", string(id.Principal.Kind),
			"outcome", "replayed", "duration", time.Since(start).String())
		return
	}

	if reused {
		// The id belongs to another request (another operation, caller or
		// arguments): nothing runs. It is said only now, to a caller that
		// may run the operation, so that an id cannot be probed.
		d.audit(r.Context(), call, auditOutcomeError)
		d.refused(w, op.Name, "request_id", id.Principal.Kind, errRequestIDReused())
		return
	}

	pending = op.Write || auditedMutation(op)
	if ent.stream != nil {
		d.serveStream(w, r, call, ent.stream)
		pending = false
		return
	}
	hctx := r.Context()
	if deduped {
		// A handler that writes in a store transaction can commit the answer
		// of the request_id with its change (requestReceipt).
		hctx = context.WithValue(hctx, receiptKey{}, receiptRequest{op: op.Name, key: dedup})
	}
	result, err := ent.handler(hctx, call)
	pending = false
	if err != nil {
		e := ErrorOf(err)
		if e.Code == proto.CodeInternal {
			d.logger().Error("operation failed", "op", op.Name, "error", err)
		}
		if !tracker.Recorded() {
			d.audit(r.Context(), call, auditOutcomeError)
		}
		if deduped && definiteRefusal(e) {
			d.remember(r.Context(), op, dedup, proto.HTTPStatus(e.Code), Envelope(e))
		}
		d.logger().Debug("api request done", "request_id", audit.RequestID(r.Context()), "op", op.Name, "principal_kind", string(id.Principal.Kind),
			"outcome", string(e.Code), "duration", time.Since(start).String())
		WriteError(w, e)
		return
	}
	if !tracker.Recorded() {
		d.audit(r.Context(), call, outcomeOf(result))
	}
	d.logger().Debug("api request done", "request_id", audit.RequestID(r.Context()), "op", op.Name, "principal_kind", string(id.Principal.Kind),
		"outcome", "ok", "duration", time.Since(start).String())
	if result == nil {
		result = struct{}{}
	}
	if deduped {
		if b, err := json.Marshal(result); err == nil && definiteResult(b) {
			d.remember(r.Context(), op, dedup, http.StatusOK, b)
		}
	}
	if err := WriteJSON(w, http.StatusOK, result); err != nil {
		d.logger().Error("operation result cannot be encoded", "op", op.Name, "error", err)
	}
}

// sessionOnlyActAs names the SessionOnly operations that an administrator
// acting as a user may run in a browser session. grant_set: D4 lets an
// administrator acting as a user create owner grants for that user only; the
// handler refuses every other grant (the acted user must own both Workers),
// and the audit row names the administrator as the actor.
var sessionOnlyActAs = map[string]bool{ops.OpGrantSet: true}

// sessionOnlyAllows reports whether id may run the SessionOnly operation op:
// a browser session acting as itself, or an act-as browser session for an
// operation in sessionOnlyActAs. An API key never may.
func sessionOnlyAllows(op ops.Op, id Identity) bool {
	if id.Via != ViaSession {
		return false
	}
	switch id.Principal.Kind {
	case authz.KindBrowserSession:
		return true
	case authz.KindActAs:
		return sessionOnlyActAs[op.Name]
	}
	return false
}

// dedupKey identifies one request for the de-duplication: the id the caller
// chose, who asked and the hash of what was asked.
type dedupKey struct {
	id, actor, argsHash string
}

// errRequestIDReused answers a request_id that was used for another request
// (another operation, caller or arguments) in the retention time.
func errRequestIDReused() *Error {
	return NewError(proto.CodeConflict, "this request_id was already used for a different request").
		WithDetails(map[string]any{"field": "request_id", "reason": "request_id_reused"})
}

// dedupKeyOf reports whether the call is de-duplicated, and by which key:
// a strict operation (ops.StrictArgs) of the Hub scope that is a Write or
// Mutates, that does not stream and whose arguments carry a request_id. The
// hash covers the decoded arguments, so the order and spacing of the body do
// not make two requests differ; a strict operation has no undecoded members.
func (d *Dispatcher) dedupKeyOf(op ops.Op, ent entry, args any, id Identity) (dedupKey, bool) {
	if d.Requests == nil || ent.stream != nil || op.Scope != ops.ScopeHub || (!op.Write && !op.Mutates) || !ops.StrictArgs(op.Name) {
		return dedupKey{}, false
	}
	b, err := json.Marshal(args)
	if err != nil {
		return dedupKey{}, false
	}
	var head struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.RequestID == "" {
		return dedupKey{}, false
	}
	actor := id.Actor
	if id.ActingAs != "" {
		actor += " as " + id.ActingAs
	}
	sum := sha256.Sum256(b)
	return dedupKey{id: head.RequestID, actor: actor, argsHash: hex.EncodeToString(sum[:])}, true
}

// receiptKey is the context key of the de-duplicated request a handler runs
// for; receiptRequest its value.
type receiptKey struct{}

type receiptRequest struct {
	op  string
	key dedupKey
}

// requestReceipt is the stored answer of the de-duplicated request of ctx
// when the handler answers it with result: the row the dispatcher would store
// after the handler returned, for a handler that writes it in the transaction
// of its change, so that the change and its answer commit together. It is nil
// when the request is not de-duplicated or the result is not kept (see
// definiteResult). The dispatcher still stores its own row afterwards; the
// first one written stays.
func requestReceipt(ctx context.Context, result any) (*store.OpRequestRow, error) {
	req, ok := ctx.Value(receiptKey{}).(receiptRequest)
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("api: the answer of a request_id cannot be encoded: %w", err)
	}
	if !definiteResult(b) {
		return nil, nil
	}
	ans, err := json.Marshal(storedAnswer{Status: http.StatusOK, Body: b})
	if err != nil {
		return nil, fmt.Errorf("api: the answer of a request_id cannot be encoded: %w", err)
	}
	return &store.OpRequestRow{RequestID: req.key.id, Op: req.op, Actor: req.key.actor, ArgsSHA256: req.key.argsHash,
		ResultJSON: string(ans), CreatedAt: time.Now().UnixMilli()}, nil
}

// storedAnswer is what the de-duplication keeps of an answer: the HTTP status
// and the body, a result or an error envelope.
type storedAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// What the store knows of a request_id.
const (
	requestNew      = iota // not seen in the retention time: the request runs
	requestRepeated        // the same request: answered from the stored answer
	requestReused          // the id of another request: a conflict
)

// recall looks the request_id of k up: the stored answer when the request is
// a repeat (same operation, caller and arguments), or that the id was used
// for another request.
func (d *Dispatcher) recall(ctx context.Context, op ops.Op, k dedupKey) (storedAnswer, int, error) {
	row, found, err := d.Requests.GetOpRequest(ctx, k.id, time.Now().UnixMilli())
	if err != nil {
		return storedAnswer{}, requestNew, err
	}
	if !found {
		return storedAnswer{}, requestNew, nil
	}
	if row.Op != op.Name || row.Actor != k.actor || row.ArgsSHA256 != k.argsHash {
		return storedAnswer{}, requestReused, nil
	}
	var a storedAnswer
	if err := json.Unmarshal([]byte(row.ResultJSON), &a); err != nil {
		return storedAnswer{}, requestNew, fmt.Errorf("api: the stored answer of a request_id cannot be read: %w", err)
	}
	if a.Status == 0 || len(a.Body) == 0 {
		return storedAnswer{}, requestNew, errors.New("api: the stored answer of a request_id is incomplete")
	}
	return a, requestRepeated, nil
}

// dataFreeAnswer reports whether a stored answer is the bare acknowledgement
// of a write (outcome delivered, nothing else), which tells a repeat of the
// request nothing it did not know. Every other stored answer, an error
// envelope included, is given again only to a caller the same decision as a
// new request allows now.
func dataFreeAnswer(a storedAnswer) bool {
	var ack ops.WriteResult
	dec := json.NewDecoder(bytes.NewReader(a.Body))
	dec.DisallowUnknownFields()
	return a.Status == http.StatusOK && dec.Decode(&ack) == nil && ack.Outcome == ops.OutcomeDelivered && ack.Reason == "" &&
		len(ack.Warnings) == 0 && ack.Pane == nil && ack.Note == ""
}

// writeStored writes a stored answer as WriteJSON and WriteError wrote it.
func writeStored(w http.ResponseWriter, a storedAnswer) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(a.Status)
	_, _ = w.Write(append(a.Body, '\n'))
}

// remember stores the answer of a de-duplicated request. A failure never
// changes the answer of the request that ran, but it is logged: a repeat of
// it would run again. The write does not end with the request, because a
// caller that is gone is the one that will ask again.
func (d *Dispatcher) remember(ctx context.Context, op ops.Op, k dedupKey, status int, body []byte) {
	b, err := json.Marshal(storedAnswer{Status: status, Body: body})
	if err == nil {
		_, err = d.Requests.PutOpRequest(context.WithoutCancel(ctx), store.OpRequestRow{
			RequestID: k.id, Op: op.Name, Actor: k.actor, ArgsSHA256: k.argsHash,
			ResultJSON: string(b), CreatedAt: time.Now().UnixMilli(),
		})
	}
	if err != nil {
		d.logger().Error("the answer of a request_id was not stored", "op", op.Name, "error", err)
	}
}

// definiteRefusal reports whether an error answer is kept for its
// request_id: a refusal that says the request did not and will not run as it
// is. An internal error, a rate limit and everything about the caller's
// sign-in (a fresh sign-in is asked for, then the same request is sent
// again) are not kept, so the repeat runs.
func definiteRefusal(e *Error) bool {
	if e.RetryAfter > 0 {
		return false
	}
	switch e.Code {
	case proto.CodeConflict, proto.CodeNotFound, proto.CodeTargetNotFound, proto.CodeForbidden, proto.CodeInvalidArgument:
		return true
	}
	return false
}

// definiteResult reports whether a result is kept for its request_id: every
// result but one that states the outcome "unknown", which a repeat settles.
func definiteResult(body []byte) bool {
	var head struct {
		Outcome ops.Outcome `json:"outcome"`
	}
	return json.Unmarshal(body, &head) != nil || head.Outcome != ops.OutcomeUnknown
}

// requestLocks is one lock per request_id in flight; the zero value is
// ready. The Hub is one process over one database, so a lock in memory
// orders every request that can carry the same id.
type requestLocks struct {
	mu sync.Mutex
	m  map[string]*requestLock
}

type requestLock struct {
	mu   sync.Mutex
	refs int
}

// lock waits for the lock of id and returns the function that releases it.
func (l *requestLocks) lock(id string) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*requestLock)
	}
	e := l.m[id]
	if e == nil {
		e = &requestLock{}
		l.m[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}

func (d *Dispatcher) find(name string) (ops.Op, entry, bool) {
	op, ok := d.Catalog.Lookup(name)
	if !ok {
		return ops.Op{}, entry{}, false
	}
	ent, ok := d.Handlers.lookup(name)
	if !ok {
		return ops.Op{}, entry{}, false
	}
	return op, ent, true
}

func allowedMethods(op ops.Op, rt route) []string {
	if rt.methods != nil {
		return rt.methods
	}
	return op.RESTMethods()
}

func methodAllowed(op ops.Op, rt route, method string) bool {
	for _, m := range allowedMethods(op, rt) {
		if m == method {
			return true
		}
	}
	return false
}

// argsFromQuery reports whether the arguments of a method travel in the query
// string (GET and DELETE) instead of the body.
func argsFromQuery(method string) bool {
	return method == http.MethodGet || method == http.MethodDelete
}

// decode builds the validated argument struct of op from the request. It
// also returns the worker_id member of the request: a ScopeWorker operation
// whose arguments have no target (sessions_list, session_create) is routed by
// it, whether or not the argument struct declares the member.
func (d *Dispatcher) decode(w http.ResponseWriter, r *http.Request, op ops.Op, rt route, p authz.Principal) (any, string, error) {
	var obj rawObject
	// A strict operation refuses a member it does not declare; every other
	// operation ignores it, as before.
	strict := ops.StrictArgs(op.Name)
	if argsFromQuery(r.Method) {
		// The arguments are the query string; a body is not part of the
		// request. It is still read (and dropped) under the deadline, so a
		// client cannot stall on it once the operation runs.
		if err := d.discardBody(w, r); err != nil {
			return nil, "", err
		}
		obj = rawObject{}
		query := r.URL.Query()
		if d.Handlers.targetResolver() != nil && op.Scope == ops.ScopeWorker {
			query = humanTargetsFromQuery(op, query, obj)
		}
		if strict {
			if name, unknown := unknownValue(op.Args, query, routingMembers(op)...); unknown {
				return nil, "", errUnknownMember(name)
			}
		}
		if err := obj.addValues(op.Args, query); err != nil {
			return nil, "", err
		}
		if vs := r.URL.Query()["worker_id"]; len(vs) > 0 && op.Scope == ops.ScopeWorker {
			raw, _ := json.Marshal(vs[0])
			if _, ok := obj["worker_id"]; !ok { // the arguments may declare it
				obj["worker_id"] = raw
			}
		}
	} else {
		var err error
		if obj, err = d.readBody(w, r); err != nil {
			return nil, "", err
		}
	}
	if err := obj.addValues(op.Args, rt.path); err != nil {
		return nil, "", err
	}
	if op.Scope == ops.ScopeWorker {
		// A routing-only {worker_id} of an alias (the argument struct has no
		// such member) is not an argument, but it names the Worker and, like
		// every path parameter, overrides the body and the query string.
		if v := rt.path.Get("worker_id"); v != "" {
			raw, _ := json.Marshal(v)
			obj["worker_id"] = raw
		}
	}
	if err := resolveTargets(r.Context(), d.Handlers.targetResolver(), p, op, obj); err != nil {
		return nil, "", err
	}
	var requestWorker string
	if op.Scope == ops.ScopeWorker {
		var err error
		if requestWorker, err = obj.workerID(); err != nil {
			return nil, "", err
		}
	}
	// An internal member (ops.HiddenMember, such as pipe_send's message_id) is
	// no argument of the public API: like any unknown member it is ignored, so
	// a caller cannot set what only the Hub sets.
	// A strict operation refuses it instead, like any member it does not
	// declare.
	hidden := ""
	for name := range obj {
		if ops.HiddenMember(op.Name, name) {
			if hidden == "" || name < hidden {
				hidden = name
			}
			delete(obj, name)
		}
	}
	if strict {
		if hidden != "" {
			return nil, "", errUnknownMember(hidden)
		}
		if name, unknown := unknownMember(op.Args, withoutMembers(obj, routingMembers(op))); unknown {
			return nil, "", errUnknownMember(name)
		}
	}
	args := op.NewArgs()
	if err := decodeArgs(obj, args); err != nil {
		return nil, "", err
	}
	if v, ok := args.(ops.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, "", err
		}
	}
	return args, requestWorker, nil
}

// routingMembers are the request members of an operation that are no
// arguments but name where it runs: the worker_id and the "all" flag of a
// human target of a ScopeWorker operation. A strict operation accepts them
// although its argument type need not declare them.
func routingMembers(op ops.Op) []string {
	if op.Scope == ops.ScopeWorker {
		return []string{"worker_id", "all"}
	}
	return nil
}

// withoutMembers returns o without the named members; o itself when it has
// none of them.
func withoutMembers(o rawObject, names []string) rawObject {
	out := o
	for _, name := range names {
		if _, ok := out[name]; !ok {
			continue
		}
		if len(out) == len(o) {
			out = make(rawObject, len(o))
			for k, v := range o {
				out[k] = v
			}
		}
		delete(out, name)
	}
	return out
}

func (d *Dispatcher) bodyTimeout() time.Duration {
	if d.BodyTimeout > 0 {
		return d.BodyTimeout
	}
	return DefaultBodyTimeout
}

// hasBody reports whether the request announces a body.
func hasBody(r *http.Request) bool { return r.Body != nil && r.Body != http.NoBody }

// armBody puts a read deadline on the connection of a request that has a
// body. The Hub's server timeouts cover the headers and idle connections
// only, so without it a client that announces a body and stops sending would
// hold the connection and its goroutine for ever. The deadline stays in force
// for every answer that is written before the body is complete (net/http
// reads the rest of the body first); [Dispatcher.liftBody] lifts it once the
// body has been read. A writer that cannot set a deadline (a recorder) has no
// connection to stall: the error is deliberately ignored.
func (d *Dispatcher) armBody(w http.ResponseWriter, r *http.Request) {
	if !hasBody(r) {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d.bodyTimeout()))
}

// liftBody lifts the deadline of armBody. It is called only when the whole
// body has been read, before the operation (and a possible stream) runs.
func (d *Dispatcher) liftBody(w http.ResponseWriter, r *http.Request) {
	if !hasBody(r) {
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// readBody reads the request body under the deadline of armBody. When the
// read fails the deadline stays, so that the rest of an unread body is not
// waited for either; the connection is closed after the answer.
func (d *Dispatcher) readBody(w http.ResponseWriter, r *http.Request) (rawObject, error) {
	obj, err := readObject(r.Body)
	if err != nil {
		return nil, err
	}
	d.liftBody(w, r)
	return obj, nil
}

// discardBody reads and drops the body of a request whose arguments are in
// the query string.
func (d *Dispatcher) discardBody(w http.ResponseWriter, r *http.Request) error {
	if !hasBody(r) {
		return nil
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return invalidArg("", "the request body could not be read")
	}
	if n > MaxBodyBytes {
		return NewError(proto.CodeMessageTooLarge, fmt.Sprintf("the request body exceeds %d bytes", MaxBodyBytes))
	}
	d.liftBody(w, r)
	return nil
}

// outcomeOf maps a handler result to the audit outcome: a WriteResult keeps
// its action-contract outcome, anything else that returned without error is a
// success.
func outcomeOf(result any) string {
	switch r := result.(type) {
	case ops.WriteResult:
		return writeOutcome(r)
	case *ops.WriteResult:
		if r != nil {
			return writeOutcome(*r)
		}
	case ops.WorkerUpdateResult:
		return updateOutcome(r)
	case *ops.WorkerUpdateResult:
		if r != nil {
			return updateOutcome(*r)
		}
	}
	return AuditOutcomeSuccess
}

// updateOutcome is "failed" when the Worker did not start the update (or it
// failed at once), so the audit never records a success for it.
func updateOutcome(r ops.WorkerUpdateResult) string {
	if r.Accepted {
		return AuditOutcomeSuccess
	}
	return auditOutcomeError
}

func writeOutcome(r ops.WriteResult) string {
	if r.Outcome == ops.OutcomeDelivered {
		return AuditOutcomeSuccess
	}
	return string(r.Outcome)
}

// auditedMutation reports whether the dispatcher audits op although it is not a
// Write op: a state-changing Worker op (share_set) has no handler of its own
// that audits it, so the dispatcher records it like a write, denied calls
// included. The Hub ops that mutate (Mutates) audit themselves.
func auditedMutation(op ops.Op) bool { return op.Mutates && op.Scope == ops.ScopeWorker }

// auditAction is the action of the audit row the dispatcher writes for op:
// the operation name, except where the design names the action otherwise.
func auditAction(op ops.Op) string {
	if op.Name == ops.OpGrantRevoke {
		return AuditActionGrantRevoke
	}
	return op.Name
}

// audit records a write operation. A failing audit write never changes the
// answer, but it is logged and never silent.
func (d *Dispatcher) audit(ctx context.Context, c *Call, outcome string) {
	if d.Audit == nil || (!c.Op.Write && !auditedMutation(c.Op)) {
		return
	}
	ev := audit.Event{
		Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs,
		Action: auditAction(c.Op), Target: ResourceLabel(c.Resource), Outcome: outcome,
		Source: c.AuditSource, Detail: c.AuditDetail, GrantID: c.AuditGrantID,
	}
	if c.Op.Write && c.Request != nil {
		// The standing allow rule a client named for this write
		// (proto.ClientRuleHeader): an unverified label for the audit trail,
		// recorded when it is a valid id and never read by a decision.
		ev.Detail = proto.AppendDetail(ev.Detail, proto.ClientRuleDetail(c.Request.Header.Get(proto.ClientRuleHeader)))
	}
	if err := d.Audit.Record(context.WithoutCancel(ctx), ev); err != nil {
		d.logger().Error("audit write failed", "action", c.Op.Name, "error", err)
	}
}

// recheck authenticates the stream's request again and decides the call anew
// with the refreshed principal. The identity must still be the same caller.
func (d *Dispatcher) recheck(ctx context.Context, r *http.Request, c *Call) error {
	// The re-check is not the person acting: it never extends the idle
	// lifetime of a session.
	id, err := d.Auth.Authenticate(r.WithContext(session.AsBackground(ctx)))
	if err != nil {
		return err
	}
	if id.Actor != c.Identity.Actor || id.ActingAs != c.Identity.ActingAs || id.Principal.UserID != c.Principal.UserID ||
		id.Principal.Kind != c.Principal.Kind {
		return NewError(proto.CodeUnauthenticated, "the credentials changed")
	}
	return d.Authz.Decide(ctx, id.Principal, c.Op.Actions, c.Resource)
}

// serveStream answers a streaming operation as newline-delimited JSON, one
// element per line. Every ReauthInterval the credentials of the request are
// authenticated again (a revoked key, a logged-out or expired session, a
// changed credential version all end the stream) and the refreshed principal
// is authorised again; when either fails the stream ends with an error line.
func (d *Dispatcher) serveStream(w http.ResponseWriter, r *http.Request, c *Call, fn StreamHandler) {
	interval := d.ReauthInterval
	if interval <= 0 {
		interval = DefaultReauthInterval
	}
	writeTimeout := d.StreamWriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = DefaultStreamWriteTimeout
	}
	rc := http.NewResponseController(w)
	// mu orders the write deadlines: emit arms one before each write and the
	// re-check, when it revokes the stream, sets it to now to interrupt a
	// write that is blocked on a client that does not read. After the
	// revocation emit refuses before it touches the deadline again.
	var mu sync.Mutex
	ctx, cancel := context.WithCancelCause(r.Context())
	defer func() {
		// A deadline left on the connection would fail the next request on
		// it. The lock keeps the re-check from setting one after this.
		mu.Lock()
		cancel(nil)
		_ = rc.SetWriteDeadline(time.Time{})
		mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := d.recheck(ctx, r, c); err != nil && ctx.Err() == nil {
					mu.Lock()
					if ctx.Err() == nil {
						_ = rc.SetWriteDeadline(time.Now())
						cancel(err)
					}
					mu.Unlock()
					return
				}
			}
		}
	}()

	started := false
	start := func() {
		h := w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		started = true
	}
	arm := func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := context.Cause(ctx); err != nil {
			return err
		}
		// A writer that cannot set a deadline (a recorder) cannot block.
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		return nil
	}
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := arm(); err != nil {
			return err
		}
		if !started {
			start()
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		return rc.Flush()
	}

	err := fn(ctx, c, emit)
	// The lock waits for a revocation that is in progress, so that its cause
	// is seen; the deadline that revocation left is replaced by a fresh one
	// for the last bytes, which are still bounded.
	mu.Lock()
	cause := context.Cause(ctx)
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	mu.Unlock()
	if cause != nil && !errors.Is(cause, context.Canceled) && r.Context().Err() == nil {
		err = cause // the permission check failed: that is the real reason
	}
	if err == nil {
		if !started {
			start()
		}
		return
	}
	if r.Context().Err() != nil {
		return // the client is gone
	}
	e := ErrorOf(err)
	if e.Code == proto.CodeInternal {
		d.logger().Error("stream failed", "op", c.Op.Name, "error", err)
	}
	if !started {
		WriteError(w, e)
		return
	}
	_, _ = w.Write(append(Envelope(e), '\n'))
	_ = rc.Flush()
}
