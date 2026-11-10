package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Security finding 3 at the adapter: a text with an input control is refused
// before tmux runs, whatever the pane is. TAB and LF are the explicit
// exceptions, because they are the only controls a verbatim paste delivers
// unchanged.
func TestPasteTextRefusesControlsWithoutRunningTmux(t *testing.T) {
	live := paneA
	s := &script{handler: livePane(&live)}
	h := s.host(t)
	for _, text := range []string{"a\rb", "a\r\nb", "a\x1bb", "a\x1b[201~b", "a\x00b", "a\x03b", "a\x7fb", "a\u0085b", "a\u009bb"} {
		if err := h.PasteText(context.Background(), paneA, text, paneA); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%q: err = %v, want ErrInvalidArgument", text, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("tmux ran for refused text: %v", s.subs())
	}
}

// The paste is verbatim and bracketed (-p -r) and its KEEP check is the tmux
// condition of the same invocation, so the pane cannot change between the
// check and the paste.
func TestPasteTextUsesTheGuardedVerbatimPaste(t *testing.T) {
	live := paneA
	s := &script{}
	s.handler = func(sub string, a []string, in []byte) (string, error) {
		if sub == "display-message" {
			return stateOf(live), nil
		}
		return "", nil
	}
	h := s.host(t)
	_ = h.PasteText(context.Background(), paneA, "x\ty\nz", paneA)
	joined := ""
	for _, c := range s.calls {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "paste-buffer -d -p -r") {
		t.Errorf("the paste is not bracketed and verbatim (-p -r):\n%s", joined)
	}
	if !strings.Contains(joined, "if-shell -F") {
		t.Errorf("the KEEP check is not part of the paste invocation:\n%s", joined)
	}
}
