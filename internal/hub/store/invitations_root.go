package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrRootChanged: the Hub root changed between the moment a key was bound to
// the root fingerprint and its insert.
var ErrRootChanged = errors.New("store: the Hub root fingerprint changed")

// InsertInvitationForRoot adds an invitation key like [Store.InsertInvitation],
// in one transaction that first compares the stored root fingerprint with
// rootFingerprint, the one the key is bound to. When they differ nothing is
// inserted and ErrRootChanged is returned, so a key bound to a superseded root
// is never stored. A database without an identity row has no stored root to
// compare (a bare test store) and inserts.
func (s *Store) InsertInvitationForRoot(ctx context.Context, r InvitationRow, rootFingerprint string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		var fp string
		switch err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM hub_identity WHERE id = 1`).Scan(&fp); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("store: read the root fingerprint: %w", err)
		case fp != rootFingerprint:
			return ErrRootChanged
		}
		var expires, maxUses any
		if r.ExpiresAt != 0 {
			expires = r.ExpiresAt
		}
		if r.MaxUses != 0 {
			maxUses = r.MaxUses
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO invitation_keys (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, expires_at, max_uses)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.SecretHash, r.Name, r.Mode, r.Kind, r.TTL, r.CreatorID, r.CreatedAt, expires, maxUses); err != nil {
			return fmt.Errorf("store: insert invitation key: %w", err)
		}
		return nil
	})
}
