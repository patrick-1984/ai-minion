package store

// Verified session grants (DESIGN section 6): a grant whose
// source_incarnation is not empty belongs to one kernel-proven terminal
// incarnation of its source session. This file binds and unbinds a grant and
// reads the verified grants for the bound-master lane.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// BindSessionGrant sets source_incarnation of the grant: an incarnation id
// makes it Verified, "" makes it Claimed again. The revision is checked, both
// Workers' owners are pinned and the revision goes up. The acting user
// w.ActorID must be, in the same transaction, an active administrator or the
// active owner of both Workers, else [ErrGrantAuthority]; the audit row of w
// (old = the grant before, now = the grant as stored) commits with the
// change. A second grant for the same source (with that incarnation) and
// target returns [ErrGrantExists].
func (s *Store) BindSessionGrant(ctx context.Context, id, incarnation string, ifRevision uint64, w GrantWrite) (SessionGrantRow, error) {
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if ifRevision == 0 || cur.Revision != ifRevision {
			return ErrGrantRevision
		}
		if !cur.NamedSource() {
			// Deprecated binding names one source session; a Worker-wide or
			// everyone grant has none.
			return fmt.Errorf("%w: only a grant for one named source session can be bound", ErrGrantInvalid)
		}
		if err := pinGrantOwners(ctx, tx, cur); err != nil {
			return err
		}
		// Whoever owns both Workers may bind, whatever authority stands
		// behind the grant: binding changes the proof of the source only.
		may := cur
		may.Authority = GrantAuthorityOwner
		if err := grantActorMay(ctx, tx, w, may, false); err != nil {
			return err
		}
		// A claimed grant carries no file authority: unbinding clears the
		// file transfer flags (files.v1) and the app install flag.
		if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET source_incarnation = ?1,
			files_pull = CASE WHEN ?1 = '' THEN 0 ELSE files_pull END, files_push = CASE WHEN ?1 = '' THEN 0 ELSE files_push END,
			apps_install = CASE WHEN ?1 = '' THEN 0 ELSE apps_install END,
			revision = revision + 1, updated_at = ?2 WHERE id = ?3 AND revision = ?4`, incarnation, time.Now().UnixMilli(), id, int64(ifRevision)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint") {
				return ErrGrantExists
			}
			return fmt.Errorf("store: bind session grant: %w", err)
		}
		if out, err = sessionGrantByID(ctx, tx, id); err != nil {
			return err
		}
		return w.audit(ctx, s, tx, &cur, out)
	})
	if err != nil {
		return SessionGrantRow{}, err
	}
	return out, nil
}

// VerifiedGrantSource is one terminal incarnation of a source session that
// holds at least one verified grant.
type VerifiedGrantSource struct {
	SessionName   string
	IncarnationID string
}

// VerifiedGrantSources lists the (session, incarnation) pairs of the Worker
// that hold a verified grant, while the Worker is approved and still enrolled
// with that owner and key. Suspended and expired grants are included; the
// call judges them.
func (s *Store) VerifiedGrantSources(ctx context.Context, workerID, ownerID string, workerPub []byte) ([]VerifiedGrantSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT g.source_session, g.source_incarnation FROM session_grant g
		JOIN workers w ON w.id = g.source_worker_id
		WHERE g.source_worker_id = ? AND g.source_incarnation <> '' AND g.source_owner_id = w.owner_id
		AND w.owner_id = ? AND w.worker_pub = ? AND w.approval = 'approved'
		ORDER BY g.source_session, g.source_incarnation`, workerID, ownerID, workerPub)
	if err != nil {
		return nil, fmt.Errorf("store: verified grant sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []VerifiedGrantSource
	for rows.Next() {
		var v VerifiedGrantSource
		if err := rows.Scan(&v.SessionName, &v.IncarnationID); err != nil {
			return nil, fmt.Errorf("store: verified grant sources: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VerifiedGrantCaller is the proven caller of the bound-master lane: one
// terminal incarnation of a session on an enrolled Worker.
type VerifiedGrantCaller struct {
	WorkerID      string
	OwnerID       string
	WorkerPub     []byte
	SessionName   string
	IncarnationID string
}

// VerifiedSessionGrants takes one store snapshot of the grants of the
// caller's incarnation: those pinned to it (legacy bound grants) and those
// whose source selector covers its session (by name, Worker or everyone) (expired and suspended rows included, the decision
// judges them). The source Worker must be enrolled as the caller says and its
// session shared, else [ErrWorkerOwnerChanged] or [ErrMasterCandidate]. With a
// target it is also the dispatch fence: the target Worker must be approved and
// owned as given, and at least one of the given sessions must be shared and
// covered by a grant that is usable at now (unix milliseconds: not suspended,
// not expired, owners as stored, an administrator behind an admin grant);
// otherwise [ErrMasterCandidate].
func (s *Store) VerifiedSessionGrants(ctx context.Context, c VerifiedGrantCaller, target *MasterTarget, now int64) ([]SessionGrantRow, error) {
	var out []SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		out = nil
		if err := grantSourceEnrollment(ctx, tx, c.WorkerID, c.OwnerID, c.WorkerPub); err != nil {
			return err
		}
		if c.IncarnationID == "" {
			return ErrMasterCandidate
		}
		if err := requireMasterShare(ctx, tx, c.WorkerID, []string{c.SessionName}); err != nil {
			return err
		}
		// The grants of this incarnation's session by name, by Worker or for
		// everyone (a prepared terminal uses an ordinary grant it matches),
		// and the legacy grants pinned to this very incarnation.
		rows, err := tx.QueryContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant
			WHERE `+grantSourceMatch("?3")+` ORDER BY created_at, id`,
			c.WorkerID, c.SessionName, c.IncarnationID)
		if err != nil {
			return fmt.Errorf("store: verified session grants: %w", err)
		}
		for rows.Next() {
			g, err := scanSessionGrant(rows)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: verified session grants: %w", err)
			}
			out = append(out, g)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if target == nil {
			return nil
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ? AND approval = 'approved'`, target.WorkerID).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrMasterCandidate
			}
			return err
		}
		if owner != target.OwnerID {
			return ErrMasterCandidate
		}
		admins := map[string]bool{}
		usable := func(g SessionGrantRow) (bool, error) {
			if g.TargetWorkerID != target.WorkerID || g.SuspendedAt != nil || g.ExpiresAt != nil && *g.ExpiresAt <= now ||
				g.Scope() != GrantSourceEveryone && g.SourceOwnerID != c.OwnerID || g.TargetOwnerID != owner {
				return false, nil
			}
			switch g.Authority {
			case GrantAuthorityOwner:
				return g.SourceOwnerID == g.TargetOwnerID, nil
			case GrantAuthorityAdmin:
				live, done := admins[g.GrantedBy]
				if !done {
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN user_roles r ON r.user_id = u.id
						WHERE u.id = ? AND u.active = 1 AND r.role = 'admin')`, g.GrantedBy).Scan(&live); err != nil {
						return false, err
					}
					admins[g.GrantedBy] = live
				}
				return live, nil
			}
			return false, nil
		}
		var allowed []string
		for _, name := range target.Sessions {
			for _, g := range out {
				if g.TargetSession != "" && g.TargetSession != name {
					continue
				}
				ok, err := usable(g)
				if err != nil {
					return err
				}
				if ok {
					allowed = append(allowed, name)
					break
				}
			}
		}
		return requireMasterShare(ctx, tx, target.WorkerID, allowed)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
