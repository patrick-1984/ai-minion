package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Invitation modes and TTL labels as stored (AD-7). The workers package owns
// their meaning; the CHECK constraints of 0005_invitations.sql repeat them.
const (
	InvitationOneTime  = "one_time"
	InvitationReusable = "reusable"
)

// Sources of an invitation row. The row of the Hub-wide key file
// (MINION_INFRA_INVITE_KEY_FILE) enrolls only while the file holds its secret.
const (
	InvitationSourceUser      = "user"
	InvitationSourceInfraFile = "infra_file"
)

// Errors of the invitation tables. Callers match them with errors.Is.
var (
	// ErrInvitationNotFound: no invitation key has this id or secret hash.
	ErrInvitationNotFound = errors.New("store: invitation key not found")
	// ErrInvitationRevoked: the invitation key was revoked.
	ErrInvitationRevoked = errors.New("store: invitation key was revoked")
	// ErrInvitationExpired: the invitation key's TTL ran out.
	ErrInvitationExpired = errors.New("store: invitation key has expired")
	// ErrInvitationExhausted: the invitation key has no uses left.
	ErrInvitationExhausted = errors.New("store: invitation key has no uses left")
	// ErrWorkerTombstoned: the Worker key was removed (AD-9) and cannot enroll
	// again.
	ErrWorkerTombstoned = errors.New("store: the Worker key was removed")
)

// maxNameAttempts bounds the "-2", "-3" suffix search of one enrollment.
const maxNameAttempts = 10000

// InvitationRow is one invitation key. SecretHash is SHA-256 of the secret;
// the secret itself is never stored.
type InvitationRow struct {
	ID         string // UUIDv7
	SecretHash []byte // 32 bytes
	Name       string
	Mode       string // InvitationOneTime or InvitationReusable
	Kind       string // WorkerKindPrivate or WorkerKindInfrastructure
	TTL        string // 1h, 4h, 8h, 24h, 1w, 1m or unlimited
	CreatorID  string // the owner of the Workers it enrolls
	CreatedAt  int64  // unix milliseconds
	ExpiresAt  int64  // unix milliseconds, 0 for ttl unlimited
	Uses       int    // enrollments consumed so far
	MaxUses    int    // 1 for one_time, 0 (no limit) for reusable
	Revoked    bool
	RevokedAt  int64 // unix milliseconds, 0 when not revoked
	// Source is InvitationSourceUser (the default) or InvitationSourceInfraFile.
	Source string
	// Notice is the owner-visible text a migration left on a key it limited.
	Notice string
}

const invitationColumns = `id, secret_hash, name, mode, kind, ttl, creator_id, created_at,
    COALESCE(expires_at, 0), uses, COALESCE(max_uses, 0), revoked, COALESCE(revoked_at, 0), source, notice`

func scanInvitation(row interface{ Scan(...any) error }) (InvitationRow, error) {
	var (
		r       InvitationRow
		revoked int
	)
	err := row.Scan(&r.ID, &r.SecretHash, &r.Name, &r.Mode, &r.Kind, &r.TTL, &r.CreatorID, &r.CreatedAt,
		&r.ExpiresAt, &r.Uses, &r.MaxUses, &revoked, &r.RevokedAt, &r.Source, &r.Notice)
	r.Revoked = revoked != 0
	return r, err
}

func invitationErr(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return ErrInvitationNotFound
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// InsertInvitation adds an invitation key. Uses and the revoked flag of row
// are ignored: a new key is unused and not revoked.
func (s *Store) InsertInvitation(ctx context.Context, r InvitationRow) error {
	var expires, maxUses any
	if r.ExpiresAt != 0 {
		expires = r.ExpiresAt
	}
	if r.MaxUses != 0 {
		maxUses = r.MaxUses
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invitation_keys (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, expires_at, max_uses)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SecretHash, r.Name, r.Mode, r.Kind, r.TTL, r.CreatorID, r.CreatedAt, expires, maxUses)
	// Source and Notice are not inserted: a key made through this method is a
	// user key (the column default); the key file has [Store.EnsureInfraKey].
	if err != nil {
		return fmt.Errorf("store: insert invitation key: %w", err)
	}
	return nil
}

// InvitationByID returns the invitation key with the given id, or
// ErrInvitationNotFound.
func (s *Store) InvitationByID(ctx context.Context, id string) (InvitationRow, error) {
	r, err := scanInvitation(s.db.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitation_keys WHERE id = ?`, id))
	return r, invitationErr("read invitation key", err)
}

// ListInvitations returns the invitation keys of creatorID, newest first; an
// empty creatorID returns every key (the admin view).
func (s *Store) ListInvitations(ctx context.Context, creatorID string) ([]InvitationRow, error) {
	return s.ListInvitationsWithInfra(ctx, creatorID, false)
}

// ListInvitationsWithInfra is [Store.ListInvitations] that can add the row of
// the Hub-wide key file to the keys of one creator (the admin's own view).
func (s *Store) ListInvitationsWithInfra(ctx context.Context, creatorID string, withInfra bool) ([]InvitationRow, error) {
	q := `SELECT ` + invitationColumns + ` FROM invitation_keys`
	var args []any
	if creatorID != "" {
		q += ` WHERE (creator_id = ? AND source = 'user')`
		args = append(args, creatorID)
		if withInfra {
			q += ` OR source = 'infra_file'`
		}
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list invitation keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []InvitationRow
	for rows.Next() {
		r, err := scanInvitation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan invitation key: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list invitation keys: %w", err)
	}
	return out, nil
}

// RevokeInvitation sets the revoked flag. It is idempotent: revoking a revoked
// key keeps the first revocation time. It returns ErrInvitationNotFound for an
// unknown id. Workers that already enrolled are not touched.
func (s *Store) RevokeInvitation(ctx context.Context, id string, nowMS int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invitation_keys SET revoked = 1, revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, nowMS, id)
	if err != nil {
		return fmt.Errorf("store: revoke invitation key: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrInvitationNotFound
	}
	return nil
}

// EnrollResult is the outcome of an enrollment transaction.
type EnrollResult struct {
	// Worker is the enrolled Worker: the new row, or the existing one when
	// Existing is set.
	Worker WorkerRow
	// Existing is true when the Worker key was already enrolled; nothing was
	// consumed or written then.
	Existing bool
	// InvitationID is the id of the invitation key that was used or checked;
	// empty for the Hub-wide infrastructure key.
	InvitationID string
}

// EnrollParams describes one enrollment with a stored invitation key.
type EnrollParams struct {
	// SecretHash is SHA-256 of the presented invitation secret.
	SecretHash []byte
	// Pub is the raw public key of the enrolling Worker.
	Pub []byte
	// Name is the Worker's wanted name, already cleaned; a collision within
	// the owner's space gets the suffix -2, -3, and so on.
	Name string
	// WorkerID is the UUIDv7 of the new Worker row.
	WorkerID string
	// Now is the current time in unix milliseconds.
	Now int64
	// Audit, when not nil, is written in the same transaction when a Worker
	// row is created (not for an Existing result).
	Audit *AuditRow
	// Addr is the client address the Worker enrolled from; the approval shows
	// it.
	Addr string
	// Infra says the presented secret is the one the Hub-wide key file holds
	// now. Only then may a row of source infra_file be used (a secret that is
	// no longer in the file is withdrawn), and a user row is refused for it.
	Infra bool
	// OwnerID is the owner of the Worker for an Infra enrollment
	// (infra_default_owner, resolved by the caller).
	OwnerID string
}

// EnrollWithInvitation runs the AD-7 enrollment in one transaction:
//
//  1. the invitation key is looked up by the hash of its secret
//     (ErrInvitationNotFound when unknown);
//  2. a removed Worker key is refused with ErrWorkerTombstoned;
//  3. a Worker key that is already enrolled returns the existing row and
//     consumes nothing, whatever the state of the key (the retry after a
//     lost reply);
//  4. the key must not be revoked (ErrInvitationRevoked), expired
//     (ErrInvitationExpired) and must have a use left
//     (ErrInvitationExhausted);
//  5. one use is consumed and the Worker is created PENDING (F-02) with owner
//     = creator of the key (OwnerID for the key file) and kind from the key.
//
// Two Workers racing on a one-time key cannot both pass step 4 because every
// transaction takes the write lock when it begins (AD-16).
func (s *Store) EnrollWithInvitation(ctx context.Context, p EnrollParams) (EnrollResult, error) {
	var res EnrollResult
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		inv, err := scanInvitation(tx.QueryRowContext(ctx,
			`SELECT `+invitationColumns+` FROM invitation_keys WHERE secret_hash = ?`, p.SecretHash))
		if err := invitationErr("read invitation key", err); err != nil {
			return err
		}
		if (inv.Source == InvitationSourceInfraFile) != p.Infra {
			return ErrInvitationNotFound
		}
		res.InvitationID = inv.ID
		owner := inv.CreatorID
		if p.Infra {
			owner = p.OwnerID
		}
		if done, err := s.existingWorker(ctx, tx, p.Pub, &res); done || err != nil {
			return err
		}
		switch {
		case inv.Revoked:
			return ErrInvitationRevoked
		case inv.ExpiresAt != 0 && inv.ExpiresAt <= p.Now:
			return ErrInvitationExpired
		case inv.MaxUses != 0 && inv.Uses >= inv.MaxUses:
			return ErrInvitationExhausted
		}
		// AD-10: deactivation revokes sessions and keys. A key whose creator
		// is gone or no longer active enrolls nothing, and consumes no use.
		var active int
		switch err := tx.QueryRowContext(ctx, `SELECT active FROM users WHERE id = ?`, owner).Scan(&active); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrInvitationRevoked
		case err != nil:
			return fmt.Errorf("store: read invitation creator: %w", err)
		case active == 0:
			return ErrInvitationRevoked
		}
		// The guard repeats the checks above so that the counter can never
		// pass max_uses even if this statement ever ran outside the lock.
		r, err := tx.ExecContext(ctx,
			`UPDATE invitation_keys SET uses = uses + 1
			  WHERE id = ? AND revoked = 0 AND (max_uses IS NULL OR uses < max_uses)`, inv.ID)
		if err != nil {
			return fmt.Errorf("store: consume invitation use: %w", err)
		}
		if n, err := r.RowsAffected(); err != nil || n != 1 {
			return ErrInvitationExhausted
		}
		w, err := s.insertWorkerNamed(ctx, tx, WorkerRow{
			ID: p.WorkerID, Pub: p.Pub, Name: p.Name, OwnerID: owner, Kind: inv.Kind, CreatedAt: p.Now,
			Pending: true, EnrollAddr: p.Addr,
		})
		if err != nil {
			return err
		}
		res.Worker = w
		return s.auditEnroll(ctx, tx, p.Audit, w.ID)
	})
	if err != nil {
		// The invitation id stays known for the audit row of a refusal.
		return EnrollResult{InvitationID: res.InvitationID}, err
	}
	return res, nil
}

// EnsureInfraKey records the Hub-wide key file (MINION_INFRA_INVITE_KEY_FILE)
// as an invitation row keyed by the hash of its secret, and returns the row.
// The first load creates it with expiresAt (unix milliseconds, 0 for none);
// every later load returns the row as it is, so reloading the file never resets
// the expiry or the use counter and never undoes a revocation.
func (s *Store) EnsureInfraKey(ctx context.Context, hash []byte, expiresAt, nowMS int64) (InvitationRow, error) {
	var out InvitationRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var expires any
		if expiresAt != 0 {
			expires = expiresAt
		}
		ttl := "unlimited"
		if expiresAt != 0 {
			ttl = "1m"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO invitation_keys
			   (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, expires_at, max_uses, source)
			 VALUES (?, ?, 'infrastructure key file', 'reusable', 'infrastructure', ?, '', ?, ?, NULL, 'infra_file')`,
			NewID(), hash, ttl, nowMS, expires); err != nil {
			return fmt.Errorf("store: record the infrastructure key: %w", err)
		}
		r, err := scanInvitation(tx.QueryRowContext(ctx, `SELECT `+invitationColumns+` FROM invitation_keys WHERE secret_hash = ?`, hash))
		if err != nil {
			return invitationErr("read the infrastructure key", err)
		}
		if r.Source != InvitationSourceInfraFile {
			return errors.New("store: the infrastructure key file holds the secret of a user invitation key")
		}
		out = r
		return nil
	})
	return out, err
}

// existingWorker handles the tombstone and idempotency steps of an
// enrollment. done is true when the enrollment ends with res filled in.
func (s *Store) existingWorker(ctx context.Context, tx *sql.Tx, pub []byte, res *EnrollResult) (done bool, err error) {
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM worker_tombstones WHERE worker_pub = ?`, pub).Scan(&one); {
	case err == nil:
		return false, ErrWorkerTombstoned
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: read tombstone: %w", err)
	}
	w, err := scanWorker(tx.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE worker_pub = ?`, pub))
	switch {
	case err == nil:
		res.Worker, res.Existing = w, true
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	}
	return false, fmt.Errorf("store: read worker: %w", err)
}

// insertWorkerNamed inserts w inside tx, trying w.Name, then w.Name-2,
// w.Name-3 and so on until the owner has no Worker with that name. A failed
// INSERT does not abort a SQLite transaction, so the search stays in one.
func (s *Store) insertWorkerNamed(ctx context.Context, tx *sql.Tx, w WorkerRow) (WorkerRow, error) {
	base := w.Name
	for n := 1; n <= maxNameAttempts; n++ {
		w.Name = base
		if n > 1 {
			w.Name = base + "-" + strconv.Itoa(n)
		}
		err := s.InsertWorker(ctx, tx, w)
		switch {
		case err == nil:
			return w, nil
		case errors.Is(err, ErrWorkerNameTaken):
			continue
		}
		return WorkerRow{}, err
	}
	return WorkerRow{}, ErrWorkerNameTaken
}

func (s *Store) auditEnroll(ctx context.Context, tx *sql.Tx, a *AuditRow, workerID string) error {
	if a == nil {
		return nil
	}
	row := *a
	row.Target = workerID
	return s.InsertAudit(ctx, tx, row)
}

// AdminSeedMarker is the auth_meta key under which the admin seed records the
// id of the account it created (Story 3.5).
const AdminSeedMarker = "admin_seed"

// InfraOwnerID resolves the owner of Workers enrolled with the Hub-wide
// infrastructure key: the active user whose username or e-mail address is
// name, or, for an empty name, the seeded admin. It returns ErrNotFound when
// no such active user exists.
func (s *Store) InfraOwnerID(ctx context.Context, name string) (string, error) {
	var (
		id     string
		active int
		err    error
	)
	if name == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT u.id, u.active FROM auth_meta m JOIN users u ON u.id = m.value WHERE m.key = ?`, AdminSeedMarker).
			Scan(&id, &active)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, active FROM users WHERE username = ?1 OR email = ?1 ORDER BY (username = ?1) DESC LIMIT 1`, name).
			Scan(&id, &active)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("store: resolve infrastructure owner: %w", err)
	case active == 0:
		return "", ErrNotFound
	}
	return id, nil
}
