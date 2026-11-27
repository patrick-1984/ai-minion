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
	PutSessionGrants(ctx context.Context, rows []store.SessionGrantRow, w store.GrantWrite) ([]store.SessionGrantRow, error)
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
func RegisterGrants(r *Registry, st GrantStore, workers func(context.Context) ([]GrantWorker, error), dec Decider, au Auditor, opts ...GrantOption) error {
	if st == nil || workers == nil || dec == nil {
		return errors.New("api: the grant handlers need a store, the Workers and a decider")
	}
	h := grantHandlers{st: st, workers: workers, dec: dec, au: au, now: time.Now}
	for _, o := range opts {
		o(&h)
	}
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
	// targetGuard refuses a grant to a target Worker that must stay
	// unshared (device control on); nil refuses nothing.
	targetGuard func(ctx context.Context, p authz.Principal, workerID string) error
}

// GrantOption configures the grant handlers.
type GrantOption func(*grantHandlers)

// WithGrantTargetGuard makes grant_set ask guard before it lets a session of
// another Worker reach the target Worker: a Worker with device control
// (on-device adb) on must stay dedicated and unshared, so the guard answers an
// error for it.
func WithGrantTargetGuard(guard func(ctx context.Context, p authz.Principal, workerID string) error) GrantOption {
	return func(h *grantHandlers) { h.targetGuard = guard }
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
		ID: g.ID, SourceScope: g.Scope(), SourceWorkerID: g.SourceWorkerID, SourceSession: g.SourceSession, SourceProof: ops.GrantProofClaimed,
		TargetWorkerID: g.TargetWorkerID, TargetSession: g.TargetSession, Level: level, Authority: g.Authority,
		GrantedBy: g.GrantedBy, Revision: g.Revision,
		CreatedAt: ops.Timestamp(time.UnixMilli(g.CreatedAt)), UpdatedAt: ops.Timestamp(time.UnixMilli(g.UpdatedAt)),
		Suspended: g.SuspendedAt != nil, SuspendCause: g.SuspendCause, Note: g.Note, CommandShell: g.CommandShell,
		FilesPull: g.FilesPull, FilesPush: g.FilesPush, AppsInstall: g.AppsInstall,
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
	if w := ws[g.SourceWorkerID]; g.SourceSession != "" && (w.OwnerID != viewer || viewer == "") {
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
	if a.SourceWorkerID == "" {
		// Everyone names no source Worker: decided on the first target
		// (the handler requires an administrator).
		if t := a.TargetList(); len(t) > 0 {
			return authz.WorkerResource{ID: t[0].WorkerID}, nil
		}
	}
	return authz.WorkerResource{ID: a.SourceWorkerID}, nil
}

// grantScope is the stored source scope of a grant_set's source_scope.
func grantScope(s string) string {
	switch s {
	case ops.GrantSourceWorker:
		return store.GrantSourceWorker
	case ops.GrantSourceEveryone:
		return store.GrantSourceEveryone
	}
	return store.GrantSourceSession
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
	res, err := h.doSet(ctx, c, a, &old)
	if err != nil {
		// The store wrote nothing: the refusal is audited by itself.
		h.record(ctx, c, grantSetEvent(a, old, nil), err)
		return ops.GrantSetResult{}, err
	}
	return res, nil
}

// grantResultReceipt returns the Receipt of a grant write that answers with
// the grant as stored: the result is built in the writing transaction, from
// what was read before it, into *res, and its request receipt commits with
// the change.
func (h grantHandlers) grantResultReceipt(ctx context.Context, ws map[string]GrantWorker, res *ops.GrantSetResult, note func(store.SessionGrantRow) string) func(store.SessionGrantRow) (*store.OpRequestRow, error) {
	return func(now store.SessionGrantRow) (*store.OpRequestRow, error) {
		*res = h.grantResult(now, ws, note)
		return requestReceipt(ctx, *res)
	}
}

// grantResult is the answer of a grant write that committed g.
func (h grantHandlers) grantResult(g store.SessionGrantRow, ws map[string]GrantWorker, note func(store.SessionGrantRow) string) ops.GrantSetResult {
	res := ops.GrantSetResult{Outcome: ops.OutcomeDelivered, Grant: h.info(g, ws)}
	if note != nil {
		res.Note = note(g)
	}
	return res
}

// keptResult is res when the store built it in the writing transaction, and
// otherwise (a store that called no Receipt) the answer built from g.
func (h grantHandlers) keptResult(res ops.GrantSetResult, g store.SessionGrantRow, ws map[string]GrantWorker, note func(store.SessionGrantRow) string) ops.GrantSetResult {
	if res.Grant.ID != "" {
		return res
	}
	return h.grantResult(g, ws, note)
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
			if old != nil && old.CommandShell != now.CommandShell {
				ev.Detail += " command_shell=" + strconv.FormatBool(now.CommandShell)
			}
			if old != nil && old.FilesPull != now.FilesPull {
				ev.Detail += " files_pull=" + strconv.FormatBool(now.FilesPull)
			}
			if old != nil && old.FilesPush != now.FilesPush {
				ev.Detail += " files_push=" + strconv.FormatBool(now.FilesPush)
			}
			if old != nil && old.AppsInstall != now.AppsInstall {
				ev.Detail += " apps_install=" + strconv.FormatBool(now.AppsInstall)
			}
		}
	case now != nil:
		// The selector the grant was created with: "<worker>:*" for every
		// shared session of a Worker, "*" for everyone.
		ev.GrantID = now.ID
		ev.Source, ev.Target = store.GrantSourceAudit(*now), grantAuditTarget(now.TargetWorkerID, now.TargetSession)
		ev.Detail = "level=" + newLevel + " authority=" + now.Authority + " expires=none"
		if now.ExpiresAt != nil {
			ev.Detail = "level=" + newLevel + " authority=" + now.Authority + " expires=" + strconv.FormatInt(*now.ExpiresAt, 10)
		}
		ev.Detail += " source_scope=" + now.Scope()
		if now.CommandShell {
			ev.Detail += " command_shell=true"
		}
		if now.FilesPull {
			ev.Detail += " files_pull=true"
		}
		if now.FilesPush {
			ev.Detail += " files_push=true"
		}
		if now.AppsInstall {
			ev.Detail += " apps_install=true"
		}
	default:
		ev.Detail = "level=" + newLevel
	}
	return ev
}

// doSet decides and stores a grant_set and returns its result, which is built
// in the writing transaction. For an update *old receives the stored grant as
// soon as it is read (for the audit).
func (h grantHandlers) doSet(ctx context.Context, c *Call, a ops.GrantSetArgs, old **store.SessionGrantRow) (ops.GrantSetResult, error) {
	var none ops.GrantSetResult
	p := c.Principal
	if p.UserID == "" {
		return none, NewError(proto.CodeForbidden, "not allowed")
	}
	level, _ := ops.GrantLevelRank(a.Level)
	scope := grantScope(a.SourceScope)
	targets := a.TargetList()
	ws, err := h.byID(ctx)
	if err != nil {
		return none, err
	}
	admin, err := h.isAdmin(ctx, p)
	if err != nil {
		return none, err
	}
	if scope == store.GrantSourceEveryone && !admin {
		return none, NewError(proto.CodeForbidden, "only an administrator can grant from everyone")
	}
	var src GrantWorker
	if scope != store.GrantSourceEveryone {
		var ok bool
		if src, ok = ws[a.SourceWorkerID]; !ok {
			return none, NewError(proto.CodeNotFound, "no such Worker")
		}
		if !src.Approved {
			return none, NewError(proto.CodeConflict, "both Workers must be approved")
		}
	}
	// Every target is checked before anything is written.
	for _, t := range targets {
		tgt, ok := ws[t.WorkerID]
		if !ok {
			return none, NewError(proto.CodeNotFound, "no such Worker")
		}
		if !tgt.Approved {
			return none, NewError(proto.CodeConflict, "both Workers must be approved")
		}
		if h.targetGuard != nil {
			if err := h.targetGuard(ctx, p, tgt.ID); err != nil {
				return none, err
			}
		}
	}
	// The store reads the caller's role and active state again in the
	// transaction that writes, and writes the audit row and the answer of the
	// request there; the answer is built from what was read before.
	var res ops.GrantSetResult
	write := store.GrantWrite{ActorID: p.UserID, Audit: func(o *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
		return grantAuditRow(c, grantSetEvent(a, o, &now))
	}, Receipt: h.grantResultReceipt(ctx, ws, &res, nil)}
	if p.Kind == authz.KindActAs {
		// An administrator acting as p.UserID (D4): the grant is decided with
		// that user's authority. The act-as period this request was served
		// under must still be the live one, and the administrator still one,
		// when it is written.
		s := c.Identity.Session
		if s == nil || s.ActAs == nil || s.ID == "" || s.UserID != p.ActorID || s.ActAs.TargetID != p.UserID {
			return none, NewError(proto.CodeForbidden, "not allowed")
		}
		write.ActAs = &store.ActAsRow{SessionID: s.ID, ActorID: s.UserID, TargetID: s.ActAs.TargetID,
			StartedAt: store.UnixMS(s.ActAs.StartedAt), ExpiresAt: store.UnixMS(s.ActAs.ExpiresAt)}
	}
	var exp *int64
	if a.ExpiresAt != "" {
		var ts ops.Timestamp
		if err := ts.UnmarshalJSON([]byte(`"` + a.ExpiresAt + `"`)); err != nil {
			return none, NewError(proto.CodeInvalidArgument, "expires_at must be RFC 3339 UTC with milliseconds")
		}
		// An end in the past would store a grant that never allows
		// anything, on a create and on an update alike.
		if !ts.Time().After(h.now()) {
			return none, &Error{Code: proto.CodeInvalidArgument, Message: "expires_at must be in the future",
				Details: map[string]any{"field": "expires_at"}}
		}
		ms := ts.Time().UnixMilli()
		exp = &ms
	}
	if a.ID != "" {
		cur, err := h.st.SessionGrant(ctx, a.ID)
		if err != nil {
			return none, grantStoreErr(err)
		}
		before := cur
		*old = &before
		// Who may edit is decided on the stored grant and the current owners
		// of its Workers, before any endpoint the caller names is compared:
		// a caller who may not edit it gets the same answer whatever names
		// they send, so a masked session name cannot be confirmed.
		ownsBoth := ws[cur.SourceWorkerID].OwnerID == p.UserID && ws[cur.TargetWorkerID].OwnerID == p.UserID
		if !admin && (cur.Authority != store.GrantAuthorityOwner || !ownsBoth) {
			return none, NewError(proto.CodeForbidden, "not allowed")
		}
		if cur.Scope() != scope || cur.SourceWorkerID != a.SourceWorkerID || cur.SourceSession != a.SourceSession ||
			cur.TargetWorkerID != a.TargetWorkerID || cur.TargetSession != a.TargetSession {
			return none, NewError(proto.CodeInvalidArgument, "the source and the target of a grant cannot be changed")
		}
		if cur.SuspendedAt != nil {
			return none, NewError(proto.CodeConflict, "the grant is suspended; an administrator must approve it first")
		}
		cur.Level, cur.ExpiresAt, cur.Note, cur.UpdatedAt = level, exp, a.Note, 0
		// A116: an absent flag keeps the stored one; a command shell needs
		// Control on one named session, so a lower level clears it.
		if a.CommandShell != nil {
			cur.CommandShell = *a.CommandShell
		}
		if level != authz.LevelControl || cur.TargetSession == "" {
			cur.CommandShell = false
		}
		// files.v1: an absent flag keeps the stored one, an explicit false
		// clears it. files_push needs Control, so a lower level clears it (an
		// explicit true below Control is refused by Validate).
		if a.FilesPull != nil {
			cur.FilesPull = *a.FilesPull
		}
		if a.FilesPush != nil {
			cur.FilesPush = *a.FilesPush
		}
		if level != authz.LevelControl {
			cur.FilesPush = false
		}
		// app_install (v0.25.0): the same rules as files_push, never
		// implied by it.
		if a.AppsInstall != nil {
			cur.AppsInstall = *a.AppsInstall
		}
		if level != authz.LevelControl {
			cur.AppsInstall = false
		}
		g, err := h.st.PutSessionGrant(ctx, cur, a.IfRevision, write)
		if err != nil {
			return none, grantStoreErr(err)
		}
		return h.keptResult(res, g, ws, nil), nil
	}
	if scope != store.GrantSourceEveryone && !src.CanBeSource {
		return none, NewError(proto.CodeConflict, "this Worker cannot reach others (a thin Worker)")
	}
	rows := make([]store.SessionGrantRow, 0, len(targets))
	for _, t := range targets {
		tgt := ws[t.WorkerID]
		// D4: the authority behind the grant. Two Workers of one owner take
		// an owner grant, also when an administrator decides it for that
		// owner: it stands while the owner keeps both Workers, whatever
		// becomes of the administrator's role, and the owner can edit it. A
		// grant for everyone stands on an administrator.
		authority := store.GrantAuthorityAdmin
		switch {
		case scope == store.GrantSourceEveryone:
		case src.OwnerID == tgt.OwnerID && (src.OwnerID == p.UserID || admin):
			authority = store.GrantAuthorityOwner
		case admin:
		case src.OwnerID != tgt.OwnerID:
			return none, NewError(proto.CodeForbidden, "only an administrator can grant between Workers of different owners")
		default:
			return none, NewError(proto.CodeForbidden, "not allowed")
		}
		// The boxes ticked on the form are saved with the grant.
		rows = append(rows, store.SessionGrantRow{
			SourceScope: scope, SourceWorkerID: src.ID, SourceSession: a.SourceSession, TargetWorkerID: t.WorkerID,
			TargetSession: t.SessionName, Level: level, Authority: authority, GrantedBy: p.UserID,
			SourceOwnerID: src.OwnerID, TargetOwnerID: tgt.OwnerID, Note: a.Note, ExpiresAt: exp,
			CommandShell: a.CommandShell != nil && *a.CommandShell,
			FilesPull:    a.FilesPull != nil && *a.FilesPull, FilesPush: a.FilesPush != nil && *a.FilesPush,
			AppsInstall: a.AppsInstall != nil && *a.AppsInstall,
		})
	}
	if a.Targets == nil {
		// The request of before v0.26.0: one grant, the same answer.
		g, err := h.st.PutSessionGrant(ctx, rows[0], 0, write)
		if err != nil {
			return none, grantStoreErr(err)
		}
		return h.keptResult(res, g, ws, nil), nil
	}
	// Several targets: every grant, its audit row and the one answer of the
	// request commit together, or nothing does.
	write.Receipt = nil
	write.ReceiptMany = func(now []store.SessionGrantRow) (*store.OpRequestRow, error) {
		res = h.manyResult(now, ws)
		return requestReceipt(ctx, res)
	}
	gs, err := h.st.PutSessionGrants(ctx, rows, write)
	if err != nil {
		return none, grantStoreErr(err)
	}
	if res.Grant.ID == "" {
		res = h.manyResult(gs, ws)
	}
	return res, nil
}

// manyResult is the answer of a grant_set that created the grants gs.
func (h grantHandlers) manyResult(gs []store.SessionGrantRow, ws map[string]GrantWorker) ops.GrantSetResult {
	res := ops.GrantSetResult{Outcome: ops.OutcomeDelivered}
	for _, g := range gs {
		res.Grants = append(res.Grants, h.info(g, ws))
		res.CreatedIDs = append(res.CreatedIDs, g.ID)
	}
	if len(res.Grants) > 0 {
		res.Grant = res.Grants[0]
	}
	return res
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
			Source: store.GrantSourceAudit(g), Target: grantAuditTarget(g.TargetWorkerID, g.TargetSession),
			Detail: "level=" + grantAuditLevel(g.Level) + " authority=" + g.Authority + " rev=" + strconv.FormatUint(g.Revision, 10)})
	}, Receipt: func(store.SessionGrantRow) (*store.OpRequestRow, error) {
		// The answer commits with the delete: a repeat after a stop at the
		// commit is answered delivered, not not_found.
		return requestReceipt(ctx, delivered())
	}}
	// The dispatcher audits a refusal; its row names the grant.
	c.AuditGrantID = a.ID
	// if_revision is required (the arguments refuse zero) and checked by the
	// DELETE itself: a grant changed meanwhile stays.
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
		ev.Source = store.GrantSourceAudit(g)
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
	// What the answer shows is read before the change: once the suspension
	// is lifted, the answer commits with it and nothing can fail after.
	ws, err := h.byID(ctx)
	if err != nil {
		return ops.GrantSetResult{}, err
	}
	// The store reads the administrator role again in the transaction that
	// lifts the suspension, and writes the audit row and the answer there.
	var res ops.GrantSetResult
	write := store.GrantWrite{ActorID: c.Principal.UserID, Audit: func(_ *store.SessionGrantRow, now store.SessionGrantRow) (store.AuditRow, error) {
		return grantAuditRow(c, grantApproveEvent(now.ID, now))
	}, Receipt: h.grantResultReceipt(ctx, ws, &res, nil)}
	g, err := h.st.ApproveSessionGrant(ctx, a.ID, a.IfRevision, write)
	if err != nil {
		return ops.GrantSetResult{}, grantStoreErr(err)
	}
	return h.keptResult(res, g, ws, nil), nil
}
