package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// refreshTokenUsable reports whether the identity has a stored refresh token
// that decrypts successfully to a non-empty value. An unreadable token cannot
// be used by the background refresh, so it cannot protect an IdP admin grant.
func (s *Store) refreshTokenUsable(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, identityID string) (bool, error) {
	var sealed []byte
	err := q.QueryRowContext(ctx, `SELECT refresh_token_enc FROM external_identities WHERE id = ?`, identityID).Scan(&sealed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read refresh token: %w", err)
	}
	if len(sealed) == 0 {
		return false, nil
	}
	plain, err := s.box.Open(identitiesTable, identitiesTokenCol, identityID, sealed)
	return err == nil && len(plain) > 0, nil
}

// suspendUnusableIdPAdmins runs at every start, before the Hub serves. It
// removes the source-idp admin grant of every account that has no identity
// with a usable refresh token (see refreshTokenUsable), because such a grant
// can never be re-checked by the background refresh. Each suspension is
// audited (role.suspend, actor system). Local grants, keys and Workers stay.
// It returns the number of suspended accounts.
func (s *Store) suspendUnusableIdPAdmins(ctx context.Context) (int, error) {
	n := 0
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('user_roles', 'external_identities', 'audit_events')`).Scan(&tables); err != nil {
		return 0, fmt.Errorf("store: read schema: %w", err)
	}
	if tables < 3 {
		return 0, nil // a database that is not a Hub schema (tests with custom migrations)
	}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		n = 0
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM user_roles WHERE source = 'idp' AND role = 'admin'`)
		if err != nil {
			return fmt.Errorf("store: list idp admins: %w", err)
		}
		var users []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: list idp admins: %w", err)
			}
			users = append(users, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		for _, uid := range users {
			irows, err := tx.QueryContext(ctx, `SELECT id FROM external_identities WHERE user_id = ?`, uid)
			if err != nil {
				return fmt.Errorf("store: list identities: %w", err)
			}
			var ids []string
			for irows.Next() {
				var id string
				if err := irows.Scan(&id); err != nil {
					_ = irows.Close()
					return err
				}
				ids = append(ids, id)
			}
			_ = irows.Close()
			usable := false
			for _, id := range ids {
				ok, err := s.refreshTokenUsable(ctx, tx, id)
				if err != nil {
					return err
				}
				if ok {
					usable = true
					break
				}
			}
			if usable {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND source = 'idp' AND role = 'admin'`, uid); err != nil {
				return fmt.Errorf("store: suspend idp admin: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events (id, ts, actor, acting_as, action, target, outcome) VALUES (?, ?, 'system', '', 'role.suspend', ?, 'changed')`,
				NewID(), time.Now().UnixMilli(), uid); err != nil {
				return fmt.Errorf("store: audit idp admin suspension: %w", err)
			}
			var stillAdmin int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND role = 'admin'`, uid).Scan(&stillAdmin); err != nil {
				return fmt.Errorf("store: read remaining admin grants: %w", err)
			}
			if stillAdmin == 0 {
				if err := capDemotedAdminInvitations(ctx, tx, uid); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// capDemotedAdminInvitations applies the legacy invitation caps of migration
// 0020 to the invitations of an account that the reconciliation just demoted
// (migration 0020 exempted it because it still held an admin grant). Only
// unlimited or use-unlimited keys change, so a second run changes nothing.
func capDemotedAdminInvitations(ctx context.Context, tx *sql.Tx, userID string) error {
	expiry := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE invitation_keys
   SET expires_at = ?,
       notice = 'Limited by the upgrade to 0.6.0: this key had no expiry; it now expires 7 days after the upgrade.'
 WHERE creator_id = ? AND revoked = 0 AND expires_at IS NULL`, expiry, userID); err != nil {
		return fmt.Errorf("store: cap invitations of a demoted admin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invitation_keys
   SET max_uses = uses + 10,
       notice = CASE WHEN notice = '' THEN '' ELSE notice || ' ' END ||
                'Limited by the upgrade to 0.6.0: this reusable key had no use limit; it now allows 10 more enrollments.'
 WHERE creator_id = ? AND revoked = 0 AND mode = 'reusable' AND max_uses IS NULL`, userID); err != nil {
		return fmt.Errorf("store: cap invitations of a demoted admin: %w", err)
	}
	return nil
}
