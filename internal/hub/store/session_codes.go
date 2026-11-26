package store

// Session codes (v0.23.0, migration 0039): every Worker has a number that the
// Hub gives once and never again, and every session of a Worker a number the
// Hub gives the first time it lists the session and never again within that
// Worker. Together they are the short code "5-667" (ops.SessionCode).
//
// The identity of a session is its instance id when the Worker reports one
// (tmux, v0.22.0): the number then follows a rename, and a session created
// again under the same name gets a new number. Without an instance id (psmux,
// older Workers) the name is the identity: a session
// created again under the same name keeps its number. A sighting with an
// instance id adopts the row an earlier sighting of the same name without one
// made, so both sources agree on the number.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionCodeTouchEvery is how stale last_seen of a mapping may get before a
// sighting writes it again; a listing that changes nothing writes nothing
// until then.
const SessionCodeTouchEvery = time.Hour

// SessionCodeRetention is how long a mapping is kept after its session was
// last seen. Its number is not given again after the prune.
const SessionCodeRetention = 30 * 24 * time.Hour

// MaxSessionCodesPerWorker bounds the mappings kept for one Worker: beyond
// it the ones seen least recently go (their numbers are not given again). It
// is the Hub's limit on the sessions of a Worker, twice.
const MaxSessionCodesPerWorker = 8192

// SessionSighting is one session of a Worker's listing.
type SessionSighting struct {
	Name string
	// InstanceID is the host-attested identity, "" when unknown.
	InstanceID string
}

type codeRow struct {
	number   int64
	iid      string // "" for NULL
	name     string // "" once another session took the name
	lastSeen int64
	// state of the row in this pass
	isNew, changed, deleted bool
}

// SessionCodes returns the number of the Worker and the session number of
// each sighting (aligned with seen), giving numbers to the sessions seen for
// the first time. It writes only when a number is given, a mapping changes or
// a mapping's last_seen is older than [SessionCodeTouchEvery]; a write also
// prunes the mappings not seen for [SessionCodeRetention] and those of the
// Worker beyond [MaxSessionCodesPerWorker].
func (s *Store) SessionCodes(ctx context.Context, workerID string, seen []SessionSighting, now time.Time) (worker int64, codes []int64, err error) {
	worker, codes, dirty, err := s.assignCodes(ctx, s.db, workerID, seen, now, false)
	if err != nil || !dirty {
		return worker, codes, err
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		var e error
		worker, codes, _, e = s.assignCodes(ctx, tx, workerID, seen, now, true)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM session_codes WHERE last_seen < ?`, UnixMS(now.Add(-SessionCodeRetention))); e != nil {
			return e
		}
		return boundSessionCodes(ctx, tx, workerID)
	})
	if err != nil {
		return 0, nil, fmt.Errorf("store: session codes: %w", err)
	}
	return worker, codes, nil
}

// boundSessionCodes drops the mappings of the Worker beyond
// [MaxSessionCodesPerWorker], the ones seen least recently first.
func boundSessionCodes(ctx context.Context, tx *sql.Tx, workerID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_codes WHERE worker_id = ?`, workerID).Scan(&n); err != nil {
		return err
	}
	if n <= MaxSessionCodesPerWorker {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM session_codes WHERE worker_id = ?1 AND number NOT IN
		(SELECT number FROM session_codes WHERE worker_id = ?1 ORDER BY last_seen DESC, number DESC LIMIT ?2)`,
		workerID, MaxSessionCodesPerWorker)
	return err
}

type codeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// assignCodes computes the codes of seen on q. dirty tells that something has
// to be written; with write it is written on q (a transaction).
func (s *Store) assignCodes(ctx context.Context, q codeQueryer, workerID string, seen []SessionSighting, now time.Time, write bool) (int64, []int64, bool, error) {
	var worker, next int64
	err := q.QueryRowContext(ctx, `SELECT number, next_session FROM worker_numbers WHERE worker_id = ?`, workerID).Scan(&worker, &next)
	dirty := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Every enrolled Worker has a number (migration 0039 and its trigger);
		// a Worker unknown to the table gets the next one.
		if !write {
			return 0, nil, true, nil
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO worker_numbers (worker_id, number)
			VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM worker_numbers))`, workerID); err != nil {
			return 0, nil, false, err
		}
		if err := q.QueryRowContext(ctx, `SELECT number, next_session FROM worker_numbers WHERE worker_id = ?`, workerID).Scan(&worker, &next); err != nil {
			return 0, nil, false, err
		}
	case err != nil:
		return 0, nil, false, err
	}
	rows, err := loadCodeRows(ctx, q, workerID, seen)
	if err != nil {
		return 0, nil, false, err
	}
	nowMS := UnixMS(now)
	stale := UnixMS(now.Add(-SessionCodeTouchEvery))
	codes := make([]int64, len(seen))
	startNext := next
	for i, sg := range seen {
		if sg.Name == "" {
			continue
		}
		r := pickCodeRow(rows, sg)
		if r == nil {
			r = &codeRow{number: next, iid: sg.InstanceID, name: sg.Name, lastSeen: nowMS, isNew: true}
			next++
			rows = append(rows, r)
		} else {
			if sg.InstanceID != "" && r.iid == "" {
				r.iid, r.changed = sg.InstanceID, true // adopt the row of a sighting by name
			}
			if r.name != sg.Name {
				r.name, r.changed = sg.Name, true // a rename (known by the instance id)
			}
			if r.lastSeen < stale {
				r.changed = true
			}
		}
		if sg.InstanceID != "" {
			// The instance id says who holds the name now: a row without an
			// instance id under that name was this session (a rename seen by
			// name first) and goes; a row of another instance loses the name,
			// so that its code no longer reaches this session.
			for _, o := range rows {
				if o == r || o.deleted || o.name != sg.Name {
					continue
				}
				if o.iid == "" {
					o.deleted = true
				} else {
					o.name, o.changed = "", true
				}
			}
		}
		codes[i] = r.number
		if r.changed || r.isNew {
			r.lastSeen = nowMS
		}
	}
	for _, r := range rows {
		if r.isNew || r.changed || r.deleted {
			dirty = true
		}
	}
	if !dirty || !write {
		return worker, codes, dirty, nil
	}
	for _, r := range rows {
		var err error
		switch {
		case r.deleted && !r.isNew:
			_, err = q.ExecContext(ctx, `DELETE FROM session_codes WHERE worker_id = ? AND number = ?`, workerID, r.number)
		case r.isNew:
			_, err = q.ExecContext(ctx, `INSERT INTO session_codes (worker_id, number, instance_id, name, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?, ?)`, workerID, r.number, nullString(r.iid), r.name, nowMS, nowMS)
		case r.changed:
			_, err = q.ExecContext(ctx, `UPDATE session_codes SET instance_id = ?, name = ?, last_seen = ? WHERE worker_id = ? AND number = ?`,
				nullString(r.iid), r.name, r.lastSeen, workerID, r.number)
		}
		if err != nil {
			return 0, nil, false, err
		}
	}
	if next != startNext {
		if _, err := q.ExecContext(ctx, `UPDATE worker_numbers SET next_session = ? WHERE worker_id = ?`, next, workerID); err != nil {
			return 0, nil, false, err
		}
	}
	return worker, codes, true, nil
}

// loadCodeRows reads the mappings of the Worker that a sighting of seen can
// match or take a name from: those of a seen name or instance id. No other
// mapping takes part in [Store.assignCodes].
func loadCodeRows(ctx context.Context, q codeQueryer, workerID string, seen []SessionSighting) ([]*codeRow, error) {
	names, iids := []string{}, []string{}
	for _, sg := range seen {
		if sg.Name == "" {
			continue
		}
		names = append(names, sg.Name)
		if sg.InstanceID != "" {
			iids = append(iids, sg.InstanceID)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	nj, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	ij, err := json.Marshal(iids)
	if err != nil {
		return nil, err
	}
	rs, err := q.QueryContext(ctx, `SELECT number, COALESCE(instance_id, ''), name, last_seen FROM session_codes
		WHERE worker_id = ?1 AND (name IN (SELECT value FROM json_each(?2)) OR instance_id IN (SELECT value FROM json_each(?3)))
		ORDER BY number`, workerID, string(nj), string(ij))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out []*codeRow
	for rs.Next() {
		r := &codeRow{}
		if err := rs.Scan(&r.number, &r.iid, &r.name, &r.lastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// pickCodeRow finds the mapping of a sighting: by instance id when it has one
// (else the row of the same name without an instance id), by name otherwise
// (the most recently seen row of that name).
func pickCodeRow(rows []*codeRow, sg SessionSighting) *codeRow {
	if sg.InstanceID != "" {
		for _, r := range rows {
			if !r.deleted && r.iid == sg.InstanceID {
				return r
			}
		}
		for _, r := range rows {
			if !r.deleted && r.iid == "" && r.name == sg.Name {
				return r
			}
		}
		return nil
	}
	var best *codeRow
	for _, r := range rows {
		if !r.deleted && r.name == sg.Name && (best == nil || r.lastSeen > best.lastSeen) {
			best = r
		}
	}
	return best
}

// WorkerNumbers returns the number of every Worker that has one, by Worker id.
func (s *Store) WorkerNumbers(ctx context.Context) (map[string]int64, error) {
	rs, err := s.db.QueryContext(ctx, `SELECT worker_id, number FROM worker_numbers`)
	if err != nil {
		return nil, fmt.Errorf("store: worker numbers: %w", err)
	}
	defer func() { _ = rs.Close() }()
	out := map[string]int64{}
	for rs.Next() {
		var id string
		var n int64
		if err := rs.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: worker numbers: %w", err)
		}
		out[id] = n
	}
	return out, rs.Err()
}

// ResolveSessionCode returns the Worker and the current session name of a
// code. ok is false for a code that names no Worker or no session, and for a
// code whose session lost its name to another session.
func (s *Store) ResolveSessionCode(ctx context.Context, worker, session int64) (workerID, name string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT w.worker_id, c.name FROM worker_numbers w
		JOIN session_codes c ON c.worker_id = w.worker_id AND c.number = ?
		WHERE w.number = ? AND c.name <> ''`, session, worker).Scan(&workerID, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: resolve a session code: %w", err)
	}
	return workerID, name, true, nil
}
