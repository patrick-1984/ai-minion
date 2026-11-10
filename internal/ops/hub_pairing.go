package ops

import (
	"fmt"
	"log/slog"
	"reflect"
	"unicode"
	"unicode/utf8"
)

// OpPairingClaim is the name of the Hub operation that claims a pairing code
// shown by a Worker (AD-8, R54).
const OpPairingClaim = "pairing_claim"

// Limits of pairing_claim.
const (
	// MaxPairingCodeLen caps the code as typed: "MN-XXXX-XXXX" has 12
	// characters; the rest is room for spaces. The Hub parses the code
	// exactly (internal/identity); this only bounds the input.
	MaxPairingCodeLen = 32
	// MaxWorkerNameLen caps a Worker name in bytes.
	MaxWorkerNameLen = maxWorkerNameLen
)

// Guard and warning codes of pairing_claim.
const (
	// WarnWorkerRenamed says the requested or announced name was already
	// used by another Worker of the owner and a numeric suffix was added.
	WarnWorkerRenamed = "worker_renamed"
)

var (
	// guardPairingLive refuses a code that no Worker is waiting for, that
	// expired, was used or is out of attempts. It is KEEP: force never
	// revives a code.
	guardPairingLive = Guard{Name: "pairing_live", Class: GuardKeep, Reads: "pairing.pending"}
	// guardPairingNameFree reports that the name was already taken in the
	// owner's space. It never blocks.
	guardPairingNameFree = Guard{Name: "pairing_name_free", Class: GuardWarn, Reads: "owner.workers"}
)

// PairingClaimArgs are the arguments of pairing_claim.
type PairingClaimArgs struct {
	WriteArgs
	// Code is the pairing code the Worker shows, MN-XXXX-XXXX in any letter
	// case with optional hyphens. It is a secret until it is used: it is
	// never logged, audited or echoed back.
	Code string `json:"code"`
	// Name optionally names the new Worker; the default is the name the
	// Worker announced.
	Name string `json:"name,omitempty"`
}

// Validate checks the request id, the shape of the code and the name. The
// error never repeats the code.
func (a PairingClaimArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.Code == "" {
		return invalid("code", "is required")
	}
	if len(a.Code) > MaxPairingCodeLen || !utf8.ValidString(a.Code) {
		return invalid("code", "is not a pairing code")
	}
	for _, r := range a.Code {
		if unicode.IsControl(r) {
			return invalid("code", "is not a pairing code")
		}
	}
	if a.Name != "" && !validWorkerName(a.Name) {
		return invalid("name", "must be at most %d bytes without spaces, colons or control characters", MaxWorkerNameLen)
	}
	return nil
}

// ValidWorkerName reports whether name can name a Worker: the rule of the
// human target "worker:session", so non-empty, at most MaxWorkerNameLen bytes,
// without ":", whitespace or control characters.
func ValidWorkerName(name string) bool { return validWorkerName(name) }

// String redacts the code, so that formatting the arguments never prints it.
func (a PairingClaimArgs) String() string {
	return fmt.Sprintf("pairing_claim{request_id:%s name:%q code:[redacted]}", a.RequestID, a.Name)
}

// GoString implements fmt.GoStringer with the same redaction.
func (a PairingClaimArgs) GoString() string { return a.String() }

// LogValue implements slog.LogValuer with the same redaction.
func (a PairingClaimArgs) LogValue() slog.Value {
	return slog.GroupValue(slog.String("request_id", a.RequestID), slog.String("code", "[redacted]"))
}

func init() {
	Register(Op{
		Name:    OpPairingClaim,
		Summary: "Enter the pairing code a Worker shows, so that the Worker appears in your space as a private Worker.",
		Scope:   ScopeHub, Actions: MustActionSet(ActionOperate), Write: true,
		Guards: []Guard{guardPairingLive, guardPairingNameFree},
		Args:   reflect.TypeFor[PairingClaimArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
