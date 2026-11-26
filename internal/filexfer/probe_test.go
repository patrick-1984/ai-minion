package filexfer

import (
	"errors"
	"testing"
)

func TestInterpretProbe(t *testing.T) {
	for _, c := range []struct {
		out  string
		err  error
		mode ResolveMode
	}{
		{"begin statx\nstatx ok\nbegin openat2\nopenat2 ok\nend\n", nil, ResolveOpenat2},
		{"begin statx\nstatx ok\nbegin openat2\n", errors.New("signal: bad system call"), ResolveWalk},
		{"begin statx\nstatx ok\nbegin openat2\nopenat2 failed: open: ENOSYS\nend\n", nil, ResolveWalk},
		{"begin statx\n", errors.New("signal: bad system call"), ResolveUnavailable},
		{"begin statx\nstatx failed: no mount id\nbegin openat2\nopenat2 failed: open: ENOSYS\nend\n", nil, ResolveUnavailable},
		{"begin statx\nstatx failed: no mount id\nbegin openat2\nopenat2 ok\nend\n", nil, ResolveOpenat2},
		{"", errors.New("exec: not found"), ResolveUnavailable},
		{"", nil, ResolveUnavailable},
	} {
		r := interpretProbe(c.out, c.err)
		if r.Mode != c.mode {
			t.Errorf("%q: %+v, want %v", c.out, r, c.mode)
		}
		if r.Mode == ResolveUnavailable && r.Reason == "" {
			t.Errorf("%q: no reason", c.out)
		}
	}
}

func TestResolveModeString(t *testing.T) {
	for m, s := range map[ResolveMode]string{ResolveDefault: "default", ResolveOpenat2: "openat2", ResolveWalk: "walk", ResolveUnavailable: "unavailable", 9: "invalid"} {
		if m.String() != s {
			t.Errorf("%d: %q", m, m.String())
		}
	}
}
