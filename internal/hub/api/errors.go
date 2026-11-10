package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Error is an API error: a code from internal/proto/codes.go, an English
// message that never contains secrets or terminal content, and optional
// details. The HTTP status is proto.HTTPStatus(Code).
type Error struct {
	Code    proto.Code
	Message string
	// Details is machine-readable context, for example {"field":"limit"}.
	Details map[string]any
	// RetryAfter, when positive, is sent as the Retry-After header.
	RetryAfter time.Duration
	// challenge adds "WWW-Authenticate: Bearer" (unauthenticated only).
	challenge bool
}

// Error implements error.
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Is makes errors.Is match an *Error or *proto.Error with the same code.
func (e *Error) Is(target error) bool {
	switch t := target.(type) {
	case *Error:
		return t != nil && t.Code == e.Code
	case *proto.Error:
		return t != nil && t.Code == e.Code
	}
	return false
}

// NewError returns an Error with the given code and message.
func NewError(code proto.Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithDetails returns a copy of e with details.
func (e *Error) WithDetails(details map[string]any) *Error {
	c := *e
	c.Details = details
	return &c
}

func invalidArg(field, message string) *Error {
	e := NewError(proto.CodeInvalidArgument, message)
	if field != "" {
		e.Details = map[string]any{"field": field}
	}
	return e
}

// errorBody is the AD-18 envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    proto.Code     `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// Envelope returns the AD-18 body for e: {"error":{"code","message","details"}}
// with details always an object.
func Envelope(e *Error) []byte {
	d := e.Details
	if d == nil {
		d = map[string]any{}
	}
	b, err := json.Marshal(errorBody{Error: errorDetail{Code: e.Code, Message: e.Message, Details: d}})
	if err != nil {
		// Details held a value that cannot be encoded: send the envelope
		// without it rather than no envelope at all.
		b, _ = json.Marshal(errorBody{Error: errorDetail{Code: e.Code, Message: e.Message, Details: map[string]any{}}})
	}
	return b
}

// WriteError writes the envelope of err with the HTTP status of its code.
// Any error is accepted: see [ErrorOf] for the mapping.
func WriteError(w http.ResponseWriter, err error) {
	e := ErrorOf(err)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfterSeconds(e.RetryAfter)))
	}
	if e.challenge {
		h.Set("WWW-Authenticate", `Bearer realm="minion"`)
	}
	w.WriteHeader(proto.HTTPStatus(e.Code))
	_, _ = w.Write(append(Envelope(e), '\n'))
}

// retryAfterSeconds rounds up, so that a client that waits the header value
// is admitted.
func retryAfterSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// internalError is the answer for every failure the caller cannot act on. It
// never carries the cause: causes may contain paths or SQL, and are logged by
// the dispatcher instead.
func internalError() *Error {
	return NewError(proto.CodeInternal, "an internal error occurred")
}

// ErrorOf maps any error to the one *Error that is written. This is the only
// mapping from Go errors to codes in the API:
//
//   - *Error is used as is;
//   - *proto.Error keeps its code, message and details (what authz.Decide
//     returns);
//   - an error that matches ops.ErrInvalidArgument is invalid_argument, with
//     the field when it is an *ops.ArgumentError;
//   - authz.ErrNotFound is not_found;
//   - everything else, including a lookup that is not wired yet, is
//     internal with a fixed message (fail closed, no cause leaks).
func ErrorOf(err error) *Error {
	if err == nil {
		return internalError()
	}
	var ae *Error
	if errors.As(err, &ae) && ae != nil {
		return ae
	}
	var pe *proto.Error
	if errors.As(err, &pe) && pe != nil {
		e := &Error{Code: pe.Code, Message: pe.Message}
		if len(pe.Details) > 0 {
			var d map[string]any
			if json.Unmarshal(pe.Details, &d) == nil {
				e.Details = d
			}
		}
		if !pe.Code.Known() {
			return internalError()
		}
		return e
	}
	if errors.Is(err, ops.ErrInvalidArgument) {
		var arg *ops.ArgumentError
		if errors.As(err, &arg) {
			e := NewError(proto.CodeInvalidArgument, arg.Reason)
			if arg.Field != "" {
				e.Details = map[string]any{"field": arg.Field}
			}
			return e
		}
		return NewError(proto.CodeInvalidArgument, "an argument is invalid")
	}
	if errors.Is(err, authz.ErrNotFound) {
		return NewError(proto.CodeNotFound, "not found")
	}
	return internalError()
}
