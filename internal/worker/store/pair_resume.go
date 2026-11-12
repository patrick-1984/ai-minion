package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoPairResume is returned by [Store.LoadPairResume] when no record exists.
var ErrNoPairResume = errors.New("store: no pairing resume record")

// PairResume is the pairing resume record (design hub-identity-v2 section 12.2):
// the code and its public id and expiry, with the authenticated candidate root
// and its certificate ratchet from the handshake that authenticated the Hub.
// ChainPosition is always 0: a root claimed by a pairing starts a chain.
type PairResume struct {
	Code          string
	PairingID     string
	ExpiresAt     time.Time
	RootPub       ed25519.PublicKey
	ChainPosition uint64
	Serial        uint64
	CertSHA256    []byte
}

// SavePairResume stores the record, replacing an earlier one. The commit is
// durable when it returns (the database syncs the log on every commit): the
// caller sends sig_worker only afterwards.
func (s *Store) SavePairResume(ctx context.Context, r PairResume) error {
	if len(r.RootPub) != ed25519.PublicKeySize || len(r.CertSHA256) != 32 || r.Serial < 1 || r.Code == "" || r.PairingID == "" {
		return fmt.Errorf("%w: incomplete pairing resume record", ErrInvalidEnrollment)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pair_resume (id, code, pairing_id, expires_at, root_pub, chain_position, serial, cert_sha256)
		 VALUES (1, ?, ?, ?, ?, 0, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET code = excluded.code, pairing_id = excluded.pairing_id,
		   expires_at = excluded.expires_at, root_pub = excluded.root_pub, chain_position = 0,
		   serial = excluded.serial, cert_sha256 = excluded.cert_sha256`,
		r.Code, r.PairingID, r.ExpiresAt.UnixMilli(), []byte(r.RootPub), int64(r.Serial), r.CertSHA256)
	if err != nil {
		return fmt.Errorf("store: save pairing resume record: %w", err)
	}
	return nil
}

// LoadPairResume returns the record, or [ErrNoPairResume].
func (s *Store) LoadPairResume(ctx context.Context) (PairResume, error) {
	var (
		r      PairResume
		exp    int64
		pos    int64
		serial int64
		root   []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT code, pairing_id, expires_at, root_pub, chain_position, serial, cert_sha256 FROM pair_resume WHERE id = 1`).
		Scan(&r.Code, &r.PairingID, &exp, &root, &pos, &serial, &r.CertSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return PairResume{}, ErrNoPairResume
	}
	if err != nil {
		return PairResume{}, fmt.Errorf("store: load pairing resume record: %w", err)
	}
	r.ExpiresAt = time.UnixMilli(exp).UTC()
	r.RootPub = ed25519.PublicKey(root)
	r.ChainPosition, r.Serial = uint64(pos), uint64(serial)
	return r, nil
}

// DeletePairResume removes the record and reports whether there was one.
func (s *Store) DeletePairResume(ctx context.Context) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pair_resume`)
	if err != nil {
		return false, fmt.Errorf("store: delete pairing resume record: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
