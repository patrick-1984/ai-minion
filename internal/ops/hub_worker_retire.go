package ops

import "reflect"

// OpWorkerRetire is the name of the Hub operation with which a Worker retires
// its own enrollment (AD-8, AD-9, Story 4.7; integrator amendment A-16).
//
// Authorization: the op is served only on a Worker's own `ca` stream, and the
// decision is the KEEP guard retire_self_only on the identity that the
// handshake authenticated. There is no authz.Decide call: authz never grants
// operate to a Worker principal, and the op names no Worker. Story 7.4's call
// authentication must keep it that way. The catalog entry therefore carries the
// action operate only as its classification; it is not a REST, MCP or CLI
// operation (worker_remove is the one for users).
const OpWorkerRetire = "worker_retire"

// guardRetireSelf is KEEP: the Hub removes only the enrollment of the key that
// authenticated the connection the request arrived on, never another Worker's.
var guardRetireSelf = Guard{Name: "retire_self_only", Class: GuardKeep, Reads: "call.principal"}

// WorkerRetireArgs are the arguments of worker_retire. The Worker is not
// named: the Hub acts on the Worker identity that authenticated the `call`
// stream (AD-10), so a Worker can retire only itself.
type WorkerRetireArgs struct {
	WriteArgs
}

func init() {
	Register(Op{
		Name: OpWorkerRetire,
		Summary: "Retire the calling Worker's identity: remove its enrollment and leave a tombstone, " +
			"so that the old key gets a signed revoke on its next connect.",
		Scope: ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardRetireSelf},
		Args:   reflect.TypeFor[WorkerRetireArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
