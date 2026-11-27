package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Data legs as net.Conn (BRD 5.2, files.v1).
//
// A data leg is one stream of kind fx that carries exactly one HTTP/1.1
// request and its response (RFC 9112 framing: Content-Length or chunked with
// trailers) and is closed after it. The opener is the HTTP client
// ([NewLegTransport]); the acceptor serves it with [ServeLeg], an ordinary
// net/http server on a one-connection in-memory listener. No socket is opened
// or bound.
//
// Every write of a leg is cut into frames of at most proto.LegChunkBytes,
// passes the session's bulk pacer (pacer.go) and yields, so op, call and
// event frames interleave with file content on the shared send path.

// LegHost is the host name of a data-leg request URL; nothing resolves it.
const LegHost = "minion-fx"

// LegURL returns the request URL of a data leg for path (normally
// proto.FilesLegPath).
func LegURL(path string) string { return "http://" + LegHost + path }

// errLegUsed is returned by a second dial of a leg transport: one leg carries
// one request.
var errLegUsed = errors.New("transport: data leg already used")

// LegConn is a data leg as a net.Conn. Close abandons the stream
// ([Stream.Reset]): the peer sees the end of the stream, which HTTP framing
// reports as a truncated message unless the exchange was complete.
//
// The end of the stream from the peer also ends the writes of this side
// ([ErrLegPeerGone]): a leg is never half-closed while its exchange is still
// running (the Go HTTP client and server do not half-close a net.Conn without
// CloseWrite), so the peer closing its side means the exchange is over or
// abandoned. That is how the sender of a body stops within moments of the
// receiver's cancellation (NFR-7): the server notices it through the
// background read of net/http, the client through its body write.
type LegConn struct {
	st *Stream

	wmu    sync.Mutex // one writer at a time; keeps chunks of a Write together
	dlMu   sync.Mutex
	wdl    time.Time // write deadline, for the pacer
	closed chan struct{}
	once   sync.Once
	// stop is closed by Close or by the peer's end of stream: it ends the
	// writes, including one that waits in the pacer.
	stop     chan struct{}
	stopOnce sync.Once

	// onClose, when set, runs once after Close (the one-shot listener).
	hookMu  sync.Mutex
	onClose func()
}

// NewLegConn wraps st as a net.Conn. st is normally an fx stream (from
// [Session.OpenStream] or [Session.Accept]); a stream of another kind works,
// but is not paced.
func NewLegConn(st *Stream) *LegConn {
	return &LegConn{st: st, closed: make(chan struct{}), stop: make(chan struct{})}
}

// ErrLegPeerGone is returned by a write of a leg whose peer has closed its
// side of the stream: the exchange is over or abandoned.
var ErrLegPeerGone = errors.New("transport: data leg closed by the peer")

func (c *LegConn) halt() { c.stopOnce.Do(func() { close(c.stop) }) }

// Stream returns the underlying stream.
func (c *LegConn) Stream() *Stream { return c.st }

// Read reads from the stream.
func (c *LegConn) Read(p []byte) (int, error) {
	n, err := c.st.Read(p)
	if errors.Is(err, io.EOF) {
		c.halt()
	}
	if err != nil && errors.Is(err, ErrStreamReset) {
		select {
		case <-c.closed:
			return n, net.ErrClosed
		default:
		}
	}
	return n, err
}

// Write writes p in chunks of at most proto.LegChunkBytes, each one through
// the session's bulk pacer and followed by a yield.
func (c *LegConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		if err := c.stopped(); err != nil {
			return total, err
		}
		n := min(len(p), proto.LegChunkBytes)
		if err := c.st.pacer.wait(n, c.writeDeadline(), c.stop); err != nil {
			if errors.Is(err, errPacerClosed) {
				if err = c.stopped(); err == nil {
					err = net.ErrClosed // the session ended
				}
			}
			return total, err
		}
		w, err := c.writeChunk(p[:n])
		total += w
		c.st.pacer.wrote(w)
		if err != nil {
			return total, err
		}
		p = p[n:]
		runtime.Gosched()
	}
	return total, nil
}

// stopped returns why writes have ended, or nil.
func (c *LegConn) stopped() error {
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	select {
	case <-c.stop:
		return ErrLegPeerGone
	default:
	}
	return nil
}

// writeChunk writes one chunk completely (yamux may write less than asked when
// the send window is smaller).
func (c *LegConn) writeChunk(b []byte) (int, error) {
	done := 0
	for done < len(b) {
		n, err := c.st.Write(b[done:])
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (c *LegConn) writeDeadline() time.Time {
	c.dlMu.Lock()
	defer c.dlMu.Unlock()
	return c.wdl
}

// Close abandons the leg: the stream is reset and its leg slot given back. It
// is safe to call more than once.
func (c *LegConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.halt()
		c.st.Reset()
		c.hookMu.Lock()
		f := c.onClose
		c.hookMu.Unlock()
		if f != nil {
			f()
		}
	})
	return nil
}

// Done is closed when the leg was closed.
func (c *LegConn) Done() <-chan struct{} { return c.closed }

func (c *LegConn) setOnClose(f func()) {
	c.hookMu.Lock()
	c.onClose = f
	c.hookMu.Unlock()
	select {
	case <-c.closed:
		f()
	default:
	}
}

// LocalAddr returns a placeholder address naming the stream.
func (c *LegConn) LocalAddr() net.Addr { return legAddr(c.st.ID()) }

// RemoteAddr returns a placeholder address naming the stream.
func (c *LegConn) RemoteAddr() net.Addr { return legAddr(c.st.ID()) }

// SetDeadline sets the read and write deadlines.
func (c *LegConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.st.SetDeadline(t)
}

// SetReadDeadline sets the read deadline.
func (c *LegConn) SetReadDeadline(t time.Time) error { return c.st.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline; it also bounds a wait in the
// pacer.
func (c *LegConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.st.SetWriteDeadline(t)
}

func (c *LegConn) setWriteDeadline(t time.Time) {
	c.dlMu.Lock()
	c.wdl = t
	c.dlMu.Unlock()
}

// legAddr is the address of a data leg: the network "fx" and the stream id.
type legAddr uint32

func (a legAddr) Network() string { return "fx" }
func (a legAddr) String() string  { return "fx:" + strconv.FormatUint(uint64(a), 10) }

// oneShotListener hands out one connection and then waits until that
// connection is closed (or the listener is), so that an http.Server serves
// exactly the one leg and its Serve returns when the leg ends.
type oneShotListener struct {
	ch   chan net.Conn
	addr net.Addr
	done chan struct{}
	once sync.Once
}

// NewLegListener returns a net.Listener whose Accept returns c once; the next
// Accept waits until c or the listener is closed and then fails with
// net.ErrClosed.
func NewLegListener(c *LegConn) net.Listener {
	l := &oneShotListener{ch: make(chan net.Conn, 1), addr: c.LocalAddr(), done: make(chan struct{})}
	l.ch <- c
	c.setOnClose(func() { _ = l.Close() })
	return l
}

func (l *oneShotListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	default:
	}
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneShotListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneShotListener) Addr() net.Addr { return l.addr }

// NewLegServer returns the HTTP server of one data leg (BRD 5.2): header
// limit proto.MaxLegHeaderBytes, header timeout proto.LegReadHeaderTimeout,
// keep-alives off (one request per leg; a second, pipelined request is never
// read), no HTTP/2 and a discarded error log (a leg logs no paths or
// tokens). The caller may set further fields (ReadTimeout, BaseContext)
// before [ServeLeg].
func NewLegServer(h http.Handler) *http.Server {
	srv := &http.Server{
		Handler:           h,
		MaxHeaderBytes:    proto.MaxLegHeaderBytes,
		ReadHeaderTimeout: proto.LegReadHeaderTimeout,
		IdleTimeout:       proto.LegReadHeaderTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
	}
	srv.SetKeepAlivesEnabled(false)
	return srv
}

// ServeLeg serves the one HTTP exchange of the data leg c with srv (normally
// from [NewLegServer]; srv must serve nothing else). It returns when the leg
// has ended and the handler has returned. ctx ending abandons the leg (the
// handler's request context ends too). A leg that ends normally returns nil.
func ServeLeg(ctx context.Context, srv *http.Server, c *LegConn) error {
	var (
		mu      sync.Mutex
		started bool
		ended   = make(chan struct{})
	)
	prev := srv.ConnState
	srv.ConnState = func(nc net.Conn, s http.ConnState) {
		if prev != nil {
			prev(nc, s)
		}
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew:
			started = true
		case http.StateClosed, http.StateHijacked:
			select {
			case <-ended:
			default:
				close(ended)
			}
		}
	}
	if srv.BaseContext == nil {
		srv.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	err := srv.Serve(NewLegListener(c))
	mu.Lock()
	wait := started
	mu.Unlock()
	if wait {
		<-ended
	}
	_ = c.Close()
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

// NewLegTransport returns an HTTP client transport whose one connection is
// the data leg c (BRD 5.2): keep-alives and transparent compression off
// (encodings are explicit), response header limit proto.MaxLegHeaderBytes,
// no proxy and no HTTP/2. A second dial fails: one leg carries one request.
// The request URL is [LegURL]; canceling the request's context closes the
// leg, which resets the stream.
func NewLegTransport(c *LegConn) *http.Transport {
	var (
		mu   sync.Mutex
		used bool
	)
	return &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if used {
				return nil, errLegUsed
			}
			used = true
			return c, nil
		},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxIdleConns:           1,
		MaxResponseHeaderBytes: proto.MaxLegHeaderBytes,
		WriteBufferSize:        proto.LegChunkBytes,
		ReadBufferSize:         32 << 10,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}
