package authz

import (
	"context"
	"slices"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The levels of a direct grant: a ladder, each level includes the lower ones.
const (
	// LevelView reads the target.
	LevelView = 1
	// LevelMessage reads the target and sends it messages.
	LevelMessage = 2
	// LevelControl reads, messages and operates the target.
	LevelControl = 3
)

// The authorities that can stand behind a direct grant.
const (
	// AuthorityOwner is a grant between two Workers of the same owner.
	AuthorityOwner = "owner"
	// AuthorityAdmin is a grant decided by an administrator (GrantedBy).
	AuthorityAdmin = "admin"
)

// DirectGrant is one stored grant of the calling session: it may reach the
// target at Level. The caller reads the grants of the calling (Worker,
// session) from the store for every call; Decide judges whether each one is
// valid now.
type DirectGrant struct {
	// ID is the stored id of the grant.
	ID string
	// TargetWorkerID is the Worker the grant reaches.
	TargetWorkerID string
	// TargetSession is the session it reaches; empty means every session of
	// the target Worker.
	TargetSession string
	// Level is LevelView, LevelMessage or LevelControl.
	Level int
	// Authority is AuthorityOwner or AuthorityAdmin.
	Authority string
	// GrantedBy is the user who decided the grant.
	GrantedBy string
	// SourceOwnerID and TargetOwnerID are the owners of the two Workers at
	// decision time. Both must still be the owners at every call.
	SourceOwnerID, TargetOwnerID string
	// ExpiresAt ends the grant: it is valid while the clock is before it. Nil
	// means the grant does not expire. A stored expiry is always a pointer,
	// also when it is the zero time (year one), which is long past.
	ExpiresAt *time.Time
	// Suspended is set when the authority behind the grant was lost.
	Suspended bool
}

// ExpiryAt returns the ExpiresAt of a grant that ends at t.
func ExpiryAt(t time.Time) *time.Time { return &t }

// LevelActions returns the actions of a direct-grant level: 1 read; 2 read and
// message; 3 read, message and operate. Any other level has no actions. A new
// slice on every call; never manage.
func LevelActions(level int) []ops.Action {
	switch level {
	case LevelView:
		return []ops.Action{ops.ActionRead}
	case LevelMessage:
		return []ops.Action{ops.ActionRead, ops.ActionMessage}
	case LevelControl:
		return []ops.Action{ops.ActionRead, ops.ActionMessage, ops.ActionOperate}
	}
	return nil
}

// directGrants is the calling session of a Worker identity with its direct
// grants. It is held by pointer so that Principal stays comparable.
type directGrants struct {
	session string
	grants  []DirectGrant
}

// NewWorkerSessionPrincipal returns the principal of session on Worker
// workerID. legacy is its session-control grant (nil when it has none) and is
// judged exactly as by [NewWorkerGrantPrincipal]; direct are the direct grants
// of that session. Without direct grants the principal decides exactly as
// NewWorkerGrantPrincipal(workerID, legacy).
func NewWorkerSessionPrincipal(workerID, session string, legacy Scope, direct []DirectGrant) Principal {
	return Principal{
		Kind: KindWorker, WorkerID: workerID, Scope: legacy,
		direct: &directGrants{session: session, grants: slices.Clone(direct)},
	}
}

// SessionName returns the calling session of a principal built with
// [NewWorkerSessionPrincipal], and "" for every other principal.
func (p Principal) SessionName() string {
	if p.direct == nil {
		return ""
	}
	return p.direct.session
}

// DirectGrants returns a copy of the direct grants the principal carries.
func (p Principal) DirectGrants() []DirectGrant {
	if p.direct == nil {
		return nil
	}
	return slices.Clone(p.direct.grants)
}

// directList is the direct grants of the principal, not copied.
func (p Principal) directList() []DirectGrant {
	if p.Kind != KindWorker || p.direct == nil {
		return nil
	}
	return p.direct.grants
}

// directUsable reports the part of the validity of g that does not depend on
// the resource or on the grantor: a known level, not suspended, not expired
// and still from the current owner of the calling Worker.
func (s subject) directUsable(g DirectGrant) bool {
	if LevelActions(g.Level) == nil || g.Suspended || g.SourceOwnerID == "" || g.SourceOwnerID != s.ownerID {
		return false
	}
	return g.ExpiresAt == nil || s.now.Before(*g.ExpiresAt)
}

// liveAdmins reads, once per grantor, whether the user behind each usable
// admin-authority direct grant of p is an active administrator now. A failing
// lookup is an error (fail closed).
func (s subject) liveAdmins(ctx context.Context, pt ports) (map[string]bool, error) {
	var live map[string]bool
	for _, g := range s.p.directList() {
		if g.Authority != AuthorityAdmin || !s.directUsable(g) {
			continue
		}
		if _, done := live[g.GrantedBy]; done {
			continue
		}
		u, found, err := user(ctx, pt, g.GrantedBy)
		if err != nil {
			return nil, err
		}
		if live == nil {
			live = map[string]bool{}
		}
		live[g.GrantedBy] = found && u.Active && u.Role == RoleAdmin
	}
	return live, nil
}

// directFor returns what direct grant g authorizes on res by itself, nil when
// it does not cover res or is not valid now (D3 to D5): the target must match,
// the grant must be usable, both owners must be the ones of decision time and
// the authority must still stand. The role of the calling Worker's owner does
// not limit it.
func (s subject) directFor(g DirectGrant, res ResolvedResource) []ops.Action {
	if res.Kind != ResourceSession && res.Kind != ResourceWorker {
		return nil
	}
	if g.TargetWorkerID == "" || g.TargetWorkerID != res.WorkerID {
		return nil
	}
	if res.Kind == ResourceSession && g.TargetSession != "" && g.TargetSession != res.SessionName {
		return nil
	}
	if !s.directUsable(g) || res.OwnerID == "" || res.OwnerID != g.TargetOwnerID {
		return nil
	}
	switch g.Authority {
	case AuthorityOwner:
		if g.SourceOwnerID != g.TargetOwnerID {
			return nil
		}
	case AuthorityAdmin:
		if !s.admins[g.GrantedBy] {
			return nil
		}
	default:
		return nil
	}
	if res.Kind == ResourceWorker {
		// Discovery only: a grant never messages or operates a Worker.
		return []ops.Action{ops.ActionRead}
	}
	return LevelActions(g.Level)
}

// directActions is the union of what the direct grants authorize on res.
func (s subject) directActions(res ResolvedResource) []ops.Action {
	var out []ops.Action
	for _, g := range s.p.directList() {
		out = append(out, s.directFor(g, res)...)
	}
	return out
}

// GrantIDs returns the ids of the direct grants of p that by themselves
// authorize an operation that accepts the given actions on res, in the order
// the principal carries them. It is empty for a principal without direct
// grants, for a principal without access and for a resource that does not
// exist; only a failure of a lookup is an error. The Hub calls it after
// [Authorizer.Decide] allowed the operation, for the audit event and the
// origin of the request. It reads the current state again, like Decide.
func (a *Authorizer) GrantIDs(ctx context.Context, p Principal, accepted ops.ActionSet, res Resource) ([]string, error) {
	if len(p.directList()) == 0 {
		return nil, nil
	}
	sub, resolved, ok, err := a.grantSubject(ctx, p, res)
	if err != nil || !ok {
		return nil, err
	}
	var ids []string
	for _, g := range p.directList() {
		if accepted.Permits(sub.directFor(g, resolved)...) {
			ids = append(ids, g.ID)
		}
	}
	return ids, nil
}
