package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionRow is one row of the sessions table. TokenHash is the hex SHA-256
// of the cookie value; the cookie itself is never stored.
type SessionRow struct {
	ID                string // UUIDv7
	TokenHash         string
	UserID            string
	CredentialVersion int64
	CreatedAt         int64 // unix milliseconds
	ExpiresAt         int64
	// AuthAt is the unix milliseconds of the last credential check the server
	// itself performed for this session; 0 means none. AuthMethod is "local",
	// "oidc:<provider id>" or "".
	AuthAt     int64
	AuthMethod string
}

// CreateSession stores a new session and, in the same transaction, deletes
// the sessions that expired before nowMS, so the table does not grow without
// bound.
func (s *Store) CreateSession(ctx context.Context, row SessionRow, nowMS int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, nowMS); err != nil {
			return fmt.Errorf("store: prune sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, token_hash, user_id, credential_version, created_at, expires_at, auth_at, auth_method) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.TokenHash, row.UserID, row.CredentialVersion, row.CreatedAt, row.ExpiresAt, row.AuthAt, row.AuthMethod); err != nil {
			return fmt.Errorf("store: create session: %w", err)
		}
		return nil
	})
}

// SessionByTokenHash returns the session with the given token hash and its
// user, or [ErrNotFound]. It does not judge validity (expiry, active user,
// credential version); the session package does.
func (s *Store) SessionByTokenHash(ctx context.Context, tokenHash string) (SessionRow, UserRow, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT x.id, x.token_hash, x.user_id, x.credential_version, x.created_at, x.expires_at, x.auth_at, x.auth_method, `+userColumns+`
FROM sessions x JOIN users u ON u.id = x.user_id
WHERE x.token_hash = ?`, tokenHash)
	var (
		sess SessionRow
		sc   userScan
	)
	dest := append([]any{&sess.ID, &sess.TokenHash, &sess.UserID, &sess.CredentialVersion, &sess.CreatedAt, &sess.ExpiresAt, &sess.AuthAt, &sess.AuthMethod}, sc.dest()...)
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, UserRow{}, ErrNotFound
	}
	if err != nil {
		return SessionRow{}, UserRow{}, fmt.Errorf("store: read session: %w", err)
	}
	return sess, sc.row(), nil
}

// DeleteSession deletes the session with the given token hash. It reports
// whether a row existed; a missing row is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return false, fmt.Errorf("store: delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete session: %w", err)
	}
	return n > 0, nil
}

// SetSessionAuth records a verified authentication on the session with the
// given id: auth_at and auth_method. It reports whether the session exists.
// Only the server calls it, after its own credential check.
func (s *Store) SetSessionAuth(ctx context.Context, sessionID string, authAt int64, method string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET auth_at = ?, auth_method = ? WHERE id = ?`, authAt, method, sessionID)
	if err != nil {
		return false, fmt.Errorf("store: set session authentication: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: set session authentication: %w", err)
	}
	return n > 0, nil
}

// ExternalIdentityOf returns the issuer and subject that the user has at the
// provider, or [ErrNotFound]. The re-authentication of an OIDC session uses
// it to require the same subject.
func (s *Store) ExternalIdentityOf(ctx context.Context, userID, providerID string) (issuer, subject string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT issuer, subject FROM external_identities WHERE user_id = ? AND provider_id = ?`, userID, providerID).Scan(&issuer, &subject)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("store: read external identity: %w", err)
	}
	return issuer, subject, nil
}
