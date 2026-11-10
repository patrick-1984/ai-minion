package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Role is which end of the connection a Session is.
type Role int

const (
	// RoleWorker is the yamux client: it dials the Hub and opens call and ev
	// streams.
	RoleWorker Role = iota + 1
	// RoleHub is the yamux server: it accepts the Worker and opens op
	// streams.
	RoleHub
)

func (r Role) String() string {
	switch r {
	case RoleWorker:
		return "worker"
	case RoleHub:
		return "hub"
	}
	return "unknown"
}

// Kind is the kind of a yamux stream (AD-3).
type Kind uint8

// The three stream kinds. On the wire a kind is exactly two ASCII bytes
// (A-7): the 4-byte string "call" is never written, the call kind is "ca".
const (
	// KindOp is opened by the Hub, one per request to the Worker.
	KindOp Kind = iota + 1
	// KindCall is opened by the Worker, one per upstream request.
	KindCall
	// KindEvents is the long-lived event stream, opened by the Worker.
	KindEvents
)

// KindPrefixLen is the length of the stream kind prefix.
const KindPrefixLen = 2

// Prefix returns the two wire bytes of the kind.
func (k Kind) Prefix() [KindPrefixLen]byte {
	switch k {
	case KindOp:
		return [KindPrefixLen]byte{'o', 'p'}
	case KindCall:
		return [KindPrefixLen]byte{'c', 'a'}
	case KindEvents:
		return [KindPrefixLen]byte{'e', 'v'}
	}
	return [KindPrefixLen]byte{}
}

// String returns the logical name of the kind: op, call or ev.
func (k Kind) String() string {
	switch k {
	case KindOp:
		return "op"
	case KindCall:
		return "call"
	case KindEvents:
		return "ev"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Opener returns the role that is allowed to open streams of this kind.
func (k Kind) Opener() Role {
	if k == KindOp {
		return RoleHub
	}
	return RoleWorker
}

// ParseKind decodes a two-byte stream prefix. ok is false for any prefix other
// than the three exact ones.
func ParseKind(p [KindPrefixLen]byte) (k Kind, ok bool) {
	for _, c := range []Kind{KindOp, KindCall, KindEvents} {
		if c.Prefix() == p {
			return c, true
		}
	}
	return 0, false
}

// Errors of stream handling.
var (
	// ErrBadKind is the reason for a stream whose first two bytes are not a
	// known kind prefix.
	ErrBadKind = errors.New("transport: unknown stream kind prefix")
	// ErrWrongSide is returned when a role tries to open a kind that only the
	// other role may open, and is the reason for resetting an inbound stream
	// of a kind the peer must not open.
	ErrWrongSide = errors.New("transport: stream kind not allowed for this side")
	// ErrTooManyStreams is the cause of a session that was closed because
	// the peer kept opening streams beyond the pending-stream limit.
	ErrTooManyStreams = errors.New("transport: peer opened too many streams")
	// ErrStreamReset is returned by a read of a stream that was reset or
	// released: the stream is being drained and no longer has a reader.
	ErrStreamReset = errors.New("transport: stream reset")
	// ErrSessionClosed is returned by operations on a closed Session.
	ErrSessionClosed = errors.New("transport: session closed")
)

// ReleaseBound is the longest a stream that was closed stays in the session
// before yamux has released it, whatever the peer does: the close timeout plus
// the grace of the drain. A caller that cannot wait for [Stream.Release] counts
// a closed stream as still held for this long.
const ReleaseBound = streamCloseTimeout + resetGrace

// resetGrace is added to streamCloseTimeout to bound how long a closed stream
// is drained: yamux releases a half-closed stream at the latest after
// streamCloseTimeout, so the drain normally ends by itself before this.
const resetGrace = 5 * time.Second

// Stream is one yamux stream with its kind. The kind prefix has already been
// written (opened streams) or read (accepted streams); Read and Write carry
// only the frames after it.
type Stream struct {
	ys   *yamux.Stream
	kind Kind

	// drain runs at most once per stream: Reset and Release both start it and
	// Release waits for it, so a stream never has two drain goroutines.
	drainOnce sync.Once
	drained   chan struct{}

	// rmu guards the reader count and the reset flag. A stream has one reader
	// at a time (its handler); a reset stream has none: the drain is then the
	// only reader of the yamux stream. Two readers would share yamux's one-slot
	// wake-up and the drain resets the read deadline, so one could swallow the
	// wake-up that was meant for the other (see [Stream.Reset]).
	rmu     sync.Mutex
	readers int
	reset   bool
	gone    chan struct{} // closed by the last reader to leave while the drain waits for it

	// pre is the shared-budget unit the stream holds from the moment it was
	// accepted by yamux until its host takes over; nil without a budget.
	pre     func()
	preOnce sync.Once
}

// DropReservation gives back the shared-budget unit that the stream held while
// it was accepted and classified. A host calls it once it has taken the stream
// into capacity of its own (it must take that first, so that there is no moment
// at which the stream holds nothing), and a stream released without it gives the
// unit back at the end of [Stream.Release]. It is safe to call more than once and
// does nothing without a budget.
func (s *Stream) DropReservation() {
	s.preOnce.Do(func() {
		if s.pre != nil {
			s.pre()
		}
	})
}

// Kind returns the kind of the stream.
func (s *Stream) Kind() Kind { return s.kind }

// Read reads stream bytes after the kind prefix. It fails with
// [ErrStreamReset] on a stream that was reset, and a read that waits when the
// stream is reset ends at once (with a timeout error): after Reset the drain is
// the only reader of the stream.
func (s *Stream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	if s.reset {
		s.rmu.Unlock()
		return 0, ErrStreamReset
	}
	s.readers++
	s.rmu.Unlock()

	n, err := s.ys.Read(p)

	s.rmu.Lock()
	s.readers--
	if s.readers == 0 && s.gone != nil {
		close(s.gone)
		s.gone = nil
	}
	s.rmu.Unlock()
	return n, err
}

// Write writes stream bytes.
func (s *Stream) Write(p []byte) (int, error) { return s.ys.Write(p) }

// Close closes the write side gracefully (yamux FIN); the stream is released
// when the peer closes too.
func (s *Stream) Close() error { return s.ys.Close() }

// SetDeadline sets the read and write deadlines.
func (s *Stream) SetDeadline(t time.Time) error { return s.ys.SetDeadline(t) }

// SetReadDeadline sets the read deadline.
func (s *Stream) SetReadDeadline(t time.Time) error { return s.ys.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.ys.SetWriteDeadline(t) }

// Reset abandons the stream: it is closed at once, anything the peer still
// sends is discarded, and the peer sees end of stream. yamux v0.1.2 has no
// public RST call; the session's StreamCloseTimeout forces a real RST if the
// peer never completes the close.
//
// Reset ends the read that waits on the stream, and later reads fail: a read
// that is in progress is woken by a read deadline in the past, and the drain
// reads the stream only once the last reader has left it, so the drain never
// competes with the handler for yamux's one-slot read notification nor resets
// the read deadline that wakes it.
func (s *Stream) Reset() {
	s.markReset()
	_ = s.ys.Close()
	s.startDrain()
}

// markReset forbids new reads, and wakes the reader that waits, if any.
func (s *Stream) markReset() {
	s.rmu.Lock()
	s.reset = true
	waiting := s.readers > 0
	s.rmu.Unlock()
	if waiting {
		_ = s.ys.SetReadDeadline(time.Now())
	}
}

// awaitNoReader returns once no read of the stream is in progress, waking the
// reader (again) until it has left. A woken read returns at once, so this is
// short; the repetition covers a deadline that somebody else moved.
func (s *Stream) awaitNoReader() {
	var tick *time.Timer
	for {
		s.rmu.Lock()
		if s.readers == 0 {
			s.rmu.Unlock()
			break
		}
		if s.gone == nil {
			s.gone = make(chan struct{})
		}
		gone := s.gone
		s.rmu.Unlock()
		_ = s.ys.SetReadDeadline(time.Now())
		if tick == nil {
			tick = time.NewTimer(resetWake)
		} else {
			tick.Reset(resetWake)
		}
		select {
		case <-gone:
		case <-tick.C:
		}
	}
	if tick != nil {
		tick.Stop()
	}
}

// resetWake is how often the drain wakes a reader that has not left yet.
const resetWake = 5 * time.Millisecond

// Release abandons the stream like [Stream.Reset] and returns only when yamux
// has really released it: the peer completed the close, or yamux forced it
// after StreamCloseTimeout (a session that ended releases at once). A caller
// that holds a resource slot for the stream keeps it until Release returns, so
// a peer that never completes the close cannot make the slot free early. It
// may be called after Reset or Close, and more than once.
func (s *Stream) Release() {
	s.markReset()
	_ = s.ys.Close()
	s.startDrain()
	<-s.drained
	s.DropReservation()
}

func (s *Stream) startDrain() {
	s.drainOnce.Do(func() {
		s.drained = make(chan struct{})
		go func() {
			defer close(s.drained)
			s.awaitNoReader()
			drainStream(s.ys)
		}()
	})
}

func resetStream(ys *yamux.Stream) {
	_ = ys.Close()
	go drainStream(ys)
}

// drainStream discards what the peer still sends on a closed stream until the
// stream is gone: the peer completed the close, or yamux forced it after
// streamCloseTimeout. It returns only then, so a caller that holds a resource
// slot while draining keeps the number of lingering streams bounded.
func drainStream(ys *yamux.Stream) {
	end := time.Now().Add(streamCloseTimeout + resetGrace)
	for {
		_ = ys.SetReadDeadline(end)
		_, err := io.Copy(io.Discard, ys)
		// A handler's late interrupt of an idle read (a read deadline in the
		// past) must not end the drain before the stream is really gone.
		var ne net.Error
		if err == nil || !errors.As(err, &ne) || !ne.Timeout() || !time.Now().Before(end) {
			return
		}
	}
}

// WriteFrame writes v as one frame: a uint32 big-endian length (at most
// proto.StreamFrameLimit) and a UTF-8 JSON object with a type field. A frame
// that is too large or malformed is refused before anything is written.
func (s *Stream) WriteFrame(v any) error {
	return proto.WriteFrame(s.ys, proto.StreamFrameLimit, v)
}

// ReadFrame reads one frame body. A frame announcing more than
// proto.StreamFrameLimit bytes, or whose body is not a typed JSON object,
// resets the stream and returns the *proto.Error (frame_too_large or
// invalid_argument). io.EOF means the peer closed the stream cleanly between
// frames.
func (s *Stream) ReadFrame() (json.RawMessage, error) {
	body, err := proto.ReadFrame(streamReader{s}, proto.StreamFrameLimit)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			s.Reset()
		}
		return nil, err
	}
	return body, nil
}

// readKind reads and validates the two-byte prefix of an inbound stream.
func readKind(ys *yamux.Stream, peer Role) (Kind, error) {
	var p [KindPrefixLen]byte
	if _, err := io.ReadFull(ys, p[:]); err != nil {
		return 0, err
	}
	k, ok := ParseKind(p)
	if !ok {
		return 0, fmt.Errorf("%w: %#x", ErrBadKind, p[:])
	}
	if k.Opener() != peer {
		return 0, fmt.Errorf("%w: %s stream opened by the %s", ErrWrongSide, k, peer)
	}
	return k, nil
}

// streamReader reads through [Stream.Read], so that a frame read takes part in
// the single-reader rule.
type streamReader struct{ s *Stream }

func (r streamReader) Read(p []byte) (int, error) { return r.s.Read(p) }
