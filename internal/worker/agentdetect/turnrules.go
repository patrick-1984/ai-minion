package agentdetect

import (
	"regexp"
	"strings"
	"unicode"
)

// Per-agent screen rules of the turn extractor: where the bottom chrome of a
// screen starts, what an echo of the delivered message looks like, and how a
// reply is rendered. They read rows of text only; where a row is in the
// scrollback and whether it is the echo of this delivery is decided from the
// delivery evidence (turns.go, turnanchor.go), never from these rules.

// bottomEnd returns the index after the last line of the reply region, and
// whether any chrome (not only blank and border rows) was peeled. The
// chrome at the bottom of the screen is peeled by its position, not by words
// that may also occur in an answer:
//
//   - from the bottom up to and including the input prompt: blank and border
//     lines, footer key hints (whole segments only), busy and idle hints and the
//     prompt itself, which is peeled once, so that a quoted "> text" line that
//     ends the reply stays part of it. Agents without a prompt glyph (opencode)
//     have an input box of lines that start with a vertical bar.
//   - an input box whose draft wraps or has several rows: the rows below the
//     prompt row start with a vertical bar but carry no prompt glyph; the whole
//     box up to its prompt row is peeled, so that no part of an unsent draft
//     reaches the reply.
//   - above the prompt: blank and border lines, at most one busy status line
//     (spinner, "esc to interrupt") and the agent's trailer lines.
func (e *TurnExtractor) bottomEnd(lines []string) (end int, chrome bool) {
	end = len(lines)
	promptSeen, statusSeen := false, false
	bare := bareEcho[e.agent.Name]
	for end > 0 {
		l := lines[end-1]
		switch {
		case isBlankOrBorder(l):
		case matchAny([]string{l}, trailers[e.agent.Name]):
			chrome = true
		case !promptSeen && e.isPromptLine(l):
			promptSeen, chrome = true, true
		case !promptSeen && bare && inputBoxRe.MatchString(l):
			for end > 1 && inputBoxRe.MatchString(lines[end-2]) {
				end--
			}
			promptSeen, chrome = true, true
		case !promptSeen && !bare && boxedRowRe.MatchString(l):
			// A continuation row of a boxed draft: the box is peeled up to
			// its prompt row. Boxed rows without a prompt row above them
			// (a table that ends the reply) stay part of the reply.
			p := boxPromptRow(lines[:end])
			if p < 0 {
				return end, chrome
			}
			end = p + 1
			promptSeen, chrome = true, true
		case !promptSeen && isFooterLine(l):
			chrome = true
		case !promptSeen && e.isStatus(l):
			// Below the prompt: idle and busy hints, several may stack up.
			chrome = true
		case !promptSeen && !bare && e.unboxedDraft(lines[:end]) >= 0:
			// A draft of several rows below an unboxed prompt row: the rows
			// are indented under it. The whole input region is peeled.
			end = e.unboxedDraft(lines[:end]) + 1
			promptSeen, chrome = true, true
		case !statusSeen && e.isBusyStatus(l):
			// Above the prompt only the single busy status line is peeled.
			statusSeen, chrome = true, true
		default:
			return end, chrome
		}
		end--
	}
	return end, chrome
}

var (
	// borderRe matches a line made only of box-drawing characters and spaces,
	// the borders of a prompt box and its empty rows.
	borderRe = regexp.MustCompile(`^[\s╭╮╰╯┌┐└┘├┤┬┴┼─━═│┃┏┓┗┛]*$`)
	// promptLineRe matches the line of an input prompt: a prompt glyph, at the
	// start or inside a box, optionally followed by the (placeholder) input.
	promptLineRe = regexp.MustCompile(`^\s*[│┃]?\s*[>❯›](\s.*)?$`)
	// modePromptRe matches the prompt of an agent whose prompt names its mode
	// ("ask> ", "architect> ", plain "> "), optionally followed by the input.
	modePromptRe = regexp.MustCompile(`^\s*[A-Za-z][A-Za-z-]*>(\s.*)?$`)
	// modeGlyphRe is the mode prompt glyph at the start of an echo line.
	modeGlyphRe = regexp.MustCompile(`^[A-Za-z][A-Za-z-]*>(\s|$)`)
	// col0PromptRe matches an unboxed prompt row: the glyph is in the first
	// column. The rows of a reply are indented.
	col0PromptRe = regexp.MustCompile(`^[>❯›](\s.*)?$`)
	// spinnerRe matches a status line that starts with a spinner glyph.
	spinnerRe = regexp.MustCompile(`^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✻✶✳✢]\s`)
	// boxedRowRe matches a line that starts with a vertical box border.
	boxedRowRe = regexp.MustCompile(`^\s*[│┃]`)
	// inputBoxRe matches a line of the input box of an agent without a prompt
	// glyph: it starts with a heavy vertical bar.
	inputBoxRe = regexp.MustCompile(`^\s*┃`)
	// footerSegRe matches one whole key hint or status of a footer line. The
	// segments of a footer are separated by two or more spaces or a middle dot.
	footerSegRe = regexp.MustCompile(`(?i)^(\? for shortcuts|⏎ send|esc( to)? interrupt|ctrl\+p commands|tab agents|no sandbox( \(.*\))?|(\S+ )?\(?\d+% context left\)?)$`)
	footerSplit = regexp.MustCompile(`\s{2,}|\s·\s`)
	// A collapsed tool summary has UI padding, not code indentation.
	collapsedToolRe = regexp.MustCompile(`^\s*… \+\d+ lines? \(ctrl\+r to expand\)$`)
)

// modePrompts lists the agents whose prompt starts with the name of the mode
// (aider: "ask> ", "architect> ").
var modePrompts = map[string]bool{"aider": true}

// bareEcho lists the agents that echo a sent message without a prompt glyph.
// Only for them a plain line can be the echo, and their input box is made of
// lines that start with a vertical bar.
var bareEcho = map[string]bool{"opencode": true}

// trailers are per-agent lines that follow a reply and are not part of it.
var trailers = map[string][]*regexp.Regexp{
	// The usage trailer of aider: "Tokens: 3.1k sent, 210 received. Cost: $0.01
	// message, $0.02 session." (it may wrap before "Cost"). An answer that
	// starts a line with "Cost:" is no trailer.
	"aider": {
		rx(`^\s*Tokens:\s+[\d.,]+[kKmM]?\s+sent\b`),
		rx(`^\s*Cost:\s+\$[\d.,]+\s+message\b`),
	},
}

// markers are the glyphs with which an agent starts a reply paragraph; they are
// replaced by spaces so that the hanging indent of the following lines stays
// consistent and is removed with the common indent.
var markers = map[string]*regexp.Regexp{
	"claude": regexp.MustCompile(`^(\s{0,2})[●⏺]\s`),
	"codex":  regexp.MustCompile(`^(\s{0,2})[•◦]\s`),
	"gemini": regexp.MustCompile(`^(\s{0,2})[✦]\s`),
	"qwen":   regexp.MustCompile(`^(\s{0,2})[✦]\s`),
}

// isPromptLine reports whether a line is the input prompt of the agent.
func (e *TurnExtractor) isPromptLine(l string) bool {
	if modePrompts[e.agent.Name] {
		return modePromptRe.MatchString(l)
	}
	return promptLineRe.MatchString(l)
}

// unboxedDraft returns the index of the unboxed prompt row whose draft ends
// lines, or -1. The last line must be an indented row; going up, only indented
// rows and blank rows may follow the prompt row (a column-0 glyph), and a row
// that starts a reply paragraph (the agent's marker) or a row in the first
// column ends the search: that is reply text, with no draft in sight.
func (e *TurnExtractor) unboxedDraft(lines []string) int {
	re := markers[e.agent.Name]
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if isBlankOrBorder(l) {
			continue
		}
		if col0PromptRe.MatchString(l) {
			if i == len(lines)-1 {
				return -1
			}
			return i
		}
		if l[0] != ' ' && l[0] != '\t' {
			return -1
		}
		if re != nil && re.MatchString(l) {
			return -1
		}
	}
	return -1
}

// boxPromptRow returns the index of the prompt row of the input box that ends
// lines, going up over boxed rows only, or -1 when the run of boxed rows at the
// end holds no prompt row.
func boxPromptRow(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if !boxedRowRe.MatchString(l) {
			return -1
		}
		if promptLineRe.MatchString(l) {
			return i
		}
	}
	return -1
}

func isBlankOrBorder(l string) bool { return borderRe.MatchString(l) }

// isFooterLine reports whether a line is a footer of key hints: at least one of
// its segments is a whole known hint. A sentence of an answer that mentions
// such a phrase is no footer.
func isFooterLine(l string) bool {
	for _, seg := range footerSplit.Split(strings.TrimSpace(l), -1) {
		if footerSegRe.MatchString(strings.TrimSpace(seg)) {
			return true
		}
	}
	return false
}

// isBusyStatus reports whether a line is the busy status line of the agent: a
// spinner or a busy hint.
func (e *TurnExtractor) isBusyStatus(l string) bool {
	ls := []string{l}
	return spinnerRe.MatchString(l) || matchAny(ls, genericBusy) || matchAny(ls, e.agent.Busy)
}

// isStatus reports whether a line below the prompt (or in a screen without a
// prompt) is a status or idle line of the agent: a busy or an idle hint.
func (e *TurnExtractor) isStatus(l string) bool {
	ls := []string{l}
	return e.isBusyStatus(l) || matchAny(ls, genericIdle) || matchAny(ls, e.agent.Idle)
}

// cleanLine returns a line as part of the reply: control characters dropped,
// a box border removed, the reply marker replaced by spaces and the end
// trimmed.
func (e *TurnExtractor) cleanLine(l string) string {
	l = strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, l)
	if t := strings.TrimLeft(l, " "); strings.HasPrefix(t, "│") || strings.HasPrefix(t, "┃") {
		l = strings.TrimRight(boxInner(l), " ")
	}
	if re := markers[e.agent.Name]; re != nil {
		if m := re.FindStringSubmatch(l); m != nil {
			l = m[1] + "  " + l[len(m[0]):]
		}
	}
	return strings.TrimRight(l, " \t")
}

// boxInner removes the vertical border characters around a boxed line and
// keeps the indentation inside the box. A line is boxed when it starts with a
// border: without an opening border a trailing "│" or "┃" belongs to the text
// ("explain │") and stays.
func boxInner(l string) string {
	l = strings.TrimRight(l, " \t")
	t := strings.TrimLeft(l, " \t")
	if !boxedRowRe.MatchString(t) {
		return t
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, "│"), "┃")
	t = strings.TrimSuffix(strings.TrimSuffix(t, "│"), "┃")
	return strings.TrimPrefix(strings.TrimRight(t, " \t"), " ")
}

// stripSpace removes all whitespace: wrapping and indentation never change it.
func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// ellipses are the marks with which an agent cuts a line of the echo short.
var ellipses = []string{"…", "..."}

// trimEllipsis returns t without a trailing ellipsis and whether it had one.
func trimEllipsis(t string) (string, bool) {
	for _, suf := range ellipses {
		if strings.HasSuffix(t, suf) {
			return strings.TrimSpace(strings.TrimSuffix(t, suf)), true
		}
	}
	return t, false
}

// echoFirst returns the first row of an echo without its box border and prompt
// glyph, and whether it had a prompt glyph. The ellipsis is not touched: the
// dots may belong to the message. An agent with a bare echo shows the message
// without a glyph, so a leading ">" there belongs to the message and stays.
func echoFirst(l string, modes, bare bool) (text string, marked bool) {
	t := strings.TrimSpace(boxInner(l))
	if bare {
		return t, false
	}
	if modes {
		if m := modeGlyphRe.FindString(t); m != "" {
			return strings.TrimSpace(t[len(m):]), true
		}
	}
	for _, g := range []string{">", "❯", "›"} {
		if t == g {
			return "", true
		}
		if strings.HasPrefix(t, g+" ") {
			return strings.TrimSpace(t[len(g):]), true
		}
	}
	return t, false
}

// matchEcho reports whether lines[i:] starts with an echo of the delivered
// message (sent, without whitespace) and returns the index after its last row.
//
// The first row must start the message and carry the agent's prompt glyph (or,
// for agents that echo plainly, be the bare text). The echo covers the whole
// message over its rows (it may wrap over several unmarked rows) or ends in a
// visible cut ("..." or an ellipsis). Rows of a boxed echo lose their border
// first. The caller passes only the rows above the bottom chrome, and says
// where the echo must start: the position comes from the delivery evidence, not
// from this function.
//
// Every row is read literally first: dots at the end of a row may belong to the
// message ("wait...", a row of a list), and a row that starts with a prompt
// glyph may be a quoted line of the message ("> quoted"). A row continues the
// message when its text, glyph included, continues it at the position reached.
// An ellipsis is taken as a cut of the agent only when the literal reading does
// not cover the whole message: then the echo ends at the last row that could
// have been cut (the deepest one the message literally continues through).
func matchEcho(lines []string, i int, sent string, bare, modes bool) (end int, ok bool) {
	if i < 0 || i >= len(lines) || sent == "" {
		return 0, false
	}
	text, marked := echoFirst(lines[i], modes, bare)
	if !marked && !bare {
		return 0, false
	}
	lit := stripSpace(text)
	cutText, ell := trimEllipsis(text)
	cutS := stripSpace(cutText)
	// Only a boxed echo loses border characters; in an unboxed one they are
	// part of the message.
	boxed := boxedRowRe.MatchString(lines[i])
	pos, j, cutJ := 0, i+1, -1
	switch {
	case lit != "" && strings.HasPrefix(sent, lit):
		pos = len(lit)
		if ell && cutS != "" {
			cutJ = j
		}
	case ell && cutS != "" && strings.HasPrefix(sent, cutS):
		return j, true // the literal row does not continue the message: a cut
	default:
		return 0, false
	}
	for pos < len(sent) && j < len(lines) {
		t := strings.TrimSpace(lines[j])
		if boxed {
			t = strings.TrimSpace(boxInner(lines[j]))
		}
		full := stripSpace(t)
		if full == "" {
			j++
			continue
		}
		cutT, ell := trimEllipsis(t)
		if strings.HasPrefix(sent[pos:], full) {
			pos += len(full)
			j++
			if ell && stripSpace(cutT) != "" {
				cutJ = j
			}
			continue
		}
		if cs := stripSpace(cutT); ell {
			if cs == "" {
				j++ // a row of nothing but the ellipsis
				continue
			}
			if strings.HasPrefix(sent[pos:], cs) {
				cutJ = j + 1
			}
		}
		break
	}
	switch {
	case pos == len(sent):
		return j, true
	case cutJ >= 0:
		return cutJ, true
	}
	return 0, false
}

// renderReply joins the reply lines: the common indent is removed, the blank
// lines around it are dropped and a reply longer than MaxTurnBytes is cut.
func renderReply(lines []string) string {
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	indent := -1
	for _, l := range lines {
		if l == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	var b strings.Builder
	for i, l := range lines {
		if l != "" {
			l = l[indent:]
			if collapsedToolRe.MatchString(l) {
				l = strings.TrimLeft(l, " \t")
			}
		}
		if b.Len()+len(l)+1 > MaxTurnBytes {
			s := strings.TrimRight(b.String(), "\n")
			return s + "\n" + TruncatedMarker
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}
