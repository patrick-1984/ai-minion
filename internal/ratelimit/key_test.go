package ratelimit

import (
	"net/netip"
	"strings"
	"testing"
)

func TestIPKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 is /32", "192.0.2.7", "192.0.2.7"},
		{"ipv4 neighbour is distinct", "192.0.2.8", "192.0.2.8"},
		{"ipv6 grouped by /64", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"ipv6 same /64 other host part", "2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"ipv6 other /64", "2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"ipv4-mapped is ipv4", "::ffff:192.0.2.7", "192.0.2.7"},
		{"zone dropped", "fe80::1%eth0", "fe80::/64"},
		{"zero addr", "", "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var addr netip.Addr
			if tc.in != "" {
				addr = netip.MustParseAddr(tc.in)
			}
			if got := IPKey(addr); got != tc.want {
				t.Fatalf("IPKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseRemote(t *testing.T) {
	good := map[string]string{
		"192.0.2.7:4242":        "192.0.2.7",
		"192.0.2.7":             "192.0.2.7",
		"[2001:db8::1]:443":     "2001:db8::1",
		"2001:db8::1":           "2001:db8::1",
		" [::ffff:192.0.2.7]:1": "192.0.2.7",
	}
	for in, want := range good {
		got, err := ParseRemote(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseRemote(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "example.com:80", "not-an-ip", "300.1.1.1:80"} {
		if _, err := ParseRemote(in); err == nil {
			t.Errorf("ParseRemote(%q) succeeded, want error", in)
		}
	}
}

func TestNormalizeKey(t *testing.T) {
	if got := NormalizeKey("  Alice "); got != "alice" {
		t.Fatalf("got %q", got)
	}
	if got := NormalizeKey(""); got != "" {
		t.Fatalf("empty changed to %q", got)
	}
	long := strings.Repeat("A", 5000)
	got := NormalizeKey(long)
	if len(got) > 80 || !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("long key not digested: %q", got)
	}
	if got != NormalizeKey(strings.ToLower(long)) {
		t.Fatal("digest not stable across case")
	}
	if got == NormalizeKey(strings.Repeat("A", 5001)) {
		t.Fatal("distinct long keys collided")
	}
}
