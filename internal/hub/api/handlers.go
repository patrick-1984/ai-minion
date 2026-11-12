package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Call is everything a handler gets for one request. The dispatcher has
// already authenticated the caller, validated the arguments and checked
// authz.Decide for the operation's accepted action set on Resource.
type Call struct {
	// Op is the catalog declaration of the operation.
	Op ops.Op
	// Args is a pointer to the operation's argument struct (Op.Args),
	// already validated when the type implements ops.Validator.
	Args any
	// Principal is the authz principal to pass to further authz calls (for
	// example authz.Filter on a list result).
	Principal authz.Principal
	// Identity says how the caller authenticated and who is audited.
	Identity Identity
	// Resource is the resource the decision was made on.
	Resource authz.Resource
	// IP is the client address as the Hub sees it.
	IP netip.Addr
	// Request is the HTTP request being served. Handlers read the transport
	// facts of the request from it (TLS state, forwarded headers, Origin);
	// they must not read the body, which the dispatcher has consumed.
	Request *http.Request
}

// Handler executes one catalog operation and returns its result: a value of
// the operation's Result type (or a pointer to it). A returned error is
// mapped by [ErrorOf]; return *proto.Error or *Error for a specific code.
type Handler func(ctx context.Context, c *Call) (any, error)

// StreamHandler executes a streaming operation (Op.Stream). It calls emit for
// each element of the stream, in order, and returns when the stream ends.
// emit fails when the client is gone.
type StreamHandler func(ctx context.Context, c *Call, emit func(any) error) error

// ResourceFunc names the resource an operation acts on, from the arguments.
// The dispatcher decides authz.Decide on it. Return an *Error (usually
// invalid_argument) when the arguments do not name one.
type ResourceFunc func(p authz.Principal, args any) (authz.Resource, error)

// HandlerOption changes how a handler is registered.
type HandlerOption func(*entry)

// WithResource replaces the default resource derivation (see
// [DefaultResource]). Operations that list across Workers, or that act on a
// user, use it.
func WithResource(f ResourceFunc) HandlerOption {
	return func(e *entry) { e.resource = f }
}

type entry struct {
	handler  Handler
	stream   StreamHandler
	resource ResourceFunc
}

// Registry maps operation names to the handlers that execute them. It checks
// every registration against the operation catalog it was built on.
type Registry struct {
	mu      sync.RWMutex
	catalog *ops.Registry
	byName  map[string]entry
	targets TargetResolver
}

// NewRegistry returns an empty handler registry for the operations of
// catalog.
func NewRegistry(catalog *ops.Registry) *Registry {
	return &Registry{catalog: catalog, byName: map[string]entry{}}
}

// Default is the process-wide registry for the Default catalog. Handlers that
// need no Hub service register here from an init function with [Handle]; the
// Hub's api module merges it into its own registry.
var Default = NewRegistry(ops.Default)

// Handle registers the handler of a non-streaming operation. It fails when
// the operation is not in the catalog, is a streaming operation, or already
// has a handler.
func (r *Registry) Handle(name string, fn Handler, opts ...HandlerOption) error {
	if fn == nil {
		return fmt.Errorf("api: handler for %q is nil", name)
	}
	return r.add(name, entry{handler: fn}, false, opts)
}

// HandleStream registers the handler of a streaming operation (Op.Stream).
func (r *Registry) HandleStream(name string, fn StreamHandler, opts ...HandlerOption) error {
	if fn == nil {
		return fmt.Errorf("api: stream handler for %q is nil", name)
	}
	return r.add(name, entry{stream: fn}, true, opts)
}

func (r *Registry) add(name string, e entry, stream bool, opts []HandlerOption) error {
	op, ok := r.catalog.Lookup(name)
	if !ok {
		return fmt.Errorf("api: %q is not an operation of the catalog", name)
	}
	if op.Stream != stream {
		if op.Stream {
			return fmt.Errorf("api: %s is a streaming operation; register it with HandleStream", name)
		}
		return fmt.Errorf("api: %s is not a streaming operation; register it with Handle", name)
	}
	for _, o := range opts {
		o(&e)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("api: operation %q already has a handler", name)
	}
	r.byName[name] = e
	return nil
}

// Merge copies the handlers of from into r. It fails on a duplicate or on an
// operation r's catalog does not know; nothing is copied then.
func (r *Registry) Merge(from *Registry) error {
	from.mu.RLock()
	snapshot := make(map[string]entry, len(from.byName))
	for k, v := range from.byName {
		snapshot[k] = v
	}
	from.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range snapshot {
		if _, ok := r.catalog.Lookup(name); !ok {
			return fmt.Errorf("api: %q is not an operation of the catalog", name)
		}
		if _, dup := r.byName[name]; dup {
			return fmt.Errorf("api: operation %q already has a handler", name)
		}
	}
	for name, e := range snapshot {
		r.byName[name] = e
	}
	return nil
}

func (r *Registry) lookup(name string) (entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byName[name]
	return e, ok
}

// Names lists the operations that have a handler, unsorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	return out
}

// Handle registers a handler in the [Default] registry. It panics on error,
// so that a wrong registration fails at init.
func Handle(name string, fn Handler, opts ...HandlerOption) {
	if err := Default.Handle(name, fn, opts...); err != nil {
		panic(err)
	}
}

// HandleStream registers a stream handler in the [Default] registry and
// panics on error.
func HandleStream(name string, fn StreamHandler, opts ...HandlerOption) {
	if err := Default.HandleStream(name, fn, opts...); err != nil {
		panic(err)
	}
}

// HandleTyped registers fn for the operation name with typed arguments and
// result. It fails unless A is the operation's argument type and R its result
// type, so that a mismatch is found at startup and not on a request. fn gets
// the arguments by value.
func HandleTyped[A, R any](r *Registry, name string, fn func(ctx context.Context, c *Call, args A) (R, error), opts ...HandlerOption) error {
	op, ok := r.catalog.Lookup(name)
	if !ok {
		return fmt.Errorf("api: %q is not an operation of the catalog", name)
	}
	if op.Args != reflect.TypeFor[A]() {
		return fmt.Errorf("api: %s takes %s, not %s", name, op.Args, reflect.TypeFor[A]())
	}
	if op.Result != reflect.TypeFor[R]() {
		return fmt.Errorf("api: %s returns %s, not %s", name, op.Result, reflect.TypeFor[R]())
	}
	return r.Handle(name, func(ctx context.Context, c *Call) (any, error) {
		args, ok := c.Args.(*A)
		if !ok {
			return nil, fmt.Errorf("api: %s was called with %T", name, c.Args)
		}
		return fn(ctx, c, *args)
	}, opts...)
}

var (
	targetType    = reflect.TypeFor[ops.Target]()
	targetPtrType = reflect.TypeFor[*ops.Target]()
)

// DefaultResource derives the resource of an operation from its arguments:
//
//   - a Target field (the Worker ops, by value or by pointer; a nil pointer
//     names nothing) or a pair of worker_id and session_name members names a
//     session of a Worker;
//   - a worker_id member alone names the Worker;
//   - an operation of ScopeHub that names neither is decided on the caller's
//     own user resource: a global operation (accepted action manage) is then
//     allowed for admins only, and an operation that every user may call on
//     their own data is allowed for everyone;
//   - an operation of ScopeWorker that names no Worker fails closed as
//     invalid_argument (field worker_id), never on the caller's user
//     resource. A Worker op whose argument struct has no worker member is
//     routed by the worker_id member of the request itself (see
//     [Dispatcher]); this function sees only the arguments and therefore
//     refuses it.
//
// The worker_id must be a UUIDv7. The result of authz.Decide on a Worker or
// session resource follows ownership (AD-17): the owner and admins pass, and
// an API key additionally needs a scope on that Worker.
func DefaultResource(p authz.Principal, op ops.Op, args any) (authz.Resource, error) {
	return defaultResource(p, op, args, "")
}

// defaultResource is DefaultResource with the worker_id member of the
// request, which names the Worker of a ScopeWorker operation whose arguments
// carry no target.
func defaultResource(p authz.Principal, op ops.Op, args any, requestWorker string) (authz.Resource, error) {
	workerID, session, found := targetOf(args)
	if !found && op.Scope == ops.ScopeWorker && requestWorker != "" {
		workerID, found = requestWorker, true
	}
	if !found {
		if op.Scope == ops.ScopeWorker {
			return nil, invalidArg("worker_id", "is required")
		}
		if p.UserID == "" {
			return nil, NewError(proto.CodeForbidden, "not allowed")
		}
		return authz.UserResource{ID: p.UserID}, nil
	}
	if workerID == "" {
		return nil, invalidArg("worker_id", "is required")
	}
	if !ops.ValidUUIDv7(workerID) {
		return nil, invalidArg("worker_id", "must be a UUIDv7")
	}
	if session != "" {
		return authz.SessionResource{WorkerID: workerID, Name: session}, nil
	}
	return authz.WorkerResource{ID: workerID}, nil
}

// targetOf looks for the Worker and session an argument struct names.
func targetOf(args any) (workerID, session string, found bool) {
	v := reflect.ValueOf(args)
	for v.IsValid() && v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return "", "", false
	}
	if t, ok := findTarget(v); ok {
		return t.WorkerID, t.SessionName, true
	}
	var wid, sn *string
	collectStrings(v, &wid, &sn)
	if wid == nil {
		return "", "", false
	}
	if sn == nil {
		return *wid, "", true
	}
	return *wid, *sn, true
}

// findTarget returns the first non-nil ops.Target field, also in embedded
// structs. A nil *ops.Target is no target.
func findTarget(v reflect.Value) (ops.Target, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		switch {
		case f.Type == targetType && f.IsExported():
			return v.Field(i).Interface().(ops.Target), true
		case f.Type == targetPtrType && f.IsExported():
			if p := v.Field(i); !p.IsNil() {
				return p.Elem().Interface().(ops.Target), true
			}
		case f.Anonymous && f.Type.Kind() == reflect.Struct:
			if tg, ok := findTarget(v.Field(i)); ok {
				return tg, true
			}
		}
	}
	return ops.Target{}, false
}

func collectStrings(v reflect.Value, wid, sn **string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
			collectStrings(v.Field(i), wid, sn)
		case f.Type.Kind() == reflect.String && f.IsExported():
			s := v.Field(i).String()
			switch name {
			case "worker_id":
				*wid = &s
			case "session_name":
				*sn = &s
			}
		}
	}
}

// ResourceLabel returns a log and audit safe label of a resource: the user
// id, the Worker id, or "worker_id:session_name". It never holds content. The
// label is valid UTF-8 without control characters and at most
// ops.MaxAuditFieldLen characters, so that it is always a valid audit target.
func ResourceLabel(res authz.Resource) string {
	var s string
	switch r := res.(type) {
	case authz.UserResource:
		s = r.ID
	case authz.WorkerResource:
		s = r.ID
	case authz.SessionResource:
		s = r.WorkerID + ":" + r.Name
	}
	return auditSafe(s)
}

// auditSafe shortens and cleans s to a valid audit field value.
func auditSafe(s string) string {
	clean := utf8.ValidString(s) && utf8.RuneCountInString(s) <= ops.MaxAuditFieldLen
	if clean {
		for _, r := range s {
			if unicode.IsControl(r) {
				clean = false
				break
			}
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s { // an invalid byte ranges as U+FFFD, which is valid
		if n == ops.MaxAuditFieldLen {
			break
		}
		if unicode.IsControl(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
