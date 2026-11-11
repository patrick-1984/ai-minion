package api

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// KeyManager is the port to the API-key service (Story 6.1);
// *apikey.Service implements it. It owns every rule about who may grant,
// list or revoke which key, and refuses callers that authenticated with an API
// key themselves.
type KeyManager interface {
	Create(ctx context.Context, p authz.Principal, args ops.APIKeyCreateArgs) (ops.APIKeyCreateResult, error)
	List(ctx context.Context, p authz.Principal, args ops.APIKeyListArgs) (ops.APIKeyListResult, error)
	Revoke(ctx context.Context, p authz.Principal, id string) error
}

// RegisterAPIKeyHandlers registers the handlers of apikey_create,
// apikey_list and apikey_revoke on r. The Hub's API-key module calls it from
// its init, after the REST module has provided the registry.
//
// The dispatcher has already authenticated the caller and decided the
// operation on a user resource: the caller's own, or, for apikey_list with an
// owner_id, that user's (so a user asking for another user's keys is not_found
// and an admin passes). The manager repeats the decisions that depend on the
// stored key, such as who owns the key to revoke.
func RegisterAPIKeyHandlers(r *Registry, m KeyManager) error {
	if m == nil {
		return fmt.Errorf("api: RegisterAPIKeyHandlers needs a key manager")
	}
	if err := HandleTyped(r, ops.OpAPIKeyCreate, func(ctx context.Context, c *Call, args ops.APIKeyCreateArgs) (ops.APIKeyCreateResult, error) {
		// The session that authenticated this request, with the credential
		// version it was resolved against; the insert compares it (F-07).
		if c.Identity.Session != nil {
			ctx = apikey.WithIssuerVersion(ctx, c.Identity.Session.CredentialVersion)
		}
		return m.Create(ctx, c.Principal, args)
	}); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpAPIKeyList, func(ctx context.Context, c *Call, args ops.APIKeyListArgs) (ops.APIKeyListResult, error) {
		return m.List(ctx, c.Principal, args)
	}, WithResource(listKeysResource)); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpAPIKeyRevoke, func(ctx context.Context, c *Call, args ops.APIKeyRevokeArgs) (ops.WriteResult, error) {
		if err := m.Revoke(ctx, c.Principal, args.ID); err != nil {
			return ops.WriteResult{}, err
		}
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
}

// listKeysResource decides apikey_list on the user whose keys are asked for:
// the caller by default.
func listKeysResource(p authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.APIKeyListArgs)
	if !ok {
		return nil, internalError()
	}
	if a.OwnerID != "" {
		return authz.UserResource{ID: a.OwnerID}, nil
	}
	if p.UserID == "" {
		return nil, NewError(proto.CodeForbidden, "not allowed")
	}
	return authz.UserResource{ID: p.UserID}, nil
}
