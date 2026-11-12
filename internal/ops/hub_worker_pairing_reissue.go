package ops

import "reflect"

// OpWorkerPairingReissue is the name of the Hub operation that re-issues the
// pairing of a Worker whose pairing the Hub committed but the Worker never
// recorded (design hub-identity-v2, section 12.3).
const OpWorkerPairingReissue = "worker_pairing_reissue"

// guardWorkerNeverConnected refuses a Worker that has completed a connect
// handshake, or that was removed. It is KEEP: force does not override it.
var guardWorkerNeverConnected = Guard{Name: "worker_never_connected", Class: GuardKeep, Reads: "worker.first_connected_at"}

// WorkerPairingReissueArgs are the arguments of worker_pairing_reissue.
type WorkerPairingReissueArgs struct {
	WriteArgs
	// WorkerID is the Hub's id of the Worker (UUIDv7).
	WorkerID string `json:"worker_id"`
}

// Validate checks the request id and the Worker id.
func (a WorkerPairingReissueArgs) Validate() error {
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
		Name: OpWorkerPairingReissue,
		Summary: "Re-issue pairing for a Worker that never connected: for 10 minutes the next new pairing code " +
			"of that Worker completes the pairing for the same Worker row.",
		Scope: ScopeHub,
		// operate on that Worker: the owner and admins pass through resource
		// ownership (AD-17).
		Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardWorkerNeverConnected},
		Args:   reflect.TypeFor[WorkerPairingReissueArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
