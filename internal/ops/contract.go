package ops

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidArgument is the sentinel behind every argument validation error of
// this package. The adapter edge maps it to proto's invalid_argument code; this
// package declares no codes (AD-2).
var ErrInvalidArgument = errors.New("invalid_argument")

// ArgumentError describes one invalid argument. It matches ErrInvalidArgument
// with errors.Is.
type ArgumentError struct {
	Field  string // JSON field name, empty when the value as a whole is invalid
	Reason string // short English explanation, never contains user content
}

// Error implements error.
func (e *ArgumentError) Error() string {
	if e.Field == "" {
		return "invalid_argument: " + e.Reason
	}
	return "invalid_argument: " + e.Field + ": " + e.Reason
}

// Is makes errors.Is(err, ErrInvalidArgument) true.
func (e *ArgumentError) Is(target error) bool { return target == ErrInvalidArgument }

func invalid(field, format string, args ...any) error {
	return &ArgumentError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Validator is implemented by argument types that can check themselves. Every
// error it returns matches ErrInvalidArgument.
type Validator interface {
	Validate() error
}

// Outcome is the closed outcome vocabulary of a write (AD-15).
type Outcome string

// The four outcomes.
const (
	// OutcomeDelivered means the input reached the pane.
	OutcomeDelivered Outcome = "delivered"
	// OutcomeFailedNotSent means nothing at all was sent. It is never used
	// once any key went out.
	OutcomeFailedNotSent Outcome = "failed_not_sent"
	// OutcomePartial means some of the input was sent and the rest was not.
	OutcomePartial Outcome = "partial"
	// OutcomeUnknown means the result cannot be told, for example when the
	// transport was lost during the write.
	OutcomeUnknown Outcome = "unknown"
)

// Valid reports whether o is one of the four outcomes.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeDelivered, OutcomeFailedNotSent, OutcomePartial, OutcomeUnknown:
		return true
	}
	return false
}

// FailureOutcome returns the outcome of a write that stopped early: partial
// when anything was already sent, failed_not_sent otherwise. This is the rule
// that once any key was sent the outcome is never failed_not_sent.
func FailureOutcome(sentAny bool) Outcome {
	if sentAny {
		return OutcomePartial
	}
	return OutcomeFailedNotSent
}

// GuardClass is the action-contract class of one guard (AD-15, R87).
type GuardClass string

// The three guard classes. A fourth class, REMOVE, must not exist: the
// software informs and the operator decides.
const (
	// GuardKeep is never overridable: the target re-check, writer locks and
	// real errors.
	GuardKeep GuardClass = "KEEP"
	// GuardWarn never blocks and is reported in warnings[].
	GuardWarn GuardClass = "WARN"
	// GuardForce refuses by default with failed_not_sent and forceable=true;
	// force=true in the same call overrides it.
	GuardForce GuardClass = "FORCE"
)

// Valid reports whether c is KEEP, WARN or FORCE.
func (c GuardClass) Valid() bool {
	switch c {
	case GuardKeep, GuardWarn, GuardForce:
		return true
	}
	return false
}

// Overridable reports whether force=true overrides a guard of this class.
func (c GuardClass) Overridable() bool { return c == GuardForce }

// Blocks reports whether a guard of this class can stop a write when it fires
// (and, for FORCE, force is not set).
func (c GuardClass) Blocks() bool { return c == GuardKeep || c == GuardForce }

// Guard declares one check the Worker makes before or during a write, with
// its class and the pane state it reads (AD-12: every guard names the state it
// reads).
type Guard struct {
	Name  string     // snake_case identifier
	Class GuardClass // KEEP, WARN or FORCE
	Reads string     // the state the guard reads, for example "pane.state"
}

func (g Guard) validate() error {
	if !isSnakeCase(g.Name) {
		return fmt.Errorf("guard name %q is not snake_case", g.Name)
	}
	if !g.Class.Valid() {
		return fmt.Errorf("guard %q has unknown class %q", g.Name, g.Class)
	}
	if g.Reads == "" {
		return fmt.Errorf("guard %q does not name the state it reads", g.Name)
	}
	return nil
}

// Timestamp is a time that encodes as RFC 3339 UTC with milliseconds, the only
// time format of the JSON API (AD-18).
type Timestamp time.Time

const timestampLayout = "2006-01-02T15:04:05.000Z"

// MarshalJSON implements json.Marshaler.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(timestampLayout))
}

// UnmarshalJSON implements json.Unmarshaler. It accepts exactly the format
// that MarshalJSON writes.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(timestampLayout, s)
	if err != nil {
		return invalid("", "timestamp must be RFC 3339 UTC with milliseconds")
	}
	*t = Timestamp(parsed)
	return nil
}

// Time returns the value as a time.Time (UTC).
func (t Timestamp) Time() time.Time { return time.Time(t).UTC() }

// Pane classification vocabulary (AD-12). agentdetect alone reports these.
const (
	// PaneKindShell, PaneKindOther and PaneKindUnknown are the fixed kinds;
	// agent panes use the prefix PaneKindAgentPrefix followed by the CLI name.
	PaneKindShell       = "shell"
	PaneKindOther       = "other"
	PaneKindUnknown     = "unknown"
	PaneKindAgentPrefix = "agent:"

	// The closed pane states.
	PaneStateStable  = "stable"
	PaneStateBusy    = "busy"
	PaneStateDialog  = "dialog"
	PaneStateUnknown = "unknown"
)

// PaneInfo describes the resolved pane of a call. The Worker fills it; Kind
// and State come from agentdetect.
type PaneInfo struct {
	Session string `json:"session"`           // session name (the target identity, R88)
	ID      string `json:"id"`                // tmux pane id such as "%3", or the ConPTY pane id
	Window  int    `json:"window"`            // window index
	Index   int    `json:"index"`             // pane index within the window
	Command string `json:"command,omitempty"` // current foreground command
	Kind    string `json:"kind,omitempty"`    // agent:<cli>, shell, other or unknown
	State   string `json:"state,omitempty"`   // stable, busy, dialog or unknown

	// The reviewed metadata. A caller that wants to act on what it saw copies
	// these into the write precondition (ReviewedDialog for answer). They are
	// set by snapshot, capture and the write results; sessions_list sets PID
	// only.

	// PID is the process id of the pane's program; omitted when unknown.
	PID int `json:"pid,omitempty"`
	// SessionCreated is the creation time of the session in Unix seconds, the
	// generation of the session: a session recreated under the same name has
	// another one. Omitted when unknown.
	SessionCreated int64 `json:"session_created,omitempty"`
	// DialogDigest is the semantic digest of the open dialog (kind, question,
	// options and the keys of the options), lowercase hex SHA-256. It is
	// present only when State is dialog, and it does not change with a moved
	// selection marker or a running status line.
	DialogDigest string `json:"dialog_digest,omitempty"`
}

// Warning is one WARN-class finding. WARN never blocks.
type Warning struct {
	Code    string `json:"code"`    // snake_case code
	Message string `json:"message"` // short English text, never terminal content
}

// WriteResult is the result of every write op (AD-15).
type WriteResult struct {
	Outcome   Outcome   `json:"outcome"`
	Reason    string    `json:"reason"`    // reason code from internal/proto/codes.go
	Forceable bool      `json:"forceable"` // true only for a FORCE refusal (failed_not_sent)
	Forced    bool      `json:"forced"`    // force=true was given and overrode a FORCE guard
	Warnings  []Warning `json:"warnings"`  // always an array, never null
	Pane      *PaneInfo `json:"pane"`      // the resolved pane, null when none was resolved
}

// MarshalJSON writes warnings as an empty array instead of null.
func (r WriteResult) MarshalJSON() ([]byte, error) {
	type plain WriteResult
	if r.Warnings == nil {
		r.Warnings = []Warning{}
	}
	return json.Marshal(plain(r))
}

// Validate checks the invariants of the action contract:
// the outcome is known; forceable only goes with failed_not_sent; forceable
// and forced never both hold (a FORCE refusal was not overridden); every
// non-delivered outcome carries a reason.
func (r WriteResult) Validate() error {
	if !r.Outcome.Valid() {
		return invalid("outcome", "unknown outcome %q", string(r.Outcome))
	}
	if r.Forceable && r.Outcome != OutcomeFailedNotSent {
		return invalid("forceable", "a forceable result must be failed_not_sent")
	}
	if r.Forceable && r.Forced {
		return invalid("forced", "a result cannot be both forceable and forced")
	}
	if r.Outcome != OutcomeDelivered && r.Reason == "" {
		return invalid("reason", "outcome %s requires a reason", string(r.Outcome))
	}
	for i, w := range r.Warnings {
		if !isSnakeCase(w.Code) {
			return invalid("warnings", "warning %d has a code that is not snake_case", i)
		}
	}
	return nil
}

// Refused builds the result of a FORCE-class refusal: nothing was sent, and
// the caller may repeat the call with force=true.
func Refused(reason string, pane *PaneInfo, warnings ...Warning) WriteResult {
	return WriteResult{Outcome: OutcomeFailedNotSent, Reason: reason, Forceable: true, Warnings: warnings, Pane: pane}
}

// WriteArgs is embedded in the arguments of every write op. Its JSON fields
// are `request_id` and `force`.
type WriteArgs struct {
	// RequestID is a UUIDv7 chosen by the caller. The Worker caches the
	// result for 10 minutes and answers a duplicate from the cache; no layer
	// retries a write.
	RequestID string `json:"request_id"`
	// Force overrides FORCE-class guards in the same call (never KEEP).
	Force bool `json:"force"`
}

// Validate checks the request id.
func (w WriteArgs) Validate() error {
	if !ValidUUIDv7(w.RequestID) {
		return invalid("request_id", "must be a UUIDv7")
	}
	return nil
}

// NewUUIDv7 returns a fresh UUIDv7 in canonical lowercase text form, the
// format of request ids and turn ids.
func NewUUIDv7() string {
	return newUUIDv7(time.Now())
}

func newUUIDv7(now time.Time) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ops: no randomness: " + err.Error())
	}
	ms := uint64(now.UnixMilli())
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	const hexdigits = "0123456789abcdef"
	var sb strings.Builder
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			sb.WriteByte('-')
		}
		sb.WriteByte(hexdigits[c>>4])
		sb.WriteByte(hexdigits[c&0x0f])
	}
	return sb.String()
}

// ValidUUIDv7 reports whether s is a canonical lowercase UUID text with
// version 7 and the RFC 9562 variant.
func ValidUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	if s[14] != '7' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
		return true
	}
	return false
}
