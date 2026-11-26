package api

// grant_history: the audit rows of one session grant, for the owner of a
// Worker the grant touches and for administrators. A grant that has ended has
// no row any more, so its Workers are read from its audit rows.

import (
	"context"
	"errors"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// GrantHistoryStore is what grant_history needs of the store.
type GrantHistoryStore interface {
	SessionGrant(ctx context.Context, id string) (store.SessionGrantRow, error)
}

// GrantAuditLister reads the audit log; *audit.Log implements it.
type GrantAuditLister interface {
	ListOp(ctx context.Context, args ops.AuditListArgs) (ops.AuditListResult, error)
}

// RegisterGrantHistory registers grant_history. workers lists every Worker;
// dec answers whether the caller is an administrator.
func RegisterGrantHistory(r *Registry, st GrantHistoryStore, log GrantAuditLister, workers func(context.Context) ([]GrantWorker, error), dec Decider) error {
	if st == nil || log == nil || workers == nil || dec == nil {
		return errors.New("api: grant_history needs a store, the audit log, the Workers and a decider")
	}
	h := grantHistoryHandlers{st: st, log: log, g: grantHandlers{workers: workers, dec: dec}}
	return HandleTyped(r, ops.OpGrantHistory, h.history, WithResource(h.resource))
}

type grantHistoryHandlers struct {
	st  GrantHistoryStore
	log GrantAuditLister
	g   grantHandlers // isAdmin and the Workers
}

// auditWorkerID is the Worker id of an audit source or target of a grant row
// ("<worker id>:<session or *>"), or "" when the value has another form.
func auditWorkerID(v string) string {
	id, _, ok := strings.Cut(v, ":")
	if !ok || !ops.ValidUUIDv7(id) {
		return ""
	}
	return id
}

// owned returns the ids of the Workers the caller owns.
func (h grantHistoryHandlers) owned(ctx context.Context, p authz.Principal) (map[string]bool, error) {
	ws, err := h.g.workers(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, w := range ws {
		if p.UserID != "" && w.OwnerID == p.UserID {
			out[w.ID] = true
		}
	}
	return out, nil
}

// resource decides grant_history. An administrator is decided on their own
// user resource (they read every grant). Anyone else is decided on a Worker
// of the grant that they own: of the stored grant, or, for a grant that has
// ended, of its audit rows. A caller who owns none is decided on the source
// Worker and refused there; a grant nobody ever recorded is not found.
func (h grantHistoryHandlers) resource(p authz.Principal, args any) (authz.Resource, error) {
	a, ok := args.(*ops.GrantHistoryArgs)
	if !ok {
		return nil, NewError(proto.CodeInternal, "")
	}
	if p.UserID == "" {
		return nil, NewError(proto.CodeForbidden, "not allowed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	if admin, err := h.g.isAdmin(ctx, p); err != nil {
		return nil, err
	} else if admin {
		return authz.UserResource{ID: p.UserID}, nil
	}
	owned, err := h.owned(ctx, p)
	if err != nil {
		return nil, err
	}
	g, err := h.st.SessionGrant(ctx, a.ID)
	switch {
	case err == nil:
		if owned[g.TargetWorkerID] && !owned[g.SourceWorkerID] {
			return authz.WorkerResource{ID: g.TargetWorkerID}, nil
		}
		return authz.WorkerResource{ID: g.SourceWorkerID}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	res, err := h.log.ListOp(ctx, ops.AuditListArgs{GrantID: a.ID, Limit: ops.MaxAuditListLimit})
	if err != nil {
		return nil, err
	}
	first := ""
	for _, e := range res.Events {
		for _, id := range []string{auditWorkerID(e.Source), auditWorkerID(e.Target)} {
			if id == "" {
				continue
			}
			if owned[id] {
				return authz.WorkerResource{ID: id}, nil
			}
			if first == "" {
				first = id
			}
		}
	}
	if first == "" {
		return nil, NewError(proto.CodeNotFound, "no such grant")
	}
	return authz.WorkerResource{ID: first}, nil
}

// history returns one page of the grant's audit rows. A caller who is no
// administrator sees only the rows that name a Worker they own, so a row
// that carries the grant id for another pair of Workers stays hidden. Then
// every endpoint ("<worker id>:<session>") whose Worker the caller may not
// operate (another owner's Worker, a Worker outside the API key's scope, a
// removed Worker) loses its session name, as grant_list masks it.
func (h grantHistoryHandlers) history(ctx context.Context, c *Call, a ops.GrantHistoryArgs) (ops.GrantHistoryResult, error) {
	res, err := h.log.ListOp(ctx, ops.AuditListArgs{GrantID: a.ID, Cursor: a.Cursor, Limit: a.Limit})
	if err != nil {
		return ops.GrantHistoryResult{}, err
	}
	out := ops.GrantHistoryResult(res)
	admin, err := h.g.isAdmin(ctx, c.Principal)
	if err != nil {
		return ops.GrantHistoryResult{}, err
	}
	ws, err := h.g.workers(ctx)
	if err != nil {
		return ops.GrantHistoryResult{}, err
	}
	byID := make(map[string]GrantWorker, len(ws))
	for _, w := range ws {
		byID[w.ID] = w
	}
	owned := map[string]bool{}
	for _, w := range ws {
		if c.Principal.UserID != "" && w.OwnerID == c.Principal.UserID {
			owned[w.ID] = true
		}
	}
	visible := map[string]bool{}
	may := func(workerID string) (bool, error) {
		if workerID == "" {
			return false, nil
		}
		if v, ok := visible[workerID]; ok {
			return v, nil
		}
		v := false
		if _, known := byID[workerID]; known && (admin || owned[workerID]) {
			err := h.g.dec.Decide(ctx, c.Principal, ops.MustActionSet(ops.ActionOperate), authz.WorkerResource{ID: workerID})
			if err == nil {
				v = true
			} else if e := ErrorOf(err); e.Code != proto.CodeForbidden && e.Code != proto.CodeNotFound {
				return false, err
			}
		}
		visible[workerID] = v
		return v, nil
	}
	out.Events = make([]ops.AuditEvent, 0, len(res.Events))
	for _, e := range res.Events {
		if !admin && !owned[auditWorkerID(e.Source)] && !owned[auditWorkerID(e.Target)] && !owned[e.Target] {
			continue
		}
		for _, f := range []*string{&e.Source, &e.Target} {
			id := auditWorkerID(*f)
			if id == "" || strings.HasSuffix(*f, ":*") {
				continue
			}
			ok, err := may(id)
			if err != nil {
				return ops.GrantHistoryResult{}, err
			}
			if !ok {
				*f = id + ":" + maskedSessionLabel(byID[id])
			}
		}
		out.Events = append(out.Events, e)
	}
	return out, nil
}

// maskedSessionLabel is the text that replaces a session name the caller may
// not see (the label of grant_list).
func maskedSessionLabel(w GrantWorker) string {
	if w.Owner == "" {
		return "a session of another user"
	}
	return "a session of " + w.Owner
}
