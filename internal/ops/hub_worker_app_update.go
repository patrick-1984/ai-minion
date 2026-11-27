package ops

import "reflect"

// OpWorkerAppUpdateSet sets how the Hub updates one Worker (v0.24.0, made for
// the Android app, which installs a new APK and restarts every session):
//
//   - auto: the Hub sends the update to its own release by itself once the
//     Worker is online, announces self.update and is older; the Worker installs
//     it when no client is attached and no agent is busy.
//   - manual (the default, no stored choice): only an administrator's Update
//     button or worker_update updates it.
//   - off: the Hub refuses worker_update for this Worker, the button included.
//
// The local veto on the device (Updates: Off in the app, remote_update=off)
// still wins. Administrators only (manage on the Worker), audited.
const OpWorkerAppUpdateSet = "worker_app_update_set"

// Values of WorkerAppUpdateSetArgs.Policy and WorkerInfo.AppUpdate.
const (
	WorkerAppUpdateAuto   = "auto"
	WorkerAppUpdateManual = "manual"
	WorkerAppUpdateOff    = "off"
)

// WorkerAppUpdateSetArgs are the arguments of worker_app_update_set.
type WorkerAppUpdateSetArgs struct {
	WriteArgs
	WorkerID string `json:"worker_id"`
	// Policy is auto, manual or off.
	Policy string `json:"policy"`
}

// Validate checks the request id, the Worker id and the policy.
func (a WorkerAppUpdateSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	switch a.Policy {
	case WorkerAppUpdateAuto, WorkerAppUpdateManual, WorkerAppUpdateOff:
		return nil
	}
	return invalid("policy", "must be %s, %s or %s", WorkerAppUpdateAuto, WorkerAppUpdateManual, WorkerAppUpdateOff)
}

func init() {
	Register(Op{
		Name:    OpWorkerAppUpdateSet,
		Summary: "Set how the Hub updates one Worker: auto (the Hub sends its release when the Worker is older), manual (the Update button only, the default) or off (worker_update is refused). Administrators only.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionManage), Write: true,
		Guards: []Guard{guardWorkerKnown},
		Args:   reflect.TypeFor[WorkerAppUpdateSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
