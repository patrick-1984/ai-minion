package identity

// kind classifies an *Error. It is unexported: the wire and HTTP codes are
// declared only in internal/proto/codes.go (AD-2), and the adapter edge maps
// each sentinel below to the proto code with the same meaning.
type kind int

const (
	kindInvalidArgument kind = iota + 1
	kindInviteInvalid
	kindPairingCodeInvalid
	kindUnauthenticated
)

// Error is an identity error. Two errors match under errors.Is when their
// kinds are equal, so the Err* sentinels below can be used to test any error
// returned by this package.
type Error struct {
	kind kind
	msg  string // human readable detail in English; never contains secrets
}

// Error implements error. The text carries no code string and never echoes
// the offending input.
func (e *Error) Error() string { return "identity: " + e.msg }

// Is reports whether target is an *Error of the same kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.kind == e.kind
}

// Sentinels for errors.Is.
var (
	// ErrInvalidArgument matches a malformed key, value or file.
	ErrInvalidArgument = &Error{kind: kindInvalidArgument, msg: "invalid argument"}
	// ErrInviteInvalid matches a malformed invitation key.
	ErrInviteInvalid = &Error{kind: kindInviteInvalid, msg: "invalid invitation key"}
	// ErrPairingCodeInvalid matches a malformed pairing code.
	ErrPairingCodeInvalid = &Error{kind: kindPairingCodeInvalid, msg: "invalid pairing code"}
	// ErrUnauthenticated matches a signature that does not verify.
	ErrUnauthenticated = &Error{kind: kindUnauthenticated, msg: "unauthenticated"}
)

func newErr(k kind, msg string) *Error { return &Error{kind: k, msg: msg} }
