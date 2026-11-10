// Package stallh2 is a raw TLS/HTTP/2 client for tests of streaming handlers
// that must survive clients that stop reading (Story 5.4).
//
// The client advertises a very large flow-control window, so the server is
// never held back by HTTP/2 credit: once the client stops reading its TCP
// socket (small receive buffer), the server's socket write blocks, which is the
// state that stream-level deadlines cannot release in net/http.
//
// Test helper only; it is not linked into any binary.
package stallh2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// Client is one HTTP/2 connection.
type Client struct {
	t   *testing.T
	tcp net.Conn
	tls *tls.Conn
	fr  *http2.Framer
}

// Dial connects to addr ("host:port") with TLS, certificate checks off,
// protocol h2, and sends the connection preface with a 1 GiB window.
func Dial(t *testing.T, addr string) *Client {
	t.Helper()
	tcp, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tc, ok := tcp.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	c := &Client{t: t, tcp: tcp, tls: tls.Client(tcp, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})} //nolint:gosec // test client of a test server
	t.Cleanup(func() { _ = c.tls.Close() })
	_ = c.tls.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.tls.Handshake(); err != nil {
		t.Fatal(err)
	}
	if got := c.tls.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("negotiated %q, want h2", got)
	}
	c.fr = http2.NewFramer(c.tls, c.tls)
	if _, err := io.WriteString(c.tls, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	if err := c.fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := c.fr.WriteWindowUpdate(0, 1<<30-65535); err != nil {
		t.Fatal(err)
	}
	_ = c.tls.SetDeadline(time.Time{})
	return c
}

// Get sends a GET on the given odd stream id. hdr adds request headers.
func (c *Client) Get(streamID uint32, path string, hdr map[string]string) {
	c.t.Helper()
	var b bytes.Buffer
	e := hpack.NewEncoder(&b)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "hub.test"}, {Name: ":path", Value: path},
	}
	for k, v := range hdr {
		fields = append(fields, hpack.HeaderField{Name: strings.ToLower(k), Value: v})
	}
	for _, f := range fields {
		if err := e.WriteField(f); err != nil {
			c.t.Fatal(err)
		}
	}
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: streamID, BlockFragment: b.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		c.t.Fatal(err)
	}
}

// PostPartial opens a request with a body on the given odd stream id and sends
// only the first part of it: the headers announce contentLength bytes (when it
// is positive), the DATA frame holds part, and the stream is not ended. It is
// the unauthenticated slow client of a body deadline.
func (c *Client) PostPartial(streamID uint32, path string, hdr map[string]string, contentLength int, part []byte) {
	c.t.Helper()
	var b bytes.Buffer
	e := hpack.NewEncoder(&b)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "hub.test"}, {Name: ":path", Value: path},
	}
	if contentLength > 0 {
		fields = append(fields, hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(contentLength)})
	}
	for k, v := range hdr {
		fields = append(fields, hpack.HeaderField{Name: strings.ToLower(k), Value: v})
	}
	for _, f := range fields {
		if err := e.WriteField(f); err != nil {
			c.t.Fatal(err)
		}
	}
	if err := c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: streamID, BlockFragment: b.Bytes(), EndHeaders: true}); err != nil {
		c.t.Fatal(err)
	}
	if len(part) > 0 {
		if err := c.fr.WriteData(streamID, false, part); err != nil {
			c.t.Fatal(err)
		}
	}
}

// SendData sends more body on the stream and ends it when end is true.
func (c *Client) SendData(streamID uint32, end bool, data []byte) {
	c.t.Helper()
	if err := c.fr.WriteData(streamID, end, data); err != nil {
		c.t.Fatal(err)
	}
}

// WaitStreamEnd reads frames (acknowledging settings and pings) until the
// server ends the stream (END_STREAM on a frame of it, or RST_STREAM) or the
// connection, within d, and reports whether it did, with the status code of
// the response headers when they came (0 when none did).
func (c *Client) WaitStreamEnd(streamID uint32, d time.Duration) (ended bool, status int) {
	c.t.Helper()
	deadline := time.Now().Add(d)
	defer func() { _ = c.tls.SetReadDeadline(time.Time{}) }()
	dec := hpack.NewDecoder(4096, nil)
	for {
		_ = c.tls.SetReadDeadline(deadline)
		f, err := c.fr.ReadFrame()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return false, status
			}
			return true, status // the server closed the connection
		}
		switch fr := f.(type) {
		case *http2.SettingsFrame:
			if !fr.IsAck() {
				_ = c.fr.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !fr.IsAck() {
				_ = c.fr.WritePing(true, fr.Data)
			}
		case *http2.GoAwayFrame:
			return true, status
		case *http2.RSTStreamFrame:
			if fr.StreamID == streamID {
				return true, status
			}
		case *http2.HeadersFrame:
			if fr.StreamID == streamID {
				if fields, err := dec.DecodeFull(fr.HeaderBlockFragment()); err == nil {
					for _, hf := range fields {
						if hf.Name == ":status" {
							status, _ = strconv.Atoi(hf.Value)
						}
					}
				}
				if fr.StreamEnded() {
					return true, status
				}
			}
		case *http2.DataFrame:
			if fr.StreamID == streamID && fr.StreamEnded() {
				return true, status
			}
		}
	}
}

// ReadHeaders reads frames (acknowledging the server's settings, discarding
// data) until the response headers of streamID arrive, within d. After it
// returns, the client simply stops reading.
func (c *Client) ReadHeaders(streamID uint32, d time.Duration) {
	c.t.Helper()
	_ = c.tls.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = c.tls.SetReadDeadline(time.Time{}) }()
	for {
		f, err := c.fr.ReadFrame()
		if err != nil {
			c.t.Fatalf("no response headers on stream %d: %v", streamID, err)
		}
		if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
			_ = c.fr.WriteSettingsAck()
		}
		if f.Header().Type == http2.FrameHeaders && f.Header().StreamID == streamID {
			return
		}
	}
}

// Drain reads and discards frames until pred accepts the data of a frame or d
// passes; it reports whether pred accepted. It is the healthy-client side.
func (c *Client) Drain(d time.Duration, pred func(data []byte) bool) bool {
	c.t.Helper()
	deadline := time.Now().Add(d)
	defer func() { _ = c.tls.SetReadDeadline(time.Time{}) }()
	for {
		_ = c.tls.SetReadDeadline(deadline)
		f, err := c.fr.ReadFrame()
		if err != nil {
			return false
		}
		switch fr := f.(type) {
		case *http2.SettingsFrame:
			if !fr.IsAck() {
				_ = c.fr.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !fr.IsAck() {
				_ = c.fr.WritePing(true, fr.Data)
			}
		case *http2.DataFrame:
			if pred != nil && pred(fr.Data()) {
				return true
			}
		}
	}
}

// Close closes the connection.
func (c *Client) Close() { _ = c.tls.Close() }
