package proto

// The origin of an op request (AD-12, F-01).
//
// The Hub derives an Origin on the server side for every op.request it sends,
// from the authenticated principal of the call and never from anything the
// caller wrote. A Worker that receives a message-class op (pipe_send,
// send_text, send_keys, answer, set_task) refuses it when the origin is
// missing or malformed, and applies its local pipe_accept_from policy to the
// source Worker the origin names. The origin is the claim of an honest Hub; a
// compromised Hub can write any origin (docs/security.md).

import "strings"

// OriginKind says what kind of principal made the call.
type OriginKind string

const (
	// OriginUser is a user: a browser session, or an admin acting as a user.
	OriginUser OriginKind = "user"
	// OriginAPIKey is an API key. A key bound to a Worker makes that Worker
	// the source Worker of the call.
	OriginAPIKey OriginKind = "apikey"
	// OriginWorker is a Worker identity on its own call stream.
	OriginWorker OriginKind = "worker"
)

// MaxOriginFieldLen bounds every string member of an origin.
const MaxOriginFieldLen = 128

// The values of Origin.SourceProof: how the Hub knows the calling session.
const (
	// OriginProofClaimed means the calling Worker named the session; the Hub
	// did not verify which of its sessions made the call.
	OriginProofClaimed = "claimed"
	// OriginProofVerified means the Hub verified the calling session (a bound
	// master incarnation).
	OriginProofVerified = "verified"
)

// Origin is the structured, server-derived provenance of an op request. The
// zero value is invalid.
type Origin struct {
	Kind OriginKind `json:"kind"`
	// UserID is the user whose permissions applied: the session user, the key
	// owner or the act-as target. Empty for a Worker identity.
	UserID string `json:"user_id,omitempty"`
	// ActorID is the admin who acted as UserID (act-as only).
	ActorID string `json:"actor_id,omitempty"`
	// KeyID is the stored id of the API key (never the token).
	KeyID string `json:"key_id,omitempty"`
	// KeyBoundWorkerID is the Worker the API key is bound to, if any.
	KeyBoundWorkerID string `json:"key_bound_worker_id,omitempty"`
	// SourceWorkerID, SourceWorkerFP and SourceWorkerOwnerID name the Worker
	// the call came from: the Worker identity itself, or the Worker a bound
	// key is bound to. SourceWorkerOwnerID is that Worker's owner, whoever the
	// key's owner is. They are set together or not at all.
	SourceWorkerID      string `json:"source_worker_id,omitempty"`
	SourceWorkerFP      string `json:"source_worker_fp,omitempty"`
	SourceWorkerOwnerID string `json:"source_worker_owner_id,omitempty"`
	// Grant is set by the Hub only when a Hub session-control grant authorized
	// this op on this target. A receiving Worker then accepts the source Worker
	// without its pipe_accept_from policy, as it accepts a user. It needs a
	// worker origin with a source Worker.
	Grant bool `json:"grant,omitempty"`
	// SourceSession, SourceProof and GrantID attribute the call: the calling
	// session on the source Worker, how the Hub knows it (OriginProofClaimed or
	// OriginProofVerified) and the id of the Hub grant that authorized this op
	// on this target (UUIDv7 text). All are optional and need a worker origin
	// with a source Worker. They attribute and never authorize: a Worker
	// records them in its local audit and no Worker decision reads them. A
	// Worker that does not know them ignores them.
	SourceSession string `json:"source_session,omitempty"`
	SourceProof   string `json:"source_proof,omitempty"`
	GrantID       string `json:"grant_id,omitempty"`
	// ShellOK is set by the Hub only on a send_text or send_keys that a
	// Control grant marked "command shell" authorized for this source session
	// on this very target session (A116); GrantID then names that grant. A
	// receiving Worker skips the shell_pane guard for it, and only that guard:
	// typing into that shell is a normal write, not a forced one. Like Grant
	// it is the claim of an honest Hub, and a Worker reads it only on a
	// request that came over its Hub connection. A Worker that does not know
	// it ignores it (and refuses with shell_pane unless forced, as before).
	ShellOK bool `json:"shell_ok,omitempty"`
}

// FromWorker reports whether the origin names a source Worker.
func (o Origin) FromWorker() bool { return o.SourceWorkerID != "" }

// Validate checks that the origin is well formed and consistent for its kind.
// Both the Hub (before it sends) and the Worker (when it receives) call it;
// the error says what is wrong and never echoes a value.
func (o Origin) Validate() error {
	bad := func(msg string) error { return NewError(CodePipeOriginRefused, "malformed origin: "+msg) }
	// SourceSession is not in this list: a session name may hold inner spaces
	// and has its own rule below.
	for _, v := range []string{o.UserID, o.ActorID, o.KeyID, o.KeyBoundWorkerID, o.SourceWorkerID, o.SourceWorkerFP, o.SourceWorkerOwnerID, o.SourceProof, o.GrantID} {
		if len(v) > MaxOriginFieldLen || strings.ContainsAny(v, "\x00\r\n\t ") {
			return bad("a member is too long or holds a control character")
		}
	}
	src := o.SourceWorkerID != ""
	if src != (o.SourceWorkerFP != "") || src != (o.SourceWorkerOwnerID != "") {
		return bad("the source Worker members must be set together")
	}
	if o.Grant && (o.Kind != OriginWorker || !src) {
		return bad("a grant needs a worker origin with the source Worker")
	}
	if (o.SourceSession != "" || o.SourceProof != "" || o.GrantID != "") && (o.Kind != OriginWorker || !src) {
		return bad("the source session, its proof and the grant id need a worker origin with the source Worker")
	}
	if o.SourceSession != "" && !ValidMasterSessionName(o.SourceSession) {
		return bad("source_session is not a session name")
	}
	switch o.SourceProof {
	case "":
		if o.SourceSession != "" {
			return bad("source_session and source_proof must be set together")
		}
	case OriginProofClaimed, OriginProofVerified:
		if o.SourceSession == "" {
			return bad("source_session and source_proof must be set together")
		}
	default:
		return bad("unknown source_proof")
	}
	if o.GrantID != "" && (!o.Grant || !validOriginUUIDv7(o.GrantID)) {
		return bad("grant_id needs grant and must be a UUIDv7")
	}
	if o.ShellOK && o.GrantID == "" {
		return bad("shell_ok needs the grant id of the command shell grant")
	}
	switch o.Kind {
	case OriginUser:
		if o.UserID == "" {
			return bad("a user origin needs user_id")
		}
		if o.KeyID != "" || o.KeyBoundWorkerID != "" || src {
			return bad("a user origin names no key or source Worker")
		}
	case OriginAPIKey:
		if o.UserID == "" || o.KeyID == "" {
			return bad("an apikey origin needs user_id and key_id")
		}
		if o.ActorID != "" {
			return bad("an apikey origin has no actor")
		}
		if o.KeyBoundWorkerID != o.SourceWorkerID {
			return bad("the source Worker of a bound key must be the bound Worker")
		}
	case OriginWorker:
		if !src {
			return bad("a worker origin needs the source Worker")
		}
		if o.UserID != "" || o.ActorID != "" || o.KeyID != "" || o.KeyBoundWorkerID != "" {
			return bad("a worker origin names no user or key")
		}
	default:
		return bad("unknown kind")
	}
	return nil
}

// validOriginUUIDv7 reports whether s is a canonical lowercase UUID text with
// version 7 and the RFC 9562 variant. It is the rule of ops.ValidUUIDv7, which
// this package cannot import (ops imports proto); a test keeps them equal.
func validOriginUUIDv7(s string) bool {
	if len(s) != 36 || s[14] != '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return strings.IndexByte("89ab", s[19]) >= 0
}
