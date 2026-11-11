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
}

// FromWorker reports whether the origin names a source Worker.
func (o Origin) FromWorker() bool { return o.SourceWorkerID != "" }

// Validate checks that the origin is well formed and consistent for its kind.
// Both the Hub (before it sends) and the Worker (when it receives) call it;
// the error says what is wrong and never echoes a value.
func (o Origin) Validate() error {
	bad := func(msg string) error { return NewError(CodePipeOriginRefused, "malformed origin: "+msg) }
	for _, v := range []string{o.UserID, o.ActorID, o.KeyID, o.KeyBoundWorkerID, o.SourceWorkerID, o.SourceWorkerFP, o.SourceWorkerOwnerID} {
		if len(v) > MaxOriginFieldLen || strings.ContainsAny(v, "\x00\r\n\t ") {
			return bad("a member is too long or holds a control character")
		}
	}
	src := o.SourceWorkerID != ""
	if src != (o.SourceWorkerFP != "") || src != (o.SourceWorkerOwnerID != "") {
		return bad("the source Worker members must be set together")
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
