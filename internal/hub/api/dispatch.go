package api

import (
	"context"
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
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
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
	id, err := d.Auth.Authenticate(r)
	if err != nil {
		WriteError(w, err)
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

	args, requestWorker, err := d.decode(w, r, op, rt, id.Principal)
	if err != nil {
		WriteError(w, err)
		return
	}
	var res authz.Resource
	if ent.resource != nil {
		res, err = ent.resource(id.Principal, args)
	} else {
		res, err = defaultResource(id.Principal, op, args, requestWorker)
	}
	if err != nil {
		WriteError(w, err)
		return
	}
	call = &Call{Op: op, Args: args, Principal: id.Principal, Identity: id, Resource: res, IP: id.IP}

	if err := d.Authz.Decide(r.Context(), id.Principal, op.Actions, res); err != nil {
		e := ErrorOf(err)
		if e.Code == proto.CodeInternal {
			d.logger().Error("authorisation failed", "op", op.Name, "error", err)
		}
		d.audit(r.Context(), call, AuditOutcomeDenied)
		WriteError(w, e)
		return
	}

	pending = op.Write || auditedMutation(op)
	if ent.stream != nil {
		d.serveStream(w, r, call, ent.stream)
		pending = false
		return
	}
	result, err := ent.handler(r.Context(), call)
	pending = false
	if err != nil {
		e := ErrorOf(err)
		if e.Code == proto.CodeInternal {
			d.logger().Error("operation failed", "op", op.Name, "error", err)
		}
		d.audit(r.Context(), call, auditOutcomeError)
		WriteError(w, e)
		return
	}
	d.audit(r.Context(), call, outcomeOf(result))
	if result == nil {
		result = struct{}{}
	}
	if err := WriteJSON(w, http.StatusOK, result); err != nil {
		d.logger().Error("operation result cannot be encoded", "op", op.Name, "error", err)
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
	for name := range obj {
		if ops.HiddenMember(op.Name, name) {
			delete(obj, name)
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
	}
	return AuditOutcomeSuccess
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

// audit records a write operation. A failing audit write never changes the
// answer, but it is logged and never silent.
func (d *Dispatcher) audit(ctx context.Context, c *Call, outcome string) {
	if d.Audit == nil || (!c.Op.Write && !auditedMutation(c.Op)) {
		return
	}
	ev := audit.Event{
		Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs,
		Action: c.Op.Name, Target: ResourceLabel(c.Resource), Outcome: outcome,
	}
	if err := d.Audit.Record(context.WithoutCancel(ctx), ev); err != nil {
		d.logger().Error("audit write failed", "action", c.Op.Name, "error", err)
	}
}

// recheck authenticates the stream's request again and decides the call anew
// with the refreshed principal. The identity must still be the same caller.
func (d *Dispatcher) recheck(ctx context.Context, r *http.Request, c *Call) error {
	id, err := d.Auth.Authenticate(r.WithContext(ctx))
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
