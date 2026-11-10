package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSE writes a Server-Sent Events stream (AD-18: live updates are SSE). It is
// the helper the live pages and the REST event endpoint share, so that the
// framing, the headers and the injection rules exist once.
//
// An SSE is used by one goroutine at a time.
type SSE struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	// timeout bounds one write when positive; zero leaves writes unbounded
	// (the stream then relies on the client closing the connection).
	timeout time.Duration
	// mu orders the write deadlines: write arms one before each write, and
	// Interrupt, called from another goroutine, shortens it to cut a write
	// that is blocked on a client that does not read.
	mu          sync.Mutex
	interrupted bool
	done        bool
}

// sseInterruptGrace is how long a write in flight may still take after
// [SSE.Interrupt]: a write that is merely in progress completes, a write
// blocked on a client that does not read is cut.
const sseInterruptGrace = time.Second

var errSSEInterrupted = errors.New("web: the event stream was interrupted")

// Message is one SSE event.
type Message struct {
	// ID becomes the "id:" field; the browser sends it back as Last-Event-ID.
	ID string
	// Event becomes the "event:" field; empty sends a default "message" event.
	Event string
	// Data becomes one "data:" line per line of the text.
	Data string
}

// ErrSSEUnsupported is returned by [NewSSE] when the connection cannot be
// flushed, so that events would not reach the browser.
var ErrSSEUnsupported = errors.New("web: the response writer cannot stream")

// NewSSE starts an event stream on w: it sets the stream headers, sends the
// response header and flushes it. The caller owns the request context and
// stops when it is done or when a write fails.
func NewSSE(w http.ResponseWriter) (*SSE, error) {
	s := &SSE{w: w, rc: http.NewResponseController(w)}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // proxies must not buffer the stream
	h.Set("X-Content-Type-Options", "nosniff")
	// A stream outlives any server-wide write deadline. Not every writer
	// supports deadlines; that is fine.
	if err := s.rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, fmt.Errorf("web: clear the write deadline: %w", err)
	}
	w.WriteHeader(http.StatusOK)
	if err := s.rc.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			return nil, ErrSSEUnsupported
		}
		return nil, err
	}
	return s, nil
}

// Send writes one event and flushes it. An ID or event name with a line break
// is refused: it would let a value forge further fields.
func (s *SSE) Send(m Message) error {
	if strings.ContainsAny(m.ID, "\r\n\x00") || strings.ContainsAny(m.Event, "\r\n") {
		return errors.New("web: an SSE id or event name must not contain a line break")
	}
	var b strings.Builder
	if m.ID != "" {
		b.WriteString("id: " + m.ID + "\n")
	}
	if m.Event != "" {
		b.WriteString("event: " + m.Event + "\n")
	}
	data := strings.ReplaceAll(strings.ReplaceAll(m.Data, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return s.write(b.String())
}

// Comment writes a comment line, which browsers ignore. Use it as a keep-alive
// so that proxies do not close an idle stream.
func (s *SSE) Comment(text string) error {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	return s.write(": " + text + "\n\n")
}

// Retry tells the browser how long to wait before it reconnects.
func (s *SSE) Retry(d time.Duration) error {
	return s.write(fmt.Sprintf("retry: %d\n\n", d.Milliseconds()))
}

// SetWriteTimeout bounds every later write to d: a client that stops reading
// then fails the write instead of holding the connection for ever. Call it
// before the first write that can block, from the goroutine that writes.
func (s *SSE) SetWriteTimeout(d time.Duration) { s.timeout = d }

// Interrupt ends a write that is blocked on a client that does not read
// within [sseInterruptGrace] and makes every later write fail. It may be
// called from any goroutine, for example when the feed behind the stream has
// ended: the writing goroutine cannot see that while it is blocked.
func (s *SSE) Interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.interrupted {
		return
	}
	s.interrupted = true
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseInterruptGrace))
}

// Finish clears the write deadline, which would fail the next request that
// uses the connection, and keeps a late Interrupt from setting one again.
func (s *SSE) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	_ = s.rc.SetWriteDeadline(time.Time{})
}

func (s *SSE) write(text string) error {
	s.mu.Lock()
	if s.done || s.interrupted {
		s.mu.Unlock()
		return errSSEInterrupted
	}
	if s.timeout > 0 {
		// A writer without deadlines (a recorder) cannot block.
		_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	}
	s.mu.Unlock()
	if _, err := io.WriteString(s.w, text); err != nil {
		return err
	}
	return s.rc.Flush()
}
