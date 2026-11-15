package authz

import (
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Kind is the type of a principal (AD-10).
type Kind string

// The four principal kinds.
const (
	// KindBrowserSession is a user with a browser session cookie.
	KindBrowserSession Kind = "browser_session"
	// KindAPIKey is a user acting through an API key. It always carries a
	// Scope and may be bound to one Worker identity, which restricts the call
	// streams it is accepted on and not the resources it reaches (AD-11).
	KindAPIKey Kind = "api_key"
	// KindActAs is an admin acting as another user for a limited time.
	KindActAs Kind = "act_as"
	// KindWorker is a Worker identity on its call streams. It holds read and
	// message on its own Worker and that Worker's sessions, resolved fresh on
	// every call. A session-control grant (Scope, see
	// [NewWorkerGrantPrincipal]) adds the owner's base permission ∩ Scope.
	KindWorker Kind = "worker"
)

// Scope limits what an API key may do (AD-11). The scope tells which actions it
// grants on a resource; Decide intersects that with the owner's current
// permissions. Story 6.1 implements Scope for the stored key scopes
// (everything, chosen users, chosen Workers, chosen sessions).
type Scope interface {
	// Actions returns the actions the scope grants on the resource. A scope
	// that does not cover the resource returns nil. The result never widens
	// what the owner may do.
	Actions(res ResolvedResource) []ops.Action
}

// Principal is who makes a request. It holds identifiers only; the current
// role and activity of the user are looked up on every decision. Build one with
// the New...Principal constructors.
type Principal struct {
	// Kind is the principal type.
	Kind Kind
	// UserID is the user whose permissions apply: the session user, the key
	// owner or, for act-as, the target user. Empty for a Worker identity.
	UserID string
	// ActorID is the admin who acts as UserID (act-as only).
	ActorID string
	// ActAsExpires ends an act-as session (AD-10: 1 h by default, 8 h at most).
	ActAsExpires time.Time
	// WorkerID is the Worker identity (KindWorker), or the Worker an API key
	// is bound to (KindAPIKey, optional). A binding limits the channel, not the
	// resources: the authentication layer accepts a bound key only on that
	// Worker's call streams (AD-11, Story 6.1), and Decide treats the key like
	// any other key (scope ∩ the owner's current permissions).
	WorkerID string
	// Scope limits an API key (required for KindAPIKey). For KindWorker it is
	// the optional session-control grant of the calling session: nil means no
	// grant, and a grant adds the owner's base permission ∩ Scope to the
	// own-Worker rule. It is ignored for the other kinds.
	Scope Scope
	// KeyID is the stored id of the API key (KindAPIKey), for the origin of the
	// requests it makes. It is set by the key service; it grants nothing.
	KeyID string
}

// NewSessionPrincipal returns the principal of a browser session of userID.
func NewSessionPrincipal(userID string) Principal {
	return Principal{Kind: KindBrowserSession, UserID: userID}
}

// NewAPIKeyPrincipal returns the principal of an API key owned by ownerID with
// the given scope. boundWorkerID is the Worker the key is bound to, or empty.
func NewAPIKeyPrincipal(ownerID string, scope Scope, boundWorkerID string) Principal {
	return Principal{Kind: KindAPIKey, UserID: ownerID, Scope: scope, WorkerID: boundWorkerID}
}

// WithKeyID returns p with the stored id of the API key it was built from.
func (p Principal) WithKeyID(id string) Principal { p.KeyID = id; return p }

// NewActAsPrincipal returns the principal of admin actorID acting as targetID
// until expires.
func NewActAsPrincipal(actorID, targetID string, expires time.Time) Principal {
	return Principal{Kind: KindActAs, ActorID: actorID, UserID: targetID, ActAsExpires: expires}
}

// NewWorkerPrincipal returns the principal of a Worker identity.
func NewWorkerPrincipal(workerID string) Principal {
	return Principal{Kind: KindWorker, WorkerID: workerID}
}

// NewWorkerGrantPrincipal returns the principal of a Worker identity whose
// calling session holds a session-control grant. scope lists the grant's
// targets with the actions of [GrantActions]; the caller builds it from the
// stored targets and passes nil when the session has no grant, which is the
// same as [NewWorkerPrincipal].
func NewWorkerGrantPrincipal(workerID string, scope Scope) Principal {
	return Principal{Kind: KindWorker, WorkerID: workerID, Scope: scope}
}

// GrantActions returns the fixed actions of a session-control grant: read,
// operate and message, never manage. A new slice on every call.
func GrantActions() []ops.Action {
	return []ops.Action{ops.ActionMessage, ops.ActionOperate, ops.ActionRead}
}

// errInvalidPrincipal marks a principal that is malformed for its kind.
var errInvalidPrincipal = errors.New("authz: malformed principal")

// validate reports whether the principal has the fields its kind needs. A
// malformed principal is denied (fail closed).
func (p Principal) validate() error {
	switch p.Kind {
	case KindBrowserSession:
		if p.UserID == "" {
			return errInvalidPrincipal
		}
	case KindAPIKey:
		if p.UserID == "" || p.Scope == nil {
			return errInvalidPrincipal
		}
	case KindActAs:
		if p.UserID == "" || p.ActorID == "" || p.ActAsExpires.IsZero() {
			return errInvalidPrincipal
		}
	case KindWorker:
		if p.WorkerID == "" {
			return errInvalidPrincipal
		}
	default:
		return errInvalidPrincipal
	}
	return nil
}
