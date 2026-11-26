package workers

import (
	"context"

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

// shellCandidate reports whether a request may carry shell_ok (A116): typed
// input (send_text, send_keys) into one session from a Worker session
// principal, never a prompt-only send (a Message grant or a bound master).
func shellCandidate(op string, req CallRequest) bool {
	return (op == ops.OpSendText || op == ops.OpSendKeys) && req.Session != "" && !req.PromptOnly &&
		req.Principal.Kind == authz.KindWorker
}

// originShell sets shell_ok on the worker origin o from shellID, the command
// shell grant ([authz.Authorizer.CommandShellGrant]) that the admission found
// for the request: then o names that grant as its grant_id. Without one, or
// on an origin that is not marked Grant, shell_ok is cleared and grant_id
// stays as [Router.originGrant] set it. It reports whether shell_ok is set.
func (rt *Router) originShell(o *proto.Origin, p authz.Principal, shellID string) bool {
	o.ShellOK = false
	if shellID == "" || !o.Grant || o.Kind != proto.OriginWorker {
		return false
	}
	if !ops.ValidUUIDv7(shellID) {
		rt.log.Warn("workers: the id of a command shell grant cannot be named in the origin", "worker_id", p.WorkerID)
		return false
	}
	o.ShellOK, o.GrantID = true, shellID
	return true
}

// decideShell reads the command shell grant of a candidate request on res for
// p, sets shell_ok on o and tells report the grant that set it ("" for none).
func (rt *Router) decideShell(ctx context.Context, o *proto.Origin, p authz.Principal, res authz.Resource, report func(string)) error {
	id, err := rt.az.CommandShellGrant(ctx, p, res)
	if err != nil {
		return err
	}
	if !rt.originShell(o, p, id) {
		id = ""
	}
	if report != nil {
		report(id)
	}
	return nil
}
