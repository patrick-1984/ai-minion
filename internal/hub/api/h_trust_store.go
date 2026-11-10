package api

import (
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// TrustStore adapts the store to the trust port of the authz domain, which
// imports no storage. It maps the store's errors to the authz ones.
func TrustStore(st *store.Store) authz.TrustStore { return trustStore{st} }

type trustStore struct{ st *store.Store }

func mapTrustErr(err error) error {
	switch {
	case errors.Is(err, store.ErrWorkerOwnerChanged):
		return authz.ErrOwnerChanged
	case errors.Is(err, store.ErrWorkerNotFound):
		return authz.ErrNotFound
	}
	return err
}

func (a trustStore) PutTrust(ctx context.Context, receiverID, workerID, sessionName string, createdAt int64) error {
	return a.st.PutTrust(ctx, store.TrustRow{ReceiverID: receiverID, WorkerID: workerID, SessionName: sessionName, CreatedAt: createdAt})
}

func (a trustStore) PutTrustIfOwner(ctx context.Context, receiverID, workerID, sessionName string, createdAt int64, expectedOwnerID string) error {
	row := store.TrustRow{ReceiverID: receiverID, WorkerID: workerID, SessionName: sessionName, CreatedAt: createdAt}
	return mapTrustErr(a.st.PutTrustIfOwner(ctx, row, expectedOwnerID))
}

func (a trustStore) WorkerOwner(ctx context.Context, workerID string) (string, error) {
	owner, err := a.st.WorkerOwner(ctx, workerID)
	return owner, mapTrustErr(err)
}

func (a trustStore) DeleteTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	return a.st.DeleteTrust(ctx, receiverID, workerID, sessionName)
}

func (a trustStore) HasTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	return a.st.HasTrust(ctx, receiverID, workerID, sessionName)
}

func (a trustStore) ListTrust(ctx context.Context, receiverID string) ([]authz.TrustRecord, error) {
	rows, err := a.st.ListTrust(ctx, receiverID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.TrustRecord, len(rows))
	for i, r := range rows {
		out[i] = authz.TrustRecord{WorkerID: r.WorkerID, SessionName: r.SessionName, CreatedAt: r.CreatedAt}
	}
	return out, nil
}
