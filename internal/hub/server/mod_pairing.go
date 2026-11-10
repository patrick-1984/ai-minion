package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
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
	if err := api.RegisterPairing(handlers, pairingClaimer{ps}); err != nil {
		return fmt.Errorf("pairing: register pairing_claim: %w", err)
	}
	return nil
}

// pairingClaimer adapts the pairing service to the REST port.
type pairingClaimer struct{ ps *workers.Pairings }

func (p pairingClaimer) ClaimPairing(ctx context.Context, c api.PairingClaim) (api.PairingClaimed, error) {
	res, err := p.ps.Claim(ctx, workers.ClaimRequest{
		UserID: c.UserID, Code: c.Code, Name: c.Name, IP: c.IP, Actor: c.Actor, ActingAs: c.ActingAs,
	})
	if err != nil {
		return api.PairingClaimed{}, err
	}
	return api.PairingClaimed{WorkerID: res.WorkerID, Name: res.Name, Renamed: res.Renamed}, nil
}
