package version

import (
	"errors"
	"testing"
)

func TestParseVersion(t *testing.T) {
	ok := map[string]SemVer{
		"v1.4.2":      {1, 4, 2, ""},
		"1.4.2":       {1, 4, 2, ""},
		"v0.1.0":      {0, 1, 0, ""},
		"v1.0.0-rc.1": {1, 0, 0, "rc.1"},
		"v10.20.30":   {10, 20, 30, ""},
	}
	for in, want := range ok {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "dev", "v1", "v1.2", "v1.2.3.4", "v01.2.3", "v1.2.3+build", "v1.2.3-", "va.b.c", "v1.2.-3", "v99999999999.0.0", "latest", " v1.2.3"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) succeeded", in)
		}
	}
	if s := (SemVer{1, 2, 3, "rc.4"}).String(); s != "v1.2.3-rc.4" {
		t.Errorf("String = %q", s)
	}
}

func TestVersionCompare(t *testing.T) {
	order := []string{"v0.9.9", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.1.0", "v2.0.0"}
	for i, a := range order {
		for j, b := range order {
			va, _ := ParseVersion(a)
			vb, _ := ParseVersion(b)
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := va.Compare(vb); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestParseSelector(t *testing.T) {
	cases := []struct {
		in   string
		want Selector
		str  string
	}{
		{"latest", Latest, "latest"},
		{"1", Selector{Kind: SelectMajor, Major: 1}, "1"},
		{"v1", Selector{Kind: SelectMajor, Major: 1}, "1"},
		{"1.4", Selector{Kind: SelectMinor, Major: 1, Minor: 4}, "1.4"},
		{"v1.4", Selector{Kind: SelectMinor, Major: 1, Minor: 4}, "1.4"},
		{"v1.4.2", Selector{Kind: SelectExact, Exact: SemVer{1, 4, 2, ""}}, "v1.4.2"},
		{"1.4.2", Selector{Kind: SelectExact, Exact: SemVer{1, 4, 2, ""}}, "v1.4.2"},
		{"v1.5.0-rc.2", Selector{Kind: SelectExact, Exact: SemVer{1, 5, 0, "rc.2"}}, "v1.5.0-rc.2"},
		{" 0.1 ", Selector{Kind: SelectMinor, Major: 0, Minor: 1}, "0.1"},
	}
	for _, tc := range cases {
		got, err := ParseSelector(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseSelector(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
			continue
		}
		if got.String() != tc.str {
			t.Errorf("%q: String = %q, want %q", tc.in, got.String(), tc.str)
		}
	}
	for _, in := range []string{"", "stable", "Latest", "1.x", "1.4.x", "v", "1.4.2.1", "-1", "1.4-rc", "99999999999", "x"} {
		if _, err := ParseSelector(in); err == nil {
			t.Errorf("ParseSelector(%q) succeeded", in)
		}
	}
}

func rel(tag string, flags ...string) Release {
	r := Release{Tag: tag}
	for _, f := range flags {
		switch f {
		case "draft":
			r.Draft = true
		case "pre":
			r.Prerelease = true
		}
	}
	return r
}

func TestResolve(t *testing.T) {
	releases := []Release{
		rel("v1.3.9"), rel("v1.4.0"), rel("v1.4.1"), rel("v1.4.2"),
		rel("v1.4.3", "draft"),    // unpublished
		rel("v1.4.4-rc.1", "pre"), // release candidate
		rel("v1.5.0-rc.1", "pre"),
		rel("v1.5.0-rc.2", "pre"),
		rel("v1.6.0", "pre"), // flagged prerelease although the tag looks final
		rel("v2.0.0-rc.1", "pre"),
		rel("v2.0.0"), rel("v2.1.0"),
		rel("v0.9.0"),
		rel("nightly"), rel("1.4.9"), // not release tags
	}
	cases := []struct {
		sel  string
		want string // empty means ErrNoRelease
	}{
		{"latest", "v2.1.0"},
		{"2", "v2.1.0"},
		{"1", "v1.4.2"},   // rc and flagged prerelease skipped
		{"1.4", "v1.4.2"}, // the draft v1.4.3 and the rc v1.4.4-rc.1 never satisfy it
		{"1.3", "v1.3.9"},
		{"1.5", ""}, // only release candidates exist for 1.5
		{"1.6", ""}, // flagged as prerelease
		{"0", "v0.9.0"},
		{"3", ""},
		{"v1.4.1", "v1.4.1"},           // exact
		{"v1.4.3", ""},                 // exact but still a draft
		{"v1.5.0-rc.2", "v1.5.0-rc.2"}, // an exact -rc may be named explicitly
		{"v1.5.0-rc.3", ""},
		{"v9.9.9", ""},
	}
	for _, tc := range cases {
		sel, err := ParseSelector(tc.sel)
		if err != nil {
			t.Fatal(err)
		}
		got, v, err := sel.Resolve(releases)
		if tc.want == "" {
			if !errors.Is(err, ErrNoRelease) {
				t.Errorf("Resolve(%s) = %q, %v; want ErrNoRelease", tc.sel, got.Tag, err)
			}
			continue
		}
		if err != nil || got.Tag != tc.want || v.String() != tc.want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", tc.sel, got.Tag, err, tc.want)
		}
	}
}

func TestChannelNeverMatchesPrerelease(t *testing.T) {
	rc := SemVer{1, 4, 0, "rc.1"}
	for _, s := range []string{"latest", "1", "1.4"} {
		sel, _ := ParseSelector(s)
		if sel.Matches(rc) {
			t.Errorf("selector %s matches %s", s, rc)
		}
		if !sel.IsChannel() {
			t.Errorf("selector %s is not a channel", s)
		}
	}
	exact, _ := ParseSelector("v1.4.0-rc.1")
	if !exact.Matches(rc) || exact.IsChannel() {
		t.Error("an exact selector must match its own -rc")
	}
}

func TestResolveOrderIndependent(t *testing.T) {
	a := []Release{rel("v1.0.0"), rel("v1.2.0"), rel("v1.1.0")}
	b := []Release{rel("v1.1.0"), rel("v1.0.0"), rel("v1.2.0")}
	ra, _, _ := Latest.Resolve(a)
	rb, _, _ := Latest.Resolve(b)
	if ra.Tag != "v1.2.0" || rb.Tag != "v1.2.0" {
		t.Errorf("got %s and %s", ra.Tag, rb.Tag)
	}
}
