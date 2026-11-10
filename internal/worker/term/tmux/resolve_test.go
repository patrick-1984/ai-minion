package tmux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Two sessions: wb-1 has window 0 (panes %0 active, %1) and window 1 (pane %2,
// the active pane of an inactive window); minion-2 has one pane.
var resolveSessions = func() []term.Session {
	c := paneCodec{fs: "_F_", rs: "_R_"}
	s, err := c.parse(c.frameTabs(listOut))
	if err != nil {
		panic(err)
	}
	return s
}()

func TestPickPane(t *testing.T) {
	tests := []struct {
		name, session, pane string
		want                string
		err                 error
	}{
		{name: "active pane by default", session: "wb-1", want: "%0"},
		{name: "other session", session: "minion-2", want: "%3"},
		{name: "explicit first pane", session: "wb-1", pane: "0.0", want: "%0"},
		{name: "explicit second pane", session: "wb-1", pane: "0.1", want: "%1"},
		{name: "explicit other window", session: "wb-1", pane: "1.0", want: "%2"},
		{name: "missing pane", session: "wb-1", pane: "0.5", err: term.ErrNotFound},
		{name: "missing window", session: "wb-1", pane: "7.0", err: term.ErrNotFound},
		{name: "missing session", session: "nope", err: term.ErrNotFound},
		{name: "no prefix match", session: "wb", err: term.ErrNotFound},
		{name: "no prefix match 2", session: "minion", err: term.ErrNotFound},
		{name: "case sensitive", session: "WB-1", err: term.ErrNotFound},
		{name: "malformed pane", session: "wb-1", pane: "x", err: ops.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := pickPane(resolveSessions, tc.session, tc.pane)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil || ref.ID != tc.want || ref.PID == 0 || ref.Command == "" {
				t.Fatalf("ref = %+v, err = %v; want %s", ref, err, tc.want)
			}
		})
	}
}

func TestPickPaneNoActive(t *testing.T) {
	s := []term.Session{{Name: "a", Panes: []term.Pane{{Ref: term.PaneRef{ID: "%1"}}}}}
	if _, err := pickPane(s, "a", ""); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestResolvePaneUnit(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: listOut}}}
	h := newFakeHost(t, f)
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb-1"})
	if err != nil || ref != (term.PaneRef{ID: "%0", PID: 101, Command: "bash"}) {
		t.Errorf("ref = %+v, err = %v", ref, err)
	}
	ref, err = h.ResolvePane(ctx, ops.Target{SessionName: "wb-1", Pane: "0.1"})
	if err != nil || ref != (term.PaneRef{ID: "%1", PID: 102, Command: "vim"}) {
		t.Errorf("explicit ref = %+v, err = %v", ref, err)
	}
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "ghost"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("missing err = %v", err)
	}
}

func TestResolvePaneValidatesTarget(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: listOut}}}
	h := newFakeHost(t, f)
	before := len(f.calls)
	for _, tg := range []ops.Target{
		{},
		{SessionName: "a:b"},
		{SessionName: "a.b"},
		{SessionName: "wb-1", Pane: "x"},
		{SessionName: "wb-1", Pane: "0"},
		{WorkerID: "not-a-uuid", SessionName: "wb-1"},
	} {
		if _, err := h.ResolvePane(context.Background(), tg); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("ResolvePane(%+v) err = %v", tg, err)
		}
	}
	if len(f.calls) != before {
		t.Error("an invalid target reached tmux")
	}
}

func TestResolvePaneSessionRecreated(t *testing.T) {
	// R88: the target is bound to the session name, not to the pane id.
	first := "1\t0\t1\t0\t1\t0\t1\t%4\t10\t\t\tbash\twb-1\n"
	second := "2\t0\t1\t0\t1\t0\t1\t%9\t90\t\t\tzsh\twb-1\n"
	f := &fake{reply: map[string]fakeReply{"list-panes": {tabbed: first}}}
	h := newFakeHost(t, f)
	a, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "wb-1"})
	if err != nil || a.ID != "%4" {
		t.Fatalf("first = %+v, %v", a, err)
	}
	f.reply["list-panes"] = fakeReply{tabbed: second}
	b, err := h.ResolvePane(context.Background(), ops.Target{SessionName: "wb-1"})
	if err != nil || b.ID != "%9" || b.Same(a) {
		t.Errorf("after recreate = %+v, %v", b, err)
	}
}

func TestTargetFromSpec(t *testing.T) {
	tests := []struct {
		spec string
		want ops.Target
		ok   bool
	}{
		{"wb-1", ops.Target{SessionName: "wb-1"}, true},
		{"wb-1:0.1", ops.Target{SessionName: "wb-1", Pane: "0.1"}, true},
		{"my session:2.0", ops.Target{SessionName: "my session", Pane: "2.0"}, true},
		{"", ops.Target{}, false},
		{":0.1", ops.Target{}, false},
		{"wb-1:", ops.Target{SessionName: "wb-1"}, true},
		{"wb-1:0", ops.Target{}, false},
		{"wb-1:0.1:2", ops.Target{}, false},
		{"wb-1:x.y", ops.Target{}, false},
	}
	for _, tc := range tests {
		got, err := TargetFromSpec(tc.spec)
		if (err == nil) != tc.ok {
			t.Errorf("TargetFromSpec(%q) err = %v", tc.spec, err)
			continue
		}
		if !tc.ok && !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("TargetFromSpec(%q) err = %v, want ErrInvalidArgument", tc.spec, err)
		}
		if tc.ok && got != tc.want {
			t.Errorf("TargetFromSpec(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestIntegrationResolveActiveVsExplicit(t *testing.T) {
	h, srv := newRealHost(t)
	target := srv.NewSession("two", "sleep", "60")
	// Split the window with -d: the new pane (1) is created without becoming
	// active, so pane 0 stays active and both stay addressable.
	mustRun(t, h, "split-window", "-d", "-t", target, "sleep", "61")
	ctx := context.Background()
	// split-window returns before the new pane has exec'd sleep, like
	// new-session does (see CreateSession): wait, so that the pane is not
	// resolved while it still reports the tmux server as its command.
	waitPaneCommand(t, h, ops.Target{SessionName: "two", Pane: "0.1"}, "sleep")
	zero, err := h.ResolvePane(ctx, ops.Target{SessionName: "two", Pane: "0.0"})
	if err != nil {
		t.Fatal(err)
	}
	one, err := h.ResolvePane(ctx, ops.Target{SessionName: "two", Pane: "0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if zero.ID == one.ID || zero.Command != "sleep" || one.Command != "sleep" || zero.PID == one.PID {
		t.Errorf("panes: %+v %+v", zero, one)
	}
	// -d keeps the original pane active.
	active, err := h.ResolvePane(ctx, ops.Target{SessionName: "two"})
	if err != nil || active != zero {
		t.Errorf("active = %+v, want %+v (err %v)", active, zero, err)
	}
	// Select pane 1: the default now follows it.
	mustRun(t, h, "select-pane", "-t", one.ID)
	active, err = h.ResolvePane(ctx, ops.Target{SessionName: "two"})
	if err != nil || active != one {
		t.Errorf("active after select = %+v, want %+v (err %v)", active, one, err)
	}
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "two", Pane: "0.2"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("missing explicit pane err = %v", err)
	}
	sessions, _ := h.ListSessions(ctx)
	if len(sessions) != 1 || len(sessions[0].Panes) != 2 {
		t.Errorf("sessions = %+v", sessions)
	}
}

func TestIntegrationResolveExactNameAndRecreate(t *testing.T) {
	h, srv := newRealHost(t)
	srv.NewSession("wb-1", "sleep", "60")
	srv.NewSession("wb-10", "sleep", "60")
	ctx := context.Background()
	a, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb-1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb-10"})
	if err != nil || a.ID == b.ID {
		t.Errorf("wb-1 = %+v, wb-10 = %+v, err %v", a, b, err)
	}
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb"}); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("prefix resolved: %v", err)
	}
	// Recreate wb-1: same name, new pane id (R88).
	mustRun(t, h, "kill-session", "-t", "=wb-1")
	srv.NewSession("wb-1", "sleep", "62")
	c, err := h.ResolvePane(ctx, ops.Target{SessionName: "wb-1"})
	if err != nil || c.ID == a.ID || c.PID == a.PID {
		t.Errorf("recreated = %+v, old %+v, err %v", c, a, err)
	}
}

// mustRun runs a tmux command through the adapter's exec helper on the private
// test socket, for test setup that tmuxtest has no helper for.
func mustRun(t *testing.T, h *Host, args ...string) {
	t.Helper()
	if _, err := h.run(context.Background(), args...); err != nil {
		t.Fatalf("tmux %s: %v", args[0], err)
	}
}

// waitPaneCommand polls until the pane runs command, within a bound.
func waitPaneCommand(t *testing.T, h *Host, target ops.Target, command string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ref, err := h.ResolvePane(context.Background(), target)
		if err == nil && ref.Command == command {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pane %+v still runs %q (err %v), want %q", target, ref.Command, err, command)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
