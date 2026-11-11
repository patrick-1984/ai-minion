package authz

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned by a [UserLookup] or [WorkerLookup] for an unknown user or Worker.
var ErrNotFound = errors.New("authz: not found")

// Role is the role of a user (AD-10).
type Role string

// The two roles.
const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// UserState is the current state of a user, as stored.
type UserState struct {
	// Role is the effective role (local and IdP grants already combined).
	Role Role
	// Active is false for a deactivated account (AD-10: account deletion
	// means deactivation).
	Active bool
}

// hasAccess reports whether the account may act at all: it is active and holds
// at least one effective grant (role user or admin). An account without any
// grant has no access for any principal kind (F-07).
func (u UserState) hasAccess() bool {
	return u.Active && (u.Role == RoleUser || u.Role == RoleAdmin)
}

// UserLookup is the port to the users table. Every call reads current state;
// an implementation must not cache positive results, because Decide relies on
// the answer being fresh (AD-11). Story 3.5 (store/users.go, mod_auth.go)
// provides the SQL adapter and wires it with [Authorizer.SetUserLookup].
type UserLookup interface {
	// User returns the current state of a user, or [ErrNotFound].
	User(ctx context.Context, userID string) (UserState, error)
}

// WorkerLookup is the port to the workers table. It has the same freshness
// contract as [UserLookup]: no positive cache. Story 4.3 (store/workers.go,
// mod_workers.go) provides the SQL adapter and wires it with
// [Authorizer.SetWorkerLookup].
type WorkerLookup interface {
	// WorkerOwner returns the current owner_id of a Worker, or [ErrNotFound]
	// when the Worker does not exist or was removed.
	WorkerOwner(ctx context.Context, workerID string) (string, error)
}

// ErrNotWired is returned by the placeholder lookups of an Authorizer that has
// not been wired yet. It makes every decision fail closed.
var ErrNotWired = errors.New("authz: lookup not wired yet")

// notWired is the fail-closed placeholder for both ports.
type notWired struct{}

func (notWired) User(context.Context, string) (UserState, error) {
	return UserState{}, ErrNotWired
}

func (notWired) WorkerOwner(context.Context, string) (string, error) {
	return "", ErrNotWired
}

// ports is the pair of lookups used for one call.
type ports struct {
	users   UserLookup
	workers WorkerLookup
}

// ResourceKind is the type of a resource.
type ResourceKind string

// The resource kinds.
const (
	ResourceUser    ResourceKind = "user"
	ResourceWorker  ResourceKind = "worker"
	ResourceSession ResourceKind = "session"
)

// ResolvedResource is a resource with its owner looked up. A Scope sees this
// form.
type ResolvedResource struct {
	Kind ResourceKind
	// UserID is set for a user resource.
	UserID string
	// WorkerID is set for a Worker and a session resource.
	WorkerID string
	// SessionName is set for a session resource.
	SessionName string
	// OwnerID is the user that owns the resource: the user itself for a user
	// resource, the Worker's owner for a Worker and its sessions.
	OwnerID string
}

// Resource is something a principal acts on: [UserResource], [WorkerResource]
// or [SessionResource].
type Resource interface {
	resolve(ctx context.Context, p ports) (ResolvedResource, error)
}

// UserResource is a user account.
type UserResource struct{ ID string }

// WorkerResource is a Worker. Its owner is its owner_id (AD-17).
type WorkerResource struct{ ID string }

// SessionResource is a session of a Worker. It belongs to the Worker's owner;
// a session has no owner of its own.
type SessionResource struct {
	WorkerID string
	Name     string
}

func (r UserResource) resolve(ctx context.Context, p ports) (ResolvedResource, error) {
	if r.ID == "" {
		return ResolvedResource{}, fmt.Errorf("%w: user resource without id", ErrNotFound)
	}
	if _, err := p.users.User(ctx, r.ID); err != nil {
		return ResolvedResource{}, err
	}
	return ResolvedResource{Kind: ResourceUser, UserID: r.ID, OwnerID: r.ID}, nil
}

func (r WorkerResource) resolve(ctx context.Context, p ports) (ResolvedResource, error) {
	if r.ID == "" {
		return ResolvedResource{}, fmt.Errorf("%w: worker resource without id", ErrNotFound)
	}
	owner, err := p.workers.WorkerOwner(ctx, r.ID)
	if err != nil {
		return ResolvedResource{}, err
	}
	return ResolvedResource{Kind: ResourceWorker, WorkerID: r.ID, OwnerID: owner}, nil
}

func (r SessionResource) resolve(ctx context.Context, p ports) (ResolvedResource, error) {
	if r.WorkerID == "" || r.Name == "" {
		return ResolvedResource{}, fmt.Errorf("%w: session resource without worker or name", ErrNotFound)
	}
	owner, err := p.workers.WorkerOwner(ctx, r.WorkerID)
	if err != nil {
		return ResolvedResource{}, err
	}
	return ResolvedResource{Kind: ResourceSession, WorkerID: r.WorkerID, SessionName: r.Name, OwnerID: owner}, nil
}

// memoWorkers answers repeated owner lookups of one call from memory. It lives
// for a single Filter call only, so it is not a positive cache across calls.
type memoWorkers struct {
	WorkerLookup
	owners map[string]string
}

func newMemoWorkers(l WorkerLookup) *memoWorkers {
	return &memoWorkers{WorkerLookup: l, owners: map[string]string{}}
}

func (m *memoWorkers) WorkerOwner(ctx context.Context, workerID string) (string, error) {
	if o, ok := m.owners[workerID]; ok {
		return o, nil
	}
	o, err := m.WorkerLookup.WorkerOwner(ctx, workerID)
	if err != nil {
		return "", err
	}
	m.owners[workerID] = o
	return o, nil
}
