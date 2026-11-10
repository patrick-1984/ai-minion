package proto

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Terminal-safe text. Anything that came from another process (an error
// message from a peer, a remote diagnostic) is untrusted when it is printed to
// an operator's terminal or log: an ESC, a C1 control, a bidi override or a
// newline can clear the screen, rewrite the clipboard (OSC 52), reorder what the
// operator reads or start a line that looks like another message. SafeText and
// SafeLines are the one place that decides what such text may contain.
//
// Both return valid UTF-8 of at most max runes (plus a final ellipsis when text was
// cut) and drop every C0 control, DEL, every C1 control (U+0080 to U+009F) and
// every Unicode format character, which covers the bidi controls, the
// zero-width characters and the invisible tag characters. Invalid UTF-8
// becomes U+FFFD. The escape character is dropped, so a sequence such as an OSC
// string is left as inert, visible text and cannot act. The input is read only
// as far as the bound needs, so a huge message costs a bounded amount of work.

// SafeText returns s as one line: tabs, line and paragraph separators, carriage
// returns and newlines become single spaces.
func SafeText(s string, max int) string { return safeText(s, max, false) }

// SafeLines is SafeText for text that has lines of its own (a multi-line local
// message): newlines and tabs stay, a carriage return is dropped, so the text
// cannot rewrite the start of a line.
func SafeLines(s string, max int) string { return safeText(s, max, true) }

func safeText(s string, max int, lines bool) string {
	if max <= 0 {
		return ""
	}
	scanLimit := max*16 + 1024
	var b strings.Builder
	b.Grow(min(len(s), max*2))
	runes := 0
	for i := 0; i < len(s); {
		if i >= scanLimit || runes >= max {
			b.WriteString("\u2026")
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune(utf8.RuneError)
		case r == '\n' || r == '\t':
			if lines {
				b.WriteRune(r)
			} else {
				writeSpace(&b)
			}
		case r == '\r':
			if !lines {
				writeSpace(&b)
			}
			continue
		case r == 0x2028 || r == 0x2029:
			writeSpace(&b)
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		default:
			b.WriteRune(r)
		}
		runes++
	}
	return b.String()
}

// writeSpace writes one space, never two in a row.
func writeSpace(b *strings.Builder) {
	if s := b.String(); s == "" || s[len(s)-1] != ' ' {
		b.WriteByte(' ')
	}
}
