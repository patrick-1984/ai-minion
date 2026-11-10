package api

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// PairingClaim is one pairing_claim call as the pairing service sees it.
type PairingClaim struct {
	// UserID owns the new Worker: the caller, or for an admin who acts as
	// another user, that user.
	UserID string
	// Code is the pairing code as typed. A secret until used: never log it.
	Code string
	// Name optionally overrides the name the Worker announced.
	Name string
	// IP is the client address, for the pairing rate limit.
	IP netip.Addr
	// Actor and ActingAs are the audit identity of the call (AD-20): who made
	// it ("key:<id>" for an API key, the admin under act-as) and the user an
	// admin acts as. UserID is the owner, never the audit actor.
	Actor    string
	ActingAs string
}

// PairingClaimed is the Worker a claim created.
type PairingClaimed struct {
	WorkerID string
	// Name is the name the Worker got; Renamed says it differs from the
	// requested or announced one because the owner already used that name.
	Name    string
	Renamed bool
}

// PairingClaimer is the port to the pairing service (Story 4.8,
// workers.Pairings). Its errors are *proto.Error values (pairing_code_invalid,
// pairing_attempts_exceeded, worker_offline, rate_limited, ...); an error with
// a RetryAfter() time.Duration method also sets Retry-After.
type PairingClaimer interface {
	ClaimPairing(ctx context.Context, c PairingClaim) (PairingClaimed, error)
}

// PairingClaimFunc adapts a function to [PairingClaimer].
type PairingClaimFunc func(ctx context.Context, c PairingClaim) (PairingClaimed, error)

// ClaimPairing calls f.
func (f PairingClaimFunc) ClaimPairing(ctx context.Context, c PairingClaim) (PairingClaimed, error) {
	return f(ctx, c)
}

// RegisterPairing registers the handler of pairing_claim in r. The Hub's
// workers module calls it with the pairing service once the service exists.
//
// The operation is decided on the caller's own user resource with action
// operate, so a user can only create Workers in their own space; an admin
// acting as a user creates them for that user. It is a write (POST only), so
// the code travels in the body, never in a URL, and the dispatcher audits the
// call without its arguments.
func RegisterPairing(r *Registry, c PairingClaimer) error {
	if c == nil {
		return errors.New("api: pairing_claim needs a pairing service")
	}
	return HandleTyped(r, ops.OpPairingClaim, func(ctx context.Context, call *Call, args ops.PairingClaimArgs) (ops.WriteResult, error) {
		got, err := c.ClaimPairing(ctx, PairingClaim{
			UserID: call.Principal.UserID, Code: args.Code, Name: args.Name, IP: call.IP,
			Actor: call.Identity.Actor, ActingAs: call.Identity.ActingAs,
		})
		if err != nil {
			var ra interface{ RetryAfter() time.Duration }
			if errors.As(err, &ra) {
				e := *ErrorOf(err)
				e.RetryAfter = ra.RetryAfter()
				return ops.WriteResult{}, &e
			}
			return ops.WriteResult{}, err
		}
		res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
		if got.Renamed {
			res.Warnings = []ops.Warning{{
				Code:    ops.WarnWorkerRenamed,
				Message: fmt.Sprintf("the name was already in use in your space; the Worker is called %s", got.Name),
			}}
		}
		return res, nil
	})
}
