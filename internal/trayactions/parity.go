package trayactions

import (
	"sort"
	"strings"
)

// Markers of the generated parity section in docs/guides/tray.md.
const (
	BeginMarker = "<!-- BEGIN GENERATED PARITY (go run ./scripts/gentraydocs) -->"
	EndMarker   = "<!-- END GENERATED PARITY -->"
)

// CommandLine renders the command of a: the program, its argv and its
// optional argument groups in brackets.
func CommandLine(a Action) string {
	parts := append([]string{string(a.Program)}, a.Argv...)
	for _, o := range a.Optional {
		parts = append(parts, "["+strings.Join(o, " ")+"]")
	}
	return strings.Join(parts, " ")
}

// inputCell describes the standard input, the confirmation and the elevated
// retry of a.
func inputCell(a Action) string {
	var s []string
	if a.Stdin == StdinToken {
		s = append(s, "join token on stdin")
	}
	switch a.Confirm {
	case ConfirmSessionLoss:
		s = append(s, "asks when sessions would end (23, 29)")
	case ConfirmLeave:
		s = append(s, "always asks; also when sessions would end (23, 29)")
	}
	if a.Elevate != ElevateNone && a.Elevate != "" {
		s = append(s, "Windows, on exit 21: elevated "+string(a.Elevate))
	}
	if len(s) == 0 {
		return "—"
	}
	return strings.Join(s, "; ")
}

func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// ParityMarkdown renders the generated parity section: the table of every
// action, the session-loss retry, the closed set of elevated argv and the
// placeholders. It ends with a newline and has no markers.
func ParityMarkdown() string {
	var b strings.Builder
	b.WriteString("| Action id | Tray item | Command | Input and confirmation | Effect | Observed by |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, a := range All {
		b.WriteString("| `" + a.ID + "` | " + cell(a.Menu) + " | `" + cell(CommandLine(a)) + "` | " +
			cell(inputCell(a)) + " | " + string(a.Effect) + " | " + cell(a.Observe) + " |\n")
	}
	b.WriteString("\nWhen a command refuses because sessions would end, the tray names the loss and, after a yes, runs the same command again with `" +
		strings.Join(AcceptLossArgv, " ") + "` (exit 23 named N sessions) or `" + strings.Join(AcceptUnknownLossArgv, " ") +
		"` (exit 29: an unknown number of sessions will end).\n")
	b.WriteString("\nThe only commands a Windows tray runs elevated, on the registered service binary only:\n\n")
	for _, e := range []Elevate{ElevateE1, ElevateE2} {
		b.WriteString("- " + string(e) + ": `minion " + strings.Join(ElevatedArgv(e), " ") + "`\n")
	}
	b.WriteString("\nPlaceholders:\n\n")
	names := make([]string, 0, len(Placeholders))
	for n := range Placeholders {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString("- `{" + n + "}`: " + Placeholders[n] + "\n")
	}
	return b.String()
}

// Splice replaces the text between the markers in doc with the generated
// section. ok is false when the markers are missing or out of order.
func Splice(doc string) (out string, ok bool) {
	i := strings.Index(doc, BeginMarker)
	j := strings.Index(doc, EndMarker)
	if i < 0 || j < i {
		return doc, false
	}
	head := doc[:i+len(BeginMarker)]
	return head + "\n\n" + ParityMarkdown() + "\n" + doc[j:], true
}
