package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrAPIKeyNotFound: no api_keys row matches.
var ErrAPIKeyNotFound = errors.New("store: API key not found")

// APIKeyScopeRow is one resource of a key scope. Kind is "everything",
// "user", "worker" or "session"; the members that do not belong to the kind
// are empty.
type APIKeyScopeRow struct {
	Kind        string
	UserID      string
	WorkerID    string
	SessionName string
}

// APIKeyRow is one API key with its scope. Hash is the SHA-256 of the key
// text; the key itself is never stored. A zero ExpiresAt means the key does not
// expire and a zero RevokedAt that it is not revoked (unix milliseconds).
type APIKeyRow struct {
	ID            string // UUIDv7
	OwnerID       string
	Name          string
	Hash          []byte
	Hint          string
	Actions       []string // sorted
	BoundWorkerID string   // empty when the key is not bound
	CreatedAt     int64
	ExpiresAt     int64
	RevokedAt     int64
	// LastUsedAt is when the key last authenticated, at most a minute late; 0
	// when it never did (unix milliseconds). CreateAPIKey ignores it.
	LastUsedAt int64
	Scopes     []APIKeyScopeRow
	// IssuerCredentialVersion is the credential_version of the authenticating
	// session at the time the request was authenticated. CreateAPIKey refuses
	// the insert while the owner's current version differs, so a request
	// authenticated before a password reset or change cannot mint a key after
	// it (F-07). 0 means "not checked"; the apikey service always sets it.
	// It is not stored.
	IssuerCredentialVersion int64
}

// ErrStaleIssuer is returned by CreateAPIKey when the credentials of the
// issuing session changed (password reset or change, role loss) after the
// request was authenticated.
var ErrStaleIssuer = errors.New("store: the session that asked for the key is no longer current")

// CreateAPIKey stores a key and its scope rows in one transaction (AD-16),
// together with the optional audit row, so that a key never exists without its
// grants. A duplicate hash fails.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKeyRow, audit *AuditRow) error {
	if len(k.Scopes) == 0 {
		return errors.New("store: an API key needs at least one scope row")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if k.IssuerCredentialVersion != 0 {
			var cur int64
			var active int
			err := tx.QueryRowContext(ctx, `SELECT credential_version, active FROM users WHERE id = ?`, k.OwnerID).Scan(&cur, &active)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrStaleIssuer
			}
			if err != nil {
				return fmt.Errorf("store: create API key: read issuer: %w", err)
			}
			if cur != k.IssuerCredentialVersion || active != 1 {
				return ErrStaleIssuer
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO api_keys (id, owner_id, name, hash, hint, actions, bound_worker_id, created_at, expires_at, revoked_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			k.ID, k.OwnerID, k.Name, k.Hash, k.Hint, strings.Join(k.Actions, ","),
			nullString(k.BoundWorkerID), k.CreatedAt, nullInt(k.ExpiresAt), nullInt(k.RevokedAt)); err != nil {
			return fmt.Errorf("store: create API key: %w", err)
		}
		for _, sc := range k.Scopes {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO api_key_scopes (key_id, kind, user_id, worker_id, session_name) VALUES (?, ?, ?, ?, ?)`,
				k.ID, sc.Kind, sc.UserID, sc.WorkerID, sc.SessionName); err != nil {
				return fmt.Errorf("store: create API key scope: %w", err)
			}
		}
		if audit != nil {
			return s.InsertAudit(ctx, tx, *audit)
		}
		return nil
	})
}

// RecordAudit writes one audit row in a transaction of its own. It is for
// attempts that change nothing and so have no transaction to join, such as a
// refused key creation.
func (s *Store) RecordAudit(ctx context.Context, row AuditRow) error {
	return s.InsertAudit(ctx, nil, row)
}

// APIKeyByHash returns the key whose SHA-256 is hash, or [ErrAPIKeyNotFound].
// It does not judge validity (expiry, revocation, owner); the apikey package
// does, on every call.
func (s *Store) APIKeyByHash(ctx context.Context, hash []byte) (APIKeyRow, error) {
	return s.oneAPIKey(ctx, `WHERE hash = ?`, hash)
}

// APIKeyByID returns the key with the given id, or [ErrAPIKeyNotFound].
func (s *Store) APIKeyByID(ctx context.Context, id string) (APIKeyRow, error) {
	return s.oneAPIKey(ctx, `WHERE id = ?`, id)
}

// ListAPIKeys returns the keys of ownerID, newest first, or the keys of every
// owner when ownerID is empty.
func (s *Store) ListAPIKeys(ctx context.Context, ownerID string) ([]APIKeyRow, error) {
	const newestFirst = ` ORDER BY created_at DESC, id DESC`
	if ownerID == "" {
		return s.queryAPIKeys(ctx, newestFirst)
	}
	return s.queryAPIKeys(ctx, `WHERE owner_id = ?`+newestFirst, ownerID)
}

func (s *Store) oneAPIKey(ctx context.Context, where string, arg any) (APIKeyRow, error) {
	keys, err := s.queryAPIKeys(ctx, where+` LIMIT 1`, arg)
	if err != nil {
		return APIKeyRow{}, err
	}
	if len(keys) == 0 {
		return APIKeyRow{}, ErrAPIKeyNotFound
	}
	return keys[0], nil
}

// queryAPIKeys reads the keys selected by tail (the WHERE, ORDER BY and LIMIT
// clauses) and then their scope rows. The two reads are separate
// statements: a key row never changes after creation except for revoked_at and last_used_at,
// and a scope row never changes at all, so no transaction is needed.
func (s *Store) queryAPIKeys(ctx context.Context, tail string, args ...any) ([]APIKeyRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_id, name, hash, hint, actions, bound_worker_id, created_at, expires_at, revoked_at, last_used_at
FROM api_keys `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read API keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		keys  []APIKeyRow
		index = map[string]int{}
	)
	for rows.Next() {
		var (
			k                    APIKeyRow
			actions              string
			bound                sql.NullString
			expires, revokedTime sql.NullInt64
			lastUsed             sql.NullInt64
		)
		if err := rows.Scan(&k.ID, &k.OwnerID, &k.Name, &k.Hash, &k.Hint, &actions, &bound, &k.CreatedAt, &expires, &revokedTime, &lastUsed); err != nil {
			return nil, fmt.Errorf("store: read API key: %w", err)
		}
		k.Actions = strings.Split(actions, ",")
		k.BoundWorkerID, k.ExpiresAt, k.RevokedAt = bound.String, expires.Int64, revokedTime.Int64
		k.LastUsedAt = lastUsed.Int64
		index[k.ID] = len(keys)
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read API keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: read API keys: %w", err)
	}
	for i := range keys {
		scopes, err := s.apiKeyScopes(ctx, keys[i].ID)
		if err != nil {
			return nil, err
		}
		keys[i].Scopes = scopes
	}
	return keys, nil
}

func (s *Store) apiKeyScopes(ctx context.Context, keyID string) ([]APIKeyScopeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, user_id, worker_id, session_name FROM api_key_scopes WHERE key_id = ? ORDER BY kind, user_id, worker_id, session_name`, keyID)
	if err != nil {
		return nil, fmt.Errorf("store: read API key scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []APIKeyScopeRow
	for rows.Next() {
		var sc APIKeyScopeRow
		if err := rows.Scan(&sc.Kind, &sc.UserID, &sc.WorkerID, &sc.SessionName); err != nil {
			return nil, fmt.Errorf("store: read API key scope: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read API key scopes: %w", err)
	}
	return out, nil
}

// RevokeAPIKey marks the key revoked at atMS together with the optional audit
// row, in one transaction. It reports whether this call revoked it: a key that
// is already revoked keeps its first revocation time and reports false. An
// unknown id is [ErrAPIKeyNotFound]; the audit row is written only when the key
// was revoked by this call.
func (s *Store) RevokeAPIKey(ctx context.Context, id string, atMS int64, audit *AuditRow) (bool, error) {
	var changed bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, atMS, id)
		if err != nil {
			return fmt.Errorf("store: revoke API key: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: revoke API key: %w", err)
		}
		if n == 0 {
			var one int
			switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, id).Scan(&one); {
			case errors.Is(err, sql.ErrNoRows):
				return ErrAPIKeyNotFound
			case err != nil:
				return fmt.Errorf("store: revoke API key: %w", err)
			}
			return nil // already revoked
		}
		changed = true
		if audit != nil {
			return s.InsertAudit(ctx, tx, *audit)
		}
		return nil
	})
	return changed, err
}

// LastUseGranularityMS is how late last_used_at may be: [Store.TouchAPIKey]
// writes a key at most once per this many milliseconds.
const LastUseGranularityMS = 60_000

// TouchAPIKey records that the key authenticated at atMS and reports whether it
// wrote. A key whose last use is less than [LastUseGranularityMS] older than
// atMS is left alone (no write), and a time before the recorded one never moves
// it back. It is not audited: it is bookkeeping, not an event.
func (s *Store) TouchAPIKey(ctx context.Context, id string, atMS int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE api_keys SET last_used_at = ?1
WHERE id = ?2 AND (last_used_at IS NULL OR last_used_at <= ?1 - ?3)`, atMS, id, LastUseGranularityMS)
	if err != nil {
		return false, fmt.Errorf("store: record API key use: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: record API key use: %w", err)
	}
	return n > 0, nil
}

// RevokeAPIKeysBoundToWorker revokes every live key bound to the Worker. It
// runs on ex, the open transaction of the caller, so that reassigning an
// infrastructure Worker revokes its keys atomically (AD-11, AD-16, AD-17). It
// returns the number of keys revoked.
func (s *Store) RevokeAPIKeysBoundToWorker(ctx context.Context, ex Execer, workerID string, atMS int64) (int64, error) {
	return s.revokeWhere(ctx, ex, `bound_worker_id = ?`, workerID, atMS)
}

// RevokeAPIKeysOfOwner revokes every live key of the user, for the
// deactivation of an account (AD-10). It runs on ex like
// [Store.RevokeAPIKeysBoundToWorker].
func (s *Store) RevokeAPIKeysOfOwner(ctx context.Context, ex Execer, ownerID string, atMS int64) (int64, error) {
	return s.revokeWhere(ctx, ex, `owner_id = ?`, ownerID, atMS)
}

func (s *Store) revokeWhere(ctx context.Context, ex Execer, cond string, arg string, atMS int64) (int64, error) {
	if ex == nil {
		ex = s.db
	}
	res, err := ex.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE revoked_at IS NULL AND `+cond, atMS, arg)
	if err != nil {
		return 0, fmt.Errorf("store: revoke API keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: revoke API keys: %w", err)
	}
	return n, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
