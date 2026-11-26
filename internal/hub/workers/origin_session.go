package workers

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// originAttribution fills the attribution members of the worker origin o from
// the principal p of the call (never from caller JSON):
//
//   - source_session: the calling session of a session principal
//     ([authz.Principal.SessionName]), with source_proof "claimed". The calling
//     Worker named the session and the Hub does not verify which of its
//     sessions made the call; no principal carries a verified incarnation yet.
//   - grant_id: the id of the direct grant that authorizes the op (accepted
//     actions on res), when the origin is marked Grant and exactly one direct
//     grant of p permits the call ([authz.Authorizer.GrantIDs]). With none or
//     several, the origin names no grant.
//
// The members attribute, they never authorize: the target Worker records them
// in its local audit and decides nothing by them. So a value that the origin
// could not carry (a name that is no session name, an id that is no UUIDv7) is
// left out with a warning and does not refuse the call; only a failing lookup
// is an error. A principal without a calling session leaves o unchanged.
func (rt *Router) originAttribution(ctx context.Context, o *proto.Origin, p authz.Principal, accepted ops.ActionSet, res authz.Resource) error {
	if p.Kind != authz.KindWorker || o.Kind != proto.OriginWorker {
		return nil
	}
	if name := p.SessionName(); name != "" {
		if proto.ValidMasterSessionName(name) {
			o.SourceSession, o.SourceProof = name, proto.OriginProofClaimed
		} else {
			rt.log.Warn("workers: the calling session cannot be named in the origin", "worker_id", p.WorkerID)
		}
	}
	if !o.Grant {
		return nil
	}
	ids, err := rt.az.GrantIDs(ctx, p, accepted, res)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return nil
	}
	if !ops.ValidUUIDv7(ids[0]) {
		rt.log.Warn("workers: the id of a grant cannot be named in the origin", "worker_id", p.WorkerID)
		return nil
	}
	o.GrantID = ids[0]
	return nil
}
