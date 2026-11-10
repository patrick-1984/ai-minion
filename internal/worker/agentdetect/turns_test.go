package agentdetect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
)

var (
	t0       = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	frameRe  = regexp.MustCompile(`^(\d\d)_(busy|stable)\.txt$`)
	agentOf  = regexp.MustCompile(`^([a-z]+)_`)
	escapeRe = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")
)

func newEx(t *testing.T, agent string, mod ...func(*TurnConfig)) *TurnExtractor {
	t.Helper()
	cfg := TurnConfig{Session: "work", Agent: agent, Now: func() time.Time { return t0 }}
	for _, m := range mod {
		m(&cfg)
	}
	e, err := NewTurnExtractor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// newDrive returns a drive over a fake pane of the given height for agent.
func newDrive(t *testing.T, agent string, height int, mod ...func(*TurnConfig)) *drive {
	t.Helper()
	return &drive{t: t, p: newFakePane(height), e: newEx(t, agent, mod...)}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// reasonOf returns the reason of an ErrTurnUnavailable, or "" when err is not
// one.
func reasonOf(err error) string {
	var ue *UnavailableError
	if errors.Is(err, ErrTurnUnavailable) && errors.As(err, &ue) {
		return ue.Reason
	}
	return ""
}

// wantUnavailable fails unless err retires the delivery for reason and the
// extractor is disarmed.
func wantUnavailable(t *testing.T, d *drive, turn *Turn, err error, reason string) {
	t.Helper()
	if turn != nil || reasonOf(err) != reason {
		t.Fatalf("turn=%+v err=%v, want no turn with reason %s", turn, err, reason)
	}
	if d.e.Armed() {
		t.Fatal("still armed after the delivery was retired")
	}
}

type fixtureEvidence struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Frames []struct {
		File string `json:"file"`
		Top  int    `json:"top"`
	} `json:"frames"`
}

// fixtureOpts changes the evidence a fixture is replayed with.
type fixtureOpts struct {
	alternate bool // alternate_on=1 for the whole replay
	pre38     bool // the evidence of tmux before 3.8
}

// replayFixture replays one testdata/turns directory on a fake pane and returns
// the turns and the first error. The evidence file gives the first visible row
// of every frame: the scroll between two frames is the difference.
func replayFixture(t *testing.T, dir string, ag *Agent, o fixtureOpts) (turns []*Turn, ids int, err error) {
	t.Helper()
	sent := strings.Join(readLines(t, filepath.Join(dir, "sent.txt")), "\n")
	raw, rerr := os.ReadFile(filepath.Join(dir, "evidence.json"))
	if rerr != nil {
		t.Fatalf("no evidence: %v", rerr)
	}
	var ev fixtureEvidence
	if jerr := json.Unmarshal(raw, &ev); jerr != nil {
		t.Fatal(jerr)
	}
	p := newFakePane(ev.Height)
	p.width = ev.Width
	p.counters = !o.pre38
	p.alt = o.alternate
	p.command = ag.Name
	d := &drive{t: t, p: p, e: newEx(t, ag.Name, func(c *TurnConfig) {
		c.NewID = func(now time.Time) (string, error) { ids++; return pipe.NewTurnID(now) }
	})}
	prev := 0
	for i, fr := range ev.Frames {
		rows := readLines(t, filepath.Join(dir, fr.File))
		if len(rows) != ev.Height {
			t.Fatalf("%s has %d rows, the evidence says %d", fr.File, len(rows), ev.Height)
		}
		if i == 0 {
			p.setView(rows)
			d.deliver(sent)
			continue
		}
		if fr.Top < prev {
			t.Fatalf("%s: the first visible row goes back from %d to %d", fr.File, prev, fr.Top)
		}
		p.step(fr.Top-prev, rows)
		prev = fr.Top
		state := frameRe.FindStringSubmatch(fr.File)[2]
		if cs, _ := screenState(ag, ops.Screen{Lines: rows}); cs != state {
			t.Fatalf("%s is classified %s, named %s", fr.File, cs, state)
		}
		turn, perr := d.poll()
		if perr != nil {
			return turns, ids, perr
		}
		if turn != nil {
			turns = append(turns, turn)
		}
	}
	return turns, ids, nil
}

func fixtureDirs(t *testing.T) map[string]*Agent {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", "turns", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	agents := map[string]*Agent{}
	for _, a := range DefaultAgents() {
		a := a
		agents[a.Name] = &a
	}
	out := map[string]*Agent{}
	for _, dir := range dirs {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue // README.md
		}
		name := filepath.Base(dir)
		m := agentOf.FindStringSubmatch(name)
		if m == nil || agents[m[1]] == nil {
			t.Fatalf("fixture %s: unknown agent", name)
		}
		out[dir] = agents[m[1]]
	}
	return out
}

// TestTurnFixtures replays the screen sequences of testdata/turns: per known
// agent a short reply, a long reply that scrolls, a redraw (identical frames, a
// changed status line, a streamed last line) and an interrupted reply. The
// sequences are synthesised from each agent's screen layout (the idle and busy
// screens of testdata/*.txt), not recorded from a live session, and rendered
// through a model of a terminal whose rows move only by scrolling from row 0;
// see testdata/turns/README.md. Each directory holds sent.txt (the delivered
// message), evidence.json (the first visible row of every frame),
// 00_delivered.txt (the screen at delivery), numbered frames named NN_<state>
// and want.txt (the expected turn text).
func TestTurnFixtures(t *testing.T) {
	seen := map[string]bool{}
	for dir, ag := range fixtureDirs(t) {
		dir, ag := dir, ag
		seen[ag.Name] = true
		t.Run(filepath.Base(dir), func(t *testing.T) {
			want := strings.Join(readLines(t, filepath.Join(dir, "want.txt")), "\n")
			entries, _ := os.ReadDir(dir)
			var frames []string
			for _, en := range entries {
				if frameRe.MatchString(en.Name()) {
					frames = append(frames, en.Name())
				}
			}
			sort.Strings(frames)
			if len(frames) < 2 {
				t.Fatalf("too few frames: %v", frames)
			}
			delivered := readLines(t, filepath.Join(dir, "00_delivered.txt"))
			if st, _ := screenState(ag, ops.Screen{Lines: delivered}); st != ops.PaneStateStable {
				t.Fatalf("00_delivered is classified %s, want stable", st)
			}

			// The normal screen: the exact reply, once, at the first stable
			// frame after a busy one.
			got, ids, err := replayFixture(t, dir, ag, fixtureOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d turns, want exactly 1 (duplicates or none)", len(got))
			}
			turn := got[0]
			if turn.Text != want {
				t.Errorf("text mismatch\n got: %q\nwant: %q", turn.Text, want)
			}
			if turn.Session != "work" || !pipe.IsTurnID(turn.TurnID) || !turn.At.Equal(t0) {
				t.Errorf("bad turn metadata: %+v", turn)
			}
			if escapeRe.MatchString(turn.Text) || strings.Contains(turn.Text, "\x1b") {
				t.Errorf("turn text has control characters: %q", turn.Text)
			}
			if ids != 1 {
				t.Errorf("ids=%d after the turn", ids)
			}

			// The alternate screen (opencode and codex run in it by default):
			// no turn at the first frame, whatever the rows say.
			got, _, err = replayFixture(t, dir, ag, fixtureOpts{alternate: true})
			if got != nil || reasonOf(err) != ReasonAlternateScreen {
				t.Errorf("alternate screen: turns=%v err=%v, want %s", got, err, ReasonAlternateScreen)
			}

			// tmux before 3.8 has no counters: no turn, however good the rows.
			got, _, err = replayFixture(t, dir, ag, fixtureOpts{pre38: true})
			if got != nil || reasonOf(err) != ReasonTmuxTooOld {
				t.Errorf("tmux before 3.8: turns=%v err=%v, want %s", got, err, ReasonTmuxTooOld)
			}
		})
	}
	for _, a := range DefaultAgents() {
		if !seen[a.Name] {
			t.Errorf("no turn fixtures for agent %s", a.Name)
		}
	}
}

// claude views used by the hand-written cases: the rows of the reply region,
// directly followed by the input box, as Claude Code draws them.
func claudeView(content []string, status string) []string {
	chrome := []string{
		"",
		"╭──────────────────────────────────────────────────────────────────────╮",
		"│ >                                                                    │",
		"╰──────────────────────────────────────────────────────────────────────╯",
		"  ? for shortcuts",
	}
	if status != "" {
		chrome = append([]string{"", status}, chrome...)
	}
	return append(append([]string{}, content...), chrome...)
}

const claudeBusyLine = "✻ Thinking… (3s · ↑ 1.2k tokens · esc to interrupt)"

func TestNewTurnExtractorNeedsSession(t *testing.T) {
	if _, err := NewTurnExtractor(TurnConfig{Session: "  "}); err == nil {
		t.Fatal("empty session accepted")
	}
}

func TestNotArmedNeverEmits(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView([]string{"> hi", "", "● hello"}, ""))
	for i := 0; i < 3; i++ {
		if turn, err := d.poll(); turn != nil || err != nil {
			t.Fatalf("turn=%v err=%v", turn, err)
		}
	}
	if turn, err := d.e.Flush(); turn != nil || err != nil {
		t.Fatalf("flush while not armed: %v %v", turn, err)
	}
	if got := d.e.Need(); got != 0 {
		t.Fatalf("Need() = %d while not armed", got)
	}
}

func TestStableWithoutReplyKeepsWaiting(t *testing.T) {
	d := newDrive(t, "claude", 24)
	base := []string{"old line"}
	d.p.setView(claudeView(base, ""))
	d.deliver("do it")
	// The agent has not reacted yet: the rows are unchanged and stable.
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn without a reply: %+v", turn)
	}
	// The pasted text sits in the input box: chrome, not an echo.
	d.p.setView(append(append([]string{}, base...), "", "╭──╮", "│ > do it │", "╰──╯", "  ? for shortcuts"))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("draft reported as a reply: %+v", turn)
	}
	if !d.e.Armed() {
		t.Fatal("extractor disarmed before any reply")
	}
	// The echo alone (no reply yet) is not a reply either.
	echo := append(append([]string{}, base...), "> do it", "")
	d.p.setView(claudeView(echo, ""))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("echo reported as a reply: %+v", turn)
	}
	d.p.setView(claudeView(append(echo, "● done"), ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "done" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestFastReplyNeverSeenBusy(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("ping")
	d.p.setView(claudeView([]string{"> ping", "", "● pong"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "pong" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestSecondTurnExcludesFirst(t *testing.T) {
	d := newDrive(t, "claude", 24)
	h1 := []string{"> first", "", "● one"}
	d.p.setView(claudeView(nil, ""))
	d.deliver("first")
	d.p.setView(claudeView(h1, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(h1, ""))
	t1 := d.pollOK()
	h2 := append(append([]string{}, h1...), "", "> second", "", "● two")
	d.deliver("second")
	d.p.setView(claudeView(h2, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(h2, ""))
	t2 := d.pollOK()
	if t1 == nil || t2 == nil || t1.Text != "one" || t2.Text != "two" {
		t.Fatalf("t1=%+v t2=%+v", t1, t2)
	}
	if t1.TurnID == t2.TurnID {
		t.Fatal("turn ids repeat")
	}
}

func TestSameMessageTwiceUsesTheNewEcho(t *testing.T) {
	d := newDrive(t, "claude", 24)
	hist := []string{"> again", "", "● old answer", "", "> again", "", "● new answer"}
	d.p.setView(claudeView(hist[:3], ""))
	d.deliver("again")
	d.p.setView(claudeView(hist, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(hist, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "new answer" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestMultiLineAndWrappedEcho(t *testing.T) {
	// A two-line message whose second line wraps on a narrow pane.
	msg := "Fix the parser\nand then run all the unit tests in the repository"
	hist := []string{
		"> Fix the parser",
		"  and then run all the unit",
		"  tests in the repository",
		"",
		"● Fixed.",
	}
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver(msg)
	d.p.setView(claudeView(hist, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(hist, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "Fixed." {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestQuoteLineAtEndOfReplyIsKept(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("q")
	d.p.setView(claudeView([]string{"> q", "", "● He said:", "  > to be or not to be"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "He said:\n> to be or not to be" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestDialogFramesAreSkipped(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("edit it")
	h := []string{"> edit it", "", "● I will edit main.go"}
	d.p.setView(claudeView(h, claudeBusyLine))
	d.pollOK()
	d.p.setView(append(append([]string{}, h...),
		"Do you want to make this edit?",
		"❯ 1. Yes",
		"  2. No",
		"Esc to cancel"))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn during a dialog: %+v", turn)
	}
	d.p.setView([]string{"> edit it", "", "● I will edit main.go", "???"})
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn on an unknown screen: %+v", turn)
	}
	d.p.setView(claudeView(append(append([]string{}, h...), "● Edited main.go"), ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "I will edit main.go\nEdited main.go" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestSettleRidesOverShortPause(t *testing.T) {
	d := newDrive(t, "claude", 24, func(c *TurnConfig) { c.Settle = 2 })
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	h1 := []string{"> go", "", "● step one"}
	d.p.setView(claudeView(h1, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(h1, ""))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("turn after one stable frame: %+v", turn)
	}
	h2 := append(append([]string{}, h1...), "● step two")
	d.p.setView(claudeView(h2, claudeBusyLine))
	d.pollOK()
	d.p.setView(claudeView(h2, ""))
	if turn := d.pollOK(); turn != nil {
		t.Fatalf("settle counter not reset by busy: %+v", turn)
	}
	turn := d.pollOK()
	if turn == nil || turn.Text != "step one\nstep two" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestFlushReturnsPartialReply(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● half an answer"}, claudeBusyLine))
	d.pollOK()
	turn, err := d.e.Flush()
	if err != nil || turn == nil || turn.Text != "half an answer" {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if d.e.Armed() {
		t.Fatal("still armed after flush")
	}
	if turn, _ := d.e.Flush(); turn != nil {
		t.Fatalf("second flush emitted %+v", turn)
	}
}

func TestFlushWithoutReplyDisarms(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	if turn, err := d.e.Flush(); turn != nil || err != nil {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if d.e.Armed() {
		t.Fatal("still armed")
	}
}

// Flush reports only what valid evidence supported: a delivery that was
// retired has nothing left.
func TestFlushAfterRetiredDeliveryReportsNothing(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● half an answer"}, claudeBusyLine))
	d.pollOK()
	d.p.resizeWidth(60)
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonResized)
	if turn, err := d.e.Flush(); turn != nil || err != nil {
		t.Fatalf("flush after a retired delivery: %+v %v", turn, err)
	}
}

func TestIDFailureKeepsArmedAndRetries(t *testing.T) {
	fail := true
	d := newDrive(t, "claude", 24, func(c *TurnConfig) {
		c.NewID = func(now time.Time) (string, error) {
			if fail {
				return "", errors.New("no entropy")
			}
			return pipe.NewTurnID(now)
		}
	})
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● ok"}, ""))
	if turn, err := d.poll(); turn != nil || err == nil || errors.Is(err, ErrTurnUnavailable) {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	if !d.e.Armed() {
		t.Fatal("disarmed by an id failure")
	}
	fail = false
	turn := d.pollOK()
	if turn == nil || turn.Text != "ok" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestNoDuplicateAfterTurn(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● ok"}, ""))
	if turn := d.pollOK(); turn == nil {
		t.Fatal("no turn")
	}
	for i := 0; i < 3; i++ {
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("duplicate turn on redraw %d: %+v", i, turn)
		}
		d.p.setView(claudeView([]string{"> go", "", "● ok"}, claudeBusyLine))
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn from a busy redraw %d: %+v", i, turn)
		}
	}
}

func TestControlCharactersAreDropped(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	// The rows are sanitised already; the extractor must not rely on that.
	d.p.setView(claudeView([]string{"> go", "", "● a\x1b[31mred\x07 word\x00"}, ""))
	turn := d.pollOK()
	if turn == nil || turn.Text != "a[31mred word" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestTurnTextIsBounded(t *testing.T) {
	d := newDrive(t, "claude", 520)
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	// A reply far beyond MaxTurnBytes.
	big := strings.Repeat("z", 1000)
	content := []string{"> go", ""}
	for i := 0; i < 400; i++ {
		content = append(content, "  "+big)
	}
	d.p.setView(claudeView(content, claudeBusyLine))
	d.pollOK()
	turn, _ := d.e.Flush()
	if turn == nil {
		t.Fatal("no turn")
	}
	if len(turn.Text) > MaxTurnBytes+len(TruncatedMarker)+1 || !strings.HasSuffix(turn.Text, "\n"+TruncatedMarker) {
		t.Fatalf("turn not bounded: %d bytes", len(turn.Text))
	}
}

func TestAccumulationIsBounded(t *testing.T) {
	d := newDrive(t, "claude", 24)
	d.p.limit = 1 << 30
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● start"}, claudeBusyLine))
	d.pollOK()
	// More rows than the extractor tracks leave through the top between two
	// frames: it keeps reading, but not holding.
	history := d.p.visible()[:3] // the echo and its reply stay what they were
	for i := 0; i < maxTrackedLines+5000; i++ {
		history = append(history, "  row")
	}
	d.p.push(history, claudeView([]string{"  fresh line"}, claudeBusyLine))
	d.pollOK()
	if len(d.e.hist) > maxTrackedLines {
		t.Fatalf("accumulation kept growing: %d rows", len(d.e.hist))
	}
	if d.e.final != d.p.top {
		t.Fatalf("final = %d, want %d: the rows that are not held still count as read", d.e.final, d.p.top)
	}
	if got, want := d.e.Need(), d.e.histFrom; got != want {
		t.Fatalf("Need() = %d, want %d: all the rows that became history are read again", got, want)
	}
	turn, _ := d.e.Flush()
	if turn == nil || !strings.HasSuffix(turn.Text, "\n"+TruncatedMarker) {
		t.Fatalf("a capped reply must say so: %+v", turn)
	}
}

func TestUnknownAgentUsesGenericMarkers(t *testing.T) {
	d := newDrive(t, "nosuchagent", 24)
	d.p.setView([]string{"$ "})
	d.deliver("go")
	hist := []string{"> go", "", "plain reply line", "second line"}
	d.p.setView(append(append([]string{}, hist...), "", "esc to interrupt"))
	d.pollOK()
	d.p.setView(append(append([]string{}, hist...), "", "  ? for shortcuts"))
	turn := d.pollOK()
	if turn == nil || turn.Text != "plain reply line\nsecond line" {
		t.Fatalf("turn=%+v", turn)
	}
}

func TestConcurrentUse(t *testing.T) {
	e := newEx(t, "claude")
	p := newFakePane(24)
	p.setView(claudeView(nil, ""))
	mark := p.mark()
	p.setView(claudeView([]string{"> go", "", "● x"}, ""))
	frame := p.frame(mark.ScreenTop())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.Delivered("go", mark)
				_, _ = e.Observe(frame)
				e.Need()
				e.Armed()
			}
		}()
	}
	wg.Wait()
}

func TestUnavailableErrorShape(t *testing.T) {
	err := Unavailable(ReasonResized)
	var ue *UnavailableError
	if !errors.Is(err, ErrTurnUnavailable) || !errors.As(err, &ue) || ue.Reason != "resized" {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "resized") {
		t.Fatalf("message %q does not name the reason", err)
	}
	wrapped := errors.Join(errors.New("x"), err)
	if reasonOf(wrapped) != "resized" {
		t.Fatal("reason lost in a wrapped error")
	}
}
