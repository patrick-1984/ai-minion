package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ActAsController is the port of the act-as handlers to the browser sessions;
// *session.Manager implements it.
type ActAsController interface {
	StartActAs(ctx context.Context, s *session.Session, targetID string, d time.Duration) (session.ActAs, error)
	StopActAs(ctx context.Context, s *session.Session) (bool, error)
}

var _ ActAsController = (*session.Manager)(nil)

// RegisterActAs registers the handlers of act_as_start and act_as_stop (Story
// 6.5, R45) in r. The Hub's act-as module calls it with the session manager.
//
// Act-as belongs to a browser session: an API key or an act-as principal
// cannot start it. act_as_start is decided as manage on the target user, so
// only admins pass (and not while acting: an act-as principal holds the
// target's permissions, not the admin's). act_as_stop is decided as operate on
// the caller's own user resource, which is the target user while acting.
func RegisterActAs(r *Registry, c ActAsController) error {
	if c == nil {
		return errors.New("api: RegisterActAs needs a controller")
	}
	if err := HandleTyped(r, ops.OpActAsStart, func(ctx context.Context, call *Call, args ops.ActAsStartArgs) (ops.WriteResult, error) {
		return actAsStart(ctx, c, call, args)
	}, WithResource(actAsStartResource)); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpActAsStop, func(ctx context.Context, call *Call, _ ops.ActAsStopArgs) (ops.WriteResult, error) {
		return actAsStop(ctx, c, call)
	}, WithResource(actAsStopResource))
}

// actAsStartResource names the user to act as, so that the decision and the
// audit target are that user.
func actAsStartResource(_ authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.ActAsStartArgs)
	if !ok {
		return nil, fmt.Errorf("api: act_as_start was called with %T", args)
	}
	return authz.UserResource{ID: a.UserID}, nil
}

// actAsStopResource is the caller's own user resource.
func actAsStopResource(p authz.Principal, _ any) (authz.Resource, error) {
	if p.UserID == "" {
		return nil, NewError(proto.CodeForbidden, "not allowed")
	}
	return authz.UserResource{ID: p.UserID}, nil
}

// browserSession returns the browser session of a call, or a forbidden error:
// act-as is a state of a browser session.
func browserSession(call *Call) (*session.Session, error) {
	if call.Identity.Via != ViaSession || call.Identity.Session == nil {
		return nil, NewError(proto.CodeForbidden, "act-as works in a signed-in browser session only")
	}
	return call.Identity.Session, nil
}

func actAsStart(ctx context.Context, c ActAsController, call *Call, args ops.ActAsStartArgs) (ops.WriteResult, error) {
	sess, err := browserSession(call)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if _, err := c.StartActAs(ctx, sess, args.UserID, time.Duration(args.EffectiveMinutes())*time.Minute); err != nil {
		return ops.WriteResult{}, actAsError(err)
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}

func actAsStop(ctx context.Context, c ActAsController, call *Call) (ops.WriteResult, error) {
	sess, err := browserSession(call)
	if err != nil {
		return ops.WriteResult{}, err
	}
	ended, err := c.StopActAs(ctx, sess)
	if err != nil {
		return ops.WriteResult{}, actAsError(err)
	}
	res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
	if !ended {
		res.Warnings = []ops.Warning{{Code: ops.WarnNotActing, Message: "this session was not acting as another user"}}
	}
	return res, nil
}

// actAsError maps the errors of the session package to API errors; anything
// else is internal and keeps its cause out of the answer.
func actAsError(err error) error {
	switch {
	case errors.Is(err, session.ErrActAsNotAdmin):
		return NewError(proto.CodeForbidden, "only an admin can act as another user")
	case errors.Is(err, session.ErrActAsSelf):
		return invalidArg("user_id", "an admin cannot act as themselves")
	case errors.Is(err, session.ErrActAsDuration):
		return invalidArg("duration_minutes", "must be between 1 and 480")
	case errors.Is(err, session.ErrActAsTargetUnknown):
		return NewError(proto.CodeNotFound, "not found")
	case errors.Is(err, session.ErrActAsTargetInactive):
		return NewError(proto.CodeConflict, "the user is deactivated")
	case errors.Is(err, session.ErrActAsTargetNoAccess):
		return NewError(proto.CodeConflict, "the user has no access")
	case errors.Is(err, session.ErrActAsActive):
		return NewError(proto.CodeConflict, "already acting as a user; exit first")
	case errors.Is(err, session.ErrActAsUnavailable):
		return NewError(proto.CodeInternal, "act-as is not available")
	default:
		return err
	}
}
