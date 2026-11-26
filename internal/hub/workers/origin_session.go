package workers

import (
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// originSource is the calling session of a request as its admission path
// established it, and how: proof is proto.OriginProofVerified for a master
// call under a bound lease (the Hub verified the incarnation that holds the
// lease) and proto.OriginProofClaimed for a call that names its session on a
// call stream (the calling Worker named it; legacy session control and
// name-only session grants). The zero value names no session.
type originSource struct {
	session string
	proof   string
}

// claimedSource is the source of a call stream request that names its
// session: the calling Worker named it and the Hub did not verify it.
func claimedSource(session string) originSource {
	if session == "" {
		return originSource{}
	}
	return originSource{session: session, proof: proto.OriginProofClaimed}
}

// verifiedSource is the source of a master call: the session of the bound
// lease the Hub verified.
func verifiedSource(session string) originSource {
	if session == "" {
		return originSource{}
	}
	return originSource{session: session, proof: proto.OriginProofVerified}
}

// originAttribution fills the attribution members of the worker origin o of a
// call of the principal p (never from caller JSON):
//
//   - source_session and source_proof: src, the calling session and its proof
//     as the admission path established them, whatever authorizes the call
//     (grant scope, a direct grant or the own-Worker rule). Without src, the
//     calling session of a session principal ([authz.Principal.SessionName]),
//     claimed.
//   - grant_id: the one direct grant among grantIDs, the grants that the
//     admission found to authorize the op by themselves
//     ([authz.Authorizer.GrantIDs]), when the origin is marked Grant. With
//     none or several, the origin names no grant. No lookup is made for it.
//
// The members attribute, they never authorize: the target Worker records them
// in its local audit and decides nothing by them. So a value that the origin
// could not carry (a name that is no session name, an id that is no UUIDv7) is
// left out with a warning and never refuses the call.
func (rt *Router) originAttribution(o *proto.Origin, p authz.Principal, src originSource, grantIDs []string) {
	if p.Kind != authz.KindWorker || o.Kind != proto.OriginWorker {
		return
	}
	if src.session == "" {
		src = claimedSource(p.SessionName())
	}
	if src.session != "" {
		if src.proof != proto.OriginProofVerified {
			src.proof = proto.OriginProofClaimed
		}
		if proto.ValidMasterSessionName(src.session) {
			o.SourceSession, o.SourceProof = src.session, src.proof
		} else {
			rt.log.Warn("workers: the calling session cannot be named in the origin", "worker_id", p.WorkerID)
		}
	}
	rt.originGrant(o, p, grantIDs)
}

// originGrant sets the grant_id of the origin o from grantIDs, the direct
// grants that authorize the request by themselves as the admission read them:
// the one id when there is exactly one and o is marked Grant, else none.
func (rt *Router) originGrant(o *proto.Origin, p authz.Principal, grantIDs []string) {
	o.GrantID = ""
	if !o.Grant || len(grantIDs) != 1 {
		return
	}
	if !ops.ValidUUIDv7(grantIDs[0]) {
		rt.log.Warn("workers: the id of a grant cannot be named in the origin", "worker_id", p.WorkerID)
		return
	}
	o.GrantID = grantIDs[0]
}
