//go:build unix && tmux

package tmux

import (
	"context"
	"encoding/hex"
	"errors"
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
)

// TestMain lets the test binary be the fake agent of a pane (see fakeagent).
func TestMain(m *testing.M) {
	if fakeagent.Run() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// rig is a private tmux server with a Host on its socket.
type rig struct {
	t   *testing.T
	srv *tmuxtest.Server
	h   *Host
	dir string
	n   int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	srv := tmuxtest.New(t)
	h, err := New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, srv: srv, h: h, dir: t.TempDir()}
}

func (r *rig) tmux(args ...string) string {
	r.t.Helper()
	out, err := r.srv.Tmux(args...)
	if err != nil {
		r.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return out
}

// pane is a pane with the fake agent in it.
type pane struct {
	r      *rig
	target string
	ref    term.PaneRef
	ctl    string
	out    string
}

// agent starts the fake agent in a new session and resolves its pane.
func (r *rig) agent(mode string) *pane {
	r.t.Helper()
	r.n++
	p := &pane{r: r, ctl: filepath.Join(r.dir, fmt.Sprintf("ctl%d", r.n)), out: filepath.Join(r.dir, fmt.Sprintf("out%d", r.n))}
	for _, f := range []string{p.ctl, p.out} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	name := fmt.Sprintf("s%d", r.n)
	p.target = r.srv.NewSession(name, fakeagent.Argv(mode, p.ctl, p.out)...)
	p.resolve(name)
	if mode == fakeagent.ModeDump || mode == fakeagent.ModeDumpBP {
		p.waitShown("ready") // raw mode (and bracketed paste) is on
	}
	return p
}

func (p *pane) resolve(name string) {
	p.r.t.Helper()
	ref, err := p.r.h.ResolvePane(context.Background(), ops.Target{SessionName: name})
	if err != nil {
		p.r.t.Fatal(err)
	}
	p.ref = ref
}

// send appends control commands for the fake agent.
func (p *pane) send(cmds ...string) {
	p.r.t.Helper()
	f, err := os.OpenFile(p.ctl, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		p.r.t.Fatal(err)
	}
	defer f.Close()
	for _, c := range cmds {
		if _, err := f.WriteString(c + "\n"); err != nil {
			p.r.t.Fatal(err)
		}
	}
}

func (p *pane) log() string {
	b, _ := os.ReadFile(p.out)
	return string(b)
}

// waitLog waits until the agent's log satisfies ok.
func (p *pane) waitLog(ok func(string) bool) string {
	p.r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if l := p.log(); ok(l) {
			return l
		}
		if time.Now().After(deadline) {
			p.r.t.Fatalf("the agent's log never matched: %q", p.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitShown waits until the pane shows text. The fake agent prints it after it
// enabled bracketed paste, and the terminal handles output in order.
func (p *pane) waitShown(text string) { p.r.srv.WaitFor(p.target, text, 10*time.Second) }

func (p *pane) buffers() string { return strings.TrimSpace(p.r.tmux("list-buffers")) }

func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

// F7: the message arrives byte for byte at a raw receiver, wrapped exactly
// when the receiver enabled bracketed paste, and a message that cannot be
// delivered verbatim is refused with nothing pasted and no buffer left. Runs
// on every tmux version.
func TestPasteMarkedDeliversBytesVerbatim(t *testing.T) {
	msgs := map[string]string{
		"lf and tab":  "one\ntwo\tT",
		"punctuation": "$ #{x} 'q' \"q\" ; \\ ` <> | & % ~ ^ ( ) [ ] * ? !",
		"utf8":        "\u00e9 \u65e5\u672c\u8a9e \U0001F600 \u03a9mega",
	}
	for name, msg := range msgs {
		msg := msg
		t.Run("raw/"+name, func(t *testing.T) {
			r := newRig(t)
			p := r.agent(fakeagent.ModeDump)
			if _, err := r.h.PasteMarked(context.Background(), p.ref, msg, p.ref); err != nil {
				t.Fatal(err)
			}
			p.waitLog(func(l string) bool { return l == hexOf(msg) })
			if got := p.buffers(); got != "" {
				t.Fatalf("a buffer is left: %q", got)
			}
		})
		t.Run("bracketed/"+name, func(t *testing.T) {
			r := newRig(t)
			p := r.agent(fakeagent.ModeDumpBP)
			if _, err := r.h.PasteMarked(context.Background(), p.ref, msg, p.ref); err != nil {
				t.Fatal(err)
			}
			want := "1b5b3230307e" + hexOf(msg) + "1b5b3230317e"
			p.waitLog(func(l string) bool { return l == want })
		})
	}
	t.Run("refused", func(t *testing.T) {
		r := newRig(t)
		p := r.agent(fakeagent.ModeDump)
		for _, msg := range []string{"a\rb", "a\x1bb", "a\x01b", "a\x7fb", "a\u0085b", "a\u009bb", "a\x1b[201~b"} {
			_, err := r.h.PasteMarked(context.Background(), p.ref, msg, p.ref)
			if !errors.Is(err, ops.ErrInvalidArgument) {
				t.Errorf("%q: err = %v", msg, err)
			}
		}
		time.Sleep(300 * time.Millisecond)
		if got := p.log(); got != "" {
			t.Fatalf("bytes reached the pane: %q", got)
		}
		if got := p.buffers(); got != "" {
			t.Fatalf("a buffer is left: %q", got)
		}
	})
}

// A two-line message to an agent that enables bracketed paste is one message:
// the pasted LF does not submit, the Enter after it does, once.
func TestPasteMarkedTwoLineMessageSubmitsOnce(t *testing.T) {
	r := newRig(t)
	p := r.agent(fakeagent.ModeBP)
	p.waitShown("> ")
	if _, err := r.h.PasteMarked(context.Background(), p.ref, "first line\nsecond line", p.ref); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := p.log(); got != "" {
		t.Fatalf("submitted before the Enter: %q", got)
	}
	if _, err := r.h.SendKeys(context.Background(), p.ref, []string{"Enter"}, p.ref); err != nil {
		t.Fatal(err)
	}
	p.waitLog(func(l string) bool { return strings.Count(l, "submitted") == 1 })
	if got := p.log(); got != "submitted \"first line\\nsecond line\"\n" {
		t.Fatalf("log = %q", got)
	}
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(p.log(), "submitted"); n != 1 {
		t.Fatalf("submitted %d times", n)
	}
}

// F6: the KEEP check is the whole predicate, evaluated in the paste
// invocation. Each refusal returns ErrPaneChanged, delivers no byte and leaves
// no buffer.
func TestPasteMarkedKeepRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("input disabled", func(t *testing.T) {
		r := newRig(t)
		p := r.agent(fakeagent.ModeDump)
		r.tmux("select-pane", "-d", "-t", p.ref.ID)
		_, err := r.h.PasteMarked(ctx, p.ref, "secret", p.ref)
		if !isChanged(err) {
			t.Fatalf("err = %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if p.log() != "" || p.buffers() != "" {
			t.Fatalf("log=%q buffers=%q", p.log(), p.buffers())
		}
	})
	t.Run("the program ended and the pane is kept", func(t *testing.T) {
		r := newRig(t)
		r.tmux("set-option", "-g", "remain-on-exit", "on")
		p := r.agent(fakeagent.ModeLine)
		r.tmux("send-keys", "-t", p.target, "x", "Enter")
		p.waitLog(func(l string) bool { return strings.Contains(l, "submitted") })
		p.send("exit")
		deadline := time.Now().Add(5 * time.Second)
		for strings.TrimSpace(r.tmux("display-message", "-p", "-t", p.target, "#{pane_dead}")) != "1" && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		_, err := r.h.PasteMarked(ctx, p.ref, "secret", p.ref)
		if !isChanged(err) {
			t.Fatalf("err = %v", err)
		}
		if p.buffers() != "" {
			t.Fatalf("a buffer is left: %q", p.buffers())
		}
	})
	t.Run("the agent exited back to the shell", func(t *testing.T) {
		r := newRig(t)
		r.n++
		name := fmt.Sprintf("sh%d", r.n)
		target := r.srv.NewSession(name)
		ctl, out := filepath.Join(r.dir, "ctl-sh"), filepath.Join(r.dir, "out-sh")
		for _, f := range []string{ctl, out} {
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		line := strings.Join(fakeagent.Argv(fakeagent.ModeLine, ctl, out), " ")
		r.srv.SendKeys(target, line, "Enter")
		p := &pane{r: r, target: target, ctl: ctl, out: out}
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.resolve(name)
			if p.ref.Command != "bash" && p.ref.Command != "sh" && p.ref.Command != "zsh" && p.ref.Command != "dash" || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		agentRef := p.ref
		r.srv.SendKeys(target, "x", "Enter")
		p.waitLog(func(l string) bool { return strings.Contains(l, "submitted") })
		p.send("exit")
		deadline = time.Now().Add(5 * time.Second)
		for {
			p.resolve(name)
			if p.ref.Command != agentRef.Command || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if p.ref.PID != agentRef.PID || p.ref.Command == agentRef.Command {
			t.Fatalf("the scenario needs the same pid and another command: agent %+v now %+v", agentRef, p.ref)
		}
		before := r.srv.Capture(target)
		_, err := r.h.PasteMarked(ctx, p.ref, "secret", agentRef)
		if !isChanged(err) {
			t.Fatalf("err = %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if got := r.srv.Capture(target); strings.Contains(got, "secret") || got != before && strings.Contains(got, "sec") {
			t.Fatalf("the shell received bytes:\n%s", got)
		}
		if p.buffers() != "" {
			t.Fatalf("a buffer is left: %q", p.buffers())
		}
	})
	t.Run("command names with format characters paste", func(t *testing.T) {
		for i, name := range []string{"a,b}c#d:e{f", "x#{pane_id}y", "p,q", "r}s", "t#u", "v:w"} {
			name := name
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				r := newRig(t)
				exe := filepath.Join(r.dir, name)
				if err := os.Symlink(os.Args[0], exe); err != nil {
					t.Fatal(err)
				}
				ctl, out := filepath.Join(r.dir, "ctl"), filepath.Join(r.dir, "out")
				for _, f := range []string{ctl, out} {
					if err := os.WriteFile(f, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				r.n++
				sess := fmt.Sprintf("n%d", r.n)
				target := r.srv.NewSession(sess, fakeagent.ArgvFor(exe, fakeagent.ModeDump, ctl, out)...)
				p := &pane{r: r, target: target, ctl: ctl, out: out}
				p.resolve(sess)
				p.waitShown("ready")
				if p.ref.Command != name {
					t.Skipf("this platform reports %q for the command, not the name %q", p.ref.Command, name)
				}
				if _, err := r.h.PasteMarked(ctx, p.ref, "hi", p.ref); err != nil {
					t.Fatal(err)
				}
				p.waitLog(func(l string) bool { return l == hexOf("hi") })
				// A different name is refused.
				wrong := p.ref
				wrong.Command += "x"
				if _, err := r.h.PasteMarked(ctx, p.ref, "no", wrong); !isChanged(err) {
					t.Fatalf("a different command: %v", err)
				}
			})
		}
	})
}

func isChanged(err error) bool {
	return errors.Is(err, term.ErrPaneChanged)
}

// Atomicity (tmux 3.8): a pane that prints continuously. In one invocation the
// evidence and the rows are one moment, so every row of a mark or a frame has
// the text of its absolute index.
func TestMarksAndFramesAreAtomic(t *testing.T) {
	r := newRig(t)
	if !r.h.TurnsSupported() {
		t.Skipf("tmux %s has no row counters", r.h.Version())
	}
	r.tmux("set-option", "-g", "history-limit", "100000")
	r.n++
	name := "busy"
	r.srv.NewSession(name, "sh", "-c", `stty -echo; i=0; while :; do echo "row $i"; i=$((i+1)); done`)
	ref, err := r.h.ResolvePane(context.Background(), ops.Target{SessionName: name})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(what string, ev term.PaneEvidence, start int64, rows []string) {
		t.Helper()
		// Row index n holds "row n" (the pane's output starts at row 0), except
		// the rows at and below the cursor, which are being written.
		for i, row := range rows {
			idx := start + int64(i)
			if idx >= ev.ScreenTop()+int64(ev.CursorY) {
				break
			}
			if want := fmt.Sprintf("row %d", idx); row != want {
				t.Fatalf("%s: row at index %d is %q, want %q (top %d, collected %d, size %d)", what, idx, row, want, ev.ScreenTop(), ev.Collected, ev.HistorySize)
			}
		}
	}
	for i := 0; i < 100; i++ {
		mark, err := r.h.PasteMarked(ctx, ref, ".", ref)
		if err != nil {
			t.Fatal(err)
		}
		check("mark", mark.PaneEvidence, mark.From, mark.Rows)
		// The row at the cursor's index, read later, is the one the mark
		// holds.
		idx := mark.ScreenTop() + int64(max(mark.CursorY-1, 0))
		fr, err := r.h.ReadFrame(ctx, ref, idx)
		if err != nil {
			t.Fatal(err)
		}
		// A start on the visible rows is cut to the first visible row, so the
		// row is found by its absolute index, not at position 0.
		if k := idx - fr.Start(); k < 0 || k >= int64(len(fr.Rows)) {
			t.Fatalf("the frame from %d holds rows %d to %d, not the row at index %d", fr.Start(), fr.Start(), fr.LastRow(), idx)
		} else if want := mark.Rows[idx-mark.From]; fr.Rows[k] != want {
			t.Fatalf("the row at index %d was %q at the mark and %q later", idx, want, fr.Rows[k])
		}
		check("frame", fr.PaneEvidence, fr.Start(), fr.Rows)
		// The format-expanded start reads the row that was asked for, also
		// from history, many rows below the mark.
		old := mark.ScreenTop() - 20
		if old >= 0 {
			fr, err = r.h.ReadFrame(ctx, ref, old)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("row %d", old); fr.Rows[0] != want || fr.Start() != old {
				t.Fatalf("a frame from %d starts at %d with %q, want %q", old, fr.Start(), fr.Rows[0], want)
			}
		}
	}
}

// A user hook on after-paste-buffer that fails makes the invocation exit nonzero
// after the paste. The paste happened, and the ok token says so: the delivery
// is a success, not "nothing sent".
func TestPasteMarkedFailingAfterPasteHookIsStillDelivered(t *testing.T) {
	for name, hook := range map[string]string{
		"a command whose target pane is not found": "set-option -p -t %999 @x 1",
		"a shell command that fails":               `run-shell "exit 3"`,
	} {
		hook := hook
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			p := r.agent(fakeagent.ModeDump)
			r.tmux("set-hook", "-g", "after-paste-buffer", hook)
			_, err := r.h.PasteMarked(context.Background(), p.ref, "hello", p.ref)
			if err != nil {
				t.Fatalf("err = %v: the message was pasted", err)
			}
			p.waitLog(func(l string) bool { return l == hexOf("hello") })
			if got := p.buffers(); got != "" {
				t.Fatalf("a buffer is left: %q", got)
			}
			// A hook that prints (run-shell reports a failure on the standard
			// output) leaves a mark that cannot be read; the turn is then
			// unavailable, the delivery is not.
		})
	}
}
