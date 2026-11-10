package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
	Kind     string
	// PreviewOnly is the Hub-side policy from the last welcome. The Worker
	// combines it with its local policy, and local restrictions always win.
	PreviewOnly bool
	// EnrolledAt is when the enrollment was first stored. It is set by
	// [Store.SaveEnrollment] and ignored on input.
	EnrolledAt time.Time
}

// WelcomeInfo is what a welcome frame tells the Worker about itself.
type WelcomeInfo struct {
	WorkerID    string
	Name        string
	Owner       string
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
	now := s.clock.now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO hub_enrollment
		   (id, hub_pub, pin_seq, worker_id, worker_name, owner, kind, preview_only, enrolled_at, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   hub_pub = excluded.hub_pub, pin_seq = excluded.pin_seq, worker_id = excluded.worker_id,
		   worker_name = excluded.worker_name, owner = excluded.owner, kind = excluded.kind,
		   preview_only = excluded.preview_only, enrolled_at = excluded.enrolled_at,
		   updated_at = excluded.updated_at`,
		[]byte(e.HubPub), int64(e.PinSeq), e.WorkerID, e.Name, e.Owner, e.Kind, boolInt(e.PreviewOnly), now, now)
	if err != nil {
		return fmt.Errorf("store: save enrollment: %w", err)
	}
	return nil
}

// LoadEnrollment returns the stored enrollment, or [ErrNotEnrolled].
func (s *Store) LoadEnrollment(ctx context.Context) (Enrollment, error) {
	var (
		e        Enrollment
		pub      []byte
		seq      int64
		preview  int64
		enrolled int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT hub_pub, pin_seq, worker_id, worker_name, owner, kind, preview_only, enrolled_at
		 FROM hub_enrollment WHERE id = 1`).
		Scan(&pub, &seq, &e.WorkerID, &e.Name, &e.Owner, &e.Kind, &preview, &enrolled)
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrNotEnrolled
	}
	if err != nil {
		return Enrollment{}, fmt.Errorf("store: load enrollment: %w", err)
	}
	e.HubPub = ed25519.PublicKey(pub)
	e.PinSeq = uint64(seq)
	e.PreviewOnly = preview != 0
	e.EnrolledAt = time.UnixMilli(enrolled).UTC()
	return e, nil
}

// UpdatePin rewrites the pin atomically after a transition chain was applied
// (AD-5). The new pin must move forward: a higher sequence number, or exactly
// the stored pin (a no-op). A lower number, or the same number with another
// key, is [ErrPinRegression] and nothing changes. [ErrNotEnrolled] when there
// is no enrollment.
func (s *Store) UpdatePin(ctx context.Context, pub ed25519.PublicKey, seq uint64) error {
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: update pin: %w", err)
	}
	return nil
}

// UpdateWelcome stores what the latest welcome said: the Worker id, name,
// owner, kind and Hub policy. [ErrNotEnrolled] when there is no enrollment.
func (s *Store) UpdateWelcome(ctx context.Context, w WelcomeInfo) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE hub_enrollment SET worker_id = ?, worker_name = ?, owner = ?, kind = ?, preview_only = ?, updated_at = ?
		 WHERE id = 1`,
		w.WorkerID, w.Name, w.Owner, w.Kind, boolInt(w.PreviewOnly), s.clock.now().UnixMilli())
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
