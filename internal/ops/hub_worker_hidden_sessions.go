package ops

import "reflect"

// OpWorkerHiddenSessionsSet sets the hidden-sessions choice of one Worker
// (v0.23.0): whether new sessions that would be hidden from the person (Windows
// ConPTY inside the Worker, a tmux server that is not their default one) may be
// created on it. The Worker's owner sets it on their own Workers, an
// administrator on any Worker. A Worker without a choice of its own follows the
// caller's account-wide preference (account_prefs_set), as before. The Hub
// setting hidden_sessions (allowed, disabled) overrides both.
const OpWorkerHiddenSessionsSet = "worker_hidden_sessions_set"

// Values of WorkerHiddenSessionsSetArgs.HiddenSessions.
const (
	// WorkerHiddenAllow allows new hidden sessions on the Worker.
	WorkerHiddenAllow = "allow"
	// WorkerHiddenRefuse refuses new hidden sessions on the Worker.
	WorkerHiddenRefuse = "refuse"
	// WorkerHiddenDefault removes the Worker's own choice: each caller's
	// account-wide preference decides again.
	WorkerHiddenDefault = "default"
)

// WorkerHiddenSessionsSetArgs are the arguments of worker_hidden_sessions_set.
type WorkerHiddenSessionsSetArgs struct {
	WriteArgs
	WorkerID string `json:"worker_id"`
	// HiddenSessions is allow, refuse or default (the account preference of
	// each caller decides).
	HiddenSessions string `json:"hidden_sessions"`
}

// Validate checks the request id, the Worker id and the choice.
func (a WorkerHiddenSessionsSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	switch a.HiddenSessions {
	case WorkerHiddenAllow, WorkerHiddenRefuse, WorkerHiddenDefault:
		return nil
	}
	return invalid("hidden_sessions", "must be %s, %s or %s", WorkerHiddenAllow, WorkerHiddenRefuse, WorkerHiddenDefault)
}

// Allow is the stored form of the choice: nil for default.
func (a WorkerHiddenSessionsSetArgs) Allow() *bool {
	switch a.HiddenSessions {
	case WorkerHiddenAllow:
		v := true
		return &v
	case WorkerHiddenRefuse:
		v := false
		return &v
	}
	return nil
}

func init() {
	// The owner and administrators hold operate on a Worker; the handler
	// also refuses anyone else who holds operate through a grant (a share):
	// the choice is the owner's or an administrator's.
	Register(Op{
		Name:    OpWorkerHiddenSessionsSet,
		Summary: "Allow or refuse new hidden sessions on one Worker, or return it to each caller's account preference (the owner on their own Workers, an administrator on any Worker).",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardWorkerKnown},
		Args:   reflect.TypeFor[WorkerHiddenSessionsSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
