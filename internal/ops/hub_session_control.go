package ops

import (
	"encoding/json"
	"errors"
	"reflect"
)

// Names of the session-control operations. A session-control grant lets one
// controller session (a Worker and a session name) reach other Workers and
// sessions. Only an admin lists or changes grants, and no grant exists until
// an admin creates one.
const (
	OpSessionControlList = "session_control_list"
	OpSessionControlSet  = "session_control_set"
)

// ValidateSessionControlTargets checks the targets of a grant. They use the
// resources of an API-key scope: one everything resource alone, or up to
// MaxKeyResources worker and session resources. An empty list is valid; it
// removes the grant.
func ValidateSessionControlTargets(targets []KeyResource) error {
	if len(targets) > MaxKeyResources {
		return invalid("targets", "must list at most %d resources", MaxKeyResources)
	}
	for _, r := range targets {
		if r.Kind == KeyResourceUser {
			return invalid("targets", "kind must be everything, worker or session")
		}
		if err := r.Validate(); err != nil {
			var ae *ArgumentError
			if errors.As(err, &ae) {
				return invalid("targets", "%s", ae.Reason)
			}
			return err
		}
		if r.Kind == KeyResourceEverything && len(targets) > 1 {
			return invalid("targets", "everything cannot be combined with other resources")
		}
	}
	return nil
}

// SessionControlListArgs are the arguments of session_control_list (none).
type SessionControlListArgs struct{}

// SessionControlInfo is one grant.
type SessionControlInfo struct {
	// WorkerID and SessionName name the controller session.
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
	// Targets are what the controller session may reach.
	Targets   []KeyResource `json:"targets"`
	CreatedBy string        `json:"created_by"`
	CreatedAt Timestamp     `json:"created_at"`
}

// SessionControlListResult is the result of session_control_list, ordered by
// Worker and session.
type SessionControlListResult struct {
	Grants []SessionControlInfo `json:"grants"`
}

// MarshalJSON writes grants as an empty array instead of null.
func (r SessionControlListResult) MarshalJSON() ([]byte, error) {
	type plain SessionControlListResult
	if r.Grants == nil {
		r.Grants = []SessionControlInfo{}
	}
	return json.Marshal(plain(r))
}

// SessionControlSetArgs are the arguments of session_control_set.
type SessionControlSetArgs struct {
	WriteArgs
	// WorkerID is the Worker of the controller session (UUIDv7).
	WorkerID string `json:"worker_id"`
	// SessionName is the controller session on that Worker.
	SessionName string `json:"session_name"`
	// Targets replace the grant of the controller session; an empty list
	// removes it.
	Targets []KeyResource `json:"targets,omitempty"`
}

// Validate checks the request id, the Worker id, the session name and the
// targets. Whether the Worker exists is decided on the Hub.
func (a SessionControlSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if !validSessionName(a.SessionName) {
		return invalid("session_name", "invalid session name")
	}
	return ValidateSessionControlTargets(a.Targets)
}

func init() {
	manage := MustActionSet(ActionManage)

	Register(Op{
		Name:    OpSessionControlList,
		Summary: "List the session-control grants: which controller session may reach which Workers and sessions.",
		Scope:   ScopeHub, Actions: manage,
		Args: reflect.TypeFor[SessionControlListArgs](), Result: reflect.TypeFor[SessionControlListResult](),
	})
	Register(Op{
		Name: OpSessionControlSet,
		Summary: "Set the targets a controller session may reach (everything, or Workers and sessions); " +
			"empty targets remove the grant.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardWorkerKnown},
		Args:   reflect.TypeFor[SessionControlSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
