package ops

import "reflect"

// OpAccessMatrix is the read-only "who reaches what" matrix: every master
// session and the sessions on other Workers it reaches, with the layer that
// blocks a pair an acceptance covers. Nothing decides from it; it is a
// snapshot with an "as of" time.
const OpAccessMatrix = "access_matrix"

// Values of AccessSource.Proof: how the Hub knows the source session.
const (
	AccessProofClaimed  = "claimed"  // a name the Worker reports
	AccessProofVerified = "verified" // a session-bound (attested) master
)

// AccessViaMaster is AccessCell.Via for reach that comes from a master
// nomination. A direct grant will name its grant id there.
const AccessViaMaster = "master"

// AccessLevelControl is the only level a master session holds.
const AccessLevelControl = "control"

// Values of AccessCell.BlockedBy: the first layer that fails, in this order.
const (
	AccessBlockedOwnerRule     = "owner_rule"
	AccessBlockedWorkerPending = "worker_pending"
	AccessBlockedNotShared     = "not_shared"
	AccessBlockedWorkerOffline = "worker_offline"
	// AccessBlockedPreviewOnlyUnknown is reserved: the Hub does not know a
	// Worker's preview-only policy yet.
	AccessBlockedPreviewOnlyUnknown = "preview_only_unknown"
)

// AccessMatrixArgs are the arguments of access_matrix.
type AccessMatrixArgs struct {
	// WorkerID keeps only the pairs with this Worker at either end.
	WorkerID string `json:"worker_id,omitempty"`
}

// AccessSource is one session that reaches (or would reach) other Workers.
type AccessSource struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Owner      string `json:"owner"`
	Session    string `json:"session"`
	Proof      string `json:"proof"`
	Master     bool   `json:"master"`
}

// AccessTarget is one session a source reaches or is blocked from.
type AccessTarget struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Owner      string `json:"owner"`
	Online     bool   `json:"online"`
	Session    string `json:"session"`
}

// AccessCell is one source-target pair an acceptance covers. Source and
// Target index AccessMatrixResult.Sources and Targets; a pair without a cell
// has no access.
type AccessCell struct {
	Source  int    `json:"source"`
	Target  int    `json:"target"`
	Granted string `json:"granted"`
	// InEffect is the level that works now; empty while a layer blocks it.
	InEffect string `json:"in_effect"`
	// Via is "master" or, later, the id of a direct grant.
	Via       string `json:"via"`
	BlockedBy string `json:"blocked_by"`
}

// AccessMatrixResult is the matrix at one moment.
type AccessMatrixResult struct {
	AsOf    Timestamp      `json:"as_of"`
	Sources []AccessSource `json:"sources"`
	Targets []AccessTarget `json:"targets"`
	Cells   []AccessCell   `json:"cells"`
}

func init() {
	Register(Op{Name: OpAccessMatrix,
		Summary: "Show which master sessions reach which sessions on other Workers, and the layer that blocks a pair (an admin sees every Worker, others their own).",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[AccessMatrixArgs](), Result: reflect.TypeFor[AccessMatrixResult]()})
}
