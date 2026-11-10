package proto

import (
	"encoding/json"
	"errors"
)

// Frame type names: the value of the "type" field of every frame (AD-3).
const (
	// Handshake frames (AD-6), exchanged as raw frames before the yamux
	// session starts. TypeRevoke can also arrive later over the live
	// channel (AD-9).
	TypeHello        = "hello"
	TypeSig          = "sig"
	TypePakeMsg      = "pake.msg"
	TypePakeConfirm  = "pake.confirm"
	TypeInviteSecret = "invite.secret"
	TypeWelcome      = "welcome"
	TypeRevoke       = "revoke"
	TypeNotEnrolled  = "not_enrolled"
)

// Frame types carried on yamux streams after the handshake (AD-3).
const (
	TypeOpRequest  = "op.request"
	TypeOpProgress = "op.progress"
	TypeOpResult   = "op.result"
	TypeError      = "error"
	TypeEventsSnap = "events.snapshot"
	TypeEvent      = "event"
	// TypeTurnsOpen is the first frame of the Worker's turn channel, an "ev"
	// stream of its own (AM-7). TypeTurn carries one turn from the Worker and
	// TypeTurnAck is the Hub's acknowledgement (AD-23).
	TypeTurnsOpen = "turns.open"
	TypeTurn      = "turn"
	TypeTurnAck   = "turn.ack"
	TypePing      = "ping"
	TypePong      = "pong"
	// TypeShareProbe is the first frame of an op stream that the Hub opens to
	// ask a connected Worker for the share rule it has now (the Hub's internal
	// probe, not an op of the catalog); the Worker answers with an op.result
	// whose result is an ops.ShareProbe.
	TypeShareProbe = "share.probe"
)

// StreamKind names the kind of a yamux stream. The wire form is a prefix of
// exactly two ASCII bytes at the start of the stream (AD-3, A-7).
type StreamKind string

// Stream kinds. The logical name of the second kind is "call"; its wire prefix
// is "ca". The four-byte string "call" is never written.
const (
	// StreamOp is opened by the Hub, one per request.
	StreamOp StreamKind = "op"
	// StreamCall is opened by the Worker, one per upstream request.
	StreamCall StreamKind = "ca"
	// StreamEvents is the one long-lived event stream opened by the Worker.
	StreamEvents StreamKind = "ev"
)

// StreamKindPrefixLen is the length in bytes of every stream kind prefix.
const StreamKindPrefixLen = 2

// Prefix returns the two wire bytes of the kind.
func (k StreamKind) Prefix() []byte { return []byte(k) }

// Name returns the logical name of the kind: "op", "call" or "ev".
func (k StreamKind) Name() string {
	if k == StreamCall {
		return "call"
	}
	return string(k)
}

// ParseStreamKind maps the first bytes of a stream to its kind. Only the first
// StreamKindPrefixLen bytes are read and any further bytes are ignored: the
// bytes "call" parse as StreamCall, and the remaining "ll" would belong to
// the stream payload, not to the kind. ok is false for anything but the three
// known prefixes.
func ParseStreamKind(prefix []byte) (k StreamKind, ok bool) {
	if len(prefix) < StreamKindPrefixLen {
		return "", false
	}
	switch k = StreamKind(prefix[:StreamKindPrefixLen]); k {
	case StreamOp, StreamCall, StreamEvents:
		return k, true
	}
	return "", false
}

// Error is the code/message/details payload {"code","message","details"} used
// in the AD-18 API error envelope {"error":{...}} and carried by ErrorFrame on
// the wire. It has no "type" field and so is not a frame by itself: use Frame
// to get the "error" frame. It implements error, and errors.Is matches on Code
// only, so a sentinel such as ErrFrameTooLarge matches every error with that
// code.
type Error struct {
	Code    Code            `json:"code"`
	Message string          `json:"message,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// NewError returns an Error with the given code and
// message.
func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// ErrorFrame is the wire "error" frame (TypeError): the flat JSON object
// {"type":"error","code","message","details"}. Build it with (*Error).Frame so
// Type is always TypeError.
type ErrorFrame struct {
	Type    string          `json:"type"`
	Code    Code            `json:"code"`
	Message string          `json:"message,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// Frame returns the wire "error" frame for e, with Type set to TypeError.
func (e *Error) Frame() ErrorFrame {
	return ErrorFrame{Type: TypeError, Code: e.Code, Message: e.Message, Details: e.Details}
}

// Err returns the Error carried by f.
func (f ErrorFrame) Err() *Error {
	return &Error{Code: f.Code, Message: f.Message, Details: f.Details}
}

// Error implements the error interface.
//
// The text is safe to print to a terminal or a log: the code and the message
// came from a peer or a remote service as often as from this process, so
// control sequences, bidi controls and newlines are neutralised and the length
// is bounded (see [SafeText]). The Message field itself is not changed.
func (e *Error) Error() string {
	code := SafeText(string(e.Code), maxCodeText)
	if e.Message == "" {
		return code
	}
	return code + ": " + SafeText(e.Message, maxMessageText)
}

// Bounds, in runes, of the code and the message in the text of an Error.
const (
	maxCodeText    = 64
	maxMessageText = 2048
)

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t != nil && t.Code == e.Code
}

// CodeOf returns the code of the first *Error in err's chain, or CodeInternal
// when there is none.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// Sentinels for errors.Is checks against the codec and negotiation errors.
var (
	ErrFrameTooLarge     = &Error{Code: CodeFrameTooLarge}
	ErrProtoIncompatible = &Error{Code: CodeProtoIncompatible}
	ErrInvalidArgument   = &Error{Code: CodeInvalidArgument}
)
