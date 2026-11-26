package ops

import "reflect"

// OpSessionKeepSet marks a session as kept (persistent) or not (v0.24.0).
//
// A kept session is remembered by its Worker: the Worker saves its layout
// (windows, pane splits, each pane's folder and the agent tool with its
// conversation) every minute and on every change, and recreates it when the
// Worker starts again and the session does not exist (after a reboot, a
// restart of the service or of the terminal server). It is a Worker op next to
// the core ops ([CoreWorkerOpNames] stays the fixed core): the Hub routes it to
// the Worker the target names, it is a write (request_id, cached result, local
// audit) and needs the action operate on the session, like set_task. A Worker
// that predates it answers unsupported_op; one that serves it announces the
// feature proto.FeatureSessionKeepV1 and reports [SessionInfo.Keep].
//
// Semantics the Worker guarantees:
//   - keep=true needs an existing shared session; keep=false always succeeds
//     (also for a session that is gone) and removes the session from the
//     Worker's keep list, with its saved layout.
//   - The flag belongs to the session name and follows a rename of the same
//     terminal session (by its instance id).
//   - A kept session that ends while the Worker runs (closed by its user, or
//     the terminal server stopped) keeps its flag: it is recreated only at the
//     next start of the Worker or of its managed terminal server, never while
//     both keep running. Only an explicit keep=false forgets it.
//   - A restore never kills, renames or changes an existing session of that
//     name: such a name is skipped.
const OpSessionKeepSet = "session_keep_set"

// KeepWorkerOpNames lists the keep ops, in catalog order.
var KeepWorkerOpNames = []string{OpSessionKeepSet}

// SessionKeepSetArgs are the arguments of session_keep_set.
type SessionKeepSetArgs struct {
	WriteArgs
	// Target names the session; a window or pane in it is ignored.
	Target Target `json:"target"`
	// Keep is true to keep the session across restarts, false to forget it.
	Keep bool `json:"keep"`
}

// Validate checks the request id and the target.
func (a SessionKeepSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	return a.Target.Validate()
}

func init() {
	Register(Op{
		Name: OpSessionKeepSet, Summary: "Keep a session across Worker restarts (keep=true) or forget it (keep=false).",
		Scope: ScopeWorker, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardSessionExists},
		Args:   reflect.TypeFor[SessionKeepSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
