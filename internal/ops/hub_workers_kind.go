package ops

import "reflect"

// Names of the Worker administration operations (AD-17, AD-12, R97).
const (
	OpWorkerReassign = "worker_reassign"
	OpWorkerRestrict = "worker_restrict"
	OpWorkerRename   = "worker_rename"
)

// Guards of the Worker administration operations. They are Hub guards: the Hub
// evaluates them before it changes anything, and none can be overridden.
var (
	// guardWorkerInfrastructure refuses to reassign a private Worker, for
	// every caller including admins (AD-17).
	guardWorkerInfrastructure = Guard{Name: "worker_infrastructure", Class: GuardKeep, Reads: "worker.kind"}
	// guardNewOwnerActive refuses a new owner that is unknown or deactivated.
	guardNewOwnerActive = Guard{Name: "new_owner_active", Class: GuardKeep, Reads: "user.active"}
	// guardWorkerNameFree refuses a name that the (new) owner already uses
	// for another Worker: Worker names are unique per owner (AD-17).
	guardWorkerNameFree = Guard{Name: "worker_name_free", Class: GuardKeep, Reads: "worker.name"}
	// guardWorkerKnown refuses an unknown or removed Worker.
	guardWorkerKnown = Guard{Name: "worker_known", Class: GuardKeep, Reads: "worker.id"}
)

// WorkerReassignArgs are the arguments of worker_reassign.
type WorkerReassignArgs struct {
	WriteArgs
	// WorkerID is the infrastructure Worker to move.
	WorkerID string `json:"worker_id"`
	// UserID is the user who becomes the owner.
	UserID string `json:"user_id"`
}

// Validate checks the request id and both ids.
func (a WorkerReassignArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if !ValidUUIDv7(a.UserID) {
		return invalid("user_id", "must be a UUIDv7")
	}
	return nil
}

// WorkerRestrictArgs are the arguments of worker_restrict.
type WorkerRestrictArgs struct {
	WriteArgs
	WorkerID string `json:"worker_id"`
	// PreviewOnly turns the Hub-side restriction on or off. With it on, the
	// Worker refuses every write; reads still work. The Worker's own local
	// restriction is never lifted by turning this off (AD-12).
	PreviewOnly bool `json:"preview_only"`
}

// Validate checks the request id and the Worker id.
func (a WorkerRestrictArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	return nil
}

// WorkerRenameArgs are the arguments of worker_rename.
type WorkerRenameArgs struct {
	WriteArgs
	WorkerID string `json:"worker_id"`
	// Name is the new name, unique among the owner's Workers: not empty, at
	// most 64 bytes, no ":", whitespace or control characters.
	Name string `json:"name"`
}

// Validate checks the request id, the Worker id and the name.
func (a WorkerRenameArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if !validWorkerName(a.Name) {
		return invalid("name", "must be 1 to %d bytes without ':', whitespace or control characters", maxWorkerNameLen)
	}
	return nil
}

func init() {
	// worker_reassign is global in effect (it moves a Worker between users),
	// so it needs manage: admins only. A Worker owner passes the authorisation
	// of operate but holds no manage and gets forbidden.
	Register(Op{
		Name:    OpWorkerReassign,
		Summary: "Reassign an infrastructure Worker to another user (admin only); trust is dropped and bound keys are revoked.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionManage), Write: true,
		Guards: []Guard{guardWorkerInfrastructure, guardNewOwnerActive, guardWorkerNameFree},
		Args:   reflect.TypeFor[WorkerReassignArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	// worker_restrict and worker_rename act on one Worker: the owner and
	// admins hold operate on it.
	Register(Op{
		Name:    OpWorkerRestrict,
		Summary: "Restrict a Worker to preview only (or lift the Hub-side restriction); the Worker's local restrictions stay.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardWorkerKnown},
		Args:   reflect.TypeFor[WorkerRestrictArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name:    OpWorkerRename,
		Summary: "Rename a Worker; the name must be unique among the owner's Workers.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardWorkerKnown, guardWorkerNameFree},
		Args:   reflect.TypeFor[WorkerRenameArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
