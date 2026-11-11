package ops

import "reflect"

// OpWorkerApprove is the name of the Hub operation that approves a pending
// Worker (F-02, AD-9, AD-17).
const OpWorkerApprove = "worker_approve"

// WorkerApproveArgs are the arguments of worker_approve.
type WorkerApproveArgs struct {
	WriteArgs
	// WorkerID is the Hub's id of the Worker (UUIDv7).
	WorkerID string `json:"worker_id"`
	// Fingerprint, when given, is the fingerprint of the Worker's public key
	// that the caller looked at. The Hub approves only if it matches, so that
	// a Worker that was replaced between the look and the click is not
	// approved by accident.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Validate checks the request id, the Worker id and the optional fingerprint.
func (a WorkerApproveArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if a.Fingerprint != "" && !validFingerprint(a.Fingerprint) {
		return invalid("fingerprint", "must be 52 lowercase base32 characters")
	}
	return nil
}

func validFingerprint(s string) bool {
	if len(s) != 52 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func init() {
	// Approval needs manage or operate on the Worker: the owner and admins
	// hold operate on it (AD-17), an admin's API key holds manage through its
	// scope. A Worker identity holds neither, so a pending Worker cannot
	// approve itself.
	Register(Op{
		Name: OpWorkerApprove,
		Summary: "Approve a pending Worker so that the Hub routes operations to it and accepts its calls; " +
			"compare the fingerprint, name and source address first.",
		Scope:   ScopeHub,
		Actions: MustActionSet(ActionOperate, ActionManage), Write: true,
		Guards: []Guard{guardWorkerKnown},
		Args:   reflect.TypeFor[WorkerApproveArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
