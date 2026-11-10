package agentdetect

import (
	"strings"
	"testing"
)

// The probes of Story 7.2 (the 19 screen sequences that the text heuristics of
// branch story/7.2 could not settle), replayed as a pane whose history grows by
// the rows that left the top of the screen. The scroll before each frame is
// part of the data: it is what tmux's counters would show. The expected texts
// are those of story/7.2.

// pf is one frame of a probe: the rows the agent drew in the reply region, the
// number of rows that left through the top just before, and the state.
type pf struct {
	scroll int
	rows   []string
	busy   bool
}

type probeCase struct {
	sent   string
	base   []string
	frames []pf
	want   string
}

type probeOpts struct {
	noScroll bool                     // replay without any scroll: rows are rewritten in place
	before   func(i int, p *fakePane) // runs before frame i is drawn
	pane     func(p *fakePane)        // runs once before the delivery
	agentCfg func(c *TurnConfig)      // extractor configuration
	observe  func(i int, turn *Turn)  // sees every frame's result
	_        struct{}                 // keeps the literal keyed
}

// run replays the probe on a fake pane and returns the last turn and the first
// error.
func (c probeCase) run(t *testing.T, o probeOpts) (*Turn, error) {
	t.Helper()
	var mods []func(*TurnConfig)
	if o.agentCfg != nil {
		mods = append(mods, o.agentCfg)
	}
	d := newDrive(t, "claude", 24, mods...)
	if o.pane != nil {
		o.pane(d.p)
	}
	d.p.setView(claudeView(c.base, ""))
	d.deliver(c.sent)
	var turn *Turn
	for i, f := range c.frames {
		if o.before != nil {
			o.before(i, d.p)
		}
		status := ""
		if f.busy {
			status = claudeBusyLine
		}
		n := f.scroll
		if o.noScroll {
			n = 0
		}
		d.p.step(n, claudeView(f.rows, status))
		tn, err := d.poll()
		if err != nil {
			return nil, err
		}
		if o.observe != nil {
			o.observe(i, tn)
		}
		if tn != nil {
			if i != len(c.frames)-1 {
				t.Fatalf("a turn at frame %d of %d: %+v", i, len(c.frames), tn)
			}
			turn = tn
		}
	}
	return turn, nil
}

func (c probeCase) exact(t *testing.T) {
	t.Helper()
	turn, err := c.run(t, probeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if turn == nil || turn.Text != c.want {
		got := "<nil>"
		if turn != nil {
			got = turn.Text
		}
		t.Fatalf("turn=%q\nwant %q", got, c.want)
	}
}

func busy(scroll int, rows ...string) pf   { return pf{scroll: scroll, rows: rows, busy: true} }
func stable(scroll int, rows ...string) pf { return pf{scroll: scroll, rows: rows} }

func join(lines ...string) string { return strings.Join(lines, "\n") }

// probeIDs are the names of the probes, in the order of the story.
var probes = map[string]probeCase{
	// Echo stays at the top of the screen (empty baseline); a line in the
	// middle of the reply is redrawn (Running -> Done).
	"InteriorRedrawEchoAtTop": {
		sent: "fix the bug",
		frames: []pf{
			busy(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Running…", "  ⎿ === RUN   TestParse"),
			busy(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Done", "  ⎿ === RUN   TestParse", "  ⎿ --- FAIL: TestParse (0.00s)", "", "● The test expects four items."),
			stable(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Done", "  ⎿ === RUN   TestParse", "  ⎿ --- FAIL: TestParse (0.00s)", "", "● The test expects four items."),
		},
		want: join("I will look at the failing test first.", "", "Bash(go test ./...)  Done", "⎿ === RUN   TestParse", "⎿ --- FAIL: TestParse (0.00s)", "", "The test expects four items."),
	},
	// Same with a non-empty baseline that has scrolled off so that the echo is
	// at the top.
	"InteriorRedrawEchoAtTopAfterBaselineScroll": {
		sent: "fix the bug",
		base: []string{"> earlier question", "", "● earlier answer that is long enough"},
		frames: []pf{
			busy(2, "● earlier answer that is long enough", "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Running…"),
			busy(1, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Running…", "  ⎿ === RUN   TestParse"),
			busy(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Done", "  ⎿ === RUN   TestParse", "", "● The test expects four items."),
			stable(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Done", "  ⎿ === RUN   TestParse", "", "● The test expects four items."),
		},
		want: join("I will look at the failing test first.", "", "Bash(go test ./...)  Done", "⎿ === RUN   TestParse", "", "The test expects four items."),
	},
	// Quote reaches the top with only one content line visible after it that is
	// a prefix of the reply start (a reply that repeats its opening).
	"QuoteAtTopFollowedByRepeatOfOpening": {
		sent: "hello",
		frames: []pf{
			busy(0, "> hello", "", "● Sure.", "  quoting:"),
			busy(4, "  > hello", "  Sure."),
			stable(0, "  > hello", "  Sure.", "  done"),
		},
		want: "Sure.\nquoting:\n> hello\nSure.\ndone",
	},
	// Quote at the top with nothing after it yet (tiny pane, one content row).
	"QuoteAtTopNothingAfter": {
		sent: "hello",
		frames: []pf{
			busy(0, "> hello", "", "● first paragraph", "  quoting:"),
			busy(3, "  quoting:"),
			busy(1, "  > hello"),
			busy(1, "  final paragraph"),
			stable(0, "  final paragraph"),
		},
		want: "first paragraph\nquoting:\n> hello\nfinal paragraph",
	},
	// Fixed-height screen: the baseline scrolled off so the echo sits at the
	// top; the tool status flips Running -> Done without new lines.
	"2SameHeightRedrawEchoAtTop": {
		sent: "fix the bug",
		base: []string{"> earlier question", "", "● earlier answer that is long enough"},
		frames: []pf{
			busy(2, "● earlier answer that is long enough", "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Running…"),
			busy(1, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Running…", "  ⎿ === RUN   TestParse"),
			stable(0, "> fix the bug", "", "● I will look at the failing test first.", "", "● Bash(go test ./...)  Done", "  ⎿ === RUN   TestParse"),
		},
		want: join("I will look at the failing test first.", "", "Bash(go test ./...)  Done", "⎿ === RUN   TestParse"),
	},
	// Empty baseline (cleared screen), short reply whose last tool line flips.
	"2EmptyBaselineFlip": {
		sent: "run ls",
		frames: []pf{
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  Running…"),
			stable(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt  b.txt"),
		},
		want: "Bash(ls)\n⎿  a.txt  b.txt",
	},
	// A: transient frame that shows only the echo (clear+redraw).
	"P3TransientEchoOnly": {
		sent: "run ls",
		frames: []pf{
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt"),
			busy(0, "> run ls"),
			stable(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt"),
		},
		want: "Bash(ls)\n⎿  a.txt",
	},
	// B: reply shrinks to a prefix (status line removed) then grows.
	"P3ShrinkToPrefixThenGrow": {
		sent: "run ls",
		frames: []pf{
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  Running…"),
			busy(0, "> run ls", "", "● Bash(ls)"),
			stable(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt  b.txt"),
		},
		want: "Bash(ls)\n⎿  a.txt  b.txt",
	},
	// C: first line short and replaced.
	"P3ShortFirstLineReplaced": {
		sent: "hi",
		frames: []pf{
			busy(0, "> hi", "", "● Hm"),
			stable(0, "> hi", "", "● Hello there."),
		},
		want: "Hello there.",
	},
	// D: collapsed tool output.
	"P3Collapse": {
		sent: "run ls",
		frames: []pf{
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt", "     b.txt", "     c.txt"),
			stable(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt", "     … +2 lines (ctrl+r to expand)"),
		},
		want: "Bash(ls)\n⎿  a.txt\n… +2 lines (ctrl+r to expand)",
	},
	// E: a quote that was never seen before jumps to the top; the text after it
	// repeats at least 8 bytes of the opening.
	"P3UnseenQuoteJumpRepeatsOpening": {
		sent: "hello",
		frames: []pf{
			busy(0, "> hello", "", "● Bash(go test ./...)", "  ⎿  ok"),
			busy(4, "  > hello", "  Bash(go test ./...) passed again"),
			stable(0, "  > hello", "  Bash(go test ./...) passed again"),
		},
		want: "Bash(go test ./...)\n⎿  ok\n> hello\nBash(go test ./...) passed again",
	},
	// F: an unseen quote jumps to the top with an unrelated tail (the original
	// blocker shape).
	"P3UnseenQuoteJump": {
		sent: "hello",
		frames: []pf{
			busy(0, "> hello", "", "● first paragraph", "  quoting:"),
			busy(4, "  > hello", "  final paragraph", "  done"),
			stable(0, "  > hello", "  final paragraph", "  done"),
		},
		want: "first paragraph\nquoting:\n> hello\nfinal paragraph\ndone",
	},
	// G: the echo at the top, then the whole reply redraws with a changed
	// opening line.
	"P3EchoTopOpeningGrows": {
		sent: "explain",
		frames: []pf{
			busy(0, "> explain", "", "● The parser reads", "  ⎿  x"),
			stable(0, "> explain", "", "● The parser reads tokens one by one.", "  ⎿  y"),
		},
		want: "The parser reads tokens one by one.\n⎿  y",
	},
	// H: the agent re-wraps at the same width and a status flips.
	"P3EchoTopRewrapAndFlip": {
		sent: "go",
		frames: []pf{
			busy(0, "> go", "", "● Working on it now and", "  then more", "● Bash(make)  Running…"),
			stable(0, "> go", "", "● Working on it now and then", "  more", "● Bash(make)  Done"),
		},
		want: "Working on it now and then\nmore\nBash(make)  Done",
	},
	// I: a known quote at the top, followed by a redraw of what followed it.
	"P3KnownQuoteThenRedrawBelow": {
		sent: "hello",
		frames: []pf{
			busy(0, "> hello", "", "● Here is the quote:", "  > hello", "  Bash(x)  Running…"),
			busy(3, "  > hello", "  Bash(x)  Done", "  end"),
			stable(0, "  > hello", "  Bash(x)  Done", "  end"),
		},
		want: "Here is the quote:\n> hello\nBash(x)  Done\nend",
	},
	// The baseline scrolled off, the echo at the top; the last reply line
	// vanishes for a frame, then output arrives and grows.
	"P4ScrolledBaselineShrinkThenGrow": {
		sent: "fix the bug",
		base: []string{"> earlier question", "", "● earlier answer that is long enough"},
		frames: []pf{
			busy(2, "● earlier answer that is long enough", "> fix the bug", "", "● I will look first.", "", "● Bash(go test ./...)", "  ⎿  Running…"),
			busy(1, "> fix the bug", "", "● I will look first.", "", "● Bash(go test ./...)"),
			busy(0, "> fix the bug", "", "● I will look first.", "", "● Bash(go test ./...)", "  ⎿  ok"),
			busy(0, "> fix the bug", "", "● I will look first.", "", "● Bash(go test ./...)", "  ⎿  ok", "", "● All tests pass."),
			stable(0, "> fix the bug", "", "● I will look first.", "", "● Bash(go test ./...)", "  ⎿  ok", "", "● All tests pass."),
		},
		want: "I will look first.\n\nBash(go test ./...)\n⎿  ok\n\nAll tests pass.",
	},
	// A transient echo-only frame must not poison the rest of the turn.
	"P4TransientEchoOnlyThenGrowth": {
		sent: "run ls",
		frames: []pf{
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt"),
			busy(0, "> run ls", ""),
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt", "", "● Two files."),
			busy(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt", "", "● Two files. Done."),
			stable(0, "> run ls", "", "● Bash(ls)", "  ⎿  a.txt", "", "● Two files. Done."),
		},
		want: "Bash(ls)\n⎿  a.txt\n\nTwo files. Done.",
	},
}

// gradualScroll is the original blocker repro: the quote reaches the top of a
// window that slides one row at a time over the transcript, for a window of h
// content rows.
func gradualScroll(h int) probeCase {
	full := []string{"> hello", "", "● first paragraph", "  quoting:", "  > hello", "  final paragraph", "  done"}
	c := probeCase{sent: "hello", want: "first paragraph\nquoting:\n> hello\nfinal paragraph\ndone"}
	for top := 0; top+h <= len(full); top++ {
		f := pf{scroll: 1, rows: full[top : top+h], busy: true}
		if top == 0 {
			f.scroll = 0
		}
		c.frames = append(c.frames, f)
	}
	c.frames = append(c.frames, pf{rows: full[len(full)-h:]})
	return c
}

func TestProbeInteriorRedrawEchoAtTop(t *testing.T) { probes["InteriorRedrawEchoAtTop"].exact(t) }
func TestProbeInteriorRedrawEchoAtTopAfterBaselineScroll(t *testing.T) {
	probes["InteriorRedrawEchoAtTopAfterBaselineScroll"].exact(t)
}
func TestProbeQuoteAtTopFollowedByRepeatOfOpening(t *testing.T) {
	probes["QuoteAtTopFollowedByRepeatOfOpening"].exact(t)
}
func TestProbeQuoteAtTopNothingAfter(t *testing.T) { probes["QuoteAtTopNothingAfter"].exact(t) }
func TestProbeBlockerGradualScroll(t *testing.T)   { gradualScroll(4).exact(t) }
func TestProbeBlockerGradualScrollH(t *testing.T) {
	for h := 1; h <= 6; h++ {
		h := h
		t.Run(string(rune('0'+h)), func(t *testing.T) { gradualScroll(h).exact(t) })
	}
}
func TestProbe2SameHeightRedrawEchoAtTop(t *testing.T) { probes["2SameHeightRedrawEchoAtTop"].exact(t) }
func TestProbe2EmptyBaselineFlip(t *testing.T)         { probes["2EmptyBaselineFlip"].exact(t) }
func TestP3TransientEchoOnly(t *testing.T)             { probes["P3TransientEchoOnly"].exact(t) }
func TestP3ShrinkToPrefixThenGrow(t *testing.T)        { probes["P3ShrinkToPrefixThenGrow"].exact(t) }
func TestP3ShortFirstLineReplaced(t *testing.T)        { probes["P3ShortFirstLineReplaced"].exact(t) }
func TestP3Collapse(t *testing.T)                      { probes["P3Collapse"].exact(t) }
func TestP3UnseenQuoteJumpRepeatsOpening(t *testing.T) {
	probes["P3UnseenQuoteJumpRepeatsOpening"].exact(t)
}
func TestP3UnseenQuoteJump(t *testing.T)           { probes["P3UnseenQuoteJump"].exact(t) }
func TestP3EchoTopOpeningGrows(t *testing.T)       { probes["P3EchoTopOpeningGrows"].exact(t) }
func TestP3EchoTopRewrapAndFlip(t *testing.T)      { probes["P3EchoTopRewrapAndFlip"].exact(t) }
func TestP3KnownQuoteThenRedrawBelow(t *testing.T) { probes["P3KnownQuoteThenRedrawBelow"].exact(t) }
func TestP4ScrolledBaselineShrinkThenGrow(t *testing.T) {
	probes["P4ScrolledBaselineShrinkThenGrow"].exact(t)
}
func TestP4TransientEchoOnlyThenGrowth(t *testing.T) {
	probes["P4TransientEchoOnlyThenGrowth"].exact(t)
}

// TestProbesAreNineteen keeps the table and the tests in step: the story names
// 19 probes (the two gradual-scroll probes are generated).
func TestProbesAreNineteen(t *testing.T) {
	if got := len(probes) + 2; got != 19 {
		t.Fatalf("%d probes, the story has 19", got)
	}
}

// With a real width change between the frames (not a rewrap at the same
// width) the reply cannot be read by index any more.
func TestProbeRewrapWithAWidthChangeIsResized(t *testing.T) {
	c := probes["P3EchoTopRewrapAndFlip"]
	turn, err := c.run(t, probeOpts{before: func(i int, p *fakePane) {
		if i == 1 {
			p.resizeWidth(60)
		}
	}})
	if turn != nil || reasonOf(err) != ReasonResized {
		t.Fatalf("turn=%+v err=%v, want %s", turn, err, ReasonResized)
	}
}

// The five quote probes replayed without scroll: history_size stays the same, so
// the quote overwrote the echo row in place. The index no longer names the echo.
func TestProbeQuotesWithoutScrollAreEvidenceMismatch(t *testing.T) {
	for _, name := range []string{
		"QuoteAtTopFollowedByRepeatOfOpening", "QuoteAtTopNothingAfter",
		"P3UnseenQuoteJumpRepeatsOpening", "P3UnseenQuoteJump", "P3KnownQuoteThenRedrawBelow",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			turn, err := probes[name].run(t, probeOpts{noScroll: true})
			if turn != nil || reasonOf(err) != ReasonEvidenceMismatch {
				t.Fatalf("turn=%+v err=%v, want %s", turn, err, ReasonEvidenceMismatch)
			}
		})
	}
}

// Whatever a probe says, a raised history_generation in the middle of the reply
// retires it, and so does a new pane_pid.
func TestProbesRetiredByClearAndRespawn(t *testing.T) {
	all := map[string]probeCase{"GradualScroll": gradualScroll(4)}
	for n, c := range probes {
		all[n] = c
	}
	for name, c := range all {
		c := c
		t.Run(name+"/clear", func(t *testing.T) {
			turn, err := c.run(t, probeOpts{before: func(i int, p *fakePane) {
				if i == 1 {
					p.clearHistory()
				}
			}})
			if turn != nil || reasonOf(err) != ReasonHistoryCleared {
				t.Fatalf("turn=%+v err=%v, want %s", turn, err, ReasonHistoryCleared)
			}
		})
		t.Run(name+"/respawn", func(t *testing.T) {
			turn, err := c.run(t, probeOpts{before: func(i int, p *fakePane) {
				if i == 1 {
					p.respawn()
				}
			}})
			if turn != nil || reasonOf(err) != ReasonPaneRespawned {
				t.Fatalf("turn=%+v err=%v, want %s", turn, err, ReasonPaneRespawned)
			}
		})
	}
}

// A window that slides over a long, repetitive reply loses no line and repeats
// none, for every scroll step up to and beyond the height of the view. Rows that
// never were on a screen are in the history, so a large step is no gap.
func TestSlidingWindowKeepsTheWholeReply(t *testing.T) {
	for _, r := range slidingReplies() {
		for _, height := range []int{24, 30, 40} {
			for _, step := range []int{1, 2, 3, 7, height / 2, height - 1, height, height + 1, height + 5, 2 * height, 3*height + 1} {
				turn := slide(t, r.lines, height, step)
				if turn == nil {
					t.Fatalf("%s: height %d step %d: no turn", r.name, height, step)
				}
				if got := nonBlank(turn.Text); strings.Join(got, "\n") != strings.Join(r.want, "\n") {
					t.Fatalf("%s: height %d step %d: %d lines, want %d\ngot  %q\nwant %q", r.name, height, step, len(got), len(r.want), got, r.want)
				}
			}
		}
	}
}

type slidingReply struct {
	name  string
	lines []string // the reply as the agent prints it, first line with its marker
	want  []string // the non-blank lines the turn must hold, in order
}

// slidingReplies are replies that are long and repetitive on purpose: identical
// rows must not make an index line up wrongly.
func slidingReplies() []slidingReply {
	var code []string
	code = append(code, "● Here is the code:", "")
	for i := 0; i < 6; i++ {
		code = append(code,
			"  func f"+string(rune('0'+i))+"() {", "      if err != nil {", "          return err", "      }", "",
			"      return nil", "  }", "")
	}
	code = append(code, "  Done.")
	var log []string
	log = append(log, "● Running the tests:", "")
	for i := 0; i < 25; i++ {
		n := string(rune('0'+i/10)) + string(rune('0'+i%10))
		log = append(log, "  === RUN   TestCase"+n, "  --- PASS: TestCase (0.00s)")
	}
	log = append(log, "", "  PASS")
	mk := func(name string, lines []string) slidingReply {
		r := slidingReply{name: name, lines: lines}
		for _, l := range lines {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "●"))
			if l != "" {
				r.want = append(r.want, l)
			}
		}
		return r
	}
	return []slidingReply{mk("go code blocks", code), mk("go test log", log)}
}

// slide prints a transcript that grows by step rows per frame under a bottom
// chrome in a pane of height content rows plus the chrome, polls after every
// growth, then the stable frame, and returns the turn.
func slide(t *testing.T, reply []string, height, step int) *Turn {
	t.Helper()
	transcript := append([]string{"> show code", ""}, reply...)
	d := newDrive(t, "claude", height+len(claudeView(nil, claudeBusyLine)))
	d.p.setView(claudeView(nil, ""))
	d.deliver("show code")
	return printTranscript(t, d, transcript, step)
}

// printTranscript prints transcript from the first visible row of the pane,
// step rows per frame, with the busy chrome at the bottom; rows that no longer
// fit leave through the top. The extractor polls after every frame and once
// more with the idle chrome; the turn of that last poll is returned.
func printTranscript(t *testing.T, d *drive, transcript []string, step int) *Turn {
	t.Helper()
	d.origin = d.p.top
	n := 0
	for n < len(transcript) {
		n = min(n+step, len(transcript))
		d.draw(transcript, n, claudeBusyLine)
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn while busy (step %d)", step)
		}
	}
	d.draw(transcript, n, "")
	return d.pollOK()
}

func nonBlank(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
