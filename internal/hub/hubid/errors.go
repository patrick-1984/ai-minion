package hubid

import (
	"errors"
	"time"
)

// Code is the stable error code of a service error; the transport layer maps
// it to an HTTP status or a command exit code.
type Code string

// The error codes.
const (
	// CodeConflict: the state does not allow the operation (wrong bootstrap
	// state, an unresolved pending file, a second operation in flight).
	CodeConflict Code = "conflict"
	// CodeInvalidArgument: a rejected argument (too short, mismatch, range).
	CodeInvalidArgument Code = "invalid_argument"
	// CodePassphraseInvalid is the one answer for a wrong passphrase, a
	// damaged or swapped sealed file and an unsupported file (section 10.5).
	CodePassphraseInvalid Code = "passphrase_invalid"
	// CodeRateLimited: a locked scope or a used-up hourly allowance.
	CodeRateLimited Code = "rate_limited"
	// CodeNotDurable: a commit was made but could not be proved durable;
	// repeat the same command to confirm and complete it.
	CodeNotDurable Code = "not_durable"
	// CodeFilesPending: the change is durable and active, but a rename or a
	// directory sync is pending; further identity changes get CodeConflict
	// until a retry succeeds.
	CodeFilesPending Code = "files_pending"
	// CodeFailClosed: start (or recovery) refuses and names the files.
	CodeFailClosed Code = "fail_closed"
	// CodeNotServing: no certificate to serve (state B0 to B2, or expired).
	CodeNotServing Code = "not_serving"
)

// Error is the error type of the service. It never contains a passphrase.
type Error struct {
	Code Code
	Msg  string
	// RetryAt is set for CodeRateLimited: when the lock or the allowance
	// window ends.
	RetryAt time.Time
	// Allowance is true for a used-up allowance (false for a lock).
	Allowance bool
	Err       error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Err != nil {
		return "hubid: " + e.Msg + ": " + e.Err.Error()
	}
	return "hubid: " + e.Msg
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinels below by code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Msg == "" && t.Code == e.Code
}

// Sentinels for errors.Is.
var (
	ErrConflict           = &Error{Code: CodeConflict}
	ErrInvalidArgument    = &Error{Code: CodeInvalidArgument}
	ErrPassphraseInvalid  = &Error{Code: CodePassphraseInvalid}
	ErrRateLimited        = &Error{Code: CodeRateLimited}
	ErrNotDurable         = &Error{Code: CodeNotDurable}
	ErrFilesPending       = &Error{Code: CodeFilesPending}
	ErrFailClosed         = &Error{Code: CodeFailClosed}
	ErrNotServing         = &Error{Code: CodeNotServing}
	errCrashed            = errors.New("hubid: the process crashed (test hook)")
	errPassphraseRejected = "The passphrase was not accepted."
)

// ErrCrash is returned by a test hook to simulate the death of the process at
// a named point: the service stops without any cleanup, exactly as a killed
// process would, and refuses every later call. Tests then start a new Service
// on the same directory and database.
var ErrCrash = errCrashed

func newErr(code Code, msg string) *Error { return &Error{Code: code, Msg: msg} }
