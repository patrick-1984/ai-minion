package pipe

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"
)

// Peer identifies one end of the pipe: the Worker and the session on it. It
// is the key of every limit and of the budget in both Hub and Solo mode
// (AD-23).
type Peer struct {
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
}

// Valid reports whether both parts of the peer are set.
func (p Peer) Valid() bool { return p.WorkerID != "" && p.SessionName != "" }

// String returns "worker_id/session_name" for logs and error messages.
func (p Peer) String() string { return p.WorkerID + "/" + p.SessionName }

// Message is a pipe message sent to a session. The store assigns ID.
type Message struct {
	ID   int64     `json:"id"`
	Peer Peer      `json:"peer"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Turn is one agent reply extracted on the Worker (AD-23). TurnID is a
// UUIDv7 and makes the commit idempotent.
type Turn struct {
	TurnID string    `json:"turn_id"`
	Peer   Peer      `json:"peer"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// AckStatus is the status of a turn.ack.
type AckStatus string

const (
	// AckStored means the turn is in the store. It is the answer to every
	// valid commit, first or repeated. It says nothing about delivery.
	AckStored AckStatus = "stored"
	// AckRejected means the turn was not stored; Reason says why. It is final:
	// sending the turn again cannot change the answer, so the sender drops it.
	AckRejected AckStatus = "rejected"
	// AckBusy means the turn was not stored because the Hub is out of capacity
	// for it now (a rate or storage limit, pressure on the database). It is
	// not final and not an acknowledgement: the sender keeps the turn and sends
	// it again after RetryAfterMS. Nothing was stored.
	AckBusy AckStatus = "busy"
)

// Ack is the payload of the wire frame turn.ack{status, reason}.
type Ack struct {
	TurnID string    `json:"turn_id"`
	Status AckStatus `json:"status"`
	Reason string    `json:"reason,omitempty"`
	// RetryAfterMS is the earliest time to send the turn again, with status
	// busy.
	RetryAfterMS int64 `json:"retry_after_ms,omitempty"`
}

// IsTurnID reports whether s is the canonical lower-case text form of a
// UUIDv7: version nibble 7 and variant bits 10 (RFC 9562).
func IsTurnID(s string) bool {
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

// NewTurnID returns a new UUIDv7 for the given time, with random bits from
// crypto/rand.
func NewTurnID(now time.Time) (string, error) {
	return NewTurnIDFrom(now, rand.Reader)
}

// NewTurnIDFrom is NewTurnID with an explicit source of random bytes, for
// deterministic tests. It reads 10 bytes from r.
func NewTurnIDFrom(now time.Time, r io.Reader) (string, error) {
	ms := now.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return "", errors.New("pipe: time out of range for a UUIDv7")
	}
	var b [16]byte
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	if _, err := io.ReadFull(r, b[6:]); err != nil {
		return "", fmt.Errorf("pipe: read random bits: %w", err)
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Code is a snake_case error code. The values are the codes of
// internal/proto/codes.go (this kernel package cannot import proto).
type Code string

// The codes the broker returns.
const (
	CodeMessageTooLarge    Code = "message_too_large"
	CodePipeRateLimited    Code = "pipe_rate_limited"
	CodePipeBudgetExceeded Code = "pipe_budget_exceeded"
	CodeInvalidArgument    Code = "invalid_argument"
	CodeNotFound           Code = "not_found"
	CodeInternal           Code = "internal"
)

// Error is a broker rejection. errors.Is matches on Code only, so the
// sentinels below match every error with that code.
type Error struct {
	Code    Code
	Message string
	// RetryAfter is set for CodePipeRateLimited: the earliest time after
	// which a send of this peer can be accepted. It is advice for the
	// caller; the broker never retries or delays on its own.
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t != nil && t.Code == e.Code
}

// Sentinels for errors.Is checks.
var (
	ErrMessageTooLarge    = &Error{Code: CodeMessageTooLarge}
	ErrPipeRateLimited    = &Error{Code: CodePipeRateLimited}
	ErrPipeBudgetExceeded = &Error{Code: CodePipeBudgetExceeded}
	ErrInvalidArgument    = &Error{Code: CodeInvalidArgument}
	ErrNotFound           = &Error{Code: CodeNotFound}
)

// CodeOf returns the code of the first *Error in err's chain, or CodeInternal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

func newError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
