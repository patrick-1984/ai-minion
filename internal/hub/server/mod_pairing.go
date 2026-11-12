package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// pairingModuleOrder places the pairing module after the workers module (30),
// whose service and connect handler it extends, and the REST framework (40),
// whose *api.Registry it registers pairing_claim on.
const pairingModuleOrder = 45

func init() { registerPairingAPIModule(DefaultRegistry) }

// registerPairingAPIModule registers the pairing module (Story 4.8) in r. It
// turns on the pair_code mode of the Worker connect endpoint, wires the owner
// user name that welcome carries and registers pairing_claim on the REST
// handler registry. It requires the workers service, the store and the REST
// framework's handler registry; it provides nothing.
func registerPairingAPIModule(r *Registry) {
	if err := r.Register("pairing", pairingModuleOrder, initPairingAPI); err != nil {
		panic(err)
	}
}

func initPairingAPI(_ context.Context, env *Env) error {
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	svc.SetOwnerNames(func(ctx context.Context, ownerID string) string {
		if u, err := st.UserByID(ctx, ownerID); err == nil && u.Username != "" {
			return u.Username
		}
		return ownerID
	})
	ps, err := svc.EnablePairing(workers.PairingOptions{})
	if err != nil {
		return fmt.Errorf("pairing: enable the pair_code mode: %w", err)
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	if err := api.RegisterPairing(handlers, pairingClaimer{ps: ps, st: st, az: az}); err != nil {
		return fmt.Errorf("pairing: register pairing_claim: %w", err)
	}
	auditLog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	if err := api.RegisterWorkerPairingReissue(handlers, pairingReissuer{st: st, online: svc.Registry(), audit: auditLog, now: time.Now}); err != nil {
		return fmt.Errorf("pairing: register worker_pairing_reissue: %w", err)
	}
	return nil
}

// pairingReissuer adapts workers.ReissuePairing (design 12.3) to the REST port.
type pairingReissuer struct {
	st     *store.Store
	online workers.OfflineGate
	audit  *audit.Log
	now    func() time.Time
}

func (p pairingReissuer) ReissuePairing(ctx context.Context, req workers.ReissueRequest) error {
	return workers.ReissuePairing(ctx, p.st, p.online, p.audit, p.now(), req)
}

func (p pairingReissuer) IsAdmin(ctx context.Context, userID string) (bool, error) {
	u, err := p.st.UserByID(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.Role == string(authz.RoleAdmin), nil
}

// pairingClaimer adapts the pairing service to the REST port. A claimant who
// does not own the Worker of a re-issued pairing (design 12.3) may complete it
// only as an administrator who holds operate on that very Worker, decided with
// the call's own principal: an API key counts with its scope, never with the
// role of its owner alone.
type pairingClaimer struct {
	ps *workers.Pairings
	st *store.Store
	az *authz.Authorizer
}

func (p pairingClaimer) ClaimPairing(ctx context.Context, c api.PairingClaim) (api.PairingClaimed, error) {
	principal := c.Principal
	res, err := p.ps.Claim(ctx, workers.ClaimRequest{
		UserID: c.UserID, Code: c.Code, Name: c.Name, IP: c.IP, Actor: c.Actor, ActingAs: c.ActingAs,
		MayRepair: func(ctx context.Context, workerID string) (bool, error) {
			return p.mayRepair(ctx, principal, workerID)
		},
	})
	if err != nil {
		return api.PairingClaimed{}, err
	}
	return api.PairingClaimed{WorkerID: res.WorkerID, Name: res.Name, Renamed: res.Renamed}, nil
}

// mayRepair is the administrator override of a repair: the principal's user
// is an administrator AND the principal holds operate on the Worker.
func (p pairingClaimer) mayRepair(ctx context.Context, principal authz.Principal, workerID string) (bool, error) {
	if principal.UserID == "" || principal.Kind == authz.KindWorker {
		return false, nil
	}
	u, err := p.st.UserByID(ctx, principal.UserID)
	if err != nil {
		return false, err
	}
	if u.Role != string(authz.RoleAdmin) {
		return false, nil
	}
	err = p.az.Decide(ctx, principal, ops.ActionSet{ops.ActionOperate}, authz.WorkerResource{ID: workerID})
	if err == nil {
		return true, nil
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		return false, nil // forbidden or not_found: no operate on this Worker
	}
	return false, err
}
