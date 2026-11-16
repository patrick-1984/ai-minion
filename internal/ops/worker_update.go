package ops

import "reflect"

// Names of the Worker update operations (v0.9.0, A10).
const (
	// OpWorkerUpdate asks a Worker to install one exact signed release and
	// restart. The Worker answers at once (accepted or why not) and updates in
	// the background.
	OpWorkerUpdate = "worker_update"
	// OpWorkerUpdateStatus reads the state of the Worker's last update.
	OpWorkerUpdateStatus = "worker_update_status"
)

// Worker update states.
const (
	UpdateStateIdle        = "idle"
	UpdateStateDownloading = "downloading"
	UpdateStateRestarting  = "restarting"
	UpdateStateFailed      = "failed"
	UpdateStateRolledBack  = "rolled_back"
)

// WorkerUpdateArgs are the arguments of worker_update.
type WorkerUpdateArgs struct {
	// WorkerID names the Worker; required through the Hub.
	WorkerID string `json:"worker_id"`
	// Version is the exact release to install, "vX.Y.Z" or "vX.Y.Z-rc.N". A
	// channel ("latest", "0.9") is refused, and so is an older version.
	Version string `json:"version"`
	// Force updates a Worker whose sessions end on a restart (Windows ConPTY)
	// while it has sessions. It never allows a downgrade.
	Force bool `json:"force,omitempty"`
}

// WorkerUpdateResult says whether the Worker started the update.
type WorkerUpdateResult struct {
	Accepted bool   `json:"accepted"`
	From     string `json:"from"`
	To       string `json:"to"`
	// Reason says why not, when Accepted is false.
	Reason string `json:"reason,omitempty"`
}

// WorkerUpdateStatusArgs are the arguments of worker_update_status.
type WorkerUpdateStatusArgs struct {
	WorkerID string `json:"worker_id"`
}

// WorkerUpdateStatus is the state of the Worker's last update attempt.
type WorkerUpdateStatus struct {
	State string `json:"state"`
	// Running is the version the Worker runs now.
	Running string     `json:"running"`
	From    string     `json:"from,omitempty"`
	To      string     `json:"to,omitempty"`
	Error   string     `json:"error,omitempty"`
	At      *Timestamp `json:"at,omitempty"`
}

func init() {
	manage := MustActionSet(ActionManage)
	// worker_update is admin only (manage). It is Mutates, not Write: the
	// result is the acceptance, and a repeat is harmless (a running update
	// refuses a second one).
	Register(Op{
		Name:    OpWorkerUpdate,
		Summary: "Ask a Worker to install an exact signed release and restart (admin only); it answers at once and updates in the background.",
		Scope:   ScopeWorker, Actions: manage, Mutates: true,
		Args: reflect.TypeFor[WorkerUpdateArgs](), Result: reflect.TypeFor[WorkerUpdateResult](),
	})
	Register(Op{
		Name:    OpWorkerUpdateStatus,
		Summary: "Show the state of a Worker's last update: downloading, restarting, failed or rolled back, with the error.",
		Scope:   ScopeWorker, Actions: manage,
		Args: reflect.TypeFor[WorkerUpdateStatusArgs](), Result: reflect.TypeFor[WorkerUpdateStatus](),
	})
}
