package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

const fakeSocket = "/run/test/tmux.sock"

// fake is an ExecFunc recorder. reply maps the tmux subcommand (the first
// argument after "-S <socket> -u") to its canned output.
type fake struct {
	mu      sync.Mutex
	calls   [][]string
	stdins  [][]byte
	version string
	reply   map[string]fakeReply
}

type fakeReply struct {
	stdout, stderr string
	err            error
	// tabbed is a list-panes fixture whose fields are separated by a tab and
	// whose rows end in a newline. The fake frames it with the delimiters of
	// the -F argument it receives, like tmux would for tab-free values.
	tabbed string
	// fn computes the stdout from the arguments of the invocation.
	fn func(args []string) string
}

// codecFromArgs recovers the per-call delimiters from the -F argument of a
// list-panes invocation.
func codecFromArgs(args []string) paneCodec {
	for i, a := range args {
		if a == "-F" && i+1 < len(args) {
			return codecFromFormat(args[i+1])
		}
	}
	panic("no -F argument in " + strings.Join(args, " "))
}

func codecFromFormat(format string) paneCodec {
	const first, second = "#{session_created}", "#{session_attached}"
	i := strings.Index(format, first) + len(first)
	j := strings.Index(format, second)
	rest := format[strings.LastIndex(format, "#{session_name}")+len("#{session_name}"):]
	return paneCodec{fs: format[i:j], rs: rest}
}

// frame builds list-panes output from rows of exactly paneFields fields,
// exactly as tmux prints them: raw values, fs between them, rs and a newline
// after each row.
func (c paneCodec) frame(rows ...[]string) string {
	var b strings.Builder
	for _, f := range rows {
		b.WriteString(strings.Join(f, c.fs) + c.rs + "\n")
	}
	return b.String()
}

// frameTabs frames a tab-separated fixture (values without tabs or newlines).
func (c paneCodec) frameTabs(tabbed string) string {
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSuffix(tabbed, "\n"), "\n") {
		rows = append(rows, strings.Split(line, "\t"))
	}
	return c.frame(rows...)
}

// row builds one list-panes row for session name with the given task and
// command; every other field is fixed.
func row(pane, task, command, name string) []string {
	return []string{"1700000000", "0", "1", "0", "1", "0", "1", pane, "10", task, "", command, name}
}

func (f *fake) exec(_ context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.stdins = append(f.stdins, stdin)
	sub := ""
	for i, a := range args {
		if a == "-V" {
			sub = "-V"
			break
		}
		if i >= 3 {
			sub = a
			break
		}
	}
	if sub == "-V" {
		v := f.version
		if v == "" {
			v = "tmux 3.5a\n"
		}
		return []byte(v), nil, nil
	}
	r, ok := f.reply[sub]
	if !ok {
		return nil, []byte("unexpected command " + sub), errors.New("exit status 1")
	}
	if r.tabbed != "" {
		r.stdout = codecFromArgs(args).frameTabs(r.tabbed)
	}
	if r.fn != nil {
		r.stdout = r.fn(args)
	}
	return []byte(r.stdout), []byte(r.stderr), r.err
}

func newFakeHost(t *testing.T, f *fake) *Host {
	t.Helper()
	h, err := New(fakeSocket, WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

var errExit1 = errors.New("exit status 1")

func TestNewRejectsRelativeOrEmptySocket(t *testing.T) {
	for _, p := range []string{"", "tmux.sock", "./tmux.sock", "../x/tmux.sock", "~/tmux.sock"} {
		f := &fake{}
		_, err := New(p, WithExec(f.exec))
		if err == nil {
			t.Errorf("New(%q) succeeded, want an error", p)
		}
		if len(f.calls) != 0 {
			t.Errorf("New(%q) ran tmux before rejecting the path: %v", p, f.calls)
		}
	}
}

func TestEveryInvocationPassesSocket(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"list-panes":      {tabbed: "1700000000\t0\t1\t0\t1\t0\t1\t%0\t100\t\t\tbash\twb-1\n"},
		"capture-pane":    {stdout: "hello\n"},
		"display-message": {stdout: "80\t24\t0\t0\t1\n"},
	}}
	h := newFakeHost(t, f)
	ctx := context.Background()
	if _, err := h.ListSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb-1"}); err != nil {
		t.Fatal(err)
	}
	ref := term.PaneRef{ID: "%0"}
	if _, err := h.Snapshot(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(ctx, ref, 50); err != nil {
		t.Fatal(err)
	}
	// New (-V), ListSessions, ResolvePane, Snapshot (2), Capture (2).
	if len(f.calls) != 7 {
		t.Fatalf("got %d tmux invocations, want 7: %v", len(f.calls), f.calls)
	}
	for _, c := range f.calls {
		if len(c) < 3 || c[0] != "-S" || c[1] != fakeSocket {
			t.Errorf("invocation %v does not start with -S %s", c, fakeSocket)
		}
		// capture-pane has its own -S (start line, "-50"); a socket is a path.
		for i, a := range c[3:] {
			if a == "-S" && i+4 < len(c) && strings.HasPrefix(c[i+4], "/") {
				t.Errorf("invocation %v names a second socket", c)
			}
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		ok      bool
		atLeast bool
	}{
		{"tmux 3.5a\n", true, true},
		{"tmux 3.8", true, true},
		{"tmux 3.2", true, true},
		{"tmux 3.2a", true, true},
		{"tmux 3.1c", true, false},
		{"tmux 3.0a", true, false},
		{"tmux 2.9", true, false},
		{"tmux 1.8", true, false},
		{"tmux 4.0", true, true},
		{"tmux 10.1", true, true},
		{"tmux next-3.6", true, true},
		{"tmux next-3.1", true, false},
		{"tmux master", true, true},
		{"tmux", false, false},
		{"screen 4.09", false, false},
		{"tmux abc", false, false},
		{"", false, false},
	}
	for _, tc := range tests {
		v, err := ParseVersion(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("ParseVersion(%q) err = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if err == nil && v.AtLeast(MinMajor, MinMinor) != tc.atLeast {
			t.Errorf("ParseVersion(%q).AtLeast(3,2) = %v, want %v", tc.in, !tc.atLeast, tc.atLeast)
		}
	}
	v, _ := ParseVersion("tmux 3.5a")
	if v.Major != 3 || v.Minor != 5 || v.Suffix != "a" || v.String() != "3.5a" {
		t.Errorf("ParseVersion(3.5a) = %+v", v)
	}
}

func TestNewRefusesOldTmux(t *testing.T) {
	f := &fake{version: "tmux 3.1c\n"}
	_, err := New(fakeSocket, WithExec(f.exec))
	if err == nil {
		t.Fatal("New accepted tmux 3.1c")
	}
	for _, want := range []string{"3.1c", "too old", "3.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, v := range []string{"tmux 3.2\n", "tmux 3.5a\n", "tmux 3.8\n"} {
		f := &fake{version: v}
		h, err := New(fakeSocket, WithExec(f.exec))
		if err != nil {
			t.Errorf("New refused %q: %v", strings.TrimSpace(v), err)
			continue
		}
		if h.Socket() != fakeSocket || h.Version().Major != 3 {
			t.Errorf("host = %q %v", h.Socket(), h.Version())
		}
	}
}

func TestNewReportsUnrunnableTmux(t *testing.T) {
	_, err := New(fakeSocket, WithBinary("/nonexistent/tmux-binary"))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v", err)
	}
	f := &fake{version: "garbage"}
	if _, err := New(fakeSocket, WithExec(f.exec)); err == nil {
		t.Fatal("New accepted unparsable version output")
	}
}

func TestCleanEnv(t *testing.T) {
	got := cleanEnv([]string{"A=1", "TMUX=/tmp/x,1,0", "TMUX_PANE=%3", "TMUXY=2", "B=3"})
	want := []string{"A=1", "TMUXY=2", "B=3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cleanEnv = %v, want %v", got, want)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{limit: 4}
	if n, err := c.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if c.String() != "abcd" || !c.exceeded {
		t.Errorf("buffer = %q exceeded=%v", c.String(), c.exceeded)
	}
}

const listOut = "1700000000\t1\t2\t0\t1\t0\t1\t%0\t101\t\t\tbash\twb-1\n" +
	"1700000000\t1\t2\t0\t1\t1\t0\t%1\t102\t\t\tvim\twb-1\n" +
	"1700000000\t1\t2\t1\t0\t0\t1\t%2\t103\t\t\tless\twb-1\n" +
	"1700000100\t0\t1\t0\t1\t0\t1\t%3\t201\tfix bug\t\tclaude\tminion-2\n"

func TestListSessionsParsesRows(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: listOut}}}
	h := newFakeHost(t, f)
	got, err := h.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions: %+v", len(got), got)
	}
	wb, mi := got[0], got[1]
	if wb.Name != "wb-1" || !wb.Attached || wb.Windows != 2 || len(wb.Panes) != 3 ||
		!wb.Created.Equal(time.Unix(1700000000, 0)) || wb.Task != "" {
		t.Errorf("wb-1 = %+v", wb)
	}
	ap, ok := wb.ActivePane()
	if !ok || ap.Ref != (term.PaneRef{ID: "%0", PID: 101, Command: "bash"}) || ap.Window != 0 || ap.Index != 0 {
		t.Errorf("active pane of wb-1 = %+v", ap)
	}
	// %2 is the active pane of its window, but that window is not active.
	if wb.Panes[2].Active || wb.Panes[1].Active {
		t.Errorf("only the active pane of the active window is Active: %+v", wb.Panes)
	}
	if wb.Panes[2].Window != 1 || wb.Panes[1].Index != 1 {
		t.Errorf("window and pane indexes: %+v", wb.Panes)
	}
	if mi.Name != "minion-2" || mi.Attached || mi.Task != "fix bug" || mi.Windows != 1 {
		t.Errorf("minion-2 = %+v", mi)
	}
	if got := f.calls[len(f.calls)-1]; got[3] != "list-panes" || got[4] != "-a" {
		t.Errorf("call = %v", got)
	}
}

func TestListSessionsKeepsNamesUnaltered(t *testing.T) {
	// tmux (>= 3.2) vis-escapes a session name when it creates the session and
	// prints the stored form raw. That stored form is what has-session matches,
	// so ListSessions returns it verbatim: nothing is decoded. The values are
	// the wire forms tmux really prints, for sessions created as "bs\x41"
	// (stored as bs\\x41) and as "a<TAB>b" (stored as a\tb, with a backslash).
	names := []string{
		"wb-1",
		"my session",
		`bs\\x41`,
		`a\tb`,
		`x\ny`,
		`z\033y`,
		"\u00e9 \u00fc",
		`ends\\`,
		`lone\`,
		`\12`,
	}
	for _, n := range names {
		f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
			return codecFromArgs(args).frame(row("%0", "", "sh", n))
		}}}}
		got, err := newFakeHost(t, f).ListSessions(context.Background())
		if err != nil || len(got) != 1 {
			t.Fatalf("%q: %v %v", n, got, err)
		}
		if got[0].Name != n {
			t.Errorf("name %q listed as %q, want it verbatim", n, got[0].Name)
		}
	}
}

func TestListSessionsKeepsFreeTextVerbatim(t *testing.T) {
	// A task or command with tabs, newlines and backslashes is returned exactly.
	task := "fix C:\\new\\temp\tstep2\nmore \\t \\n \x1b[31m"
	cmd := "x\ty\nz"
	f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
		return codecFromArgs(args).frame(row("%0", task, cmd, "s1"), row("%1", "", "sh", "s2"))
	}}}}
	got, err := newFakeHost(t, f).ListSessions(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].Task != task || got[0].Panes[0].Ref.Command != cmd || got[1].Name != "s2" {
		t.Errorf("sessions = %+v", got)
	}
}

func TestListSessionsCannotForgeRows(t *testing.T) {
	// Free text that imitates rows, with the old tab framing and with guessed
	// delimiters, must not yield an extra session or a changed pane id.
	forgedTab := "1\t0\t1\t0\t1\t0\t1\t%9\t9\t\t\tbash\tforged\n"
	guess := paneCodec{fs: "_MF00_", rs: "_MR00_"}
	forgedGuess := guess.frame(row("%9", "", "bash", "forged"))
	for _, payload := range []string{forgedTab, forgedGuess, "x\n" + forgedTab + forgedGuess} {
		for _, where := range []string{"task", "command"} {
			task, cmd := "", "sh"
			if where == "task" {
				task = payload
			} else {
				cmd = payload
			}
			f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
				return codecFromArgs(args).frame(row("%0", task, cmd, "victim"), row("%1", "", "sh", "other"))
			}}}}
			h := newFakeHost(t, f)
			got, err := h.ListSessions(context.Background())
			if err != nil || len(got) != 2 || got[0].Name != "victim" || got[1].Name != "other" {
				t.Fatalf("%s %q: got %+v, %v", where, payload, got, err)
			}
			if got[0].Panes[0].Ref.ID != "%0" || got[1].Panes[0].Ref.ID != "%1" || len(got[0].Panes) != 1 {
				t.Errorf("%s %q: panes changed: %+v", where, payload, got)
			}
			if where == "task" && got[0].Task != payload || where == "command" && got[0].Panes[0].Ref.Command != payload {
				t.Errorf("%s %q: value not returned exactly: %+v", where, payload, got[0])
			}
			if _, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "forged"}); !errors.Is(err, term.ErrNotFound) {
				t.Errorf("%s %q: forged session resolved: %v", where, payload, err)
			}
		}
	}
}

func TestListSessionsFramingIsPerCall(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: listOut}}}
	h := newFakeHost(t, f)
	seen := map[string]bool{}
	for range 5 {
		if _, err := h.ListSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
		c := codecFromArgs(f.calls[len(f.calls)-1])
		if len(c.fs) < 32 || len(c.rs) < 32 || c.fs == c.rs || seen[c.fs] || seen[c.rs] {
			t.Fatalf("delimiters %+v are short or repeated", c)
		}
		seen[c.fs], seen[c.rs] = true, true
	}
}

func TestParseRejectsMalformedFraming(t *testing.T) {
	c := paneCodec{fs: "_F_", rs: "_R_"}
	good := c.frame(row("%0", "", "sh", "a"))
	if _, err := c.parse(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.TrimSuffix(good, "\n"),      // no newline after the terminator
		good + "trailing",                   // unterminated row
		strings.Replace(good, "_F_", "", 1), // a missing field
	} {
		if _, err := c.parse(bad); err == nil {
			t.Errorf("parse accepted %q", bad)
		}
	}
}

func TestParseDeadPaneWithoutPID(t *testing.T) {
	// tmux 3.8 prints an empty pane_pid for a dead pane (remain-on-exit).
	c := paneCodec{fs: "_F_", rs: "_R_"}
	r := row("%0", "", "true", "dead")
	r[8] = ""
	got, err := c.parse(c.frame(r))
	if err != nil || len(got) != 1 || got[0].Panes[0].Ref.PID != 0 || got[0].Panes[0].Ref.Command != "true" {
		t.Errorf("got %+v err %v", got, err)
	}
	r[8] = "x"
	if _, err := c.parse(c.frame(r)); err == nil {
		t.Error("a non-numeric pid was accepted")
	}
}

func TestListSessionsNoServer(t *testing.T) {
	for _, msg := range []string{
		"no server running on /run/test/tmux.sock",
		"error connecting to /run/test/tmux.sock (No such file or directory)",
		"error connecting to /run/test/tmux.sock (Connection refused)",
	} {
		f := &fake{reply: map[string]fakeReply{"list-panes": {stderr: msg + "\n", err: errExit1}}}
		got, err := newFakeHost(t, f).ListSessions(context.Background())
		if err != nil || len(got) != 0 {
			t.Errorf("%q: got %v, %v; want no sessions and no error", msg, got, err)
		}
	}
}

func TestListSessionsErrors(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{"list-panes": {stderr: "error connecting to /x (Permission denied)\n", err: errExit1}}}
	_, err := newFakeHost(t, f).ListSessions(context.Background())
	if err == nil || errors.Is(err, term.ErrNotFound) {
		t.Errorf("permission error = %v, want a real error", err)
	}
	if !strings.Contains(err.Error(), "Permission denied") || !errors.Is(err, errExit1) {
		t.Errorf("error %q lost its cause", err)
	}
	for _, bad := range []string{
		"1\t0\t1\n",
		"x\t0\t1\t0\t1\t0\t1\t%0\t5\t\t\tsh\tn\n",
		"1\t0\t1\t0\t1\t0\t1\t%0\tpid\t\t\tsh\tn\n",
	} {
		f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: bad}}}
		if _, err := newFakeHost(t, f).ListSessions(context.Background()); err == nil {
			t.Errorf("ListSessions accepted %q", bad)
		}
	}
}

func TestParseGeometry(t *testing.T) {
	g, err := parseGeometry("120\t40\t5\t7\t1\n")
	if err != nil || g != (geometry{120, 40, 5, 7, true}) {
		t.Errorf("g = %+v, %v", g, err)
	}
	g, _ = parseGeometry("80\t24\t0\t0\t0")
	if g.cursorVisible {
		t.Error("cursor_flag 0 must hide the cursor")
	}
	for _, bad := range []string{"", "1\t2", "a\t2\t3\t4\t5"} {
		if _, err := parseGeometry(bad); err == nil {
			t.Errorf("parseGeometry(%q) succeeded", bad)
		}
	}
}

func TestSnapshotUnit(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: "plain \x1b[31mred\x1b[0m \x1b]52;c;ZXZpbA==\x07done\nsecond\n"},
		"display-message": {stdout: "20\t4\t6\t1\t1\n"},
	}}
	h := newFakeHost(t, f)
	scr, err := h.Snapshot(context.Background(), term.PaneRef{ID: "%7"})
	if err != nil {
		t.Fatal(err)
	}
	if scr.Width != 20 || scr.Height != 4 || len(scr.Lines) != 4 {
		t.Errorf("size %dx%d, %d lines", scr.Width, scr.Height, len(scr.Lines))
	}
	if scr.Lines[0] != "plain red done" || scr.Lines[1] != "second" {
		t.Errorf("lines = %q", scr.Lines)
	}
	if scr.Cursor == nil || scr.Cursor.Row != 1 || scr.Cursor.Col != 6 {
		t.Errorf("cursor = %+v", scr.Cursor)
	}
	if len(scr.Spans) != 1 || scr.Spans[0].FG != "p1" || scr.Spans[0].Start != 6 || scr.Spans[0].End != 9 {
		t.Errorf("spans = %+v", scr.Spans)
	}
	if err := scr.Validate(); err != nil {
		t.Errorf("screen invalid: %v", err)
	}
	// The capture command is exactly capture-pane -p -e -t %7.
	var capture []string
	for _, c := range f.calls {
		if len(c) > 3 && c[3] == "capture-pane" {
			capture = c
		}
	}
	if got := strings.Join(capture[3:], " "); got != "capture-pane -p -e -t %7" {
		t.Errorf("snapshot command = %q", got)
	}
}

func TestSnapshotHiddenCursor(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: "x\n"},
		"display-message": {stdout: "20\t4\t0\t0\t0\n"},
	}}
	scr, err := newFakeHost(t, f).Snapshot(context.Background(), term.PaneRef{ID: "%0"})
	if err != nil || scr.Cursor != nil {
		t.Errorf("cursor = %+v, err = %v; want a null cursor", scr.Cursor, err)
	}
}

func TestCaptureUnit(t *testing.T) {
	out := "l1\n\x1b[1mbold2\x1b[0m\nl3\nl4\nl5\n\n\n"
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: out},
		"display-message": {stdout: "33\t10\t0\t0\t1\n"},
	}}
	h := newFakeHost(t, f)
	scr, err := h.Capture(context.Background(), term.PaneRef{ID: "%2"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(scr.Lines, "|"); got != "bold2|l3|l4|l5" {
		t.Errorf("lines = %q", got)
	}
	if len(scr.Spans) != 1 || scr.Spans[0].Line != 0 || !scr.Spans[0].Bold {
		t.Errorf("spans were not moved with their line: %+v", scr.Spans)
	}
	if scr.Width != 33 || scr.Height != 0 || scr.Cursor != nil {
		t.Errorf("width=%d height=%d cursor=%v", scr.Width, scr.Height, scr.Cursor)
	}
	var capture []string
	for _, c := range f.calls {
		if len(c) > 3 && c[3] == "capture-pane" {
			capture = c
		}
	}
	if got := strings.Join(capture[3:], " "); got != "capture-pane -p -e -t %2 -S -4" {
		t.Errorf("capture command = %q", got)
	}
}

func TestCaptureBlankPane(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: "\n\n\n"},
		"display-message": {stdout: "33\t3\t0\t0\t1\n"},
	}}
	scr, err := newFakeHost(t, f).Capture(context.Background(), term.PaneRef{ID: "%0"}, 10)
	if err != nil || scr.Lines == nil || len(scr.Lines) != 0 || scr.Spans == nil {
		t.Errorf("scr = %+v, err = %v; want empty non-nil slices", scr, err)
	}
}

func largeCaptureHost(t *testing.T, rows, cols int) *Host {
	t.Helper()
	var b strings.Builder
	for i := range rows {
		fmt.Fprintf(&b, "%05d", i)
		b.WriteString(strings.Repeat("x", cols-5))
		b.WriteString("\n")
	}
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: b.String()},
		"display-message": {stdout: fmt.Sprintf("%d\t24\t0\t0\t1\n", cols)},
	}}
	return newFakeHost(t, f)
}

func TestCaptureKeepsNewestRows(t *testing.T) {
	// More rows than the screen builder's row cap: the result must be the
	// newest rows, not the oldest.
	h := largeCaptureHost(t, 60000, 8)
	scr, err := h.Capture(context.Background(), term.PaneRef{ID: "%0"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(scr.Lines) != 100 || !strings.HasPrefix(scr.Lines[0], "59900") || !strings.HasPrefix(scr.Lines[99], "59999") {
		t.Errorf("got %d rows from %q to %q", len(scr.Lines), scr.Lines[0], scr.Lines[len(scr.Lines)-1])
	}
}

func TestCaptureTooLargeIsAnError(t *testing.T) {
	// 10000 rows of 500 columns exceed the cell budget of the screen builder.
	// That must be an error, never a successful capture without the newest rows.
	h := largeCaptureHost(t, 10020, 500)
	_, err := h.Capture(context.Background(), term.PaneRef{ID: "%0"}, 10000)
	if !errors.Is(err, ErrCaptureTooLarge) {
		t.Errorf("err = %v, want ErrCaptureTooLarge", err)
	}
	// The same pane with a depth that fits is returned whole and newest-last.
	scr, err := h.Capture(context.Background(), term.PaneRef{ID: "%0"}, 1000)
	if err != nil || len(scr.Lines) != 1000 || !strings.HasPrefix(scr.Lines[999], "10019") {
		t.Errorf("1000 rows: %d rows, err %v", len(scr.Lines), err)
	}
}

func TestExecProcessEnforcesOutputLimits(t *testing.T) {
	// A real subprocess writing more than the limits: os/exec copies output
	// with ReadFrom when the buffer offers it, which would bypass the limit.
	h := &Host{bin: "sh", timeout: 30 * time.Second}
	stdout, _, err := h.execProcess(context.Background(), []string{"-c", "head -c 17825792 /dev/zero"}, nil)
	if err == nil || len(stdout) > maxStdout {
		t.Errorf("stdout: %d bytes, err %v; want at most %d bytes and an error", len(stdout), err, maxStdout)
	}
	_, stderr, _ := h.execProcess(context.Background(), []string{"-c", "head -c 200000 /dev/zero >&2"}, nil)
	if len(stderr) > maxStderr {
		t.Errorf("stderr: %d bytes, want at most %d", len(stderr), maxStderr)
	}
}

func TestReadArgumentValidation(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{}}
	h := newFakeHost(t, f)
	before := len(f.calls)
	ctx := context.Background()
	for _, id := range []string{"", "3", "%", "%x", "-S", "%1;send-keys", "wb-1", "%1234567890"} {
		if _, err := h.Snapshot(ctx, term.PaneRef{ID: id}); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("Snapshot(%q) err = %v", id, err)
		}
		if _, err := h.Capture(ctx, term.PaneRef{ID: id}, 10); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("Capture(%q) err = %v", id, err)
		}
	}
	for _, n := range []int{0, -1, MaxCaptureLines + 1} {
		if _, err := h.Capture(ctx, term.PaneRef{ID: "%1"}, n); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("Capture(lines=%d) err = %v", n, err)
		}
	}
	if len(f.calls) != before {
		t.Errorf("invalid arguments reached tmux: %v", f.calls[before:])
	}
}

func TestReadMissingPane(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"capture-pane": {stderr: "can't find pane: %9\n", err: errExit1},
	}}
	h := newFakeHost(t, f)
	if _, err := h.Snapshot(context.Background(), term.PaneRef{ID: "%9"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("Snapshot err = %v, want ErrNotFound", err)
	}
	if _, err := h.Capture(context.Background(), term.PaneRef{ID: "%9"}, 5); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("Capture err = %v, want ErrNotFound", err)
	}
}

func TestTailScreen(t *testing.T) {
	s := ops.Screen{
		Lines: []string{"a", "b", "c", "", ""},
		Spans: []ops.StyleSpan{{Line: 0, Start: 0, End: 1, Bold: true}, {Line: 2, Start: 0, End: 1, Dim: true}},
	}
	got := tailScreen(s, 2)
	if strings.Join(got.Lines, "|") != "b|c" || len(got.Spans) != 1 || got.Spans[0].Line != 1 || !got.Spans[0].Dim {
		t.Errorf("tail = %+v", got)
	}
	if got := tailScreen(s, 99); len(got.Lines) != 3 || len(got.Spans) != 2 {
		t.Errorf("tail 99 = %+v", got)
	}
}

// --- Integration: a real tmux on a private socket (tmuxtest). ---

func newRealHost(t *testing.T) (*Host, *tmuxtest.Server) {
	t.Helper()
	srv := tmuxtest.New(t)
	h, err := New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	return h, srv
}

func TestIntegrationListSessions(t *testing.T) {
	h, srv := newRealHost(t)
	srv.NewSession("wb-1")
	srv.NewSession("minion-2", "sleep", "60")
	got, err := h.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]term.Session{}
	for _, s := range got {
		by[s.Name] = s
	}
	if len(got) != 2 || len(by["wb-1"].Panes) != 1 || len(by["minion-2"].Panes) != 1 {
		t.Fatalf("sessions = %+v", got)
	}
	for _, name := range []string{"wb-1", "minion-2"} {
		s := by[name]
		p, ok := s.ActivePane()
		if !ok || !strings.HasPrefix(p.Ref.ID, "%") || p.Ref.PID <= 0 || p.Ref.Command == "" {
			t.Errorf("%s active pane = %+v", name, p)
		}
		if s.Attached || s.Windows != 1 || time.Since(s.Created) > time.Hour || s.Created.IsZero() {
			t.Errorf("%s = %+v", name, s)
		}
	}
	if c := by["minion-2"].Panes[0].Ref.Command; c != "sleep" {
		t.Errorf("minion-2 command = %q, want sleep", c)
	}
}

// listedByName returns the sessions of the real server by their listed name.
func listedByName(t *testing.T, h *Host) (map[string]term.Session, []term.Session) {
	t.Helper()
	got, err := h.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	by := map[string]term.Session{}
	for _, s := range got {
		by[s.Name] = s
	}
	return by, got
}

func TestIntegrationStoredSessionNames(t *testing.T) {
	// tmux stores a created name vis-escaped and prints the stored form raw;
	// has-session matches the stored form only. The adapter must list that
	// form and resolve it (R86).
	h, srv := newRealHost(t)
	// Each case is the created name and the stored name tmux lists for it.
	cases := []struct{ created, stored string }{
		{`bs\x41`, `bs\\x41`}, {"a\tb", `a\tb`}, {"\u00e9 \u00fc", "\u00e9 \u00fc"}, {"plain", "plain"},
	}
	var want []string
	for _, c := range cases {
		_, err := h.run(context.Background(), "new-session", "-d", "-s", c.created, "--", "sleep", "60")
		if err != nil && strings.Contains(err.Error(), "invalid session name") && strings.ContainsAny(c.created, "\t\n") {
			// tmux 3.8 refuses a control character in a session name; older
			// versions store it vis-escaped. Only that refusal is tolerated.
			t.Logf("this tmux refuses the session name %q: %v", c.created, err)
			continue
		}
		if err != nil {
			t.Fatalf("new-session %q: %v", c.created, err)
		}
		want = append(want, c.stored)
	}
	// The sessions were created through h.run, not srv.NewSession, so nothing
	// has waited for their panes to exec sleep yet. Wait for that, verified on
	// the pane processes, so that ListSessions and the later ResolvePane read
	// the same command (R86 compares the two refs). Only fixture
	// synchronisation: see tmuxtest.Server.WaitAllExec for why
	// pane_current_command alone does not settle it.
	srv.WaitAllExec("sleep", 5*time.Second)
	by, got := listedByName(t, h)
	if len(got) != len(want) {
		t.Fatalf("sessions = %+v, want %q", got, want)
	}
	for _, name := range want {
		s, ok := by[name]
		if !ok {
			t.Fatalf("session %q not listed; have %+v", name, got)
		}
		ref, err := h.ResolvePane(context.Background(), ops.Target{SessionName: s.Name})
		if err != nil || ref != s.Panes[0].Ref {
			t.Errorf("ResolvePane(%q) = %+v, %v; want %+v", name, ref, err, s.Panes[0].Ref)
		}
		// The listed name addresses the session in tmux itself.
		mustRun(t, h, "has-session", "-t", "="+s.Name)
	}
}

func TestIntegrationTaskWithSeparators(t *testing.T) {
	h, srv := newRealHost(t)
	srv.NewSession("one", "sleep", "60")
	srv.NewSession("two", "sleep", "60")
	srv.NewSession("three", "sleep", "60")
	tasks := map[string]string{
		"one": "step1\tstep2\nmore",
		"two": `fix C:\new\temp and \\ and \t \n \033`,
	}
	for name, task := range tasks {
		mustRun(t, h, "set-option", "-t", "="+name+":", "@minion_task", task)
	}
	by, got := listedByName(t, h)
	if len(got) != 3 {
		t.Fatalf("sessions = %+v", got)
	}
	for name, task := range tasks {
		if by[name].Task != task {
			t.Errorf("task of %s = %q, want %q", name, by[name].Task, task)
		}
	}
	if by["three"].Task != "" {
		t.Errorf("task of three = %q", by["three"].Task)
	}
	for _, name := range []string{"one", "two", "three"} {
		if _, err := h.ResolvePane(context.Background(), ops.Target{SessionName: name}); err != nil {
			t.Errorf("ResolvePane(%s) = %v", name, err)
		}
	}
}

// refusesControlChars reports whether this tmux (3.8 and later) refuses a
// control character in a session name, which is also the version that prints
// an empty pane_current_command for an argv[0] with one. The probe runs on a
// private server of its own.
func refusesControlChars(t *testing.T) bool {
	t.Helper()
	h, _ := newRealHost(t)
	_, err := h.run(context.Background(), "new-session", "-d", "-s", "a\tb", "--", "sleep", "60")
	return err != nil && strings.Contains(err.Error(), "invalid session name")
}

func TestIntegrationCommandWithSeparators(t *testing.T) {
	// A process whose argv[0] holds separators must not break the listing of
	// the other sessions, and its command comes back exactly as tmux prints it.
	// tmux 3.8 prints an empty command for an argv[0] with a control character
	// (tab, newline); tmux up to 3.6 prints it raw. The printable argv[0] with
	// a backslash sequence and the framing look-alikes comes back verbatim on
	// every version.
	h, srv := newRealHost(t)
	srv.NewSession("before", "sleep", "60")
	srv.NewSession("odd", "bash", "-c", `exec -a "$(printf 'x\ty\nz')" sleep 60`)
	srv.NewSession("lit", "bash", "-c", `exec -a 'x;y|z_MF00_MR00_' sleep 60`)
	srv.NewSession("after", "sleep", "60")
	wantOdd := "x\ty\nz"
	if refusesControlChars(t) {
		wantOdd = ""
	}
	var by map[string]term.Session
	deadline := time.Now().Add(5 * time.Second)
	for {
		by, _ = listedByName(t, h)
		odd, lit := by["odd"].Panes[0].Ref.Command, by["lit"].Panes[0].Ref.Command
		// Both have exec'd sleep once neither shows the bash wrapper.
		if (odd != "bash" && lit != "bash") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(by) != 4 {
		t.Fatalf("sessions = %+v", by)
	}
	if got := by["lit"].Panes[0].Ref.Command; got != `x;y|z_MF00_MR00_` {
		t.Errorf("literal command = %q, want it verbatim", got)
	}
	if got := by["odd"].Panes[0].Ref.Command; got != wantOdd {
		t.Errorf("command = %q, want %q", got, wantOdd)
	}
	ctx := context.Background()
	for _, name := range []string{"before", "odd", "lit", "after"} {
		ref, err := h.ResolvePane(ctx, ops.Target{SessionName: name})
		if err != nil || ref != by[name].Panes[0].Ref || len(by[name].Panes) != 1 {
			t.Errorf("ResolvePane(%s) = %+v, %v", name, ref, err)
		}
	}
}

func TestIntegrationNoServer(t *testing.T) {
	srv := tmuxtest.New(t)
	h, err := New(srv.Socket() + "-none")
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.ListSessions(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("no server: %v, %v", got, err)
	}
	if _, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "wb-1"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("resolve without server err = %v", err)
	}
}

func TestIntegrationSnapshotSanitised(t *testing.T) {
	h, srv := newRealHost(t)
	// printf with octal escapes: red text, an OSC 52 clipboard write, an OSC 8
	// hyperlink and an OSC 0 title change.
	script := `printf 'start \033[31mcoloured\033[0m \033]52;c;ZXZpbA==\007clip \033]8;;http://evil.example\007link\033]8;;\007 \033]0;title\007end\n'; sleep 60`
	target := srv.NewSession("snap", "sh", "-c", script)
	srv.WaitFor(target, "end", 10*time.Second)

	ref, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	scr, err := h.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if scr.Width != 120 || scr.Height != 40 || len(scr.Lines) != 40 {
		t.Errorf("size %dx%d, %d lines", scr.Width, scr.Height, len(scr.Lines))
	}
	if got := scr.Lines[0]; got != "start coloured clip link end" {
		t.Errorf("line 0 = %q", got)
	}
	all := strings.Join(scr.Lines, "\n")
	for _, bad := range []string{"\x1b", "\x07", "ZXZpbA", "evil.example", "title"} {
		if strings.Contains(all, bad) {
			t.Errorf("screen contains %q", bad)
		}
	}
	if len(scr.Spans) != 1 || scr.Spans[0].Line != 0 || scr.Spans[0].FG != "p1" ||
		scr.Spans[0].Start != 6 || scr.Spans[0].End != 14 {
		t.Errorf("spans = %+v", scr.Spans)
	}
	if err := scr.Validate(); err != nil {
		t.Errorf("screen invalid: %v", err)
	}
}

func TestIntegrationSnapshotCursor(t *testing.T) {
	h, srv := newRealHost(t)
	target := srv.NewSession("cur", "sh", "-c", `printf 'abc\ndef'; sleep 60`)
	srv.WaitFor(target, "def", 10*time.Second)
	ref, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "cur"})
	if err != nil {
		t.Fatal(err)
	}
	scr, err := h.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if scr.Cursor == nil || scr.Cursor.Row != 1 || scr.Cursor.Col != 3 {
		t.Errorf("cursor = %+v, want row 1 col 3", scr.Cursor)
	}
}

func TestIntegrationCapture(t *testing.T) {
	h, srv := newRealHost(t)
	// 100 numbered lines overflow the 40-row pane into the scrollback.
	target := srv.NewSession("cap", "sh", "-c", `i=1; while [ $i -le 100 ]; do echo "line $i"; i=$((i+1)); done; printf '\033[1mbold\033[0m\n'; sleep 60`)
	srv.WaitFor(target, "bold", 10*time.Second)
	ref, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "cap"})
	if err != nil {
		t.Fatal(err)
	}
	scr, err := h.Capture(context.Background(), ref, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(scr.Lines) != 60 || scr.Lines[0] != "line 42" || scr.Lines[58] != "line 100" || scr.Lines[59] != "bold" {
		t.Errorf("capture: %d lines, first %q, last %q", len(scr.Lines), scr.Lines[0], scr.Lines[len(scr.Lines)-1])
	}
	if len(scr.Spans) != 1 || scr.Spans[0].Line != 59 || !scr.Spans[0].Bold {
		t.Errorf("spans = %+v", scr.Spans)
	}
	small, err := h.Capture(context.Background(), ref, 3)
	if err != nil || strings.Join(small.Lines, "|") != "line 99|line 100|bold" {
		t.Errorf("capture 3 = %q, %v", small.Lines, err)
	}
}

func TestIntegrationMissingPane(t *testing.T) {
	h, srv := newRealHost(t)
	srv.NewSession("wb-1")
	ctx := context.Background()
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "nope"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("missing session err = %v", err)
	}
	if _, err := h.Snapshot(ctx, term.PaneRef{ID: "%999"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("snapshot of missing pane err = %v", err)
	}
	if _, err := h.Capture(ctx, term.PaneRef{ID: "%999"}, 5); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("capture of missing pane err = %v", err)
	}
}

// TestListSessionsReadsProbeSeparatelyFromTask: the probe marker is its own
// field; a task that looks like a probe value stays a task.
func TestListSessionsReadsProbeSeparatelyFromTask(t *testing.T) {
	task := "minion-probe-debugging"
	f := &fake{reply: map[string]fakeReply{"list-panes": {fn: func(args []string) string {
		r1 := row("%0", task, "sh", "s1")
		r1[10] = "nonce-1"
		return codecFromArgs(args).frame(r1, row("%1", "", "sh", "s2"))
	}}}}
	got, err := newFakeHost(t, f).ListSessions(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].Task != task || got[0].Probe != "nonce-1" || got[1].Task != "" || got[1].Probe != "" {
		t.Errorf("sessions = %+v", got)
	}
}

// TestIntegrationProbeOptionIsIndependentOfTask drives the real tmux: writing
// and clearing the probe never changes the task, and the reverse.
func TestIntegrationProbeOptionIsIndependentOfTask(t *testing.T) {
	h, srv := newRealHost(t)
	ctx := context.Background()
	srv.NewSession("adopted", "sleep", "300")
	state := func() (task, probe string) {
		ss, err := h.ListSessions(ctx)
		if err != nil || len(ss) != 1 {
			t.Fatalf("sessions = %+v %v", ss, err)
		}
		return ss[0].Task, ss[0].Probe
	}
	if err := h.SetOption(ctx, "adopted", OptionTask, "minion-probe-debugging"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetOption(ctx, "adopted", OptionProbe, "nonce-9"); err != nil {
		t.Fatal(err)
	}
	if task, probe := state(); task != "minion-probe-debugging" || probe != "nonce-9" {
		t.Errorf("task %q probe %q", task, probe)
	}
	if err := h.SetOption(ctx, "adopted", OptionProbe, ""); err != nil {
		t.Fatal(err)
	}
	if task, probe := state(); task != "minion-probe-debugging" || probe != "" {
		t.Errorf("after clearing the probe: task %q probe %q", task, probe)
	}
}

// A tmux server that is running without any session (exit-empty off, or the
// moment the last session ended) answers list-panes -a with "no current
// target". That is an empty list, not an error: a Worker on such a server must
// list nothing and still be able to create its first session.
func TestIntegrationAServerWithoutSessionsListsNothingAndCreatesTheFirstSession(t *testing.T) {
	h, srv := newRealHost(t) // tmuxtest starts the server with exit-empty off and no session
	got, err := h.ListSessions(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("ListSessions on a server without sessions = %+v, %v", got, err)
	}
	s, err := h.CreateSession(context.Background(), "first", "", []string{"sleep", "60"})
	if err != nil || s.Name != "first" {
		t.Fatalf("CreateSession = %+v, %v", s, err)
	}
	if got, err = h.ListSessions(context.Background()); err != nil || len(got) != 1 {
		t.Fatalf("ListSessions after the create = %+v, %v", got, err)
	}
	_ = srv
}
