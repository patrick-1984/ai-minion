package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Errors of the enrollment API. Test with [errors.Is].
var (
	// ErrNotEnrolled is returned when the Worker has no Hub enrollment.
	ErrNotEnrolled = errors.New("store: the Worker is not enrolled with a Hub")
	// ErrPinRegression is returned by [Store.UpdatePin] for a pin that does
	// not move forward in the Hub's transition chain: a lower sequence number,
	// or the same number with another key (AD-5).
	ErrPinRegression = errors.New("store: the new Hub pin does not follow the stored pin")
	// ErrInvalidEnrollment is returned (wrapped) for an enrollment without a
	// 32-byte Hub key.
	ErrInvalidEnrollment = errors.New("store: invalid enrollment")
)

// Enrollment is the stored pairing of the Worker with its Hub: the pin and
// what the Hub reported in welcome.
type Enrollment struct {
	// HubPub is the pinned 32-byte Ed25519 key of the Hub.
	HubPub ed25519.PublicKey
	// PinSeq is the position of the pin in the Hub's transition chain; 0
	// means unknown (a fresh pin from an invitation or a pairing).
	PinSeq uint64
	// WorkerID, Name, Owner and Kind come from welcome. They are empty until
	// the first welcome (enrollment commits the pin first).
	WorkerID string
	Name     string
	Owner    string
	// OwnerID is the stable Hub id of the owner (Owner is its display name);
	// "" from a Hub that does not send it.
	OwnerID string
	Kind    string
	// PreviewOnly is the Hub-side policy from the last welcome. The Worker
	// combines it with its local policy, and local restrictions always win.
	PreviewOnly bool
	// MaxSerial and CertSHA256 are the certificate ratchet scoped to HubPub:
	// the highest Hub certificate serial seen and the SHA-256 of the
	// certificate at that serial (nil when none). A pin from 0.6 has 0 and nil.
	MaxSerial  uint64
	CertSHA256 []byte
	// HubCheck is how the pinned Hub was checked (HubCheck* constants; ""
	// is stored as none), HubCheckAt when it was set (zero for never) and
	// HubCheckAnchor the full fingerprint the operator checked ("" when none
	// was). A new enrollment sets all three; a pin update keeps them.
	HubCheck       string
	HubCheckAt     time.Time
	HubCheckAnchor string
	// HubCheckLineage are the full fingerprints, separated by spaces, of the
	// keys the pin moved through by signed transitions since the anchor was
	// checked (the current pin not included). A pin update with an anchor
	// adds to it; a new anchor empties it.
	HubCheckLineage string
	// EnrolledAt is when the enrollment was first stored. It is set by
	// [Store.SaveEnrollment] and ignored on input.
	EnrolledAt time.Time
}

// Values of Enrollment.HubCheck.
const (
	// HubCheckNone: first use, or enrolled by a version before 0.11.
	HubCheckNone = "none"
	// HubCheckFingerprint: the pinned root matched a fingerprint the operator
	// gave.
	HubCheckFingerprint = "fingerprint"
	// HubCheckInvite: the pinned root matched the invitation key.
	HubCheckInvite = "invite"
	// HubCheckMismatch: the pinned root did not match a fingerprint the
	// operator gave. The Worker does not connect.
	HubCheckMismatch = "mismatch"
)

// ValidHubCheck reports whether v is one of the HubCheck* values.
func ValidHubCheck(v string) bool {
	return v == HubCheckNone || v == HubCheckFingerprint || v == HubCheckInvite || v == HubCheckMismatch
}

func hubCheckOrNone(v string) string {
	if v == "" {
		return HubCheckNone
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// WelcomeInfo is what a welcome frame tells the Worker about itself.
type WelcomeInfo struct {
	WorkerID    string
	Name        string
	Owner       string
	OwnerID     string
	Kind        string
	PreviewOnly bool
}

// SaveEnrollment stores the pin and the welcome data of e in one statement,
// replacing an earlier enrollment. Enrollment calls it before it reports
// success (AD-7), so the pairing survives a crash right after.
func (s *Store) SaveEnrollment(ctx context.Context, e Enrollment) error {
	if len(e.HubPub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: the Hub key must be %d bytes", ErrInvalidEnrollment, ed25519.PublicKeySize)
	}
	if len(e.CertSHA256) != 0 && len(e.CertSHA256) != 32 {
		return fmt.Errorf("%w: the certificate hash must be 32 bytes", ErrInvalidEnrollment)
	}
	check := hubCheckOrNone(e.HubCheck)
	if !ValidHubCheck(check) {
		return fmt.Errorf("%w: unknown hub check %q", ErrInvalidEnrollment, e.HubCheck)
	}
	now := s.clock.now().UnixMilli()
	checkAt := nullTime(e.HubCheckAt)
	if checkAt == nil && check != HubCheckNone {
		checkAt = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save enrollment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO hub_enrollment
		   (id, hub_pub, pin_seq, worker_id, worker_name, owner, owner_id, kind, preview_only, enrolled_at, updated_at,
		    max_serial, cert_sha256, hub_check, hub_check_at, hub_check_anchor, hub_check_lineage)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   hub_pub = excluded.hub_pub, pin_seq = excluded.pin_seq, worker_id = excluded.worker_id,
		   worker_name = excluded.worker_name, owner = excluded.owner, owner_id = excluded.owner_id, kind = excluded.kind,
		   preview_only = excluded.preview_only, enrolled_at = excluded.enrolled_at,
		   updated_at = excluded.updated_at, max_serial = excluded.max_serial,
		   cert_sha256 = excluded.cert_sha256, hub_check = excluded.hub_check,
		   hub_check_at = excluded.hub_check_at, hub_check_anchor = excluded.hub_check_anchor,
		   hub_check_lineage = excluded.hub_check_lineage`,
		[]byte(e.HubPub), int64(e.PinSeq), e.WorkerID, e.Name, e.Owner, e.OwnerID, e.Kind, boolInt(e.PreviewOnly), now, now,
		int64(e.MaxSerial), nullBytes(e.CertSHA256), check, checkAt, e.HubCheckAnchor, e.HubCheckLineage)
	if err != nil {
		return fmt.Errorf("store: save enrollment: %w", err)
	}
	// The enrollment commit ends a pairing: the resume record goes in the same
	// transaction (design section 12.2).
	if _, err := tx.ExecContext(ctx, `DELETE FROM pair_resume`); err != nil {
		return fmt.Errorf("store: save enrollment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: save enrollment: %w", err)
	}
	return nil
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// HasEnrollment reports whether an enrollment is stored. It reads only the
// row's existence, so it works on the schema of every release (also on a
// read-only store before the migrations of this release ran).
func (s *Store) HasEnrollment(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM hub_enrollment WHERE id = 1`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: look for an enrollment: %w", err)
	}
	return n > 0, nil
}

// LoadEnrollment returns the stored enrollment, or [ErrNotEnrolled]. On a
// read-only store of an older release, whose schema has no owner_id or
// hub_check columns yet (the migrations of this release did not run), it
// returns the enrollment without them: a status or a preflight check never
// needs to migrate.
func (s *Store) LoadEnrollment(ctx context.Context) (Enrollment, error) {
	e, err := s.loadEnrollment(ctx, true, true)
	if noSuchColumn(err) {
		e, err = s.loadEnrollment(ctx, true, false)
	}
	if noSuchColumn(err) {
		e, err = s.loadEnrollment(ctx, false, false)
	}
	return e, err
}

func noSuchColumn(err error) bool {
	return err != nil && !errors.Is(err, ErrNotEnrolled) && strings.Contains(err.Error(), "no such column")
}

func (s *Store) loadEnrollment(ctx context.Context, withCheck, withOwnerID bool) (Enrollment, error) {
	var (
		e         Enrollment
		pub       []byte
		seq       int64
		preview   int64
		enrolled  int64
		maxSerial int64
		certHash  []byte
		checkAt   sql.NullInt64
	)
	cols := `hub_pub, pin_seq, worker_id, worker_name, owner, kind, preview_only, enrolled_at, max_serial, cert_sha256`
	dest := []any{&pub, &seq, &e.WorkerID, &e.Name, &e.Owner, &e.Kind, &preview, &enrolled, &maxSerial, &certHash}
	if withOwnerID {
		cols += `, owner_id`
		dest = append(dest, &e.OwnerID)
	}
	if withCheck {
		cols += `, hub_check, hub_check_at, hub_check_anchor, hub_check_lineage`
		dest = append(dest, &e.HubCheck, &checkAt, &e.HubCheckAnchor, &e.HubCheckLineage)
	}
	err := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM hub_enrollment WHERE id = 1`).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrNotEnrolled
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("store: load enrollment: %w", err)
	}
	e.HubPub = ed25519.PublicKey(pub)
	e.PinSeq = uint64(seq)
	e.MaxSerial = uint64(maxSerial)
	e.CertSHA256 = certHash
	e.PreviewOnly = preview != 0
	e.EnrolledAt = time.UnixMilli(enrolled).UTC()
	if checkAt.Valid {
		e.HubCheckAt = time.UnixMilli(checkAt.Int64).UTC()
	}
	return e, nil
}

// SetHubCheck records the result of a fingerprint check of the pinned Hub:
// check (a HubCheck* value) and anchor (the full fingerprint that was
// checked, "" for none). The update applies only while pub is still the
// pinned key, so a check of one pin never marks another; no matching row is
// [ErrNotEnrolled].
//
// A new anchor keeps the history: every stored anchor was the pinned key (or
// the key the pairing named) when it was checked, and the pin moves only
// along verified signed transitions, so the old anchor and its lineage are
// authenticated predecessors of the pin; the old anchor is added to the
// lineage. A check of the current pin therefore never erases a verified
// rotation (a setting that still names the old root stays a known
// predecessor). Only an empty anchor (no check) clears the lineage.
func (s *Store) SetHubCheck(ctx context.Context, pub ed25519.PublicKey, check, anchor string) error {
	if !ValidHubCheck(check) {
		return fmt.Errorf("%w: unknown hub check %q", ErrInvalidEnrollment, check)
	}
	now := s.clock.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE hub_enrollment SET hub_check = ?, hub_check_at = ?, updated_at = ?,
		   hub_check_lineage = CASE
		     WHEN hub_check_anchor = ? THEN hub_check_lineage
		     WHEN ? = '' THEN ''
		     WHEN hub_check_anchor = '' THEN hub_check_lineage
		     WHEN instr(' ' || hub_check_lineage || ' ', ' ' || hub_check_anchor || ' ') > 0 THEN hub_check_lineage
		     ELSE trim(hub_check_anchor || ' ' || hub_check_lineage) END,
		   hub_check_anchor = ?
		 WHERE id = 1 AND hub_pub = ?`,
		check, now, now, anchor, anchor, anchor, []byte(pub))
	if err != nil {
		return fmt.Errorf("store: set hub check: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotEnrolled
	}
	return nil
}

// UpdatePin rewrites the pin atomically after a transition chain was applied
// (AD-5). The new pin must move forward: a higher sequence number, or exactly
// the stored pin (a no-op). A lower number, or the same number with another
// key, is [ErrPinRegression] and nothing changes. [ErrNotEnrolled] when there
// is no enrollment.
//
// passed are the keys between the stored pin and pub that the verified chain
// went through; with a checked anchor, the stored pin and passed are added to
// the lineage.
func (s *Store) UpdatePin(ctx context.Context, pub ed25519.PublicKey, seq uint64, passed ...ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: the Hub key must be %d bytes", ErrInvalidEnrollment, ed25519.PublicKeySize)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		cur    []byte
		curSeq int64
	)
	err = tx.QueryRowContext(ctx, `SELECT hub_pub, pin_seq FROM hub_enrollment WHERE id = 1`).Scan(&cur, &curSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotEnrolled
	}
	if err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	switch {
	case int64(seq) < curSeq, int64(seq) == curSeq && !bytes.Equal(cur, pub):
		return ErrPinRegression
	case int64(seq) == curSeq:
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE hub_enrollment SET hub_pub = ?, pin_seq = ?, updated_at = ? WHERE id = 1`,
		[]byte(pub), int64(seq), s.clock.now().UnixMilli()); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	if !bytes.Equal(cur, pub) {
		if err := addLineage(ctx, tx, cur, passed); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	return nil
}

// UpdatePinRatchet is the commit of step 8 of the handshake (connect mode): in
// ONE transaction it moves the pin forward (as [Store.UpdatePin] does) and
// records the certificate ratchet. A pin that changed the root starts a new
// scope: max_serial and the hash are replaced by the given ones. Under the same
// root max_serial never goes down: a higher serial replaces serial and hash, an
// equal serial keeps the stored hash (or takes the given one when none is
// stored), a lower serial changes nothing. A commit is needed also when only the
// serial rose. [ErrNotEnrolled] when there is no enrollment.
//
// passed are the keys between the stored pin and pub that the verified chain
// went through; with a checked anchor, the stored pin and passed are added to
// the lineage in the same transaction.
func (s *Store) UpdatePinRatchet(ctx context.Context, pub ed25519.PublicKey, seq, maxSerial uint64, certSHA256 []byte, passed ...ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: the Hub key must be %d bytes", ErrInvalidEnrollment, ed25519.PublicKeySize)
	}
	if len(certSHA256) != 0 && len(certSHA256) != 32 {
		return fmt.Errorf("%w: the certificate hash must be 32 bytes", ErrInvalidEnrollment)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		cur     []byte
		curSeq  int64
		curMax  int64
		curHash []byte
	)
	err = tx.QueryRowContext(ctx, `SELECT hub_pub, pin_seq, max_serial, cert_sha256 FROM hub_enrollment WHERE id = 1`).
		Scan(&cur, &curSeq, &curMax, &curHash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotEnrolled
	}
	if err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	if int64(seq) < curSeq || int64(seq) == curSeq && !bytes.Equal(cur, pub) {
		return ErrPinRegression
	}
	newMax, newHash := curMax, curHash
	if !bytes.Equal(cur, pub) {
		newMax, newHash = int64(maxSerial), certSHA256 // a new root: a new scope
	} else {
		switch {
		case int64(maxSerial) > curMax:
			newMax, newHash = int64(maxSerial), certSHA256
		case int64(maxSerial) == curMax && len(curHash) == 0:
			newHash = certSHA256
		}
	}
	if bytes.Equal(cur, pub) && int64(seq) == curSeq && newMax == curMax && bytes.Equal(newHash, curHash) {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE hub_enrollment SET hub_pub = ?, pin_seq = ?, max_serial = ?, cert_sha256 = ?, updated_at = ? WHERE id = 1`,
		[]byte(pub), int64(seq), newMax, nullBytes(newHash), s.clock.now().UnixMilli()); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	if !bytes.Equal(cur, pub) {
		if err := addLineage(ctx, tx, cur, passed); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	return nil
}

// UpdateWelcome stores what the latest welcome said: the Worker id, name,
// owner, kind and Hub policy. [ErrNotEnrolled] when there is no enrollment.
func (s *Store) UpdateWelcome(ctx context.Context, w WelcomeInfo) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE hub_enrollment SET worker_id = ?, worker_name = ?, owner = ?, owner_id = ?, kind = ?, preview_only = ?, updated_at = ?
		 WHERE id = 1`,
		w.WorkerID, w.Name, w.Owner, w.OwnerID, w.Kind, boolInt(w.PreviewOnly), s.clock.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: update welcome: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotEnrolled
	}
	return nil
}

// WipeEnrollment deletes the pin and the enrollment, and nothing else: the
// share selection, the session counter and the audit stay (a signed revoke
// keeps the sessions untouched, AD-9). It reports whether an enrollment
// existed and is idempotent.
func (s *Store) WipeEnrollment(ctx context.Context) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM hub_enrollment WHERE id = 1`)
	if err != nil {
		return false, fmt.Errorf("store: wipe enrollment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: wipe enrollment: %w", err)
	}
	return n > 0, nil
}

// addLineage appends the full fingerprints of old (the pin the chain started
// from) and passed (the keys it went through) to the lineage of a checked
// enrollment, inside the transaction of the pin update. An enrollment without
// an anchor has no lineage.
func addLineage(ctx context.Context, tx *sql.Tx, old []byte, passed []ed25519.PublicKey) error {
	var fps []string
	for _, k := range append([]ed25519.PublicKey{old}, passed...) {
		f, err := identity.FingerprintOf(k)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEnrollment, err)
		}
		fps = append(fps, string(f))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE hub_enrollment SET hub_check_lineage = trim(hub_check_lineage || ' ' || ?)
		 WHERE id = 1 AND hub_check_anchor != ''`, strings.Join(fps, " ")); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	return nil
}
