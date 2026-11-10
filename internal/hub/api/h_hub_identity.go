package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// IdentityRotator is the port to the Hub identity rotation (Story 4.9);
// *workers.Rotator implements it.
type IdentityRotator interface {
	Rotate(ctx context.Context, actor workers.RotateActor) (workers.RotateResult, error)
}

var _ IdentityRotator = (*workers.Rotator)(nil)

// RegisterHubIdentityHandlers registers the handler of hub_identity_rotate on
// r. The Hub's hub-identity module calls it from its init, after the REST
// module has provided the registry.
//
// The dispatcher has already authenticated the caller and required the manage
// action on the Hub (an admin; a user without the role and an API key without
// a manage scope never reach the handler). The handler additionally requires
// a key to hold manage over every user, because the decision is taken on the
// owner's own user resource and a key limited to chosen users must not rotate
// the identity of the whole Hub.
//
// A transition chain at its size limit is a conflict that force does not
// override (guard hub_identity_chain_not_full is KEEP). A rotation that
// committed but whose follow-up step failed is delivered, with a warning:
// the new key is active, so reporting an error would invite a second rotation.
func RegisterHubIdentityHandlers(r *Registry, rot IdentityRotator) error {
	if rot == nil {
		return fmt.Errorf("api: RegisterHubIdentityHandlers needs a rotator")
	}
	return HandleTyped(r, ops.OpHubIdentityRotate, func(ctx context.Context, c *Call, _ ops.HubIdentityRotateArgs) (ops.WriteResult, error) {
		if err := requireAllUsersManage(c.Principal); err != nil {
			return ops.WriteResult{}, err
		}
		res, err := rot.Rotate(ctx, rotateActor(c.Principal))
		switch {
		case err == nil:
		case errors.Is(err, workers.ErrChainFull):
			return ops.WriteResult{}, proto.NewError(proto.CodeConflict, err.Error())
		case res.Seq > 0:
			// Committed: only a step after the commit failed.
			slog.Default().Error("hub identity rotation committed, a follow-up step failed",
				"seq", res.Seq, "fingerprint", res.NewFingerprint.String(), "error", err)
		default:
			return ops.WriteResult{}, err
		}
		out := ops.WriteResult{Outcome: ops.OutcomeDelivered}
		if res.RevokedInvitations > 0 {
			out.Warnings = append(out.Warnings, ops.Warning{
				Code:    ops.WarnInvitationsRevoked,
				Message: fmt.Sprintf("%d outstanding invitation keys were revoked; create new ones", res.RevokedInvitations),
			})
		}
		if err != nil {
			out.Warnings = append(out.Warnings, ops.Warning{
				Code:    ops.WarnRotationFollowUp,
				Message: "the rotation is active, but a step after it failed; see the Hub log",
			})
		}
		return out, nil
	})
}

// rotateActor names who rotates, as the audit row records it: the admin, and
// the user they act as when the call is an act-as call.
func rotateActor(p authz.Principal) workers.RotateActor {
	if p.ActorID != "" {
		return workers.RotateActor{ID: p.ActorID, ActingAs: p.UserID}
	}
	return workers.RotateActor{ID: p.UserID}
}

// requireAllUsersManage refuses an API key whose scope does not grant manage
// on a user it was never given (the "everything" scope). Browser sessions and
// act-as are not scoped.
func requireAllUsersManage(p authz.Principal) error {
	if p.Kind != authz.KindAPIKey {
		return nil
	}
	if p.Scope != nil {
		id := store.NewID() // a user no chosen-users scope can list
		probe := authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
		if slices.Contains(p.Scope.Actions(probe), ops.ActionManage) {
			return nil
		}
	}
	return NewError(proto.CodeForbidden, "this API key is limited to some resources; rotating the Hub identity takes an everything scope")
}
