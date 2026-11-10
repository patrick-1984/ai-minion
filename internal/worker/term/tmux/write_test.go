package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// script is an ExecFunc recorder whose answers are computed per invocation.
// The handler receives the tmux subcommand, the arguments after "-S <socket>
// -u" and stdin, and returns stdout or an error (stderr is the error text).
type script struct {
	mu      sync.Mutex
	calls   [][]string // arguments after "-S <socket> -u"
	stdins  [][]byte
	handler func(sub string, args []string, stdin []byte) (string, error)
}

func (s *script) exec(_ context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) >= 1 && args[len(args)-1] == "-V" {
		return []byte("tmux 3.5a\n"), nil, nil
	}
	rest := append([]string(nil), args[3:]...)
	s.calls = append(s.calls, rest)
	s.stdins = append(s.stdins, stdin)
	out, err := s.handler(rest[0], rest, stdin)
	if err != nil {
		return nil, []byte(err.Error()), errExit1
	}
	return []byte(out), nil, nil
}

func (s *script) host(t *testing.T) *Host {
	t.Helper()
	h, err := New(fakeSocket, WithExec(s.exec))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// subs returns the subcommands invoked so far, in order.
func (s *script) subs() []string {
	var out []string
	for _, c := range s.calls {
		out = append(out, c[0])
	}
	return out
}

func (s *script) count(sub string) int {
	n := 0
	for _, c := range s.calls {
		if c[0] == sub {
			n++
		}
	}
	return n
}

var paneA = term.PaneRef{ID: "%4", PID: 4242, Command: "cat"}

// stateOf renders the display-message answer for a pane state.
func stateOf(p term.PaneRef) string { return fmt.Sprintf("%s %d 0 0 0 0 %s\n", p.ID, p.PID, p.Command) }

// flagState renders the display-message answer for a pane with the dead,
// input_off and in_mode flags set as given.
func flagState(p term.PaneRef, dead, inputOff, inMode int) string {
	return fmt.Sprintf("%s %d %d %d %d 0 %s\n", p.ID, p.PID, dead, inputOff, inMode, p.Command)
}

// livePane is a handler that answers the KEEP re-check with *live and accepts
// every other command.
func livePane(live *term.PaneRef) func(string, []string, []byte) (string, error) {
	return func(sub string, _ []string, _ []byte) (string, error) {
		if sub == "display-message" {
			return stateOf(*live), nil
		}
		return "", nil
	}
}

// pasteSim answers the one-invocation paste of PasteText the way tmux does:
// the evidence line, then the KEEP condition of the if-shell evaluated against
// the live pane, then the ok or the refused token. Every invocation whose
// condition held is a paste tmux ran; sim.pastes counts them.
type pasteSim struct {
	mu     sync.Mutex
	expect term.PaneRef
	live   func() (ref term.PaneRef, dead, inputOff bool)
	pastes int
	// deleted counts the refusals: the else branch of the if-shell deletes
	// the loaded buffer in the same invocation.
	deleted int
	// onLoad runs when load-buffer is invoked, so that a test can change the
	// pane while the text loads.
	onLoad func()
	// failInvocation makes the paste invocation fail without any output.
	failInvocation error
}

func (p *pasteSim) handler(sub string, args []string, _ []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch sub {
	case "load-buffer":
		if p.onLoad != nil {
			p.onLoad()
		}
	case "display-message":
		if p.failInvocation != nil {
			return "", p.failInvocation
		}
		ref, dead, off := p.live()
		flags := "0\t0\t0"
		if dead {
			flags = "1\t0\t0"
		} else if off {
			flags = "0\t1\t0"
		}
		out := evLine(ref, 24, 0, "", "", flags) + "\n"
		okTok, refTok := "", ""
		for _, a := range args {
			if m := tokenRE.FindStringSubmatch(a); m != nil {
				if strings.Contains(a, "paste-buffer") {
					okTok = m[1]
				} else {
					refTok = m[1]
				}
			}
		}
		if ref.PID == p.expect.PID && ref.Command == p.expect.Command && !dead && !off {
			p.pastes++
			return out + okTok + "\n", nil
		}
		p.deleted++
		return out + refTok + "\n", nil
	}
	return "", nil
}

func (p *pasteSim) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pastes
}

func simFor(live *term.PaneRef) *pasteSim {
	return &pasteSim{expect: paneA, live: func() (term.PaneRef, bool, bool) { return *live, false, false }}
}

func TestPasteTextRechecksBeforeSending(t *testing.T) {
	for name, live := range map[string]term.PaneRef{
		"pid changed":     {ID: "%4", PID: 9999, Command: "cat"},
		"command changed": {ID: "%4", PID: 4242, Command: "vim"},
	} {
		t.Run(name, func(t *testing.T) {
			live := live
			sim := simFor(&live)
			s := &script{handler: sim.handler}
			err := s.host(t).PasteText(context.Background(), paneA, "secret text", paneA)
			if !errors.Is(err, term.ErrPaneChanged) {
				t.Fatalf("err = %v, want ErrPaneChanged", err)
			}
			if sim.count() != 0 {
				t.Error("pasted into a pane that changed")
			}
			if sim.deleted != 1 {
				t.Errorf("loaded buffer not deleted: %v", s.subs())
			}
		})
	}
}

func TestPasteTextGonePaneIsPaneChanged(t *testing.T) {
	s := &script{handler: func(sub string, _ []string, _ []byte) (string, error) {
		if sub == "display-message" {
			return "", errors.New("can't find pane: %4")
		}
		return "", nil
	}}
	err := s.host(t).PasteText(context.Background(), paneA, "x", paneA)
	if !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("err = %v, want ErrPaneChanged", err)
	}
	if s.count("delete-buffer") != 1 {
		t.Errorf("the loaded buffer was not deleted: %v", s.subs())
	}
}

func TestPasteTextExpectForAnotherPane(t *testing.T) {
	live := paneA
	sim := simFor(&live)
	s := &script{handler: sim.handler}
	other := term.PaneRef{ID: "%5", PID: 4242, Command: "cat"}
	err := s.host(t).PasteText(context.Background(), paneA, "x", other)
	if !errors.Is(err, term.ErrPaneChanged) || len(s.calls) != 0 {
		t.Fatalf("err = %v calls = %v", err, s.calls)
	}
}

func TestPasteTextCommandsAndNoContentOnArgv(t *testing.T) {
	live := paneA
	sim := simFor(&live)
	s := &script{handler: sim.handler}
	h := s.host(t)
	text := "line one \"quoted\" 'single' $HOME `tick` ; -rf\n\u00fcn\u00ef 🙂 \\ done\n"
	if err := h.PasteText(context.Background(), paneA, text, paneA); err != nil {
		t.Fatal(err)
	}
	// The text is loaded first; then ONE invocation checks the pane and
	// pastes, so nothing can change between the check and the paste.
	want := []string{"load-buffer", "display-message"}
	if got := s.subs(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("invocations = %v, want %v", got, want)
	}
	load, run := s.calls[0], strings.Join(s.calls[1], " ")
	if len(load) != 4 || load[1] != "-b" || load[3] != "-" || !strings.HasPrefix(load[2], "minion-") {
		t.Errorf("load-buffer args = %v, want load-buffer -b <unique> -", load)
	}
	if !strings.Contains(run, "paste-buffer -d -p -r -b "+load[2]+" -t %4") {
		t.Errorf("the paste is not bracketed, verbatim and bound to the buffer: %s", run)
	}
	if sim.count() != 1 {
		t.Errorf("pastes = %d, want 1", sim.count())
	}
	if string(s.stdins[0]) != text {
		t.Errorf("load-buffer stdin = %q, want the text", s.stdins[0])
	}
	for i, c := range s.calls {
		for _, a := range c {
			for _, frag := range []string{"line one", "quoted", "\u00fcn\u00ef", "🙂", "$HOME", "done"} {
				if strings.Contains(a, frag) {
					t.Errorf("invocation %d carries content %q on argv: %v", i, frag, c)
				}
			}
		}
		if i != 0 && s.stdins[i] != nil {
			t.Errorf("invocation %d (%s) got stdin", i, c[0])
		}
	}
}

func TestPasteTextBufferNamesAreUnique(t *testing.T) {
	live := paneA
	sim := simFor(&live)
	s := &script{handler: sim.handler}
	h := s.host(t)
	seen := map[string]bool{}
	for range 20 {
		if err := h.PasteText(context.Background(), paneA, "x", paneA); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range s.calls {
		if c[0] == "load-buffer" {
			if seen[c[2]] {
				t.Fatalf("buffer name %s reused", c[2])
			}
			seen[c[2]] = true
		}
	}
	if len(seen) != 20 {
		t.Errorf("%d distinct buffer names, want 20", len(seen))
	}
}

func TestPasteTextChangeDuringLoadSendsNothing(t *testing.T) {
	live := paneA
	sim := simFor(&live)
	sim.onLoad = func() { live = term.PaneRef{ID: "%4", PID: 77, Command: "vim"} } // replaced while loading
	s := &script{handler: sim.handler}
	err := s.host(t).PasteText(context.Background(), paneA, "x", paneA)
	if !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("err = %v", err)
	}
	if sim.count() != 0 {
		t.Error("pasted into a pane that changed")
	}
	if sim.deleted != 1 {
		t.Errorf("loaded buffer not deleted: %v", s.subs())
	}
}

func TestPasteTextFailuresDeleteTheBuffer(t *testing.T) {
	live := paneA
	for _, failing := range []string{"load-buffer", "display-message"} {
		t.Run(failing, func(t *testing.T) {
			sim := simFor(&live)
			s := &script{}
			s.handler = func(sub string, a []string, in []byte) (string, error) {
				if sub == failing {
					return "", errors.New("boom")
				}
				return sim.handler(sub, a, in)
			}
			err := s.host(t).PasteText(context.Background(), paneA, "x", paneA)
			if err == nil || errors.Is(err, term.ErrPaneChanged) {
				t.Fatalf("err = %v, want a plain tmux error", err)
			}
			if s.count("delete-buffer") != 1 {
				t.Errorf("invocations = %v, want one delete-buffer", s.subs())
			}
			if failing == "load-buffer" && s.count("display-message") != 0 {
				t.Error("pasted after a failed load")
			}
		})
	}
}

func TestPasteTextRejectsBadInput(t *testing.T) {
	live := paneA
	sim := simFor(&live)
	s := &script{handler: sim.handler}
	h := s.host(t)
	ctx := context.Background()
	bad := map[string]struct {
		pane term.PaneRef
		text string
	}{
		"empty text":   {paneA, ""},
		"bad pane id":  {term.PaneRef{ID: "4"}, "x"},
		"pane flag":    {term.PaneRef{ID: "-t"}, "x"},
		"invalid utf8": {paneA, "a\xffb"},
		"too large":    {paneA, strings.Repeat("a", MaxPasteBytes+1)},
	}
	for name, tc := range bad {
		if err := h.PasteText(ctx, tc.pane, tc.text, tc.pane); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("tmux ran for invalid input: %v", s.subs())
	}
}

func TestSendKeysStopsWhenPaneChanges(t *testing.T) {
	live := paneA
	s := &script{}
	s.handler = func(sub string, a []string, in []byte) (string, error) {
		if sub == "send-keys" {
			live = term.PaneRef{ID: "%4", PID: 5555, Command: "bash"} // replaced after the first key
		}
		return livePane(&live)(sub, a, in)
	}
	sent, err := s.host(t).SendKeys(context.Background(), paneA, []string{"a", "b", "Enter"}, paneA)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 1 {
		t.Fatalf("sent = %d err = %v, want 1 and ErrPaneChanged", sent, err)
	}
	if n := s.count("send-keys"); n != 1 {
		t.Errorf("%d send-keys invocations, want 1", n)
	}
}

func TestSendKeysChangedBeforeFirstKeySendsNone(t *testing.T) {
	live := term.PaneRef{ID: "%4", PID: 1, Command: "other"}
	s := &script{handler: livePane(&live)}
	sent, err := s.host(t).SendKeys(context.Background(), paneA, []string{"Enter"}, paneA)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 0 || s.count("send-keys") != 0 {
		t.Fatalf("sent = %d err = %v calls = %v", sent, err, s.subs())
	}
}

func TestSendKeysAllDelivered(t *testing.T) {
	live := paneA
	s := &script{handler: livePane(&live)}
	keys := []string{"h", "C-c", "Enter", ";", "-", "\u00fc"}
	sent, err := s.host(t).SendKeys(context.Background(), paneA, keys, paneA)
	if err != nil || sent != len(keys) {
		t.Fatalf("sent = %d err = %v", sent, err)
	}
	// One re-check before every key.
	var got []string
	for _, c := range s.calls {
		got = append(got, c[0])
	}
	want := strings.Repeat("display-message send-keys ", len(keys))
	if strings.Join(got, " ")+" " != want {
		t.Errorf("invocations = %v", got)
	}
	var args [][]string
	for _, c := range s.calls {
		if c[0] == "send-keys" {
			args = append(args, c)
		}
	}
	if fmt.Sprint(args[3]) != fmt.Sprint([]string{"send-keys", "-t", "%4", "--", `\;`}) {
		t.Errorf("a semicolon key must be escaped for tmux: %v", args[3])
	}
	if fmt.Sprint(args[4]) != fmt.Sprint([]string{"send-keys", "-t", "%4", "--", "-"}) {
		t.Errorf("a dash key must come after --: %v", args[4])
	}
}

func TestSendKeysErrorReportsCountSoFar(t *testing.T) {
	live := paneA
	n := 0
	s := &script{}
	s.handler = func(sub string, a []string, in []byte) (string, error) {
		if sub == "send-keys" {
			if n++; n == 3 {
				return "", errors.New("boom")
			}
		}
		return livePane(&live)(sub, a, in)
	}
	sent, err := s.host(t).SendKeys(context.Background(), paneA, []string{"a", "b", "c", "d"}, paneA)
	if err == nil || errors.Is(err, term.ErrPaneChanged) || sent != 2 {
		t.Fatalf("sent = %d err = %v, want 2 and a plain error", sent, err)
	}
}

func TestSendKeysRejectsBadInput(t *testing.T) {
	live := paneA
	s := &script{handler: livePane(&live)}
	h := s.host(t)
	ctx := context.Background()
	cases := map[string][]string{
		"none":        nil,
		"empty key":   {"Enter", ""},
		"long key":    {strings.Repeat("a", ops.MaxKeyLen+1)},
		"control":     {"a", "\x1b"},
		"newline":     {"\n"},
		"invalid utf": {"\xff"},
	}
	for name, keys := range cases {
		sent, err := h.SendKeys(ctx, paneA, keys, paneA)
		if !errors.Is(err, ops.ErrInvalidArgument) || sent != 0 {
			t.Errorf("%s: sent = %d err = %v", name, sent, err)
		}
	}
	if _, err := h.SendKeys(ctx, term.PaneRef{ID: "x"}, []string{"a"}, paneA); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("bad pane id: err = %v", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("tmux ran for invalid input: %v", s.subs())
	}
}

func TestParsePaneState(t *testing.T) {
	got, err := parsePaneState("%3 123 0 0 0 0 my cmd; with\nnewline\n")
	want := term.PaneRef{ID: "%3", PID: 123, Command: "my cmd; with\nnewline"}
	if err != nil || got != (paneState{ref: want}) {
		t.Errorf("got %+v err %v", got, err)
	}
	if got, err := parsePaneState("%3 5 0 0 0 0 \n"); err != nil || got.ref.Command != "" {
		t.Errorf("empty command: %+v %v", got, err)
	}
	got, err = parsePaneState("%3 5 1 1 1 1 cat\n")
	if err != nil || !got.dead || !got.inputOff || !got.inMode || !got.synced {
		t.Errorf("flags: %+v %v", got, err)
	}
	// tmux 3.8 prints an empty pane_pid for a dead pane; the pid reads as 0.
	got, err = parsePaneState("%3  1 0 0 0 true\n")
	if err != nil || !got.dead || got.ref.PID != 0 || got.ref.Command != "true" {
		t.Errorf("dead pane without pid: %+v %v", got, err)
	}
	// An empty pid is an error for a pane that is not dead.
	if _, err := parsePaneState("%3  0 0 0 0 cat\n"); err == nil {
		t.Error("empty pid of a live pane was accepted")
	}
	// tmux prints empty fields, with success, for a pane that does not exist.
	if _, err := parsePaneState("      \n"); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("empty fields: err = %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"", "%3 5", "%3 5 0 0 0 0", "%3 x 0 0 0 0 cat\n", "%3 5 2 0 0 0 cat\n", "%3 5 0 x 0 0 cat\n", "%3 5 0 0  0 cat\n", "%3 5 0 0 0 2 cat\n"} {
		if _, err := parsePaneState(bad); err == nil {
			t.Errorf("parsePaneState(%q) succeeded", bad)
		}
	}
}

// TestWritesRefuseStatesThatDropInput covers panes that keep their identity
// while tmux silently discards what is sent to them.
func TestWritesRefuseStatesThatDropInput(t *testing.T) {
	cases := []struct {
		name                 string
		dead, inputOff, mode int
		pasteRefused         bool
	}{
		{"dead pane", 1, 0, 0, true},
		{"input disabled", 0, 1, 0, true},
		{"copy mode", 0, 0, 1, false}, // paste-buffer reaches the program in a mode
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &script{handler: func(sub string, _ []string, _ []byte) (string, error) {
				if sub == "display-message" {
					return flagState(paneA, tc.dead, tc.inputOff, tc.mode), nil
				}
				return "", nil
			}}
			h := s.host(t)
			ctx := context.Background()
			sent, err := h.SendKeys(ctx, paneA, []string{"a", "Enter"}, paneA)
			if !errors.Is(err, term.ErrPaneChanged) || sent != 0 || s.count("send-keys") != 0 {
				t.Errorf("SendKeys sent = %d err = %v calls = %v", sent, err, s.subs())
			}
			live := paneA
			sim := &pasteSim{expect: paneA, live: func() (term.PaneRef, bool, bool) { return live, tc.dead == 1, tc.inputOff == 1 }}
			ps := &script{handler: sim.handler}
			err = ps.host(t).PasteText(ctx, paneA, "x", paneA)
			if tc.pasteRefused {
				if !errors.Is(err, term.ErrPaneChanged) || sim.count() != 0 {
					t.Errorf("PasteText err = %v calls = %v", err, ps.subs())
				}
			} else if err != nil || sim.count() != 1 {
				t.Errorf("PasteText err = %v calls = %v, want a paste", err, ps.subs())
			}
		})
	}
}

// TestSendKeysRefusesSynchronizedPanes: with synchronize-panes on, tmux sends
// the keys to every pane of the window, so the keys are refused; a paste is
// not broadcast and goes through.
func TestSendKeysRefusesSynchronizedPanes(t *testing.T) {
	s := &script{handler: func(sub string, _ []string, _ []byte) (string, error) {
		if sub == "display-message" {
			return fmt.Sprintf("%s %d 0 0 0 1 %s\n", paneA.ID, paneA.PID, paneA.Command), nil
		}
		return "", nil
	}}
	h := s.host(t)
	ctx := context.Background()
	sent, err := h.SendKeys(ctx, paneA, []string{"C-c", "Enter"}, paneA)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 0 || s.count("send-keys") != 0 {
		t.Errorf("SendKeys sent = %d err = %v calls = %v", sent, err, s.subs())
	}
	live := paneA
	sim := simFor(&live)
	ps := &script{handler: sim.handler}
	if err := ps.host(t).PasteText(ctx, paneA, "x", paneA); err != nil || sim.count() != 1 {
		t.Errorf("PasteText err = %v calls = %v", err, ps.subs())
	}
	for _, c := range append(s.calls, ps.calls...) {
		if c[0] == "set-option" {
			t.Errorf("the user's configuration was changed: %v", c)
		}
	}
}

func TestSendKeysKeepsCountWhenPaneStopsTakingKeys(t *testing.T) {
	s := &script{}
	inMode := 0
	s.handler = func(sub string, _ []string, _ []byte) (string, error) {
		switch sub {
		case "send-keys":
			inMode = 1 // copy mode starts after the first key
		case "display-message":
			return flagState(paneA, 0, 0, inMode), nil
		}
		return "", nil
	}
	sent, err := s.host(t).SendKeys(context.Background(), paneA, []string{"a", "b", "c"}, paneA)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 1 {
		t.Fatalf("sent = %d err = %v, want 1 and ErrPaneChanged", sent, err)
	}
}

func TestPasteTextInputDisabledDuringLoadSendsNothing(t *testing.T) {
	off := false
	live := paneA
	sim := &pasteSim{expect: paneA, live: func() (term.PaneRef, bool, bool) { return live, false, off }}
	sim.onLoad = func() { off = true }
	s := &script{handler: sim.handler}
	err := s.host(t).PasteText(context.Background(), paneA, "x", paneA)
	if !errors.Is(err, term.ErrPaneChanged) || sim.count() != 0 || sim.deleted != 1 {
		t.Fatalf("err = %v calls = %v", err, s.subs())
	}
}

func TestEscapeArg(t *testing.T) {
	for in, want := range map[string]string{"a": "a", ";": `\;`, "x;": `x\;`, `x\;`: `x\\;`, "a;b": "a;b"} {
		if got := escapeArg(in); got != want {
			t.Errorf("escapeArg(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNeverKillsOrRenames is the grep test of AD-13: no production file of the
// package names a tmux command that destroys or renames a session.
func TestNeverKillsOrRenames(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %d", err, len(files))
	}
	banned := []string{"kill" + "-session", "kill" + "-server", "rename" + "-session", "rename" + "-window", "kill" + "-pane", "kill" + "-window"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range banned {
			if strings.Contains(string(b), w) {
				t.Errorf("%s mentions %q", f, w)
			}
		}
	}
}

// --- Integration: a real tmux on a private socket (tmuxtest). ---

func TestIntegrationPasteMultilineIntoCat(t *testing.T) {
	h, srv := newRealHost(t)
	target := srv.NewSession("paste-1", "cat")
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "paste-1"})
	if err != nil {
		t.Fatal(err)
	}
	text := "first \"double\" 'single' $HOME `tick` \\n ; -x\nsecond: \u00fcn\u00ef 🙂 \u65e5\u672c\u8a9e\nthird\n"
	if err := h.PasteText(ctx, ref, text, ref); err != nil {
		t.Fatal(err)
	}
	// cat echoes the typed text (tty echo) and prints it again: it shows up in
	// both forms, byte for byte.
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		srv.WaitFor(target, line, 5*time.Second)
	}
	if out, err := h.run(ctx, "list-buffers"); err != nil || len(out) != 0 {
		t.Errorf("paste buffers left behind: %q %v", out, err)
	}
	scr, err := h.Snapshot(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(scr.Lines, "\n"), "second: \u00fcn\u00ef 🙂 \u65e5\u672c\u8a9e"); n != 2 {
		t.Errorf("unicode line appears %d times, want 2 (echo and cat output):\n%s", n, strings.Join(scr.Lines, "\n"))
	}
}

func TestIntegrationPasteRefusedWhenProgramReplaced(t *testing.T) {
	h, srv := newRealHost(t)
	target := srv.NewSession("paste-2", "cat")
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "paste-2"})
	if err != nil {
		t.Fatal(err)
	}
	respawn(t, h, ref.ID, "sleep", "300")
	err = h.PasteText(ctx, ref, "must-not-arrive\n", ref)
	if !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("err = %v, want ErrPaneChanged", err)
	}
	time.Sleep(200 * time.Millisecond)
	if out := srv.Capture(target); strings.Contains(out, "must-not-arrive") {
		t.Errorf("text reached the replaced program:\n%s", out)
	}
}

// respawn replaces the program of a pane through the adapter's own exec path
// (private socket, no other server).
func respawn(t *testing.T, h *Host, pane string, argv ...string) {
	t.Helper()
	args := append([]string{"respawn-pane", "-k", "-t", pane, "--"}, argv...)
	if _, err := h.run(context.Background(), args...); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationSendKeysPartialWhenProgramReplaced(t *testing.T) {
	srv := tmuxtest.New(t)
	inner, err := New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var pane string
	wrapped := func(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
		out, errOut, err := inner.exec(ctx, args, stdin)
		if err == nil && len(args) > 3 && args[3] == "send-keys" {
			once.Do(func() { respawn(t, inner, pane, "sleep", "300") }) // replace the program after the first key
		}
		return out, errOut, err
	}
	h, err := New(srv.Socket(), WithExec(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	srv.NewSession("keys-1", "cat")
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "keys-1"})
	if err != nil {
		t.Fatal(err)
	}
	pane = ref.ID
	sent, err := h.SendKeys(ctx, ref, []string{"a", "b", "c"}, ref)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 1 {
		t.Fatalf("sent = %d err = %v, want 1 and ErrPaneChanged", sent, err)
	}
}

func TestIntegrationSendKeysDelivers(t *testing.T) {
	h, srv := newRealHost(t)
	target := srv.NewSession("keys-2", "cat")
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "keys-2"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"h", "i", "-", ";", "Space", "\u00fc", "Enter"}
	sent, err := h.SendKeys(ctx, ref, keys, ref)
	if err != nil || sent != len(keys) {
		t.Fatalf("sent = %d err = %v", sent, err)
	}
	srv.WaitFor(target, "hi-; \u00fc", 5*time.Second)
}

// waitSessionGone waits until ListSessions no longer reports name. The server
// keeps running (another session lives), so tmux answers a write to the dead
// pane id with empty fields and a success status, not with "no server".
func waitSessionGone(t *testing.T, h *Host, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ss, err := h.ListSessions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range ss {
			if s.Name == name {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %q still listed after 5s", name)
}

func TestIntegrationWritesToVanishedPane(t *testing.T) {
	h, srv := newRealHost(t)
	srv.NewSession("keep", "sleep", "300") // keeps the server running
	srv.NewSession("gone-1", "cat")
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "gone-1"})
	if err != nil {
		t.Fatal(err)
	}
	respawn(t, h, ref.ID, "true") // the pane ends and its session with it
	waitSessionGone(t, h, "gone-1")
	if err := h.PasteText(ctx, ref, "x", ref); !errors.Is(err, term.ErrPaneChanged) {
		t.Errorf("PasteText err = %v", err)
	}
	if sent, err := h.SendKeys(ctx, ref, []string{"a"}, ref); !errors.Is(err, term.ErrPaneChanged) || sent != 0 {
		t.Errorf("SendKeys sent = %d err = %v", sent, err)
	}
	if out, err := h.run(ctx, "list-buffers"); err != nil || len(out) != 0 {
		t.Errorf("paste buffers left behind: %q %v", out, err)
	}
}

// TestIntegrationWritesToPanesThatDropInput checks, on a real tmux, the three
// states that keep the pane identity while tmux discards input.
func TestIntegrationWritesToPanesThatDropInput(t *testing.T) {
	ctx := context.Background()
	setups := map[string]struct {
		prepare      func(t *testing.T, h *Host, pane string)
		pasteRefused bool
	}{
		"dead pane": {func(t *testing.T, h *Host, pane string) {
			if _, err := h.run(ctx, "set-option", "-p", "-t", pane, "remain-on-exit", "on"); err != nil {
				t.Fatal(err)
			}
			respawn(t, h, pane, "true")
			waitPaneDead(t, h, pane)
		}, true},
		"input disabled": {func(t *testing.T, h *Host, pane string) {
			if _, err := h.run(ctx, "select-pane", "-d", "-t", pane); err != nil {
				t.Fatal(err)
			}
		}, true},
		"copy mode": {func(t *testing.T, h *Host, pane string) {
			if _, err := h.run(ctx, "copy-mode", "-t", pane); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for name, su := range setups {
		t.Run(name, func(t *testing.T) {
			h, srv := newRealHost(t)
			srv.NewSession("keep", "sleep", "300")
			target := srv.NewSession("drop-1", "cat")
			ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "drop-1"})
			if err != nil {
				t.Fatal(err)
			}
			su.prepare(t, h, ref.ID)
			if name == "dead pane" {
				if ref, err = h.ResolvePane(ctx, ops.Target{SessionName: "drop-1"}); err != nil {
					t.Fatal(err)
				}
			}
			sent, err := h.SendKeys(ctx, ref, []string{"a", "b"}, ref)
			if !errors.Is(err, term.ErrPaneChanged) || sent != 0 {
				t.Errorf("SendKeys sent = %d err = %v, want 0 and ErrPaneChanged", sent, err)
			}
			err = h.PasteText(ctx, ref, "pasted-text\n", ref)
			if su.pasteRefused && !errors.Is(err, term.ErrPaneChanged) {
				t.Errorf("PasteText err = %v, want ErrPaneChanged", err)
			}
			if !su.pasteRefused && err != nil {
				t.Errorf("PasteText err = %v, want a paste", err)
			}
			if out, err := h.run(ctx, "list-buffers"); err != nil || len(out) != 0 {
				t.Errorf("paste buffers left behind: %q %v", out, err)
			}
			if !su.pasteRefused { // the paste really reached cat behind the mode
				if _, err := h.run(ctx, "send-keys", "-X", "-t", ref.ID, "cancel"); err != nil {
					t.Fatal(err)
				}
				srv.WaitFor(target, "pasted-text", 5*time.Second)
			}
		})
	}
}

// TestIntegrationSendKeysRefusedWhenPanesSynchronized reproduces the broadcast
// of send-keys to sibling panes on a real tmux: nothing may reach either pane,
// and the option stays on.
func TestIntegrationSendKeysRefusedWhenPanesSynchronized(t *testing.T) {
	ctx := context.Background()
	h, srv := newRealHost(t)
	target := srv.NewSession("sync-1", "cat")
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "sync-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(ctx, "split-window", "-d", "-t", ref.ID, "cat"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(ctx, "set-option", "-w", "-t", ref.ID, "synchronize-panes", "on"); err != nil {
		t.Fatal(err)
	}
	sent, err := h.SendKeys(ctx, ref, []string{"X", "Y", "Enter"}, ref)
	if !errors.Is(err, term.ErrPaneChanged) || sent != 0 {
		t.Fatalf("SendKeys sent = %d err = %v, want 0 and ErrPaneChanged", sent, err)
	}
	if out, err := h.run(ctx, "show-options", "-w", "-v", "-t", ref.ID, "synchronize-panes"); err != nil || strings.TrimSpace(string(out)) != "on" {
		t.Errorf("synchronize-panes = %q %v, want it left on", out, err)
	}
	// Turn it off for the check: a key to the target now reaches only the
	// target, which shows that anything the refused call had sent would
	// have been visible in both panes by now.
	time.Sleep(300 * time.Millisecond)
	if _, err := h.run(ctx, "set-option", "-w", "-t", ref.ID, "synchronize-panes", "off"); err != nil {
		t.Fatal(err)
	}
	if n, err := h.SendKeys(ctx, ref, []string{"Q"}, ref); err != nil || n != 1 {
		t.Fatalf("SendKeys after turning it off: %d %v", n, err)
	}
	srv.WaitFor(target, "Q", 5*time.Second)
	panes, err := h.run(ctx, "list-panes", "-t", ref.ID, "-F", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(panes))
	if len(ids) != 2 {
		t.Fatalf("panes = %q, want 2", panes)
	}
	for _, id := range ids {
		out, err := h.run(ctx, "capture-pane", "-p", "-t", id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(string(out), "XY") {
			t.Errorf("pane %s received refused input: %q", id, out)
		}
		if got := strings.Contains(string(out), "Q"); got != (id == ref.ID) {
			t.Errorf("pane %s has Q = %v, want only the target to have it", id, got)
		}
	}
}

func waitPaneDead(t *testing.T, h *Host, pane string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := h.currentState(context.Background(), pane); err == nil && st.dead {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pane %s did not become dead", pane)
}
