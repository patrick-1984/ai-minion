package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// PrepareUsageIntent stages explicit, local enrollment consent. Public URL and
// expected pin survive the configure-only handoff. An already enrolled identity
// also binds the intent to its present Worker ID and owner. No future Worker or
// reassignment inherits an accepted enrollment.
func (s *Store) PrepareUsageIntent(ctx context.Context, server, expected string) error {
	server, err := config.ValidateServerURL(server)
	if err != nil || server == "" {
		return errors.New("usage acceptance needs the selected Hub URL")
	}
	if expected != "" {
		fp, err := identity.ParseFingerprint(expected)
		if err != nil {
			return errors.New("invalid usage enrollment fingerprint")
		}
		expected = fp.String()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var pub []byte
	var worker, owner string
	err = tx.QueryRowContext(ctx, `SELECT hub_pub,worker_id,owner FROM hub_enrollment WHERE id=1`).Scan(&pub, &worker, &owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(pub) > 0 {
		fp, err := identity.FingerprintOf(ed25519.PublicKey(pub))
		if err != nil {
			return err
		}
		if expected != "" && expected != fp.String() {
			return errors.New("the enrollment belongs to a different Hub")
		}
		expected = fp.String()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_registration_intent(id,server,expected_pin,worker_id,owner,expires_at) VALUES(1,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET server=excluded.server,expected_pin=excluded.expected_pin,worker_id=excluded.worker_id,owner=excluded.owner,expires_at=excluded.expires_at`, server, expected, worker, owner, s.clock.now().Add(30*time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeUsageIntent runs after an authenticated welcome is saved and before
// the link starts serving. Legacy and unknown policies consume the one-shot
// without granting telemetry. An expired, retargeted or reassigned intent also
// cannot grant consent.
func (s *Store) ConsumeUsageIntent(ctx context.Context, server, workerID, owner string, pub ed25519.PublicKey, policy int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE hub_enrollment SET usage_hub_policy=? WHERE id=1 AND worker_id=? AND hub_pub=? AND usage_hub_policy<>?`, policy, workerID, []byte(pub), policy); err != nil {
		return err
	}
	var target, pin, boundWorker, boundOwner string
	var deadline int64
	err = tx.QueryRowContext(ctx, `SELECT server,expected_pin,worker_id,owner,expires_at FROM usage_registration_intent WHERE id=1`).Scan(&target, &pin, &boundWorker, &boundOwner, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_registration_intent WHERE id=1`); err != nil {
		return err
	}
	fp, err := identity.FingerprintOf(pub)
	if err != nil {
		return err
	}
	actual, err := config.ValidateServerURL(server)
	if err != nil {
		return err
	}
	matches := policy == ops.UsageVersion && deadline > s.clock.now().UnixMilli() && target == actual && (pin == "" || pin == fp.String()) && (boundWorker == "" || boundWorker == workerID) && (boundOwner == "" || boundOwner == owner)
	if matches {
		result, err := tx.ExecContext(ctx, `UPDATE hub_enrollment SET usage_policy=1 WHERE id=1 AND worker_id=? AND owner=? AND hub_pub=? AND worker_id<>'' AND owner<>''`, workerID, owner, []byte(pub))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNotEnrolled
		}
	}
	return tx.Commit()
}

func (s *Store) UsageRequired(ctx context.Context, id string) (bool, error) {
	var required bool
	err := s.db.QueryRowContext(ctx, `SELECT usage_required=1 AND usage_hub_policy=1 FROM hub_enrollment WHERE id=1 AND worker_id=?`, id).Scan(&required)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return required, err
}
