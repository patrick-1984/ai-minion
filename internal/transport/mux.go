package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// Session limits.
const (
	// DefaultKindTimeout bounds how long an inbound stream may stay silent
	// before its two-byte kind prefix arrives.
	DefaultKindTimeout = 10 * time.Second

	// maxPendingStreams bounds inbound streams whose kind is being read or
	// that wait for Accept; beyond it new streams are reset.
	maxPendingStreams = 256

	// maxRejectedStreams bounds streams rejected outside the pending limit.
	maxRejectedStreams = 64

	streamOpenTimeout  = 30 * time.Second
	streamCloseTimeout = 30 * time.Second
	writeTimeout       = 10 * time.Second
)

// StreamBudget is a budget that several sessions share (the Hub's global and
// per-Worker stream capacity). The session consults it for every inbound stream
// before it reads a single byte of it, so a stream that never sends its kind
// prefix, and a rejected stream that is still draining, hold capacity from the
// moment yamux accepts them, not from the moment they are classified. The
// transport knows nothing about Workers or Hubs; the owner of the budget keys it.
type StreamBudget interface {
	// Reserve takes one unit for a new inbound stream. release gives it back and
	// is called once, when the stream is gone or handed over (see
	// [Stream.DropReservation]); ok is false when the budget has no room.
	Reserve() (release func(), ok bool)
	// Refuse accounts for a stream that is rejected, because Reserve said no or
	// because its prefix is invalid: the stream is closed and drained, and the
	// unit is held until yamux has released it. ok is false when the session
	// already has too many rejected streams: it is abusive and is closed.
	Refuse() (release func(), ok bool)
}

// SessionOptions tunes a Session. The zero value is the production setup.
type SessionOptions struct {
	// Budget, when set, is consulted for every inbound stream before its kind is
	// read. It must be set before the session is created: the accept loop starts
	// at once, and there is no other.
	Budget StreamBudget
	// Keepalive sets the ping interval, the death threshold and the clock.
	Keepalive KeepaliveConfig
	// KindTimeout bounds the wait for the kind prefix of an inbound
	// stream; zero means DefaultKindTimeout.
	KindTimeout time.Duration
}

// Session is the yamux multiplexer on top of an authenticated connection
// (normally a Channel). The Worker side is the yamux client and the Hub side
// the yamux server. Streams carry a two-byte kind prefix; Accept delivers only
// inbound streams whose kind the peer is allowed to open, and resets all
// others. The Session runs the keepalive monitor and closes itself when the
// peer is silent for DeadAfter.
type Session struct {
	ys   *yamux.Session
	conn *activityConn
	role Role
	opts SessionOptions

	accepted chan *Stream
	done     chan struct{}
	once     sync.Once
	err      error // cause of the close; valid after done is closed

	// held are the budget units that streams of this session still hold, so that
	// a session that ends gives every one of them back.
	heldMu  sync.Mutex
	held    map[uint64]func()
	heldSeq uint64
	ended   bool
}

// NewSession starts yamux over conn as the given role and starts the accept
// and keepalive loops. conn is closed when the Session closes.
func NewSession(conn net.Conn, role Role, opts SessionOptions) (*Session, error) {
	if opts.Keepalive.Clock == nil {
		opts.Keepalive.Clock = SystemClock
	}
	if opts.KindTimeout <= 0 {
		opts.KindTimeout = DefaultKindTimeout
	}
	ac := &activityConn{Conn: conn, clk: opts.Keepalive.Clock}
	ac.touch()

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false // the keepalive monitor below replaces it
	cfg.LogOutput = io.Discard
	cfg.StreamOpenTimeout = streamOpenTimeout
	cfg.StreamCloseTimeout = streamCloseTimeout
	cfg.ConnectionWriteTimeout = writeTimeout

	var (
		ys  *yamux.Session
		err error
	)
	switch role {
	case RoleWorker:
		ys, err = yamux.Client(ac, cfg)
	case RoleHub:
		ys, err = yamux.Server(ac, cfg)
	default:
		err = ErrWrongSide
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	s := &Session{
		ys:       ys,
		conn:     ac,
		role:     role,
		opts:     opts,
		accepted: make(chan *Stream, maxPendingStreams),
		done:     make(chan struct{}),
		held:     map[uint64]func(){},
	}
	go s.acceptLoop()
	go s.watch()
	return s, nil
}

// Role returns the role of this end.
func (s *Session) Role() Role { return s.role }

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended (for example ErrKeepaliveTimeout or the
// error of the connection), or nil while it is running. After a local Close it
// is ErrSessionClosed.
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// NumStreams returns the number of open yamux streams.
func (s *Session) NumStreams() int { return s.ys.NumStreams() }

// Ping sends a yamux ping and returns the round trip time.
func (s *Session) Ping() (time.Duration, error) { return s.ys.Ping() }

// Close ends the session and the underlying connection.
func (s *Session) Close() error {
	s.shutdown(ErrSessionClosed)
	return nil
}

func (s *Session) shutdown(cause error) {
	s.once.Do(func() {
		s.err = cause
		close(s.done)
		_ = s.ys.Close()
		_ = s.conn.Close()
		s.heldMu.Lock()
		s.ended = true
		held := s.held
		s.held = nil
		s.heldMu.Unlock()
		for _, release := range held { // the streams are gone with the session
			release()
		}
	})
}

// OpenStream opens a stream of the given kind and writes its two-byte prefix
// first. A role may open only its own kinds: op for the Hub, call and ev for
// the Worker; anything else is ErrWrongSide and nothing is opened.
func (s *Session) OpenStream(ctx context.Context, kind Kind) (*Stream, error) {
	if _, ok := ParseKind(kind.Prefix()); !ok || kind.Opener() != s.role {
		return nil, ErrWrongSide
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-s.done:
		return nil, s.err
	default:
	}
	// yamux blocks in OpenStream (SYN backlog) and in the write of the prefix
	// (a stalled connection) with only its own timeouts, so the open runs in
	// its own goroutine and ctx ends the wait. A stream that is obtained
	// after ctx ended is closed again.
	type result struct {
		st  *Stream
		err error
	}
	ch := make(chan result, 1)
	go func() {
		st, err := s.openStream(ctx, kind)
		ch <- result{st, err}
	}()
	select {
	case r := <-ch:
		return r.st, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.st != nil {
				r.st.Reset()
			}
		}()
		return nil, ctx.Err()
	}
}

func (s *Session) openStream(ctx context.Context, kind Kind) (*Stream, error) {
	ys, err := s.ys.OpenStream()
	if err != nil {
		return nil, s.wrapErr(err)
	}
	// ctx ending interrupts a blocked write of the prefix.
	stop := context.AfterFunc(ctx, func() { _ = ys.SetWriteDeadline(time.Now()) })
	p := kind.Prefix()
	_, err = ys.Write(p[:])
	if !stop() {
		// ctx ended and the write deadline may have been set.
		resetStream(ys)
		return nil, ctx.Err()
	}
	if err != nil {
		resetStream(ys)
		return nil, s.wrapErr(err)
	}
	return &Stream{ys: ys, kind: kind}, nil
}

// OpenRaw opens a stream and writes nothing to it, not even the kind prefix. No
// Worker library does this; it exists so that the receiving side can be tested
// against a peer that withholds or garbles the prefix (the bytes written to the
// returned stream are the prefix the receiver reads). The stream has no kind.
func (s *Session) OpenRaw(ctx context.Context) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-s.done:
		return nil, s.err
	default:
	}
	ys, err := s.ys.OpenStream()
	if err != nil {
		return nil, s.wrapErr(err)
	}
	return &Stream{ys: ys}, nil
}

// OpenOp opens an op stream (Hub only).
func (s *Session) OpenOp(ctx context.Context) (*Stream, error) { return s.OpenStream(ctx, KindOp) }

// OpenCall opens a call stream (Worker only).
func (s *Session) OpenCall(ctx context.Context) (*Stream, error) {
	return s.OpenStream(ctx, KindCall)
}

// OpenEvents opens the event stream (Worker only).
func (s *Session) OpenEvents(ctx context.Context) (*Stream, error) {
	return s.OpenStream(ctx, KindEvents)
}

// Accept returns the next inbound stream whose kind prefix was valid and
// allowed for the peer. It returns ctx.Err() when ctx ends and the session's
// cause when the session ends.
func (s *Session) Accept(ctx context.Context) (*Stream, error) {
	select {
	case st := <-s.accepted:
		return st, nil
	case <-s.done:
		return nil, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) wrapErr(err error) error {
	select {
	case <-s.done:
		return s.err
	default:
		return err
	}
}

// hold registers a budget unit of this session and returns its release, which is
// idempotent and is also run when the session ends.
func (s *Session) hold(release func()) func() {
	var once sync.Once
	s.heldMu.Lock()
	if s.ended {
		s.heldMu.Unlock()
		release()
		return func() {}
	}
	s.heldSeq++
	id := s.heldSeq
	s.held[id] = release
	s.heldMu.Unlock()
	return func() {
		once.Do(func() {
			s.heldMu.Lock()
			delete(s.held, id)
			s.heldMu.Unlock()
			release()
		})
	}
}

// acceptLoop takes yamux streams and classifies each one in its own goroutine,
// so a peer that opens a stream and stays silent cannot block the others. With
// a [StreamBudget] every stream is admitted to it first, before anything is read
// from it.
func (s *Session) acceptLoop() {
	sem := make(chan struct{}, maxPendingStreams)
	rej := make(chan struct{}, maxRejectedStreams)
	// rejectBounded rejects ys on a goroutine of its own, but only a bounded
	// number at a time: a peer that keeps opening streams while the limits are
	// exceeded is abusing the session. done runs when yamux has released ys.
	rejectBounded := func(ys *yamux.Stream, done func()) bool {
		select {
		case rej <- struct{}{}:
			go func() {
				defer func() { <-rej }()
				defer done()
				s.reject(ys)
			}()
			return true
		default:
			done()
			s.shutdown(ErrTooManyStreams)
			return false
		}
	}
	for {
		ys, err := s.ys.AcceptStream()
		if err != nil {
			s.shutdown(err)
			return
		}
		var release func()
		if b := s.opts.Budget; b != nil {
			r, ok := b.Reserve()
			if !ok {
				fr, ok := b.Refuse()
				if !ok {
					resetStream(ys)
					s.shutdown(ErrTooManyStreams)
					return
				}
				if !rejectBounded(ys, s.hold(fr)) {
					return
				}
				continue
			}
			release = s.hold(r)
		}
		select {
		case sem <- struct{}{}:
		default:
			// Too many streams are waiting: reject this one.
			if release == nil {
				release = func() {}
			}
			if !rejectBounded(ys, release) {
				return
			}
			continue
		}
		go func() {
			defer func() { <-sem }()
			s.classify(ys, release)
		}()
	}
}

// classify reads the kind of an inbound stream. release (nil without a budget)
// is the budget unit the stream holds: it travels with the accepted stream until
// the host takes over ([Stream.DropReservation]), and is given back at once when
// the stream is rejected, after yamux has released it.
func (s *Session) classify(ys *yamux.Stream, release func()) {
	_ = ys.SetReadDeadline(time.Now().Add(s.opts.KindTimeout))
	peer := RoleHub
	if s.role == RoleHub {
		peer = RoleWorker
	}
	kind, err := readKind(ys, peer)
	if err != nil {
		s.reject(ys)
		if release != nil {
			release()
		}
		return
	}
	_ = ys.SetReadDeadline(time.Time{})
	select {
	case s.accepted <- &Stream{ys: ys, kind: kind, pre: release}:
	case <-s.done:
		resetStream(ys)
		if release != nil {
			release()
		}
	}
}

// reject closes an unwanted inbound stream and returns only when yamux has
// released it, so the caller's resource slot covers the stream's whole life.
func (s *Session) reject(ys *yamux.Stream) {
	_ = ys.Close()
	drainStream(ys)
}

// watch runs the keepalive monitor and closes the session when the peer is
// dead.
func (s *Session) watch() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	err := RunKeepalive(ctx, s.opts.Keepalive, s.conn.lastRead, func() error {
		_, err := s.ys.Ping()
		return err
	})
	if err == ErrKeepaliveTimeout {
		s.shutdown(err)
	}
}

// activityConn records the time of the last bytes read from the peer.
type activityConn struct {
	net.Conn
	clk  Clock
	last atomic.Int64 // unix nanoseconds
}

func (c *activityConn) touch() { c.last.Store(c.clk.Now().UnixNano()) }

func (c *activityConn) lastRead() time.Time { return time.Unix(0, c.last.Load()) }

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

// ConnectWorker is the whole Worker side of a connection: it dials the Hub
// WebSocket (any TLS certificate is accepted, A-15), runs the AD-6 handshake
// described by cfg and starts the yamux client session on the resulting
// channel. ctx bounds the dial and the handshake and nothing after: close the
// Session to end the connection.
func ConnectWorker(ctx context.Context, hubURL string, cfg WorkerConfig, opts SessionOptions) (*Session, *WorkerResult, error) {
	conn, err := Dial(ctx, hubURL)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	res, err := WorkerHandshake(conn, cfg)
	if !stop() && err == nil {
		_ = res.Channel.Close()
		return nil, nil, ctx.Err()
	}
	if err != nil {
		return nil, nil, err
	}
	s, err := NewSession(res.Channel, RoleWorker, opts)
	if err != nil {
		return nil, nil, err
	}
	return s, res, nil
}

// AcceptWorker is the whole Hub side of a connection: it upgrades the request
// to the Worker WebSocket, runs the AD-6 handshake described by cfg and starts
// the yamux server session on the resulting channel. The caller decides, from
// the HubResult, whether the Worker is enrolled and answers inside the
// session. When the handshake fails the connection is closed and the error
// returned.
func AcceptWorker(w http.ResponseWriter, r *http.Request, cfg HubConfig, opts SessionOptions) (*Session, *HubResult, error) {
	conn, err := Accept(w, r)
	if err != nil {
		return nil, nil, err
	}
	res, err := HubHandshake(conn, cfg)
	if err != nil {
		return nil, nil, err
	}
	s, err := NewSession(res.Channel, RoleHub, opts)
	if err != nil {
		return nil, nil, err
	}
	return s, res, nil
}
