//go:build e2e

package e2e

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// userLookup and workerLookup give the real authorizer the Hub's own state:
// one active user and the owners the store holds.
type userLookup struct{}

func (userLookup) User(_ context.Context, id string) (authz.UserState, error) {
	if id != "owner-1" {
		return authz.UserState{}, authz.ErrNotFound
	}
	return authz.UserState{Role: authz.RoleUser, Active: true}, nil
}

type storeDirectory struct {
	workers.StoreDirectory
}

func (d storeDirectory) WorkerOwner(ctx context.Context, id string) (string, error) {
	recs, err := d.Workers(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range recs {
		if r.ID == id {
			return r.OwnerID, nil
		}
	}
	return "", authz.ErrNotFound
}
