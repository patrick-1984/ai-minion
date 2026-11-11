package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Limits of an act-as session (AD-10).
const (
	// DefaultActAsDuration is used when no duration is given.
	DefaultActAsDuration = time.Hour
	// MaxActAsDuration is the longest act-as session.
	MaxActAsDuration = 8 * time.Hour
	// MinActAsDuration is the shortest one.
	MinActAsDuration = time.Minute
)

// Audit vocabulary of the automatic end of an act-as session (AD-20). Starting
// and stopping are write operations and are audited by the API dispatcher as
// act_as_start and act_as_stop; only the ends that nobody requests are
// recorded here, with actor and acting_as set like every other act-as event:
// act_as.expire when the period runs out, act_as.end when it is cut short
// because the target was deactivated or the actor is no longer an active admin.
const (
	AuditActionActAsExpire = "act_as.expire"
	AuditActionActAsEnd    = "act_as.end"
	auditOutcomeSuccess    = "success"
)

// Errors of [Manager.StartActAs]. Each one maps to one API code in the
// handler; none carries a user-supplied value.
var (
	// ErrActAsUnavailable means the Manager's store has no act-as support.
	ErrActAsUnavailable = errors.New("session: act-as is not available")
	// ErrActAsNotAdmin is returned when the session's user is not an active admin.
	ErrActAsNotAdmin = errors.New("session: only an active admin can act as another user")
	// ErrActAsActive is returned when the session is already acting as a user.
	ErrActAsActive = errors.New("session: already acting as a user; exit first")
	// ErrActAsSelf is returned when the admin names their own account.
	ErrActAsSelf = errors.New("session: an admin cannot act as themselves")
	// ErrActAsTargetUnknown is returned for a user that does not exist.
	ErrActAsTargetUnknown = errors.New("session: no such user")
	// ErrActAsTargetInactive is returned for a deactivated user.
	ErrActAsTargetInactive = errors.New("session: the user is deactivated")
	// ErrActAsTargetNoAccess is returned for a user without any effective
	// grant: acting as it would trap the admin session (F-07).
	ErrActAsTargetNoAccess = errors.New("session: the user has no access")
	// ErrActAsDuration is returned for a duration outside the allowed range.
	ErrActAsDuration = errors.New("session: the duration must be between 1 minute and 8 hours")
)

// ActAs is the act-as state of a browser session: the admin of the session
// acts as TargetID until ExpiresAt.
type ActAs struct {
	TargetID       string
	TargetUsername string
	StartedAt      time.Time
	ExpiresAt      time.Time
}

// Remaining returns the time left at now, never negative.
func (a ActAs) Remaining(now time.Time) time.Duration {
	if d := a.ExpiresAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

// MinutesLeft returns the time left at now in whole minutes, rounded up, so
// that the banner never shows "0 min left" while the session still works.
func (a ActAs) MinutesLeft(now time.Time) int {
	d := a.Remaining(now)
	return int((d + time.Minute - 1) / time.Minute)
}

// ActAsStore is the persistence port of act-as; *store.Store implements it.
// [NewManager] enables act-as when the Store it is given also implements this
// port.
type ActAsStore interface {
	StartActAs(ctx context.Context, row store.ActAsRow) error
	ActAsBySession(ctx context.Context, sessionID string) (store.ActAsInfo, error)
	EndActAs(ctx context.Context, sessionID string) (bool, error)
	EndActAsIf(ctx context.Context, row store.ActAsRow, audit store.AuditRow) (bool, error)
	UserByID(ctx context.Context, id string) (store.UserRow, error)
}

var _ ActAsStore = (*store.Store)(nil)

// WithActAsStore sets the act-as port explicitly; nil switches act-as off. It
// exists for tests, because [NewManager] detects the port itself.
func WithActAsStore(a ActAsStore) Option { return func(m *Manager) { m.actAs = a } }

// attachActAs sets s.ActAs when the session is acting as a user and the
// period is still valid. user is the session's user as Resolve just read it.
//
// A period ends on the spot, with an audit event written in the same
// transaction as the delete, and the session goes on as the plain session of
// its user, when
//   - it has expired (act_as.expire);
//   - the target has been deactivated, or has lost every effective grant
//     (act_as.end); or
//   - the session's user is no longer an active admin, for example after an
//     IdP demotion (act_as.end).
//
// Without the last two the principal would be denied every request, including
// act_as_stop (the target's user resource), and the Exit button would answer
// 403 until the period ran out.
//
// The delete matches the period that was read, so a request that read an old
// period cannot end (or audit) one that another request started since. A
// failed clean-up is not a failed request: the end is enforced here whatever
// the row says, and the next request retries.
//
// A store failure on the read is returned: a session that cannot be proven to
// be acting must not be served as the admin's own, so the request fails (the
// caller answers internal, never "logged out").
func (m *Manager) attachActAs(ctx context.Context, s *Session, row store.SessionRow, user store.UserRow) error {
	if m.actAs == nil {
		return nil
	}
	info, err := m.actAs.ActAsBySession(ctx, row.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.ActorID != row.UserID {
		return nil // not this session's admin: never trust it
	}
	now := m.now()
	expires := store.FromUnixMS(info.ExpiresAt)
	action := ""
	switch {
	case !now.Before(expires):
		action = AuditActionActAsExpire
	case !info.TargetActive || user.Role != string(authz.RoleAdmin):
		action = AuditActionActAsEnd
	default:
		// A target without any effective grant is denied everything, act_as_stop
		// included (F-07): end the period. A failing read keeps it.
		if t, err := m.actAs.UserByID(ctx, info.TargetID); err == nil && t.Role == "" {
			action = AuditActionActAsEnd
		}
	}
	if action != "" {
		_, _ = m.actAs.EndActAsIf(ctx, info.ActAsRow, store.AuditRow{
			ID: m.newID(), TS: store.UnixMS(now), Actor: info.ActorID, ActingAs: info.TargetID,
			Action: action, Target: info.TargetID, Outcome: auditOutcomeSuccess,
		})
		return nil
	}
	s.ActAs = &ActAs{
		TargetID: info.TargetID, TargetUsername: info.TargetUsername,
		StartedAt: store.FromUnixMS(info.StartedAt), ExpiresAt: expires,
	}
	return nil
}

// StartActAs makes the browser session s act as targetID for d (0 means
// [DefaultActAsDuration]) and returns the new state. s must be a session that
// [Manager.Resolve] returned. The Manager checks again, against the store,
// that the session's user is an active admin; nobody else can start.
func (m *Manager) StartActAs(ctx context.Context, s *Session, targetID string, d time.Duration) (ActAs, error) {
	if m.actAs == nil {
		return ActAs{}, ErrActAsUnavailable
	}
	if d == 0 {
		d = DefaultActAsDuration
	}
	if d < MinActAsDuration || d > MaxActAsDuration {
		return ActAs{}, ErrActAsDuration
	}
	if s.ActAs != nil {
		return ActAs{}, ErrActAsActive
	}
	actor, err := m.actAs.UserByID(ctx, s.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return ActAs{}, ErrActAsNotAdmin
	}
	if err != nil {
		return ActAs{}, err
	}
	if !actor.Active || actor.Role != string(authz.RoleAdmin) {
		return ActAs{}, ErrActAsNotAdmin
	}
	if targetID == s.UserID {
		return ActAs{}, ErrActAsSelf
	}
	target, err := m.actAs.UserByID(ctx, targetID)
	if errors.Is(err, store.ErrNotFound) {
		return ActAs{}, ErrActAsTargetUnknown
	}
	if err != nil {
		return ActAs{}, err
	}
	if !target.Active {
		return ActAs{}, ErrActAsTargetInactive
	}
	if target.Role == "" {
		return ActAs{}, ErrActAsTargetNoAccess
	}
	now := m.now()
	row := store.ActAsRow{
		SessionID: s.ID, ActorID: actor.ID, TargetID: target.ID,
		StartedAt: store.UnixMS(now), ExpiresAt: store.UnixMS(now.Add(d)),
	}
	if err := m.actAs.StartActAs(ctx, row); err != nil {
		if errors.Is(err, store.ErrActAsActive) {
			return ActAs{}, ErrActAsActive
		}
		return ActAs{}, fmt.Errorf("session: start act-as: %w", err)
	}
	return ActAs{
		TargetID: target.ID, TargetUsername: target.Username,
		StartedAt: store.FromUnixMS(row.StartedAt), ExpiresAt: store.FromUnixMS(row.ExpiresAt),
	}, nil
}

// StopActAs ends the act-as of the browser session s. It reports whether the
// session was acting; stopping a session that is not acting (it expired, or
// another request stopped it) is not an error. The API dispatcher audits the
// stop as the write operation act_as_stop.
func (m *Manager) StopActAs(ctx context.Context, s *Session) (bool, error) {
	if m.actAs == nil {
		return false, ErrActAsUnavailable
	}
	ended, err := m.actAs.EndActAs(ctx, s.ID)
	if err != nil {
		return false, fmt.Errorf("session: stop act-as: %w", err)
	}
	return ended, nil
}

// ErrPasswordChangeWhileActing is the refusal of a password change made while
// acting as another user (AD-10: act-as cannot change the target user's
// password).
var ErrPasswordChangeWhileActing = proto.NewError(proto.CodeForbidden,
	"a password cannot be changed while acting as another user")

// CheckPasswordChange is the guard of every operation that changes a password
// while the caller may be acting: password_change and user_reset_password
// must both call it. It refuses an act-as principal, whose user is the target,
// and allows everybody else. user_reset_password is not exempt: an act-as
// principal whose target is an admin holds manage on the target's user
// resource, so a reset would change the target's password, which AD-10
// forbids while acting.
func CheckPasswordChange(p authz.Principal) error {
	if p.Kind == authz.KindActAs {
		return ErrPasswordChangeWhileActing
	}
	return nil
}

// TrustUserID returns the user whose trust rows decide whether a model reads
// output as trusted for p (AD-17: trust is evaluated for the actor, never for
// the user an admin acts as). For an act-as principal that is the admin; for
// any other principal it is the user of the principal. A Worker identity has
// no user and yields "".
func TrustUserID(p authz.Principal) string {
	if p.Kind == authz.KindActAs {
		return p.ActorID
	}
	return p.UserID
}
