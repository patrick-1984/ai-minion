package ops

import "reflect"

// Names of the act-as operations (AD-10, R45).
const (
	OpActAsStart = "act_as_start"
	OpActAsStop  = "act_as_stop"
)

// Limits of act_as_start.
const (
	// DefaultActAsMinutes is used when act_as_start.duration_minutes is 0
	// (AD-10: one hour).
	DefaultActAsMinutes = 60
	// MaxActAsMinutes is the longest act-as session (AD-10: eight hours).
	MaxActAsMinutes = 8 * 60
)

// Warning codes of act_as_stop.
const (
	// WarnNotActing says the browser session was not acting as anyone (it
	// expired already, or was stopped by another request), so there was
	// nothing to stop.
	WarnNotActing = "not_acting"
)

// Guards of the act-as operations. They are Hub guards: the Hub evaluates
// them before it changes anything.
var (
	// guardActAsTargetActive refuses a target that is unknown or deactivated.
	guardActAsTargetActive = Guard{Name: "target_active", Class: GuardKeep, Reads: "user.active"}
	// guardActAsNotSelf refuses an admin that names their own account.
	guardActAsNotSelf = Guard{Name: "target_not_self", Class: GuardKeep, Reads: "user.id"}
	// guardActAsNotNested refuses a second act-as in a session that is
	// already acting; the admin leaves first.
	guardActAsNotNested = Guard{Name: "not_acting", Class: GuardKeep, Reads: "session.act_as"}
	// guardActAsWasActing reports that there was nothing to stop.
	guardActAsWasActing = Guard{Name: "was_acting", Class: GuardWarn, Reads: "session.act_as"}
)

// ActAsStartArgs are the arguments of act_as_start.
type ActAsStartArgs struct {
	WriteArgs
	// UserID is the user to act as.
	UserID string `json:"user_id"`
	// DurationMinutes is how long the session lasts: 1 to MaxActAsMinutes.
	// 0 means DefaultActAsMinutes.
	DurationMinutes int `json:"duration_minutes,omitempty"`
}

// Validate checks the request id, the user id and the duration.
func (a ActAsStartArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.UserID) {
		return invalid("user_id", "must be a UUIDv7")
	}
	if a.DurationMinutes < 0 || a.DurationMinutes > MaxActAsMinutes {
		return invalid("duration_minutes", "must be between 1 and %d (0 means %d)", MaxActAsMinutes, DefaultActAsMinutes)
	}
	return nil
}

// EffectiveMinutes returns DurationMinutes, or DefaultActAsMinutes when it is 0.
func (a ActAsStartArgs) EffectiveMinutes() int {
	if a.DurationMinutes == 0 {
		return DefaultActAsMinutes
	}
	return a.DurationMinutes
}

// ActAsStopArgs are the arguments of act_as_stop. Stopping is idempotent, so
// request_id is optional here (the banner's Exit button posts "{}"); a value
// that is given must still be a UUIDv7.
type ActAsStopArgs struct {
	WriteArgs
}

// Validate checks request_id when it is present.
func (a ActAsStopArgs) Validate() error {
	if a.RequestID == "" {
		return nil
	}
	return a.WriteArgs.Validate()
}

func init() {
	Register(Op{
		Name:    OpActAsStart,
		Summary: "Act as another user in this browser session for a limited time (admin only, audited).",
		Scope:   ScopeHub, Actions: MustActionSet(ActionManage), Write: true,
		Guards: []Guard{guardActAsTargetActive, guardActAsNotSelf, guardActAsNotNested},
		Args:   reflect.TypeFor[ActAsStartArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	// operate on the caller's own user resource: while acting, the caller is
	// the target user, whose own permissions apply.
	Register(Op{
		Name:    OpActAsStop,
		Summary: "Stop acting as another user in this browser session.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardActAsWasActing},
		Args:   reflect.TypeFor[ActAsStopArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
