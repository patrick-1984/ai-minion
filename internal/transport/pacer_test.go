package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// emuLink is one direction of an emulated network path: a sender buffer of
// limit bytes (the socket buffer), a bottleneck of bw bytes per second and a
// one-way delay. Writes block while the buffer is full.
type emuLink struct {
	bw    float64
	delay time.Duration
	limit int

	mu     sync.Mutex
	cond   *sync.Cond
	txq    []emuChunk // arrive holds the time the chunk was queued
	queued int
	closed bool

	flight chan emuChunk

	rmu   sync.Mutex
	rcond *sync.Cond
	rbuf  []byte
	rdone bool
}

type emuChunk struct {
	b      []byte
	arrive time.Time
}

func newEmuLink(bw float64, delay time.Duration, limit int) *emuLink {
	l := &emuLink{bw: bw, delay: delay, limit: limit, flight: make(chan emuChunk, 1<<14)}
	l.cond = sync.NewCond(&l.mu)
	l.rcond = sync.NewCond(&l.rmu)
	go l.transmit()
	go l.deliver()
	return l
}

func (l *emuLink) write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 16<<10)
		b := append([]byte(nil), p[:n]...)
		l.mu.Lock()
		for !l.closed && l.queued+n > l.limit {
			l.cond.Wait()
		}
		if l.closed {
			l.mu.Unlock()
			return total - len(p), net.ErrClosed
		}
		l.queued += n
		l.txq = append(l.txq, emuChunk{b: b, arrive: time.Now()})
		l.cond.Broadcast()
		l.mu.Unlock()
		p = p[n:]
	}
	return total, nil
}

// transmit serialises the queued chunks at the bottleneck rate.
func (l *emuLink) transmit() {
	var free time.Time
	for {
		l.mu.Lock()
		for !l.closed && len(l.txq) == 0 {
			l.cond.Wait()
		}
		if l.closed {
			l.mu.Unlock()
			close(l.flight)
			return
		}
		q := l.txq[0]
		l.txq = l.txq[1:]
		l.mu.Unlock()
		b := q.b

		// Virtual time: a chunk starts when the link is free or when it was
		// queued, whichever is later, so a late wake-up loses no bandwidth.
		start := q.arrive
		if free.After(start) {
			start = free
		}
		free = start.Add(time.Duration(float64(len(b)) / l.bw * float64(time.Second)))
		if d := time.Until(free); d > 0 {
			time.Sleep(d)
		}
		l.mu.Lock()
		l.queued -= len(b)
		l.cond.Broadcast()
		l.mu.Unlock()
		l.flight <- emuChunk{b: b, arrive: free.Add(l.delay)}
	}
}

// deliver hands chunks to the reader after the one-way delay.
func (l *emuLink) deliver() {
	for c := range l.flight {
		if d := time.Until(c.arrive); d > 0 {
			time.Sleep(d)
		}
		l.rmu.Lock()
		l.rbuf = append(l.rbuf, c.b...)
		l.rcond.Broadcast()
		l.rmu.Unlock()
	}
	l.rmu.Lock()
	l.rdone = true
	l.rcond.Broadcast()
	l.rmu.Unlock()
}

func (l *emuLink) read(p []byte) (int, error) {
	l.rmu.Lock()
	defer l.rmu.Unlock()
	for len(l.rbuf) == 0 && !l.rdone {
		l.rcond.Wait()
	}
	if len(l.rbuf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, l.rbuf)
	l.rbuf = l.rbuf[n:]
	return n, nil
}

func (l *emuLink) close() {
	l.mu.Lock()
	l.closed = true
	l.cond.Broadcast()
	l.mu.Unlock()
}

// emuConn is one end of an emulated path.
type emuConn struct {
	in, out *emuLink
	once    sync.Once
}

func (c *emuConn) Read(p []byte) (int, error)  { return c.in.read(p) }
func (c *emuConn) Write(p []byte) (int, error) { return c.out.write(p) }
func (c *emuConn) Close() error {
	c.once.Do(func() { c.out.close(); c.in.close() })
	return nil
}
func (c *emuConn) LocalAddr() net.Addr              { return legAddr(0) }
func (c *emuConn) RemoteAddr() net.Addr             { return legAddr(0) }
func (c *emuConn) SetDeadline(time.Time) error      { return nil }
func (c *emuConn) SetReadDeadline(time.Time) error  { return nil }
func (c *emuConn) SetWriteDeadline(time.Time) error { return nil }

// emuSessions connects a Hub and a Worker session over an emulated path with
// files.v1 on both ends.
func emuSessions(t testing.TB, bw float64, oneWay time.Duration, buffer int, pc PacerConfig) (hub, worker *Session) {
	t.Helper()
	ab := newEmuLink(bw, oneWay, buffer)
	ba := newEmuLink(bw, oneWay, buffer)
	a := &emuConn{in: ba, out: ab}
	b := &emuConn{in: ab, out: ba}
	opts := SessionOptions{Files: true, Pacer: pc}
	hub, err := NewSession(a, RoleHub, opts)
	if err != nil {
		t.Fatal(err)
	}
	worker, err = NewSession(b, RoleWorker, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close(); _ = worker.Close() })
	return hub, worker
}

// echoProbe opens an op stream from the Hub, echoes frames on the Worker and
// measures round trips every interval until stop is closed.
type echoProbe struct {
	st *Stream
}

func startEcho(t testing.TB, ctx context.Context, hub, worker *Session) *echoProbe {
	t.Helper()
	st, err := hub.OpenOp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(map[string]any{"type": "hello"}); err != nil {
		t.Fatal(err)
	}
	in, err := worker.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			body, err := in.ReadFrame()
			if err != nil {
				return
			}
			if err := in.WriteFrame(map[string]any{"type": "echo", "n": len(body)}); err != nil {
				return
			}
		}
	}()
	if _, err := st.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	return &echoProbe{st: st}
}

func (e *echoProbe) measure(t testing.TB, d, every time.Duration) []time.Duration {
	t.Helper()
	var out []time.Duration
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		start := time.Now()
		if err := e.st.WriteFrame(map[string]any{"type": "key", "k": "a"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.ReadFrame(); err != nil {
			t.Fatal(err)
		}
		out = append(out, time.Since(start))
		time.Sleep(every)
	}
	return out
}

func percentile(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return s[i]
}

// startPull runs an endless GET on a leg from the Hub to the Worker (the
// Worker sends, like a pull) and counts the bytes the Hub receives.
func startPull(t testing.TB, ctx context.Context, hub, worker *Session) *atomic.Int64 {
	t.Helper()
	var got atomic.Int64
	go func() {
		st, err := worker.Accept(ctx)
		if err != nil {
			return
		}
		chunk := pattern(256 << 10)
		_ = ServeLeg(ctx, NewLegServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		})), NewLegConn(st))
	}()
	c, err := hub.OpenLeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, LegURL(proto.FilesLegPath("x")), nil)
		resp, err := (&http.Client{Transport: NewLegTransport(c)}).Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 256<<10)
		for {
			n, err := resp.Body.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	return &got
}

type loadResult struct {
	base, loaded time.Duration // p99 round trips
	rate         float64       // bytes per second of the transfer
	linkEst      float64
}

// runLoad measures the echo p99 without and with a saturating pull over an
// emulated 100 Mbit/s path with 10 ms RTT and a 4 MiB socket buffer.
func runLoad(t *testing.T, pc PacerConfig) loadResult {
	const (
		bw     = 12.5e6 // 100 Mbit/s
		oneWay = 5 * time.Millisecond
		buffer = 4 << 20
	)
	hub, worker := emuSessions(t, bw, oneWay, buffer, pc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	echo := startEcho(t, ctx, hub, worker)
	base := echo.measure(t, 400*time.Millisecond, 10*time.Millisecond)
	got := startPull(t, ctx, hub, worker)
	start := time.Now()
	loaded := echo.measure(t, 3*time.Second, 10*time.Millisecond)
	rate := float64(got.Load()) / time.Since(start).Seconds()
	return loadResult{
		base:    percentile(base, 0.99),
		loaded:  percentile(loaded, 0.99),
		rate:    rate,
		linkEst: worker.Pacer().LinkRate(),
	}
}

// NFR-3: under a saturating transfer, the added p99 latency of an op stream
// on the same connection stays within 50 ms at 10 ms RTT, and the transfer
// still uses most of the link.
func TestPacerKeepsOpLatencyUnderBulkLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if raceEnabled {
		t.Skip("timing test; the race detector distorts it")
	}
	r := runLoad(t, PacerConfig{})
	added := r.loaded - r.base
	t.Logf("paced: base p99 %v, loaded p99 %v (+%v), transfer %.1f MB/s, link estimate %.1f MB/s",
		r.base, r.loaded, added, r.rate/1e6, r.linkEst/1e6)
	if added > 50*time.Millisecond {
		t.Errorf("added p99 latency %v > 50 ms", added)
	}
	if r.rate < 0.5*12.5e6 {
		t.Errorf("transfer %.1f MB/s uses less than half of the 12.5 MB/s link", r.rate/1e6)
	}
}

// The same load without the token bucket: the emulated socket buffer fills
// and the op stream waits behind it. This shows the test path can bloat.
func TestUnpacedBulkLoadBloatsTheQueue(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if raceEnabled {
		t.Skip("timing test; the race detector distorts it")
	}
	r := runLoad(t, PacerConfig{Disabled: true})
	t.Logf("unpaced: base p99 %v, loaded p99 %v, transfer %.1f MB/s", r.base, r.loaded, r.rate/1e6)
	if r.loaded-r.base < 100*time.Millisecond {
		t.Errorf("unpaced added p99 %v: the emulated path does not queue", r.loaded-r.base)
	}
}

// BenchmarkLegPull measures the throughput of one leg over an in-memory pipe
// (no pacer limit): the cost of yamux, chunking and HTTP framing.
func BenchmarkLegPull(b *testing.B) {
	a, c := net.Pipe()
	opts := SessionOptions{Files: true, Pacer: PacerConfig{Disabled: true}}
	hub, err := NewSession(a, RoleHub, opts)
	if err != nil {
		b.Fatal(err)
	}
	worker, err := NewSession(c, RoleWorker, opts)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = hub.Close(); _ = worker.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := startPull(b, ctx, hub, worker)
	b.SetBytes(1 << 20)
	b.ResetTimer()
	target := int64(b.N) << 20
	for got.Load() < target {
		time.Sleep(time.Millisecond)
	}
}

// fakeClock-free unit tests of the controller: samples are fed directly.

func newTestPacer(cfg PacerConfig) *Pacer {
	return newPacer(cfg, func() (time.Duration, error) { return 0, errors.New("no ping") }, make(chan struct{}))
}

func TestPacerCutsRateToShareOfMeasuredLink(t *testing.T) {
	p := newTestPacer(PacerConfig{InitialRate: 100e6})
	t0 := time.Unix(1000, 0)
	p.sample(t0, 10*time.Millisecond) // base 10 ms, first sample of the period
	// The queue grows from 20 to 40 ms over 100 ms while 2.4 MB were sent:
	// the link carried 2.4 MB in 120 ms = 20 MB/s.
	p.lastDelay = 20 * time.Millisecond
	p.sent = 2_400_000
	p.sample(t0.Add(100*time.Millisecond), 50*time.Millisecond)
	if got := p.LinkRate(); got < 19.9e6 || got > 20.1e6 {
		t.Fatalf("link estimate %.0f, want 20e6", got)
	}
	// Delay 40 ms > 2x target: 0.8 * 0.8 of the link.
	if got := p.Rate(); got < 12.7e6 || got > 12.9e6 {
		t.Fatalf("rate %.0f, want 12.8e6", got)
	}
}

func TestPacerGrowsWhenQueueIsShortAndLimited(t *testing.T) {
	p := newTestPacer(PacerConfig{InitialRate: 1e6})
	t0 := time.Unix(1000, 0)
	p.sample(t0, 10*time.Millisecond)
	p.limited, p.sent = true, 20_000
	p.sample(t0.Add(20*time.Millisecond), 10*time.Millisecond)
	// Doubling, but at most 1.5 times what the legs got through (1 MB/s).
	if got := p.Rate(); got != 1.5e6 {
		t.Fatalf("slow start: rate %.0f, want 1.5e6", got)
	}
	// Not limited (the legs did not use the rate): hold.
	p.sample(t0.Add(40*time.Millisecond), 10*time.Millisecond)
	if got := p.Rate(); got != 1.5e6 {
		t.Fatalf("unlimited legs changed the rate to %.0f", got)
	}
	// After the first congestion the growth is 1/8 per probe.
	p.grown = true
	p.limited, p.sent = true, 40_000
	p.sample(t0.Add(60*time.Millisecond), 10*time.Millisecond)
	if got := p.Rate(); got != 1.6875e6 {
		t.Fatalf("avoidance: rate %.0f, want 1.6875e6", got)
	}
}

func TestPacerNeverBelowMinRate(t *testing.T) {
	p := newTestPacer(PacerConfig{InitialRate: 200e3, MinRate: 150e3})
	t0 := time.Unix(1000, 0)
	p.sample(t0, time.Millisecond)
	for i := 1; i < 20; i++ {
		p.sample(t0.Add(time.Duration(i)*20*time.Millisecond), 500*time.Millisecond)
	}
	if got := p.Rate(); got != 150e3 {
		t.Fatalf("rate %.0f below the floor", got)
	}
}

func TestPacerWaitHonoursRateDeadlineAndClose(t *testing.T) {
	p := newTestPacer(PacerConfig{InitialRate: 1e6, MinRate: 1e6})
	closed := make(chan struct{})
	// The burst (two chunks) passes at once.
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := p.wait(proto.LegChunkBytes, time.Time{}, closed); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("the burst was delayed")
	}
	// The next 100 KB at 1 MB/s wait about 100 ms.
	start = time.Now()
	if err := p.wait(100_000, time.Time{}, closed); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("waited %v for 100 KB at 1 MB/s", d)
	}
	// A deadline before the tokens: timeout.
	err := p.wait(500_000, time.Now().Add(20*time.Millisecond), closed)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	// Close ends a wait.
	go func() { time.Sleep(20 * time.Millisecond); close(closed) }()
	if err := p.wait(1_000_000, time.Time{}, closed); !errors.Is(err, errPacerClosed) {
		t.Fatalf("close: %v", err)
	}
	var nilPacer *Pacer
	if err := nilPacer.wait(1<<30, time.Time{}, nil); err != nil || nilPacer.Rate() != 0 {
		t.Fatal("a nil pacer must not limit")
	}
	if err := newTestPacer(PacerConfig{Disabled: true}).wait(1<<30, time.Time{}, nil); err != nil {
		t.Fatal("a disabled pacer must not limit")
	}
}

// The probes stop when the legs are idle.
func TestPacerProbesStopWhenIdle(t *testing.T) {
	var pings atomic.Int32
	p := newPacer(PacerConfig{ProbeInterval: 5 * time.Millisecond, InitialRate: 1e9},
		func() (time.Duration, error) { pings.Add(1); return time.Millisecond, nil }, make(chan struct{}))
	base := time.Now()
	var off atomic.Int64
	p.now = func() time.Time { return base.Add(time.Duration(off.Load())) }
	if err := p.wait(1, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a probe", func() bool { return pings.Load() > 0 })
	off.Store(int64(2 * pacerIdle))
	waitFor(t, "probes to stop", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !p.probing
	})
}
