package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ShareFence is what the share handlers tell the Hub about a Worker's share:
// *workers.EventHub implements it. See [workers.EventHub.BeginShare] and
// [workers.EventHub.ApplyShare].
type ShareFence interface {
	// ApplyShare is for an answer that carries no revision (a Worker that
	// predates revisions): it names the unshared sessions and nothing more.
	ApplyShare(workerID string, unshared []string)
	// BeginShare is called before a share op is dispatched. For a share_set,
	// next is the rule that is asked for; the returned call is settled with
	// exactly one of Refused, Unknown or Applied.
	BeginShare(workerID string, next *ops.ShareRule) workers.ShareCall
	// ConfirmShare asks the Worker, on its current connection, for the rule it
	// has now and settles the restrictions of unknown outcome that this proves
	// settled. It never changes the Worker beyond refusing changes the Hub has
	// stopped waiting for.
	ConfirmShare(ctx context.Context, workerID string) error
}

var _ ShareFence = (*workers.EventHub)(nil)

// reconcileTimeout bounds the follow-up share_get after a share_set whose
// outcome is unknown.
const reconcileTimeout = 10 * time.Second

// RegisterShareOps registers share_get and share_set (Story 4.15, spine AM-1)
// on r: both are routed to the Worker the request names, through rt.
//
// The Hub's privacy state follows the Worker's rule, never the order in which
// answers happen to arrive:
//
//   - Before a share_set is dispatched, what its rule would not share is hidden
//     at once (the stored pipe history, the queued events). That protection is
//     settled only by the outcome of this call: a Worker's answer applies the
//     rule it carries, a refusal that happened before anything could change
//     lifts it, and anything else (the connection lost after the request went
//     out, an answer that cannot be read, a Worker failure that does not say
//     whether the rule changed) leaves it in force, and a share_get is made to
//     find out, until the Worker declares its rule again.
//   - An answer carries the Worker's durable revision of its rule and is
//     applied only if it is not older than the rule the Hub holds and only if
//     it comes from the connection that was current when the call began. A
//     slow share_get cannot undo a newer share_set, also not across a
//     reconnect.
//
// The dispatcher has already decided operate on the Worker named by worker_id
// (the owner and admins; an API key needs that scope; anyone else is refused)
// and audits share_set like a write. The router decides again and sends the
// Worker the effective actions; the Worker's local policy (preview-only, the
// local share lock) still refuses what the Hub allowed.
func RegisterShareOps(r *Registry, rt WorkerRouter, fence ShareFence) error {
	if rt == nil || fence == nil {
		return fmt.Errorf("api: the share handlers need a router and the Hub's session state")
	}
	for _, name := range ops.ShareWorkerOpNames {
		op := name
		if err := r.Handle(op, func(ctx context.Context, c *Call) (any, error) {
			workerID, err := shareWorker(c)
			if err != nil {
				return nil, err
			}
			var next *ops.ShareRule
			switch a := c.Args.(type) {
			case *ops.ShareSetArgs:
				a.WithRevision = true // the Worker adds the revision of its rule to the answer
				rule := ops.ShareRule{Mode: a.Mode, Names: a.Names}
				next = &rule
			case *ops.ShareGetArgs:
				a.WithRevision = true
			}
			call := fence.BeginShare(workerID, next)
			if err := call.Err(); err != nil {
				return nil, err // the restriction could not be recorded: nothing is sent
			}
			if a, ok := c.Args.(*ops.ShareSetArgs); ok {
				a.OpSeq = call.Seq() // the Hub's own order of its changes; a caller cannot set it
			}
			settled := false
			defer func() {
				if !settled { // a panic, or a path that forgot: the outcome is not known
					call.Unknown()
				}
			}()
			res, err := rt.Call(ctx, workers.CallRequest{
				Principal: c.Principal, Op: op, WorkerID: workerID, Args: c.Args,
			})
			if err != nil {
				settled = true
				if changedNothing(err) {
					call.Refused()
				} else {
					call.Unknown()
					go reconcile(fence, workerID)
				}
				return nil, err
			}
			sr, ok := res.(ops.ShareResult)
			if !ok {
				settled = true
				call.Unknown()
				go reconcile(fence, workerID)
				return nil, fmt.Errorf("api: %s: the Worker answered with a %T", op, res)
			}
			sr.Normalize()
			// Both ops report the rule in force, so both bring the Hub's view up
			// to date: a Worker that was changed locally is caught up by
			// share_get too. The answer is applied before it is returned, so
			// nothing the Hub kept shows a session the Worker has just stopped
			// sharing once share_set has returned.
			settled = true
			call.Applied(sr)
			sr.Revision = "" // the revision and the barrier are the Hub's own bookkeeping, not part of what callers get
			sr.OpBarrier = ""
			return sr, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// changedNothing reports whether err of a share op was returned before the
// Worker could have changed anything: the Hub refused the call, or the Worker
// refused it by its policy (the share lock, preview-only, an argument that is
// not valid, another Worker's id), or the Worker was not reached. Every other
// error, and every error that is not a plain protocol error, may have come
// after the change was made and is not treated as "nothing happened".
func changedNothing(err error) bool {
	var unknown *workers.OutcomeUnknownError
	if errors.As(err, &unknown) {
		return false
	}
	var pe *proto.Error
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Code {
	case proto.CodeForbidden, proto.CodeInvalidArgument, proto.CodeUnsupportedOp, proto.CodeNotFound, proto.CodeConflict,
		proto.CodeUnauthenticated, proto.CodeWorkerOffline, proto.CodeRateLimited,
		proto.CodeProtoIncompatible, proto.CodeRevoked:
		return true
	}
	return false
}

// reconcile asks the Worker, on its current connection, for the rule it has
// after a share_set of unknown outcome. The probe raises the Worker's barrier
// past every change the Hub has stopped waiting for, so its answer settles
// those restrictions with proof; when the Worker cannot be asked, the
// restrictions stay (also across a restart) until a later probe can.
func reconcile(fence ShareFence, workerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	_ = fence.ConfirmShare(ctx, workerID)
}

// shareWorker is the Worker the dispatcher decided the call on.
func shareWorker(c *Call) (string, error) {
	switch a := c.Args.(type) {
	case *ops.ShareGetArgs:
		if a.WorkerID != "" {
			return a.WorkerID, nil
		}
	case *ops.ShareSetArgs:
		if a.WorkerID != "" {
			return a.WorkerID, nil
		}
	}
	return "", invalidArg("worker_id", "the Worker that runs this operation must be named by worker_id")
}
