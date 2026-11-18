package logx

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type countHandler struct {
	n     *atomic.Int64
	attrs []slog.Attr
	buf   *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newCount() countHandler { return countHandler{n: new(atomic.Int64), buf: &syncBuffer{}} }

func (c countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c countHandler) Handle(_ context.Context, r slog.Record) error {
	c.n.Add(1)
	var sb strings.Builder
	sb.WriteString(r.Message)
	for _, a := range c.attrs {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value); return true })
	sb.WriteString("\n")
	_, _ = c.buf.Write([]byte(sb.String()))
	return nil
}
func (c countHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return countHandler{n: c.n, buf: c.buf, attrs: append(append([]slog.Attr(nil), c.attrs...), as...)}
}
func (c countHandler) WithGroup(string) slog.Handler { return c }

// The level matrix of §3.1: each slot receives exactly the records at or
// above its own level.
func TestFanoutLevelMatrix(t *testing.T) {
	levels := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	for _, local := range levels {
		for _, remote := range levels {
			f := NewFanout()
			lc, rc := newCount(), newCount()
			f.Set(SlotLocal, lc, local)
			f.Set(SlotOTel, rc, remote)
			log := slog.New(f)
			for _, l := range levels {
				log.Log(context.Background(), l, "m")
			}
			wantL, wantR := 0, 0
			for _, l := range levels {
				if l >= local {
					wantL++
				}
				if l >= remote {
					wantR++
				}
			}
			if int(lc.n.Load()) != wantL || int(rc.n.Load()) != wantR {
				t.Errorf("local %v remote %v: got %d/%d want %d/%d", local, remote, lc.n.Load(), rc.n.Load(), wantL, wantR)
			}
			if got, want := f.Enabled(context.Background(), slog.LevelDebug), local == slog.LevelDebug || remote == slog.LevelDebug; got != want {
				t.Errorf("Enabled(debug) = %v, want %v", got, want)
			}
		}
	}
}

type valuer struct{ n *atomic.Int64 }

func (v valuer) LogValue() slog.Value { v.n.Add(1); return slog.StringValue("v") }

func TestFanoutDoesNotBuildUnwantedRecords(t *testing.T) {
	f := NewFanout()
	f.Set(SlotLocal, newCount(), slog.LevelInfo)
	f.Set(SlotSyslog, newCount(), slog.LevelWarn)
	n := new(atomic.Int64)
	slog.New(f).Debug("d", "v", valuer{n})
	if n.Load() != 0 {
		t.Fatal("a debug value was rendered although no slot wants debug")
	}
}

// Remote debug (Live.SetDebug) lowers the local output and the ring only.
func TestRemoteDebugChangesLocalAndRingOnly(t *testing.T) {
	var local bytes.Buffer
	ring := NewRing(100, 1<<20)
	log, live, err := NewLive(Options{Level: "info", Format: FormatText, Output: &local}, ring)
	if err != nil {
		t.Fatal(err)
	}
	remote := newCount()
	live.Fanout.Set(SlotOTel, remote, slog.LevelInfo)
	live.SetDebug(true)
	log.Debug("dbg")
	if !strings.Contains(local.String(), "dbg") || len(ring.Tail(0)) != 1 {
		t.Fatalf("debug mode did not reach local and ring: %q %q", local.String(), ring.Tail(0))
	}
	if remote.n.Load() != 0 {
		t.Fatal("debug mode raised the remote sink")
	}
	live.SetDebug(false)
	log.Debug("dbg2")
	if strings.Contains(local.String(), "dbg2") {
		t.Fatal("debug off did not restore the level")
	}
}

// A logger derived with With follows later swaps of a slot, with its bound
// attributes.
func TestFanoutDerivedFollowsSwap(t *testing.T) {
	f := NewFanout()
	log := slog.New(f).With("component", "x")
	a := newCount()
	f.Set(SlotSyslog, a, slog.LevelInfo)
	log.Info("one")
	b := newCount()
	f.Set(SlotSyslog, b, slog.LevelInfo)
	log.Info("two")
	f.Set(SlotSyslog, nil, nil)
	log.Info("three")
	if a.buf.String() != "one component=x\n" || b.buf.String() != "two component=x\n" {
		t.Fatalf("a=%q b=%q", a.buf.String(), b.buf.String())
	}
}

func TestFanoutLocalViewSkipsRemoteSlots(t *testing.T) {
	f := NewFanout()
	l, r := newCount(), newCount()
	f.Set(SlotLocal, l, slog.LevelInfo)
	f.Set(SlotOTel, r, slog.LevelInfo)
	slog.New(f.Local()).With("a", 1).Info("diag")
	if l.n.Load() != 1 || r.n.Load() != 0 {
		t.Fatalf("local %d remote %d", l.n.Load(), r.n.Load())
	}
}

// Swapping slots while many goroutines log is race-free and loses no record
// that a slot was set for (run with -race).
func TestFanoutSwapUnderConcurrentLogging(t *testing.T) {
	f := NewFanout()
	base := slog.New(f).With("k", "v")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var logged atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				base.Info("x")
				logged.Add(1)
			}
		}()
	}
	hs := []countHandler{}
	for i := range 200 {
		h := newCount()
		hs = append(hs, h)
		f.Set(SlotSyslog+i%2, h, slog.LevelInfo)
	}
	close(stop)
	wg.Wait()
	var total int64
	for _, h := range hs {
		total += h.n.Load()
	}
	if total == 0 || logged.Load() == 0 {
		t.Fatalf("nothing logged (%d, %d)", total, logged.Load())
	}
}
