package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// versioned answers "tmux -V" with ver and hands every other invocation to s.
func versioned(ver string, s *script) ExecFunc {
	return func(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
		if len(args) >= 1 && args[len(args)-1] == "-V" {
			return []byte("tmux " + ver + "\n"), nil, nil
		}
		return s.exec(ctx, args, stdin)
	}
}

func hostAt(t *testing.T, ver string, s *script) *Host {
	t.Helper()
	h, err := New(fakeSocket, WithExec(versioned(ver, s)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// evLine renders the evidence line for a pane of the given size with history.
func evLine(p term.PaneRef, h, hist int, collected, gen string, extra ...string) string {
	flags := "0\t0\t0" // dead, input_off, alternate_on
	if len(extra) > 0 {
		flags = extra[0]
	}
	return fmt.Sprintf("%s\t%d\t%s\t80\t%d\t3\t%d\t0\t2000\t%d\t%s\t%s\t%s", p.ID, p.PID, flags, h, h-1, hist, collected, gen, p.Command)
}

var tokenRE = regexp.MustCompile(`display-message -p (minion-(?:ok|refused)-[0-9a-f]+)`)

// paste is the handler of a PasteMarked invocation: it answers the combined
// command with the evidence, rows and the token chosen by result ("ok",
// "refused", "" for none).
func paste(ev string, rows []string, result string) func(string, []string, []byte) (string, error) {
	return func(sub string, args []string, _ []byte) (string, error) {
		if sub != "display-message" {
			return "", nil
		}
		out := ev + "\n"
		for _, r := range rows {
			out += r + "\n"
		}
		ok, refused := "", ""
		for _, a := range args {
			if m := tokenRE.FindStringSubmatch(a); m != nil {
				if strings.Contains(a, "paste-buffer") {
					ok = m[1]
				} else {
					refused = m[1]
				}
			}
		}
		switch result {
		case "ok":
			out += ok + "\n"
		case "refused":
			out += refused + "\n"
		}
		return out, nil
	}
}

func TestCheckPasteText(t *testing.T) {
	good := []string{"hello", "one\ntwo\tT", "$ #{x} 'q' \"q\" ; \\ ` \u00e9 \u65e5\u672c \U0001F600", strings.Repeat("a", 100)}
	for _, s := range good {
		if err := checkPasteText(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	bad := []string{"", "a\rb", "a\x1bb", "a\x00b", "a\x01b", "a\x7fb", "a\u0085b", "a\u009bb", "a\vb", "a\fb", "a\x08b", "\xff\xfe"}
	for _, s := range bad {
		if err := checkPasteText(s); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%q: err=%v, want ErrInvalidArgument", s, err)
		}
	}
	if err := checkPasteText(strings.Repeat("a", MaxPasteBytes+1)); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("an over-long text: %v", err)
	}
}

func TestKeepPredicateEscapesTheCommand(t *testing.T) {
	got := keepPredicate(term.PaneRef{ID: "%4", PID: 77, Command: "a,b}c#d:e{f"})
	want := "#{&&:#{==:#{pane_pid},77},#{&&:#{==:#{pane_current_command},a#,b#}c##d:e{f},#{&&:#{==:#{pane_dead},0},#{==:#{pane_input_off},0}}}}"
	if got != want {
		t.Fatalf("predicate\n got %s\nwant %s", got, want)
	}
	if got := formatEscape("x#{pane_id}y"); got != "x##{pane_id#}y" {
		t.Fatalf("escape = %q", got)
	}
}

func TestParseEvidence(t *testing.T) {
	ev, err := parseEvidence(evLine(paneA, 24, 61, "40", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Width != 80 || ev.Height != 24 || ev.HistorySize != 61 || ev.Collected != 40 || ev.Generation != 2 || ev.Command != "cat" || ev.PID != 4242 {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev.ScreenTop() != 101 || ev.LastRow() != 124 {
		t.Fatalf("top=%d last=%d", ev.ScreenTop(), ev.LastRow())
	}
	// Before 3.8 the counters expand to nothing.
	ev, err = parseEvidence(evLine(paneA, 24, 61, "", ""))
	if err != nil || ev.HasCounters() || ev.Collected != -1 || ev.Generation != -1 {
		t.Fatalf("evidence = %+v err=%v", ev, err)
	}
	// A dead pane has no pid in 3.8; flags are read.
	ev, err = parseEvidence(strings.Replace(evLine(paneA, 24, 0, "0", "0", "1\t1\t1"), "%4\t4242", "%4\t", 1))
	if err != nil || ev.PID != 0 || !ev.Dead || !ev.InputOff || !ev.Alternate {
		t.Fatalf("evidence = %+v err=%v", ev, err)
	}
	// A command may hold tabs: it is the last field.
	ev, err = parseEvidence(evLine(term.PaneRef{ID: "%4", PID: 1, Command: "a\tb"}, 24, 0, "0", "0"))
	if err != nil || ev.Command != "a\tb" {
		t.Fatalf("evidence = %+v err=%v", ev, err)
	}
	for name, line := range map[string]string{
		"too few fields": "%4\t1\t0",
		"empty pane":     strings.Repeat("\t", 14),
		"bad number":     strings.Replace(evLine(paneA, 24, 0, "0", "0"), "\t80\t", "\tx\t", 1),
		"bad flag":       evLine(paneA, 24, 0, "0", "0", "2\t0\t0"),
		"no height":      evLine(paneA, 0, 0, "0", "0"),
	} {
		if _, err := parseEvidence(line); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseEvidence(strings.Repeat("\t", 14)); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("an empty answer is a missing pane: %v", err)
	}
}

func TestPasteMarkedOneInvocation(t *testing.T) {
	rows := []string{"a", "b", "c"} // 1 history row and a 2-row screen
	s := &script{handler: paste(evLine(paneA, 2, 1, "10", "0"), rows, "ok")}
	h := hostAt(t, "3.8", s)
	mark, err := h.PasteMarked(context.Background(), paneA, "hello\nworld", paneA)
	if err != nil {
		t.Fatal(err)
	}
	// The text is loaded from stdin into a named buffer, then one invocation
	// reads the evidence, the rows and pastes.
	if got := s.subs(); len(got) != 2 || got[0] != "load-buffer" || got[1] != "display-message" {
		t.Fatalf("invocations = %v", got)
	}
	if string(s.stdins[0]) != "hello\nworld" {
		t.Fatalf("stdin = %q", s.stdins[0])
	}
	joined := strings.Join(s.calls[1], " ")
	for _, want := range []string{
		"display-message -p -t %4 ",
		"; capture-pane -p -t %4 -S -4 ;",
		"; if-shell -F -t %4 #{&&:#{==:#{pane_pid},4242},#{&&:#{==:#{pane_current_command},cat},",
		"paste-buffer -d -p -r -b minion-",
		" -t %4 ; display-message -p minion-ok-",
		"delete-buffer -b minion-",
		"; display-message -p minion-refused-",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the invocation lacks %q:\n%s", want, joined)
		}
	}
	if mark.PaneID != "%4" || mark.Collected != 10 || mark.HistorySize != 1 || mark.From != 10+1-1 {
		t.Fatalf("mark = %+v", mark)
	}
	if strings.Join(mark.Rows, ",") != "a,b,c" || mark.From != 10 {
		t.Fatalf("rows = %q from %d", mark.Rows, mark.From)
	}
	if s.count("delete-buffer") != 0 {
		t.Fatal("the buffer is deleted by paste-buffer -d, not separately")
	}
}

func TestPasteMarkedMarkHoldsFourHistoryRows(t *testing.T) {
	rows := []string{"h6", "h7", "h8", "h9", "s0", "s1"}
	s := &script{handler: paste(evLine(paneA, 2, 10, "100", "1"), rows, "ok")}
	mark, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
	if err != nil {
		t.Fatal(err)
	}
	if mark.From != 106 || mark.ScreenTop() != 110 || len(mark.Rows) != 6 {
		t.Fatalf("from=%d top=%d rows=%d", mark.From, mark.ScreenTop(), len(mark.Rows))
	}
}

func TestPasteMarkedRefusedIsPaneChanged(t *testing.T) {
	for name, c := range map[string]struct {
		ev   string
		want string
	}{
		"command":   {evLine(term.PaneRef{ID: "%4", PID: 4242, Command: "bash"}, 2, 0, "0", "0"), `"bash"`},
		"pid":       {evLine(term.PaneRef{ID: "%4", PID: 9, Command: "cat"}, 2, 0, "0", "0"), "pid 9"},
		"dead":      {evLine(paneA, 2, 0, "0", "0", "1\t0\t0"), "program has ended"},
		"input off": {evLine(paneA, 2, 0, "0", "0", "0\t1\t0"), "input is disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{handler: paste(c.ev, []string{"", ""}, "refused")}
			mark, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
			if !errors.Is(err, term.ErrPaneChanged) || !strings.Contains(err.Error(), c.want) || len(mark.Rows) != 0 {
				t.Fatalf("mark=%+v err=%v", mark, err)
			}
		})
	}
}

func TestPasteMarkedNoTokenIsUnknown(t *testing.T) {
	for name, rows := range map[string][]string{"no rows": nil, "rows": {"a", "b"}} {
		t.Run(name, func(t *testing.T) {
			s := &script{handler: paste(evLine(paneA, 2, 0, "0", "0"), rows, "")}
			_, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
			if err == nil || errors.Is(err, term.ErrPaneChanged) || errors.Is(err, ops.ErrInvalidArgument) {
				t.Fatalf("err = %v, want an unknown-outcome error", err)
			}
			if s.count("delete-buffer") != 1 {
				t.Fatalf("the buffer must be cleaned up: %v", s.subs())
			}
		})
	}
	// A screen row that looks like a token does not count: tokens are random.
	s := &script{handler: paste(evLine(paneA, 2, 0, "0", "0"), []string{"minion-ok-0000", "minion-refused-0000"}, "")}
	if _, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA); err == nil {
		t.Fatal("a forged token was accepted")
	}
}

// The ok token proves the paste happened. Whatever else fails is not an unknown
// outcome: the mark is empty and the extractor retires it as no_evidence, so
// that the caller still sends Enter.
func TestPasteMarkedPastedButUnreadableMarkIsNotAnError(t *testing.T) {
	good := evLine(paneA, 3, 0, "0", "0")
	for name, c := range map[string]struct {
		ev   string
		rows []string
	}{
		"garbled evidence":      {"%4\tnot\tenough", nil},
		"a non-numeric field":   {strings.Replace(good, "\t3\t", "\tx\t", 1), []string{"a", "b", "c"}},
		"too many rows":         {good, []string{"a", "b", "c", "d", "e", "f", "g"}},
		"an implausible height": {strings.Replace(good, "\t80\t3\t", "\t80\t0\t", 1), nil},
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{handler: paste(c.ev, c.rows, "ok")}
			mark, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
			if err != nil {
				t.Fatalf("err = %v, want nil: the paste happened", err)
			}
			if mark.Height != 0 || len(mark.Rows) != 0 {
				t.Fatalf("mark = %+v, want an empty mark", mark)
			}
		})
	}
	// Only the ok token, not even an evidence line.
	s := &script{handler: func(sub string, args []string, _ []byte) (string, error) {
		if sub != "display-message" {
			return "", nil
		}
		for _, a := range args {
			if m := tokenRE.FindStringSubmatch(a); m != nil && strings.Contains(a, "paste-buffer") {
				return m[1] + "\n", nil
			}
		}
		return "", nil
	}}
	if _, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA); err != nil {
		t.Fatalf("only the ok token: err = %v", err)
	}
}

// The refused token is checked before the evidence is parsed: a refusal is
// always ErrPaneChanged, nothing was sent.
func TestPasteMarkedRefusedWithUnreadableEvidenceIsStillPaneChanged(t *testing.T) {
	for name, ev := range map[string]string{"garbled": "%4\tx", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			s := &script{handler: paste(ev, []string{"a"}, "refused")}
			_, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
			if !errors.Is(err, term.ErrPaneChanged) {
				t.Fatalf("err = %v, want ErrPaneChanged", err)
			}
		})
	}
}

func TestPasteMarkedRefusesBeforeSending(t *testing.T) {
	s := &script{handler: livePane(&paneA)}
	h := hostAt(t, "3.8", s)
	for _, text := range []string{"", "a\rb", "a\x1b[201~b", "a\x00", "a\u0085"} {
		if _, err := h.PasteMarked(context.Background(), paneA, text, paneA); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%q: err = %v", text, err)
		}
	}
	if _, err := h.PasteMarked(context.Background(), term.PaneRef{ID: "x"}, "a", paneA); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("bad pane id: %v", err)
	}
	other := paneA
	other.ID = "%9"
	if _, err := h.PasteMarked(context.Background(), paneA, "a", other); !errors.Is(err, term.ErrPaneChanged) {
		t.Errorf("a different expected pane: %v", err)
	}
	ctl := paneA
	ctl.Command = "a\x01b"
	if _, err := h.PasteMarked(context.Background(), paneA, "a", ctl); !errors.Is(err, term.ErrPaneChanged) {
		t.Errorf("a command with a control character: %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("invocations before the refusals: %v", s.subs())
	}
}

func TestPasteMarkedGonePane(t *testing.T) {
	s := &script{handler: func(sub string, _ []string, _ []byte) (string, error) {
		if sub == "display-message" {
			return "", errors.New("can't find pane: %4")
		}
		return "", nil
	}}
	_, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
	if !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("err = %v", err)
	}
	if s.count("delete-buffer") != 1 {
		t.Fatalf("the buffer must be cleaned up: %v", s.subs())
	}
}

func TestPasteMarkedRowCount(t *testing.T) {
	ev := evLine(paneA, 3, 0, "0", "0")
	// tmux may leave out blank rows at the end.
	s := &script{handler: paste(ev, []string{"a"}, "ok")}
	mark, err := hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
	if err != nil || len(mark.Rows) != 3 || mark.Rows[0] != "a" || mark.Rows[2] != "" {
		t.Fatalf("mark=%+v err=%v", mark, err)
	}
	// More rows than the evidence implies is not a capture of this pane; the
	// paste still happened, so the mark is empty (no_evidence) and not an error.
	s = &script{handler: paste(ev, []string{"a", "b", "c", "d"}, "ok")}
	mark, err = hostAt(t, "3.8", s).PasteMarked(context.Background(), paneA, "x", paneA)
	if err != nil || len(mark.Rows) != 0 || mark.Height != 0 {
		t.Fatalf("too many rows: the paste happened, want an empty mark and no error: mark=%+v err=%v", mark, err)
	}
}

func TestPasteMarkedBefore38DeliversWithoutCounters(t *testing.T) {
	s := &script{handler: paste(evLine(paneA, 2, 5, "", ""), nil, "ok")}
	h := hostAt(t, "3.6a", s)
	if h.TurnsSupported() {
		t.Fatal("3.6a supports turns")
	}
	mark, err := h.PasteMarked(context.Background(), paneA, "hello", paneA)
	if err != nil {
		t.Fatal(err)
	}
	if mark.HasCounters() || len(mark.Rows) != 0 {
		t.Fatalf("mark = %+v", mark)
	}
	if strings.Contains(strings.Join(s.calls[1], " "), "capture-pane") {
		t.Fatal("a capture on a tmux that cannot anchor it")
	}
	// The paste was still sent: the if-shell command is there.
	if !strings.Contains(strings.Join(s.calls[1], " "), "paste-buffer -d -p -r") {
		t.Fatal("no paste")
	}
}

func TestTurnsSupportedFrom38(t *testing.T) {
	for ver, want := range map[string]bool{"3.2": false, "3.5a": false, "3.6": false, "3.7": false, "3.8": true, "3.9": true, "4.0": true, "master": true, "next-3.9": true} {
		s := &script{handler: livePane(&paneA)}
		if got := hostAt(t, ver, s).TurnsSupported(); got != want {
			t.Errorf("tmux %s: TurnsSupported = %v, want %v", ver, got, want)
		}
	}
}

func TestReadFrameFormat(t *testing.T) {
	// A pane of 3 rows with 10 history rows after 40 trimmed ones: rows 50-52
	// are visible. Asking from 48 returns 2 history rows and 3 screen rows.
	s := &script{handler: func(sub string, args []string, _ []byte) (string, error) {
		return evLine(paneA, 3, 10, "40", "1") + "\nh48\nh49\nv50\nv51\nv52\n", nil
	}}
	fr, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), paneA, 48)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(s.calls[0], " ")
	x := "#{e|-:48,#{e|+:#{history_collected},#{history_size}}}"
	want := "capture-pane -p -t %4 -S #{?#{e|>:" + x + ",0},0," + x + "}"
	if !strings.Contains(joined, want) {
		t.Fatalf("the invocation lacks\n%s\n in\n%s", want, joined)
	}
	if fr.From != 48 || fr.Start() != 48 || strings.Join(fr.Rows, ",") != "h48,h49,v50,v51,v52" || fr.Collected != 40 {
		t.Fatalf("frame = %+v start=%d", fr, fr.Start())
	}
}

func TestReadFrameClampAndTail(t *testing.T) {
	ev := evLine(paneA, 3, 10, "40", "1") // visible rows are 50..52
	for name, c := range map[string]struct {
		from  int64
		rows  []string
		start int64
	}{
		"trimmed start is clamped by tmux":                   {10, []string{"h40", "h41", "h42", "h43", "h44", "h45", "h46", "h47", "h48", "h49", "v50", "v51", "v52"}, 40},
		"a start inside the screen starts at the screen top": {52, []string{"v50", "v51", "v52"}, 50},
		"the top row": {50, []string{"v50", "v51", "v52"}, 50},
	} {
		t.Run(name, func(t *testing.T) {
			s := &script{handler: func(string, []string, []byte) (string, error) {
				out := ev + "\n"
				for _, r := range c.rows {
					out += r + "\n"
				}
				return out, nil
			}}
			fr, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), paneA, c.from)
			if err != nil || fr.Start() != c.start || len(fr.Rows) != len(c.rows) || fr.From != c.from {
				t.Fatalf("frame=%+v start=%d err=%v", fr, fr.Start(), err)
			}
		})
	}
	s := &script{handler: func(string, []string, []byte) (string, error) { return ev + "\n", nil }}
	if _, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), paneA, -1); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("a negative start: %v", err)
	}
	if _, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), term.PaneRef{ID: "x"}, 0); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("a bad pane id: %v", err)
	}
}

// A start beyond the last row is no error: after clear-history or a reflow the
// history is shorter than the caller's position. The frame holds the evidence
// and the visible rows, and the extractor reads the reason from the evidence.
func TestReadFrameFromBeyondTheLastRowReturnsTheVisibleRows(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) {
		return evLine(paneA, 3, 0, "40", "2") + "\nv40\nv41\nv42\n", nil
	}}
	fr, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), paneA, 70)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fr.From != 70 || fr.Start() != 40 || fr.Generation != 2 || fr.LastRow() != 42 || strings.Join(fr.Rows, ",") != "v40,v41,v42" {
		t.Fatalf("frame = %+v start=%d", fr, fr.Start())
	}
}

func TestReadFrameBefore38ReadsOnlyEvidence(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) {
		return evLine(paneA, 3, 10, "", "") + "\n", nil
	}}
	fr, err := hostAt(t, "3.5a", s).ReadFrame(context.Background(), paneA, 0)
	if err != nil || fr.HasCounters() || len(fr.Rows) != 0 {
		t.Fatalf("frame=%+v err=%v", fr, err)
	}
	if len(s.calls) != 1 || strings.Contains(strings.Join(s.calls[0], " "), "capture-pane") {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestReadFrameSanitisesRows(t *testing.T) {
	ev := evLine(paneA, 2, 0, "0", "0")
	s := &script{handler: func(string, []string, []byte) (string, error) {
		return ev + "\nplain\x1b]52;c;ZXZpbA==\x07 text\nsecond\x01\n", nil
	}}
	fr, err := hostAt(t, "3.8", s).ReadFrame(context.Background(), paneA, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range fr.Rows {
		if strings.ContainsAny(r, "\x1b\x07\x01") || strings.Contains(r, "ZXZpbA") {
			t.Fatalf("an unsanitised row: %q", r)
		}
	}
}

// keepStdout is an ExecFunc whose paste invocation fails after it printed: the
// output it gives for the combined command comes with a not-found error, as it
// does when a user hook of the paste fails.
func keepStdout(out func(args []string) string, stderr string) ExecFunc {
	s := &script{handler: func(sub string, args []string, _ []byte) (string, error) { return "", nil }}
	return func(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
		if len(args) >= 1 && args[len(args)-1] == "-V" {
			return []byte("tmux 3.8\n"), nil, nil
		}
		if len(args) > 3 && args[3] == "display-message" {
			return []byte(out(args)), []byte(stderr), errExit1
		}
		return s.exec(ctx, args, stdin)
	}
}

func tokensOf(args []string) (ok, refused string) {
	for _, a := range args {
		if m := tokenRE.FindStringSubmatch(a); m != nil {
			if strings.Contains(a, "paste-buffer") {
				ok = m[1]
			} else {
				refused = m[1]
			}
		}
	}
	return ok, refused
}

func TestPasteMarkedFailureAfterTheTokensIsHonouredBeforeTheExitStatus(t *testing.T) {
	ev := evLine(paneA, 2, 0, "0", "0")
	run := func(out func(args []string) string) (term.DeliveryMark, error) {
		h, err := New(fakeSocket, WithExec(keepStdout(out, "can't find pane: %4")))
		if err != nil {
			t.Fatal(err)
		}
		return h.PasteMarked(context.Background(), paneA, "x", paneA)
	}
	t.Run("the ok token with a not-found error from a hook is a delivery", func(t *testing.T) {
		mark, err := run(func(args []string) string {
			ok, _ := tokensOf(args)
			return ev + "\n\n\n" + ok + "\n"
		})
		if err != nil || mark.Height != 2 {
			t.Fatalf("mark=%+v err=%v", mark.PaneEvidence, err)
		}
	})
	t.Run("the refused token with an error is a refusal", func(t *testing.T) {
		_, err := run(func(args []string) string {
			_, refused := tokensOf(args)
			return ev + "\n\n\n" + refused + "\n"
		})
		if !errors.Is(err, term.ErrPaneChanged) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("output without a token and a not-found error is unknown, not nothing sent", func(t *testing.T) {
		_, err := run(func([]string) string { return ev + "\n\n\n" })
		if err == nil || errors.Is(err, term.ErrPaneChanged) || errors.Is(err, term.ErrNotFound) {
			t.Fatalf("err = %v, want an unknown-outcome error", err)
		}
	})
	t.Run("no output and a not-found error is a gone pane", func(t *testing.T) {
		h, err := New(fakeSocket, WithExec(keepStdout(func([]string) string { return "" }, "can't find pane: %4")))
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.PasteMarked(context.Background(), paneA, "x", paneA)
		if !errors.Is(err, term.ErrPaneChanged) {
			t.Fatalf("err = %v", err)
		}
	})
}
