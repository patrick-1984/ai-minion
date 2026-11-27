package store

// Session grants (D1, D2): one row says that a source session on one Worker
// may reach a target on another Worker at a level. This file is the storage
// only; who may decide and what a grant allows is judged elsewhere.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrGrantRevision reports a write whose if_revision is not the stored one.
	ErrGrantRevision = errors.New("store: session grant revision mismatch")
	// ErrGrantOwners reports a grant whose Workers are not approved, are not
	// owned as the row says, or whose authority does not fit their owners.
	ErrGrantOwners = errors.New("store: session grant owners changed")
	// ErrGrantExists reports a second grant for the same source and target.
	ErrGrantExists = errors.New("store: session grant exists")
	// ErrGrantInvalid reports a row the table refuses (same Worker, level).
	ErrGrantInvalid = errors.New("store: session grant invalid")
	// ErrGrantAuthority reports a write whose acting user does not hold, at
	// the moment of the write, the authority the change needs (D4).
	ErrGrantAuthority = errors.New("store: session grant authority lost")
	// ErrGrantNotSuspended reports an approval of a grant that is not
	// suspended: there is nothing to lift.
	ErrGrantNotSuspended = errors.New("store: session grant not suspended")
)

// GrantWrite names who writes a grant and the audit row of the write. The
// store judges both inside the transaction that writes, so a role lost after
// the caller's own check cannot commit a change, and no change commits
// without its audit row.
type GrantWrite struct {
	// ActorID is the acting user. [Store.PutSessionGrant],
	// [Store.ApproveSessionGrant] and [Store.BindSessionGrant] read its role
	// and active state again in the writing transaction; an empty id is
	// refused there.
	ActorID string
	// ActAs is the act-as period of the browser session through which an
	// administrator (ActAs.ActorID) acts as ActorID, as the request observed
	// it; nil when nobody acts as ActorID. In the writing transaction that
	// very period must still be stored and not expired, its target must be
	// ActorID, and its administrator must still be an active administrator; a
	// period that ended or was replaced cannot commit a change. The authority
	// of the change is still that of ActorID.
	ActAs *ActAsRow
	// Audit builds the audit row of the change: old is the stored grant before
	// it (nil for a create), now the grant as stored (for a revoke the deleted
	// grant). An error refuses the change. A nil Audit writes no row. An empty
	// ID and a zero TS are filled in.
	Audit func(old *SessionGrantRow, now SessionGrantRow) (AuditRow, error)
	// Receipt builds the stored answer of the request_id of the change from
	// the grant as stored (for a revoke the deleted grant); it is written in
	// the transaction of the change, so that a repeat of a request that
	// committed finds its answer even when the Hub stopped right after the
	// commit. A nil Receipt, or a nil row, stores none; an error refuses the
	// change.
	Receipt func(now SessionGrantRow) (*OpRequestRow, error)
	// ReceiptMany is the Receipt of [Store.PutSessionGrants]: the stored
	// answer of the request from every grant it created.
	ReceiptMany func(now []SessionGrantRow) (*OpRequestRow, error)
}

// audit writes the audit row and the request receipt of a change inside tx.
func (w GrantWrite) audit(ctx context.Context, s *Store, tx *sql.Tx, old *SessionGrantRow, now SessionGrantRow) error {
	if w.Audit != nil {
		row, err := w.Audit(old, now)
		if err != nil {
			return err
		}
		if row.ID == "" {
			row.ID = NewID()
		}
		if row.TS == 0 {
			row.TS = time.Now().UnixMilli()
		}
		if err := s.InsertAudit(ctx, tx, row); err != nil {
			return err
		}
	}
	if w.Receipt == nil {
		return nil
	}
	r, err := w.Receipt(now)
	if err != nil || r == nil {
		return err
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	_, err = putOpRequestTx(ctx, tx, *r)
	return err
}

// grantUserRole reads, inside tx, whether userID is active, holds any
// effective role (user or admin) and holds the administrator role. A missing
// user holds nothing.
func grantUserRole(ctx context.Context, tx *sql.Tx, userID string) (active, anyRole, admin bool, err error) {
	var a, r, ad int
	err = tx.QueryRowContext(ctx, `SELECT u.active,
		EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id),
		EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'admin')
		FROM users u WHERE u.id = ?`, userID).Scan(&a, &r, &ad)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false, nil
	}
	if err != nil {
		return false, false, false, fmt.Errorf("store: session grant authority: %w", err)
	}
	return a != 0, r != 0, ad != 0, nil
}

// grantActAsLive reports, inside tx, whether the act-as period a is still the
// stored period of its browser session and has not expired at now (unix
// milliseconds).
func grantActAsLive(ctx context.Context, tx *sql.Tx, a ActAsRow, now int64) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM act_as_sessions WHERE session_id = ? AND actor_id = ?
		AND target_id = ? AND started_at = ? AND expires_at = ? AND expires_at > ?)`,
		a.SessionID, a.ActorID, a.TargetID, a.StartedAt, a.ExpiresAt, now).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("store: session grant act-as: %w", err)
	}
	return live, nil
}

// grantActorMay checks inside the writing transaction that the actor of w may
// decide g now (D4): an active administrator always; otherwise, unless
// adminOnly, the active owner of both Workers of an owner grant who still
// holds an effective role (an account without one has no access at all). When
// w names an act-as period, that period must still be live and its
// administrator must still be an active administrator too. The caller has
// pinned the owners of g to the current ones.
func grantActorMay(ctx context.Context, tx *sql.Tx, w GrantWrite, g SessionGrantRow, adminOnly bool) error {
	actorID := w.ActorID
	if actorID == "" {
		return ErrGrantAuthority
	}
	if a := w.ActAs; a != nil {
		if a.SessionID == "" || a.ActorID == "" || a.TargetID != actorID {
			return ErrGrantAuthority
		}
		live, err := grantActAsLive(ctx, tx, *a, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if !live {
			return ErrGrantAuthority
		}
		active, _, admin, err := grantUserRole(ctx, tx, a.ActorID)
		if err != nil {
			return err
		}
		if !active || !admin {
			return ErrGrantAuthority
		}
	}
	active, anyRole, admin, err := grantUserRole(ctx, tx, actorID)
	if err != nil {
		return err
	}
	switch {
	case !active || !anyRole:
		return ErrGrantAuthority
	case admin:
		return nil
	case adminOnly || g.Authority != GrantAuthorityOwner || g.SourceOwnerID != actorID || g.TargetOwnerID != actorID:
		return ErrGrantAuthority
	}
	return nil
}

// grantSourceEnrollment checks, inside tx, that the source Worker of a direct
// grant is enrolled as the caller says (owner, key, approved) and that its
// owner is an active account. Unlike [masterEnrollment] it asks for no
// effective role: the authority of a direct grant stands behind the grant
// (D4), and the owner's role limits only legacy master reach.
func grantSourceEnrollment(ctx context.Context, tx *sql.Tx, workerID, owner string, pub []byte) error {
	var ok bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workers w JOIN users u ON u.id = w.owner_id
		WHERE w.id = ? AND w.owner_id = ? AND w.worker_pub = ? AND w.approval = 'approved' AND u.active = 1)`,
		workerID, owner, pub).Scan(&ok)
	if err == nil && !ok {
		err = ErrWorkerOwnerChanged
	}
	return err
}

// Grant authorities.
const (
	GrantAuthorityOwner = "owner"
	GrantAuthorityAdmin = "admin"
)

// Grant source scopes (source_scope): which calling sessions a grant is for.
const (
	// GrantSourceSession is one named session of the source Worker.
	GrantSourceSession = "session"
	// GrantSourceWorker is every shared session of the source Worker.
	GrantSourceWorker = "worker"
	// GrantSourceEveryone is every shared session of every eligible Worker:
	// no source Worker, session or source owner; admin authority only.
	GrantSourceEveryone = "everyone"
)

// GrantSourceAudit is the audit source of a grant's source selector:
// "<worker>:<session>", "<worker>:*" for every shared session of the source
// Worker, "*" for everyone.
func GrantSourceAudit(g SessionGrantRow) string {
	switch g.Scope() {
	case GrantSourceEveryone:
		return "*"
	case GrantSourceWorker:
		return g.SourceWorkerID + ":*"
	}
	return g.SourceWorkerID + ":" + g.SourceSession
}

// Scope is the source scope of g; a row written without one (an older
// caller) is a one-session grant.
func (g SessionGrantRow) Scope() string {
	if g.SourceScope == "" {
		return GrantSourceSession
	}
	return g.SourceScope
}

// NamedSource reports whether g is for one named source session (and so
// ends with it, and is fenced by its name), not a whole Worker or everyone.
func (g SessionGrantRow) NamedSource() bool { return g.Scope() == GrantSourceSession }

// SessionGrantRow is one row of session_grant. Times are unix milliseconds.
type SessionGrantRow struct {
	ID string
	// SourceScope is GrantSourceSession, GrantSourceWorker or
	// GrantSourceEveryone; "" on a write is GrantSourceSession.
	SourceScope string
	// SourceWorkerID is "" for everyone; SourceSession is "" unless the
	// scope is one session.
	SourceWorkerID string
	SourceSession  string
	// SourceIncarnation is "" for every grant made since v0.26.0; a value is
	// a legacy restriction of a bound grant to one terminal incarnation.
	SourceIncarnation string
	TargetWorkerID    string
	TargetSession     string // "" = every shared session of the target Worker
	Level             int    // 1 View, 2 Message, 3 Control
	Authority         string
	GrantedBy         string
	SourceOwnerID     string
	TargetOwnerID     string
	Note              string
	ExpiresAt         *int64
	Revision          uint64
	CreatedAt         int64
	UpdatedAt         int64
	SuspendedAt       *int64
	SuspendCause      string
	// CommandShell marks the target session as a command shell of the source
	// session (A116): only on a Control grant that names one target session.
	CommandShell bool
	// FilesPull and FilesPush are the file transfer flags (files.v1),
	// FilesPush only at Control.
	FilesPull, FilesPush bool
	// AppsInstall is the app install flag (v0.25.0), only at Control.
	AppsInstall bool
}

// SessionGrantFilter narrows [Store.ListSessionGrants]. An empty member does
// not filter. WorkerID and OwnerID match the source or the target side.
type SessionGrantFilter struct {
	WorkerID string
	OwnerID  string
}

const sessionGrantCols = `id, source_scope, COALESCE(source_worker_id, ''), source_session, source_incarnation, target_worker_id, target_session,
	level, authority, granted_by, source_owner_id, target_owner_id, note, expires_at, revision, created_at, updated_at,
	suspended_at, suspend_cause, command_shell, files_pull, files_push, apps_install`

func scanSessionGrant(sc rowScanner) (SessionGrantRow, error) {
	var g SessionGrantRow
	var exp, sus sql.NullInt64
	var rev, shell, pull, push, apps int64
	err := sc.Scan(&g.ID, &g.SourceScope, &g.SourceWorkerID, &g.SourceSession, &g.SourceIncarnation, &g.TargetWorkerID, &g.TargetSession,
		&g.Level, &g.Authority, &g.GrantedBy, &g.SourceOwnerID, &g.TargetOwnerID, &g.Note, &exp, &rev, &g.CreatedAt,
		&g.UpdatedAt, &sus, &g.SuspendCause, &shell, &pull, &push, &apps)
	g.CommandShell = shell != 0
	g.FilesPull, g.FilesPush, g.AppsInstall = pull != 0, push != 0, apps != 0
	if err != nil {
		return g, err
	}
	g.Revision = uint64(rev)
	if exp.Valid {
		g.ExpiresAt = &exp.Int64
	}
	if sus.Valid {
		g.SuspendedAt = &sus.Int64
	}
	return g, nil
}

type sessionGrantQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func sessionGrantByID(ctx context.Context, q sessionGrantQuerier, id string) (SessionGrantRow, error) {
	g, err := scanSessionGrant(q.QueryRowContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, fmt.Errorf("store: session grant: %w", err)
	}
	return g, nil
}

// pinGrantOwners refuses a grant whose Workers are missing, not approved or
// not owned as the row says, and an owner grant across two owners.
func pinGrantOwners(ctx context.Context, tx *sql.Tx, g SessionGrantRow) error {
	sides := []struct{ worker, owner string }{{g.TargetWorkerID, g.TargetOwnerID}}
	if g.Scope() != GrantSourceEveryone {
		sides = append(sides, struct{ worker, owner string }{g.SourceWorkerID, g.SourceOwnerID})
	}
	for _, side := range sides {
		var owner, approval string
		err := tx.QueryRowContext(ctx, `SELECT owner_id, approval FROM workers WHERE id = ?`, side.worker).Scan(&owner, &approval)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGrantOwners
		}
		if err != nil {
			return fmt.Errorf("store: session grant owners: %w", err)
		}
		if owner != side.owner || approval != "approved" {
			return ErrGrantOwners
		}
	}
	if g.Authority == GrantAuthorityOwner && g.SourceOwnerID != g.TargetOwnerID {
		return ErrGrantOwners
	}
	return nil
}

// PutSessionGrant creates the grant (ifRevision 0, no row with that id) or
// changes level, expiry, note, the command_shell flag and the file transfer
// flags of the stored one
// when its revision is ifRevision. Both Workers' owners and approvals, and
// the authority of the acting user w.ActorID (an active administrator, or the
// active owner of both Workers of an owner grant), are checked in the same
// transaction, which also writes the audit row of w. A new grant names w.ActorID as granted_by. It
// returns the row as stored. An empty ID gets a new one; a zero UpdatedAt is
// now. An update leaves created_at, the purge fence, as it is.
func (s *Store) PutSessionGrant(ctx context.Context, row SessionGrantRow, ifRevision uint64, w GrantWrite) (SessionGrantRow, error) {
	if row.ID == "" {
		row.ID = NewID()
	}
	now := row.UpdatedAt
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, row.ID)
		var old *SessionGrantRow
		switch {
		case errors.Is(err, ErrNotFound):
			if ifRevision != 0 {
				return ErrNotFound
			}
			if err := createSessionGrantTx(ctx, tx, &row, now, w); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if ifRevision == 0 || cur.Revision != ifRevision {
				return ErrGrantRevision
			}
			if err := pinGrantOwners(ctx, tx, cur); err != nil {
				return err
			}
			if err := grantActorMay(ctx, tx, w, cur, false); err != nil {
				return err
			}
			old = &cur
			if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET level = ?, expires_at = ?, note = ?, command_shell = ?,
				files_pull = ?, files_push = ?, apps_install = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
				row.Level, nullInt64(row.ExpiresAt), row.Note, boolInt(row.CommandShell), boolInt(row.FilesPull), boolInt(row.FilesPush),
				boolInt(row.AppsInstall), now, row.ID, int64(ifRevision)); err != nil {
				if strings.Contains(err.Error(), "CHECK constraint") {
					return fmt.Errorf("%w: %v", ErrGrantInvalid, err)
				}
				return fmt.Errorf("store: update session grant: %w", err)
			}
		}
		if out, err = sessionGrantByID(ctx, tx, row.ID); err != nil {
			return err
		}
		return w.audit(ctx, s, tx, old, out)
	})
	if err != nil {
		return SessionGrantRow{}, err
	}
	return out, nil
}

// createSessionGrantTx checks and inserts the new grant row inside tx: the
// shape of its source selector, both Workers' owners and approvals and the
// authority of the acting user. row.GrantedBy becomes w.ActorID.
func createSessionGrantTx(ctx context.Context, tx *sql.Tx, row *SessionGrantRow, now int64, w GrantWrite) error {
	row.SourceScope = row.Scope()
	switch row.SourceScope {
	case GrantSourceSession:
	case GrantSourceWorker:
		row.SourceSession, row.SourceIncarnation = "", ""
	case GrantSourceEveryone:
		row.SourceWorkerID, row.SourceSession, row.SourceIncarnation, row.SourceOwnerID = "", "", "", ""
		if row.Authority != GrantAuthorityAdmin {
			return fmt.Errorf("%w: a grant for everyone needs admin authority", ErrGrantInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown source scope", ErrGrantInvalid)
	}
	if row.SourceWorkerID == row.TargetWorkerID {
		return fmt.Errorf("%w: source and target on the same Worker", ErrGrantInvalid)
	}
	if err := pinGrantOwners(ctx, tx, *row); err != nil {
		return err
	}
	row.GrantedBy = w.ActorID
	if err := grantActorMay(ctx, tx, w, *row, false); err != nil {
		return err
	}
	created := row.CreatedAt
	if created == 0 {
		created = now
	}
	var source any
	if row.SourceWorkerID != "" {
		source = row.SourceWorkerID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO session_grant (id, source_scope, source_worker_id, source_session, source_incarnation,
		target_worker_id, target_session, level, authority, granted_by, source_owner_id, target_owner_id, note,
		expires_at, revision, created_at, updated_at, command_shell, files_pull, files_push, apps_install) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?,?)`,
		row.ID, row.SourceScope, source, row.SourceSession, row.SourceIncarnation, row.TargetWorkerID, row.TargetSession,
		row.Level, row.Authority, row.GrantedBy, row.SourceOwnerID, row.TargetOwnerID, row.Note,
		nullInt64(row.ExpiresAt), created, now, boolInt(row.CommandShell), boolInt(row.FilesPull), boolInt(row.FilesPush),
		boolInt(row.AppsInstall))
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "UNIQUE constraint"):
			return ErrGrantExists
		case strings.Contains(msg, "CHECK constraint"):
			return fmt.Errorf("%w: %v", ErrGrantInvalid, err)
		}
		return fmt.Errorf("store: create session grant: %w", err)
	}
	return nil
}

// PutSessionGrants creates several grants in one transaction: each is
// checked and inserted as by [Store.PutSessionGrant] (ifRevision 0), with the
// audit row of w for each. Either all of them commit or none does. The
// request receipt is w.ReceiptMany of the rows as stored, written once in the
// same transaction (w.Receipt is not used). It returns the rows as stored, in
// the order given.
func (s *Store) PutSessionGrants(ctx context.Context, rows []SessionGrantRow, w GrantWrite) ([]SessionGrantRow, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no grant", ErrGrantInvalid)
	}
	now := time.Now().UnixMilli()
	var out []SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		out = make([]SessionGrantRow, 0, len(rows))
		one := w
		one.Receipt = nil
		for _, row := range rows {
			if row.ID == "" {
				row.ID = NewID()
			}
			if err := createSessionGrantTx(ctx, tx, &row, now, w); err != nil {
				return err
			}
			g, err := sessionGrantByID(ctx, tx, row.ID)
			if err != nil {
				return err
			}
			if err := one.audit(ctx, s, tx, nil, g); err != nil {
				return err
			}
			out = append(out, g)
		}
		if w.ReceiptMany == nil {
			return nil
		}
		r, err := w.ReceiptMany(out)
		if err != nil || r == nil {
			return err
		}
		if r.CreatedAt == 0 {
			r.CreatedAt = now
		}
		_, err = putOpRequestTx(ctx, tx, *r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// DeleteSessionGrant deletes one grant and reports whether it existed.
func (s *Store) DeleteSessionGrant(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete session grant: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeSessionGrant deletes one grant and writes the audit row of w in the
// same transaction. ifRevision is required (D2: every write carries it) and
// must be the stored revision, which the DELETE itself checks: a zero
// revision, or a grant changed meanwhile, leaves the grant and the answer is
// [ErrGrantRevision]. A missing grant is [ErrNotFound]. It returns the deleted
// row. Revoking only narrows, so the acting user's role is not read here.
func (s *Store) RevokeSessionGrant(ctx context.Context, id string, ifRevision uint64, w GrantWrite) (SessionGrantRow, error) {
	if ifRevision == 0 {
		return SessionGrantRow{}, ErrGrantRevision
	}
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Revision != ifRevision {
			return ErrGrantRevision
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ? AND revision = ?`, id, int64(ifRevision))
		if err != nil {
			return fmt.Errorf("store: revoke session grant: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrGrantRevision
		}
		out = cur
		return w.audit(ctx, s, tx, &cur, cur)
	})
	if err != nil {
		return SessionGrantRow{}, err
	}
	return out, nil
}

// ApproveSessionGrant lifts a suspension: w.ActorID becomes granted_by and
// the revision goes up. That user must be an active administrator, which is
// read in the same transaction; the audit row of w commits with the change.
// A grant that is not suspended is left as it is: [ErrGrantNotSuspended].
func (s *Store) ApproveSessionGrant(ctx context.Context, id string, ifRevision uint64, w GrantWrite) (SessionGrantRow, error) {
	var out SessionGrantRow
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := sessionGrantByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Revision != ifRevision {
			return ErrGrantRevision
		}
		if err := pinGrantOwners(ctx, tx, cur); err != nil {
			return err
		}
		if err := grantActorMay(ctx, tx, w, cur, true); err != nil {
			return err
		}
		// Said only to an administrator, after the authority check.
		if cur.SuspendedAt == nil {
			return ErrGrantNotSuspended
		}
		if _, err := tx.ExecContext(ctx, `UPDATE session_grant SET suspended_at = NULL, suspend_cause = '', granted_by = ?,
			revision = revision + 1, updated_at = ? WHERE id = ? AND revision = ?`,
			w.ActorID, time.Now().UnixMilli(), id, int64(ifRevision)); err != nil {
			return fmt.Errorf("store: approve session grant: %w", err)
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

// SessionGrant returns one grant or [ErrNotFound].
func (s *Store) SessionGrant(ctx context.Context, id string) (SessionGrantRow, error) {
	return sessionGrantByID(ctx, s.db, id)
}

func (s *Store) querySessionGrants(ctx context.Context, where string, args ...any) ([]SessionGrantRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionGrantCols+` FROM session_grant `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list session grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionGrantRow
	for rows.Next() {
		g, err := scanSessionGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list session grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListSessionGrants returns the grants that match f, oldest first.
func (s *Store) ListSessionGrants(ctx context.Context, f SessionGrantFilter) ([]SessionGrantRow, error) {
	where, args := "WHERE 1=1", []any{}
	if f.WorkerID != "" {
		where += " AND (source_worker_id = ? OR target_worker_id = ?)"
		args = append(args, f.WorkerID, f.WorkerID)
	}
	if f.OwnerID != "" {
		where += " AND (source_owner_id = ? OR target_owner_id = ?)"
		args = append(args, f.OwnerID, f.OwnerID)
	}
	return s.querySessionGrants(ctx, where, args...)
}

// SessionGrantsFor returns the grants whose source selector names the session
// sourceSession of sourceWorkerID by name: the one-session grants of that
// name with no legacy incarnation pin, the grants for every shared session
// of that Worker, and the grants for everyone whose target is on another
// Worker. Whether the session is shared, which a Worker-wide or everyone
// selector needs, is the caller's question. It is one read; expired and
// suspended rows are included, the caller judges them.
func (s *Store) SessionGrantsFor(ctx context.Context, sourceWorkerID, sourceSession string) ([]SessionGrantRow, error) {
	if sourceWorkerID == "" || sourceSession == "" {
		return nil, nil
	}
	return s.querySessionGrants(ctx, `WHERE `+grantSourceMatch(""), sourceWorkerID, sourceSession)
}

// grantSourceMatch is the condition of the grants whose source selector
// covers the session ?1:?2 (worker id, session name): by name, by its Worker,
// or everyone on another Worker. incarnation, when not "", is the SQL
// parameter of a bound incarnation whose legacy pinned grants count too.
func grantSourceMatch(incarnation string) string {
	pin := `source_incarnation = ''`
	if incarnation != "" {
		pin = `source_incarnation IN ('', ` + incarnation + `)`
	}
	return `((source_scope = 'session' AND source_worker_id = ?1 AND source_session = ?2 AND ` + pin + `)
		OR (source_scope = 'worker' AND source_worker_id = ?1)
		OR (source_scope = 'everyone' AND target_worker_id <> ?1))`
}

// PurgeSessionGrants deletes the grants that name a gone session of workerID
// as source or target and were created strictly before asOf. The fence is
// created_at, the moment the named sessions were bound: a later edit of
// level, expiry or note, or an approval, does not move it, so an edited grant
// cannot outlive the session it was made for. A grant on the whole target
// Worker (target_session ”) names no session and stays.
func (s *Store) PurgeSessionGrants(ctx context.Context, workerID string, goneNames []string, asOf int64) (int64, error) {
	if workerID == "" || len(goneNames) == 0 {
		return 0, nil
	}
	set := make(map[string]bool, len(goneNames))
	for _, n := range goneNames {
		set[n] = true
	}
	var total int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		total, err = purgeSessionGrants(ctx, tx, workerID, func(n string) bool { return set[n] }, asOf)
		return err
	})
	return total, err
}

func purgeSessionGrants(ctx context.Context, tx *sql.Tx, workerID string, gone func(string) bool, asOf int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(source_worker_id, ''), source_session, target_worker_id, target_session
		FROM session_grant WHERE (source_worker_id = ? OR target_worker_id = ?) AND created_at < ?`, workerID, workerID, asOf)
	if err != nil {
		return 0, fmt.Errorf("store: purge session grants: %w", err)
	}
	var drop []string
	for rows.Next() {
		var id, sw, ss, tw, ts string
		if err := rows.Scan(&id, &sw, &ss, &tw, &ts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		// A Worker-wide or everyone source names no session (ss is '').
		if (sw == workerID && ss != "" && gone(ss)) || (tw == workerID && ts != "" && gone(ts)) {
			drop = append(drop, id)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, id := range drop {
		res, err := tx.ExecContext(ctx, `DELETE FROM session_grant WHERE id = ? AND created_at < ?`, id, asOf)
		if err != nil {
			return total, fmt.Errorf("store: purge session grant: %w", err)
		}
		k, _ := res.RowsAffected()
		total += k
	}
	return total, nil
}

// DeleteExpiredSessionGrants deletes the grants that expired before the given
// time (unix milliseconds) and returns their number.
func (s *Store) DeleteExpiredSessionGrants(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_grant WHERE expires_at IS NOT NULL AND expires_at < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired session grants: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
