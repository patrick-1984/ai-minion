package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The two passphrase scopes of the Hub identity (hub-identity-v2 section
// 10.6). Each has its own failure counter, lock and hourly allowance.
const (
	ScopeWeb = "web"
	ScopeCLI = "cli"
)

// Lockout constants of section 10.6.
const (
	// LockEvery is the number of failures per lock step.
	LockEvery = 5
	// LockBase and LockMax are the first and the longest lock, milliseconds.
	LockBase = int64(15 * 60 * 1000)
	LockMax  = int64(4 * 60 * 60 * 1000)
	// AllowanceWindow is the length of the hourly allowance window.
	AllowanceWindow = int64(60 * 60 * 1000)
)

// Audit actions written by the identity transactions.
const (
	ActionUnsealFailed = "hub_identity.unseal_failed"
	ActionLocked       = "hub_identity.locked"
)

var (
	// ErrIdentityState: the row is not in the state the transaction needs
	// (another change ran first, or the bootstrap state does not allow it).
	ErrIdentityState = errors.New("store: the Hub identity is not in the expected state")
	// ErrUnsealLocked and ErrUnsealAllowance are matched by errors.Is on a
	// *LimitError.
	ErrUnsealLocked    = errors.New("store: the passphrase scope is locked")
	ErrUnsealAllowance = errors.New("store: the hourly passphrase allowance is used up")
)

// LimitError says that a scope is locked or its hourly allowance is used up,
// and until when (unix milliseconds).
type LimitError struct {
	Locked bool
	Until  int64
}

// Error implements error.
func (e *LimitError) Error() string {
	if e.Locked {
		return "store: the passphrase scope is locked until " + strconv.FormatInt(e.Until, 10)
	}
	return "store: the hourly passphrase allowance is used up until " + strconv.FormatInt(e.Until, 10)
}

// Is matches ErrUnsealLocked or ErrUnsealAllowance.
func (e *LimitError) Is(target error) bool {
	if e.Locked {
		return target == ErrUnsealLocked
	}
	return target == ErrUnsealAllowance
}

// ScopeState is the persistent limit state of one scope.
type ScopeState struct {
	Failures int64
	// Pending: an attempt began and was not settled (see BeginUnsealAttempt).
	Pending     bool
	LockedUntil int64
	WindowStart int64
	WindowCount int64
}

// HubIdentityV2 is the whole hub_identity row after migration 0021.
type HubIdentityV2 struct {
	HubIdentityRow
	LastIssued     int64
	CurrentSerial  int64
	CurrentCert    []byte // nil, or 153 bytes
	RootSealed     bool
	RootFileSHA256 []byte // nil, or 32 bytes
	MigratedFrom06 bool
	Web, CLI       ScopeState
	LastOp         string
	LastOpAt       int64
	ExpiryWarnDay  int64
	ExpiredSerial  int64
}

const identityV2Cols = `pub, fingerprint, created_at, last_issued, current_serial, current_cert, root_sealed, root_file_sha256,
 migrated_from_06, unseal_failures, unseal_locked_until, unseal_window_start, unseal_window_count,
 cli_unseal_failures, cli_unseal_locked_until, cli_window_start, cli_window_count, last_op, last_op_at,
 expiry_warn_day, expired_serial, unseal_pending, cli_unseal_pending`

// HubIdentityV2 returns the whole identity row, or ok=false when none exists.
func (s *Store) HubIdentityV2(ctx context.Context) (HubIdentityV2, bool, error) {
	return readIdentityV2(ctx, s.db)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readIdentityV2(ctx context.Context, q queryRower) (HubIdentityV2, bool, error) {
	var (
		r              HubIdentityV2
		sealed, migr   int64
		wPend, cPend   int64
		cert, rootHash []byte
	)
	err := q.QueryRowContext(ctx, `SELECT `+identityV2Cols+` FROM hub_identity WHERE id = 1`).Scan(
		&r.Pub, &r.Fingerprint, &r.CreatedAt, &r.LastIssued, &r.CurrentSerial, &cert, &sealed, &rootHash,
		&migr, &r.Web.Failures, &r.Web.LockedUntil, &r.Web.WindowStart, &r.Web.WindowCount,
		&r.CLI.Failures, &r.CLI.LockedUntil, &r.CLI.WindowStart, &r.CLI.WindowCount, &r.LastOp, &r.LastOpAt,
		&r.ExpiryWarnDay, &r.ExpiredSerial, &wPend, &cPend)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return HubIdentityV2{}, false, nil
	case err != nil:
		return HubIdentityV2{}, false, fmt.Errorf("store: read hub identity: %w", err)
	}
	r.CurrentCert, r.RootFileSHA256 = cert, rootHash
	r.RootSealed, r.MigratedFrom06 = sealed != 0, migr != 0
	r.Web.Pending, r.CLI.Pending = wPend != 0, cPend != 0
	return r, true, nil
}

type scopeCols struct{ failures, locked, wstart, wcount, pending string }

func colsOf(scope string) (scopeCols, error) {
	switch scope {
	case ScopeWeb:
		return scopeCols{"unseal_failures", "unseal_locked_until", "unseal_window_start", "unseal_window_count", "unseal_pending"}, nil
	case ScopeCLI:
		return scopeCols{"cli_unseal_failures", "cli_unseal_locked_until", "cli_window_start", "cli_window_count", "cli_unseal_pending"}, nil
	}
	return scopeCols{}, fmt.Errorf("store: unknown passphrase scope %q", scope)
}

func (st HubIdentityV2) scope(scope string) ScopeState {
	if scope == ScopeCLI {
		return st.CLI
	}
	return st.Web
}

// ClearUnsealIntent clears the pending-attempt mark of scope: the attempt
// ended without a failure (the passphrase was right). It does not make the
// change durable; a lost clear only makes the next start count the attempt as
// a failure (conservative).
func (s *Store) ClearUnsealIntent(ctx context.Context, scope string) error {
	c, err := colsOf(scope)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE hub_identity SET `+c.pending+` = 0 WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("store: clear passphrase attempt intent: %w", err)
	}
	return nil
}

// BeginUnsealAttempt is the gate in front of the key derivation for scope.
// In one transaction: a locked scope (now < locked_until) answers *LimitError
// with Locked set and changes nothing; a used-up allowance (maxPerHour
// attempts inside the current window) answers *LimitError and changes
// nothing; otherwise the attempt is counted in the window and, in the same
// transaction, marked pending (the intent): an attempt that is never settled
// by RecordUnsealFailure or ClearUnsealIntent (a crash, a failed record) is
// counted as a failure at the next start. A request refused
// by a lock or by the allowance therefore consumes no allowance, an attempt
// that goes on to the derivation does. It returns ErrIdentityState when no
// identity row exists.
func (s *Store) BeginUnsealAttempt(ctx context.Context, scope string, nowMS int64, maxPerHour int64) error {
	c, err := colsOf(scope)
	if err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		row, ok, err := readIdentityV2(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIdentityState
		}
		st := row.scope(scope)
		if nowMS < st.LockedUntil {
			return &LimitError{Locked: true, Until: st.LockedUntil}
		}
		start, count := st.WindowStart, st.WindowCount
		if start == 0 || nowMS >= start+AllowanceWindow || nowMS < start {
			start, count = nowMS, 0
		}
		if count >= maxPerHour {
			return &LimitError{Until: start + AllowanceWindow}
		}
		_, err = tx.ExecContext(ctx, `UPDATE hub_identity SET `+c.wstart+` = ?, `+c.wcount+` = ?, `+c.pending+` = 1 WHERE id = 1`, start, count+1)
		if err != nil {
			return fmt.Errorf("store: count passphrase attempt: %w", err)
		}
		return nil
	})
}

// RecordUnsealFailure applies the failure transition of section 10.6 to scope
// and writes its audit rows in the same transaction: f = f + 1; when f is a
// multiple of LockEvery, L = now + min(LockBase * 2^(k-1), LockMax) with
// k = f / LockEvery; otherwise L is unchanged. It returns the new state.
func (s *Store) RecordUnsealFailure(ctx context.Context, scope string, nowMS int64, actor, channel string) (ScopeState, error) {
	c, err := colsOf(scope)
	if err != nil {
		return ScopeState{}, err
	}
	var out ScopeState
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		row, ok, err := readIdentityV2(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIdentityState
		}
		out = row.scope(scope)
		out.Failures++
		locked := false
		if out.Failures%LockEvery == 0 {
			k := out.Failures / LockEvery
			d := LockMax
			if k-1 < 8 {
				if v := LockBase << (k - 1); v < LockMax {
					d = v
				}
			}
			out.LockedUntil = nowMS + d
			locked = true
		}
		if _, err := tx.ExecContext(ctx, `UPDATE hub_identity SET `+c.failures+` = ?, `+c.locked+` = ?, `+c.pending+` = 0 WHERE id = 1`,
			out.Failures, out.LockedUntil); err != nil {
			return fmt.Errorf("store: record passphrase failure: %w", err)
		}
		target := "channel=" + channel + " failures=" + strconv.FormatInt(out.Failures, 10)
		if err := s.InsertAudit(ctx, tx, AuditRow{ID: NewID(), TS: nowMS, Actor: actor, Action: ActionUnsealFailed, Target: target, Outcome: "failed"}); err != nil {
			return err
		}
		if locked {
			target += " locked_until=" + strconv.FormatInt(out.LockedUntil, 10)
			return s.InsertAudit(ctx, tx, AuditRow{ID: NewID(), TS: nowMS, Actor: actor, Action: ActionLocked, Target: target, Outcome: "failed"})
		}
		return nil
	})
	return out, err
}

// CreateSealedIdentity inserts the identity of a pristine Hub (B0) already
// sealed: root_sealed = 1 and the hash of root.key.age, with its audit rows.
// It fails with ErrIdentityState when a row exists.
func (s *Store) CreateSealedIdentity(ctx context.Context, id HubIdentityRow, rootSHA256 []byte, nowMS int64, audit []AuditRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, ok, err := readIdentityV2(ctx, tx); err != nil {
			return err
		} else if ok {
			return ErrIdentityState
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO hub_identity (id, pub, fingerprint, created_at, root_sealed, root_file_sha256, last_op, last_op_at)
			 VALUES (1, ?, ?, ?, 1, ?, 'passphrase_set', ?)`, id.Pub, id.Fingerprint, id.CreatedAt, rootSHA256, nowMS); err != nil {
			return fmt.Errorf("store: record the sealed Hub identity: %w", err)
		}
		return s.insertAudits(ctx, tx, audit)
	})
}

// RecordPlainIdentity inserts a row for a plain identity.key found without a
// record (B1, root_sealed = 0, migrated from 0.6). It is idempotent for the
// same key and fails with ErrIdentityState for another one.
func (s *Store) RecordPlainIdentity(ctx context.Context, id HubIdentityRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		row, ok, err := readIdentityV2(ctx, tx)
		if err != nil {
			return err
		}
		if ok {
			if bytes.Equal(row.Pub, id.Pub) {
				return nil
			}
			return ErrIdentityState
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_identity (id, pub, fingerprint, created_at, migrated_from_06) VALUES (1, ?, ?, ?, 1)`,
			id.Pub, id.Fingerprint, id.CreatedAt)
		if err != nil {
			return fmt.Errorf("store: record the Hub identity: %w", err)
		}
		return nil
	})
}

// SealExistingIdentity records that the plain root of a B1 identity is now
// sealed (root_sealed = 1 and the file hash), with its audit rows. It fails
// with ErrIdentityState unless the row is the expected plain identity.
func (s *Store) SealExistingIdentity(ctx context.Context, expectPub, rootSHA256 []byte, nowMS int64, audit []AuditRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE hub_identity SET root_sealed = 1, root_file_sha256 = ?, last_op = 'passphrase_set', last_op_at = ?
			 WHERE id = 1 AND pub = ? AND root_sealed = 0`, rootSHA256, nowMS, expectPub)
		if err != nil {
			return fmt.Errorf("store: record the sealed root: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrIdentityState
		}
		return s.insertAudits(ctx, tx, audit)
	})
}

// RotationParams extends IssuanceParams for a master-key rotation.
type RotationParams struct {
	Transition TransitionRow
	Identity   HubIdentityRow // the new root
	// NewRootSHA256 is the hash of the new sealed root file.
	NewRootSHA256 []byte
	// MaxLinks bounds the chain; reaching it fails with ErrChainFull.
	MaxLinks int
}

// ErrChainFull is returned by a rotation when the chain has MaxLinks links.
var ErrChainFull = errors.New("store: the transition chain is full")

// IssuanceParams is one certificate commit (the transaction of step 5 of
// section 9.2).
type IssuanceParams struct {
	ExpectPub    []byte // the recorded root the caller based the issuance on
	ExpectRootSH []byte // the recorded sealed-root hash (checked in a rotation)
	Serial       int64
	Cert         []byte
	NowMS        int64
	// Scope is the passphrase scope whose counters reset; "" for none.
	Scope string
	// Op names the operation for the status page: "reissue" or "rotate".
	Op       string
	Rotation *RotationParams
	Audit    []AuditRow
}

// CommitIssuance stores a certificate: current_cert, current_serial and
// last_issued = serial (which must be higher than the recorded last_issued),
// resets the lockout counters of the scope, and for a rotation also appends
// the transition, replaces the identity, records the new sealed-root hash,
// clears migrated_from_06 and revokes every invitation key; the audit rows
// commit in the same transaction. It does not make the commit durable.
func (s *Store) CommitIssuance(ctx context.Context, p IssuanceParams) error {
	if len(p.Cert) != 153 || p.Serial <= 0 {
		return errors.New("store: malformed certificate commit")
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		row, ok, err := readIdentityV2(ctx, tx)
		if err != nil {
			return err
		}
		if !ok || !row.RootSealed || !bytes.Equal(row.Pub, p.ExpectPub) || row.LastIssued >= p.Serial {
			return ErrIdentityState
		}
		if r := p.Rotation; r != nil {
			if !bytes.Equal(row.RootFileSHA256, p.ExpectRootSH) {
				return ErrIdentityState
			}
			if err := s.applyRotation(ctx, tx, row, r); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE hub_identity SET current_cert = ?, current_serial = ?, last_issued = ?, last_op = ?, last_op_at = ? WHERE id = 1`,
			p.Cert, p.Serial, p.Serial, p.Op, p.NowMS); err != nil {
			return fmt.Errorf("store: store the certificate: %w", err)
		}
		if err := resetScope(ctx, tx, p.Scope); err != nil {
			return err
		}
		return s.insertAudits(ctx, tx, p.Audit)
	})
}

func (s *Store) applyRotation(ctx context.Context, tx *sql.Tx, row HubIdentityV2, r *RotationParams) error {
	t := r.Transition
	if !bytes.Equal(t.OldPub, row.Pub) || !bytes.Equal(t.NewPub, r.Identity.Pub) || bytes.Equal(t.OldPub, t.NewPub) {
		return errors.New("store: the rotation does not connect the recorded root to the new root")
	}
	var last, count int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM hub_key_transitions`).Scan(&last, &count); err != nil {
		return fmt.Errorf("store: read key transitions: %w", err)
	}
	if r.MaxLinks > 0 && int(count) >= r.MaxLinks {
		return ErrChainFull
	}
	if t.Seq != uint64(last)+1 {
		return ErrRotationConflict
	}
	var reused int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_key_transitions WHERE old_pub = ?1 OR new_pub = ?1`, t.NewPub).Scan(&reused); err != nil {
		return fmt.Errorf("store: read key transitions: %w", err)
	}
	if reused > 0 {
		return errors.New("store: the new Hub root was already part of the chain; a rotation needs a fresh key")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO hub_key_transitions (seq, old_pub, new_pub, created_at, sig_by_old) VALUES (?, ?, ?, ?, ?)`,
		int64(t.Seq), t.OldPub, t.NewPub, t.CreatedAt, t.SigByOld); err != nil {
		return fmt.Errorf("store: append key transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE hub_identity SET pub = ?, fingerprint = ?, created_at = ?, root_file_sha256 = ?, migrated_from_06 = 0 WHERE id = 1`,
		r.Identity.Pub, r.Identity.Fingerprint, r.Identity.CreatedAt, r.NewRootSHA256); err != nil {
		return fmt.Errorf("store: replace hub identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invitation_keys SET revoked = 1, revoked_at = COALESCE(revoked_at, ?) WHERE revoked = 0`, t.CreatedAt); err != nil {
		return fmt.Errorf("store: revoke invitation keys: %w", err)
	}
	return nil
}

// CommitPassphraseChange records the new sealed-root hash of a passphrase
// change (the same root, sealed again), resets the scope counters and writes
// the audit rows. It fails with ErrIdentityState unless the recorded hash is
// expectOld.
func (s *Store) CommitPassphraseChange(ctx context.Context, expectPub, expectOld, newSHA256 []byte, scope string, nowMS int64, audit []AuditRow) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE hub_identity SET root_file_sha256 = ?, last_op = 'passphrase_change', last_op_at = ?
			 WHERE id = 1 AND pub = ? AND root_sealed = 1 AND root_file_sha256 = ?`, newSHA256, nowMS, expectPub, expectOld)
		if err != nil {
			return fmt.Errorf("store: record the new sealed root: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrIdentityState
		}
		if err := resetScope(ctx, tx, scope); err != nil {
			return err
		}
		return s.insertAudits(ctx, tx, audit)
	})
}

// MarkExpiryWarning records, with its audit row, that the daily warning of
// UTC day (days since the epoch) was written. It reports false, writing
// nothing, when that day or a later one is already recorded.
func (s *Store) MarkExpiryWarning(ctx context.Context, day int64, audit AuditRow) (bool, error) {
	return s.markOnce(ctx, `UPDATE hub_identity SET expiry_warn_day = ? WHERE id = 1 AND expiry_warn_day < ?`, audit, day, day)
}

// MarkExpired records, with its audit row, that the expiry of the certificate
// with this serial was reported. It reports false, writing nothing, when it
// already was or when this serial is no longer the current certificate (a
// re-issue replaced it meanwhile: the report would be about a certificate that
// no longer serves).
func (s *Store) MarkExpired(ctx context.Context, serial int64, audit AuditRow) (bool, error) {
	return s.markOnce(ctx, `UPDATE hub_identity SET expired_serial = ? WHERE id = 1 AND expired_serial < ? AND current_serial = ?`, audit, serial, serial, serial)
}

func (s *Store) markOnce(ctx context.Context, q string, audit AuditRow, args ...any) (bool, error) {
	done := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("store: mark identity event: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		done = true
		return s.InsertAudit(ctx, tx, audit)
	})
	return done, err
}

func resetScope(ctx context.Context, tx *sql.Tx, scope string) error {
	if scope == "" {
		return nil
	}
	c, err := colsOf(scope)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE hub_identity SET `+c.failures+` = 0, `+c.locked+` = 0, `+c.pending+` = 0 WHERE id = 1`); err != nil {
		return fmt.Errorf("store: reset passphrase counters: %w", err)
	}
	return nil
}

func (s *Store) insertAudits(ctx context.Context, tx *sql.Tx, rows []AuditRow) error {
	for _, r := range rows {
		if err := s.InsertAudit(ctx, tx, r); err != nil {
			return err
		}
	}
	return nil
}

// RecordedFingerprint reads the fingerprint of the Hub identity recorded in the
// database of dir, read-only and without migrating or creating anything. It
// returns "" when there is no database or no identity row. The
// `minion hub identity` command uses it once the private key file is gone.
func RecordedFingerprint(ctx context.Context, dir string) (string, error) {
	p := filepath.Join(dir, DBFile)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	db, err := sql.Open("sqlite", "file:"+urlPath(p)+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return "", fmt.Errorf("store: open the database read-only: %w", err)
	}
	defer func() { _ = db.Close() }()
	var fp string
	err = db.QueryRowContext(ctx, `SELECT fingerprint FROM hub_identity WHERE id = 1`).Scan(&fp)
	switch {
	case errors.Is(err, sql.ErrNoRows), err != nil && strings.Contains(err.Error(), "no such table"):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: read the recorded Hub identity: %w", err)
	}
	return fp, nil
}
