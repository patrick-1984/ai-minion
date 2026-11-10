package term

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// fakeHost is a minimal in-memory SessionHost. It proves that the port can be
// implemented without tmux and shows the KEEP re-check contract that adapters
// follow.
type fakeHost struct {
	sessions map[string]*fakeSession
}

type fakeSession struct {
	pane  PaneRef
	typed []string
	task  string
}

var _ SessionHost = (*fakeHost)(nil)

func (h *fakeHost) ListSessions(context.Context) ([]Session, error) {
	var out []Session
	for name, s := range h.sessions {
		out = append(out, Session{Name: name, Created: time.Unix(0, 0), Windows: 1,
			Panes: []Pane{{Ref: s.pane, Active: true}}, Task: s.task})
	}
	return out, nil
}

func (h *fakeHost) ResolvePane(_ context.Context, t ops.Target) (PaneRef, error) {
	s, ok := h.sessions[t.SessionName]
	if !ok {
		return PaneRef{}, fmt.Errorf("session %q: %w", t.SessionName, ErrNotFound)
	}
	return s.pane, nil
}

func (h *fakeHost) find(p PaneRef) *fakeSession {
	for _, s := range h.sessions {
		if s.pane.ID == p.ID {
			return s
		}
	}
	return nil
}

func (h *fakeHost) Snapshot(_ context.Context, p PaneRef) (ops.Screen, error) {
	s := h.find(p)
	if s == nil {
		return ops.Screen{}, ErrNotFound
	}
	return ops.Screen{Lines: s.typed}, nil
}

func (h *fakeHost) Capture(ctx context.Context, p PaneRef, _ int) (ops.Screen, error) {
	return h.Snapshot(ctx, p)
}

func (h *fakeHost) PasteText(_ context.Context, p PaneRef, text string, expect PaneRef) error {
	s := h.find(p)
	if s == nil {
		return ErrNotFound
	}
	if err := CheckExpect(s.pane, expect); err != nil {
		return err
	}
	s.typed = append(s.typed, text)
	return nil
}

func (h *fakeHost) SendKeys(_ context.Context, p PaneRef, keys []string, expect PaneRef) (int, error) {
	s := h.find(p)
	if s == nil {
		return 0, ErrNotFound
	}
	for i, k := range keys {
		if err := CheckExpect(s.pane, expect); err != nil {
			return i, err
		}
		s.typed = append(s.typed, k)
		if k == "swap" { // simulate the program being replaced after this key
			s.pane.PID++
		}
	}
	return len(keys), nil
}

func (h *fakeHost) CreateSession(_ context.Context, name, _ string, _ []string) (Session, error) {
	if h.sessions == nil {
		h.sessions = map[string]*fakeSession{}
	}
	h.sessions[name] = &fakeSession{pane: PaneRef{ID: "%" + name, PID: 100, Command: "sh"}}
	return Session{Name: name}, nil
}

func (h *fakeHost) SetOption(_ context.Context, session, key, value string) error {
	s, ok := h.sessions[session]
	if !ok {
		return ErrNotFound
	}
	if key == "@minion_task" {
		s.task = value
	}
	return nil
}

func TestPaneRefSame(t *testing.T) {
	a := PaneRef{ID: "%1", PID: 10, Command: "claude"}
	for name, tc := range map[string]struct {
		b    PaneRef
		want bool
	}{
		"equal":           {a, true},
		"pid changed":     {PaneRef{ID: "%1", PID: 11, Command: "claude"}, false},
		"command changed": {PaneRef{ID: "%1", PID: 10, Command: "bash"}, false},
		"id changed":      {PaneRef{ID: "%2", PID: 10, Command: "claude"}, false},
	} {
		if got := a.Same(tc.b); got != tc.want {
			t.Errorf("%s: Same = %v", name, got)
		}
	}
}

func TestCheckExpect(t *testing.T) {
	a := PaneRef{ID: "%1", PID: 10, Command: "claude"}
	if err := CheckExpect(a, a); err != nil {
		t.Fatalf("same pane: %v", err)
	}
	err := CheckExpect(PaneRef{ID: "%1", PID: 99, Command: "claude"}, a)
	if !errors.Is(err, ErrPaneChanged) {
		t.Fatalf("err = %v, want ErrPaneChanged", err)
	}
}

func TestErrorValuesAreDistinctAndWrappable(t *testing.T) {
	if errors.Is(ErrPaneChanged, ErrNotFound) || errors.Is(ErrNotFound, ErrPaneChanged) {
		t.Fatal("error values are not distinct")
	}
	wrapped := fmt.Errorf("resolve: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("wrapped ErrNotFound not matched")
	}
}

func TestSessionActivePane(t *testing.T) {
	s := Session{Panes: []Pane{{Ref: PaneRef{ID: "%1"}}, {Ref: PaneRef{ID: "%2"}, Active: true}}}
	if p, ok := s.ActivePane(); !ok || p.Ref.ID != "%2" {
		t.Fatalf("ActivePane = %+v, %v", p, ok)
	}
	if _, ok := (Session{}).ActivePane(); ok {
		t.Fatal("empty session has an active pane")
	}
}

func TestPortContractWithFakeHost(t *testing.T) {
	ctx := context.Background()
	h := &fakeHost{}
	if _, err := h.CreateSession(ctx, "minion-1", "/tmp", nil); err != nil {
		t.Fatal(err)
	}
	target := ops.Target{SessionName: "minion-1"}
	pane, err := h.ResolvePane(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ResolvePane(ctx, ops.Target{SessionName: "gone"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}

	// A write with the right expectation lands.
	if err := h.PasteText(ctx, pane, "hello", pane); err != nil {
		t.Fatal(err)
	}
	// A write with a stale expectation sends nothing.
	stale := pane
	stale.PID = 1
	before := len(h.sessions["minion-1"].typed)
	if err := h.PasteText(ctx, pane, "nope", stale); !errors.Is(err, ErrPaneChanged) {
		t.Fatalf("stale paste: %v", err)
	}
	if len(h.sessions["minion-1"].typed) != before {
		t.Fatal("text was sent despite ErrPaneChanged")
	}

	// The pane changes after the first key: the count lets the executor
	// report partial, never failed_not_sent.
	sent, err := h.SendKeys(ctx, pane, []string{"swap", "Enter", "Enter"}, pane)
	if !errors.Is(err, ErrPaneChanged) || sent != 1 {
		t.Fatalf("SendKeys = %d, %v; want 1, ErrPaneChanged", sent, err)
	}
	if got := ops.FailureOutcome(sent > 0); got != ops.OutcomePartial {
		t.Fatalf("outcome = %s, want partial", got)
	}

	if err := h.SetOption(ctx, "minion-1", "@minion_task", "do it"); err != nil {
		t.Fatal(err)
	}
	list, _ := h.ListSessions(ctx)
	if len(list) != 1 || list[0].Task != "do it" {
		t.Fatalf("list = %+v", list)
	}
	if err := h.SetOption(ctx, "gone", "@minion_task", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetOption on a missing session: %v", err)
	}
}

// TestCheckPasteText is the one rule of what a paste may carry, shared by the
// tmux and ConPTY adapters.
func TestCheckPasteText(t *testing.T) {
	for _, ok := range []string{"x", "echo hi", "a\nb", "a\tb\n", "tab\tand\nlf", "\u00fcn\u00efcode \u2713", strings.Repeat("x", MaxPasteBytes)} {
		if err := CheckPasteText(ok); err != nil {
			t.Errorf("CheckPasteText(%.20q) = %v, want nil", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty": "", "CR": "a\rb", "CRLF": "a\r\nb", "ESC": "\x1b", "NUL": "a\x00", "DEL": "\x7f", "C1": "\u0085", "CSI": "\u009b",
		"BEL": "\a", "invalid UTF-8": "\xff", "too long": strings.Repeat("x", MaxPasteBytes+1),
	} {
		if err := CheckPasteText(bad); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
}
