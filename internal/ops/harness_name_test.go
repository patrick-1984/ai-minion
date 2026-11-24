package ops

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeHarnessName(t *testing.T) {
	for in, want := range map[string]string{
		"fix flaky tests":        "fix flaky tests",
		"  padded\t\tname \n":    "padded name",
		"esc\x1b[31mred\x07":     "esc[31mred",
		"bidi\u202egnirts\u200b": "bidignirts",
		"bad\xffutf8":            "badutf8",
		"":                       "",
		"\x00\x01":               "",
		"\u00dcn\u00efc\u00f6d\u00e9 \u540d\u524d": "\u00dcn\u00efc\u00f6d\u00e9 \u540d\u524d",
	} {
		if got := SanitizeHarnessName(in); got != want {
			t.Errorf("SanitizeHarnessName(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeHarnessName(strings.Repeat("\u540d", 200))
	if utf8.RuneCountInString(long) != MaxHarnessNameRunes {
		t.Errorf("long name has %d runes", utf8.RuneCountInString(long))
	}
	if got := SanitizeHarnessName(strings.Repeat("a", 79) + " b"); got != strings.Repeat("a", 79) {
		t.Errorf("a space at the limit is kept: %q", got)
	}
}
