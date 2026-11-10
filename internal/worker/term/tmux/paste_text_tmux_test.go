//go:build unix && tmux

package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/fakeagent"
)

// Security finding 3: send_text pasted through PasteText, which sent neither
// -p nor -r. A receiver that enabled bracketed paste got "first\rsecond" without
// the paste markers, so a multi-line text submitted a prompt although no Enter
// was asked for. PasteText now is the same guarded verbatim paste as
// PasteMarked.
func TestPasteTextDeliversBytesVerbatim(t *testing.T) {
	msgs := map[string]string{
		"lf and tab":  "one\ntwo\tT",
		"two lines":   "first\nsecond",
		"punctuation": "$ #{x} 'q' \"q\" ; \\ ` <> | & % ~ ^ ( ) [ ] * ? !",
		"utf8":        "\u00e9 \u65e5\u672c\u8a9e \U0001F600 \u03a9mega",
	}
	for name, msg := range msgs {
		msg := msg
		t.Run("raw/"+name, func(t *testing.T) {
			r := newRig(t)
			p := r.agent(fakeagent.ModeDump)
			if err := r.h.PasteText(context.Background(), p.ref, msg, p.ref); err != nil {
				t.Fatal(err)
			}
			// Without bracketed paste the bytes arrive as they are: LF stays LF
			// (it is never rewritten to the CR of the Enter key).
			p.waitLog(func(l string) bool { return l == hexOf(msg) })
			if got := p.buffers(); got != "" {
				t.Fatalf("a buffer is left: %q", got)
			}
		})
		t.Run("bracketed/"+name, func(t *testing.T) {
			r := newRig(t)
			p := r.agent(fakeagent.ModeDumpBP)
			if err := r.h.PasteText(context.Background(), p.ref, msg, p.ref); err != nil {
				t.Fatal(err)
			}
			want := "1b5b3230307e" + hexOf(msg) + "1b5b3230317e"
			p.waitLog(func(l string) bool { return l == want })
		})
	}
}

func TestPasteTextRefusesInputControls(t *testing.T) {
	r := newRig(t)
	p := r.agent(fakeagent.ModeDump)
	for _, msg := range []string{"a\rb", "a\x1bb", "a\x01b", "a\x00b", "a\x7fb", "a\u0085b", "a\u009bb", "a\x1b[201~b"} {
		err := r.h.PasteText(context.Background(), p.ref, msg, p.ref)
		if !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%q: err = %v, want ErrInvalidArgument", msg, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := p.log(); got != "" {
		t.Fatalf("bytes reached the pane: %q", got)
	}
	if got := p.buffers(); got != "" {
		t.Fatalf("a buffer is left: %q", got)
	}
}

// A two-line text to an agent that enables bracketed paste is not submitted by
// the paste: the pasted LF stays part of the input row.
func TestPasteTextTwoLinesDoNotSubmit(t *testing.T) {
	r := newRig(t)
	p := r.agent(fakeagent.ModeBP)
	p.waitShown("> ")
	if err := r.h.PasteText(context.Background(), p.ref, "first line\nsecond line", p.ref); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := p.log(); got != "" {
		t.Fatalf("a paste without an Enter submitted: %q", got)
	}
	if _, err := r.h.SendKeys(context.Background(), p.ref, []string{"Enter"}, p.ref); err != nil {
		t.Fatal(err)
	}
	p.waitLog(func(l string) bool { return strings.Count(l, "submitted") == 1 })
}
