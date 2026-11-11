package ops

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"
)

// Names of the API key operations (AD-2, AD-11).
const (
	OpAPIKeyCreate = "apikey_create"
	OpAPIKeyList   = "apikey_list"
	OpAPIKeyRevoke = "apikey_revoke"
)

// Kinds of resource an API key can be limited to (AD-11).
const (
	// KeyResourceEverything covers every resource of the Hub. Only an admin
	// may grant it.
	KeyResourceEverything = "everything"
	// KeyResourceUser covers a user account and every Worker and session
	// that user owns.
	KeyResourceUser = "user"
	// KeyResourceWorker covers one Worker and all its sessions.
	KeyResourceWorker = "worker"
	// KeyResourceSession covers one session of one Worker.
	KeyResourceSession = "session"
)

// Limits of the API key operations.
const (
	// MaxAPIKeyNameLen caps the display name of a key in bytes.
	MaxAPIKeyNameLen = 64
	// MaxKeyResources caps the resources of one key.
	MaxKeyResources = 64
)

// KeyResource names one resource of a key scope. Which members are set depends
// on Kind: everything none, user user_id, worker worker_id, session worker_id
// and session_name.
type KeyResource struct {
	Kind        string `json:"kind"`
	UserID      string `json:"user_id,omitempty"`
	WorkerID    string `json:"worker_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

// Validate checks that the members fit the kind: exactly the members of the
// kind are set, ids are UUIDv7 and the session name is a valid tmux name.
func (r KeyResource) Validate() error {
	var want [3]bool // user_id, worker_id, session_name
	switch r.Kind {
	case KeyResourceEverything:
	case KeyResourceUser:
		want[0] = true
	case KeyResourceWorker:
		want[1] = true
	case KeyResourceSession:
		want[1], want[2] = true, true
	default:
		return invalid("resources", "kind must be everything, user, worker or session")
	}
	for i, f := range []struct{ name, v string }{{"user_id", r.UserID}, {"worker_id", r.WorkerID}, {"session_name", r.SessionName}} {
		if want[i] && f.v == "" {
			return invalid("resources", "a %s resource needs %s", r.Kind, f.name)
		}
		if !want[i] && f.v != "" {
			return invalid("resources", "a %s resource takes no %s", r.Kind, f.name)
		}
	}
	if r.UserID != "" && !ValidUUIDv7(r.UserID) {
		return invalid("resources", "user_id must be a UUIDv7")
	}
	if r.WorkerID != "" && !ValidUUIDv7(r.WorkerID) {
		return invalid("resources", "worker_id must be a UUIDv7")
	}
	if r.SessionName != "" && !validSessionName(r.SessionName) {
		return invalid("resources", "session_name is not a valid session name")
	}
	return nil
}

// DefaultAPIKeyLifetime is how long a key lives when the creator names no
// expiry and does not ask for a key that never expires (F-07).
const DefaultAPIKeyLifetime = 90 * 24 * time.Hour

// APIKeyCreateArgs are the arguments of apikey_create. The key belongs to the
// caller; the resources and actions limit what it may do (AD-11).
type APIKeyCreateArgs struct {
	// Name is a display name, 1 to MaxAPIKeyNameLen bytes.
	Name string `json:"name"`
	// ExpiresAt ends the key; it must lie in the future. When omitted (and
	// Never is false) the key expires after DefaultAPIKeyLifetime.
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
	// Never asks for a key without an expiry. Only an admin may; it cannot be
	// combined with ExpiresAt.
	Never bool `json:"never,omitempty"`
	// Resources limits the key. A user can grant only resources they own;
	// "everything" is for admins.
	Resources []KeyResource `json:"resources"`
	// Actions are the actions the key holds on every listed resource. manage
	// is for admins.
	Actions []Action `json:"actions"`
	// BoundWorkerID, when set, binds the key to a Worker identity: it is
	// accepted only on that Worker's call streams and refused on REST.
	BoundWorkerID string `json:"bound_worker_id,omitempty"`
}

// Validate checks the shape of the arguments. Whether the caller may grant the
// resources and actions is decided on the Hub.
func (a APIKeyCreateArgs) Validate() error {
	if a.Name == "" || len(a.Name) > MaxAPIKeyNameLen || !utf8.ValidString(a.Name) || !validDisplayName(a.Name) {
		return invalid("name", "must be 1 to %d characters without control characters or surrounding space", MaxAPIKeyNameLen)
	}
	if a.Never && a.ExpiresAt != nil {
		return invalid("never", "cannot be combined with expires_at")
	}
	if len(a.Resources) == 0 || len(a.Resources) > MaxKeyResources {
		return invalid("resources", "must list 1 to %d resources", MaxKeyResources)
	}
	for _, r := range a.Resources {
		if err := r.Validate(); err != nil {
			return err
		}
		if r.Kind == KeyResourceEverything && len(a.Resources) > 1 {
			return invalid("resources", "everything cannot be combined with other resources")
		}
	}
	if len(a.Actions) == 0 || len(a.Actions) > 4 {
		return invalid("actions", "must list 1 to 4 actions")
	}
	for _, act := range a.Actions {
		if !act.Valid() {
			return invalid("actions", "unknown action %q", string(act))
		}
	}
	if a.BoundWorkerID != "" && !ValidUUIDv7(a.BoundWorkerID) {
		return invalid("bound_worker_id", "must be a UUIDv7")
	}
	return nil
}

func validDisplayName(s string) bool {
	for _, r := range s {
		if isControl(r) {
			return false
		}
	}
	return s[0] != ' ' && s[len(s)-1] != ' '
}

// APIKeyInfo describes a stored key. It never holds the key itself, only the
// 8-character hint.
type APIKeyInfo struct {
	ID            string        `json:"id"` // UUIDv7
	Name          string        `json:"name"`
	Hint          string        `json:"hint"` // first 8 characters after "mn_"
	OwnerID       string        `json:"owner_id"`
	Resources     []KeyResource `json:"resources"`
	Actions       []Action      `json:"actions"`
	BoundWorkerID string        `json:"bound_worker_id,omitempty"`
	CreatedAt     Timestamp     `json:"created_at"`
	ExpiresAt     *Timestamp    `json:"expires_at,omitempty"`
	RevokedAt     *Timestamp    `json:"revoked_at,omitempty"`
	// LastUsedAt is when the key last authenticated a request, at most a minute
	// late; absent while it never has.
	LastUsedAt *Timestamp `json:"last_used_at,omitempty"`
}

// APIKeyCreateResult is the result of apikey_create. Key is the only time the
// key is ever shown.
type APIKeyCreateResult struct {
	// Key is the new key, "mn_" and 52 base32 characters. It is a secret.
	Key  string     `json:"key"`
	Info APIKeyInfo `json:"info"`
}

// String redacts the key, so that a stray %v or %s never prints it.
func (r APIKeyCreateResult) String() string {
	return fmt.Sprintf("apikey_create result{id=%s}", r.Info.ID)
}

// GoString redacts the key for %#v as well.
func (r APIKeyCreateResult) GoString() string { return r.String() }

// APIKeyListArgs are the arguments of apikey_list. Without arguments it lists
// the caller's keys.
type APIKeyListArgs struct {
	// OwnerID lists the keys of another user (admin only).
	OwnerID string `json:"owner_id,omitempty"`
	// All lists the keys of every user (admin only). It excludes OwnerID.
	All bool `json:"all,omitempty"`
}

// Validate checks the owner id and that the two filters are not combined.
func (a APIKeyListArgs) Validate() error {
	if a.OwnerID != "" && !ValidUUIDv7(a.OwnerID) {
		return invalid("owner_id", "must be a UUIDv7")
	}
	if a.All && a.OwnerID != "" {
		return invalid("all", "cannot be combined with owner_id")
	}
	return nil
}

// APIKeyListResult is the result of apikey_list, newest key first.
type APIKeyListResult struct {
	Keys []APIKeyInfo `json:"keys"`
}

// MarshalJSON writes keys as an empty array instead of null.
func (r APIKeyListResult) MarshalJSON() ([]byte, error) {
	type plain APIKeyListResult
	if r.Keys == nil {
		r.Keys = []APIKeyInfo{}
	}
	return json.Marshal(plain(r))
}

// APIKeyRevokeArgs are the arguments of apikey_revoke.
type APIKeyRevokeArgs struct {
	WriteArgs
	// ID is the id of the key, as listed.
	ID string `json:"id"`
}

// Validate checks the request id and the key id.
func (a APIKeyRevokeArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.ID) {
		return invalid("id", "must be a UUIDv7")
	}
	return nil
}

// guardKeyExists refuses to revoke a key that does not exist or is not the
// caller's (admins see all keys). It is KEEP: force never overrides a
// permission.
var guardKeyExists = Guard{Name: "key_exists", Class: GuardKeep, Reads: "api_keys.id"}

func init() {
	// The key operations run on the caller's own user resource: every user
	// holds operate and read on it, and admins on every user. Deciding who
	// may touch another user's keys is done by the Hub, through the same
	// authorisation function (AD-10).
	read, operate := MustActionSet(ActionRead), MustActionSet(ActionOperate)

	// apikey_create returns the secret once, so it cannot return a
	// WriteResult and is not declared a write; it is POST in practice (its
	// resources cannot be given in a query string) and audited by the Hub.
	Register(Op{
		Name: OpAPIKeyCreate, Summary: "Create an API key limited to chosen users, Workers or sessions and actions; the key is shown once.",
		Scope: ScopeHub, Actions: operate,
		Args: reflect.TypeFor[APIKeyCreateArgs](), Result: reflect.TypeFor[APIKeyCreateResult](),
	})
	Register(Op{
		Name: OpAPIKeyList, Summary: "List API keys with their hint, scope and expiry; never the key itself.",
		Scope: ScopeHub, Actions: read,
		Args: reflect.TypeFor[APIKeyListArgs](), Result: reflect.TypeFor[APIKeyListResult](),
	})
	Register(Op{
		Name: OpAPIKeyRevoke, Summary: "Revoke an API key at once.",
		Scope: ScopeHub, Actions: operate, Write: true,
		Guards: []Guard{guardKeyExists},
		Args:   reflect.TypeFor[APIKeyRevokeArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
