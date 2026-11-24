package ops

import (
	"strings"
	"unicode"
)

// MaxHarnessNameRunes bounds a harness session name (see
// [PaneStateDetail.HarnessName]).
const MaxHarnessNameRunes = 80

// SanitizeHarnessName makes a harness session name safe to keep and display:
// valid UTF-8 without control or format characters (no escape sequences, no
// bidirectional overrides), runs of white space folded into one space, trimmed
// and cut to [MaxHarnessNameRunes] runes.
func SanitizeHarnessName(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), r == unicode.ReplacementChar:
			continue
		}
		if n >= MaxHarnessNameRunes {
			break
		}
		if space {
			if n+1 >= MaxHarnessNameRunes {
				break
			}
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
