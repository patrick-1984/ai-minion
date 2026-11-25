package api

// Session grants (D1 to D7): a stored grant lets one session of a Worker
// reach a session (or every shared session) of another Worker at a level.
// The handlers decide who may write a grant; what a grant allows on a call is
// judged by authz for every call.

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
	PutSessionGrant(ctx context.Context, row store.SessionGrantRow, ifRevision uint64, w store.GrantWrite) (store.SessionGrantRow, error)
	RevokeSessionGrant(ctx context.Context, id string, ifRevision uint64, w store.GrantWrite) (store.SessionGrantRow, error)
	ApproveSessionGrant(ctx context.Context, id string, ifRevision uint64, w store.GrantWrite) (store.SessionGrantRow, error)
	SessionGrant(ctx context.Context, id string) (store.SessionGrantRow, error)
	ListSessionGrants(ctx context.Context, f store.SessionGrantFilter) ([]store.SessionGrantRow, error)
}

var _ GrantStore = (*store.Store)(nil)

// RegisterGrants registers the grant_* operations. workers lists every
// Worker; dec answers whether the caller is an administrator; au receives the
// audit events of a refused grant_set or grant_approve. The audit row of a
// change that commits is written by the store in the transaction of the
// change (grant_revoke included; the dispatcher audits its refusals).
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

// Audit actions of the grant handlers.
const (
	AuditActionGrantCreate  = "grant.create"
	AuditActionGrantUpdate  = "grant.update"
	AuditActionGrantApprove = "grant.approve"
	// AuditActionGrantRevoke is the action of every audit row of grant_revoke:
	// the row the store writes with the delete and the dispatcher's row of a
	// refusal. The operation keeps its name.
	AuditActionGrantRevoke = "grant.revoke"
)

// grantAuditOutcome maps a handler error to the audit outcome: a refusal
// (forbidden, not_found) is "denied", any other error "failed".
func grantAuditOutcome(err error) string {
	if err == nil {
		return AuditOutcomeSuccess
	}
	if c := ErrorOf(err).Code; c == proto.CodeForbidden || c == proto.CodeNotFound {
		return AuditOutcomeDenied
	}
	return auditOutcomeError
}

// grantAuditSource is the audit Source of a grant, grantAuditTarget its
// Target ("*" for every shared session of the target Worker).
func grantAuditSource(workerID, session string) string {
	if workerID == "" {
		return ""
	}
	return workerID + ":" + session
}

func grantAuditTarget(workerID, session string) string {
	if workerID == "" {
		return "grant"
	}
	if session == "" {
		session = "*"
	}
	return workerID + ":" + session
}

// grantAuditLevel is the level name of a stored rank for an audit detail.
func grantAuditLevel(rank int) string {
	if n, ok := ops.GrantLevelName(rank); ok {
		return n
	}
	return "unknown"
}

// grantAuditRow turns the event of a change that commits into the audit row
// the store writes in the transaction of the change. An event the audit log
// would refuse fails the change: no grant change without its audit row.
func grantAuditRow(c *Call, ev audit.Event) (store.AuditRow, error) {
	ev.Actor, ev.ActingAs, ev.Outcome = c.Identity.Actor, c.Identity.ActingAs, AuditOutcomeSuccess
	if !audit.ValidDetail(ev.Detail) {
		ev.Detail = ""
	}
	if err := ev.Validate(); err != nil {
		return store.AuditRow{}, err
	}
	return store.AuditRow{Actor: ev.Actor, ActingAs: ev.ActingAs, Action: ev.Action, Target: ev.Target, Outcome: ev.Outcome,
		Source: ev.Source, GrantID: ev.GrantID, Detail: ev.Detail}, nil
}

// record writes the audit event of a refused grant_set or grant_approve. The
// detail holds only levels, flags and numbers, never a name.
func (h grantHandlers) record(ctx context.Context, c *Call, ev audit.Event, err error) {
	if h.au == nil || err == nil {
		return
	}
	ev.Actor, ev.ActingAs, ev.Outcome = c.Identity.Actor, c.Identity.ActingAs, grantAuditOutcome(err)
	if !audit.ValidDetail(ev.Detail) {
		ev.Detail = ""
	}
	_ = h.au.Record(context.WithoutCancel(ctx), ev)
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
		gi := h.info(g, ws)
		if !admin {
			maskGrant(&gi, c.Principal.UserID, ws)
		}
		out.Grants = append(out.Grants, gi)
	}
	return out, nil
}

// maskGrant hides from a viewer who is no administrator the session name on
// the side of a grant whose Worker they do not own: the name becomes
// "a session of <owner>" and the masked member says so. The id, the Workers
// and the level stay. An empty target (every shared session) names nothing.
func maskGrant(g *ops.Grant, viewer string, ws map[string]GrantWorker) {
	label := func(w GrantWorker) string {
		if w.Owner == "" {
			return "a session of another user"
		}
		return "a session of " + w.Owner
	}
	if w := ws[g.SourceWorkerID]; w.OwnerID != viewer || viewer == "" {
		g.SourceSession, g.SourceMasked = label(w), true
	}
	if w := ws[g.TargetWorkerID]; g.TargetSession != "" && (w.OwnerID != viewer || viewer == "") {
		g.TargetSession, g.TargetMasked = label(w), true
	}
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
	case errors.Is(err, store.ErrGrantNotSuspended):
		return NewError(proto.CodeConflict, "this grant is not suspended")
	case errors.Is(err, store.ErrGrantAuthority):
		return NewError(proto.CodeForbidden, "not allowed")
	case errors.Is(err, store.ErrNotFound):
		return NewError(proto.CodeNotFound, "no such grant")
	}
	return err
}

func (h grantHandlers) set(ctx context.Context, c *Call, a ops.GrantSetArgs) (ops.GrantSetResult, error) {
	var old *store.SessionGrantRow
	g, ws, err := h.doSet(ctx, c, a, &old)
	if err != nil {
		// The store wrote nothing: the refusal is audited by itself.
		h.record(ctx, c, grantSetEvent(a, old, nil), err)
		return ops.GrantSetResult{}, err
	}
	return ops.GrantSetResult{Outcome: ops.OutcomeDelivered, Grant: h.info(g, ws)}, nil
}

// grantSetEvent is the audit event of a grant_set: old is the stored grant
// before an update (nil when it was not read), now the grant as stored (nil
// for a refusal).
func grantSetEvent(a ops.GrantSetArgs, old, now *store.SessionGrantRow) audit.Event {
	ev := audit.Event{Action: AuditActionGrantCreate, GrantID: a.ID,
		Source: grantAuditSource(a.SourceWorkerID, a.SourceSession), Target: grantAuditTarget(a.TargetWorkerID, a.TargetSession)}
	newLevel := "unknown"
	if _, ok := ops.GrantLevelRank(a.Level); ok {
		newLevel = a.Level
	}
	switch {
	case a.ID != "":
		ev.Action = AuditActionGrantUpdate
		ev.Detail = "level_new=" + newLevel
		if old != nil {
			ev.Detail = "level_old=" + grantAuditLevel(old.Level) + " " + ev.Detail
		}
		if now != nil {
			ev.Detail += " rev=" + strconv.FormatUint(now.Revision, 10)
		}
	case now != nil:
		ev.GrantID = now.ID
		ev.Detail = "level=" + newLevel + " authority=" + now.Authority + " expires=none"
		if now.ExpiresAt != nil {
			ev.Detail = "level=" + newLevel + " authority=" + now.Authority + " expires=" + strconv.FormatInt(*now.ExpiresAt, 10)
		}
	default:
		ev.Detail = "level=" + newLevel
	}
	return ev
}

// doSet decides and stores a grant_set. For an update *old receives the
// stored grant as soon as it is read (for the audit).
func (h grantHandlers) doSet(ctx context.Context, c *Call, a ops.GrantSetArgs, old **store.SessionGrantRow) (store.SessionGrantRow, map[string]GrantWorker, error) {
	var none store.SessionGrantRow
	p := c.Principal
	if p.UserID == "" {
		return none, nil, NewError(proto.CodeForbidden, "not allowed")
	}
	level, _ := ops.GrantLevelRank(a.Level)
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
	// The store reads the caller's role and active state again in the
	// transaction that writes, and writes the audit row there.
	write := store.GrantWrite{ActorID: p.UserID, Audit: func(o *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
		return grantAuditRow(c, grantSetEvent(a, o, &now))
	}}
	var exp *int64
	if a.ExpiresAt != "" {
		var ts ops.Timestamp
		if err := ts.UnmarshalJSON([]byte(`"` + a.ExpiresAt + `"`)); err != nil {
			return none, nil, NewError(proto.CodeInvalidArgument, "expires_at must be RFC 3339 UTC with milliseconds")
		}
		// An end in the past would store a grant that never allows
		// anything, on a create and on an update alike.
		if !ts.Time().After(h.now()) {
			return none, nil, &Error{Code: proto.CodeInvalidArgument, Message: "expires_at must be in the future",
				Details: map[string]any{"field": "expires_at"}}
		}
		ms := ts.Time().UnixMilli()
		exp = &ms
	}
	if a.ID != "" {
		cur, err := h.st.SessionGrant(ctx, a.ID)
		if err != nil {
			return none, nil, grantStoreErr(err)
		}
		before := cur
		*old = &before
		if cur.SourceWorkerID != a.SourceWorkerID || cur.SourceSession != a.SourceSession ||
			cur.TargetWorkerID != a.TargetWorkerID || cur.TargetSession != a.TargetSession {
			return none, nil, NewError(proto.CodeInvalidArgument, "the source and the target of a grant cannot be changed")
		}
		ownsBoth := src.OwnerID == p.UserID && tgt.OwnerID == p.UserID
		if !admin && (cur.Authority != store.GrantAuthorityOwner || !ownsBoth) {
			return none, nil, NewError(proto.CodeForbidden, "not allowed")
		}
		if cur.SuspendedAt != nil {
			return none, nil, NewError(proto.CodeConflict, "the grant is suspended; an administrator must approve it first")
		}
		cur.Level, cur.ExpiresAt, cur.Note, cur.UpdatedAt = level, exp, a.Note, 0
		out, err := h.st.PutSessionGrant(ctx, cur, a.IfRevision, write)
		return out, ws, grantStoreErr(err)
	}
	if !src.CanBeSource {
		return none, nil, NewError(proto.CodeConflict, "this Worker cannot reach others (a thin Worker)")
	}
	// D4: the authority behind the grant. Two Workers of one owner take an
	// owner grant, also when an administrator decides it for that owner: it
	// stands while the owner keeps both Workers, whatever becomes of the
	// administrator's role, and the owner can edit it.
	authority := store.GrantAuthorityAdmin
	switch {
	case src.OwnerID == tgt.OwnerID && (src.OwnerID == p.UserID || admin):
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
	}, 0, write)
	return out, ws, grantStoreErr(err)
}

// revokeResource decides grant_revoke (D4: it only narrows). The decision
// runs on the first Worker of the stored grant the caller may operate, the
// source first: that covers the owner of either Worker, an administrator and
// an API key with operate on either Worker. The person who decided the grant
// (granted_by) may always withdraw it, also after losing the administrator
// role: for them the decision runs on their own user resource. Anyone else is
// refused by the dispatcher on the source Worker.
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
	operate := ops.MustActionSet(ops.ActionOperate)
	src, tgt := authz.WorkerResource{ID: g.SourceWorkerID}, authz.WorkerResource{ID: g.TargetWorkerID}
	if h.dec.Decide(ctx, p, operate, src) == nil {
		return src, nil
	}
	if h.dec.Decide(ctx, p, operate, tgt) == nil {
		return tgt, nil
	}
	if grantorPerson(p, g) {
		return authz.UserResource{ID: p.UserID}, nil
	}
	return src, nil
}

// grantorPerson reports whether p is a person (a browser session, also one
// acting as a user) whose effective user decided g.
func grantorPerson(p authz.Principal, g store.SessionGrantRow) bool {
	return p.UserID != "" && p.UserID == g.GrantedBy && (p.Kind == authz.KindBrowserSession || p.Kind == authz.KindActAs)
}

func (h grantHandlers) revoke(ctx context.Context, c *Call, a ops.GrantRevokeArgs) (ops.WriteResult, error) {
	// The decision ran on a Worker of the grant, or for granted_by on the
	// caller's own user resource; that last case is checked again on the row
	// the transaction deletes.
	_, asGrantor := c.Resource.(authz.UserResource)
	write := store.GrantWrite{ActorID: c.Principal.UserID, Audit: func(_ *store.SessionGrantRow, g store.SessionGrantRow) (store.AuditRow, error) {
		if asGrantor && !grantorPerson(c.Principal, g) {
			return store.AuditRow{}, NewError(proto.CodeForbidden, "not allowed")
		}
		// The row commits with the delete; the dispatcher then adds none.
		return grantAuditRow(c, audit.Event{Action: AuditActionGrantRevoke, GrantID: g.ID,
			Source: grantAuditSource(g.SourceWorkerID, g.SourceSession), Target: grantAuditTarget(g.TargetWorkerID, g.TargetSession),
			Detail: "level=" + grantAuditLevel(g.Level) + " authority=" + g.Authority + " rev=" + strconv.FormatUint(g.Revision, 10)})
	}}
	// The dispatcher audits a refusal; its row names the grant.
	c.AuditGrantID = a.ID
	// if_revision is checked by the DELETE itself; zero revokes whatever the
	// revision is.
	if _, err := h.st.RevokeSessionGrant(ctx, a.ID, a.IfRevision, write); err != nil {
		return ops.WriteResult{}, grantStoreErr(err)
	}
	return delivered(), nil
}

func (h grantHandlers) approve(ctx context.Context, c *Call, a ops.GrantApproveArgs) (ops.GrantSetResult, error) {
	res, err := h.doApprove(ctx, c, a)
	if err != nil {
		// Name the grant in the audit of a refusal too, when it exists.
		g, _ := h.st.SessionGrant(ctx, a.ID)
		h.record(ctx, c, grantApproveEvent(a.ID, g), err)
	}
	return res, err
}

// grantApproveEvent is the audit event of a grant_approve on g (the zero row
// when the grant is not known).
func grantApproveEvent(id string, g store.SessionGrantRow) audit.Event {
	ev := audit.Event{Action: AuditActionGrantApprove, GrantID: id,
		Source: grantAuditSource(g.SourceWorkerID, g.SourceSession), Target: grantAuditTarget(g.TargetWorkerID, g.TargetSession)}
	if g.ID != "" {
		ev.Detail = "level=" + grantAuditLevel(g.Level) + " authority=" + g.Authority + " rev=" + strconv.FormatUint(g.Revision, 10)
	}
	return ev
}

func (h grantHandlers) doApprove(ctx context.Context, c *Call, a ops.GrantApproveArgs) (ops.GrantSetResult, error) {
	admin, err := h.isAdmin(ctx, c.Principal)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	if !admin {
		return ops.GrantSetResult{}, NewError(proto.CodeForbidden, "not allowed")
	}
	// The store reads the administrator role again in the transaction that
	// lifts the suspension, and writes the audit row there.
	write := store.GrantWrite{ActorID: c.Principal.UserID, Audit: func(_ *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
		return grantAuditRow(c, grantApproveEvent(now.ID, now))
	}}
	g, err := h.st.ApproveSessionGrant(ctx, a.ID, a.IfRevision, write)
	if err != nil {
		return ops.GrantSetResult{}, grantStoreErr(err)
	}
	ws, err := h.byID(ctx)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	return ops.GrantSetResult{Outcome: ops.OutcomeDelivered, Grant: h.info(g, ws)}, nil
}
