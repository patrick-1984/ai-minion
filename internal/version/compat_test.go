package version

import (
	"errors"
	"strings"
	"testing"
)

func mustRange(t *testing.T, s string) ProtoRange {
	t.Helper()
	r, err := ParseProtoRange(s)
	if err != nil {
		t.Fatalf("ParseProtoRange(%q): %v", s, err)
	}
	return r
}

func TestParseProtoRange(t *testing.T) {
	for in, want := range map[string]string{"1.0..1.1": "1.0..1.1", "1.2": "1.2..1.2", " 2.0..2.3 ": "2.0..2.3"} {
		if got := mustRange(t, in).String(); got != want {
			t.Errorf("ParseProtoRange(%q) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "1", "1.0..", "..1.0", "1.1..1.0", "1.0..2.0", "a.b", "1.0-1.1", "+1.0", "1.-1", "70000.0"} {
		if _, err := ParseProtoRange(in); err == nil {
			t.Errorf("ParseProtoRange(%q) succeeded", in)
		}
	}
}

// The matrix mirrors Negotiate of internal/proto (Story 1.2, A-10): the Hub
// supports its current minor and N-1 of its major.
func TestNegotiateRanges(t *testing.T) {
	cases := []struct {
		name     string
		next     string
		hub      string
		want     string // highest common version; empty means incompatible
		wantMajr bool   // the message names a major difference
	}{
		{name: "identical single versions", next: "1.0", hub: "1.0", want: "1.0"},
		{name: "new range contains the Hub range", next: "1.0..1.3", hub: "1.1..1.2", want: "1.2"},
		{name: "Hub range contains the new range", next: "1.1..1.1", hub: "1.0..1.1", want: "1.1"},
		{name: "overlap at one end", next: "1.1..1.2", hub: "1.0..1.1", want: "1.1"},
		{name: "overlap at the other end", next: "1.0..1.1", hub: "1.1..1.2", want: "1.1"},
		{name: "new version is one minor ahead, still overlaps", next: "1.1..1.2", hub: "1.1..1.2", want: "1.2"},
		{name: "same major, new range entirely above the Hub window", next: "1.3..1.4", hub: "1.0..1.1", want: ""},
		{name: "same major, new range entirely below the Hub window", next: "1.0..1.1", hub: "1.3..1.4", want: ""},
		{name: "same major, adjacent but disjoint", next: "1.2..1.3", hub: "1.0..1.1", want: ""},
		{name: "new major", next: "2.0", hub: "1.0..1.1", want: "", wantMajr: true},
		{name: "old major", next: "1.0..1.1", hub: "2.0", want: "", wantMajr: true},
		{name: "major 0 minors are minors too", next: "0.5..0.6", hub: "0.1..0.2", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NegotiateRanges(mustRange(t, tc.next), mustRange(t, tc.hub))
			if tc.want != "" {
				if err != nil || got.String() != tc.want {
					t.Fatalf("got %v, %v; want %s", got, err, tc.want)
				}
				return
			}
			var inc *IncompatibleError
			if !errors.Is(err, ErrIncompatible) || !errors.As(err, &inc) {
				t.Fatalf("err = %v, want an IncompatibleError", err)
			}
			if !strings.Contains(err.Error(), "incompatible with the running Hub connection") {
				t.Errorf("message %q lacks the A-10 wording", err)
			}
			if tc.wantMajr != strings.Contains(err.Error(), "major") {
				t.Errorf("message %q: major mention = %v, want %v", err, !tc.wantMajr, tc.wantMajr)
			}
			if inc.New.String() != mustRange(t, tc.next).String() || inc.Hub.String() != mustRange(t, tc.hub).String() {
				t.Errorf("error carries %v / %v", inc.New, inc.Hub)
			}
		})
	}
}

func TestNegotiateRangesRejectsInvalidRanges(t *testing.T) {
	bad := ProtoRange{Min: ProtoVersion{1, 2}, Max: ProtoVersion{1, 1}}
	good := mustRange(t, "1.0..1.1")
	if _, err := NegotiateRanges(bad, good); err == nil || errors.Is(err, ErrIncompatible) {
		t.Errorf("empty new range: err = %v", err)
	}
	if _, err := NegotiateRanges(good, bad); err == nil || errors.Is(err, ErrIncompatible) {
		t.Errorf("empty Hub range: err = %v", err)
	}
}
