// Package httpbody bounds the time a client may take to send a request body.
//
// The Hub's http.Server has a header timeout and an idle timeout, and
// deliberately no read timeout: the Worker's WebSocket, the event streams and
// the dashboard are long-lived, so a deadline on the whole connection would
// kill them. A request body is the one thing that needs a deadline of its own.
// Without it a client that sends complete headers and part of a body and then
// stops holds its connection, and the goroutine of whatever handler is reading
// the body (the login, a form, the API) or, for a route that never reads it,
// net/http's own discard of the unread body, until it closes the socket. The
// client needs no credentials for that, and net/http reads the rest of an
// unread body before it answers, so every route is affected, not only the ones
// that parse a body.
//
// [Guard] closes the gap for every route at once: it runs before routing, puts
// a read deadline on the request when (and only when) the request has a body,
// and lifts it as soon as the body has been read to its end. A request without
// a body (a WebSocket upgrade, an event stream, a page) is never touched, so
// nothing long-lived gets a deadline. [Arm] is the same for a handler that
// wants to be safe on its own, whatever it is mounted behind.
//
// The deadline is lifted only when the body has really been read to its end
// (the reader returned io.EOF). There is no way to lift it by hand: a handler
// that has parsed one JSON value, or one form, has not seen the end of a body
// that was declared longer, and net/http reads the rest after the handler
// returns, so the deadline must stay in force until it is read. A handler that
// wants to go on to long work after its request calls [Drain] first.
//
// The deadline is on the connection for HTTP/1.1 and on the stream for HTTP/2
// (net/http's http.ResponseController does both), so one stalled stream does
// not end the other streams of its connection. A connection that is taken
// over (a WebSocket) has its deadlines cleared by net/http.
package httpbody

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeout is how long a client may take to send a request body. A body
// is at most a few hundred kilobytes (a login, a form, a 1 MiB API call), so
// this is generous for any client that is sending.
const DefaultTimeout = 30 * time.Second

// armedBody is the body of a request whose deadline is armed: reading it to
// the end lifts the deadline.
type armedBody struct {
	io.ReadCloser
	lift func()
}

func (b *armedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.lift()
	}
	return n, err
}

// HasBody reports whether r announces a body.
func HasBody(r *http.Request) bool { return r.Body != nil && r.Body != http.NoBody }

// Arm puts a read deadline of timeout (zero means [DefaultTimeout]) on a
// request that has a body, before the handler reads any of it, and lifts it
// when the body has been read to its end (never earlier); r.Body is replaced to
// do so. When
// the body is not read completely the deadline stays in force while net/http
// discards the rest after the handler, and it is cleared with the request
// (HTTP/1.1: net/http clears it when it reads the next request; HTTP/2: it is
// the stream's). It reports whether a deadline was armed; a request that is
// armed already (by [Guard]) is left as it is, and a writer that cannot set a
// deadline (a recorder) has no connection to stall.
func Arm(w http.ResponseWriter, r *http.Request, timeout time.Duration) bool {
	if !HasBody(r) {
		return false
	}
	if _, ok := r.Body.(*armedBody); ok {
		return true
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return false
	}
	var once sync.Once
	r.Body = &armedBody{ReadCloser: r.Body, lift: func() {
		once.Do(func() { _ = rc.SetReadDeadline(time.Time{}) })
	}}
	return true
}

// Drain reads the rest of the body to its end, so that the deadline of [Arm]
// is lifted by a body that really is complete and only by it. It returns the
// read error: a body that stops, is cut by the deadline or exceeds a limit
// that the reader enforces is not complete. A handler calls it after it has
// parsed what it needs, before it goes on to work that must not be cut.
func Drain(r *http.Request) error {
	if !HasBody(r) {
		return nil
	}
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

// Guard returns a middleware that arms every request that has a body, before
// the handler (and the mux, which answers some requests itself) runs. A
// timeout of zero means [DefaultTimeout].
func Guard(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Arm(w, r, timeout)
			next.ServeHTTP(w, r)
		})
	}
}
