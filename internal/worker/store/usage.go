package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
)

// UsageRegistration reads the consent for this exact enrollment. An absent
// grant is expected on upgraded Workers, pending enrollment and reassignment.
func (s *Store) UsageRegistration(ctx context.Context, workerID string) (bool, error) {
	var granted bool
	err := s.db.QueryRowContext(ctx, `SELECT usage_policy=1 FROM hub_enrollment WHERE id=1 AND worker_id=? AND worker_id<>'' AND owner<>''`, workerID).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return granted, err
}

// AcceptUsageRegistration updates only the enrollment the local person saw.
// A concurrent reset, re-pair or reassignment invalidates the selection.
func (s *Store) AcceptUsageRegistration(ctx context.Context, key ed25519.PublicKey, workerID, owner string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE hub_enrollment SET usage_policy=1 WHERE id=1 AND hub_pub=? AND worker_id=? AND owner=? AND owner<>'' AND worker_id<>''`, []byte(key), workerID, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrNotEnrolled
	}
	return err
}
