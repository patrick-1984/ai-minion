//go:build unix && tmux

package agentdetect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/fakeagent"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// TestMain lets the test binary be the fake agent of a pane (see fakeagent).
func TestMain(m *testing.M) {
	if fakeagent.Run() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The turn extractor on a real tmux (Story 7.2, criteria 12 to 24). The agent
// is a fake line-oriented program in the normal screen: a prompt "> ", the echo
// by the tty, and reply rows that the test prints one batch at a time, polling
// between them. On tmux before 3.8 only TestTmuxBefore38GivesNoTurnButDelivers
// runs; the other tests need the counters.

// tmuxRig is a pane with the fake agent, the host and an extractor for it.
type tmuxRig struct {
	t      *testing.T
	srv    *tmuxtest.Server
	h      *tmux.Host
	e      *TurnExtractor
	name   string
	target string
	ref    term.PaneRef
	ctl    string
	out    string
	rows   int // the number of "rows" printed so far (the agent's counter)
}

type rigOpts struct {
	mode   string // fakeagent mode, ModeLine by default
	agent  string // the agent profile of the extractor, "aider" by default
	limit  int    // history-limit of the server, 0 for the default
	height int    // pane height, 12 by default
	width  int    // pane width, 80 by default
	settle int
	// shell starts the agent from a shell: the pane's process is the shell, the
	// agent is its child, and when the agent exits the pane shows the shell.
	shell bool
}

var rigCount int

func newTmuxRig(t *testing.T, o rigOpts) *tmuxRig {
	t.Helper()
	srv := tmuxtest.New(t)
	h, err := tmux.New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if o.mode == "" {
		o.mode = fakeagent.ModeLine
	}
	if o.agent == "" {
		o.agent = "aider"
	}
	if o.height == 0 {
		o.height = 12
	}
	if o.width == 0 {
		o.width = 80
	}
	if o.limit > 0 {
		mustTmux(t, srv, "set-option", "-g", "history-limit", fmt.Sprint(o.limit))
	}
	rigCount++
	r := &tmuxRig{t: t, srv: srv, h: h, name: fmt.Sprintf("t%d", rigCount)}
	dir := t.TempDir()
	r.ctl, r.out = filepath.Join(dir, "ctl"), filepath.Join(dir, "out")
	for _, f := range []string{r.ctl, r.out} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	argv := fakeagent.Argv(o.mode, r.ctl, r.out)
	if o.shell {
		// The shell runs the agent as a child and then prints a line of its own
		// and waits, like an interactive shell after the program has ended.
		argv = append([]string{"sh", "-c", `"$@"; echo SHELL-PROMPT; exec sleep 300`, "sh"}, argv...)
	}
	r.target = srv.NewSession(r.name, argv...)
	if o.mode != fakeagent.ModeBare {
		// The agent draws its prompt after it started: deliver only once it is
		// there, or the tty echo of the message races with the prompt.
		srv.WaitFor(r.target, ">", 10*time.Second)
	}
	mustTmux(t, srv, "resize-window", "-t", r.target, "-x", fmt.Sprint(o.width), "-y", fmt.Sprint(o.height))
	r.ref = r.resolve()
	e, err := NewTurnExtractor(TurnConfig{Session: "work", Agent: o.agent, Settle: o.settle,
		Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

func mustTmux(t *testing.T, srv *tmuxtest.Server, args ...string) string {
	t.Helper()
	out, err := srv.Tmux(args...)
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return out
}

func (r *tmuxRig) tmux(args ...string) string { r.t.Helper(); return mustTmux(r.t, r.srv, args...) }

func (r *tmuxRig) resolve() term.PaneRef {
	r.t.Helper()
	ref, err := r.h.ResolvePane(context.Background(), ops.Target{SessionName: r.name})
	if err != nil {
		r.t.Fatal(err)
	}
	return ref
}

// old reports whether the tmux is older than 3.8. Then the scenario of the
// calling test is not run: on such a tmux every turn case gives the same
// result, so old checks that, with the message delivered, and the test ends.
func (r *tmuxRig) old() bool {
	r.t.Helper()
	if r.h.TurnsSupported() {
		return false
	}
	r.say("hello")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(readFile(r.out), `submitted "hello"`) {
		if time.Now().After(deadline) {
			r.t.Fatalf("tmux %s: the message was not delivered: %q", r.h.Version(), readFile(r.out))
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.print(3)
	turn, err := r.finish()
	wantReason(r.t, turn, err, ReasonTmuxTooOld)
	return true
}

// send appends control commands for the fake agent.
func (r *tmuxRig) send(cmds ...string) {
	r.t.Helper()
	f, err := os.OpenFile(r.ctl, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	for _, c := range cmds {
		if _, err := f.WriteString(c + "\n"); err != nil {
			r.t.Fatal(err)
		}
	}
}

// print makes the agent print n reply rows, waits until the last one is
// visible and returns the text the reply gets from them.
func (r *tmuxRig) print(n int) []string {
	r.t.Helper()
	var want []string
	for i := 0; i < n; i++ {
		want = append(want, fmt.Sprintf("r%d", r.rows+1+i))
	}
	r.send(fmt.Sprintf("rows %d", n))
	r.rows += n
	r.srv.WaitFor(r.target, fmt.Sprintf("r%d", r.rows), 10*time.Second)
	return want
}

// sync makes the agent print a marker row and waits for it: every control
// command before it has taken effect.
func (r *tmuxRig) sync(marker string) {
	r.t.Helper()
	r.send("text " + marker)
	r.srv.WaitFor(r.target, marker, 10*time.Second)
}

// say delivers msg the way the pipe does: the marked paste, then Enter.
func (r *tmuxRig) say(msg string) {
	r.t.Helper()
	ctx := context.Background()
	mark, err := r.h.PasteMarked(ctx, r.ref, msg, r.ref)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.h.SendKeys(ctx, r.ref, []string{"Enter"}, r.ref); err != nil {
		r.t.Fatal(err)
	}
	r.e.Delivered(msg, mark)
}

// poll reads one frame at Need() and observes it.
func (r *tmuxRig) poll() (*Turn, error) {
	r.t.Helper()
	f, err := r.h.ReadFrame(context.Background(), r.ref, r.e.Need())
	if err != nil {
		r.t.Fatalf("ReadFrame: %v", err)
	}
	return r.e.Observe(f)
}

func (r *tmuxRig) pollOK() *Turn {
	r.t.Helper()
	turn, err := r.poll()
	if err != nil {
		r.t.Fatalf("poll: %v", err)
	}
	return turn
}

// finish ends the reply and polls until the turn or an error comes.
func (r *tmuxRig) finish() (*Turn, error) {
	r.t.Helper()
	r.send("done")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		turn, err := r.poll()
		if turn != nil || err != nil {
			return turn, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatal("no turn and no error within 15s")
	return nil, nil
}

// wantExact fails unless the turn is exactly rows, one per line.
func wantExact(t *testing.T, turn *Turn, err error, rows []string) {
	t.Helper()
	if err != nil || turn == nil || turn.Text != strings.Join(rows, "\n") {
		got := "<nil>"
		if turn != nil {
			got = turn.Text
		}
		t.Fatalf("turn=%q err=%v\nwant %q", got, err, strings.Join(rows, "\n"))
	}
}

func wantReason(t *testing.T, turn *Turn, err error, reason string) {
	t.Helper()
	if turn != nil || reasonOf(err) != reason {
		t.Fatalf("turn=%+v err=%v, want %s", turn, err, reason)
	}
}

// warm runs one message with an n-row reply so that the pane has history and
// rows above the next echo.
func (r *tmuxRig) warm(n int) {
	r.t.Helper()
	r.say("warm")
	r.print(n)
	r.send("done")
	r.srv.WaitFor(r.target, "> ", 10*time.Second)
	// Wait until the prompt is the last row of the pane.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := r.h.Capture(context.Background(), r.ref, 10)
		if n := len(out.Lines); n > 0 && strings.TrimSpace(out.Lines[n-1]) == ">" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatal("the agent never showed its prompt again")
}

// ---------------------------------------------------------------------------

// Before tmux 3.8 the message is delivered, and no turn comes: the reason is
// tmux_too_old. Every turn case below ends with that check on such a tmux; this
// one is the plain case.
func TestTmuxBefore38GivesNoTurnButDelivers(t *testing.T) {
	r := newTmuxRig(t, rigOpts{})
	if r.h.TurnsSupported() {
		t.Skipf("tmux %s supports turns", r.h.Version())
	}
	if !r.old() {
		t.Fatal("old() must report an old tmux")
	}
	if r.e.Armed() {
		t.Fatal("still armed")
	}
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// 12: a reply of three screens.
func TestTmuxScrollThreeScreens(t *testing.T) {
	r := newTmuxRig(t, rigOpts{})
	if r.old() {
		return
	}
	r.say("hello")
	var want []string
	for i := 0; i < 4; i++ {
		want = append(want, r.print(9)...)
		if turn := r.pollOK(); turn != nil {
			t.Fatalf("turn mid reply: %+v", turn)
		}
	}
	turn, err := r.finish()
	wantExact(t, turn, err, want)
}

// 13: the whole reply scrolled the echo off before the first frame.
func TestTmuxFastReplyEchoInHistory(t *testing.T) {
	r := newTmuxRig(t, rigOpts{})
	if r.old() {
		return
	}
	r.say("hello")
	want := r.print(60)
	time.Sleep(300 * time.Millisecond) // the echo is far above the screen now
	turn, err := r.finish()
	wantExact(t, turn, err, want)
}

// 14: bare echo in the normal screen: reply rows equal to the echo row reach
// the top of the screen.
func TestTmuxBareEchoInTheNormalScreen(t *testing.T) {
	r := newTmuxRig(t, rigOpts{mode: fakeagent.ModeBare, agent: "opencode"})
	if r.old() {
		return
	}
	r.say("hello")
	r.send("text hello") // a reply row that equals the echo
	want := append([]string{"hello"}, r.print(20)...)
	if turn := r.pollOK(); turn != nil {
		t.Fatalf("turn mid reply: %+v", turn)
	}
	turn, err := r.finish()
	wantExact(t, turn, err, want)
}

// 15: a mature pane (history at its limit) whose oldest rows are trimmed behind
// the reader, one step taking history_size down.
func TestTmuxMaturePaneTrimBehindTheReader(t *testing.T) {
	r := newTmuxRig(t, rigOpts{limit: 100})
	if r.old() {
		return
	}
	r.warm(110)
	r.say("hello")
	var want []string
	dropped := false
	prev := r.historySize()
	for i := 0; i < 12; i++ {
		want = append(want, r.print(5)...)
		if turn := r.pollOK(); turn != nil {
			t.Fatalf("turn mid reply: %+v", turn)
		}
		if size := r.historySize(); size < prev {
			dropped = true
		}
		prev = r.historySize()
	}
	if !dropped {
		t.Fatal("history_size never went down: the scenario trimmed nothing")
	}
	turn, err := r.finish()
	wantExact(t, turn, err, want)
}

// waitFormat waits until the tmux format expands to want for the pane.
func (r *tmuxRig) waitFormat(format, want string) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for strings.TrimSpace(r.tmux("display-message", "-p", "-t", r.target, format)) != want {
		if time.Now().After(deadline) {
			r.t.Fatalf("%s never became %q", format, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *tmuxRig) historySize() int {
	r.t.Helper()
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(r.tmux("display-message", "-p", "-t", r.target, "#{history_size}")), &n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// 16: more rows than the history holds leave before the first frame.
func TestTmuxTrimPastUnreadRows(t *testing.T) {
	r := newTmuxRig(t, rigOpts{limit: 100})
	if r.old() {
		return
	}
	r.say("hello")
	r.print(300)
	time.Sleep(300 * time.Millisecond)
	turn, err := r.finish()
	wantReason(t, turn, err, ReasonHistoryTrimmed)
}

// 17: the width.
func TestTmuxWidth(t *testing.T) {
	t.Run("change mid reply", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.print(5)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-x", "100")
		r.print(3)
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonResized)
	})
	t.Run("change and back between two frames", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.print(5)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-x", "100")
		r.tmux("resize-window", "-t", r.target, "-x", "80")
		r.print(3)
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonHistoryCleared)
	})
}

// 17: widening a pane whose history holds wrapped rows. The reflow joins them,
// the history gets shorter and the first visible row moves up.
func TestTmuxWidenWithWrappedHistory(t *testing.T) {
	r := newTmuxRig(t, rigOpts{height: 12})
	if r.old() {
		return
	}
	r.say("hello")
	long := strings.Repeat("0123456789", 14) + "abcde" // 145 columns, two rows at 80
	for i := 0; i < 40; i++ {
		r.send("text " + long)
	}
	r.srv.WaitFor(r.target, "abcde", 10*time.Second)
	r.sync("SYNC1")
	r.pollOK()
	final := r.e.final
	r.tmux("resize-window", "-t", r.target, "-x", "200")
	deadline := time.Now().Add(10 * time.Second)
	for r.evidence().LastRow() >= final && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.evidence().LastRow() >= final {
		t.Fatalf("the reflow did not shorten the history below row %d", final)
	}
	turn, err := r.finish()
	wantReason(t, turn, err, ReasonResized)
}

// evidence reads the evidence of the pane (the frame from the screen top).
func (r *tmuxRig) evidence() term.PaneEvidence {
	r.t.Helper()
	f, err := r.h.ReadFrame(context.Background(), r.ref, 0)
	if err != nil {
		r.t.Fatalf("ReadFrame: %v", err)
	}
	return f.PaneEvidence
}

// The agent exits back to the shell that started it in the middle of the reply.
// The pane keeps its pid; the program is another one. Flush must not hand out
// the rows of the shell.
func TestTmuxAgentExitsToItsShell(t *testing.T) {
	r := newTmuxRig(t, rigOpts{shell: true})
	if r.old() {
		return
	}
	r.say("hello")
	r.print(4)
	r.pollOK()
	r.send("exit")
	r.srv.WaitFor(r.target, "SHELL-PROMPT", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for r.resolve().Command == r.ref.Command && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := r.resolve(); got.PID != r.ref.PID || got.Command == r.ref.Command {
		t.Fatalf("the scenario needs the same pid and another program: before %+v, now %+v", r.ref, got)
	}
	turn, err := r.poll()
	wantReason(t, turn, err, ReasonPaneRespawned)
	if turn, err := r.e.Flush(); turn != nil || err != nil {
		t.Fatalf("Flush after the program ended: turn=%+v err=%v", turn, err)
	}
}

// 18: the height.
func TestTmuxHeight(t *testing.T) {
	t.Run("grow and shrink mid reply", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		want := r.print(14)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-y", "16")
		want = append(want, r.print(6)...)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-y", "8")
		want = append(want, r.print(6)...)
		r.pollOK()
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
	t.Run("grow, redraw a consumed row, shrink between two frames", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.print(14)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-y", "30")
		// The program redraws the first visible row, a row of the reply that
		// was history, and the pane shrinks before the next poll.
		r.send(`raw \e7\e[1;1HREDRAWN\e[K\e8`)
		r.sync("SYNC")
		r.tmux("resize-window", "-t", r.target, "-y", "12")
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonEvidenceMismatch)
	})
	t.Run("a round trip between two frames after a trim", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{limit: 100})
		if r.old() {
			return
		}
		r.warm(110)
		r.say("hello")
		want := r.print(10)
		r.pollOK()
		r.tmux("resize-window", "-t", r.target, "-y", "17")
		r.tmux("resize-window", "-t", r.target, "-y", "12")
		want = append(want, r.print(10)...)
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
}

// 19: clears.
func TestTmuxClear(t *testing.T) {
	for name, c := range map[string]struct {
		clear  func(r *tmuxRig)
		refill bool
	}{
		"CSI 3 J":                  {func(r *tmuxRig) { r.send(`raw \e[3J`); r.sync("SYNC") }, false},
		"CSI 3 J then a refill":    {func(r *tmuxRig) { r.send(`raw \e[3J`); r.sync("SYNC") }, true},
		"clear-history":            {func(r *tmuxRig) { r.tmux("clear-history", "-t", r.target) }, false},
		"clear-history and refill": {func(r *tmuxRig) { r.tmux("clear-history", "-t", r.target) }, true},
	} {
		c := c
		t.Run(name, func(t *testing.T) {
			r := newTmuxRig(t, rigOpts{})
			if r.old() {
				return
			}
			r.say("hello")
			r.print(20) // history is not empty
			r.pollOK()
			c.clear(r)
			if c.refill {
				r.print(30)
			}
			turn, err := r.finish()
			wantReason(t, turn, err, ReasonHistoryCleared)
		})
	}
	// tmux 3.8 clears the history without raising history_collected, so the first
	// visible row drops below the row the reader needs next. With 20 rows of
	// history that is still within one screen; after 40 rows it is not.
	for name, clear := range map[string]func(r *tmuxRig){
		"clear-history": func(r *tmuxRig) { r.tmux("clear-history", "-t", r.target) },
		"CSI 3 J":       func(r *tmuxRig) { r.send(`raw \e[3J`); r.sync("SYNC") },
	} {
		clear := clear
		t.Run(name+" after 40 rows at height 12", func(t *testing.T) {
			r := newTmuxRig(t, rigOpts{height: 12})
			if r.old() {
				return
			}
			r.say("hello")
			r.print(40)
			r.pollOK()
			if r.e.final <= 12 {
				t.Fatalf("consumed up to %d: the scenario needs more than one screen of history", r.e.final)
			}
			clear(r)
			turn, err := r.finish()
			wantReason(t, turn, err, ReasonHistoryCleared)
		})
	}
	t.Run("CSI 2 J with scroll-on-clear is exact", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		want := r.print(15)
		r.pollOK()
		r.send(`raw \e[2J\e[H`)
		want = append(want, r.print(10)...)
		r.pollOK()
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
	t.Run("a reprint of the transcript", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.warm(2)
		r.say("hello")
		r.print(3)
		r.pollOK()
		r.send(`raw \e[2J\e[H`, "text > warm", "text r1", "text r2", "text > hello")
		r.print(3)
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonTranscriptReprinted)
	})
}

// 20: the alternate screen.
func TestTmuxAlternateScreen(t *testing.T) {
	t.Run("on at delivery", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("warm")
		r.send(`raw \e[?1049h`, "done")
		r.waitFormat("#{alternate_on}", "1")
		r.srv.WaitFor(r.target, ">", 10*time.Second) // the prompt, drawn in the alternate screen
		r.say("hello")
		turn, err := r.poll()
		wantReason(t, turn, err, ReasonAlternateScreen)
	})
	t.Run("a pager in the middle of the reply", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		want := r.print(4)
		r.pollOK()
		// The pager draws a screen that looks idle; it must not complete the turn.
		r.send(`raw \e[?1049h`, "text pager", "raw > ")
		r.srv.WaitFor(r.target, "pager", 10*time.Second)
		for i := 0; i < 3; i++ {
			if turn := r.pollOK(); turn != nil {
				t.Fatalf("turn read while the alternate screen is on: %+v", turn)
			}
			time.Sleep(50 * time.Millisecond)
		}
		r.send(`raw \e[?1049l`)
		r.sync("SYNC")
		want = append(want, "SYNC")
		want = append(want, r.print(4)...)
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
}

// 21: respawn-pane -k.
func TestTmuxRespawn(t *testing.T) {
	r := newTmuxRig(t, rigOpts{})
	if r.old() {
		return
	}
	r.say("hello")
	r.print(4)
	r.pollOK()
	r.tmux("respawn-pane", "-k", "-t", r.ref.ID)
	deadline := time.Now().Add(10 * time.Second)
	for r.resolve().PID == r.ref.PID && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	turn, err := r.poll()
	wantReason(t, turn, err, ReasonPaneRespawned)
}

// 23: history-limit lowered on the live session.
func TestTmuxLiveHistoryLimit(t *testing.T) {
	t.Run("unread rows survive", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.warm(110)
		r.say("hello")
		want := r.print(5)
		r.pollOK()
		r.tmux("set-option", "-t", r.target, "history-limit", "50")
		want = append(want, r.print(5)...)
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
	t.Run("unread rows are trimmed", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.warm(110)
		r.say("hello")
		r.print(40)
		r.tmux("set-option", "-t", r.target, "history-limit", "10")
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonHistoryTrimmed)
	})
}

// 24: scroll regions (F2).
func TestTmuxScrollRegion(t *testing.T) {
	t.Run("a region from row 2, the echo visible", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.send(`raw \e[3;12r\e[12;1H`)
		r.print(4)
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonScrollRegion)
	})
	t.Run("a region from row 2, the echo in history", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.print(30)
		r.pollOK()
		r.send(`raw \e[3;12r\e[12;1H`)
		r.print(4)
		turn, err := r.finish()
		wantReason(t, turn, err, ReasonScrollRegion)
	})
	t.Run("a region from row 0 above a composer is exact", func(t *testing.T) {
		r := newTmuxRig(t, rigOpts{})
		if r.old() {
			return
		}
		r.say("hello")
		r.send(`raw \e[1;9r\e[9;1H`)
		want := r.print(15)
		if turn := r.pollOK(); turn != nil {
			t.Fatalf("turn mid reply: %+v", turn)
		}
		turn, err := r.finish()
		wantExact(t, turn, err, want)
	})
}

// Several grow, erase and shrink cycles between two polls expose rows of the
// history that lie further back than one pane height, and drop consumed rows
// that were erased. The delivery must be retired by the next frame; it must
// not survive to Flush, which would emit the original rows as the reply.
func TestTmuxCumulativeResizeCyclesRewriteDeepHistory(t *testing.T) {
	r := newTmuxRig(t, rigOpts{limit: 50000, height: 24})
	if r.old() {
		return
	}
	r.say("hello")
	r.print(12000)
	r.pollOK()
	for i := 0; i < 3; i++ {
		r.tmux("resize-window", "-t", r.target, "-y", "5000")
		// Erase the bottom 3000 rows of the grown screen and put the cursor on
		// the last row that is left: the blank rows go first when the pane
		// shrinks, so the scrollback ends 3000 rows shorter and the consumed
		// rows below the new screen are gone. No marker row is printed: it
		// would be a changed row itself.
		r.send(`raw \e[2001;1H\e[J\e[1A`)
		time.Sleep(300 * time.Millisecond)
		r.tmux("resize-window", "-t", r.target, "-y", "24")
	}
	turn, err := r.poll()
	wantReason(t, turn, err, ReasonEvidenceMismatch)
	if turn, err := r.e.Flush(); turn != nil || err != nil {
		t.Fatalf("a retired delivery flushed turn=%+v err=%v", turn, err)
	}
}
