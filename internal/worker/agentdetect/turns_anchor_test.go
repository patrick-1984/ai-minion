package agentdetect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// opencode screens: the reply region, then the input box of lines that start
// with a vertical bar, then the footer.
var ocBox = []string{"   ┃                ┃", "   ┃  Ask anything...  ┃", "   ┃                ┃", "", "   tab agents   ctrl+p commands"}

func ocView(content []string, busy bool) []string {
	v := append([]string{}, content...)
	if busy {
		v = append(v, "", "  ⠴ Thinking")
	}
	return append(append(v, ""), ocBox...)
}

// drive1 delivers on a blank or prepared pane and returns the drive.
func start(t *testing.T, agent string, height int, base []string, msg string) *drive {
	t.Helper()
	d := newDrive(t, agent, height)
	d.p.setView(base)
	d.deliver(msg)
	return d
}

// --- opencode: a bare echo, no prompt glyph, in the normal screen ------------

func TestOpencodeBareEchoInTheAlternateScreenIsNoTurn(t *testing.T) {
	d := newDrive(t, "opencode", 24)
	d.p.setView(ocView([]string{"  an older reply"}, false))
	d.p.alt = true // what the real agent reports
	d.deliver("hello")
	d.p.setView(ocView([]string{"  an older reply", "", "  hello", "", "  fine"}, false))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonAlternateScreen)
}

// The reply rows equal the echo row, with the same text and indentation, and
// reach the top of the screen. The echo has left the screen. The old extractor
// took such a row for the echo; the index never does.
func TestOpencodeBareEchoReplyEqualToTheEcho(t *testing.T) {
	d := start(t, "opencode", 24, ocView([]string{"  an older reply"}, false), "hello")
	d.p.step(0, ocView([]string{"  an older reply", "", "  hello", "", "  hello", "  more"}, true))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn while busy: %+v", turn)
	}
	// The older rows and the echo leave through the top; the reply's first row
	// equals the echo row and now is the first row on the screen.
	d.p.step(4, ocView([]string{"  hello", "  more", "  last"}, true))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn while busy: %+v", turn)
	}
	d.p.step(0, ocView([]string{"  hello", "  more", "  last"}, false))
	turn := d.pollOK()
	if turn == nil || turn.Text != "hello\nmore\nlast" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestOpencodeAnswerEqualToTheMessage(t *testing.T) {
	d := start(t, "opencode", 24, ocView([]string{"  old reply"}, false), "hello")
	d.p.step(0, ocView([]string{"  old reply", "", "  hello", "", "  hello"}, false))
	turn := d.pollOK()
	if turn == nil || turn.Text != "hello" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestOpencodeAnswerEqualToTheMessageStreamed(t *testing.T) {
	d := start(t, "opencode", 24, ocView([]string{"  old reply"}, false), "hello")
	d.p.step(0, ocView([]string{"  old reply", "", "  hello", "", "  hello"}, true))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn while busy: %+v", turn)
	}
	d.p.step(0, ocView([]string{"  old reply", "", "  hello", "", "  hello", "  and more"}, false))
	turn := d.pollOK()
	if turn == nil || turn.Text != "hello\nand more" {
		t.Fatalf("turn=%+v", turn)
	}
}

// --- the screen at delivery is never the reply --------------------------------

func TestBaselineEchoIsNotAReply(t *testing.T) {
	cases := []struct {
		name, msg string
		base      []string
		reply     string
	}{
		{"same message", "continue", []string{"> continue", "", "● old answer"}, "new answer"},
		{"older prefix message", "yes please run the tests", []string{"> yes", "", "● old answer"}, "all tests pass"},
		{"same message twice, same answer", "again", []string{"> again", "", "● done"}, "done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := start(t, "claude", 24, claudeView(c.base, ""), c.msg)
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("old reply emitted as a turn: %+v", turn)
			}
			if !d.e.Armed() {
				t.Fatal("extractor disarmed by the unchanged screen")
			}
			// A status line flickers in without a busy classification.
			d.p.setView(claudeView(c.base, "  ✻ Tip: use /help"))
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("turn from an unrelated change: %+v", turn)
			}
			h := append(append([]string{}, c.base...), "", "> "+c.msg, "", "● "+c.reply)
			d.p.setView(claudeView(h, claudeBusyLine))
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("turn while busy: %+v", turn)
			}
			d.p.setView(claudeView(h, ""))
			turn := d.pollOK()
			if turn == nil || turn.Text != c.reply {
				t.Fatalf("turn=%+v, want %q", turn, c.reply)
			}
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("duplicate: %+v", turn)
			}
		})
	}
}

func TestBaselineEchoFastReplyStillAnchors(t *testing.T) {
	// No busy frame between: the new echo and reply appear at once.
	base := []string{"> continue", "", "● old answer"}
	d := start(t, "claude", 24, claudeView(base, ""), "continue")
	d.p.setView(claudeView(append(append([]string{}, base...), "", "> continue", "", "● new answer"), ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "new answer" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestEchoMustCoverTheWholeMessage(t *testing.T) {
	t.Run("a message nothing echoes is no turn", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "Explain the build system in detail")
		d.p.setView(claudeView([]string{"● First part.", "  Explain the build", "  Third part."}, ""))
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
	})
	t.Run("a partial echo does not cover the message", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "Explain the build system in detail")
		d.p.setView(claudeView([]string{"> Explain the build", "", "● First part."}, ""))
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
	})
	t.Run("an echo that ends in a cut covers it", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "Explain the build system in detail")
		d.p.setView(claudeView([]string{"> Explain the build sys…", "", "● First part."}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "First part." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("reply line equal to a short message", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "ok")
		d.p.setView(claudeView([]string{"> ok", "", "● Running step", "  ok", "  done"}, claudeBusyLine))
		d.pollOK()
		d.p.setView(claudeView([]string{"> ok", "", "● Running step", "  ok", "  done"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "Running step\nok\ndone" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("aider answers hello with hello", func(t *testing.T) {
		aider := func(h []string) []string { return append(append([]string{}, h...), "", "> ") }
		d := start(t, "aider", 24, aider(nil), "hello")
		h := []string{"> hello", "", "hello"}
		d.p.setView(append(append([]string{}, h...), "", "Waiting for gpt-5"))
		d.pollOK()
		d.p.setView(aider(h))
		turn := d.pollOK()
		if turn == nil || turn.Text != "hello" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("claude reply that quotes the message", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "hello")
		d.p.setView(claudeView([]string{"> hello", "", "● hello", "  and more"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "hello\nand more" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// --- reply start is positional (Story 7.2, criterion 6) -----------------------

func TestOldEchoAboveNeverAnchors(t *testing.T) {
	// The same message was echoed earlier, and the agent now prints something
	// that is not an echo: the old echo above must not become the anchor.
	base := []string{"> hello", "", "● old reply"}
	d := start(t, "claude", 24, claudeView(base, ""), "hello")
	d.p.setView(claudeView(append(append([]string{}, base...), "", "● something else"), ""))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
}

func TestReplyLineEqualToTheEchoStaysReplyText(t *testing.T) {
	d := start(t, "claude", 24, claudeView(nil, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "", "● x", "  > hello", "  y"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "x\n> hello\ny" {
		t.Fatalf("turn=%+v", turn)
	}
}

// F3: the agent redraws the transcript rows above the input and prints the
// echo over a row that was a reply row: the message also reads as an echo at an
// unchanged row above it, so the start is not known.
func TestRepeatedMessageWithARedrawIsAmbiguous(t *testing.T) {
	base := []string{"> hello", "old reply"}
	d := start(t, "claude", 24, claudeView(base, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "> hello", "new reply"}, ""))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonEchoAmbiguous)
}

// The same message appended at the input row is an append: the new echo is
// where the input was, nothing above it changed.
func TestRepeatedMessageAppendedAnchorsOnTheNewEcho(t *testing.T) {
	base := []string{"> hello", "old reply"}
	d := start(t, "claude", 24, claudeView(base, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "old reply", "> hello", "new reply"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "new reply" {
		t.Fatalf("turn=%+v", turn)
	}
}

// A draft in the prompt is chrome: it is never the echo, one row or several,
// boxed or not.
func TestDraftInThePromptIsNeverTheEcho(t *testing.T) {
	t.Run("claude draft", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "fix it")
		d.p.setView([]string{"● Working on something else", "", "╭──╮", "│ > fix it │", "╰──╯", "  ? for shortcuts"})
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
	})
	t.Run("the pasted text waits in the box", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView([]string{"old"}, ""), "fix it")
		d.p.setView([]string{"old", "", "╭──╮", "│ > fix it │", "╰──╯", "  ? for shortcuts"})
		if turn := d.pollOK(); turn != nil || !d.e.Armed() {
			t.Fatalf("turn=%+v armed=%v: a draft is no reaction yet", turn, d.e.Armed())
		}
		if d.e.anchored {
			t.Fatal("anchored on the input box draft")
		}
		d.p.setView(claudeView([]string{"old", "", "> fix it", "", "● fixed"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "fixed" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("opencode draft", func(t *testing.T) {
		d := start(t, "opencode", 24, ocView([]string{"  old reply"}, false), "fix it")
		draft := []string{"   ┃                ┃", "   ┃  fix it        ┃", "   ┃                ┃", "", "   tab agents   ctrl+p commands"}
		d.p.setView(append([]string{"  old reply", ""}, draft...))
		if turn := d.pollOK(); turn != nil || d.e.anchored {
			t.Fatalf("anchored on the input box draft: turn=%+v", turn)
		}
		h := []string{"  old reply", "", "  fix it", "", "  fixed"}
		d.p.setView(ocView(h, false))
		turn := d.pollOK()
		if turn == nil || turn.Text != "fixed" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

func TestFooterWordsInAnAnswerAreKept(t *testing.T) {
	cases := []struct {
		agent  string
		chrome []string
		reply  []string
		want   string
	}{
		{"codex", []string{"", "› Find and fix a bug in @filename", "", "  ⏎ send   ⌃J newline   ? for shortcuts"},
			[]string{"• The command runs with no sandbox."}, "The command runs with no sandbox."},
		{"codex", []string{"", "› Ask", "", "  100% context left · ? for shortcuts"},
			[]string{"• Step one.", "  Use ctrl+p commands to open the palette.", "  Press ? for shortcuts anywhere."}, "Step one.\nUse ctrl+p commands to open the palette.\nPress ? for shortcuts anywhere."},
		{"opencode", []string{"", "   ┃  Ask anything...  ┃", "", "   tab agents   ctrl+p commands"},
			[]string{"  Open it with ctrl+p commands or tab agents."}, "Open it with ctrl+p commands or tab agents."},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.want[:8], func(t *testing.T) {
			d := start(t, c.agent, 24, nil, "explain")
			echo := "› explain"
			if c.agent == "opencode" {
				echo = "  explain"
			}
			d.p.setView(append(append([]string{echo, ""}, c.reply...), c.chrome...))
			turn := d.pollOK()
			if turn == nil || turn.Text != c.want {
				t.Fatalf("turn=%+v, want %q", turn, c.want)
			}
		})
	}
	// The footer itself is still peeled.
	if !isFooterLine("  ⏎ send   ⌃J newline   ⌃T transcript   ? for shortcuts") ||
		!isFooterLine("~/project   no sandbox (see /docs)   gemini-2.5-pro (99% context left)") ||
		isFooterLine("The command runs with no sandbox.") || isFooterLine("Press ctrl+p commands to start") {
		t.Fatal("isFooterLine")
	}
}

// An unsent draft in the input box never reaches a turn, however many rows it
// spans.
func TestMultilineDraftIsNotPartOfTheTurn(t *testing.T) {
	boxes := map[string][]string{
		"wrapped":                         {"╭──╮", "│ > this is a long draft │", "│ that wraps to next row │", "╰──╯", "  ? for shortcuts"},
		"three rows":                      {"╭──╮", "│ > first row │", "│ second row │", "│ │", "│ fourth row │", "╰──╯", "  ? for shortcuts"},
		"draft equal to message, wrapped": {"╭──╮", "│ > fix │", "│ it │", "╰──╯", "  ? for shortcuts"},
	}
	for name, box := range boxes {
		t.Run(name, func(t *testing.T) {
			d := start(t, "claude", 24, claudeView(nil, ""), "fix it")
			d.p.setView(append([]string{"> fix it", "", "● Done.", ""}, box...))
			turn := d.pollOK()
			if turn == nil || turn.Text != "Done." {
				t.Fatalf("turn=%+v", turn)
			}
		})
	}
	t.Run("draft is never the echo", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "fix it")
		d.p.setView([]string{"● Working on something else", "", "╭──╮", "│ > fix │", "│ it │", "╰──╯", "  ? for shortcuts"})
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
	})
	t.Run("boxed table at the end of a reply is kept", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "table")
		d.p.setView(claudeView([]string{"> table", "", "● Here:", "  │ a │ b │", "  │ 1 │ 2 │"}, ""))
		turn := d.pollOK()
		if turn == nil || !strings.Contains(turn.Text, "Here:") || !strings.Contains(turn.Text, "a │ b") || !strings.Contains(turn.Text, "1 │ 2") {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// The unsent draft of an unboxed prompt (codex), however many rows it spans,
// never reaches a turn; indented reply text that merely looks like a prompt row
// does.
func TestUnboxedMultilineDraftIsNotPartOfTheTurn(t *testing.T) {
	footer := []string{"", "  ⏎ send   ⌃J newline   ⌃T transcript"}
	drafts := map[string][]string{
		"two rows":        {"› my secret draft", "  SECRET second row"},
		"blank row":       {"› first", "", "  SECRET third row"},
		"three rows":      {"› a", "  SECRET b", "  SECRET c"},
		"single row":      {"› my secret draft"},
		"empty":           {"› Ask Codex to do anything"},
		"draft like echo": {"› fix it", "  SECRET it again"},
	}
	for name, draft := range drafts {
		t.Run(name, func(t *testing.T) {
			d := start(t, "codex", 24, []string{"", "› Ask Codex to do anything", "  ⏎ send"}, "fix it")
			d.p.setView(append(append([]string{"› fix it", "", "• Done.", ""}, draft...), footer...))
			turn := d.pollOK()
			if turn == nil || turn.Text != "Done." {
				t.Fatalf("turn=%+v", turn)
			}
		})
	}
	t.Run("reply rows are kept", func(t *testing.T) {
		d := start(t, "codex", 24, []string{"", "› Ask Codex to do anything", "  ⏎ send"}, "fix it")
		d.p.setView([]string{"› fix it", "", "• Result:", "  > a quote", "  last row", "", "› ", "", "  ⏎ send"})
		turn := d.pollOK()
		if turn == nil || turn.Text != "Result:\n> a quote\nlast row" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// Aider names its mode in the prompt: the echo "ask> hello" is an echo, with or
// without a busy frame, and a draft after such a prompt is chrome.
func TestAiderModePrompts(t *testing.T) {
	t.Run("busy then stable", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "ask> "}, "hello")
		d.p.setView([]string{"ask> hello", "", "Hi", "", "Waiting for gpt-5"})
		d.pollOK()
		d.p.setView([]string{"ask> hello", "", "Hi there.", "", "ask> "})
		if turn := d.pollOK(); turn == nil || turn.Text != "Hi there." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("reply between two polls", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "ask> "}, "hello")
		d.p.setView([]string{"ask> hello", "", "Hi there.", "", "ask> "})
		if turn := d.pollOK(); turn == nil || turn.Text != "Hi there." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("architect mode with a draft", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "architect> "}, "plan it")
		d.p.setView([]string{"architect> plan it", "", "A plan.", "", "architect> half typed"})
		// A prompt with a draft is not idle (unknown state): the turn is not
		// complete, but the draft is already chrome.
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn=%+v", turn)
		}
		if turn, err := d.e.Flush(); err != nil || turn == nil || turn.Text != "A plan." {
			t.Fatalf("flush: turn=%+v err=%v", turn, err)
		}
	})
}

// Only the usage trailer of aider is chrome; an answer line that starts with
// "Cost:" or "Tokens:" is kept.
func TestAiderTrailerIsNotAnAnswer(t *testing.T) {
	cases := []struct{ name, last, want string }{
		{"cost line", "Cost: O(n)", "The operation is linear.\nCost: O(n)"},
		{"tokens line", "Tokens: are the units", "The operation is linear.\nTokens: are the units"},
		{"usage trailer", "Tokens: 3.1k sent, 210 received. Cost: $0.01 message, $0.02 session.", "The operation is linear."},
		{"wrapped usage trailer", "Tokens: 3.1k sent, 210 received.\nCost: $0.01 message, $0.02 session.", "The operation is linear."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := start(t, "aider", 24, []string{"old", "", "> "}, "explain")
			h := append([]string{"> explain", "", "The operation is linear."}, strings.Split(c.last, "\n")...)
			d.p.setView(append(h, "", "> "))
			turn := d.pollOK()
			if turn == nil || turn.Text != c.want {
				t.Fatalf("turn=%+v, want %q", turn, c.want)
			}
		})
	}
}

// A verified echo followed by an empty reply ends the delivery with no turn
// and no error: the conversation that follows is not its reply.
func TestEmptyReplyAfterAVerifiedEchoRetiresTheDelivery(t *testing.T) {
	old := []string{"old"}
	d := start(t, "claude", 24, claudeView(old, ""), "/compact")
	withEcho := []string{"old", "", "> /compact"}
	d.p.setView(claudeView(withEcho, claudeBusyLine))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn=%+v", turn)
	}
	d.p.setView(claudeView(withEcho, ""))
	if turn, err := d.poll(); turn != nil || err != nil {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if d.e.Armed() {
		t.Fatal("still armed after an empty completion")
	}
	d.p.setView(claudeView(append(withEcho, "", "> human question", "", "● human answer"), ""))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("unrelated conversation emitted: %+v", turn)
	}
	// Waiting for the first reaction is not a completion.
	d.deliver("do it")
	d.pollOK()
	if !d.e.Armed() {
		t.Fatal("disarmed before the agent reacted")
	}
}

// A command that completes between two polls shows only its stable echo. The
// extractor does not wait for it for ever, and what a human types afterwards is
// not its reply.
func TestEchoOnlyCompletionWithoutABusyFrame(t *testing.T) {
	old := []string{"old"}
	withEcho := []string{"old", "", "> /compact"}
	human := append(append([]string{}, withEcho...), "", "> human question", "", "● human answer")
	t.Run("the next input is not the reply", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(old, ""), "/compact")
		d.p.setView(claudeView(withEcho, ""))
		d.pollOK()
		d.p.setView(claudeView(human, ""))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("a human exchange emitted: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed")
		}
	})
	t.Run("a later exchange is not the reply", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(old, ""), "/compact")
		d.p.setView(claudeView(withEcho, ""))
		for i := 0; i < 5; i++ {
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("turn=%+v", turn)
			}
		}
		if d.e.Armed() {
			t.Fatal("armed for ever after a verified echo without a reply")
		}
		d.p.setView(claudeView(append(withEcho, "", "● human answer"), ""))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("unrelated output emitted: %+v", turn)
		}
	})
	t.Run("a slow start still gets its reply", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(old, ""), "hello")
		echo := []string{"old", "", "> hello"}
		d.p.setView(claudeView(echo, ""))
		d.pollOK()
		d.p.setView(claudeView(append(echo, "", "● hi"), ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "hi" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// --- literal glyphs and borders of the message in its echo ---------------------

func TestEchoKeepsLiteralGlyphsAndBorders(t *testing.T) {
	t.Run("a bare echo of a message that starts with a prompt glyph", func(t *testing.T) {
		d := start(t, "opencode", 24, ocView([]string{"  old"}, false), "> hello")
		d.p.setView(ocView([]string{"  old", "", "  > hello", "", "  fine"}, false))
		turn := d.pollOK()
		if turn == nil || turn.Text != "fine" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("an unboxed echo of a message that ends in a border character", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "explain │")
		d.p.setView(claudeView([]string{"> explain │", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("an unboxed continuation row with border characters", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "a\n│ b │\nc")
		d.p.setView(claudeView([]string{"> a", "  │ b │", "  c", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a boxed echo still loses its border", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "explain │")
		d.p.setView(claudeView([]string{"│ > explain │  │", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// --- state from the same frame (criterion 7) ----------------------------------

func TestBusyFrameWithPartialReplyEmitsNothing(t *testing.T) {
	d := start(t, "claude", 24, claudeView(nil, ""), "go")
	partial := []string{"> go", "", "● first part"}
	d.p.setView(claudeView(partial, claudeBusyLine))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn from a busy frame: %+v", turn)
	}
	// Observe takes the frame and nothing else. A stable read of the same pane
	// a moment before changes nothing: the frame decides.
	d.p.setView(claudeView(partial, claudeBusyLine))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn from a busy frame after a stable one: %+v", turn)
	}
	full := append(append([]string{}, partial...), "  second part")
	d.p.setView(claudeView(full, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "first part\nsecond part" {
		t.Fatalf("turn=%+v", turn)
	}
}

// --- tmux before 3.8 and the counters (criterion 8) ---------------------------

type scenario struct {
	name  string
	limit int
	setup func(p *fakePane)
	// frames draws the pane in steps; the extractor polls after each.
	frames []func(p *fakePane)
	// want38 is the reason with the 3.8 counters, "" for the exact reply.
	want38 string
	reply  string
}

func rowsN(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func scenarios() []scenario {
	return []scenario{
		{
			// The witness row is evicted by 150 rows, and one identical row
			// stands at a former candidate index with history_size 100 again.
			name: "evicted witness", limit: 100,
			setup: func(p *fakePane) { p.push(rowsN("h", 100), claudeView(nil, "")) },
			frames: []func(p *fakePane){func(p *fakePane) {
				hist := append(rowsN("y", 149), "> hello")
				p.push(hist, claudeView([]string{"", "● reply"}, ""))
			}},
			want38: ReasonHistoryTrimmed,
		},
		{
			// A clear followed by a refill to the same size that reproduces the
			// rows that were already read.
			name: "clear and refill", limit: 2000,
			frames: []func(p *fakePane){
				func(p *fakePane) { p.setView(claudeView([]string{"> hello", "", "● r1", "  r2"}, claudeBusyLine)) },
				func(p *fakePane) {
					p.clearHistory()
					p.push([]string{"> hello", "", "● r1", "  r2"}, claudeView([]string{"  r3"}, ""))
				},
			},
			want38: ReasonHistoryCleared,
		},
		{
			// history_size drops from 100 to 95 after five rows at constant
			// height: a trim of limit/10 rows. With the 3.8 counters the turn
			// is exact.
			name: "size drop by a trim", limit: 100,
			setup: func(p *fakePane) { p.push(rowsN("h", 100), claudeView(nil, "")) },
			frames: []func(p *fakePane){
				func(p *fakePane) { p.setView(claudeView([]string{"> hello", "", "● r1"}, claudeBusyLine)) },
				func(p *fakePane) {
					if p.size() != 100 {
						panic("the scenario needs a full history")
					}
					p.push([]string{"> hello", "", "● r1", "  r2", "  r3"}, claudeView([]string{"  r4", "  r5"}, ""))
					if p.size() != 95 {
						panic(fmt.Sprintf("history_size is %d, want 95", p.size()))
					}
				},
			},
			reply: "r1\nr2\nr3\nr4\nr5",
		},
	}
}

func (s scenario) run(t *testing.T, counters bool) (*Turn, error, *drive) { //nolint:staticcheck // test helper keeps the turn, error and drive order
	t.Helper()
	d := newDrive(t, "claude", 24)
	d.p.counters = counters
	d.p.limit = s.limit
	if s.setup != nil {
		s.setup(d.p)
	} else {
		d.p.setView(claudeView(nil, ""))
	}
	d.deliver("hello")
	var turn *Turn
	var err error
	for _, f := range s.frames {
		f(d.p)
		if turn, err = d.poll(); err != nil || turn != nil {
			break
		}
	}
	return turn, err, d
}

func TestBeforeTmux38GivesTmuxTooOld(t *testing.T) {
	for _, s := range scenarios() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			turn, err, d := s.run(t, false)
			wantUnavailable(t, d, turn, err, ReasonTmuxTooOld)
		})
	}
}

func TestWithTmux38CountersTheSameFramesAreExactOrNamed(t *testing.T) {
	for _, s := range scenarios() {
		s := s
		t.Run(s.name, func(t *testing.T) {
			turn, err, d := s.run(t, true)
			if s.want38 != "" {
				wantUnavailable(t, d, turn, err, s.want38)
				return
			}
			if err != nil || turn == nil || turn.Text != s.reply {
				t.Fatalf("turn=%+v err=%v, want %q", turn, err, s.reply)
			}
		})
	}
}

// --- the fail-closed reasons ---------------------------------------------------

func TestTrimPastUnreadRows(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.limit = 100
	d.p.setView(claudeView(nil, ""))
	d.deliver("hello")
	d.p.setView(claudeView([]string{"> hello", "", "● start"}, claudeBusyLine))
	d.pollOK()
	// 300 rows leave through the top before the next frame; 100 survive.
	hist := append(d.p.visible()[:3], rowsN("  row", 297)...)
	d.p.push(hist, claudeView([]string{"  tail"}, claudeBusyLine))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonHistoryTrimmed)
}

// A mature pane whose history is full: rows are trimmed behind the reader, one
// step takes history_size from 100 to 95, and the reply is still exact.
func TestTrimBehindTheReaderIsExact(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.limit = 100
	d.p.push(rowsN("h", 98), claudeView(nil, ""))
	d.deliver("hello")
	transcript := []string{"> hello", "", "● r0"}
	want := []string{"r0"}
	for i := 1; i < 60; i++ {
		transcript = append(transcript, fmt.Sprintf("  r%d", i))
		want = append(want, fmt.Sprintf("r%d", i))
	}
	turn := printTranscript(t, d, transcript, 5)
	if turn == nil || turn.Text != strings.Join(want, "\n") {
		t.Fatalf("turn=%+v", turn)
	}
	if d.p.collected == 0 {
		t.Fatal("the scenario never trimmed")
	}
}

func TestHeightChanges(t *testing.T) {
	T := []string{"> hello", "", "● one", "  two", "  three", "  four", "  five", "  six"}
	const want = "one\ntwo\nthree\nfour\nfive\nsix"
	t.Run("grow and shrink mid reply", func(t *testing.T) {
		d := newDrive(t, "claude", 14)
		d.p.setView(claudeView(nil, ""))
		d.deliver("hello")
		d.draw(T, 4, claudeBusyLine)
		d.pollOK()
		d.p.resizeHeight(20)
		d.draw(T, 6, claudeBusyLine)
		d.pollOK()
		d.p.resizeHeight(10) // rows go into history now
		d.draw(T, 8, claudeBusyLine)
		d.pollOK()
		d.draw(T, 8, "")
		turn := d.pollOK()
		if turn == nil || turn.Text != want {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a round trip between two frames after a trim", func(t *testing.T) {
		d := newDrive(t, "claude", 14)
		d.p.limit = 100
		d.p.push(rowsN("h", 100), claudeView(nil, ""))
		d.p.trim(10) // a trim before delivery: the history is mature
		d.origin = d.p.top
		d.deliver("hello")
		d.draw(T, 4, claudeBusyLine)
		d.pollOK()
		d.p.resizeHeight(19)
		d.p.resizeHeight(14)
		d.draw(T, 8, claudeBusyLine)
		d.pollOK()
		d.draw(T, 8, "")
		turn := d.pollOK()
		if turn == nil || turn.Text != want {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a consumed row that changes when the pane grows", func(t *testing.T) {
		d := newDrive(t, "claude", 10)
		d.p.setView(claudeView(nil, ""))
		d.deliver("hello")
		d.draw(T, 4, claudeBusyLine)
		d.pollOK()
		d.draw(T, 8, claudeBusyLine)
		d.pollOK()
		if d.p.top == 0 {
			t.Fatal("the scenario needs rows in history")
		}
		d.p.resizeHeight(16) // pulls history rows back into the screen
		*d.p.at(d.p.top + 1) = "  CHANGED"
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEvidenceMismatch)
	})
}

// A pane that grows, redraws the rows it got back and shrinks before the next
// poll shows the same counters and the same size (tmux 3.8): only the rows tell
// that a consumed one changed.
func TestHeightRoundTripRewritesAConsumedRow(t *testing.T) {
	T := []string{"> hello", "", "● one", "  two", "  three", "  four", "  five", "  six"}
	d := newDrive(t, "claude", 10)
	d.p.setView(claudeView(nil, ""))
	d.deliver("hello")
	d.draw(T, 4, claudeBusyLine)
	d.pollOK()
	d.draw(T, 8, claudeBusyLine)
	d.pollOK()
	top, collected := d.p.top, d.p.collected
	if top == 0 {
		t.Fatal("the scenario needs rows in history")
	}
	d.p.resizeHeight(16)
	*d.p.at(top - 1) = "  CHANGED"
	d.p.resizeHeight(10)
	if d.p.top != top || d.p.collected != collected {
		t.Fatalf("the round trip must leave the counters alone: top %d->%d", top, d.p.top)
	}
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonEvidenceMismatch)
}

func TestRespawnAndWidth(t *testing.T) {
	for name, c := range map[string]struct {
		do     func(p *fakePane)
		reason string
	}{
		"respawn":            {func(p *fakePane) { p.respawn() }, ReasonPaneRespawned},
		"width":              {func(p *fakePane) { p.resizeWidth(60) }, ReasonResized},
		"width and back":     {func(p *fakePane) { p.resizeWidth(60); p.resizeWidth(100) }, ReasonHistoryCleared},
		"clear history":      {func(p *fakePane) { p.clearHistory() }, ReasonHistoryCleared},
		"scroll region":      {func(p *fakePane) { p.region = 2 }, ReasonScrollRegion},
		"dead pane":          {func(p *fakePane) { p.pid = 0 }, ReasonPaneRespawned},
		"counters withdrawn": {func(p *fakePane) { p.counters = false }, ReasonTmuxTooOld},
	} {
		c := c
		t.Run(name, func(t *testing.T) {
			d := start(t, "claude", 24, claudeView(nil, ""), "hello")
			d.p.setView(claudeView([]string{"> hello", "", "● one"}, claudeBusyLine))
			d.pollOK()
			c.do(d.p)
			turn, err := d.poll()
			wantUnavailable(t, d, turn, err, c.reason)
		})
	}
}

func TestScrollRegionAtDelivery(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.p.region = 2
	d.deliver("hello")
	d.p.region = 0
	d.p.setView(claudeView([]string{"> hello", "", "● one"}, ""))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonScrollRegion)
}

// The alternate screen: on at delivery, no turn. A pager that is entered and
// left in the middle of the reply is skipped, and a stable state is not
// honoured while it is on.
func TestAlternateScreen(t *testing.T) {
	t.Run("on at delivery", func(t *testing.T) {
		d := newDrive(t, "claude", 24)
		d.p.setView(claudeView(nil, ""))
		d.p.alt = true
		d.deliver("hello")
		d.p.setView(claudeView([]string{"> hello", "", "● one"}, ""))
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonAlternateScreen)
	})
	t.Run("a pager in the middle of the reply", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "hello")
		d.p.setView(claudeView([]string{"> hello", "", "● one"}, claudeBusyLine))
		d.pollOK()
		d.p.alt = true
		d.p.setView([]string{"pager line 1", "pager line 2", "  ? for shortcuts"})
		for i := 0; i < 3; i++ {
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("turn read while the alternate screen is on: %+v", turn)
			}
		}
		d.p.alt = false
		d.p.setView(claudeView([]string{"> hello", "", "● one", "  two"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "one\ntwo" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a stable state does not count while it is on", func(t *testing.T) {
		d := newDrive(t, "claude", 24, func(c *TurnConfig) { c.Settle = 2 })
		d.p.setView(claudeView(nil, ""))
		d.deliver("hello")
		d.p.setView(claudeView([]string{"> hello", "", "● one"}, ""))
		d.pollOK() // one stable frame, Settle is 2
		d.p.alt = true
		for i := 0; i < 4; i++ {
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("turn in the alternate screen: %+v", turn)
			}
		}
		d.p.alt = false
		turn := d.pollOK()
		if turn == nil || turn.Text != "one" {
			t.Fatalf("turn=%+v: the second stable frame, now that the screen is back", turn)
		}
	})
}

// Rows above the echo and the echo appear again after it, as when an agent
// reprints its transcript.
func TestTranscriptReprinted(t *testing.T) {
	d := start(t, "claude", 24, claudeView([]string{"> earlier", "● old answer"}, ""), "hello")
	d.p.setView(claudeView([]string{"> earlier", "● old answer", "", "> hello", "", "● r1"}, claudeBusyLine))
	d.pollOK()
	// The screen is cleared into history and the transcript is printed again.
	d.p.push(d.p.visible(), claudeView([]string{"> earlier", "● old answer", "", "> hello", "", "● r1", "  r2"}, claudeBusyLine))
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonTranscriptReprinted)
}

func TestTranscriptWithoutBlankContextIsNotChecked(t *testing.T) {
	// Nothing above the echo: there is nothing to tell a reprint by.
	d := start(t, "claude", 24, claudeView(nil, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "", "● r1"}, claudeBusyLine))
	d.pollOK()
	d.p.step(0, claudeView([]string{"> hello", "", "● r1", "  > hello", "  r2"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "r1\n> hello\nr2" {
		t.Fatalf("turn=%+v", turn)
	}
}

// A frame that does not start where the extractor needs it, or that does not
// match its own evidence, is a caller error: the delivery stays.
func TestMalformedFramesAreCallerErrors(t *testing.T) {
	d := start(t, "claude", 24, claudeView(nil, ""), "hello")
	f := d.p.frame(d.e.Need())
	bad := f
	bad.From++
	if _, err := d.e.Observe(bad); err == nil || errors.Is(err, ErrTurnUnavailable) {
		t.Fatalf("a frame at the wrong row: %v", err)
	}
	bad = f
	bad.Rows = bad.Rows[1:]
	if _, err := d.e.Observe(bad); err == nil || errors.Is(err, ErrTurnUnavailable) {
		t.Fatalf("a frame with too few rows: %v", err)
	}
	if !d.e.Armed() {
		t.Fatal("a malformed frame disarmed the extractor")
	}
}

func TestZeroMarkIsNoEvidence(t *testing.T) {
	e := newEx(t, "claude")
	e.Delivered("hello", term.DeliveryMark{})
	_, err := e.Observe(term.Frame{})
	if reasonOf(err) != ReasonNoEvidence || e.Armed() {
		t.Fatalf("err=%v armed=%v", err, e.Armed())
	}
}

// --- a host without the Anchorer port (criterion 9) ---------------------------

type plainHost struct{ pasted []string }

func (h *plainHost) ListSessions(context.Context) ([]term.Session, error) { return nil, nil }
func (h *plainHost) ResolvePane(context.Context, ops.Target) (term.PaneRef, error) {
	return term.PaneRef{ID: "%1", PID: 1, Command: "claude"}, nil
}
func (h *plainHost) Snapshot(context.Context, term.PaneRef) (ops.Screen, error) {
	return ops.Screen{}, nil
}
func (h *plainHost) Capture(context.Context, term.PaneRef, int) (ops.Screen, error) {
	return ops.Screen{}, nil
}
func (h *plainHost) PasteText(_ context.Context, _ term.PaneRef, text string, _ term.PaneRef) error {
	h.pasted = append(h.pasted, text)
	return nil
}
func (h *plainHost) SendKeys(context.Context, term.PaneRef, []string, term.PaneRef) (int, error) {
	return 0, nil
}
func (h *plainHost) CreateSession(context.Context, string, string, []string) (term.Session, error) {
	return term.Session{}, nil
}
func (h *plainHost) SetOption(context.Context, string, string, string) error { return nil }

type anchoredHost struct{ plainHost }

func (anchoredHost) PasteMarked(context.Context, term.PaneRef, string, term.PaneRef) (term.DeliveryMark, error) {
	return term.DeliveryMark{}, nil
}
func (anchoredHost) ReadFrame(context.Context, term.PaneRef, int64) (term.Frame, error) {
	return term.Frame{}, nil
}

func TestHostWithoutAnchorerDeliversButGivesNoTurn(t *testing.T) {
	h := &plainHost{}
	ref := term.PaneRef{ID: "%1", PID: 1, Command: "claude"}
	a, err := AnchorerOf(h)
	if a != nil || reasonOf(err) != ReasonNoEvidence {
		t.Fatalf("anchorer=%v err=%v, want no_evidence", a, err)
	}
	// The pipe delivery still pastes the message.
	if err := h.PasteText(context.Background(), ref, "hello", ref); err != nil || len(h.pasted) != 1 || h.pasted[0] != "hello" {
		t.Fatalf("pasted=%q err=%v", h.pasted, err)
	}
	a, err = AnchorerOf(&anchoredHost{})
	if a == nil || err != nil {
		t.Fatalf("a host with the port: anchorer=%v err=%v", a, err)
	}
}

// --- the boundary of an empty delivery -------------------------------------------

// After a stable frame that holds only the verified echo (a command that
// completed between two polls) the pane has been seen at rest. A busy frame
// that follows may be a human's work: it is no evidence for this delivery, and
// what the next stable frame shows is not its reply.
func TestBusyFrameAfterAnEchoOnlyStableFrameIsNotEvidence(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		old := []string{"old"}
		withEcho := []string{"old", "", "> /compact"}
		human := append(append([]string{}, withEcho...), "", "> human question", "", "● human answer")
		d := start(t, "claude", 24, claudeView(old, ""), "/compact")
		d.p.setView(claudeView(withEcho, ""))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn=%+v", turn)
		}
		d.p.setView(claudeView(append(append([]string{}, withEcho...), "", "> human question"), claudeBusyLine))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn while busy: %+v", turn)
		}
		d.p.setView(claudeView(human, ""))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("a human exchange emitted as the reply: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed: the next exchange would be attributed to this delivery")
		}
		if turn, err := d.e.Flush(); turn != nil || err != nil {
			t.Fatalf("flush: turn=%+v err=%v", turn, err)
		}
	})
	t.Run("opencode, a bare echo that no glyph tells from a reply", func(t *testing.T) {
		d := start(t, "opencode", 24, ocView([]string{"  an older reply"}, false), "/compact")
		withEcho := []string{"  an older reply", "", "  /compact"}
		d.p.setView(ocView(withEcho, false))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn=%+v", turn)
		}
		human := append(append([]string{}, withEcho...), "", "  human question", "", "  human answer")
		d.p.setView(ocView(human[:len(human)-2], true))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn while busy: %+v", turn)
		}
		d.p.setView(ocView(human, false))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("a human exchange emitted as the reply: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed")
		}
	})
	t.Run("opencode, no busy frame between the echo and the human exchange", func(t *testing.T) {
		d := start(t, "opencode", 24, ocView([]string{"  an older reply"}, false), "/compact")
		withEcho := []string{"  an older reply", "", "  /compact"}
		d.p.setView(ocView(withEcho, false))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn=%+v", turn)
		}
		human := append(append([]string{}, withEcho...), "", "  /models", "", "  human command output")
		d.p.setView(ocView(human, false))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("a human exchange emitted as the reply: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed")
		}
		if turn, err := d.e.Flush(); turn != nil || err != nil {
			t.Fatalf("flush: turn=%+v err=%v", turn, err)
		}
	})
	t.Run("settle 2, an echo-only stable frame then a human busy frame", func(t *testing.T) {
		d := newDrive(t, "claude", 24, func(c *TurnConfig) { c.Settle = 2 })
		d.p.setView(claudeView([]string{"old"}, ""))
		d.deliver("/compact")
		withEcho := []string{"old", "", "> /compact"}
		d.p.setView(claudeView(withEcho, ""))
		if turn := d.pollOK(); turn != nil { // one stable frame, Settle is 2
			t.Fatalf("turn=%+v", turn)
		}
		human := append(append([]string{}, withEcho...), "", "> human question")
		d.p.setView(claudeView(human, claudeBusyLine))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn while busy: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed after the human busy frame")
		}
		human = append(human, "", "● human answer")
		d.p.setView(claudeView(human, ""))
		for i := 0; i < 3; i++ {
			if turn := d.pollOK(); turn != nil {
				t.Fatalf("a human exchange emitted as the reply: %+v", turn)
			}
		}
	})
	t.Run("a busy frame before any stable frame still counts", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView([]string{"old"}, ""), "hello")
		echo := []string{"old", "", "> hello"}
		d.p.setView(claudeView(echo, claudeBusyLine))
		d.pollOK()
		d.p.setView(claudeView(append(echo, "", "● hi"), ""))
		if turn := d.pollOK(); turn == nil || turn.Text != "hi" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

// A fast reply that is read for the first time in a stable frame may start with
// a row that looks like the echo of an input: a quoted line, a prompt glyph.
// Nothing shows another input (the pane was never seen at rest with only the
// echo), so it is the reply. Once the pane was seen at rest with only the echo,
// an input-looking row that follows is another input.
func TestFastReplyThatStartsLikeAnInputIsKept(t *testing.T) {
	t.Run("aider, a quotation", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "ask> "}, "hello")
		d.p.setView([]string{"ask> hello", "", "> A quotation", "", "More explanation.", "", "ask> "})
		turn := d.pollOK()
		if turn == nil || turn.Text != "> A quotation\n\nMore explanation." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("aider, a reply that starts with a mode prompt", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "ask> "}, "hello")
		d.p.setView([]string{"ask> hello", "", "ask> is how the mode prompt reads.", "", "More explanation.", "", "ask> "})
		turn := d.pollOK()
		if turn == nil || turn.Text != "ask> is how the mode prompt reads.\n\nMore explanation." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("claude, a blockquote", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView([]string{"old"}, ""), "hello")
		d.p.setView(claudeView([]string{"old", "", "> hello", "", "> A quotation", "", "  More explanation."}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "> A quotation\n\n  More explanation." {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("after a stable frame with only the echo, another input is dropped", func(t *testing.T) {
		d := start(t, "aider", 24, []string{"old", "", "ask> "}, "hello")
		d.p.setView([]string{"ask> hello", "", "ask> "})
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn=%+v", turn)
		}
		d.p.setView([]string{"ask> hello", "", "ask> human question", "", "Human answer.", "", "ask> "})
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("another input emitted: %+v", turn)
		}
		if d.e.Armed() {
			t.Fatal("still armed")
		}
	})
}
