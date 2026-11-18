package agent

import (
	"io"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// consoleRule is the line above and below a console box. The boxes are what a
// person has to act on (a pairing code, a fingerprint to compare), so they
// must stand out from the log lines around them on every console, Windows
// included: plain ASCII, no colour, no box-drawing characters.
const consoleRule = "=============================================================="

// writeConsoleBox writes title and lines between two rules, with an empty
// line before and after so that the box never touches a log line. The title
// and every line pass through proto.SafeText: text from a peer or a
// certificate can never start a line of its own or move the cursor.
func writeConsoleBox(w io.Writer, title string, lines ...string) {
	if w == nil {
		return
	}
	var b strings.Builder
	b.WriteString("\n" + consoleRule + "\n")
	b.WriteString("  " + proto.SafeText(title, 240) + "\n")
	b.WriteString(consoleRule + "\n")
	for _, l := range lines {
		if l == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + proto.SafeText(l, 240) + "\n")
	}
	b.WriteString(consoleRule + "\n\n")
	_, _ = io.WriteString(w, b.String())
}
