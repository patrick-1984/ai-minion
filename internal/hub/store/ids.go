package store

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Clock returns the current time. It is a plain function so that tests can
// fix it; nil means [time.Now] wherever a Clock is accepted.
type Clock func() time.Time

// NewID returns a new UUIDv7 in its canonical text form. Identifiers are
// time-ordered, so they index well and sort by creation time (AD-16).
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 fails only when the system random source fails; no
		// identifier can be made safely then.
		panic(fmt.Sprintf("store: cannot generate a UUIDv7: %v", err))
	}
	return id.String()
}

// ParseID reports whether s is a canonical lower-case UUIDv7 text.
func ParseID(s string) error {
	id, err := uuid.Parse(s)
	if err != nil {
		return fmt.Errorf("store: invalid id %q: %w", s, err)
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return fmt.Errorf("store: id %q is not a UUIDv7", s)
	}
	if id.String() != s {
		return fmt.Errorf("store: id %q is not in canonical lower-case form", s)
	}
	return nil
}

// UnixMS converts t to unix milliseconds, the time representation of every
// SQLite column (spine "IDs and time").
func UnixMS(t time.Time) int64 { return t.UnixMilli() }

// FromUnixMS converts unix milliseconds back to a UTC time.
func FromUnixMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// NowMS returns the current time in unix milliseconds.
func NowMS() int64 { return UnixMS(time.Now()) }
