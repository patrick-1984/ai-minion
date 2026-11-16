package tmux

// psmux mode (v0.9.2, A21) with a fake psmux: one server per session and pane
// ids per session, so every command addresses a pane through its session.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

type fakePsmux struct {
	mu      sync.Mutex
	version string
	calls   [][]string
}

func (f *fakePsmux) exec(_ context.Context, args []string, _ []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case contains(args, "-V"):
		v := f.version
		if v == "" {
			v = "tmux 3.4.0\npsmux 3.4.0 (20aa1d7 2026-10-13)\n"
		}
		return []byte(v), nil, nil
	case contains(args, "list-panes"):
		// Two sessions, both with pane %1.
		return []byte(codecFromArgs(args).frame(row("%1", "", "pwsh", "alpha"), row("%1", "", "pwsh", "beta"))), nil, nil
	case contains(args, "capture-pane"), contains(args, "display-message"):
		return []byte("x\n"), nil, nil
	}
	return nil, nil, nil
}

func contains(args []string, v string) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

func TestPsmuxDefaultNamespaceAndVersionFloor(t *testing.T) {
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	if contains(f.calls[0], "-S") {
		t.Errorf("the default namespace got -S: %v", f.calls[0])
	}
	if !h.Psmux() {
		t.Error("not in psmux mode")
	}
	if _, err := New("", WithExec(f.exec)); err == nil {
		t.Error("tmux mode accepted an empty socket")
	}
	old := &fakePsmux{version: "tmux 3.4.0\npsmux 3.3.2 (x)\n"}
	if _, err := New("", WithPsmux(), WithExec(old.exec)); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("psmux 3.3 accepted: %v", err)
	}
	notPsmux := &fakePsmux{version: "tmux 3.5a\n"}
	if _, err := New("", WithPsmux(), WithExec(notPsmux.exec)); err == nil {
		t.Error("tmux accepted as psmux")
	}
}

// Two sessions share pane id %1: each is addressed through its own session,
// and a pane without a session target is refused.
func TestPsmuxAddressesPanesThroughTheirSession(t *testing.T) {
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "%1" || ref.Target != "=beta:0.0" {
		t.Fatalf("ref = %+v", ref)
	}
	if _, err := h.Snapshot(ctx, ref); err != nil && !errors.Is(err, term.ErrIncomplete) {
		t.Logf("snapshot: %v", err) // the fake's screen is minimal; only the target matters
	}
	last := f.calls[len(f.calls)-1]
	for _, c := range f.calls {
		if contains(c, "capture-pane") {
			last = c
		}
	}
	if !contains(last, "=beta:0.0") || contains(last, "%1") {
		t.Fatalf("capture targets %v", last)
	}
	if _, err := h.Snapshot(ctx, term.PaneRef{ID: "%1"}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Fatalf("a bare pane id was accepted: %v", err)
	}
	// Writing: the expected pane must be the same session's pane.
	alpha, _ := h.ResolvePane(ctx, ops.Target{SessionName: "alpha"})
	if _, err := h.SendKeys(ctx, ref, []string{"Enter"}, alpha); !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("a key for beta was checked against alpha's pane: %v", err)
	}
	if got := h.bufferTarget(ref.Target); strings.Join(got, " ") != "-t =beta:" {
		t.Errorf("buffer target = %v", got)
	}
}

func TestPsmuxNamesThatCannotBeAddressedAreNotServed(t *testing.T) {
	for _, n := range []string{"%work", "@w", "=x", "a:b", "a.b", "Bob's", `q"`, "c#"} {
		if PsmuxNameOK(n) {
			t.Errorf("%q accepted", n)
		}
	}
	for _, n := range []string{"work", "wb-12", "my work", "x_1"} {
		if !PsmuxNameOK(n) {
			t.Errorf("%q refused", n)
		}
	}
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateSession(context.Background(), "%work", "", nil); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("created %%work: %v", err)
	}
	if err := h.SetOption(context.Background(), "a.b", "@minion_task", "x"); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("set an option on a.b: %v", err)
	}
	// psmux takes -s and -c literally: no format escaping.
	got := strings.Join(newSessionArgs("work", `C:\Projects\C#`, nil, false), " ")
	if !strings.Contains(got, `-c C:\Projects\C#`) || strings.Contains(got, "C##") {
		t.Errorf("args = %s", got)
	}
}

// psmux reads ';' tokens as command separators and $N as a session id.
func TestPsmuxRefusesSeparatorsAndSessionIDs(t *testing.T) {
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if PsmuxNameOK("$0") || PsmuxNameOK("a;b") {
		t.Error("$0 or a;b accepted")
	}
	if _, err := h.CreateSession(ctx, "work", "", []string{"python.exe", "-c", "x", ";", "kill-server"}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("a ';' argument was accepted: %v", err)
	}
	if err := h.SetOption(ctx, "work", "@minion_task", "fix;"); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("a value ending in ';' was accepted: %v", err)
	}
	ref, _ := h.ResolvePane(ctx, ops.Target{SessionName: "beta"})
	if _, err := h.SendKeys(ctx, ref, []string{";"}, ref); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("a ';' key was accepted: %v", err)
	}
	for _, c := range f.calls {
		if contains(c, "kill-server") || contains(c, "new-session") {
			t.Fatalf("ran %v", c)
		}
	}
}
