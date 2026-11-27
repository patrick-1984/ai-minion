package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// KeepService is the keep list as the executor uses it (session_keep_set and
// the keep member of sessions_list). *keep.Keeper implements it.
type KeepService interface {
	Keep(ctx context.Context, name, instanceID string) error
	Unkeep(ctx context.Context, name string) error
	Kept(ctx context.Context) (map[string]bool, error)
}

// AttachKeep serves session_keep_set through k and reports the keep flag on
// sessions_list. The keep module calls it before the connector starts, so
// the op and proto.FeatureSessionKeepV1 are in the first hello.
func (e *Executor) AttachKeep(k KeepService) {
	if k == nil {
		return
	}
	e.keep.Store(&k)
	e.Handle(ops.OpSessionKeepSet, e.sessionKeepSet)
}

// keepService returns the attached keep list, or nil.
func (e *Executor) keepService() KeepService {
	if p := e.keep.Load(); p != nil {
		return *p
	}
	return nil
}

// keptNames returns the kept session names; nil when there is no keep list or
// it cannot be read (a listing then shows no session as kept).
func (e *Executor) keptNames(ctx context.Context) map[string]bool {
	k := e.keepService()
	if k == nil {
		return nil
	}
	names, err := k.Kept(ctx)
	if err != nil {
		e.log.Warn("cannot read the keep list", "error", err.Error())
		return nil
	}
	return names
}

// keptForState returns the keep lookup of the state stream: the keep list of
// each look, and the last list read when a read fails (so that a failing read
// neither flips the flags the Hub shows nor logs on every poll).
func (e *Executor) keptForState() func(ctx context.Context) map[string]bool {
	var mu sync.Mutex
	var last map[string]bool
	return func(ctx context.Context) map[string]bool {
		k := e.keepService()
		if k == nil {
			return nil
		}
		names, err := k.Kept(ctx)
		mu.Lock()
		defer mu.Unlock()
		if err == nil {
			last = names
		}
		return last
	}
}

// sessionKeepSet adds the target session to the keep list (keep=true: the
// session must exist and be shared, on a Worker that is not preview-only) or
// removes it (keep=false: also for a session that is gone or not shared, and
// on a preview-only Worker, so a kept name can always be forgotten).
func (e *Executor) sessionKeepSet(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.SessionKeepSetArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	k := e.keepService()
	if k == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "the Worker does not implement this operation")
	}
	name := args.Target.SessionName
	// Forgetting needs no share and no write access to the session: a kept
	// name that is no longer shared, or on a Worker that became preview-only,
	// can still be forgotten (else the keeper would keep restoring it).
	var policy func(ctx context.Context) error
	if args.Keep {
		policy = func(ctx context.Context) error { return e.checkPolicy(ctx, args.Target, true) }
	}
	return e.writeChecked(ctx, op, args.WriteArgs, args.Target, policy, func(ctx context.Context) (ops.WriteResult, error) {
		if policy != nil {
			if err := policy(ctx); err != nil {
				return ops.WriteResult{}, err
			}
		}
		if !args.Keep {
			if err := k.Unkeep(ctx, name); err != nil {
				return ops.WriteResult{}, internalErr("update the keep list", err)
			}
			return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
		}
		// The session is the target here, whatever pane the caller named.
		ref, err := e.resolve(ctx, ops.Target{WorkerID: args.Target.WorkerID, SessionName: name})
		if err != nil {
			return ops.WriteResult{}, err
		}
		instance := ""
		if all, err := e.host.ListSessions(ctx); err == nil {
			for _, s := range all {
				if s.Name == name {
					instance = s.Instance
				}
			}
		}
		if err := k.Keep(ctx, name, instance); err != nil {
			if errors.Is(err, store.ErrKeepListFull) {
				return ops.WriteResult{}, apiErr(proto.CodeInvalidArgument, "", "the keep list of this Worker is full; forget a kept session first")
			}
			if errors.Is(err, term.ErrNotFound) {
				return ops.WriteResult{}, hostErr("keep the session", err)
			}
			return ops.WriteResult{}, internalErr("update the keep list", err)
		}
		info := basicInfo(name, ref)
		e.locate(ctx, &info)
		return ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &info}, nil
	})
}
