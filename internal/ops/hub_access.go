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

// Levels of AccessCell.Granted and InEffect. A name-only master session holds
// control; a session-bound (verified) master holds message, because its
// catalog is sessions_list, session_snapshot and a submitted prompt to an
// agent pane; a direct grant holds any of the three.
const (
	AccessLevelView    = "view"
	AccessLevelMessage = "message"
	AccessLevelControl = "control"
)

// OpAccessExplain answers "why can (or cannot) this session reach that one":
// the layers of one pair, in order, computed by the code of the matrix.
const OpAccessExplain = "access_explain"

// Values of AccessCheck.State.
const (
	AccessCheckPass    = "pass"
	AccessCheckFail    = "fail"
	AccessCheckSkip    = "skip" // not evaluated: an earlier check failed
	AccessCheckUnknown = "unknown"
)

// Values of AccessCheck.ID, in the order access_explain reports them.
const (
	AccessCheckCaller      = "caller"
	AccessCheckGrant       = "grant"
	AccessCheckAuthority   = "authority"
	AccessCheckTime        = "time"
	AccessCheckSharing     = "sharing"
	AccessCheckWorkers     = "workers"
	AccessCheckPreviewOnly = "preview_only"
)

// Values of AccessCell.BlockedBy: the first layer that fails, in this order.
// A master cell starts at owner_rule; a grant cell goes suspended, expired,
// authority and then the Worker layers.
const (
	AccessBlockedSuspended     = "suspended"
	AccessBlockedExpired       = "expired"
	AccessBlockedAuthority     = "authority"
	AccessBlockedOwnerRule     = "owner_rule"
	AccessBlockedWorkerPending = "worker_pending"
	AccessBlockedNotShared     = "not_shared"
	AccessBlockedWorkerOffline = "worker_offline"
	// AccessBlockedOwnerInactive: the owner of the source Worker is not an
	// active account, so the Hub refuses every call of that Worker.
	AccessBlockedOwnerInactive = "owner_inactive"
	// AccessBlockedSourceIncarnation: the grant is bound to one incarnation
	// of the source session; the call path does not read such grants yet.
	AccessBlockedSourceIncarnation = "source_incarnation"
	// AccessBlockedMasterNotAttached: a session-bound master whose terminal
	// holds no lease for the nominated incarnation now.
	AccessBlockedMasterNotAttached = "master_not_attached"
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
	// Masked: the session belongs to another owner; Session is then
	// "a session of <owner>", not its name.
	Masked bool `json:"masked,omitempty"`
}

// AccessTarget is one session a source reaches or is blocked from.
type AccessTarget struct {
	WorkerID   string `json:"worker_id"`
	WorkerName string `json:"worker_name"`
	Owner      string `json:"owner"`
	Online     bool   `json:"online"`
	Session    string `json:"session"`
	// Masked: see AccessSource.Masked.
	Masked bool `json:"masked,omitempty"`
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
	// CommandShell is true when the direct grant of the cell marks the target
	// as a command shell of the source (A116): typing there from the source
	// needs no force on a Worker that knows the flag.
	CommandShell bool `json:"command_shell,omitempty"`
}

// AccessMatrixResult is the matrix at one moment.
type AccessMatrixResult struct {
	AsOf    Timestamp      `json:"as_of"`
	Sources []AccessSource `json:"sources"`
	Targets []AccessTarget `json:"targets"`
	Cells   []AccessCell   `json:"cells"`
}

// AccessExplainArgs name one pair.
type AccessExplainArgs struct {
	SourceWorkerID string `json:"source_worker_id"`
	SourceSession  string `json:"source_session"`
	TargetWorkerID string `json:"target_worker_id"`
	TargetSession  string `json:"target_session"`
}

// AccessCheck is one layer of the answer. Code is the stable reason of the
// state (one of the AccessCode* values or, for a failing layer, the
// AccessBlocked* value the matrix names); Text says the same for a person.
// Fix, when set, is a relative URL of the page where the failing layer can be
// changed.
type AccessCheck struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Code  string `json:"code"`
	Text  string `json:"text"`
	Fix   string `json:"fix,omitempty"`
}

// Values of AccessCheck.Code that are not an AccessBlocked* value.
const (
	AccessCodeCallerClaimed  = "caller_claimed"
	AccessCodeCallerVerified = "caller_verified"
	AccessCodeGrant          = "grant"
	AccessCodeMaster         = "master"
	AccessCodeNoGrant        = "no_grant"
	AccessCodeSameWorker     = "same_worker"
	AccessCodeNoSuchSession  = "no_such_session"
	AccessCodeAuthorityOK    = "authority_ok"
	AccessCodeTimeOK         = "time_ok"
	AccessCodeShared         = "shared"
	AccessCodeWorkersOK      = "workers_ok"
	// AccessCodeMessageAsView: the workers check passes, but the target
	// Worker does not announce prompts to agent panes only, so a Message
	// level works as View.
	AccessCodeMessageAsView = "message_as_view"
	AccessCodeSkipped       = "skipped"
)

// AccessExplainResult is the answer for one pair: what works now, through
// which grant ("master" or a grant id), and every layer in order. After the
// first failing check the rest are "skip".
type AccessExplainResult struct {
	Source   AccessSource  `json:"source"`
	Target   AccessTarget  `json:"target"`
	InEffect string        `json:"in_effect"`
	Via      string        `json:"via"`
	Checks   []AccessCheck `json:"checks"`
}

func init() {
	Register(Op{Name: OpAccessExplain,
		Summary: "Explain why one session can or cannot reach a session on another Worker: every layer in order, and the first one that fails.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[AccessExplainArgs](), Result: reflect.TypeFor[AccessExplainResult]()})
	Register(Op{Name: OpAccessMatrix,
		Summary: "Show which master sessions reach which sessions on other Workers, and the layer that blocks a pair (an admin sees every Worker, others their own).",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[AccessMatrixArgs](), Result: reflect.TypeFor[AccessMatrixResult]()})
}
