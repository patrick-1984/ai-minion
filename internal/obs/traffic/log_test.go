package traffic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// records parses the JSON lines of buf.
func records(t *testing.T, buf *syncBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if m["msg"] == "worker traffic" {
			out = append(out, m)
		}
	}
	return out
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sessions returns a Hub and a Worker session over net.Pipe, each tapped by
// its own traffic log.
func sessions(t *testing.T, hubTap, workerTap transport.FrameTap) (*transport.Session, *transport.Session) {
	t.Helper()
	a, b := net.Pipe()
	type res struct {
		s   *transport.Session
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{FrameTap: workerTap})
		ch <- res{s, err}
	}()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{FrameTap: hubTap})
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = r.s.Close() })
	return hub, r.s
}

// A loopback exchange: both sides record the request and the result with
// its duration and outcome, in their own direction words; no body field.
func TestLoopbackRequestResultPairs(t *testing.T) {
	var hubOut, workerOut syncBuf
	_, hubLive, _ := logx.NewLiveLocal(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &hubOut})
	ring := logx.NewRing(100, 1<<20)
	_, wLive, _ := logx.NewLive(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &workerOut}, ring)
	hl, wl := New(SideHub, hubLive.Fanout), New(SideWorker, wLive.Fanout)
	on := config.TrafficConfig{Enabled: true, To: []string{config.TrafficToLocal}}
	hl.Configure(on)
	wl.Configure(on)
	hub, worker := sessions(t, hl.Tap(Peer{WorkerID: "w-1", Name: "pi"}), wl.Tap(Peer{}))
	ctx := context.Background()
	go func() {
		st, err := worker.Accept(ctx)
		if err != nil {
			return
		}
		if _, err := st.ReadFrame(); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
		_ = st.WriteFrame(map[string]any{"type": "op.result", "result": map[string]any{"outcome": "delivered", "text": "SECRET-BODY"}})
		_ = st.Close()
	}()
	st, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(map[string]any{"type": "op.request", "op": "send_text", "request_id": "r-1", "args": map[string]any{"text": "SECRET-ARGS"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(records(t, &hubOut)) == 2 && len(records(t, &workerOut)) == 2 })
	h := records(t, &hubOut)
	if h[0]["direction"] != "hub->worker" || h[0]["frame_type"] != "op.request" || h[0]["op"] != "send_text" || h[0]["request_id"] != "r-1" ||
		h[0]["worker_id"] != "w-1" || h[0]["stream"] != "op" || h[0]["level"] != "INFO" {
		t.Fatalf("hub request = %v", h[0])
	}
	if h[1]["direction"] != "worker->hub" || h[1]["frame_type"] != "op.result" || h[1]["op"] != "send_text" || h[1]["outcome"] != "delivered" ||
		h[1]["duration_ms"].(float64) < 15 || h[1]["bytes"].(float64) < 20 {
		t.Fatalf("hub result = %v", h[1])
	}
	w := records(t, &workerOut)
	if w[0]["direction"] != "received" || w[1]["direction"] != "sent" || w[1]["outcome"] != "delivered" {
		t.Fatalf("worker = %v", w)
	}
	for _, out := range []string{hubOut.String(), workerOut.String(), strings.Join(ring.Tail(0), "\n")} {
		if strings.Contains(out, "SECRET") {
			t.Fatalf("a body reached a log: %s", out)
		}
	}
	if len(ring.Tail(0)) != 0 {
		t.Fatalf("the ring got traffic records: %q", ring.Tail(0))
	}
	_ = st.Close()
}

// Off, a Worker outside the filter, and a stalled output: nothing is logged
// or the tap still never blocks.
func TestFilterOffAndNonBlocking(t *testing.T) {
	var out syncBuf
	_, live, _ := logx.NewLiveLocal(logx.Options{Format: logx.FormatJSON, Output: &out})
	l := New(SideHub, live.Fanout)
	tap := l.Tap(Peer{WorkerID: "w-2", Name: "other"})
	frame := transport.TapFrame{Kind: transport.KindOp, Stream: 1, Sent: true, Body: []byte(`{"type":"op.request","op":"x"}`), Size: 34}
	tap.TapFrame(frame)
	l.Configure(config.TrafficConfig{Enabled: true, To: []string{config.TrafficToLocal}, Workers: []string{"w-1", "pi"}})
	tap.TapFrame(frame)
	l.Tap(Peer{WorkerID: "w-9", Name: "pi"}).TapFrame(frame)
	eventually(t, func() bool { return len(records(t, &out)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(records(t, &out)); n != 1 {
		t.Fatalf("%d records, want only the Worker named pi", n)
	}

	// A writer that never returns: the tap stays non-blocking and counts.
	block := make(chan struct{})
	defer close(block)
	_, slive, _ := logx.NewLiveLocal(logx.Options{Format: logx.FormatJSON, Output: blockWriter{block}})
	sl := New(SideHub, slive.Fanout)
	sl.Configure(config.TrafficConfig{Enabled: true, To: []string{config.TrafficToLocal}})
	st := sl.Tap(Peer{WorkerID: "w"})
	start := time.Now()
	for range QueueSize + 500 {
		st.TapFrame(frame)
	}
	if time.Since(start) > 2*time.Second || sl.Dropped() < 400 {
		t.Fatalf("took %s, dropped %d", time.Since(start), sl.Dropped())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	sl.Close(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("Close ignored its deadline")
	}
}

type blockWriter struct{ c chan struct{} }

func (b blockWriter) Write(p []byte) (int, error) { <-b.c; return len(p), io.EOF }

// The record level is fixed (info): a destination slot at error still gets
// it, and the in-memory log never does.
func TestFixedLevelAndNoRing(t *testing.T) {
	var out syncBuf
	ring := logx.NewRing(10, 1<<16)
	_, live, _ := logx.NewLive(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &out}, ring)
	l := New(SideWorker, live.Fanout)
	l.Configure(config.TrafficConfig{Enabled: true, To: []string{config.TrafficToLocal}})
	l.Tap(Peer{}).TapFrame(transport.TapFrame{Kind: transport.KindEvents, Stream: 3, Sent: true, Body: []byte(`{"type":"event"}`), Size: 20})
	eventually(t, func() bool { return len(records(t, &out)) == 1 })
	if len(ring.Tail(0)) != 0 {
		t.Fatal("the ring got a traffic record")
	}
	_ = slog.LevelInfo
}

// BenchmarkWriteFrame compares a stream's WriteFrame without and with a tap
// whose log is off (the frame is still marshalled once).
func BenchmarkWriteFrame(b *testing.B) {
	_, live, _ := logx.NewLiveLocal(logx.Options{Output: io.Discard})
	l := New(SideHub, live.Fanout)
	for _, c := range []struct {
		name string
		tap  transport.FrameTap
	}{{"untapped", nil}, {"tapped-off", l.Tap(Peer{WorkerID: "w"})}} {
		b.Run(c.name, func(b *testing.B) {
			hub, worker := sessionsB(b, c.tap)
			go func() {
				st, err := worker.Accept(context.Background())
				if err == nil {
					_, _ = io.Copy(io.Discard, st)
				}
			}()
			st, err := hub.OpenOp(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			v := map[string]any{"type": "op.request", "op": "session_snapshot", "request_id": "r", "args": map[string]any{"session": "main"}}
			b.ResetTimer()
			for range b.N {
				if err := st.WriteFrame(v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func sessionsB(b *testing.B, tap transport.FrameTap) (*transport.Session, *transport.Session) {
	a, c := net.Pipe()
	ch := make(chan *transport.Session, 1)
	go func() {
		s, _ := transport.NewSession(c, transport.RoleWorker, transport.SessionOptions{})
		ch <- s
	}()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{FrameTap: tap})
	if err != nil {
		b.Fatal(err)
	}
	w := <-ch
	b.Cleanup(func() { _ = hub.Close(); _ = w.Close() })
	return hub, w
}
