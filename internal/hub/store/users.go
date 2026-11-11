package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a looked-up row does not exist.
var ErrNotFound = errors.New("store: not found")

// UserRow is one row of the users table with the effective role resolved.
// PasswordHash is a secret-equivalent value: never log it.
type UserRow struct {
	ID                 string // UUIDv7
	Username           string
	Email              string // empty when the account has none
	PasswordHash       string // PHC string, empty without a local password
	MustChangePassword bool
	CredentialVersion  int64
	Active             bool
	Role               string // "admin", "user" or "" (no effective grant: no access), from user_roles
	CreatedAt          int64  // unix milliseconds
	UpdatedAt          int64
}

// userColumns is the select list that matches scanUser. The role is admin
// when any admin grant exists, whatever its source; user when only user
// grants exist; and empty when the account has no grant at all (F-07: an
// account without effective grants has no access for any principal).
const userColumns = `u.id, u.username, u.email, u.password_hash, u.must_change_password,
    u.credential_version, u.active,
    CASE WHEN EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'admin') THEN 'admin'
         WHEN EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id) THEN 'user'
         ELSE '' END,
    u.created_at, u.updated_at`

// userScan scans the columns of [userColumns].
type userScan struct {
	u                  UserRow
	mustChange, active int
}

func (s *userScan) dest() []any {
	u := &s.u
	return []any{&u.ID, &u.Username, &u.Email, &u.PasswordHash, &s.mustChange,
		&u.CredentialVersion, &s.active, &u.Role, &u.CreatedAt, &u.UpdatedAt}
}

func (s *userScan) row() UserRow {
	u := s.u
	u.MustChangePassword, u.Active = s.mustChange != 0, s.active != 0
	return u
}

// HasUsers reports whether the users table has any row.
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM users LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: count users: %w", err)
	}
	return true, nil
}

// UserByID returns the user with the given id, or [ErrNotFound].
func (s *Store) UserByID(ctx context.Context, id string) (UserRow, error) {
	return s.queryUser(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id)
}

// UserByIdentifier returns the user whose username or e-mail address equals
// ident, compared case-insensitively. A username match wins over an e-mail
// match. It returns [ErrNotFound] for no match.
func (s *Store) UserByIdentifier(ctx context.Context, ident string) (UserRow, error) {
	if ident == "" {
		return UserRow{}, ErrNotFound
	}
	return s.queryUser(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.username = ?1 OR u.email = ?1
         ORDER BY (u.username = ?1) DESC LIMIT 1`, ident)
}

func (s *Store) queryUser(ctx context.Context, query string, args ...any) (UserRow, error) {
	var sc userScan
	err := s.db.QueryRowContext(ctx, query, args...).Scan(sc.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("store: read user: %w", err)
	}
	return sc.row(), nil
}

// SeedAdminParams describes the first admin account.
type SeedAdminParams struct {
	// User is the account to create. Role is ignored: the account gets the
	// local admin grant.
	User UserRow
	// Marker is the auth_meta key written with the account, for example
	// "admin_seed". Its value is the id of the created user.
	Marker string
	// Audit, when not nil, is written in the same transaction.
	Audit *AuditRow
}

// SeedAdmin creates the first account in one transaction: the user row, its
// local admin grant, the seed marker and the optional audit row. It does
// nothing and reports created=false when the marker already exists or the
// users table is not empty (the
// check runs inside the transaction, which holds the write lock, so two
// starting Hubs cannot both seed). It never touches an existing account.
func (s *Store) SeedAdmin(ctx context.Context, p SeedAdminParams) (created bool, err error) {
	if p.User.ID == "" || p.User.Username == "" || p.Marker == "" {
		return false, errors.New("store: seed admin needs an id, a username and a marker")
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM auth_meta WHERE key = ?`, p.Marker).Scan(&one); {
		case err == nil:
			return nil // the seed marker exists: the seed already ran once
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: seed admin: read marker: %w", err)
		}
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM users LIMIT 1`).Scan(&one); {
		case err == nil:
			return nil // not empty: nothing to seed
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: seed admin: count users: %w", err)
		}
		u := p.User
		if _, err := tx.ExecContext(ctx, `
INSERT INTO users (id, username, email, password_hash, must_change_password, credential_version, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			u.ID, u.Username, u.Email, u.PasswordHash, boolInt(u.MustChangePassword), max(u.CredentialVersion, 1), u.CreatedAt, u.UpdatedAt); err != nil {
			return fmt.Errorf("store: seed admin: insert user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, u.ID); err != nil {
			return fmt.Errorf("store: seed admin: grant role: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_meta (key, value, created_at) VALUES (?, ?, ?)`,
			p.Marker, u.ID, u.CreatedAt); err != nil {
			return fmt.Errorf("store: seed admin: write marker: %w", err)
		}
		if p.Audit != nil {
			if err := s.InsertAudit(ctx, tx, *p.Audit); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
