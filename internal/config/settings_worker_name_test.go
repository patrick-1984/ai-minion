package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShortWorkerNameFitsTheLimit(t *testing.T) {
	long := strings.Repeat("a", 80)
	for _, c := range []struct{ host, want string }{
		{"box", "box"},
		{"Mac-1731350990.local", "Mac-1731350990"},
		{long + ".example.com", long[:MaxNameLength]},
		{"  ", "minion"},
		{"", "minion"},
		{".hidden", ".hidden"},
	} {
		got := shortWorkerName(c.host)
		if got != c.want {
			t.Errorf("shortWorkerName(%q) = %q, want %q", c.host, got, c.want)
		}
		if _, err := validateWorkerName(got); err != nil {
			t.Errorf("shortWorkerName(%q) = %q does not validate: %v", c.host, got, err)
		}
		if utf8.RuneCountInString(got) > MaxNameLength {
			t.Errorf("too long: %q", got)
		}
	}
}
