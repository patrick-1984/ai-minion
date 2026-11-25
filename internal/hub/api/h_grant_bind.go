package api

// grant_bind (DESIGN section 6): a direct grant becomes Verified when it is
// bound to the kernel-proven terminal incarnation of its source session, and
// Claimed again when it is unbound.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit actions of grant_bind: binding and unbinding.
const (
	AuditActionGrantBind   = "grant.bind"
	AuditActionGrantUnbind = "grant.unbind"
)

// GrantBindStore is what grant_bind needs; *store.Store implements it.
type GrantBindStore interface {
	SessionGrant(ctx context.Context, id string) (store.SessionGrantRow, error)
	BindSessionGrant(ctx context.Context, id, incarnation string, ifRevision uint64, w store.GrantWrite) (store.SessionGrantRow, error)
}

var _ GrantBindStore = (*store.Store)(nil)

// GrantBindLane is the live master lane as grant_bind uses it;
// *workers.MasterControl implements it.
type GrantBindLane interface {
	// PreparedIncarnation returns the incarnation id the Worker reports now
	// for the session, false when it is not a prepared terminal right now.
	PreparedIncarnation(ctx context.Context, workerID, session string) (string, bool)
	// RefreshLeases recomputes the leases of the Worker's lane.
	RefreshLeases(workerID string)
}

// RegisterGrantBind registers grant_bind. workers lists every Worker, dec
// answers whether the caller is an administrator, au receives the audit event
// of a refusal. The audit row of a bind or unbind that commits is written by
// the store in the transaction of the change.
func RegisterGrantBind(r *Registry, st GrantBindStore, lane GrantBindLane, workers func(context.Context) ([]GrantWorker, error), dec Decider, au Auditor) error {
	if st == nil || lane == nil || workers == nil || dec == nil {
		return errors.New("api: grant_bind needs a store, the master lane, the Workers and a decider")
	}
	h := grantBindHandler{grantHandlers: grantHandlers{workers: workers, dec: dec, au: au, now: time.Now}, st: st, lane: lane}
	return HandleTyped(r, ops.OpGrantBind, h.bind, WithResource(h.resource))
}

type grantBindHandler struct {
	grantHandlers
	st   GrantBindStore
	lane GrantBindLane
}

// resource decides grant_bind on the source Worker of the stored grant; a
// missing grant answers not_found.
func (h grantBindHandler) resource(_ authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.GrantBindArgs)
	if !ok || a == nil {
		return nil, NewError(proto.CodeInternal, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	g, err := h.st.SessionGrant(ctx, a.ID)
	if err != nil {
		return nil, grantStoreErr(err)
	}
	return authz.WorkerResource{ID: g.SourceWorkerID}, nil
}

func (h grantBindHandler) bind(ctx context.Context, c *Call, a ops.GrantBindArgs) (ops.GrantSetResult, error) {
	res, err := h.doBind(ctx, c, a)
	if err != nil {
		// The store wrote nothing: the refusal is audited by itself, with the
		// grant named when it exists.
		g, _ := h.st.SessionGrant(ctx, a.ID)
		h.record(ctx, c, grantBindEvent(a.ID, a.Bind, g), err)
	}
	return res, err
}

// grantBindEvent is the audit event of a grant_bind on g (the zero row when
// the grant is not known): "grant.bind" or "grant.unbind", the grant id, its
// source and target, and a detail of flags, level and revision only.
func grantBindEvent(id string, bind bool, g store.SessionGrantRow) audit.Event {
	ev := audit.Event{Action: AuditActionGrantUnbind, GrantID: id,
		Source: grantAuditSource(g.SourceWorkerID, g.SourceSession), Target: grantAuditTarget(g.TargetWorkerID, g.TargetSession),
		Detail: "bind=" + strconv.FormatBool(bind)}
	if bind {
		ev.Action = AuditActionGrantBind
	}
	if g.ID != "" {
		ev.Detail += " level=" + grantAuditLevel(g.Level) + " rev=" + strconv.FormatUint(g.Revision, 10)
	}
	return ev
}

func (h grantBindHandler) doBind(ctx context.Context, c *Call, a ops.GrantBindArgs) (ops.GrantSetResult, error) {
	p := c.Principal
	if p.UserID == "" {
		return ops.GrantSetResult{}, NewError(proto.CodeForbidden, "not allowed")
	}
	cur, err := h.st.SessionGrant(ctx, a.ID)
	if err != nil {
		return ops.GrantSetResult{}, grantStoreErr(err)
	}
	ws, err := h.byID(ctx)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	admin, err := h.isAdmin(ctx, p)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	// An administrator, or the owner of both Workers of the grant.
	src, okS := ws[cur.SourceWorkerID]
	tgt, okT := ws[cur.TargetWorkerID]
	if !admin && (!okS || !okT || src.OwnerID != p.UserID || tgt.OwnerID != p.UserID) {
		return ops.GrantSetResult{}, NewError(proto.CodeForbidden, "not allowed")
	}
	if cur.Revision != a.IfRevision {
		return ops.GrantSetResult{}, grantStoreErr(store.ErrGrantRevision)
	}
	incarnation := ""
	if a.Bind {
		if cur.SuspendedAt != nil {
			return ops.GrantSetResult{}, NewError(proto.CodeConflict, "the grant is suspended; an administrator must approve it first")
		}
		var ok bool
		if incarnation, ok = h.lane.PreparedIncarnation(ctx, cur.SourceWorkerID, cur.SourceSession); !ok {
			return ops.GrantSetResult{}, NewError(proto.CodeConflict, "this session is not a prepared terminal right now")
		}
	}
	out := ops.GrantSetResult{Outcome: ops.OutcomeDelivered}
	g := cur
	if cur.SourceIncarnation != incarnation {
		// The store reads the caller's authority again in the transaction
		// that writes, and writes the audit row there.
		write := store.GrantWrite{ActorID: p.UserID, Audit: func(_ *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
			return grantAuditRow(c, grantBindEvent(now.ID, a.Bind, now))
		}}
		if g, err = h.st.BindSessionGrant(ctx, a.ID, incarnation, a.IfRevision, write); err != nil {
			return ops.GrantSetResult{}, grantStoreErr(err)
		}
		// The lease of the terminal starts or ends now.
		h.lane.RefreshLeases(g.SourceWorkerID)
	}
	if g.SourceIncarnation != "" {
		out.Note = ops.GrantBindNote
	}
	out.Grant = h.info(g, ws)
	return out, nil
}
