package api

// Session grants (D1 to D7): a stored grant lets one session of a Worker
// reach a session (or every shared session) of another Worker at a level.
// The handlers decide who may write a grant; what a grant allows on a call is
// judged by authz for every call.

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// GrantWorker is what the grant handlers know of a Worker.
type GrantWorker struct {
	ID, Name string
	// OwnerID is the owning user's id, Owner the name shown for it.
	OwnerID, Owner string
	// Approved is false while the Worker waits for approval.
	Approved bool
	// CanBeSource is false for a Worker that cannot originate calls (thin).
	CanBeSource bool
}

// GrantStore is what the grant handlers need; *store.Store implements it.
type GrantStore interface {
	PutSessionGrant(ctx context.Context, row store.SessionGrantRow, ifRevision uint64) (store.SessionGrantRow, error)
	DeleteSessionGrant(ctx context.Context, id string) (bool, error)
	ApproveSessionGrant(ctx context.Context, id, approverUserID string, ifRevision uint64) (store.SessionGrantRow, error)
	SessionGrant(ctx context.Context, id string) (store.SessionGrantRow, error)
	ListSessionGrants(ctx context.Context, f store.SessionGrantFilter) ([]store.SessionGrantRow, error)
}

var _ GrantStore = (*store.Store)(nil)

// RegisterGrants registers the grant_* operations. workers lists every
// Worker; dec answers whether the caller is an administrator; au receives the
// audit events of grant_set and grant_approve (the dispatcher audits
// grant_revoke).
func RegisterGrants(r *Registry, st GrantStore, workers func(context.Context) ([]GrantWorker, error), dec Decider, au Auditor) error {
	if st == nil || workers == nil || dec == nil {
		return errors.New("api: the grant handlers need a store, the Workers and a decider")
	}
	h := grantHandlers{st: st, workers: workers, dec: dec, au: au, now: time.Now}
	for _, reg := range []error{
		// grant_list is decided on the caller's own user resource, so every
		// user may call it; the handler filters by the Workers they own.
		HandleTyped(r, ops.OpGrantList, h.list, WithResource(h.listResource)),
		HandleTyped(r, ops.OpGrantSet, h.set, WithResource(h.setResource)),
		HandleTyped(r, ops.OpGrantRevoke, h.revoke, WithResource(h.revokeResource)),
		HandleTyped(r, ops.OpGrantApprove, h.approve),
	} {
		if reg != nil {
			return reg
		}
	}
	return nil
}

type grantHandlers struct {
	st      GrantStore
	workers func(context.Context) ([]GrantWorker, error)
	dec     Decider
	au      Auditor
	now     func() time.Time
}

func (h grantHandlers) byID(ctx context.Context) (map[string]GrantWorker, error) {
	ws, err := h.workers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]GrantWorker, len(ws))
	for _, w := range ws {
		m[w.ID] = w
	}
	return m, nil
}

// isAdmin reports whether the caller acts with an administrator's own
// authority. An act-as principal never does: it holds what the acted user
// holds (D4).
func (h grantHandlers) isAdmin(ctx context.Context, p authz.Principal) (bool, error) {
	if p.Kind == authz.KindActAs || p.UserID == "" {
		return false, nil
	}
	err := h.dec.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: p.UserID})
	if err == nil {
		return true, nil
	}
	if e := ErrorOf(err); e.Code == proto.CodeForbidden || e.Code == proto.CodeNotFound {
		return false, nil
	}
	return false, err
}

// record writes the audit event of a grant_set or grant_approve, also for a
// refusal. id is "" while no grant is known.
func (h grantHandlers) record(ctx context.Context, c *Call, action, id string, err error) {
	if h.au == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = string(ErrorOf(err).Code)
	}
	target := "grant:" + id
	if id == "" {
		target = "grant:new"
	}
	_ = h.au.Record(context.WithoutCancel(ctx), audit.Event{
		Actor: c.Identity.Actor, ActingAs: c.Identity.ActingAs, Action: action, Target: target, Outcome: outcome,
	})
}

func (h grantHandlers) info(g store.SessionGrantRow, ws map[string]GrantWorker) ops.Grant {
	level, _ := ops.GrantLevelName(g.Level)
	out := ops.Grant{
		ID: g.ID, SourceWorkerID: g.SourceWorkerID, SourceSession: g.SourceSession, SourceProof: ops.GrantProofClaimed,
		TargetWorkerID: g.TargetWorkerID, TargetSession: g.TargetSession, Level: level, Authority: g.Authority,
		GrantedBy: g.GrantedBy, Revision: g.Revision,
		CreatedAt: ops.Timestamp(time.UnixMilli(g.CreatedAt)), UpdatedAt: ops.Timestamp(time.UnixMilli(g.UpdatedAt)),
		Suspended: g.SuspendedAt != nil, SuspendCause: g.SuspendCause, Note: g.Note,
		SourceWorkerName: ws[g.SourceWorkerID].Name, TargetWorkerName: ws[g.TargetWorkerID].Name,
		SourceOwner: ws[g.SourceWorkerID].Owner, TargetOwner: ws[g.TargetWorkerID].Owner,
		State: ops.GrantStateActive,
	}
	if g.SourceIncarnation != "" {
		out.SourceProof = ops.GrantProofVerified
	}
	if g.ExpiresAt != nil {
		t := ops.Timestamp(time.UnixMilli(*g.ExpiresAt))
		out.ExpiresAt = &t
		if !h.now().Before(time.UnixMilli(*g.ExpiresAt)) {
			out.State = ops.GrantStateExpired
		}
	}
	if out.Suspended {
		out.State = ops.GrantStateSuspended
	}
	return out
}

func (h grantHandlers) list(ctx context.Context, c *Call, a ops.GrantListArgs) (ops.GrantListResult, error) {
	// The list spans Workers: an API key limited to some resources may not
	// read it.
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.GrantListResult{}, err
	}
	admin, err := h.isAdmin(ctx, c.Principal)
	if err != nil {
		return ops.GrantListResult{}, err
	}
	ws, err := h.byID(ctx)
	if err != nil {
		return ops.GrantListResult{}, err
	}
	f := store.SessionGrantFilter{WorkerID: a.WorkerID}
	if !admin {
		f.OwnerID = c.Principal.UserID
	}
	rows, err := h.st.ListSessionGrants(ctx, f)
	if err != nil {
		return ops.GrantListResult{}, err
	}
	out := ops.GrantListResult{Grants: make([]ops.Grant, 0, len(rows))}
	for _, g := range rows {
		// The current owners decide who sees a grant, not the stored ones.
		if !admin && ws[g.SourceWorkerID].OwnerID != c.Principal.UserID && ws[g.TargetWorkerID].OwnerID != c.Principal.UserID {
			continue
		}
		out.Grants = append(out.Grants, h.info(g, ws))
	}
	return out, nil
}

// listResource decides grant_list on the caller's own user resource, also
// when a Worker filter is given (the filter never widens what is listed).
func (h grantHandlers) listResource(p authz.Principal, _ any) (authz.Resource, error) {
	if p.UserID == "" {
		return nil, NewError(proto.CodeForbidden, "not allowed")
	}
	return authz.UserResource{ID: p.UserID}, nil
}

// setResource decides grant_set on the source Worker.
func (h grantHandlers) setResource(_ authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.GrantSetArgs)
	if !ok {
		return nil, NewError(proto.CodeInternal, "")
	}
	return authz.WorkerResource{ID: a.SourceWorkerID}, nil
}

func grantStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrGrantRevision):
		return NewError(proto.CodeConflict, "the grant was changed meanwhile; reload and decide again")
	case errors.Is(err, store.ErrGrantExists):
		return NewError(proto.CodeConflict, "a grant for this source and target exists already")
	case errors.Is(err, store.ErrGrantOwners):
		return NewError(proto.CodeConflict, "the Worker changed owner")
	case errors.Is(err, store.ErrGrantInvalid):
		return NewError(proto.CodeInvalidArgument, "the grant is not valid")
	case errors.Is(err, store.ErrNotFound):
		return NewError(proto.CodeNotFound, "no such grant")
	}
	return err
}

func (h grantHandlers) set(ctx context.Context, c *Call, a ops.GrantSetArgs) (ops.GrantSetResult, error) {
	g, ws, err := h.doSet(ctx, c, a)
	id := a.ID
	if err == nil {
		id = g.ID
	}
	h.record(ctx, c, ops.OpGrantSet, id, err)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	return ops.GrantSetResult{Outcome: ops.OutcomeDelivered, Grant: h.info(g, ws)}, nil
}

func (h grantHandlers) doSet(ctx context.Context, c *Call, a ops.GrantSetArgs) (store.SessionGrantRow, map[string]GrantWorker, error) {
	var none store.SessionGrantRow
	p := c.Principal
	if p.UserID == "" {
		return none, nil, NewError(proto.CodeForbidden, "not allowed")
	}
	level, _ := ops.GrantLevelRank(a.Level)
	if a.Level == ops.GrantLevelMessage {
		return none, nil, NewError(proto.CodeInvalidArgument, "level message is not available yet")
	}
	ws, err := h.byID(ctx)
	if err != nil {
		return none, nil, err
	}
	src, okS := ws[a.SourceWorkerID]
	tgt, okT := ws[a.TargetWorkerID]
	if !okS || !okT {
		return none, nil, NewError(proto.CodeNotFound, "no such Worker")
	}
	if !src.Approved || !tgt.Approved {
		return none, nil, NewError(proto.CodeConflict, "both Workers must be approved")
	}
	admin, err := h.isAdmin(ctx, p)
	if err != nil {
		return none, nil, err
	}
	var exp *int64
	if a.ExpiresAt != "" {
		var ts ops.Timestamp
		if err := ts.UnmarshalJSON([]byte(`"` + a.ExpiresAt + `"`)); err != nil {
			return none, nil, NewError(proto.CodeInvalidArgument, "expires_at must be RFC 3339 UTC with milliseconds")
		}
		ms := ts.Time().UnixMilli()
		exp = &ms
	}
	if a.ID != "" {
		cur, err := h.st.SessionGrant(ctx, a.ID)
		if err != nil {
			return none, nil, grantStoreErr(err)
		}
		if cur.SourceWorkerID != a.SourceWorkerID || cur.SourceSession != a.SourceSession ||
			cur.TargetWorkerID != a.TargetWorkerID || cur.TargetSession != a.TargetSession {
			return none, nil, NewError(proto.CodeInvalidArgument, "the source and the target of a grant cannot be changed")
		}
		ownsBoth := src.OwnerID == p.UserID && tgt.OwnerID == p.UserID
		if !admin && !(cur.Authority == store.GrantAuthorityOwner && ownsBoth) {
			return none, nil, NewError(proto.CodeForbidden, "not allowed")
		}
		if cur.SuspendedAt != nil {
			return none, nil, NewError(proto.CodeConflict, "the grant is suspended; an administrator must approve it first")
		}
		cur.Level, cur.ExpiresAt, cur.Note, cur.UpdatedAt = level, exp, a.Note, 0
		out, err := h.st.PutSessionGrant(ctx, cur, a.IfRevision)
		return out, ws, grantStoreErr(err)
	}
	if !src.CanBeSource {
		return none, nil, NewError(proto.CodeConflict, "this Worker cannot reach others (a thin Worker)")
	}
	// D4: the authority behind the grant.
	authority := store.GrantAuthorityAdmin
	switch {
	case src.OwnerID == tgt.OwnerID && src.OwnerID == p.UserID:
		authority = store.GrantAuthorityOwner
	case admin:
	case src.OwnerID != tgt.OwnerID:
		return none, nil, NewError(proto.CodeForbidden, "only an administrator can grant between Workers of different owners")
	default:
		return none, nil, NewError(proto.CodeForbidden, "not allowed")
	}
	out, err := h.st.PutSessionGrant(ctx, store.SessionGrantRow{
		SourceWorkerID: a.SourceWorkerID, SourceSession: a.SourceSession, TargetWorkerID: a.TargetWorkerID,
		TargetSession: a.TargetSession, Level: level, Authority: authority, GrantedBy: p.UserID,
		SourceOwnerID: src.OwnerID, TargetOwnerID: tgt.OwnerID, Note: a.Note, ExpiresAt: exp,
	}, 0)
	return out, ws, grantStoreErr(err)
}

// revokeResource decides grant_revoke on a Worker of the stored grant: the
// one the caller owns, else the source Worker (an administrator operates
// every Worker). A grantor who is no administrator any more and owns neither
// Worker is refused by that decision.
func (h grantHandlers) revokeResource(p authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.GrantRevokeArgs)
	if !ok {
		return nil, NewError(proto.CodeInternal, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	g, err := h.st.SessionGrant(ctx, a.ID)
	if err != nil {
		return nil, grantStoreErr(err)
	}
	if ws, err := h.byID(ctx); err == nil && ws[g.TargetWorkerID].OwnerID == p.UserID && ws[g.SourceWorkerID].OwnerID != p.UserID {
		return authz.WorkerResource{ID: g.TargetWorkerID}, nil
	}
	return authz.WorkerResource{ID: g.SourceWorkerID}, nil
}

func (h grantHandlers) revoke(ctx context.Context, _ *Call, a ops.GrantRevokeArgs) (ops.WriteResult, error) {
	g, err := h.st.SessionGrant(ctx, a.ID)
	if err != nil {
		return ops.WriteResult{}, grantStoreErr(err)
	}
	if a.IfRevision != 0 && a.IfRevision != g.Revision {
		return ops.WriteResult{}, grantStoreErr(store.ErrGrantRevision)
	}
	ok, err := h.st.DeleteSessionGrant(ctx, a.ID)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if !ok {
		return ops.WriteResult{}, NewError(proto.CodeNotFound, "no such grant")
	}
	return delivered(), nil
}

func (h grantHandlers) approve(ctx context.Context, c *Call, a ops.GrantApproveArgs) (ops.GrantSetResult, error) {
	res, err := h.doApprove(ctx, c, a)
	h.record(ctx, c, ops.OpGrantApprove, a.ID, err)
	return res, err
}

func (h grantHandlers) doApprove(ctx context.Context, c *Call, a ops.GrantApproveArgs) (ops.GrantSetResult, error) {
	admin, err := h.isAdmin(ctx, c.Principal)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	if !admin {
		return ops.GrantSetResult{}, NewError(proto.CodeForbidden, "not allowed")
	}
	g, err := h.st.ApproveSessionGrant(ctx, a.ID, c.Principal.UserID, a.IfRevision)
	if err != nil {
		return ops.GrantSetResult{}, grantStoreErr(err)
	}
	ws, err := h.byID(ctx)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	return ops.GrantSetResult{Outcome: ops.OutcomeDelivered, Grant: h.info(g, ws)}, nil
}
