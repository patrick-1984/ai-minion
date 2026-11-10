package api

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"
	"weak"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// TrustService is what the trust handlers need; *authz.Trust implements it.
type TrustService interface {
	Owner(ctx context.Context, workerID string) (string, error)
	SetUnder(ctx context.Context, receiverID, workerID, sessionName, expectedOwnerID string, recheck func(context.Context) error) error
	Remove(ctx context.Context, receiverID, workerID, sessionName string) (bool, error)
	List(ctx context.Context, receiverID string) ([]authz.Entry, error)
}

var _ TrustService = (*authz.Trust)(nil)

// RegisterTrustHandlers registers trust_set and trust_list (Story 6.6, AD-17,
// R29) on r. az is the Hub's authorizer: trust_set decides its call again
// right before it stores the mark, and the store refuses the mark when the
// Worker changed owner in between (a reassignment deletes all trust, and an
// in-flight request must not bring a mark back; it answers conflict).
//
// Trust belongs to the calling user: the receiver is derived from the
// principal ([authz.TrustReceiver]; the acting admin during act-as) and is
// never an argument. trust_set is decided as read on the Worker or session it
// names, so nobody marks a source trusted that they cannot see; trust_list is
// decided on the caller's own user resource. A Worker identity has no trust
// and is refused. The mark changes only whether a model reads output as
// trusted; it takes no part in any permission decision.
func RegisterTrustHandlers(r *Registry, svc TrustService, az Decider) error {
	if svc == nil || az == nil {
		return errors.New("api: the trust handlers need a service and an authorizer")
	}
	op, ok := r.catalog.Lookup(ops.OpTrustSet)
	if !ok {
		return errors.New("api: trust_set is not an operation of the catalog")
	}
	h := trustHandlers{svc: svc, az: az, op: op, pins: &ownerPins{m: map[weak.Pointer[ops.TrustSetArgs]]string{}}}
	if err := HandleTyped(r, ops.OpTrustSet, h.set, WithResource(h.resource)); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpTrustList, h.list)
}

type trustHandlers struct {
	svc TrustService
	az  Decider
	op  ops.Op
	// pins carries the owner captured by resource to the handler of the same
	// request.
	pins *ownerPins
}

// ownerPins hands a value from the resource derivation of a request to its
// handler, keyed by the request's argument struct: the dispatcher passes the
// same pointer to both. Keys are weak and an entry is dropped when its
// argument struct is collected, so a request that is refused between the two
// (the handler never runs) leaves nothing behind.
type ownerPins struct {
	mu sync.Mutex
	m  map[weak.Pointer[ops.TrustSetArgs]]string
}

func (p *ownerPins) put(a *ops.TrustSetArgs, owner string) {
	key := weak.Make(a)
	p.mu.Lock()
	p.m[key] = owner
	p.mu.Unlock()
	runtime.AddCleanup(a, func(k weak.Pointer[ops.TrustSetArgs]) {
		p.mu.Lock()
		delete(p.m, k)
		p.mu.Unlock()
	}, key)
}

func (p *ownerPins) take(a *ops.TrustSetArgs) (string, bool) {
	key := weak.Make(a)
	p.mu.Lock()
	defer p.mu.Unlock()
	owner, ok := p.m[key]
	delete(p.m, key)
	return owner, ok
}

// trustCaptureTimeout bounds the owner read of [trustHandlers.resource], which
// has no request context.
const trustCaptureTimeout = 5 * time.Second

// resource derives the resource of trust_set as the default does and captures
// the owner of the Worker first: this runs before the dispatcher's first
// authorization decision, so the mark is conditioned on the ownership the
// whole request was decided under (a reassignment between the capture and the
// insert, an admin's request included, ends in conflict). A Worker that does
// not exist yet is left uncaptured: the decision answers not_found.
func (h trustHandlers) resource(p authz.Principal, args any) (authz.Resource, error) {
	res, err := DefaultResource(p, h.op, args)
	if err != nil {
		return nil, err
	}
	a, ok := args.(*ops.TrustSetArgs)
	if !ok || a.Remove {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	owner, err := h.svc.Owner(ctx, a.WorkerID)
	switch {
	case errors.Is(err, authz.ErrNotFound):
		// Left unpinned: the decision answers not_found.
	case err != nil:
		return nil, err
	default:
		h.pins.put(a, owner)
	}
	return res, nil
}

func trustReceiver(c *Call) (string, error) {
	id := authz.TrustReceiver(c.Principal)
	if id == "" {
		return "", NewError(proto.CodeForbidden, "not allowed")
	}
	return id, nil
}

func (h trustHandlers) set(ctx context.Context, c *Call, a ops.TrustSetArgs) (ops.WriteResult, error) {
	id, err := trustReceiver(c)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if a.Remove {
		_, err = h.svc.Remove(ctx, id, a.WorkerID, a.SessionName)
	} else {
		owner, pinned := "", false
		if args, ok := c.Args.(*ops.TrustSetArgs); ok {
			owner, pinned = h.pins.take(args)
		}
		if !pinned {
			// The Worker was unknown when the request began (the decision
			// cannot have allowed it), or the request did not pass through
			// resource: fail closed.
			return ops.WriteResult{}, NewError(proto.CodeNotFound, "not found")
		}
		err = h.svc.SetUnder(ctx, id, a.WorkerID, a.SessionName, owner, func(ctx context.Context) error {
			return h.az.Decide(ctx, c.Principal, c.Op.Actions, c.Resource)
		})
	}
	switch {
	case errors.Is(err, authz.ErrOwnerChanged):
		return ops.WriteResult{}, NewError(proto.CodeConflict, "the Worker changed owner; try again")
	case errors.Is(err, authz.ErrNotFound):
		return ops.WriteResult{}, NewError(proto.CodeNotFound, "not found")
	case err != nil:
		return ops.WriteResult{}, err
	}
	return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
}

func (h trustHandlers) list(ctx context.Context, c *Call, _ ops.TrustListArgs) (ops.TrustListResult, error) {
	id, err := trustReceiver(c)
	if err != nil {
		return ops.TrustListResult{}, err
	}
	entries, err := h.svc.List(ctx, id)
	if err != nil {
		return ops.TrustListResult{}, err
	}
	out := ops.TrustListResult{Trust: make([]ops.TrustInfo, 0, len(entries))}
	for _, e := range entries {
		out.Trust = append(out.Trust, ops.TrustInfo{
			WorkerID: e.WorkerID, SessionName: e.SessionName, CreatedAt: ops.Timestamp(e.CreatedAt),
		})
	}
	return out, nil
}
