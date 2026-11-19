package ops

import "reflect"

// OpWorkersList is the name of the Hub operation that lists Workers.
const OpWorkersList = "workers_list"

// Worker kinds as reported by workers_list (AD-17).
const (
	// WorkerKindPrivate is a Worker enrolled by a user for their own space;
	// it is never reassignable.
	WorkerKindPrivate = "private"
	// WorkerKindInfrastructure is a Worker enrolled with an admin
	// infrastructure key; only an admin can reassign it.
	WorkerKindInfrastructure = "infrastructure"
)

// WarnNoAnswer is the warning code of a write whose result is unknown: the
// Worker may or may not have applied it (the stream was lost, or the Worker
// stayed silent). The Hub never retries such a write.
const WarnNoAnswer = "no_answer"

// WorkersListArgs are the arguments of workers_list.
type WorkersListArgs struct {
	// All lists every Worker the caller may read instead of only the
	// caller's own. It only widens the list for an admin (the "mine" filter
	// off, R47): everyone else may read just their own Workers, so All
	// changes nothing for them.
	All bool `json:"all,omitempty"`
}

// WorkerInfo describes one Worker in a workers_list result.
type WorkerInfo struct {
	// ID is the Worker's UUIDv7, the canonical form of a target (AD-18).
	ID string `json:"id"`
	// Name is the Worker's name, unique per owner (AD-17).
	Name string `json:"name"`
	// OwnerID is the owning user's id.
	OwnerID string `json:"owner_id"`
	// Owner is the owning user's name, or the id when no name is known.
	Owner string `json:"owner"`
	// Kind is WorkerKindPrivate or WorkerKindInfrastructure.
	Kind string `json:"kind"`
	// Online is true while the Worker is connected to the Hub.
	Online bool `json:"online"`
	// ProtoVersion is the protocol version negotiated for the current
	// connection ("major.minor"); empty when the Worker is offline.
	ProtoVersion string `json:"proto_version,omitempty"`
	// ConnectedAt is when the current connection was established; absent
	// when the Worker is offline.
	ConnectedAt *Timestamp `json:"connected_at,omitempty"`
	// Ops are the operations the Worker announced for the current
	// connection, sorted; empty when the Worker is offline.
	Ops []string `json:"ops,omitempty"`
	// Features are the safeguards the Worker announced in the signed hello of
	// the current connection (for example "answer.reviewed"), sorted. A Worker
	// that lacks one is refused the requests that rely on it; the list shows
	// why, and that the Worker needs an update. Empty when offline.
	Features []string `json:"features,omitempty"`
	// Build, OS and Arch are what the connected Worker reported (v0.9.0 and
	// later): its release ("v0.9.0" or "dev") and platform. Display only.
	Build string `json:"build,omitempty"`
	OS    string `json:"os,omitempty"`
	Arch  string `json:"arch,omitempty"`
	// UpdateBlock is why the connected Worker cannot update itself from the
	// Hub; empty when it can or when it did not say (older Workers).
	UpdateBlock string `json:"update_block,omitempty"`
	// SessionHost is what hosts the connected Worker's sessions: "tmux",
	// "psmux" or "conpty" (v0.10.0); empty when the Worker did not say
	// (older Workers) or is offline.
	SessionHost string `json:"session_host,omitempty"`
	// SessionsHidden is true when a new session on the Worker would be hidden
	// from the person (they could not see or join it from their own
	// terminal); SessionHostReason says why the host is visible or hidden.
	SessionsHidden    bool   `json:"sessions_hidden,omitempty"`
	SessionHostReason string `json:"session_host_reason,omitempty"`
	// NewSessionRefused is set when session_create on this Worker would be
	// refused for the caller because the new session would be hidden and the
	// hidden_sessions policy does not allow it: the message, with the fix.
	NewSessionRefused string `json:"new_session_refused,omitempty"`
	// SessionsSynced is true once the connected Worker sent its first session
	// list on this connection: before that an empty list means "not known
	// yet", not "no sessions".
	SessionsSynced bool `json:"sessions_synced,omitempty"`
	// SessionsListError is why the Worker could not list its sessions when it
	// connected (short, no paths); empty when it could or did not say.
	SessionsListError string `json:"sessions_list_error,omitempty"`
	// DebugRemote is true when the connected Worker allows remote debug
	// (worker_debug, worker_logs; allow_debug_remote on the Worker).
	DebugRemote bool `json:"debug_remote,omitempty"`
	// TrafficPayload is true when the connected Worker records the frame
	// bodies of its connection to the Hub in its own log (A49 T19); the Hub
	// shows a notice on its sessions.
	TrafficPayload bool `json:"traffic_payload,omitempty"`
	// Pending is true while the Worker waits for approval by its owner or an
	// admin (F-02): it may be connected, but the Hub routes no operation to it.
	Pending bool `json:"pending,omitempty"`
	// Fingerprint is the fingerprint of the Worker's public key (52 base32
	// characters), the value to compare before approving.
	Fingerprint string `json:"fingerprint,omitempty"`
	// FingerprintWords is the same fingerprint as six words of the BIP39
	// English word list joined with "-", the form people compare.
	FingerprintWords string `json:"fingerprint_words,omitempty"`
	// EnrollAddr is the client address the Worker enrolled from.
	EnrollAddr string `json:"enroll_addr,omitempty"`
	// HubCheck is how the connected Worker checked this Hub, as it states in
	// its signed hello (v0.11.0): "none" (it did not check the Hub's
	// fingerprint), "fingerprint" or "invite". Empty when unknown (offline,
	// or an older Worker).
	HubCheck string `json:"hub_check,omitempty"`
	// EnrolledAt is when the Worker enrolled.
	EnrolledAt *Timestamp `json:"enrolled_at,omitempty"`
	// NeverConnected is true while the Worker has never completed a connect
	// handshake with the Hub; only such a Worker is eligible for
	// worker_pairing_reissue.
	NeverConnected bool `json:"never_connected,omitempty"`
}

// WorkersListResult is the result of workers_list, ordered by Worker name,
// then owner, then id.
type WorkersListResult struct {
	Workers []WorkerInfo `json:"workers"`
}

func init() {
	Register(Op{
		Name:    OpWorkersList,
		Summary: "List the Workers you may see with their owner, kind and online state; an admin sees only their own unless all is true.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[WorkersListArgs](), Result: reflect.TypeFor[WorkersListResult](),
	})
}
