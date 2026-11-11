package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Authorizer is the single decision point of the Hub (AD-10, AD-11). It is
// safe for concurrent use when its lookups are.
//
// The user and Worker lookups belong to different modules (users to the auth
// module, Workers to the workers module), so an Authorizer can be built before
// both exist: [NewUnwired] starts with fail-closed placeholders and the two
// Set methods wire the real ports, once each.
type Authorizer struct {
	mu         sync.RWMutex
	users      UserLookup
	workers    WorkerLookup
	usersSet   bool
	workersSet bool

	now func() time.Time
}

// Option configures an [Authorizer].
type Option func(*Authorizer)

// WithClock sets the clock that ends act-as sessions. Tests use it; the
// default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(a *Authorizer) { a.now = now }
}

// New returns an Authorizer that reads current state through users and
// workers. Both are required; use [NewUnwired] when they are not available
// yet.
func New(users UserLookup, workers WorkerLookup, opts ...Option) *Authorizer {
	if users == nil || workers == nil {
		panic("authz: New needs a UserLookup and a WorkerLookup")
	}
	a := NewUnwired(opts...)
	a.users, a.usersSet = users, true
	a.workers, a.workersSet = workers, true
	return a
}

// NewUnwired returns an Authorizer whose lookups fail with [ErrNotWired]: every
// Decide, EffectiveActions and Filter call fails closed until
// [Authorizer.SetUserLookup] and [Authorizer.SetWorkerLookup] have been called.
func NewUnwired(opts ...Option) *Authorizer {
	a := &Authorizer{users: notWired{}, workers: notWired{}, now: time.Now}
	for _, o := range opts {
		o(a)
	}
	return a
}

// SetUserLookup wires the user port. Story 3.5 calls it from the init of its
// module. It can be called once; a nil lookup and a second call are errors.
func (a *Authorizer) SetUserLookup(l UserLookup) error {
	if l == nil {
		return errors.New("authz: SetUserLookup needs a lookup")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usersSet {
		return errors.New("authz: the user lookup is already set")
	}
	a.users, a.usersSet = l, true
	return nil
}

// SetWorkerLookup wires the Worker port. Story 4.3 calls it from the init of
// its module. It can be called once; a nil lookup and a second call are errors.
func (a *Authorizer) SetWorkerLookup(l WorkerLookup) error {
	if l == nil {
		return errors.New("authz: SetWorkerLookup needs a lookup")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.workersSet {
		return errors.New("authz: the worker lookup is already set")
	}
	a.workers, a.workersSet = l, true
	return nil
}

// ports returns the lookups to use for one call.
func (a *Authorizer) ports() ports {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return ports{users: a.users, workers: a.workers}
}

// allActions is what an admin holds on every resource.
var allActions = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

// ownerActions is what the owner of a resource holds. manage is global and
// admin only, so the owner does not hold it.
var ownerActions = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}

// workerActions is what a Worker identity holds on its own Worker and that
// Worker's sessions. It never holds operate or manage.
var workerActions = []ops.Action{ops.ActionRead, ops.ActionMessage}

// Decide checks that principal p may perform an operation that accepts the
// given actions (any one suffices) on res. It returns nil when allowed.
// Otherwise it returns a *proto.Error: not_found when p holds no action at all
// on res or res does not exist (single-resource reads leak no existence), and
// forbidden when p holds some other action but none that is accepted. A
// failure of a lookup is returned as is and denies.
func (a *Authorizer) Decide(ctx context.Context, p Principal, accepted ops.ActionSet, res Resource) error {
	pt := a.ports()
	sub, ok, err := a.subject(ctx, p, pt)
	if err != nil {
		return err
	}
	if !ok {
		return proto.NewError(proto.CodeForbidden, "not allowed")
	}
	resolved, err := res.resolve(ctx, pt)
	if err != nil {
		return mapLookupErr(err)
	}
	held := sub.actions(resolved)
	if accepted.Permits(held...) {
		return nil
	}
	if len(held) == 0 {
		return proto.NewError(proto.CodeNotFound, "not found")
	}
	return proto.NewError(proto.CodeForbidden, "not allowed")
}

// Valid reports whether p still has any permission at all (see subject):
// its account is active and holds a grant, its act-as session is live, its
// Worker exists and its owner has access. It reads the current state.
func (a *Authorizer) Valid(ctx context.Context, p Principal) (bool, error) {
	_, ok, err := a.subject(ctx, p, a.ports())
	return ok, err
}

// EffectiveActions returns the actions p currently holds on res, sorted. It is
// the set to put into op.request.actions (AD-12); the Worker makes the final
// message versus operate check. A principal without access, or a resource that
// does not exist, yields an empty set without an error. Only a failure of the
// lookup is an error.
func (a *Authorizer) EffectiveActions(ctx context.Context, p Principal, res Resource) (ops.ActionSet, error) {
	pt := a.ports()
	sub, ok, err := a.subject(ctx, p, pt)
	if err != nil || !ok {
		return ops.ActionSet{}, err
	}
	resolved, err := res.resolve(ctx, pt)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ops.ActionSet{}, nil
		}
		return nil, fmt.Errorf("authz: resolve resource: %w", err)
	}
	return sub.actions(resolved), nil
}

func mapLookupErr(err error) error {
	if errors.Is(err, ErrNotFound) {
		return proto.NewError(proto.CodeNotFound, "not found")
	}
	return fmt.Errorf("authz: resolve resource: %w", err)
}

// subject is a principal whose user state was read for one call.
type subject struct {
	p    Principal
	role Role
	// ownerID is the owner of a Worker identity (KindWorker only).
	ownerID string
}

// subject reads the current state behind p. ok is false when p has no
// permission at all: malformed, deactivated, unknown user, an act-as session
// that expired or whose actor is no longer an active admin, a Worker identity
// whose Worker was removed or whose owner is missing or deactivated. Every
// call reads the store again; nothing is cached.
func (a *Authorizer) subject(ctx context.Context, p Principal, pt ports) (sub subject, ok bool, err error) {
	if p.validate() != nil {
		return subject{}, false, nil
	}
	if p.Kind == KindWorker {
		return a.workerSubject(ctx, p, pt)
	}
	if p.Kind == KindActAs {
		if !a.now().Before(p.ActAsExpires) {
			return subject{}, false, nil
		}
		actor, found, err := user(ctx, pt, p.ActorID)
		if err != nil || !found || !actor.Active || actor.Role != RoleAdmin {
			return subject{}, false, err
		}
	}
	u, found, err := user(ctx, pt, p.UserID)
	if err != nil || !found || !u.hasAccess() {
		return subject{}, false, err
	}
	return subject{p: p, role: u.Role}, true, nil
}

// workerSubject resolves a Worker identity: the Worker must exist and its
// owner must be an active user. The identity then acts with the permission of a
// plain owner, whatever the owner's role.
func (a *Authorizer) workerSubject(ctx context.Context, p Principal, pt ports) (subject, bool, error) {
	owner, err := pt.workers.WorkerOwner(ctx, p.WorkerID)
	if errors.Is(err, ErrNotFound) {
		return subject{}, false, nil
	}
	if err != nil {
		return subject{}, false, fmt.Errorf("authz: look up worker owner: %w", err)
	}
	u, found, err := user(ctx, pt, owner)
	if err != nil || !found || !u.Active || (u.Role != RoleUser && u.Role != RoleAdmin) {
		return subject{}, false, err
	}
	return subject{p: p, ownerID: owner}, true, nil
}

func user(ctx context.Context, pt ports, id string) (UserState, bool, error) {
	u, err := pt.users.User(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return UserState{}, false, nil
	}
	if err != nil {
		return UserState{}, false, fmt.Errorf("authz: look up user: %w", err)
	}
	return u, true, nil
}

// actions computes the permission of the subject on a resolved resource.
func (s subject) actions(res ResolvedResource) ops.ActionSet {
	if s.p.Kind == KindWorker {
		// read and message on the Worker itself and on that Worker's sessions,
		// nothing else (F-01). A Worker identity has no reach to other Workers
		// of the same owner, user resources, or the operate and manage
		// actions. Cross-Worker use takes a Worker-bound API key with a scope
		// (AD-12, call_api_key_file) and the receiver's pipe_accept_from.
		if res.Kind != ResourceUser && res.WorkerID == s.p.WorkerID && res.OwnerID == s.ownerID {
			return canonical(workerActions)
		}
		return ops.ActionSet{}
	}
	var base []ops.Action
	switch {
	case s.role == RoleAdmin:
		base = allActions
	case s.role == RoleUser && res.OwnerID == s.p.UserID:
		base = ownerActions
	default:
		return ops.ActionSet{}
	}
	if s.p.Kind != KindAPIKey {
		return canonical(base)
	}
	// An API key gets scope ∩ the owner's current permissions. A Worker
	// binding (Principal.WorkerID) is no limit on the resource: it only
	// restricts the key to that Worker's call streams, which the
	// authentication layer enforces (AD-11, Story 6.1), so a bound key may
	// reach a session on another Worker when scope and owner allow it.
	granted := s.p.Scope.Actions(res)
	var eff []ops.Action
	for _, act := range base {
		if slices.Contains(granted, act) {
			eff = append(eff, act)
		}
	}
	return canonical(eff)
}

// canonical returns a sorted, duplicate free copy as an ActionSet (possibly
// empty, which ops.NewActionSet would refuse).
func canonical(in []ops.Action) ops.ActionSet {
	out := slices.Clone(in)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		return ops.ActionSet{}
	}
	return ops.ActionSet(out)
}
