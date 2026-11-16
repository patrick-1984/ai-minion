package ops

import "reflect"

// Names of the Minion control operations (v0.9.0, A17). A session controls
// sessions on other Workers only when it is a master session on its Worker
// and the target Worker accepts control from that Worker; every other
// session is a slave session. All of them are administrator operations.
const (
	OpControlList      = "control_list"
	OpControlSetWorker = "control_set_worker"
	OpControlSetMaster = "control_set_master"
	OpControlSetAccept = "control_set_accept"
	OpControlCheck     = "control_check"
)

// ControlListArgs are the arguments of control_list (none).
type ControlListArgs struct{}

// ControlMaster is one master session of a Worker.
type ControlMaster struct {
	SessionName string `json:"session_name"`
	// Legacy is true for a migrated v0.8 grant, whose old targets still limit it.
	Legacy bool `json:"legacy,omitempty"`
}

// ControlAccept is one Worker a Worker accepts control from.
type ControlAccept struct {
	SourceWorkerID string `json:"source_worker_id"`
	// Sessions limits the acceptance to some sessions; absent means all
	// shared sessions, an empty list means none (the last allowed session
	// ended). A pointer, so that the two never look the same in JSON.
	Sessions *[]string `json:"sessions,omitempty"`
}

// ControlWorker is the Minion control state of one Worker.
type ControlWorker struct {
	WorkerID    string `json:"worker_id"`
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Online      bool   `json:"online"`
	UserManaged bool   `json:"user_managed"`
	// AcceptAll: the Worker accepts control from every Worker.
	AcceptAll bool            `json:"accept_all"`
	Accepts   []ControlAccept `json:"accepts"`
	Masters   []ControlMaster `json:"masters"`
	// CanBeMaster is false for a Worker that cannot originate control calls
	// (a thin Worker, or one older than 0.8.0).
	CanBeMaster bool `json:"can_be_master"`
}

// ControlListResult lists every Worker's Minion control state.
type ControlListResult struct {
	Workers []ControlWorker `json:"workers"`
}

// ControlSetWorkerArgs change the switches of one Worker; absent members stay.
type ControlSetWorkerArgs struct {
	WriteArgs
	WorkerID    string `json:"worker_id"`
	UserManaged *bool  `json:"user_managed,omitempty"`
	AcceptAll   *bool  `json:"accept_all,omitempty"`
}

// ControlSetMasterArgs nominate (master true) or end a master session.
type ControlSetMasterArgs struct {
	WriteArgs
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
	Master      bool   `json:"master"`
}

// ControlSetAcceptArgs let the target Worker accept (or stop accepting)
// control from the source Worker.
type ControlSetAcceptArgs struct {
	WriteArgs
	WorkerID       string `json:"worker_id"` // the target Worker
	SourceWorkerID string `json:"source_worker_id"`
	Accept         bool   `json:"accept"`
	// Sessions limits the acceptance to these sessions of the target; absent
	// (null) means all its shared sessions. An empty list is refused.
	Sessions *[]string `json:"sessions,omitempty"`
}

// ControlCheckArgs ask who may control one session.
type ControlCheckArgs struct {
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
}

// ControlChain is one master session that reaches the session, with the
// steps of the chain of trust it passed.
type ControlChain struct {
	WorkerID    string `json:"worker_id"`
	Worker      string `json:"worker"`
	SessionName string `json:"session_name"`
}

// ControlCheckResult explains who may control the session.
type ControlCheckResult struct {
	Controllers []ControlChain `json:"controllers"`
	// Note says why nobody can, when Controllers is empty.
	Note string `json:"note,omitempty"`
}

// guardControlOwner is KEEP: a change is stored only while the Workers it
// names still have the owners it was decided for (a removal or reassignment
// in between is never undone).
var guardControlOwner = Guard{Name: "control_owner_unchanged", Class: GuardKeep, Reads: "worker.owner"}

func init() {
	manage := MustActionSet(ActionManage)
	Register(Op{Name: OpControlList, Summary: "List every Worker's Minion control: user-managed, which Workers it accepts control from, its master sessions (admin only).",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[ControlListArgs](), Result: reflect.TypeFor[ControlListResult]()})
	// The setters are writes: the dispatcher audits each one (also when it is
	// refused) with the changed Worker as target, and a request id makes a
	// repeat safe.
	Register(Op{Name: OpControlSetWorker, Summary: "Set whether people and API keys control a Worker (user-managed) and whether it accepts control from all Workers (admin only).",
		Scope: ScopeHub, Actions: manage, Write: true, Guards: []Guard{guardWorkerKnown, guardControlOwner},
		Args: reflect.TypeFor[ControlSetWorkerArgs](), Result: reflect.TypeFor[WriteResult]()})
	Register(Op{Name: OpControlSetMaster, Summary: "Nominate a session as a master session on its Worker, or make it a slave session again (admin only).",
		Scope: ScopeHub, Actions: manage, Write: true, Guards: []Guard{guardWorkerKnown, guardControlOwner},
		Args: reflect.TypeFor[ControlSetMasterArgs](), Result: reflect.TypeFor[WriteResult]()})
	Register(Op{Name: OpControlSetAccept, Summary: "Let a Worker accept control from another Worker's master sessions (optionally only some sessions), or stop it (admin only).",
		Scope: ScopeHub, Actions: manage, Write: true, Guards: []Guard{guardWorkerKnown, guardControlOwner},
		Args: reflect.TypeFor[ControlSetAcceptArgs](), Result: reflect.TypeFor[WriteResult]()})
	Register(Op{Name: OpControlCheck, Summary: "Explain which master sessions may control a session, or why none can (admin only).",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[ControlCheckArgs](), Result: reflect.TypeFor[ControlCheckResult]()})
}
