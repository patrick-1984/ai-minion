package agentdetect

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Screen regions. The state markers live at the bottom of the screen, so only
// the last non-empty lines count: older text (a spinner line that scrolled up,
// an earlier dialog) must not decide the state.
const (
	stateWindow  = 15 // non-empty lines read for busy and idle markers
	dialogWindow = 25 // non-empty lines read for a dialog
	questionSpan = 6  // lines above the first option searched for the question
)

var (
	// optionRe matches a numbered option such as "❯ 1. Yes" or "  2) No".
	optionRe = regexp.MustCompile(`^([❯›>●◉▶*]\s+)?(\d{1,2})[.)]\s+(\S.*)$`)
	// questionRe matches the question line above the options of a permission
	// or choice dialog.
	questionRe = regexp.MustCompile(`(?i)(do you want|would you like|allow |proceed\?|apply (this|these)|trust |permission|are you sure)`)
	// shortcutRe matches the lettered answers of a prompt such as
	// "(Y)es/(N)o/(D)on't ask again [Yes]:".
	shortcutRe = regexp.MustCompile(`\(([A-Za-z])\)([^/\[:()]*)`)
	// yesNoRe matches a plain yes/no prompt: "(y/n)", "[Y/n]", "(yes/no)".
	yesNoRe = regexp.MustCompile(`[\[(]\s*[yY](?:es)?\s*/\s*[nN](?:o)?\s*[\])]`)
)

// screenState decides the state of an agent pane from its sanitised screen
// lines and returns the dialog choices when the state is dialog.
//
// Precedence: dialog, then busy, then idle (stable). A screen without any
// marker is unknown, and so is an empty screen: the guards treat unknown as
// "do not deliver without force".
func screenState(a *Agent, scr ops.Screen) (string, []Choice) {
	raw, rawIdx := tail(scr.Lines, dialogWindow, func(s string) string { return strings.TrimRight(s, " \t") })
	if len(raw) == 0 {
		return ops.PaneStateUnknown, nil
	}
	if choices, ok := parseDialog(a, raw); ok {
		return ops.PaneStateDialog, choices
	}
	if matchAny(tailOf(raw, stateWindow), a.Dialog) {
		return ops.PaneStateDialog, rowMenu(a, raw, rawIdx, scr.Spans)
	}
	bottom := tailOf(raw, stateWindow)
	if matchAny(bottom, genericBusy) || matchAny(bottom, a.Busy) {
		return ops.PaneStateBusy, nil
	}
	if matchAny(bottom, genericIdle) || matchAny(bottom, a.Idle) {
		return ops.PaneStateStable, nil
	}
	return ops.PaneStateUnknown, nil
}

// tailOf returns the last n elements of lines.
func tailOf(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// tail returns the last n non-blank lines, each passed through f, and the
// index of each in lines.
func tail(lines []string, n int, f func(string) string) ([]string, []int) {
	var out []string
	var idx []int
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		out = append(out, f(lines[i]))
		idx = append(idx, i)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], idx[i], idx[j], out[j] = out[j], idx[j], idx[i], out[i]
	}
	return out, idx
}

func matchAny(lines []string, res []*regexp.Regexp) bool {
	for _, l := range lines {
		for _, re := range res {
			if re.MatchString(l) {
				return true
			}
		}
	}
	return false
}

// unbox removes the border characters of a boxed line and returns the
// indentation of its content (columns after the left border) and the trimmed
// content.
func unbox(s string) (int, string) {
	s = strings.TrimRight(s, " \t")
	t := strings.TrimLeft(s, " \t")
	t = strings.TrimPrefix(t, "│")
	t = strings.TrimPrefix(t, "┃")
	t = strings.TrimPrefix(t, "|")
	content := strings.TrimLeft(t, " \t")
	indent := len([]rune(t)) - len([]rune(content))
	content = strings.TrimRight(content, " \t")
	content = strings.TrimRight(content, "│┃|")
	return indent, strings.TrimSpace(content)
}

// parseDialog reports whether the lines show a choice dialog and returns its
// options. Three shapes exist: a numbered option list (the permission dialogs
// of claude, codex, gemini, qwen), a lettered prompt on the last line (aider,
// plain yes/no prompts) and, for agents that declare it, a single row of
// options driven by the arrow keys (opencode). Options without an addressable
// key are never returned.
func parseDialog(a *Agent, raw []string) ([]Choice, bool) {
	lines := make([]string, len(raw))
	indents := make([]int, len(raw))
	for i, l := range raw {
		indents[i], lines[i] = unbox(l)
	}
	if choices, ok := numberedDialog(a, lines, indents); ok {
		return choices, true
	}
	return promptDialog(lines[len(lines)-1])
}

// footerSpan is how many non-blank lines below the options may hold the
// footer of the dialog.
const footerSpan = 3

// statusLineRe matches the status line of a running turn: a spinner glyph
// first, or an elapsed-time counter in front of the interrupt hint. Its
// "esc to cancel" is a busy hint, never the footer of an option menu.
var statusLineRe = regexp.MustCompile(`(?i)^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✻✶✳✢·]\s|\b\d+\s*[sm]\b[^\n]{0,20}\b(esc|ctrl\+c)\b|\b(esc|ctrl\+c)\b[^\n]{0,12}\bto (cancel|interrupt|stop),\s*\d`)

// numberedDialog finds the last block of consecutive options numbered from 1
// and accepts it as a dialog when it is accompanied by a footer line just
// below it, or by a selection marker on an option together with a question
// above it. A numbered list in the agent's prose has neither. A label that
// wraps onto the following lines (a narrow pane) stays with its option.
func numberedDialog(a *Agent, lines []string, indents []int) ([]Choice, bool) {
	type opt struct {
		n      int
		label  string
		marker bool
		col    int // column of the number
	}
	var block, best []opt
	bestStart, bestEnd := 0, 0
	start := 0
	flush := func(end int) {
		if len(block) >= 2 {
			best, bestStart, bestEnd = block, start, end
		}
		block = nil
	}
	for i, l := range lines {
		m := optionRe.FindStringSubmatch(l)
		if m == nil {
			if len(block) > 0 && l != "" && indents[i] > block[len(block)-1].col && !isFooter(a, l) {
				block[len(block)-1].label += " " + l // wrapped label
				continue
			}
			flush(i)
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if len(block) == 0 {
			if n != 1 {
				continue
			}
			start = i
		} else if n != block[len(block)-1].n+1 {
			flush(i)
			if n != 1 {
				continue
			}
			start = i
		}
		block = append(block, opt{n: n, label: strings.TrimSpace(m[3]), marker: m[1] != "", col: indents[i] + len([]rune(m[1]))})
	}
	flush(len(lines))
	if best == nil {
		return nil, false
	}

	footer := false
	seen := 0
	for _, l := range lines[bestEnd:] {
		if l == "" {
			continue
		}
		if seen++; seen > footerSpan {
			break
		}
		footer = footer || isFooter(a, l)
	}
	marker := false
	for _, o := range best {
		marker = marker || o.marker
	}
	question := false
	lo := bestStart - questionSpan
	if lo < 0 {
		lo = 0
	}
	for _, l := range lines[lo:bestStart] {
		question = question || questionRe.MatchString(l)
	}
	if !footer && (!marker || !question) {
		return nil, false
	}
	choices := make([]Choice, len(best))
	for i, o := range best {
		choices[i] = Choice{Label: o.label, Keys: []string{strconv.Itoa(o.n)}}
	}
	return choices, true
}

// isFooter reports whether the line is the footer of an option menu. A busy
// status line ("⠙ Working... (esc to cancel, 2s)") is not.
func isFooter(a *Agent, l string) bool {
	if statusLineRe.MatchString(l) {
		return false
	}
	return matchAny([]string{l}, genericDialogFooter) || matchAny([]string{l}, a.DialogFooter)
}

// promptDialog parses a lettered or yes/no prompt on the last line. These
// prompts read a line, so answering is the letter followed by Enter.
func promptDialog(last string) ([]Choice, bool) {
	if ms := shortcutRe.FindAllStringSubmatch(last, -1); len(ms) >= 2 && strings.ContainsAny(last, ":?") {
		choices := make([]Choice, 0, len(ms))
		for _, m := range ms {
			label := strings.TrimSpace(m[1] + m[2])
			choices = append(choices, Choice{Label: label, Keys: []string{strings.ToLower(m[1]), "Enter"}})
		}
		return choices, true
	}
	if yesNoRe.MatchString(last) {
		return []Choice{
			{Label: "Yes", Keys: []string{"y", "Enter"}},
			{Label: "No", Keys: []string{"n", "Enter"}},
		}, true
	}
	return nil, false
}

// menuLabelSplitRe separates the options of a single-row menu.
var menuLabelSplitRe = regexp.MustCompile(`\s{3,}`)

// menuHintRe matches the key hints below a menu ("⇆ select   enter confirm").
var menuHintRe = regexp.MustCompile(`(?i)\b(select|confirm|enter|esc|tab|navigate)\b`)

// rowMenu parses the single-row menu of an agent whose Dialog marker matched
// (opencode: "Allow once    Allow always    Reject"): the first line below the
// marker that splits into at least two short options and is not a key-hint
// line.
//
// The menu is driven by the arrow keys and its current selection is not text:
// it is the option drawn in a different style. The choices are returned only
// when that selection can be read from the style spans of the screen (see
// menuSelection); the keys of option i are then the moves from the selected
// option to i followed by Enter, so each choice confirms exactly the option
// it names whatever the selection was. Assuming the menu sits on its first
// option would confirm a persistent permission for a human who moved the
// selection to "Allow always". When the row or the selection is not found the
// dialog has no choices, and the human answers it.
func rowMenu(a *Agent, raw []string, rawIdx []int, spans []ops.StyleSpan) []Choice {
	first := len(raw) - stateWindow
	if first < 0 {
		first = 0
	}
	lines, idx := raw[first:], rawIdx[first:]
	marker := -1
	for i, l := range lines {
		if matchAny([]string{l}, a.Dialog) {
			marker = i
		}
	}
	if marker < 0 {
		return nil
	}
	for k := marker + 1; k < len(lines); k++ {
		_, text := unbox(lines[k])
		if text == "" || menuHintRe.MatchString(text) {
			continue
		}
		parts := menuLabelSplitRe.Split(text, -1)
		if len(parts) < 2 {
			continue
		}
		short := true
		for _, p := range parts {
			short = short && len([]rune(p)) <= 30
		}
		if !short {
			continue
		}
		sel := menuSelection(lines[k], text, idx[k], spans)
		if sel < 0 || sel >= len(parts) {
			return nil
		}
		choices := make([]Choice, len(parts))
		for i, p := range parts {
			move, n := "Right", i-sel
			if n < 0 {
				move, n = "Left", sel-i
			}
			keys := make([]string, 0, n+1)
			for j := 0; j < n; j++ {
				keys = append(keys, move)
			}
			choices[i] = Choice{Label: p, Keys: append(keys, "Enter")}
		}
		return choices
	}
	return nil
}

// menuStyle is the part of a style span that distinguishes a selected option.
type menuStyle struct {
	fg, bg                 string
	inverse, bold, underln bool
}

func (m menuStyle) plain() bool { return m == menuStyle{} }

// menuSelection returns the index of the selected option of a single-row menu
// or -1 when it cannot be told. line is the row as on screen, text its
// trimmed content (a substring of line), lineIdx the row's index on screen.
// The options are separated by runs of spaces; the style at the first rune of
// each option is compared: the selected option is the only one drawn
// differently from the others (the others share one style), or, in a menu of
// two, the one that is highlighted (inverse video) while the other is plain.
func menuSelection(line, text string, lineIdx int, spans []ops.StyleSpan) int {
	at := strings.Index(line, text)
	if at < 0 {
		return -1
	}
	base := len([]rune(line[:at]))
	starts := []int{0}
	for _, b := range menuLabelSplitRe.FindAllStringIndex(text, -1) {
		starts = append(starts, b[1])
	}
	styles := make([]menuStyle, len(starts))
	for i, bs := range starts {
		styles[i] = styleAt(spans, lineIdx, base+len([]rune(text[:bs])))
	}
	if len(styles) == 2 {
		a, b := styles[0], styles[1]
		switch {
		case a == b:
			return -1
		case b.plain():
			return 0
		case a.plain():
			return 1
		case a.inverse && !b.inverse:
			return 0
		case b.inverse && !a.inverse:
			return 1
		}
		return -1
	}
	// Three or more: the selected option is the one whose style differs from
	// the style that all the others share.
	for i := range styles {
		ref := 0
		if i == 0 {
			ref = 1
		}
		uniform := true
		for j := range styles {
			uniform = uniform && (j == i || styles[j] == styles[ref])
		}
		if uniform && styles[ref] != styles[i] {
			return i
		}
	}
	return -1
}

// styleAt returns the style of the rune at col of line, the zero style when
// no span covers it.
func styleAt(spans []ops.StyleSpan, line, col int) menuStyle {
	for _, sp := range spans {
		if sp.Line == line && sp.Start <= col && col < sp.End {
			return menuStyle{fg: sp.FG, bg: sp.BG, inverse: sp.Inverse, bold: sp.Bold, underln: sp.Underline}
		}
	}
	return menuStyle{}
}
