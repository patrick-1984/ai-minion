package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// InvitationService is what the invitation handlers need from the Hub;
// *workers.Invitations implements it.
type InvitationService interface {
	Create(ctx context.Context, caller workers.InvitationCaller, a ops.InvitationCreateArgs) (ops.InvitationCreateResult, error)
	List(ctx context.Context, caller workers.InvitationCaller, all bool) (ops.InvitationListResult, error)
	Revoke(ctx context.Context, caller workers.InvitationCaller, id string) error
}

var _ InvitationService = (*workers.Invitations)(nil)

// RegisterInvitationHandlers registers invitation_create, invitation_list and
// invitation_revoke (R91) on r. The dispatcher has already authorised the
// caller on their own user resource (the owner and admins pass); dec is asked
// again for the manage action to tell admins apart: an infrastructure key and
// the list of every user's keys are admin-only. An API key carries a scope, so
// its reach over other users is decided per creator (manage on that user's
// resource) instead of being assumed from its owner's role, and only a key
// whose scope covers the whole Hub creates infrastructure keys.
func RegisterInvitationHandlers(r *Registry, svc InvitationService, dec Decider) error {
	if svc == nil || dec == nil {
		return errors.New("api: the invitation handlers need a service and a decider")
	}
	h := invitationHandlers{svc: svc, dec: dec}
	if err := HandleTyped(r, ops.OpInvitationCreate, h.create); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpInvitationList, h.list); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpInvitationRevoke, h.revoke)
}

type invitationHandlers struct {
	svc InvitationService
	dec Decider
}

// caller builds the service caller from the authorised request.
func (h invitationHandlers) caller(ctx context.Context, c *Call) (workers.InvitationCaller, error) {
	p := c.Principal
	if p.UserID == "" {
		return workers.InvitationCaller{}, NewError(proto.CodeForbidden, "not allowed")
	}
	admin, err := h.isAdmin(ctx, p)
	if err != nil {
		return workers.InvitationCaller{}, err
	}
	caller := workers.InvitationCaller{
		UserID: p.UserID, Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs, Admin: admin,
	}
	if admin && p.Kind == authz.KindAPIKey {
		// The admin check above looked at the key owner's own resource only.
		// The key keeps Hub-wide admin rights only when its scope also grants
		// manage on a user it was never given; otherwise it reaches the keys
		// of the creators its scope names and nothing else.
		caller.Admin = holdsHubWideManage(p)
		if !caller.Admin {
			caller.Manages = func(ctx context.Context, creatorID string) (bool, error) {
				return h.manages(ctx, p, creatorID)
			}
		}
	}
	return caller, nil
}

// holdsHubWideManage reports whether an API key's scope grants manage on a
// user chosen at random, which no scope limited to chosen users, Workers or
// sessions does (the "everything" scope does).
func holdsHubWideManage(p authz.Principal) bool {
	if p.Scope == nil {
		return false
	}
	id := ops.NewUUIDv7()
	probe := authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
	return slices.Contains(p.Scope.Actions(probe), ops.ActionManage)
}

// manages reports whether p holds manage on the resource of the given user.
func (h invitationHandlers) manages(ctx context.Context, p authz.Principal, userID string) (bool, error) {
	return h.holdsManage(ctx, p, authz.UserResource{ID: userID})
}

// isAdmin reports whether p holds the admin-only manage action. A refusal
// means "not an admin"; only a failure of the decision itself is an error.
func (h invitationHandlers) isAdmin(ctx context.Context, p authz.Principal) (bool, error) {
	return h.holdsManage(ctx, p, authz.UserResource{ID: p.UserID})
}

func (h invitationHandlers) holdsManage(ctx context.Context, p authz.Principal, res authz.Resource) (bool, error) {
	err := h.dec.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), res)
	if err == nil {
		return true, nil
	}
	if e := ErrorOf(err); e.Code == proto.CodeForbidden || e.Code == proto.CodeNotFound {
		return false, nil
	}
	return false, fmt.Errorf("api: decide the admin permission: %w", err)
}

func (h invitationHandlers) create(ctx context.Context, c *Call, a ops.InvitationCreateArgs) (ops.InvitationCreateResult, error) {
	caller, err := h.caller(ctx, c)
	if err != nil {
		return ops.InvitationCreateResult{}, err
	}
	return h.svc.Create(ctx, caller, a)
}

func (h invitationHandlers) list(ctx context.Context, c *Call, a ops.InvitationListArgs) (ops.InvitationListResult, error) {
	caller, err := h.caller(ctx, c)
	if err != nil {
		return ops.InvitationListResult{}, err
	}
	return h.svc.List(ctx, caller, a.All)
}

func (h invitationHandlers) revoke(ctx context.Context, c *Call, a ops.InvitationRevokeArgs) (ops.WriteResult, error) {
	caller, err := h.caller(ctx, c)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if err := h.svc.Revoke(ctx, caller, a.ID); err != nil {
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}
