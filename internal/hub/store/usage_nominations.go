package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var (
	// ErrNominationForbidden: a Hub-wide change without current authority
	// (an active administrator holding the manage permission).
	ErrNominationForbidden = errors.New("store: hub-wide usage publication needs an active administrator with the manage permission")
	// ErrNominationIneligible: the bucket holds observations of other users'
	// sources whose owners did not allow republishing.
	ErrNominationIneligible = errors.New("store: bucket holds sources whose owners did not allow republishing")
	// ErrNominationMoved: a withdrawal named a bucket that no longer holds
	// the slot (another bucket replaced it); nothing was changed.
	ErrNominationMoved = errors.New("store: the slot is held by another bucket now")
)

// NominationRefusal carries the number of members that block a publication.
// It names no owner or source.
type NominationRefusal struct{ Members int }

func (e *NominationRefusal) Error() string {
	return fmt.Sprintf("%v (%d member(s))", ErrNominationIneligible, e.Members)
}
func (e *NominationRefusal) Is(t error) bool { return t == ErrNominationIneligible }

// BucketAuthority tells the store what the caller was authorized to do beyond
// their own private buckets. The server derives Domain from the current
// effective manage permission of the authenticated principal; the store checks
// again inside the transaction that Actor is still an active administrator.
//
// Credential is the id of the API key that made the call (empty for a browser
// session); it is only recorded in the audit. Restrict makes a publication
// also require that the publisher may still view the Worker of every foreign
// source (Visible, decided by the caller with the ordinary Usage rules).
type BucketAuthority struct {
	Actor      string
	Domain     bool
	Credential string
	Restrict   bool
	Visible    []string
}

// auditActor is who the audit names: the key that acted, else the account.
func auditActor(actor, credential string) string {
	if credential != "" {
		return "key:" + credential
	}
	return actor
}

func usageNow() int64 { return time.Now().UnixMilli() }

func (s *Store) usageAudit(ctx context.Context, tx *sql.Tx, actor, action, target string) error {
	return s.InsertAudit(ctx, tx, AuditRow{ID: NewID(), TS: usageNow(), Actor: actor, Action: action, Target: target, Outcome: "success"})
}

func activeAdminTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users u WHERE u.id=? AND u.active=1
	 AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='admin')`, id).Scan(&n)
	return n > 0, err
}

// blockedMembersTx counts the members of a bucket that have observations but
// no publishable one. The owner's own sources and consenting owners' sources
// (visible to the publisher) are publishable.
func blockedMembersTx(ctx context.Context, tx *sql.Tx, owner, kind, bucket string, restrict bool, visible []string) (int, error) {
	source := `SELECT m.member_id AS member_id,s.worker_id AS worker_id FROM usage_members m JOIN usage_sessions s ON s.id=m.member_id`
	if kind == "provider" {
		source = `SELECT m.member_id AS member_id,a.worker_id AS worker_id FROM usage_members m JOIN usage_accounts a ON a.account_id=m.member_id`
	}
	list, err := json.Marshal(append([]string{}, visible...))
	if err != nil {
		return 0, err
	}
	// A member blocks only when it has observations and none of them may be
	// published (the same rule the projection applies): an unpublishable copy
	// of a subscription never blocks a publishable one.
	var n int
	err = tx.QueryRowContext(ctx, `WITH src(member_id,worker_id) AS (`+source+` WHERE m.owner_id=?1 AND m.kind=?2 AND m.bucket_id=?3)
	 SELECT count(DISTINCT x.member_id) FROM src x JOIN workers w ON w.id=x.worker_id
	 WHERE NOT EXISTS(SELECT 1 FROM src y JOIN workers wy ON wy.id=y.worker_id WHERE y.member_id=x.member_id
	  AND (wy.owner_id=?1 OR (coalesce((SELECT c.allow_republish FROM usage_publication_consent c WHERE c.owner_id=wy.owner_id),0)=1
	   AND (?4=0 OR wy.id IN (SELECT value FROM json_each(?5))))))`, owner, kind, bucket, restrict, string(list)).Scan(&n)
	return n, err
}

func nominatedTx(ctx context.Context, tx *sql.Tx, owner, kind, bucket string) (user, domain bool, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT scope FROM usage_nominations WHERE owner_id=? AND kind=? AND bucket_id=?`, owner, kind, bucket)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return false, false, err
		}
		user = user || sc == ops.UsageScopeUser
		domain = domain || sc == ops.UsageScopeDomain
	}
	return user, domain, rows.Err()
}

// SetUsageNomination publishes or withdraws a slot atomically. A domain
// slot is replaced or cleared as a whole by kind, whoever published it.
func (s *Store) SetUsageNomination(ctx context.Context, owner string, auth BucketAuthority, a ops.UsageNominationSetArgs) (ops.UsageNominationSetResult, error) {
	var res ops.UsageNominationSetResult
	if err := a.Validate(); err != nil {
		return res, err
	}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		domain := a.Scope == ops.UsageScopeDomain
		if domain {
			if !auth.Domain || auth.Actor == "" {
				return ErrNominationForbidden
			}
			ok, err := activeAdminTx(ctx, tx, auth.Actor)
			if err != nil {
				return err
			}
			if !ok {
				return ErrNominationForbidden
			}
		}
		target := a.Scope + "/" + a.Kind
		if a.Action == ops.UsageNominationClear {
			// A named bucket is a compare-and-clear precondition: the slot is
			// cleared only while that bucket still holds it.
			if a.BucketID != "" {
				var held string
				q, args := `SELECT bucket_id FROM usage_nominations WHERE scope='user' AND owner_id=? AND kind=?`, []any{owner, a.Kind}
				if domain {
					q, args = `SELECT bucket_id FROM usage_nominations WHERE scope='domain' AND kind=?`, []any{a.Kind}
				}
				err := tx.QueryRowContext(ctx, q, args...).Scan(&held)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if held != "" && held != a.BucketID {
					return ErrNominationMoved
				}
			}
			var change sql.Result
			var err error
			if domain {
				change, err = tx.ExecContext(ctx, `DELETE FROM usage_nominations WHERE scope='domain' AND kind=?`, a.Kind)
			} else {
				change, err = tx.ExecContext(ctx, `DELETE FROM usage_nominations WHERE scope='user' AND owner_id=? AND kind=?`, owner, a.Kind)
			}
			if err != nil {
				return err
			}
			n, err := change.RowsAffected()
			if err != nil {
				return err
			}
			res.Changed = n > 0
			if res.Changed {
				return s.usageAudit(ctx, tx, auditActor(actorOr(auth.Actor, owner), auth.Credential), "usage.nomination.clear", target)
			}
			return nil
		}
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM usage_buckets WHERE owner_id=? AND kind=? AND id=?`, owner, a.Kind, a.BucketID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if n, err := blockedMembersTx(ctx, tx, owner, a.Kind, a.BucketID, auth.Restrict, auth.Visible); err != nil {
			return err
		} else if n > 0 {
			return &NominationRefusal{Members: n}
		}
		var prev string
		if domain {
			err = tx.QueryRowContext(ctx, `SELECT bucket_id FROM usage_nominations WHERE scope='domain' AND kind=?`, a.Kind).Scan(&prev)
			if err == nil || errors.Is(err, sql.ErrNoRows) {
				_, err = tx.ExecContext(ctx, `DELETE FROM usage_nominations WHERE scope='domain' AND kind=?`, a.Kind)
			}
		} else {
			err = tx.QueryRowContext(ctx, `SELECT bucket_id FROM usage_nominations WHERE scope='user' AND owner_id=? AND kind=?`, owner, a.Kind).Scan(&prev)
			if err == nil || errors.Is(err, sql.ErrNoRows) {
				_, err = tx.ExecContext(ctx, `DELETE FROM usage_nominations WHERE scope='user' AND owner_id=? AND kind=?`, owner, a.Kind)
			}
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES(?,?,?,?,?,?)`,
			a.Scope, owner, a.Kind, a.BucketID, actorOr(auth.Actor, owner), usageNow()); err != nil {
			return err
		}
		res.Changed = prev != a.BucketID
		res.Replaced = prev != "" && prev != a.BucketID
		return s.usageAudit(ctx, tx, auditActor(actorOr(auth.Actor, owner), auth.Credential), "usage.nomination.set", target+"/"+a.BucketID)
	})
	return res, err
}

func actorOr(actor, owner string) string {
	if actor != "" {
		return actor
	}
	return owner
}

// SetUsagePublication stores the source owner's republishing permission.
func (s *Store) SetUsagePublication(ctx context.Context, owner string, allow bool) error {
	return s.SetUsagePublicationAs(ctx, owner, "", allow)
}

// SetUsagePublicationAs is [Store.SetUsagePublication] for a call made with
// the API key credential (empty for a browser session); the audit names it.
func (s *Store) SetUsagePublicationAs(ctx context.Context, owner, credential string, allow bool) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		v := 0
		if allow {
			v = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_publication_consent(owner_id,allow_republish,updated_at) VALUES(?,?,?)
		 ON CONFLICT(owner_id) DO UPDATE SET allow_republish=excluded.allow_republish,updated_at=excluded.updated_at`, owner, v, usageNow()); err != nil {
			return err
		}
		return s.usageAudit(ctx, tx, auditActor(owner, credential), "usage.publication.set", fmt.Sprintf("allow=%d", v))
	})
}

// UsageAllowRepublish reports the owner's own permission (default false).
func (s *Store) UsageAllowRepublish(ctx context.Context, owner string) (bool, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT allow_republish FROM usage_publication_consent WHERE owner_id=?`, owner).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == 1, err
}

// UsageDomainSlots is the administrator summary of the Hub-wide slots. It is
// independent of any private bucket list.
func (s *Store) UsageDomainSlots(ctx context.Context) ([]ops.UsageDomainSlot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.kind,n.bucket_id,b.label,n.owner_id,u.username,n.actor_id,n.updated_at
	 FROM usage_nominations n JOIN usage_buckets b ON b.owner_id=n.owner_id AND b.kind=n.kind AND b.id=n.bucket_id
	 JOIN users u ON u.id=n.owner_id WHERE n.scope='domain' ORDER BY n.kind`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ops.UsageDomainSlot{}
	for rows.Next() {
		var d ops.UsageDomainSlot
		if err := rows.Scan(&d.Kind, &d.BucketID, &d.Label, &d.PublisherID, &d.PublisherName, &d.UpdatedBy, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UsagePublishableSource is one observation source that a nominated bucket
// may publish: the member and the Worker that observed it.
type UsagePublishableSource struct{ MemberID, WorkerID string }

// UsagePublishableSources lists the observation sources of a bucket's members
// that may be republished: the publisher's own and those of owners who allowed
// it. A projection must use this list BEFORE it deduplicates or aggregates, so
// a refused owner's observations never influence the published numbers. It
// does not check that the publisher may still view a foreign Worker; the
// read-back build uses [Store.UsageReadbackSnapshot], which does.
func (s *Store) UsagePublishableSources(ctx context.Context, publisher, kind, bucket string) ([]UsagePublishableSource, error) {
	return usagePublishableSources(ctx, s.db, publisher, kind, bucket, false, nil)
}

// usagePublishableSources is the shared rule. With restrict, a source owned by
// somebody else also needs its Worker among visible, the Workers the
// publisher may view now.
func usagePublishableSources(ctx context.Context, q usageQuerier, publisher, kind, bucket string, restrict bool, visible []string) ([]UsagePublishableSource, error) {
	source := `SELECT m.member_id,x.worker_id FROM usage_members m JOIN usage_sessions x ON x.id=m.member_id`
	if kind == "provider" {
		source = `SELECT m.member_id,x.worker_id FROM usage_members m JOIN usage_accounts x ON x.account_id=m.member_id`
	}
	list, err := json.Marshal(append([]string{}, visible...))
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT y.member_id,y.worker_id FROM (`+source+` WHERE m.owner_id=?1 AND m.kind=?2 AND m.bucket_id=?3) y
	 JOIN workers w ON w.id=y.worker_id
	 WHERE w.owner_id=?1 OR (coalesce((SELECT c.allow_republish FROM usage_publication_consent c WHERE c.owner_id=w.owner_id),0)=1
	  AND (?4=0 OR w.id IN (SELECT value FROM json_each(?5))))
	 ORDER BY y.member_id,y.worker_id`, publisher, kind, bucket, restrict, string(list))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsagePublishableSource
	for rows.Next() {
		var p UsagePublishableSource
		if err := rows.Scan(&p.MemberID, &p.WorkerID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
