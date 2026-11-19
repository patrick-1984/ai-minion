package traffic

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/obs/otel"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// MetricsTap returns the frame tap that counts layer L3 (application frames:
// 4-byte length and JSON body on op, ca and ev streams) of one Worker's
// connection.
func MetricsTap(m *otel.Metrics, workerID string) transport.FrameTap {
	return metricsTap{m: m, worker: workerID}
}

type metricsTap struct {
	m      *otel.Metrics
	worker string
}

func (t metricsTap) TapFrame(f transport.TapFrame) {
	if f.End {
		return
	}
	t.m.Frame(t.worker, f.Kind.String(), f.Sent, f.Size)
}

// Taps combines frame taps; nil taps are left out, and no tap at all is nil.
func Taps(taps ...transport.FrameTap) transport.FrameTap {
	var out multiTap
	for _, t := range taps {
		if t != nil {
			out = append(out, t)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	return out
}

type multiTap []transport.FrameTap

func (m multiTap) TapFrame(f transport.TapFrame) {
	for _, t := range m {
		t.TapFrame(f)
	}
}

// CountingConn wraps the connection of one Worker WebSocket and counts layer
// L1: the WebSocket message payload bytes (the sealed secure-channel records
// that carry yamux), from the upgrade to the close, the handshake included.
// WebSocket frame headers, TLS records and TCP/IP are not seen here.
type CountingConn struct {
	net.Conn
	m      *otel.Metrics
	worker atomic.Pointer[string]
	since  atomic.Int64 // unix nanoseconds of the authentication
	once   sync.Once
}

// NewCountingConn wraps c and counts the connection as opened.
func NewCountingConn(c net.Conn, m *otel.Metrics) *CountingConn {
	m.ConnOpened()
	return &CountingConn{Conn: c, m: m}
}

func (c *CountingConn) workerID() string {
	if p := c.worker.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *CountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.m.WSBytes(c.workerID(), false, n)
	return n, err
}

func (c *CountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.m.WSBytes(c.workerID(), true, n)
	return n, err
}

// Authenticated attributes the following bytes to workerID.
func (c *CountingConn) Authenticated(workerID string) {
	c.since.Store(time.Now().UnixNano())
	c.worker.Store(&workerID)
}

// Closed counts the end of the connection once.
func (c *CountingConn) Closed(handshakeOK bool) {
	c.once.Do(func() {
		var d time.Duration
		if s := c.since.Load(); s != 0 {
			d = time.Since(time.Unix(0, s))
		}
		c.m.ConnClosed(c.workerID(), handshakeOK, d)
	})
}
