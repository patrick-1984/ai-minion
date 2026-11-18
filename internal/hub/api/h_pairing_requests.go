package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// PairingRequestsService is the port of the pairing requests (v0.12.0) to the
// pairing service; the server's pairing module implements it with
// *workers.Pairings and the Hub identity.
type PairingRequestsService interface {
	Requests() []workers.PairingRequest
	Admit(ctx context.Context, req workers.AdmitRequest) (workers.ClaimResult, error)
	Reject(ctx context.Context, req workers.RejectRequest) error
}

// RegisterPairingRequests registers pairing_requests_list,
// pairing_request_admit and pairing_request_reject on r. The dispatcher has
// decided manage on the Hub (an administrator, or an administrator's API key
// that holds manage); a Worker identity and a key bound to a Worker are
// refused here as well, so a credential on a waiting machine never admits it.
// The admitting caller owns the new Worker, which is approved at once: the
// administrator compared its words, and approving is what an administrator
// may do anyway (F-02).
func RegisterPairingRequests(r *Registry, svc PairingRequestsService) error {
	if svc == nil {
		return errors.New("api: the pairing requests need a service")
	}
	if err := HandleTyped(r, ops.OpPairingRequestsList, func(_ context.Context, c *Call, _ ops.PairingRequestsListArgs) (ops.PairingRequestsListResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.PairingRequestsListResult{}, err
		}
		return PairingRequestsResult(svc), nil
	}); err != nil {
		return err
	}
	if err := HandleTyped(r, ops.OpPairingRequestAdmit, func(ctx context.Context, c *Call, a ops.PairingRequestAdmitArgs) (ops.WriteResult, error) {
		if err := requestCaller(c); err != nil {
			return ops.WriteResult{}, err
		}
		got, err := svc.Admit(ctx, workers.AdmitRequest{
			UserID: c.Principal.UserID, PairingID: a.PairingID, Fingerprint: a.WorkerFingerprint, HubFingerprint: a.HubFingerprint, Name: a.Name,
			Approve: true, Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs,
		})
		if err != nil {
			return ops.WriteResult{}, err
		}
		res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
		if got.Pending {
			res.Warnings = append(res.Warnings, ops.Warning{Code: "worker_pending", Message: "the Worker waits for approval"})
		}
		if got.Renamed {
			res.Warnings = append(res.Warnings, ops.Warning{
				Code:    ops.WarnWorkerRenamed,
				Message: fmt.Sprintf("the name was already in use in your space; the Worker is called %s", got.Name),
			})
		}
		return res, nil
	}); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpPairingRequestReject, func(ctx context.Context, c *Call, a ops.PairingRequestRejectArgs) (ops.WriteResult, error) {
		if err := requestCaller(c); err != nil {
			return ops.WriteResult{}, err
		}
		if err := svc.Reject(ctx, workers.RejectRequest{
			PairingID: a.PairingID, Fingerprint: a.WorkerFingerprint, Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs,
		}); err != nil {
			return ops.WriteResult{}, err
		}
		return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
	})
}

// requestCaller refuses a Worker identity, a key bound to a Worker and a call
// without a user.
//
// The dispatcher decided manage on the caller's own user resource; a key whose
// scope reaches only some resources is refused here, as for the other
// Hub-wide administrator operations.
func requestCaller(c *Call) error {
	p := c.Principal
	if p.UserID == "" || c.Identity.Actor == "" || p.Kind == authz.KindWorker || (p.Kind == authz.KindAPIKey && p.WorkerID != "") {
		return NewError(proto.CodeForbidden, "not allowed")
	}
	return requireHubWideScope(p)
}

// PairingRequestsResult renders the open requests as the operation returns
// them.
func PairingRequestsResult(svc PairingRequestsService) ops.PairingRequestsListResult {
	out := ops.PairingRequestsListResult{Requests: []ops.PairingRequestInfo{}}
	for _, q := range svc.Requests() {
		out.Requests = append(out.Requests, ops.PairingRequestInfo{
			PairingID: q.PairingID, Name: q.Name, SourceAddr: q.Addr,
			WaitingSince: q.Since.UTC().Format(time.RFC3339), ExpiresAt: q.Expires.UTC().Format(time.RFC3339),
			WorkerFingerprint: string(q.WorkerFingerprint), WorkerFingerprintWords: q.WorkerFingerprint.Words(),
			HubFingerprint: string(q.HubFingerprint), HubFingerprintWords: q.HubFingerprint.Words(),
		})
	}
	return out
}
