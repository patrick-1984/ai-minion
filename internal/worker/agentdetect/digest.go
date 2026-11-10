package agentdetect

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// dialogDigestTag versions the digest input. A change of the rules below must
// change the tag, so that a digest of one version never matches another.
const dialogDigestTag = "minion-dialog-v3"

// selectionRe matches the selection marker, and the white space after it, in
// front of the number of an option row ("❯ 1. Yes").
var selectionRe = regexp.MustCompile(`^([ \t]*)([❯›>●◉▶*])(\s+)(\d{1,2})([.)])(\s)`)

// DialogDigest returns the digest of the dialog that r classified on scr:
// lowercase hex SHA-256 over the kind, every option with the keys that select
// it, the size of the screen, and EVERY row of the visible screen exactly as it
// was read.
//
// The digest answers one question: is this the dialog the caller reviewed? The
// rule is conservative on purpose: whatever the caller could read is bound, and a
// refusal (a new review) is always cheaper than an approval of a command that
// was not the one read. So nothing about the rows is normalised on a guess:
//   - blank rows, the order and number of rows, leading and trailing white space
//     are content (a blank line ends a here-document, a space after a backslash
//     is not a line continuation, indentation is part of a diff or a script);
//   - no row is left out because of what it looks like. A status line of a
//     running turn is not recognised by its words: a command can contain "1s esc"
//     and a regular expression for chrome is a guess about content. An animated
//     row on the screen while the dialog is open makes the review stale, and the
//     caller reviews again.
//
// The one thing that is not hashed as it is: the selection marker of an option
// row, because the marker moves with the arrow keys and the option does not. It
// is replaced by blanks of its own width only on a row that is provably an option
// of r: the text after the marker is exactly "<number><. or )> <label>" of one of
// r.Choices. A marker-like character in front of anything else stays in the row.
//
// A resize that rewraps the text changes the rows and so the digest. The caller
// must give a screen that is complete: a host that could not read the whole
// visible screen reports it (term.ErrIncomplete) and no digest is issued for it.
// Content scrolled above the visible screen is not part of the review. It returns
// "" when r is not a dialog.
func DialogDigest(r Result, scr ops.Screen) string {
	if r.State != ops.PaneStateDialog {
		return ""
	}
	h := sha256.New()
	put := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	put(dialogDigestTag, r.Kind)
	for _, c := range r.Choices {
		put("choice", strings.TrimSpace(c.Label), strings.Join(c.Keys, "\x1f"))
	}
	put("size", strconv.Itoa(scr.Width), strconv.Itoa(scr.Height), strconv.Itoa(len(scr.Lines)))
	put("screen")
	for _, l := range scr.Lines {
		put(unmarkedOption(l, r.Choices))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// unmarkedOption returns the row with the selection marker replaced by blanks of
// the same width when the row is provably an option of choices, and the row as it
// is otherwise.
func unmarkedOption(l string, choices []Choice) string {
	m := selectionRe.FindStringSubmatchIndex(l)
	if m == nil {
		return l
	}
	// Groups: 1 indent, 2 marker, 3 white space after it, 4 number, 5 "." or
	// ")", 6 one white space; the label is the rest of the row.
	label := strings.TrimSpace(l[m[13]:])
	for _, c := range choices {
		if want := strings.TrimSpace(c.Label); want != "" && want == label {
			markerAndSpace := l[m[4]:m[7]]
			return l[:m[4]] + strings.Repeat(" ", utf8.RuneCountInString(markerAndSpace)) + l[m[7]:]
		}
	}
	return l
}
