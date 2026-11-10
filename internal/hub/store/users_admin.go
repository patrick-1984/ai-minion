package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Errors of the account administration methods. Callers match them with
// errors.Is. ErrNotFound (users.go) reports an unknown user.
var (
	// ErrUsernameTaken: another account has this user name (compared
	// case-insensitively).
	ErrUsernameTaken = errors.New("store: user name is taken")
	// ErrEmailTaken: another account has this e-mail address.
	ErrEmailTaken = errors.New("store: e-mail address is taken")
	// ErrVersionConflict: the credential version changed since it was read,
	// so another change won the race.
	ErrVersionConflict = errors.New("store: credential version changed")
	// ErrUserInactive: the account is deactivated.
	ErrUserInactive = errors.New("store: user is deactivated")
	// ErrNoLocalPassword: the account has no local password.
	ErrNoLocalPassword = errors.New("store: user has no local password")
	// ErrLastAdmin: the change would leave the Hub without an active admin.
	ErrLastAdmin = errors.New("store: this is the last active admin")
)

// UserAdminRow is one account in the admin list: the user with the sources of
// its admin grants.
type UserAdminRow struct {
	UserRow
	// AdminSources are the sources ("local", "idp") of the admin grants, in
	// that order; empty for a plain user.
	AdminSources []string
}

// TxHook runs inside the transaction of an account change, with the id of the
// user it concerns. Another domain package (API keys, Story 6.1) supplies one
// to change its own tables atomically with the account.
type TxHook func(ctx context.Context, tx *sql.Tx, userID string) error

// ListUsers returns every account ordered by user name, with the sources of
// its admin grants.
func (s *Store) ListUsers(ctx context.Context) ([]UserAdminRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+`,
    COALESCE((SELECT group_concat(source) FROM (
        SELECT r.source FROM user_roles r WHERE r.user_id = u.id AND r.role = 'admin' ORDER BY r.source)), '')
FROM users u ORDER BY u.username COLLATE NOCASE, u.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UserAdminRow
	for rows.Next() {
		var (
			sc      userScan
			sources string
		)
		if err := rows.Scan(append(sc.dest(), &sources)...); err != nil {
			return nil, fmt.Errorf("store: list users: %w", err)
		}
		row := UserAdminRow{UserRow: sc.row()}
		if sources != "" {
			row.AdminSources = strings.Split(sources, ",")
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return out, nil
}

// CreateUserParams describes a new local account.
type CreateUserParams struct {
	// User is the account to create. Role is ignored in favour of Role below.
	User UserRow
	// Role is "user" or "admin"; the grant is written with source local.
	Role string
	// Audit, when not nil, is written in the same transaction.
	Audit *AuditRow
}

// CreateUser creates an account and its local role grant in one transaction.
// It returns [ErrUsernameTaken] or [ErrEmailTaken] when the name or address is
// in use. A user name that equals any account's e-mail address (and an e-mail
// that equals any account's user name) counts as in use, case-insensitively:
// [Store.UserByIdentifier] resolves a sign-in identifier against both columns
// and a user name wins, so such an account would capture the other account's
// sign-in identifier.
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) error {
	u := p.User
	if u.ID == "" || u.Username == "" || (p.Role != "user" && p.Role != "admin") {
		return errors.New("store: create user needs an id, a user name and a role")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if exists, err := rowExists(ctx, tx, `SELECT 1 FROM users WHERE username = ?1 OR email = ?1`, u.Username); err != nil {
			return fmt.Errorf("store: create user: %w", err)
		} else if exists {
			return ErrUsernameTaken
		}
		if u.Email != "" {
			if exists, err := rowExists(ctx, tx, `SELECT 1 FROM users WHERE email = ?1 OR username = ?1`, u.Email); err != nil {
				return fmt.Errorf("store: create user: %w", err)
			} else if exists {
				return ErrEmailTaken
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO users (id, username, email, password_hash, must_change_password, credential_version, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			u.ID, u.Username, u.Email, u.PasswordHash, boolInt(u.MustChangePassword), max(u.CredentialVersion, 1), u.CreatedAt, u.UpdatedAt); err != nil {
			return fmt.Errorf("store: create user: insert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source) VALUES (?, ?, 'local')`, u.ID, p.Role); err != nil {
			return fmt.Errorf("store: create user: grant role: %w", err)
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
}

// ResetPasswordParams describes an admin password reset.
type ResetPasswordParams struct {
	UserID string
	// PasswordHash is the PHC hash of the new temporary password.
	PasswordHash string
	NowMS        int64
	Audit        *AuditRow
}

// ResetPassword stores a new temporary password hash, sets must-change, bumps
// the credential version and deletes every session of the user, in one
// transaction. It returns [ErrNotFound], [ErrUserInactive] or
// [ErrNoLocalPassword] (an account without a local password, for example one
// that signs in through an IdP, gets no password from a reset).
func (s *Store) ResetPassword(ctx context.Context, p ResetPasswordParams) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if !u.Active {
			return ErrUserInactive
		}
		if u.PasswordHash == "" {
			return ErrNoLocalPassword
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE users SET password_hash = ?, must_change_password = 1, credential_version = credential_version + 1, updated_at = ?
WHERE id = ?`, p.PasswordHash, p.NowMS, p.UserID); err != nil {
			return fmt.Errorf("store: reset password: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, p.UserID); err != nil {
			return fmt.Errorf("store: reset password: revoke sessions: %w", err)
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
}

// ChangePasswordParams describes a password change by the user.
type ChangePasswordParams struct {
	UserID string
	// ExpectedVersion is the credential version the caller read before it
	// verified the old password. The change applies only while it is current.
	ExpectedVersion int64
	// PasswordHash is the PHC hash of the new password.
	PasswordHash string
	// KeepSessionID is the session that made the change. It survives, with
	// the new credential version; every other session of the user is
	// deleted. Empty keeps none.
	KeepSessionID string
	NowMS         int64
	Audit         *AuditRow
}

// ChangePassword replaces the password hash, clears must-change, bumps the
// credential version and revokes the user's other sessions in one
// transaction. The update is conditional on ExpectedVersion: of two changes
// that read the same version exactly one is applied and the other gets
// [ErrVersionConflict]. It returns the new credential version.
func (s *Store) ChangePassword(ctx context.Context, p ChangePasswordParams) (int64, error) {
	var next int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE users SET password_hash = ?, must_change_password = 0, credential_version = credential_version + 1, updated_at = ?
WHERE id = ? AND credential_version = ? AND active = 1`, p.PasswordHash, p.NowMS, p.UserID, p.ExpectedVersion)
		if err != nil {
			return fmt.Errorf("store: change password: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: change password: %w", err)
		} else if n == 0 {
			u, err := txUser(ctx, tx, p.UserID)
			switch {
			case err != nil:
				return err
			case !u.Active:
				return ErrUserInactive
			}
			return ErrVersionConflict
		}
		if err := tx.QueryRowContext(ctx, `SELECT credential_version FROM users WHERE id = ?`, p.UserID).Scan(&next); err != nil {
			return fmt.Errorf("store: change password: read version: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ?`, p.UserID, p.KeepSessionID); err != nil {
			return fmt.Errorf("store: change password: revoke sessions: %w", err)
		}
		if p.KeepSessionID != "" {
			// The kept session carries the version of the new credentials,
			// or the session check would end it too.
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET credential_version = ? WHERE id = ? AND user_id = ?`,
				next, p.KeepSessionID, p.UserID); err != nil {
				return fmt.Errorf("store: change password: keep session: %w", err)
			}
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// SetLocalRoleParams describes a change of the local role.
type SetLocalRoleParams struct {
	UserID string
	// Role is "admin" (add the local admin grant) or "user" (remove it).
	Role  string
	NowMS int64
	Audit *AuditRow
}

// SetLocalRole replaces the local role grants of a user: the local grants
// become exactly {Role}. IdP grants are never touched, so the effective role
// stays admin while an IdP admin grant exists. It returns [ErrNotFound],
// [ErrUserInactive], or [ErrLastAdmin] when the change would leave no other
// active admin. It returns the effective role afterwards.
func (s *Store) SetLocalRole(ctx context.Context, p SetLocalRoleParams) (effective string, err error) {
	if p.Role != "user" && p.Role != "admin" {
		return "", errors.New("store: set role needs the role user or admin")
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if !u.Active {
			return ErrUserInactive
		}
		var idpAdmin bool
		if idpAdmin, err = rowExists(ctx, tx, `SELECT 1 FROM user_roles WHERE user_id = ? AND role = 'admin' AND source = 'idp'`, p.UserID); err != nil {
			return fmt.Errorf("store: set role: %w", err)
		}
		if p.Role == "user" && u.Role == "admin" && !idpAdmin {
			if err := requireOtherAdmin(ctx, tx, p.UserID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND source = 'local'`, p.UserID); err != nil {
			return fmt.Errorf("store: set role: clear local grants: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source) VALUES (?, ?, 'local')`, p.UserID, p.Role); err != nil {
			return fmt.Errorf("store: set role: grant: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET updated_at = ? WHERE id = ?`, p.NowMS, p.UserID); err != nil {
			return fmt.Errorf("store: set role: touch user: %w", err)
		}
		effective = "user"
		if p.Role == "admin" || idpAdmin {
			effective = "admin"
		}
		return s.auditIn(ctx, tx, p.Audit)
	})
	if err != nil {
		return "", err
	}
	return effective, nil
}

// DeactivateUserParams describes a deactivation.
type DeactivateUserParams struct {
	UserID string
	NowMS  int64
	// Hooks run in the transaction after the account is deactivated; the API
	// key domain revokes the user's keys in one. A hook error rolls
	// everything back.
	Hooks []TxHook
	Audit *AuditRow
}

// DeactivateUser marks the account inactive, bumps its credential version and
// deletes all its sessions, then runs the hooks, in one transaction. The
// user's Workers are not touched: they stay with their owner (AD-10). It
// returns [ErrNotFound] or [ErrLastAdmin]. Deactivating a deactivated account
// succeeds and changes nothing; changed reports whether anything was done.
func (s *Store) DeactivateUser(ctx context.Context, p DeactivateUserParams) (changed bool, err error) {
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		u, err := txUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if !u.Active {
			return nil
		}
		if u.Role == "admin" {
			if err := requireOtherAdmin(ctx, tx, p.UserID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE users SET active = 0, credential_version = credential_version + 1, updated_at = ? WHERE id = ?`, p.NowMS, p.UserID); err != nil {
			return fmt.Errorf("store: deactivate user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, p.UserID); err != nil {
			return fmt.Errorf("store: deactivate user: revoke sessions: %w", err)
		}
		for _, h := range p.Hooks {
			if err := h(ctx, tx, p.UserID); err != nil {
				return fmt.Errorf("store: deactivate user: hook: %w", err)
			}
		}
		changed = true
		return s.auditIn(ctx, tx, p.Audit)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// requireOtherAdmin returns ErrLastAdmin unless an active admin other than
// userID exists.
func requireOtherAdmin(ctx context.Context, tx *sql.Tx, userID string) error {
	var n int
	err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM users u
WHERE u.active = 1 AND u.id <> ?
  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'admin')`, userID).Scan(&n)
	if err != nil {
		return fmt.Errorf("store: count admins: %w", err)
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// txUser reads a user inside a transaction, or returns ErrNotFound.
func txUser(ctx context.Context, tx *sql.Tx, id string) (UserRow, error) {
	var sc userScan
	err := tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id).Scan(sc.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("store: read user: %w", err)
	}
	return sc.row(), nil
}

func rowExists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// auditIn writes an optional audit row inside tx.
func (s *Store) auditIn(ctx context.Context, tx *sql.Tx, row *AuditRow) error {
	if row == nil {
		return nil
	}
	return s.InsertAudit(ctx, tx, *row)
}
