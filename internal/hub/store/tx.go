package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tx runs fn in one transaction on db and commits it when fn returns nil. It
// rolls back when fn returns an error or panics; a panic is re-raised after
// the rollback. The error of fn is returned unchanged.
//
// The transaction takes the write lock when it begins when db was opened
// with _txlock=immediate, as [Open] does, so a multi-row invariant (AD-16)
// never fails half way on a lock upgrade.
func Tx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	// Unconditional rollback: it does nothing after Commit, and it releases the
	// write lock when fn ends through runtime.Goexit (t.Fatal, t.FailNow),
	// where recover() below returns nil.
	defer func() { _ = tx.Rollback() }()
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// Tx runs fn in one immediate transaction of the store; see the package
// function [Tx].
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return Tx(ctx, s.db, fn)
}
