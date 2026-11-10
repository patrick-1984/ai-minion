package ops

import (
	"encoding/json"
	"reflect"
)

// Names of the trust operations (AD-17, R29).
const (
	OpTrustSet  = "trust_set"
	OpTrustList = "trust_list"
)

// guardTrustTargetVisible refuses a Worker or session the caller cannot read:
// nobody marks a source trusted that they cannot see.
var guardTrustTargetVisible = Guard{Name: "trust_target_visible", Class: GuardKeep, Reads: "worker.owner"}

// TrustSetArgs are the arguments of trust_set. The receiver is the calling
// user (the acting admin during act-as), never an argument: nobody sets trust
// for somebody else.
type TrustSetArgs struct {
	WriteArgs
	// WorkerID is the Worker whose output is trusted.
	WorkerID string `json:"worker_id"`
	// SessionName limits the trust to one session; empty trusts the whole
	// Worker.
	SessionName string `json:"session_name,omitempty"`
	// Remove deletes the trust row instead of storing it. Removing the
	// whole-Worker row keeps the rows of single sessions.
	Remove bool `json:"remove,omitempty"`
}

// Validate checks the request id, the Worker id and the session name.
func (a TrustSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if a.SessionName != "" && !validSessionName(a.SessionName) {
		return invalid("session_name", "invalid session name")
	}
	return nil
}

// TrustListArgs are the arguments of trust_list: none. It lists the trust of
// the calling user.
type TrustListArgs struct{}

// TrustInfo is one trust row.
type TrustInfo struct {
	WorkerID string `json:"worker_id"`
	// SessionName is empty for a row that covers the whole Worker.
	SessionName string    `json:"session_name"`
	CreatedAt   Timestamp `json:"created_at"`
}

// TrustListResult is the result of trust_list.
type TrustListResult struct {
	Trust []TrustInfo `json:"trust"`
}

// MarshalJSON writes trust as an empty array instead of null.
func (r TrustListResult) MarshalJSON() ([]byte, error) {
	type plain TrustListResult
	if r.Trust == nil {
		r.Trust = []TrustInfo{}
	}
	return json.Marshal(plain(r))
}

func init() {
	// Trust is the receiver's own marker: read on the Worker or session is
	// enough, which every owner and admin holds. It changes only whether a
	// model reads output as trusted and never restricts anybody.
	Register(Op{
		Name:    OpTrustSet,
		Summary: "Mark a Worker or session as a trusted source for yourself, or remove the mark.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead), Write: true,
		Guards: []Guard{guardTrustTargetVisible},
		Args:   reflect.TypeFor[TrustSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name:    OpTrustList,
		Summary: "List the Workers and sessions you trust.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionRead),
		Args: reflect.TypeFor[TrustListArgs](), Result: reflect.TypeFor[TrustListResult](),
	})
}
