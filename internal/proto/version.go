package proto

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a protocol version "major.minor", independent of the product
// semver (AD-4). Its JSON and text form is the string "3.0".
type Version struct {
	Major uint16
	Minor uint16
}

// Current is the protocol version this build speaks. The wire protocol is 3.0:
// the Hub is authenticated through a certified operational key and no longer
// through the pinned key itself, so the major version was bumped (AD-4). A 0.7
// Hub and its Workers upgrade together: negotiation accepts only 3.x, a 2.x
// peer gets proto_incompatible, and there is no fallback and no downgrade.
var Current = Version{Major: 3, Minor: 0}

// ParseVersion parses "major.minor" with two plain decimal numbers.
func ParseVersion(s string) (Version, error) {
	maj, min, ok := strings.Cut(s, ".")
	if !ok {
		return Version{}, NewError(CodeInvalidArgument, fmt.Sprintf("protocol version %q is not major.minor", s))
	}
	a, errA := parseUint16(maj)
	b, errB := parseUint16(min)
	if errA != nil || errB != nil {
		return Version{}, NewError(CodeInvalidArgument, fmt.Sprintf("protocol version %q is not major.minor", s))
	}
	return Version{Major: a, Minor: b}, nil
}

func parseUint16(s string) (uint16, error) {
	if s == "" || s[0] == '+' || s[0] == '-' {
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.ParseUint(s, 10, 16)
	return uint16(n), err
}

// String returns "major.minor".
func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Compare returns -1, 0 or 1 as v is older than, equal to or newer than o.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmp(int(v.Major), int(o.Major))
	default:
		return cmp(int(v.Minor), int(o.Minor))
	}
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// MarshalText implements encoding.TextMarshaler.
func (v Version) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (v *Version) UnmarshalText(b []byte) error {
	p, err := ParseVersion(string(b))
	if err != nil {
		return err
	}
	*v = p
	return nil
}

// Range is the inclusive range of protocol versions a Worker supports. It is
// carried in the hello frame (AD-4).
type Range struct {
	Min Version `json:"proto_min"`
	Max Version `json:"proto_max"`
}

// Validate reports invalid_argument unless Min <= Max.
func (r Range) Validate() error {
	if r.Min.Compare(r.Max) > 0 {
		return NewError(CodeInvalidArgument, fmt.Sprintf("protocol range %s..%s is empty", r.Min, r.Max))
	}
	return nil
}

// Contains reports whether v lies in the range.
func (r Range) Contains(v Version) bool {
	return r.Min.Compare(v) <= 0 && v.Compare(r.Max) <= 0
}

// Supported returns the versions a Hub at version current accepts: current and
// the previous minor of the same major (AD-4), newest first.
func Supported(current Version) []Version {
	if current.Minor == 0 {
		return []Version{current}
	}
	return []Version{current, {Major: current.Major, Minor: current.Minor - 1}}
}

// SupportedRange returns Supported(current) as a range.
func SupportedRange(current Version) Range {
	s := Supported(current)
	return Range{Min: s[len(s)-1], Max: s[0]}
}

// Negotiate picks the highest version in hub that the Worker range contains.
// It returns proto_incompatible when there is none: a major mismatch, or a
// same-major range that lies entirely outside the Hub's window (AD-4, A-10).
// An empty hub set or an invalid worker range is invalid_argument.
func Negotiate(worker Range, hub []Version) (Version, error) {
	if err := worker.Validate(); err != nil {
		return Version{}, err
	}
	if len(hub) == 0 {
		return Version{}, NewError(CodeInvalidArgument, "the Hub supports no protocol version")
	}
	var best Version
	found := false
	for _, v := range hub {
		if worker.Contains(v) && (!found || v.Compare(best) > 0) {
			best, found = v, true
		}
	}
	if found {
		return best, nil
	}
	return Version{}, NewError(CodeProtoIncompatible, fmt.Sprintf(
		"worker protocol range %s..%s does not overlap the Hub's supported versions %s",
		worker.Min, worker.Max, joinVersions(hub)))
}

func joinVersions(vs []Version) string {
	s := make([]string, len(vs))
	for i, v := range vs {
		s[i] = v.String()
	}
	return strings.Join(s, ", ")
}
