package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The protocol version is "major.minor" and independent of the product
// version (AD-4). The kernel rule is that version imports no other internal
// package, so it carries its own small copy of the range arithmetic. It must
// stay equal to Negotiate in internal/proto: the highest version both sides
// support, or incompatible.

// ProtoVersion is a protocol version "major.minor".
type ProtoVersion struct {
	Major, Minor uint16
}

// ParseProtoVersion parses "major.minor" (two plain decimal numbers).
func ParseProtoVersion(s string) (ProtoVersion, error) {
	maj, mn, ok := strings.Cut(s, ".")
	a, errA := parseU16(maj)
	b, errB := parseU16(mn)
	if !ok || errA != nil || errB != nil {
		return ProtoVersion{}, fmt.Errorf("protocol version %q is not major.minor", s)
	}
	return ProtoVersion{Major: a, Minor: b}, nil
}

func parseU16(s string) (uint16, error) {
	if s == "" || s[0] == '+' || s[0] == '-' {
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.ParseUint(s, 10, 16)
	return uint16(n), err
}

// String returns "major.minor".
func (v ProtoVersion) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Compare returns -1, 0 or 1 as v is older than, equal to or newer than o.
func (v ProtoVersion) Compare(o ProtoVersion) int {
	if v.Major != o.Major {
		return cmpUint(uint32(v.Major), uint32(o.Major))
	}
	return cmpUint(uint32(v.Minor), uint32(o.Minor))
}

// ProtoRange is an inclusive range of protocol versions inside one major.
type ProtoRange struct {
	Min, Max ProtoVersion
}

// ParseProtoRange parses "1.0..1.1" (the form of the Hub's proto_min..proto_max)
// or a single version "1.1", and validates the range.
func ParseProtoRange(s string) (ProtoRange, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(s), "..")
	if !found {
		hi = lo
	}
	min, err := ParseProtoVersion(lo)
	if err != nil {
		return ProtoRange{}, err
	}
	max, err := ParseProtoVersion(hi)
	if err != nil {
		return ProtoRange{}, err
	}
	r := ProtoRange{Min: min, Max: max}
	return r, r.Validate()
}

// Validate reports an error unless Min <= Max and both lie in one major.
func (r ProtoRange) Validate() error {
	if r.Min.Major != r.Max.Major {
		return fmt.Errorf("protocol range %s must stay inside one major version", r)
	}
	if r.Min.Compare(r.Max) > 0 {
		return fmt.Errorf("protocol range %s is empty", r)
	}
	return nil
}

// String returns "min..max".
func (r ProtoRange) String() string { return r.Min.String() + ".." + r.Max.String() }

// ErrIncompatible is wrapped by the error [NegotiateRanges] returns when two
// ranges share no version.
var ErrIncompatible = errors.New("incompatible with the running Hub connection")

// IncompatibleError carries the two ranges that do not overlap.
type IncompatibleError struct {
	// New is the range the new version supports, Hub the range the running
	// Hub supports.
	New, Hub ProtoRange
}

// Error implements error.
func (e *IncompatibleError) Error() string {
	reason := "the supported protocol ranges do not overlap"
	if e.New.Min.Major != e.Hub.Min.Major {
		reason = "the protocol major versions differ"
	}
	return fmt.Sprintf("%s: %s (new version %s, Hub %s)", ErrIncompatible.Error(), reason, e.New, e.Hub)
}

// Unwrap lets errors.Is(err, ErrIncompatible) work.
func (e *IncompatibleError) Unwrap() error { return ErrIncompatible }

// NegotiateRanges returns the highest protocol version in both ranges, or an
// [IncompatibleError] when they share none. The Hub range is its current minor
// and the previous minor of its major (AD-4), so a same-major range whose
// minors lie outside that window has no common version (A-10).
func NegotiateRanges(next, hub ProtoRange) (ProtoVersion, error) {
	if err := next.Validate(); err != nil {
		return ProtoVersion{}, err
	}
	if err := hub.Validate(); err != nil {
		return ProtoVersion{}, err
	}
	if next.Min.Major != hub.Min.Major {
		return ProtoVersion{}, &IncompatibleError{New: next, Hub: hub}
	}
	hi := next.Max
	if hub.Max.Compare(hi) < 0 {
		hi = hub.Max
	}
	lo := next.Min
	if hub.Min.Compare(lo) > 0 {
		lo = hub.Min
	}
	if lo.Compare(hi) > 0 {
		return ProtoVersion{}, &IncompatibleError{New: next, Hub: hub}
	}
	return hi, nil
}
