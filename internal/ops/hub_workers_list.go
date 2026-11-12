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
	// Pending is true while the Worker waits for approval by its owner or an
	// admin (F-02): it may be connected, but the Hub routes no operation to it.
	Pending bool `json:"pending,omitempty"`
	// Fingerprint is the fingerprint of the Worker's public key (52 base32
	// characters), the value to compare before approving.
	Fingerprint string `json:"fingerprint,omitempty"`
	// EnrollAddr is the client address the Worker enrolled from.
	EnrollAddr string `json:"enroll_addr,omitempty"`
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
