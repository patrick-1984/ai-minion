package logx

import (
	"fmt"
	"log/slog"
)

// Redacted is what a [Secret] renders as, in every output path.
const Redacted = "[redacted]"

// Secret is a string that must never reach a log line or other output. Wrap a
// secret value in it as soon as it is read, and unwrap it with [Secret.Reveal]
// only at the point of use.
//
// It redacts through slog (LogValue), fmt (String, GoString and Format) and
// encoding (MarshalText, so JSON and YAML encoders too).
type Secret string

// LogValue implements [slog.LogValuer].
func (Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// String implements [fmt.Stringer].
func (Secret) String() string { return Redacted }

// GoString implements [fmt.GoStringer] so that %#v does not leak the value.
func (Secret) GoString() string { return Redacted }

// Format implements [fmt.Formatter] so that no verb (%s, %v, %q, %x, %+v)
// prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalText implements [encoding.TextMarshaler].
func (Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Reveal returns the secret value. Call it only where the value is used.
func (s Secret) Reveal() string { return string(s) }
