package logx

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestRingKeepsTheLastLines(t *testing.T) {
	r := NewRing(3, 1<<20)
	for i := range 5 {
		r.Add(fmt.Sprintf("line %d", i))
	}
	got := r.Tail(0)
	if strings.Join(got, ",") != "line 2,line 3,line 4" {
		t.Fatalf("tail = %q", got)
	}
	if got := r.Tail(2); strings.Join(got, ",") != "line 3,line 4" {
		t.Fatalf("tail(2) = %q", got)
	}
	if got := r.Tail(10); len(got) != 3 {
		t.Fatalf("tail(10) = %q", got)
	}
	n, _, dropped := r.Stats()
	if n != 3 || dropped != 2 {
		t.Fatalf("stats = %d lines, %d dropped", n, dropped)
	}
}

func TestRingBoundsBytes(t *testing.T) {
	r := NewRing(100, 10)
	r.Add("abcde")
	r.Add("fghij")
	r.Add("klm")
	if got := r.Tail(0); strings.Join(got, ",") != "fghij,klm" {
		t.Fatalf("tail = %q", got)
	}
	_, b, _ := r.Stats()
	if b != 8 {
		t.Fatalf("bytes = %d", b)
	}
	// A line longer than the whole budget is still kept (alone).
	r.Add(strings.Repeat("x", 20))
	if got := r.Tail(0); len(got) != 1 {
		t.Fatalf("tail = %q", got)
	}
	// A very long line is cut.
	big := NewRing(10, 1<<20)
	big.Add(strings.Repeat("y", maxRingLine*2))
	if l := big.Tail(1)[0]; len(l) > maxRingLine+16 || !strings.HasSuffix(l, "(cut)") {
		t.Fatalf("long line = %d bytes", len(l))
	}
}

func TestRingWriteSplitsLines(t *testing.T) {
	r := NewRing(10, 1<<20)
	_, _ = r.Write([]byte("a\r\nb\n\nc\n"))
	if got := r.Tail(0); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("tail = %q", got)
	}
}

func TestNewLiveTeesAndSwitchesLevel(t *testing.T) {
	var out bytes.Buffer
	ring := NewRing(100, 1<<20)
	log, live, err := NewLive(Options{Level: "info", Format: FormatText, Output: &out}, ring)
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden debug")
	log.Info("shown info", "token", Secret("s3cret"))
	if live.Debug() {
		t.Fatal("debug on at start")
	}
	live.SetDebug(true)
	if !live.Debug() {
		t.Fatal("debug off after SetDebug(true)")
	}
	log.With("module", "x").Debug("now debug")
	live.SetDebug(false)
	log.Debug("hidden again")

	lines := strings.Join(ring.Tail(0), "\n")
	for _, want := range []string{"shown info", "now debug", "module=x", "token=[redacted]"} {
		if !strings.Contains(lines, want) {
			t.Errorf("ring lacks %q:\n%s", want, lines)
		}
	}
	for _, not := range []string{"hidden debug", "hidden again", "s3cret"} {
		if strings.Contains(lines, not) || strings.Contains(out.String(), not) {
			t.Errorf("%q was written", not)
		}
	}
	if !strings.Contains(out.String(), "now debug") || !strings.Contains(out.String(), "shown info") {
		t.Errorf("output = %s", out.String())
	}
}

func TestLiveKeepsAConfiguredDebugLevel(t *testing.T) {
	_, live, err := NewLive(Options{Level: "debug", Output: &bytes.Buffer{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.SetDebug(false)
	if !live.Debug() {
		t.Fatal("ending remote debug lowered a --debug Worker to info")
	}
	if live.Ring == nil {
		t.Fatal("no default ring")
	}
	if _, _, err := NewLive(Options{Level: "loud"}, nil); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestTeeSkipsDisabledHandlers(t *testing.T) {
	var a, b bytes.Buffer
	h := Tee(slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelWarn}), nil,
		slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("tee not enabled for debug")
	}
	l := slog.New(h).WithGroup("g")
	l.Info("x", "k", 1)
	if a.Len() != 0 || !strings.Contains(b.String(), "g.k=1") {
		t.Fatalf("a=%q b=%q", a.String(), b.String())
	}
}

func TestDedupWritesOnlyChanges(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var d Dedup
	d.Debug(log, "a", "listing", "n", 1)
	d.Debug(log, "a", "listing", "n", 1)
	d.Debug(log, "a", "listing", "n", 1)
	d.Debug(log, "b", "listing", "n", 2)
	got := out.String()
	if strings.Count(got, "listing") != 2 || !strings.Contains(got, "unchanged_before=2") {
		t.Fatalf("out = %s", got)
	}
	// Not at debug level: nothing, and no panic on nil.
	d.Debug(nil, "c", "x")
	quiet := slog.New(slog.NewTextHandler(&out, nil))
	d.Debug(quiet, "d", "never")
	if strings.Contains(out.String(), "never") {
		t.Fatal("wrote at info level")
	}
}
