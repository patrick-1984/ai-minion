package version

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Semver is the product version format (AD-4: the protocol version is
// independent). Build metadata ("+...") is not used by releases and is
// rejected.
var semverRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// SemVer is a parsed release version "vX.Y.Z" or "vX.Y.Z-rc.N".
type SemVer struct {
	Major, Minor, Patch uint32
	// Pre is the pre-release part without the dash ("rc.1"), or empty.
	Pre string
}

// ParseVersion parses "vX.Y.Z" with an optional pre-release part. The leading
// "v" is optional on input and always present in [Version.String].
func ParseVersion(s string) (SemVer, error) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return SemVer{}, fmt.Errorf("%q is not a version of the form vX.Y.Z", s)
	}
	var n [3]uint32
	for i := range n {
		v, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil {
			return SemVer{}, fmt.Errorf("%q: version number out of range", s)
		}
		n[i] = uint32(v)
	}
	return SemVer{Major: n[0], Minor: n[1], Patch: n[2], Pre: m[4]}, nil
}

// String returns "vX.Y.Z" or "vX.Y.Z-pre".
func (v SemVer) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// IsPrerelease reports whether v has a pre-release part (an -rc).
func (v SemVer) IsPrerelease() bool { return v.Pre != "" }

// Compare orders versions by semver precedence: -1, 0 or 1 as v is older
// than, equal to or newer than o. A pre-release is older than its release.
func (v SemVer) Compare(o SemVer) int {
	for _, p := range [][2]uint32{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if c := cmpUint(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case v.Pre == "":
		return 1
	case o.Pre == "":
		return -1
	}
	return comparePre(v.Pre, o.Pre)
}

func cmpUint(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePre compares dot-separated pre-release identifiers by semver rules:
// numeric identifiers numerically and below alphanumeric ones, a longer list
// above its prefix.
func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmpUint(uint32(len(as)), uint32(len(bs)))
}

func compareIdent(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// SelectorKind is the form of a [Selector].
type SelectorKind int

const (
	// SelectLatest selects the newest release (channel "latest").
	SelectLatest SelectorKind = iota
	// SelectMajor selects the newest release of one major ("X").
	SelectMajor
	// SelectMinor selects the newest release of one minor ("X.Y").
	SelectMinor
	// SelectExact selects exactly one version ("vX.Y.Z").
	SelectExact
)

// Selector picks a release the way a Docker tag does (R66): "vX.Y.Z" is one
// exact version, "X.Y" and "X" are channels that follow the newest matching
// release, and "latest" follows the newest release. A channel never resolves
// to a pre-release (an -rc); an exact selector may name one explicitly.
type Selector struct {
	Kind  SelectorKind
	Major uint32
	Minor uint32
	// Exact is the version of an exact selector.
	Exact SemVer
}

// Latest is the selector "latest".
var Latest = Selector{Kind: SelectLatest}

var (
	majorRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)$`)
	minorRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// ParseSelector parses "latest", "X", "X.Y" or "vX.Y.Z" (the "v" is optional
// on every form but is how an exact version is normally written).
func ParseSelector(s string) (Selector, error) {
	in := strings.TrimSpace(s)
	if in == "latest" {
		return Latest, nil
	}
	if m := majorRE.FindStringSubmatch(in); m != nil {
		maj, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			return Selector{}, errBadSelector(s)
		}
		return Selector{Kind: SelectMajor, Major: uint32(maj)}, nil
	}
	if m := minorRE.FindStringSubmatch(in); m != nil {
		maj, err1 := strconv.ParseUint(m[1], 10, 32)
		mn, err2 := strconv.ParseUint(m[2], 10, 32)
		if err1 != nil || err2 != nil {
			return Selector{}, errBadSelector(s)
		}
		return Selector{Kind: SelectMinor, Major: uint32(maj), Minor: uint32(mn)}, nil
	}
	if v, err := ParseVersion(in); err == nil {
		return Selector{Kind: SelectExact, Exact: v}, nil
	}
	return Selector{}, errBadSelector(s)
}

func errBadSelector(s string) error {
	return fmt.Errorf("invalid version selector %q: use vX.Y.Z, X.Y, X or latest", s)
}

// String returns the canonical spelling: "latest", "X", "X.Y" or "vX.Y.Z".
func (s Selector) String() string {
	switch s.Kind {
	case SelectMajor:
		return strconv.FormatUint(uint64(s.Major), 10)
	case SelectMinor:
		return fmt.Sprintf("%d.%d", s.Major, s.Minor)
	case SelectExact:
		return s.Exact.String()
	}
	return "latest"
}

// IsChannel reports whether the selector follows newer releases (anything but
// an exact version).
func (s Selector) IsChannel() bool { return s.Kind != SelectExact }

// Matches reports whether v satisfies the selector. A channel never matches a
// pre-release.
func (s Selector) Matches(v SemVer) bool {
	switch s.Kind {
	case SelectExact:
		return v.Compare(s.Exact) == 0
	case SelectMajor:
		return !v.IsPrerelease() && v.Major == s.Major
	case SelectMinor:
		return !v.IsPrerelease() && v.Major == s.Major && v.Minor == s.Minor
	}
	return !v.IsPrerelease()
}

// Release is one published release as the release host lists it.
type Release struct {
	// Tag is the git tag, "vX.Y.Z" or "vX.Y.Z-rc.N".
	Tag string
	// Prerelease is the host's pre-release flag. A flagged release never
	// satisfies a channel, whatever its tag looks like.
	Prerelease bool
	// Draft marks an unpublished release. It is never selected.
	Draft bool
	// Assets are the downloadable files of the release.
	Assets []Asset
}

// Asset is one downloadable release file.
type Asset struct {
	Name string
	URL  string
	Size int64
}

// Asset returns the asset with the given name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ErrNoRelease means no release matches the selector.
var ErrNoRelease = errors.New("no matching release")

// Resolve returns the newest published release that satisfies the selector.
// Drafts and releases with an unparsable tag are ignored; a channel also
// ignores pre-releases, so an -rc never satisfies "1.4", "1" or "latest".
func (s Selector) Resolve(releases []Release) (Release, SemVer, error) {
	var (
		best    Release
		bestVer SemVer
		found   bool
	)
	for _, r := range releases {
		if r.Draft {
			continue
		}
		v, err := ParseVersion(r.Tag)
		if err != nil || v.String() != r.Tag {
			continue
		}
		if s.IsChannel() && r.Prerelease {
			continue
		}
		if !s.Matches(v) {
			continue
		}
		if !found || v.Compare(bestVer) > 0 {
			best, bestVer, found = r, v, true
		}
	}
	if !found {
		return Release{}, SemVer{}, fmt.Errorf("%w for %q", ErrNoRelease, s)
	}
	return best, bestVer, nil
}
