package ops

import (
	"reflect"
	"strings"
)

// Pairing requests (v0.12.0): a Worker that waits with a pairing code is also
// a request that an administrator can admit without the code, after comparing
// the Worker's fingerprint words and the Hub's (NetBird peer approval, SSH and
// Signal fingerprint comparison). The three operations are admin only (manage
// on the Hub): admitting a Worker without its code gives its owner its
// terminal sessions, so a plain user still needs the code, which proves that
// they see the Worker's screen.
const (
	OpPairingRequestsList  = "pairing_requests_list"
	OpPairingRequestAdmit  = "pairing_request_admit"
	OpPairingRequestReject = "pairing_request_reject"
)

// PairingRequestsListArgs are the arguments of pairing_requests_list (none).
type PairingRequestsListArgs struct{}

// PairingRequestInfo is one waiting Worker.
type PairingRequestInfo struct {
	// PairingID is the public pairing id (the first four characters of the
	// Worker's code): it names the request, it is not a secret.
	PairingID string `json:"pairing_id"`
	// Name is the name the Worker announced.
	Name string `json:"name"`
	// SourceAddr is the client address the Worker connected from.
	SourceAddr string `json:"source_addr"`
	// WaitingSince and ExpiresAt are RFC 3339 times.
	WaitingSince string `json:"waiting_since"`
	ExpiresAt    string `json:"expires_at"`
	// WorkerFingerprint and WorkerFingerprintWords are the fingerprint of the
	// Worker key the request presents: the Worker's console shows the same.
	WorkerFingerprint      string `json:"worker_fingerprint"`
	WorkerFingerprintWords string `json:"worker_fingerprint_words"`
	// HubFingerprint and HubFingerprintWords are the Hub root the Worker's
	// handshake was answered with: the words its console box shows. After a
	// master-key rotation they differ from the Hub's current ones, and the
	// request is no longer listed.
	HubFingerprint      string `json:"hub_fingerprint"`
	HubFingerprintWords string `json:"hub_fingerprint_words"`
}

// PairingRequestsListResult is the result of pairing_requests_list.
type PairingRequestsListResult struct {
	Requests []PairingRequestInfo `json:"requests"`
}

// PairingRequestAdmitArgs are the arguments of pairing_request_admit.
type PairingRequestAdmitArgs struct {
	WriteArgs
	// PairingID names the request.
	PairingID string `json:"pairing_id"`
	// WorkerFingerprint is the 52-character fingerprint of the Worker key the
	// caller compared with the Worker's console. The Hub admits only the
	// request that presents exactly this key, and enrolls exactly this key.
	WorkerFingerprint string `json:"worker_fingerprint"`
	// HubFingerprint is the 52-character fingerprint of the Hub root that the
	// request was answered with (the list shows it): the words the caller
	// compared with the Worker's console. A request answered with another
	// root is not admitted.
	HubFingerprint string `json:"hub_fingerprint"`
	// Name optionally names the new Worker; the default is the announced name.
	Name string `json:"name,omitempty"`
}

// PairingRequestRejectArgs are the arguments of pairing_request_reject.
type PairingRequestRejectArgs struct {
	WriteArgs
	PairingID         string `json:"pairing_id"`
	WorkerFingerprint string `json:"worker_fingerprint"`
}

// validPairingID: four characters of the pairing code alphabet (Crockford
// base32 as the code uses it), any case.
func validPairingID(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range strings.ToUpper(s) {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func validateRequestRef(id, fingerprint string) error {
	if !validPairingID(id) {
		return invalid("pairing_id", "must be the four characters of the pairing id")
	}
	if !validFingerprint(fingerprint) {
		return invalid("worker_fingerprint", "must be 52 lowercase base32 characters")
	}
	return nil
}

// Validate checks the request id, the pairing id, the fingerprint and the name.
func (a PairingRequestAdmitArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := validateRequestRef(a.PairingID, a.WorkerFingerprint); err != nil {
		return err
	}
	if !validFingerprint(a.HubFingerprint) {
		return invalid("hub_fingerprint", "must be 52 lowercase base32 characters")
	}
	if a.Name != "" && !validWorkerName(a.Name) {
		return invalid("name", "must be at most %d bytes without spaces, colons or control characters", MaxWorkerNameLen)
	}
	return nil
}

// Validate checks the request id, the pairing id and the fingerprint.
func (a PairingRequestRejectArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	return validateRequestRef(a.PairingID, a.WorkerFingerprint)
}

// guardPairingRequestOpen refuses a request that is gone or now presents
// another key. It is KEEP: force never admits another key.
var guardPairingRequestOpen = Guard{Name: "pairing_request_open", Class: GuardKeep, Reads: "pairing.pending"}

func init() {
	manage := MustActionSet(ActionManage)
	Register(Op{
		Name: OpPairingRequestsList,
		Summary: "List the Workers that wait with a pairing code and can be admitted without it: name, source address, " +
			"waiting since, the Worker fingerprint and the public pairing id (admin only).",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[PairingRequestsListArgs](), Result: reflect.TypeFor[PairingRequestsListResult](),
	})
	Register(Op{
		Name: OpPairingRequestAdmit,
		Summary: "Admit a waiting Worker without its code; it joins your space approved. Admit only if the Worker's console " +
			"shows this Worker fingerprint and this Hub's fingerprint words (admin only).",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardPairingRequestOpen, guardPairingNameFree},
		Args:   reflect.TypeFor[PairingRequestAdmitArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name:    OpPairingRequestReject,
		Summary: "Reject a waiting Worker's pairing request; the Worker shows that it was rejected and a new code (admin only).",
		Scope:   ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardPairingRequestOpen},
		Args:   reflect.TypeFor[PairingRequestRejectArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
