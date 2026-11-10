package ops

import "reflect"

// OpWorkerRemove is the name of the Hub operation that removes a Worker
// (AD-2, AD-9, AD-17).
const OpWorkerRemove = "worker_remove"

// Warning codes of worker_remove.
const (
	// WarnRevokeOnNextConnect says the Worker was not online, so it learns
	// that it was removed (and wipes its pairing) the next time it connects.
	WarnRevokeOnNextConnect = "revoke_on_next_connect"
	// WarnRevokeNotConfirmed says the Worker was online but did not confirm
	// the signed revoke in time; the Hub ended its connection, and the Worker
	// receives the revoke when it reconnects.
	WarnRevokeNotConfirmed = "revoke_not_confirmed"
)

// guardWorkerExists refuses to remove a Worker that is unknown or already
// removed. It is KEEP: force does not override it.
var guardWorkerExists = Guard{Name: "worker_exists", Class: GuardKeep, Reads: "worker.row"}

// WorkerRemoveArgs are the arguments of worker_remove.
type WorkerRemoveArgs struct {
	WriteArgs
	// WorkerID is the Hub's id of the Worker (UUIDv7).
	WorkerID string `json:"worker_id"`
}

// Validate checks the request id and the Worker id.
func (a WorkerRemoveArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

func init() {
	Register(Op{
		Name: OpWorkerRemove,
		Summary: "Remove a Worker: it becomes a tombstone, its trust and bound API keys are deleted, " +
			"and it is told to wipe its pairing.",
		Scope: ScopeHub,
		// operate on that Worker: the owner and admins pass through resource
		// ownership (AD-17); the global manage action is not needed.
		Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardWorkerExists},
		Args:   reflect.TypeFor[WorkerRemoveArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
