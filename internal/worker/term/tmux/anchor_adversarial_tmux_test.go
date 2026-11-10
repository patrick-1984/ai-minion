//go:build unix && tmux

package tmux

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/fakeagent"
)

// A paste buffer that belongs to someone else on the same tmux server is
// neither pasted, replaced nor deleted by PasteMarked, on success or when the
// KEEP predicate refuses.
func TestPasteMarkedLeavesForeignBuffersAlone(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.tmux("set-buffer", "-b", "theirs", "foreign-content")
	r.tmux("set-buffer", "unnamed-content")
	before := r.tmux("list-buffers", "-F", "#{buffer_name}:#{buffer_size}")
	p := r.agent(fakeagent.ModeDump)
	if _, err := r.h.PasteMarked(ctx, p.ref, "mine", p.ref); err != nil {
		t.Fatal(err)
	}
	p.waitLog(func(l string) bool { return l == hexOf("mine") })
	r.tmux("select-pane", "-d", "-t", p.ref.ID)
	if _, err := r.h.PasteMarked(ctx, p.ref, "refused", p.ref); !isChanged(err) {
		t.Fatalf("err = %v", err)
	}
	if after := r.tmux("list-buffers", "-F", "#{buffer_name}:#{buffer_size}"); after != before {
		t.Fatalf("buffers changed\nbefore %q\n after %q", before, after)
	}
	if got := strings.TrimSpace(r.tmux("show-buffer", "-b", "theirs")); got != "foreign-content" {
		t.Fatalf("foreign buffer = %q", got)
	}
	if got := p.log(); got != hexOf("mine") {
		t.Fatalf("log = %q: the refused text or a foreign buffer reached the pane", got)
	}
}

// Parallel marked pastes to different panes of one server do not share or
// leak buffers, and every receiver gets exactly its own bytes.
func TestPasteMarkedParallelPanes(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	const n = 6
	panes := make([]*pane, n)
	for i := range panes {
		panes[i] = r.agent(fakeagent.ModeDump)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, p := range panes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.h.PasteMarked(ctx, p.ref, strings.Repeat(string(rune('a'+i)), 50)+"\ttab\nline", p.ref)
		}()
	}
	wg.Wait()
	for i, p := range panes {
		if errs[i] != nil {
			t.Fatalf("pane %d: %v", i, errs[i])
		}
		want := hexOf(strings.Repeat(string(rune('a'+i)), 50) + "\ttab\nline")
		p.waitLog(func(l string) bool { return l == want })
	}
	time.Sleep(200 * time.Millisecond)
	if got := r.tmux("list-buffers"); strings.TrimSpace(got) != "" {
		t.Fatalf("buffers left: %q", got)
	}
}

// The largest accepted message arrives complete.
func TestPasteMarkedLargestMessage(t *testing.T) {
	r := newRig(t)
	p := r.agent(fakeagent.ModeDump)
	msg := strings.Repeat("0123456789abcdef\n", MaxPasteBytes/17)
	if _, err := r.h.PasteMarked(context.Background(), p.ref, msg, p.ref); err != nil {
		t.Fatal(err)
	}
	want := hexOf(msg)
	deadline := time.Now().Add(60 * time.Second)
	for len(p.log()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := p.log(); got != want {
		t.Fatalf("received %d of %d hex digits, equal prefix %d", len(got), len(want), commonPrefix(got, want))
	}
	if got := p.buffers(); got != "" {
		t.Fatalf("buffers left: %q", got)
	}
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}
