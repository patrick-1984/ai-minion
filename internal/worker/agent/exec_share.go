package agent

import (
	"context"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// share_get and share_set (Story 4.15, spine AM-1, EPICS A-8 and A-9).
//
// Both need the action operate, which the Hub computes for the owner and the
// admins, and which the local policy takes away from a preview-only Worker (see
// [IntersectActions]): a preview-only Worker therefore refuses both with
// forbidden/preview_only. share_set is refused as well, with forbidden, while
// the local share lock is on. The local policy wins whatever the Hub sends (AD-12).
//
// Both are about this Worker only. worker_id is required (the catalog says so)
// and has to be the id the Hub gave this Worker; a Worker that has no such id
// refuses every share operation. A foreign id is never acted on, whether the
// call came from the Hub, the local control socket (the CLI) or a local MCP
// server, and nothing is ever redirected to this Worker instead.
//
// The answer is the rule, its revision and the names of the sessions that the
// rule does not share: names only, no pane, screen, question or turn. A name of
// a session the Hub may not read yet is the one thing share_get reveals; it is
// what the owner needs to choose it. The revision is the durable, monotonic
// revision of the share selection: whoever applies an answer orders it by that
// number, never by the order in which answers arrive.

// requireSelf refuses an operation that does not name this Worker.
func (e *Executor) requireSelf(_ context.Context, named string) error {
	id := e.policy.WorkerID() // the Hub's id for this Worker: from its enrollment and its welcome
	if id == "" {
		return apiErr(proto.CodeForbidden, "", "this Worker has no Hub identity, so it cannot confirm that worker_id is its own; the share operations need an enrolled Worker")
	}
	if named != id {
		return apiErr(proto.CodeForbidden, "", "worker_id names another Worker; a Worker only changes or reports its own sharing")
	}
	return nil
}

// shareResult is the answer of share_get and share_set for the state st: the
// rule (and, when the Hub asked for it, the revision) come from st, the unshared
// names from the sessions the terminal lists now. The rule is the authority; the
// names are for display. When the terminal cannot be listed the result still
// holds the rule and its revision, with no names, and the listing error is
// returned next to it: share_get answers with the error (it has nothing to show
// without the names), share_set does not (see there).
func (e *Executor) shareResult(ctx context.Context, st store.ShareState, withRevision bool) (ops.ShareResult, error) {
	res := ops.ShareResult{
		Rule:   ops.ShareRule{Mode: string(st.Mode)},
		Locked: e.policy.ShareLocked(),
	}
	if withRevision {
		res.Revision = ops.FormatShareRevision(st.Revision)
		e.shareMu.Lock()
		res.OpBarrier = ops.FormatShareRevision(e.shareBarrier)
		e.shareMu.Unlock()
	}
	if st.Mode == store.ShareSelect {
		res.Rule.Names = slices.Clone(st.Sessions)
	}
	all, listErr := e.host.ListSessions(ctx)
	if listErr != nil {
		res.Normalize()
		return res, hostErr("list sessions", listErr)
	}
	for _, s := range all {
		if !st.Includes(s.Name) {
			res.Unshared = append(res.Unshared, s.Name)
		}
	}
	res.Normalize()
	return res, nil
}

// shareGet returns the share rule and the unshared session names.
func (e *Executor) shareGet(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.ShareGetArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.requireSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	// Read the state under the same lock that share_set holds while it saves,
	// so that an answer never mixes the rule from before a change with a
	// revision from after it. The names are listed outside the lock: they do
	// not matter for the order.
	e.shareMu.Lock()
	st, err := e.policy.State(ctx)
	e.shareMu.Unlock()
	if err != nil {
		return nil, internalErr("read the share selection", err)
	}
	res, err := e.shareResult(ctx, st, args.WithRevision)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// shareSet saves the new rule in one transaction and answers the rule that is
// then in force. all and none keep the list of selected names the Worker holds
// (a later select starts from it); select replaces it. The selection is read
// again by every reader (the executor's own reads, the event observer and the
// turn emitter ask the policy each time), so the change is in force for the
// next read of any of them, and a read that was already running when the rule
// changed is discarded, not delivered (see [readStable]).
//
// Once the change is committed, nothing after it can fail the call: the answer
// is built from what the commit returned, and a terminal that cannot be listed
// only leaves the names of the unshared sessions out (ask share_get for them
// when the terminal can be read). The caller can therefore tell a refusal that
// changed nothing (an error before the commit) from a change that was made.
func (e *Executor) shareSet(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.ShareSetArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := e.requireSelf(ctx, args.WorkerID); err != nil {
		return nil, err
	}
	if e.policy.PreviewOnly() {
		return nil, apiErr(proto.CodeForbidden, proto.CodePreviewOnly, "the Worker is preview-only")
	}
	if e.policy.ShareLocked() {
		return nil, apiErr(proto.CodeForbidden, "", "the share selection of this Worker is locked locally (share_lock)")
	}
	committer, ok := e.policy.share.(ShareCommitter)
	if !ok {
		return nil, apiErr(proto.CodeForbidden, "", "this Worker cannot save a share selection")
	}
	e.shareMu.Lock()
	cur, err := e.policy.State(ctx)
	if err != nil {
		e.shareMu.Unlock()
		return nil, internalErr("read the share selection", err)
	}
	next := store.ShareSelection{Sessions: slices.Clone(cur.Sessions)}
	switch args.Mode {
	case ops.ShareModeAll:
		next.Mode = store.ShareAll
	case ops.ShareModeNone:
		next.Mode = store.ShareNone
	default:
		next.Mode, next.Sessions = store.ShareSelect, slices.Clone(args.Names)
	}
	if args.OpSeq != 0 {
		if args.OpSeq <= e.shareBarrier {
			e.shareMu.Unlock()
			// Nothing was changed: the Hub has asked for the rule since this
			// change was dispatched (or sent a newer change), so it no longer
			// waits for this one.
			return nil, apiErr(proto.CodeConflict, "", "this share change was superseded before it was applied; nothing changed")
		}
		e.shareBarrier = args.OpSeq
	}
	saved, err := committer.CommitShareContext(ctx, next)
	e.shareMu.Unlock()
	if err != nil {
		return nil, internalErr("save the share selection", err)
	}
	e.log.Info("share selection changed from the Hub", "mode", string(saved.Mode), "selected", len(saved.Sessions), "revision", saved.Revision)
	// Answer with what was stored, not with what was sent. A failing listing is
	// not an error here: the change is made.
	res, _ := e.shareResult(ctx, saved, args.WithRevision)
	return res, nil
}

// ShareProbe answers the Hub's internal probe (proto.TypeShareProbe): the rule
// in force now, read under the lock that share_set commits under, after the
// barrier was raised to the one the Hub sent. It is not an op: it needs no
// action (it reveals the rule the Worker already declares to the Hub on every
// event stream) and no caller but the Hub's own connection can send it.
func (e *Executor) ShareProbe(ctx context.Context, barrier uint64) (ops.ShareProbe, error) {
	e.shareMu.Lock()
	defer e.shareMu.Unlock()
	if barrier > e.shareBarrier {
		e.shareBarrier = barrier
	}
	st, err := e.policy.State(ctx)
	if err != nil {
		return ops.ShareProbe{}, internalErr("read the share selection", err)
	}
	p := ops.ShareProbe{
		Rule:      ops.ShareRule{Mode: string(st.Mode)},
		Revision:  ops.FormatShareRevision(st.Revision),
		OpBarrier: ops.FormatShareRevision(e.shareBarrier),
	}
	if st.Mode == store.ShareSelect {
		p.Rule.Names = slices.Clone(st.Sessions)
		slices.Sort(p.Rule.Names)
	}
	return p, nil
}

// ShareProber is the executor as the op server uses it for the Hub's share
// probe. *Executor implements it.
type ShareProber interface {
	ShareProbe(ctx context.Context, barrier uint64) (ops.ShareProbe, error)
}

var _ ShareProber = (*Executor)(nil)
