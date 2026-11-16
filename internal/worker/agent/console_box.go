package agent

import (
	"io"
	"strings"
)

// consoleRule is the line above and below a console box. The boxes are what a
// person has to act on (a pairing code, a fingerprint to compare), so they
// must stand out from the log lines around them on every console, Windows
// included: plain ASCII, no colour, no box-drawing characters.
const consoleRule = "=============================================================="

// writeConsoleBox writes title and lines between two rules, with an empty
// line before and after so that the box never touches a log line.
func writeConsoleBox(w io.Writer, title string, lines ...string) {
	if w == nil {
		return
	}
	var b strings.Builder
	b.WriteString("\n" + consoleRule + "\n")
	b.WriteString("  " + title + "\n")
	b.WriteString(consoleRule + "\n")
	for _, l := range lines {
		if l == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + l + "\n")
	}
	b.WriteString(consoleRule + "\n\n")
	_, _ = io.WriteString(w, b.String())
}
