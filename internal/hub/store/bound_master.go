package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	ErrMasterRevision  = errors.New("the master nomination changed; reload the session and its current revision")
	ErrMasterCandidate = errors.New("the prepared shared session is no longer available")
	ErrBoundMasterSlot = errors.New("use the bound master operation for this session")
)

func masterSlotError(err error) error {
	if err != nil && strings.Contains(err.Error(), "bound master slot") {
		return ErrBoundMasterSlot
	}
	return err
}

type BoundMasterRow struct {
	WorkerID, OwnerID, SessionName, IncarnationID string
	WorkerPub                                     []byte
	Generation                                    uint64
	Active                                        bool
	LegacyTargetsJSON                             string
}

const boundMasterColumns = `worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,legacy_targets_json`

type rowScanner interface{ Scan(...any) error }

func scanBoundMaster(q rowScanner) (BoundMasterRow, error) {
	var r BoundMasterRow
	err := q.Scan(&r.WorkerID, &r.OwnerID, &r.SessionName, &r.IncarnationID, &r.WorkerPub, &r.Generation, &r.Active, &r.LegacyTargetsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// BoundMasterActive reports whether the session holds an active session-bound
// master nomination. Such a nomination is not in master_session, so the
// name-only revoke cannot withdraw it.
func (s *Store) BoundMasterActive(ctx context.Context, workerID, session string) (bool, error) {
	return boundMasterActive(ctx, s.db, workerID, session)
}

// boundMasterActive is [Store.BoundMasterActive] on q: inside a transaction it
// fences a name-only revoke against a concurrent conversion.
func boundMasterActive(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, workerID, session string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM bound_master_session WHERE worker_id=? AND session_name=? AND active=1`,
		workerID, session).Scan(&n)
	return n > 0, err
}

func (s *Store) ListBoundMasters(ctx context.Context, workerID string) ([]BoundMasterRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boundMasterColumns+` FROM bound_master_session
	 WHERE (?='' OR worker_id=?) ORDER BY worker_id,session_name`, workerID, workerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BoundMasterRow
	for rows.Next() {
		r, err := scanBoundMaster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetBoundMaster stores one explicit decision. The caller keeps its live
// candidate observation stable during the transaction. Revocation uses the
// stored incarnation, including for an offline Worker. An idempotent retry
// needs neither a live candidate nor a second application of its decision.
func (s *Store) SetBoundMaster(ctx context.Context, w WorkerRow, a ops.ControlSetBoundMasterArgs, actor string, now int64, verified bool) (changed bool, err error) {
	if err := a.Validate(); err != nil {
		return false, err
	}
	if w.ID != a.WorkerID || w.OwnerID == "" || len(w.Pub) == 0 || actor == "" {
		return false, ErrMasterCandidate
	}
	decision, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if a.Master {
			if _, err := masterEnrollment(ctx, tx, w.ID, w.OwnerID, w.Pub); err != nil {
				return err
			}
		} else if err := masterEnrollmentIdentity(ctx, tx, w); err != nil {
			return err
		}
		var savedWorker, savedActor, savedDecision string
		err := tx.QueryRowContext(ctx, `SELECT worker_id,actor,decision FROM bound_master_request WHERE request_id=?`, a.RequestID).
			Scan(&savedWorker, &savedActor, &savedDecision)
		switch {
		case err == nil:
			if savedWorker == w.ID && savedActor == actor && savedDecision == string(decision) {
				return nil
			}
			return ErrMasterRevision
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		cur, err := scanBoundMaster(tx.QueryRowContext(ctx, `SELECT `+boundMasterColumns+
			` FROM bound_master_session WHERE worker_id=? AND session_name=?`, w.ID, a.SessionName))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if cur.Generation != *a.IfGeneration || (!a.Master && cur.IncarnationID != a.IncarnationID) ||
			(cur.Generation != 0 && (cur.OwnerID != w.OwnerID || !bytes.Equal(cur.WorkerPub, w.Pub))) {
			return ErrMasterRevision
		}
		if cur.Active != a.Master || cur.IncarnationID != a.IncarnationID {
			if cur.Generation == proto.MaxMasterGeneration {
				return ErrMasterRevision
			}
			if a.Master {
				if !verified {
					return ErrMasterCandidate
				}
				if err := requireMasterShare(ctx, tx, w.ID, []string{a.SessionName}); err != nil {
					return err
				}
			}
			if cur.Generation == 0 {
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(legacy_targets_json,'') FROM master_session WHERE worker_id=? AND session_name=?`,
					w.ID, a.SessionName).Scan(&cur.LegacyTargetsJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM master_session WHERE worker_id=? AND session_name=?`, w.ID, a.SessionName); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO bound_master_session
			 (worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,legacy_targets_json,changed_at)
			 VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(worker_id,session_name) DO UPDATE SET
			 incarnation_id=excluded.incarnation_id,generation=excluded.generation,active=excluded.active,changed_at=excluded.changed_at`,
				w.ID, w.OwnerID, a.SessionName, a.IncarnationID, w.Pub, cur.Generation+1, a.Master, cur.LegacyTargetsJSON, now)
			if err != nil {
				return err
			}
			changed = true
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bound_master_request(request_id,worker_id,actor,decision) VALUES(?,?,?,?)`,
			a.RequestID, w.ID, actor, string(decision))
		return err
	})
	return changed && err == nil, err
}

// MasterTarget is a captured destination link and the shared session names
// relevant to the call. A session listing needs at least one allowed name.
type MasterTarget struct {
	WorkerID, OwnerID string
	Sessions          []string
}

// BoundMasterTargets takes one immediate store snapshot of authority. For a
// destination this is also the dispatch admission fence: committed consent,
// ownership and share decisions are ordered by the same SQLite transaction.
// The caller starts network I/O only after this method returns.
func (s *Store) BoundMasterTargets(ctx context.Context, binding BoundMasterRow, target *MasterTarget) ([]ops.KeyResource, error) {
	var out []ops.KeyResource
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		admin, err := masterEnrollment(ctx, tx, binding.WorkerID, binding.OwnerID, binding.WorkerPub)
		if err != nil {
			return err
		}
		cur, err := scanBoundMaster(tx.QueryRowContext(ctx, `SELECT `+boundMasterColumns+
			` FROM bound_master_session WHERE worker_id=? AND session_name=?`, binding.WorkerID, binding.SessionName))
		if errors.Is(err, ErrNotFound) || err == nil && (!cur.Active || cur.IncarnationID != binding.IncarnationID ||
			cur.Generation != binding.Generation || cur.OwnerID != binding.OwnerID || !bytes.Equal(cur.WorkerPub, binding.WorkerPub)) {
			return ErrMasterRevision
		}
		if err != nil {
			return err
		}
		if err := requireMasterShare(ctx, tx, binding.WorkerID, []string{binding.SessionName}); err != nil {
			return err
		}
		targets, err := controlTargetsFrom(ctx, tx, binding.WorkerID, cur.LegacyTargetsJSON)
		if err != nil {
			return err
		}
		allowedWorkers, err := masterAllowedWorkers(ctx, tx, binding.WorkerID, binding.OwnerID, admin)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if allowedWorkers[t.WorkerID] {
				out = append(out, ops.KeyResource{Kind: t.Kind, WorkerID: t.WorkerID, SessionName: t.SessionName})
			}
		}
		if target == nil {
			return nil
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id=? AND approval='approved'`, target.WorkerID).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMasterCandidate
			}
			return err
		}
		if owner != target.OwnerID || !admin && owner != binding.OwnerID {
			return ErrMasterCandidate
		}
		var allowed []string
		for _, name := range target.Sessions {
			if slices.ContainsFunc(out, func(r ops.KeyResource) bool {
				return r.WorkerID == target.WorkerID && (r.Kind == ops.KeyResourceWorker || r.SessionName == name)
			}) {
				allowed = append(allowed, name)
			}
		}
		return requireMasterShare(ctx, tx, target.WorkerID, allowed)
	})
	return out, err
}

// Withdrawal still requires the same durable Worker identity. It remains
// available to administrators even if that Worker's owner lost access.
func masterEnrollmentIdentity(ctx context.Context, tx *sql.Tx, w WorkerRow) error {
	var same bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workers WHERE id=? AND owner_id=? AND worker_pub=?)`, w.ID, w.OwnerID, w.Pub).Scan(&same)
	if err == nil && !same {
		err = ErrWorkerOwnerChanged
	}
	return err
}

func masterEnrollment(ctx context.Context, tx *sql.Tx, workerID, owner string, pub []byte) (admin bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='admin')
	 FROM workers w JOIN users u ON u.id=w.owner_id WHERE w.id=? AND w.owner_id=? AND w.worker_pub=?
	 AND w.approval='approved' AND u.active=1 AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id)`,
		workerID, owner, pub).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrWorkerOwnerChanged
	}
	return admin, err
}

func requireMasterShare(ctx context.Context, tx *sql.Tx, workerID string, names []string) error {
	if len(names) == 0 {
		return ErrMasterCandidate
	}
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM json_each(?1) choice
	 JOIN worker_share s ON s.worker_id=?2 WHERE
	 (s.mode='all' OR s.mode='select' AND choice.value IN (SELECT value FROM json_each(s.names)))
	 AND NOT EXISTS(SELECT 1 FROM worker_share_pending p WHERE p.worker_id=s.worker_id
	  AND NOT(p.mode='all' OR p.mode='select' AND choice.value IN (SELECT value FROM json_each(p.names)))))`, string(data), workerID).Scan(&allowed)
	if err == nil && !allowed {
		err = ErrMasterCandidate
	}
	return err
}

func masterAllowedWorkers(ctx context.Context, tx *sql.Tx, source, owner string, admin bool) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM workers WHERE id<>? AND approval='approved' AND (? OR owner_id=?)`, source, admin, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
