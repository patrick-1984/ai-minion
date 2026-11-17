package tmux

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The debug log says which binary and version the host found, and one line per
// listing whose result changed: the count and the sessions psmux cannot address.
func TestDebugLinesOfVersionAndListing(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakePsmux{}
	h, err := New("", WithPsmux(), WithExec(f.exec), WithBinary(`C:\psmux.exe`), WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := h.ListSessions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	for _, want := range []string{"terminal host version check", "psmux 3.4.0", "terminal host ready", "terminal host listing", "sessions=2", "dropped_unaddressable=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "terminal host listing"); n != 1 {
		t.Errorf("%d listing lines for three identical listings:\n%s", n, got)
	}
	// Quiet without a logger.
	h.SetLogger(nil)
	if _, err := h.ListSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDebugListNamesDroppedSessions(t *testing.T) {
	var out bytes.Buffer
	h := &Host{psmux: true, bin: "psmux"}
	h.SetLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	h.debugList(1, []string{"a.b", "c:d"}, nil)
	if got := out.String(); !strings.Contains(got, "dropped_unaddressable=2") || !strings.Contains(got, `a.b`) {
		t.Fatalf("log = %s", got)
	}
}
