package syslog

import (
	"context"
	"crypto/tls"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

// Defaults of a [Writer] (design §3.3).
const (
	DefaultQueueSize    = 1000
	DefaultDialTimeout  = 5 * time.Second
	DefaultWriteTimeout = 5 * time.Second
	DefaultBackoffMin   = time.Second
	DefaultBackoffMax   = 60 * time.Second
)

// Reasons a message is dropped, as the status reports them.
const (
	DropQueueFull   = "queue_full"
	DropWriteFailed = "write_failed"
	DropShutdown    = "shutdown"
	DropReconfigure = "reconfigure"
)

// Options configures a [Writer].
type Options struct {
	// Network is udp, tcp or tls; Addr is host:port.
	Network, Addr string
	// TLS is the client configuration for tls (TLS 1.2 or later).
	TLS *tls.Config
	// QueueSize bounds the queue; the oldest message goes when it is full.
	QueueSize int
	// DialTimeout bounds the dial and, separately, the TLS handshake;
	// WriteTimeout bounds one write.
	DialTimeout, WriteTimeout time.Duration
	// BackoffMin and BackoffMax bound the wait between failed connects.
	BackoffMin, BackoffMax time.Duration
	// Diag receives connection and write errors, for the local-only,
	// rate-limited diagnostics of the sink. Nil drops them.
	Diag func(class string, err error)
	// Dial replaces the network dialer (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Stats is the state of a Writer.
type Stats struct {
	Queued      int
	Sent        uint64
	Dropped     map[string]uint64
	LastSuccess time.Time
	LastError   string
}

// Writer sends syslog messages over one connection from one goroutine. The
// caller never blocks: [Writer.Enqueue] puts a message into a bounded queue
// that drops the oldest message when it is full. A peer that stops reading
// costs at most one write timeout, then the writer reconnects with
// exponential backoff and jitter.
type Writer struct {
	opts Options

	mu      sync.Mutex
	queue   [][]byte // circular
	head, n int
	closing bool

	wake    chan struct{}
	done    chan struct{}
	runCtx  context.Context
	abort   context.CancelFunc
	conn    atomic.Pointer[net.Conn]
	sent    atomic.Uint64
	lastOK  atomic.Int64
	lastErr atomic.Pointer[string]
	// reason is why the writer was stopped (shutdown or reconfigure); the
	// message in hand at that moment is counted under it.
	reason atomic.Pointer[string]

	dmu     sync.Mutex
	dropped map[string]uint64
}

// NewWriter starts a writer.
func NewWriter(opts Options) *Writer {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = DefaultBackoffMin
	}
	if opts.BackoffMax < opts.BackoffMin {
		opts.BackoffMax = max(DefaultBackoffMax, opts.BackoffMin)
	}
	if opts.Diag == nil {
		opts.Diag = func(string, error) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{
		opts: opts, queue: make([][]byte, opts.QueueSize),
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		runCtx: ctx, abort: cancel, dropped: map[string]uint64{},
	}
	go w.run()
	return w
}

// Stream reports whether the transport is a byte stream (tcp, tls) that
// needs octet counting.
func (w *Writer) Stream() bool { return w.opts.Network != "udp" }

// Enqueue queues one message (without framing). It never blocks.
func (w *Writer) Enqueue(msg []byte) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		w.drop(DropShutdown, 1)
		return
	}
	if w.n == len(w.queue) {
		w.queue[w.head] = nil
		w.head = (w.head + 1) % len(w.queue)
		w.n--
		w.mu.Unlock()
		w.drop(DropQueueFull, 1)
		w.mu.Lock()
	}
	w.queue[(w.head+w.n)%len(w.queue)] = msg
	w.n++
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Writer) drop(reason string, n int) {
	if n <= 0 {
		return
	}
	w.dmu.Lock()
	w.dropped[reason] += uint64(n)
	w.dmu.Unlock()
}

// next returns the oldest queued message; ok is false when the writer must
// stop (closing with an empty queue, or aborted).
func (w *Writer) next() ([]byte, bool) {
	for {
		w.mu.Lock()
		if w.n > 0 {
			m := w.queue[w.head]
			w.queue[w.head] = nil
			w.head = (w.head + 1) % len(w.queue)
			w.n--
			w.mu.Unlock()
			return m, true
		}
		closing := w.closing
		w.mu.Unlock()
		if closing {
			return nil, false
		}
		select {
		case <-w.wake:
		case <-w.runCtx.Done():
			return nil, false
		}
	}
}

func (w *Writer) run() {
	defer close(w.done)
	defer w.closeConn()
	var conn net.Conn
	backoff := w.opts.BackoffMin
	for {
		msg, ok := w.next()
		if !ok {
			return
		}
		if w.Stream() {
			msg = OctetFrame(msg)
		}
		retried := false
		for {
			if w.runCtx.Err() != nil {
				w.drop(w.stopReason(), 1)
				return
			}
			if conn == nil {
				c, err := w.connect()
				if err != nil {
					w.fail("connect", err)
					if !w.sleep(jitter(backoff)) {
						w.drop(w.stopReason(), 1)
						return
					}
					backoff = min(backoff*2, w.opts.BackoffMax)
					continue
				}
				conn = c
				w.conn.Store(&c)
				backoff = w.opts.BackoffMin
			}
			_ = conn.SetWriteDeadline(time.Now().Add(w.opts.WriteTimeout))
			if _, err := conn.Write(msg); err != nil {
				w.fail("write", err)
				w.closeConn()
				conn = nil
				if retried {
					w.drop(DropWriteFailed, 1)
					break
				}
				retried = true
				continue
			}
			w.sent.Add(1)
			w.lastOK.Store(time.Now().UnixNano())
			break
		}
	}
}

func (w *Writer) stopReason() string {
	if p := w.reason.Load(); p != nil {
		return *p
	}
	return DropShutdown
}

func (w *Writer) fail(class string, err error) {
	if w.runCtx.Err() != nil {
		return // shutting down: the abort caused it
	}
	s := logx.ErrorText(err)
	w.lastErr.Store(&s)
	w.opts.Diag(class, err)
}

func (w *Writer) closeConn() {
	if p := w.conn.Swap(nil); p != nil && *p != nil {
		_ = (*p).Close()
	}
}

// sleep waits d unless the writer is aborted; it reports whether it slept.
func (w *Writer) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.runCtx.Done():
		return false
	}
}

func jitter(d time.Duration) time.Duration {
	// ±20 %
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func (w *Writer) connect() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(w.runCtx, w.opts.DialTimeout)
	defer cancel()
	network := w.opts.Network
	if network == "tls" {
		network = "tcp"
	}
	var c net.Conn
	var err error
	if w.opts.Dial != nil {
		c, err = w.opts.Dial(ctx, network, w.opts.Addr)
	} else {
		var d net.Dialer
		c, err = d.DialContext(ctx, network, w.opts.Addr)
	}
	if err != nil {
		return nil, err
	}
	if w.opts.Network != "tls" {
		return c, nil
	}
	cfg := w.opts.TLS
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	cfg = cfg.Clone()
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(w.opts.Addr)
	}
	tc := tls.Client(c, cfg)
	hctx, hcancel := context.WithTimeout(w.runCtx, w.opts.DialTimeout)
	defer hcancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return tc, nil
}

// Close stops accepting messages and sends what is queued until ctx ends;
// whatever is left then is dropped and counted as shutdown. It returns
// ctx's error when the queue could not be emptied in time.
func (w *Writer) Close(ctx context.Context) error {
	return w.stop(ctx, DropShutdown)
}

// Discard drops the queue at once and counts it as reconfigure: messages
// queued under an old configuration are never sent.
func (w *Writer) Discard() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = w.stop(ctx, DropReconfigure)
}

func (w *Writer) stop(ctx context.Context, reason string) error {
	w.mu.Lock()
	already := w.closing
	w.closing = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	if already {
		<-w.done
		return nil
	}
	var err error
	if ctx.Err() == nil {
		select {
		case <-w.done:
			return nil
		case <-ctx.Done():
			err = ctx.Err()
		}
	} else {
		err = ctx.Err()
	}
	if reason == DropReconfigure {
		err = nil
	}
	w.reason.Store(&reason)
	w.abort()
	w.closeConn() // unblocks a write in progress
	<-w.done
	w.mu.Lock()
	left := w.n
	for i := range w.queue {
		w.queue[i] = nil
	}
	w.n = 0
	w.mu.Unlock()
	w.drop(reason, left)
	return err
}

// Stats returns the state of the writer.
func (w *Writer) Stats() Stats {
	w.mu.Lock()
	s := Stats{Queued: w.n, Sent: w.sent.Load()}
	w.mu.Unlock()
	w.dmu.Lock()
	s.Dropped = make(map[string]uint64, len(w.dropped))
	for k, v := range w.dropped {
		s.Dropped[k] = v
	}
	w.dmu.Unlock()
	if ns := w.lastOK.Load(); ns != 0 {
		s.LastSuccess = time.Unix(0, ns)
	}
	if p := w.lastErr.Load(); p != nil {
		s.LastError = *p
	}
	return s
}
