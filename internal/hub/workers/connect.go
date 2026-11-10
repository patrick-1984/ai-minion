package workers

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

const (
	// controlTimeout bounds each read or write of a control frame after the
	// handshake.
	controlTimeout = 10 * time.Second
)

// Policy is the Hub-side restriction delivered in welcome (AD-12). The Worker
// combines it with its local policy and its local restrictions always win.
// Story 6.6 fills it from the Worker row.
type Policy struct {
	PreviewOnly bool `json:"preview_only"`
}

// Welcome is the frame the Hub sends to an admitted Worker. proto_min and
// proto_max are the Hub's supported protocol range (current minor and N-1
// minor), which "minion update" checks for overlap (AD-4, A-10).
type Welcome struct {
	Type     string `json:"type"`
	WorkerID string `json:"worker_id"`
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	Kind     string `json:"kind"`
	Policy   Policy `json:"policy"`
	proto.Range
}

// RevokeFrame is the signed revocation (AD-9). Sig is an Ed25519 signature by
// the Hub key with purpose "revoke" over the transcript_hash of the handshake
// it answers, so it cannot be replayed on another connection.
type RevokeFrame struct {
	Type string       `json:"type"`
	Sig  identity.B64 `json:"sig"`
}

// SignRevoke signs a revocation for the handshake with the given
// transcript hash.
func SignRevoke(hub *identity.Key, transcriptHash []byte) (RevokeFrame, error) {
	sig, err := hub.Sign(identity.PurposeRevoke, transcriptHash)
	if err != nil {
		return RevokeFrame{}, err
	}
	return RevokeFrame{Type: proto.TypeRevoke, Sig: sig}, nil
}

// VerifyRevoke checks a revocation against the pinned Hub key and the current
// transcript hash.
func VerifyRevoke(hubPub ed25519.PublicKey, transcriptHash []byte, f RevokeFrame) error {
	return identity.Verify(hubPub, identity.PurposeRevoke, transcriptHash, f.Sig)
}

// Repository is the read side of the workers tables that the endpoint needs;
// *store.Store implements it.
type Repository interface {
	WorkerByPub(ctx context.Context, pub []byte) (store.WorkerRow, error)
	TombstoneByPub(ctx context.Context, pub []byte) (store.TombstoneRow, error)
}

// Limiter is the shared rate limiter (internal/ratelimit); *ratelimit.Limiter
// implements it.
type Limiter interface {
	Allow(policy ratelimit.Policy, ip netip.Addr, target string) (allowed bool, retryAfter time.Duration)
}

// Auditor records security-relevant events (AD-20); *audit.Log implements it.
type Auditor interface {
	Record(ctx context.Context, ev audit.Event) error
}

// ModeHandler serves a handshake mode other than connect after the handshake
// succeeded: Story 4.5 registers enroll_invite and Story 4.8 pair_code. It
// answers the Worker through the [Conn] (Welcome, Reject) and returns when it
// is done; an error is logged and the connection closed.
type ModeHandler interface {
	Handle(ctx context.Context, c *Conn) error
}

// PairingGate is the Hub side of the pair_code handshake (Story 4.8);
// *Pairings implements it. The handshake needs the CPace password before the
// Hub answers, and the password is the code a user types in later, so the
// handler lets the gate hold the connection as pending while it waits.
type PairingGate interface {
	// Await registers the pending pairing id of a connection and blocks until
	// a user claims it. It returns the connection's ticket: the CPace
	// password, and the entry and claim that belong to this connection and no
	// other. The handler keeps the ticket on the [Conn] and reports a failed
	// handshake through it. Await fails with pairing_id_collision,
	// pairing_attempts_exceeded, an expiry or when ctx ends.
	Await(ctx context.Context, pairingID string, ip netip.Addr) (*PairingTicket, error)
	// MaxWait is the longest Await blocks. The handler lifts the handshake
	// deadline by this much while it waits, and no longer.
	MaxWait() time.Duration
}

// Config configures a [Handler]. Identity, Repository and Registry are
// required.
type Config struct {
	Identity   *Identity
	Repository Repository
	Registry   *Registry
	// Limiter, when set, applies the connect policy by client IP before the
	// handshake. The Service always sets one.
	Limiter Limiter
	// ClientIP resolves the client address of a request (trusted proxy
	// headers included). The default is the peer address of the connection.
	ClientIP func(*http.Request) netip.Addr
	// Auditor, when set, records revocations that were delivered.
	Auditor Auditor
	// Chain returns the transition chain sent in connect mode (Story 4.9).
	Chain func(ctx context.Context) ([]identity.Transition, error)
	// Policy returns the Hub-side policy of a Worker; the default is no
	// restriction.
	Policy func(ctx context.Context, w store.WorkerRow) (Policy, error)
	// OwnerName returns the display name sent as welcome.owner; the default
	// is the owner id. The auth module wires the user name.
	OwnerName func(ctx context.Context, ownerID string) string
	// PairingPassword serves the CPace password of a live pairing code for
	// the pair_code mode (Story 4.8).
	PairingPassword func(pairingID string) ([]byte, error)
	// RemoveWorker removes a Worker's enrollment and leaves a tombstone. It
	// serves the worker_retire call (Story 4.7); without it the Hub answers
	// that call with unsupported_op. The Service sets it from the store; the
	// Hub module replaces it with the full removal through
	// [Service.SetRemoveWorker].
	RemoveWorker RemoveWorkerFunc
	// Current is the Hub's protocol version; the zero value means
	// proto.Current.
	Current proto.Version
	// Session tunes the yamux session (tests inject a clock).
	Session transport.SessionOptions
	// HandshakeTimeout bounds the AD-6 handshake; zero means the transport
	// default.
	HandshakeTimeout time.Duration
	// Streams bounds the inbound streams of the Workers' sessions, per Worker
	// and for the whole Hub; a zero member selects its default (admission.go).
	Streams StreamLimits
	Logger  *slog.Logger
}

// Handler serves GET /api/v1/worker/connect.
type Handler struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.RWMutex
	modes   map[transport.Mode]ModeHandler
	pairing PairingGate
	done    chan struct{}
	once    sync.Once

	// afterRecheck, when set by a test before the first connect, is called
	// when the post-registration admission recheck of a connection has
	// finished (whatever its outcome), so that a test can order its own
	// changes after it deterministically.
	afterRecheck func()

	// removeMu guards remove and removeAudited, which [Handler.SetRemoveWorker]
	// may replace while connections are served.
	removeMu      sync.RWMutex
	remove        RemoveWorkerFunc
	removeAudited bool

	// callMu guards callOps, the ops that later stories serve on the call
	// streams ([Handler.HandleCallOp]).
	callMu  sync.RWMutex
	callOps map[string]CallOp

	// admOnce creates adm, the stream counters of all Workers.
	admOnce sync.Once
	adm     *streamAdmission

	// connMu guards live and stopped. Every accepted connection is in live
	// from the upgrade until its handler returns, so that Shutdown reaches
	// the ones that are still in the handshake; wg counts the running
	// handlers for [Handler.Wait].
	connMu  sync.Mutex
	live    map[net.Conn]context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
}

// NewHandler validates cfg and returns the endpoint handler.
func NewHandler(cfg Config) (*Handler, error) {
	switch {
	case cfg.Identity == nil:
		return nil, errors.New("workers: the connect handler needs the Hub identity")
	case cfg.Repository == nil:
		return nil, errors.New("workers: the connect handler needs a repository")
	case cfg.Registry == nil:
		return nil, errors.New("workers: the connect handler needs a registry")
	}
	if cfg.Current == (proto.Version{}) {
		cfg.Current = proto.Current
	}
	if cfg.ClientIP == nil {
		cfg.ClientIP = peerIP
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, log: log, modes: map[transport.Mode]ModeHandler{}, done: make(chan struct{}),
		live: map[net.Conn]context.CancelFunc{}, remove: cfg.RemoveWorker}, nil
}

// SetRemoveWorker replaces the function that serves the worker_retire call. A
// removal that writes the audit record itself, inside its own transaction,
// passes audited true, so that the call does not write a second, best-effort
// record. A nil f makes the Hub answer worker_retire with unsupported_op. Safe
// to call before and while the Handler serves.
func (h *Handler) SetRemoveWorker(f RemoveWorkerFunc, audited bool) {
	h.removeMu.Lock()
	defer h.removeMu.Unlock()
	h.remove, h.removeAudited = f, audited && f != nil
}

func (h *Handler) removal() (f RemoveWorkerFunc, audited bool) {
	h.removeMu.RLock()
	defer h.removeMu.RUnlock()
	return h.remove, h.removeAudited
}

// Handle registers the handler of an additional handshake mode. connect is
// served by the Handler itself and cannot be replaced.
func (h *Handler) Handle(mode transport.Mode, m ModeHandler) error {
	if mode == transport.ModeConnect || !mode.Valid() || m == nil {
		return fmt.Errorf("workers: cannot register a handler for mode %q", mode)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.modes[mode]; dup {
		return fmt.Errorf("workers: mode %q already has a handler", mode)
	}
	h.modes[mode] = m
	return nil
}

// setPairing installs the gate that serves the pair_code password (Story
// 4.8). It replaces Config.PairingPassword for every later connection.
func (h *Handler) setPairing(g PairingGate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pairing = g
}

func (h *Handler) pairingGate() PairingGate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pairing
}

// handshakeBudget is the time the AD-6 handshake may take.
func (h *Handler) handshakeBudget() time.Duration {
	if h.cfg.HandshakeTimeout > 0 {
		return h.cfg.HandshakeTimeout
	}
	return transport.DefaultHandshakeTimeout
}

// pairingPassword returns the HubConfig.PairingPassword of one connection. It
// waits for the user's claim outside the handshake budget (the wait is up to
// the code's lifetime, the budget is for talking), then starts the budget
// again for the rest of the handshake. ticket receives the connection's
// ticket, which reports a failed handshake back to the gate.
//
// While the gate waits, nothing else reads the connection and the hijacked
// request context does not end when the peer closes, so the wait would never
// notice a Worker that is gone. conn therefore watches the connection for the
// duration of the wait: a Worker that closes, or that sends anything before
// the Hub hello (a protocol violation), ends the wait.
func (h *Handler) pairingPassword(ctx context.Context, conn *watchConn, ip netip.Addr, gate PairingGate, ticket **PairingTicket) func(string) ([]byte, error) {
	return func(id string) ([]byte, error) {
		budget := h.handshakeBudget()
		_ = conn.SetDeadline(time.Now().Add(gate.MaxWait() + budget))
		awaitCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		conn.watch(cancel)
		t, err := gate.Await(awaitCtx, id, ip)
		if err != nil {
			return nil, err
		}
		*ticket = t
		_ = conn.SetDeadline(time.Now().Add(budget))
		return t.Password(), nil
	}
}

// watchConn is a net.Conn that can watch the connection while nothing else
// reads it. watch starts one background read of a single byte; the next Read
// of the owner of the connection receives that read's result (the byte or the
// error) before it reads the connection itself, so the stream the handshake
// sees is unchanged. Deadlines, Close and everything else pass through.
type watchConn struct {
	net.Conn

	mu      sync.Mutex
	pending chan watchResult // the outstanding background read, nil when none
}

type watchResult struct {
	b   byte
	err error
}

func newWatchConn(c net.Conn) *watchConn { return &watchConn{Conn: c} }

// watch starts the background read. onBreak is called when the read fails
// (the peer is gone) or returns data (the Worker spoke out of turn).
func (c *watchConn) watch(onBreak func()) {
	ch := make(chan watchResult, 1)
	c.mu.Lock()
	if c.pending != nil {
		c.mu.Unlock()
		return // one background read at a time
	}
	c.pending = ch
	c.mu.Unlock()
	go func() {
		var b [1]byte
		for {
			n, err := c.Conn.Read(b[:])
			if n > 0 {
				ch <- watchResult{b: b[0]}
				onBreak()
				return
			}
			if err != nil {
				ch <- watchResult{err: err}
				onBreak()
				return
			}
		}
	}()
}

// Read hands the result of the background read to the first Read after it,
// then reads the connection.
func (c *watchConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	ch := c.pending
	c.pending = nil
	c.mu.Unlock()
	if ch == nil {
		return c.Conn.Read(p)
	}
	r := <-ch
	if r.err != nil {
		return 0, r.err
	}
	p[0] = r.b
	return 1, nil
}

func (h *Handler) modeHandler(mode transport.Mode) ModeHandler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.modes[mode]
}

// Shutdown makes the handler refuse new connections and ends the ones that
// are still being authenticated: their context is cancelled and their
// connection closed. Online Workers end through h.done; the caller also
// closes the registry's sessions. Use [Handler.Wait] to drain the handlers.
func (h *Handler) Shutdown() {
	h.once.Do(func() {
		h.connMu.Lock()
		h.stopped = true
		close(h.done)
		conns := h.live
		h.live = map[net.Conn]context.CancelFunc{}
		h.connMu.Unlock()
		for c, cancel := range conns {
			cancel()
			_ = c.Close()
		}
	})
}

// Wait blocks until every running connection handler has returned or ctx is
// done. Call it after [Handler.Shutdown], before the services the handlers
// use (store, audit log) are stopped.
func (h *Handler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// begin registers a running handler; it reports false after Shutdown.
func (h *Handler) begin() bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.stopped {
		return false
	}
	h.wg.Add(1)
	return true
}

// track adds an accepted connection to the live set; it reports false (and
// the caller closes the connection) when Shutdown already ran.
func (h *Handler) track(c net.Conn, cancel context.CancelFunc) bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.stopped {
		return false
	}
	h.live[c] = cancel
	return true
}

func (h *Handler) untrack(c net.Conn) {
	h.connMu.Lock()
	delete(h.live, c)
	h.connMu.Unlock()
}

// ServeHTTP implements http.Handler. It blocks while the Worker is connected:
// the HTTP connection is the Worker's WebSocket.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.begin() {
		writeHTTPError(w, proto.NewError(proto.CodeWorkerOffline, "the Hub is shutting down"), time.Second)
		return
	}
	defer h.wg.Done()
	ip := h.cfg.ClientIP(r)
	if h.cfg.Limiter != nil {
		// Before the upgrade and the handshake: a flood costs one cheap
		// refusal, never a key agreement (AD-19).
		if ok, wait := h.cfg.Limiter.Allow(ratelimit.PolicyConnect, ip, ""); !ok {
			writeHTTPError(w, proto.NewError(proto.CodeRateLimited, "too many connection attempts; retry later"), wait)
			return
		}
	}

	// The chain is read before the upgrade, so that a failure is an ordinary
	// HTTP error and no WebSocket has to be closed (Story 4.9: the chain is
	// read from the database, per connection, so a rotation is visible at
	// once). The key is read first and the chain is cut at it: a rotation
	// that is recorded but not yet confirmed durable (Rotator) has its link in
	// the database while the Hub still signs with the previous key, and a
	// Worker must never be shown a link that leads past the key it meets.
	hubKey := h.cfg.Identity.Key()
	chain, err := h.chain(r.Context())
	if err != nil {
		h.log.Error("worker connect: cannot read the transition chain", "error", err)
		writeHTTPError(w, proto.NewError(proto.CodeInternal, ""), 0)
		return
	}
	chain = chainUpTo(chain, hubKey.Public())
	conn, err := transport.Accept(w, r)
	if err != nil {
		return // Accept has answered the request
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if !h.track(conn, cancel) {
		_ = conn.Close() // Shutdown ran between begin and the upgrade
		return
	}
	defer h.untrack(conn)
	hc := transport.HubConfig{
		Key:             hubKey,
		Current:         h.cfg.Current,
		Chain:           chain,
		Admit:           h.admit,
		PairingPassword: h.cfg.PairingPassword,
		Timeout:         h.cfg.HandshakeTimeout,
	}
	var pairingTicket *PairingTicket
	hsConn := conn
	if gate := h.pairingGate(); gate != nil {
		wc := newWatchConn(conn)
		hsConn = wc
		hc.PairingPassword = h.pairingPassword(ctx, wc, ip, gate, &pairingTicket)
	}
	res, err := transport.HubHandshake(hsConn, hc)
	if err != nil {
		if pairingTicket != nil {
			pairingTicket.HandshakeFailed(err)
		}
		// HubHandshake closed conn and told the Worker what it may know.
		h.log.Info("worker connect: handshake failed", "client_ip", ip.String(), "code", string(transport.ErrorCode(err)))
		return
	}
	c := &Conn{h: h, res: res, ip: ip, pairing: pairingTicket}
	defer c.finish()

	if res.Worker.Mode == transport.ModeConnect {
		err = h.serveConnect(ctx, c)
	} else if m := h.modeHandler(res.Worker.Mode); m != nil {
		err = m.Handle(ctx, c)
	} else {
		err = c.Reject(proto.NewError(proto.CodeInvalidArgument, "this Hub does not accept the "+string(res.Worker.Mode)+" mode"))
	}
	if err != nil {
		h.log.Info("worker connect: ended", "client_ip", ip.String(), "error", err.Error())
	}
}

// admit runs after the Worker hello was read and validated, before the Hub
// sends anything.
func (h *Handler) admit(wh transport.WorkerHello) error {
	if wh.Mode != transport.ModeConnect && h.modeHandler(wh.Mode) == nil {
		return proto.NewError(proto.CodeInvalidArgument, "this Hub does not accept the "+string(wh.Mode)+" mode")
	}
	return nil
}

func (h *Handler) chain(ctx context.Context) ([]identity.Transition, error) {
	if h.cfg.Chain == nil {
		return nil, nil
	}
	return h.cfg.Chain(ctx)
}

// serveConnect decides what a connect from an authenticated key means
// (AD-6, AD-9): a tombstoned key gets a signed revoke, an unknown key a
// non-destructive not_enrolled and an enrolled Worker its welcome.
func (h *Handler) serveConnect(ctx context.Context, c *Conn) error {
	pub := []byte(c.res.WorkerPub)
	if t, err := h.cfg.Repository.TombstoneByPub(ctx, pub); err == nil {
		return c.revoke(ctx, t)
	} else if !errors.Is(err, store.ErrWorkerNotFound) {
		_ = c.Reject(proto.NewError(proto.CodeInternal, ""))
		return fmt.Errorf("read tombstone: %w", err)
	}
	w, err := h.cfg.Repository.WorkerByPub(ctx, pub)
	switch {
	case errors.Is(err, store.ErrWorkerNotFound):
		h.log.Info("worker connect: unknown key", "fingerprint", c.fingerprint(), "client_ip", c.ip.String())
		return c.NotEnrolled()
	case err != nil:
		_ = c.Reject(proto.NewError(proto.CodeInternal, ""))
		return fmt.Errorf("read worker: %w", err)
	}
	return c.Welcome(ctx, w)
}

// Conn is an authenticated Worker connection after the handshake and before
// yamux: the Hub answers on it with exactly one of [Conn.Welcome],
// [Conn.NotEnrolled] and [Conn.Reject]. Mode handlers get it from the
// [Handler]. It is not safe for concurrent use.
type Conn struct {
	h    *Handler
	res  *transport.HubResult
	ip   netip.Addr
	sent bool
	sess *transport.Session
	// pairing is the ticket of a pair_code connection (nil otherwise): the
	// pending entry and claim this connection was bound to during the wait.
	pairing *PairingTicket
}

// Worker returns the validated Worker hello.
func (c *Conn) Worker() transport.WorkerHello { return c.res.Worker }

// WorkerPub returns the authenticated public key of the Worker.
func (c *Conn) WorkerPub() ed25519.PublicKey { return c.res.WorkerPub }

// ClientIP returns the client address of the connection.
func (c *Conn) ClientIP() netip.Addr { return c.ip }

// ReadFrame reads one control frame the Worker sent inside the channel (for
// example invite.secret). It must be called before the Hub's answer.
func (c *Conn) ReadFrame() (json.RawMessage, error) {
	_ = c.res.Channel.SetReadDeadline(time.Now().Add(controlTimeout))
	defer func() { _ = c.res.Channel.SetReadDeadline(time.Time{}) }()
	return proto.ReadFrame(c.res.Channel, proto.StreamFrameLimit)
}

func (c *Conn) fingerprint() string {
	f, _ := identity.FingerprintOf(c.res.WorkerPub)
	return f.String()
}

func (c *Conn) write(v any) error {
	if c.sent {
		return errors.New("workers: the connection was already answered")
	}
	c.sent = true
	_ = c.res.Channel.SetWriteDeadline(time.Now().Add(controlTimeout))
	defer func() { _ = c.res.Channel.SetWriteDeadline(time.Time{}) }()
	return proto.WriteFrame(c.res.Channel, proto.StreamFrameLimit, v)
}

// NotEnrolled answers an unknown key with the non-destructive not_enrolled
// frame (AD-9): the Worker keeps its state and retries. The connection is
// closed afterwards.
func (c *Conn) NotEnrolled() error {
	return c.write(map[string]string{"type": proto.TypeNotEnrolled})
}

// Reject answers with an error frame and closes the connection.
func (c *Conn) Reject(e *proto.Error) error { return c.write(e.Frame()) }

// revoke sends the signed revoke to a tombstoned key and records it.
func (c *Conn) revoke(ctx context.Context, t store.TombstoneRow) error {
	f, err := SignRevoke(c.h.cfg.Identity.Key(), c.res.TranscriptHash[:])
	if err != nil {
		_ = c.Reject(proto.NewError(proto.CodeInternal, ""))
		return err
	}
	if err := c.write(f); err != nil {
		return err
	}
	c.h.log.Info("worker connect: revoked a removed Worker", "worker_id", t.WorkerID, "client_ip", c.ip.String())
	if a := c.h.cfg.Auditor; a != nil {
		if err := a.Record(ctx, audit.Event{
			Actor: "worker:" + t.WorkerID, Action: "worker.connect", Target: t.WorkerID, Outcome: "revoked",
		}); err != nil {
			c.h.log.Warn("worker connect: cannot audit the revoke", "worker_id", t.WorkerID, "error", err.Error())
		}
	}
	return nil
}

// Welcome admits the Worker: it sends welcome, starts the yamux session,
// registers the Worker online and blocks until the session ends. The ops the
// Worker announced and the protocol version negotiated by the handshake are
// recorded in the registry.
func (c *Conn) Welcome(ctx context.Context, w store.WorkerRow) error {
	h := c.h
	pol := Policy{}
	if h.cfg.Policy != nil {
		p, err := h.cfg.Policy(ctx, w)
		if err != nil {
			_ = c.Reject(proto.NewError(proto.CodeInternal, ""))
			return fmt.Errorf("read policy: %w", err)
		}
		pol = p
	}
	owner := w.OwnerID
	if h.cfg.OwnerName != nil {
		owner = h.cfg.OwnerName(ctx, w.OwnerID)
	}
	if err := c.write(Welcome{
		Type:     proto.TypeWelcome,
		WorkerID: w.ID,
		Name:     w.Name,
		Owner:    owner,
		Kind:     w.Kind,
		Policy:   pol,
		Range:    proto.SupportedRange(h.cfg.Current),
	}); err != nil {
		return fmt.Errorf("send welcome: %w", err)
	}
	// The stream admission of this Worker is the session's budget: it is
	// consulted for every stream before its kind is read, so it must exist
	// before the accept loop of the session starts.
	adm := h.streamAdmission().attach(w.ID)
	sopts := h.cfg.Session
	sopts.Budget = adm
	sess, err := transport.NewSession(c.res.Channel, transport.RoleHub, sopts)
	if err != nil {
		adm.detach()
		return fmt.Errorf("start session: %w", err)
	}
	c.sess = sess
	// The session has one reader of its inbound streams: the accept loop of
	// the host below. The call streams are served by the handler's own
	// callStreams, registered before the Worker is online so that no hook of
	// the registry can take the kind; modules (the event ingest) join the host
	// from their Registry.OnAdd hook.
	host := newHostedSession(sess, adm, h.log)
	host.Handle(transport.KindCall, newCallStreams(h, w, sess))
	h.cfg.Registry.Add(Online{
		WorkerID: w.ID,
		Name:     w.Name,
		OwnerID:  w.OwnerID,
		Kind:     w.Kind,
		Version:  c.res.Version,
		Ops:      c.res.Worker.Ops,
		// The hello is covered by the transcript hash that both peers signed
		// (AD-6): these are what the authenticated Worker announced.
		Features: c.res.Worker.Features,

		TranscriptHash: append([]byte(nil), c.res.TranscriptHash[:]...),
	}, host)
	h.log.Info("worker online", "worker_id", w.ID, "name", w.Name, "protocol", c.res.Version.String())
	h.recheckAdmitted(ctx, c, w, pol)
	if h.afterRecheck != nil {
		h.afterRecheck()
	}

	streams := make(chan struct{})
	go func() {
		defer close(streams)
		host.serve(ctx)
	}()
	select {
	case <-sess.Done():
	case <-h.done:
		_ = sess.Close()
	}
	<-streams
	h.log.Info("worker offline", "worker_id", w.ID, "reason", causeText(sess.Err()))
	return nil
}

// recheckAdmitted closes the windows between reading the Worker row and the
// policy and registering the Worker online. A change that committed in between
// found the Worker offline and could not end a connection that did not exist
// yet; the Worker is registered now, so a change committing after this check
// finds it online and ends it, and one that committed before it is seen here.
// Either way the connection ends and the Worker's reconnect reads the new
// state:
//
//   - a removal: the reconnect gets the signed revoke of the tombstone (AD-9);
//   - a Hub-side policy change (Story 6.6): the reconnect gets the new policy
//     in its welcome. Without this check a Worker that connected while
//     preview_only was switched on would keep the policy it was welcomed with
//     for as long as it stays online, and repeating the restriction changes
//     nothing and ends nothing.
//
// A failed check ends the connection too (fail closed): the reconnect decides.
//
// Only this connection's own session is closed, never whatever the registry
// holds for the Worker: a late recheck of a superseded connection must not
// close the newer, correctly welcomed connection of the same Worker.
func (h *Handler) recheckAdmitted(ctx context.Context, c *Conn, w store.WorkerRow, welcomed Policy) {
	_, err := h.cfg.Repository.TombstoneByPub(ctx, []byte(c.res.WorkerPub))
	switch {
	case err == nil:
		h.log.Info("worker was removed while it connected; the connection is ended", "worker_id", w.ID)
	case !errors.Is(err, store.ErrWorkerNotFound):
		h.log.Warn("worker connect: the removal check failed; the connection is ended", "worker_id", w.ID, "error", err.Error())
	default:
		if h.cfg.Policy == nil {
			return
		}
		cur, perr := h.cfg.Policy(ctx, w)
		switch {
		case perr != nil:
			h.log.Warn("worker connect: the policy check failed; the connection is ended", "worker_id", w.ID, "error", perr.Error())
		case cur != welcomed:
			h.log.Info("worker policy changed while it connected; the connection is ended", "worker_id", w.ID)
		default:
			return
		}
	}
	_ = c.sess.Close()
}

// finish ends the connection. After a refusal frame the WebSocket close
// handshake delivers the frame before the Worker sees the close.
func (c *Conn) finish() {
	if c.sess != nil {
		_ = c.sess.Close()
		return
	}
	_ = c.res.Channel.Close()
}

func causeText(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}

// peerIP is the default client address: the peer of the connection.
func peerIP(r *http.Request) netip.Addr {
	ip, err := ratelimit.ParseRemote(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

// writeHTTPError answers a request that is refused before the WebSocket
// upgrade, in the AD-18 error shape.
func writeHTTPError(w http.ResponseWriter, e *proto.Error, retryAfter time.Duration) {
	if retryAfter > 0 {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(proto.HTTPStatus(e.Code))
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message}})
	_, _ = w.Write(body)
}

// Store is everything the [Service] needs from the database; *store.Store
// implements it.
type Store interface {
	IdentityRepository
	Repository
	WorkerOwner(ctx context.Context, workerID string) (string, error)
}

// ServiceOptions configures [NewService].
type ServiceOptions struct {
	// DataDir holds identity.key.
	DataDir string
	Store   Store
	// Limiter limits connects by IP; the default is a new limiter with the
	// built-in "connect" policy.
	Limiter Limiter
	// ClientIP, Auditor, Chain, Policy, PairingPassword, Current, Session,
	// HandshakeTimeout and Logger are passed to the [Handler] ([Config]).
	ClientIP         func(*http.Request) netip.Addr
	Auditor          Auditor
	Chain            func(ctx context.Context) ([]identity.Transition, error)
	Policy           func(ctx context.Context, w store.WorkerRow) (Policy, error)
	PairingPassword  func(pairingID string) ([]byte, error)
	Current          proto.Version
	Session          transport.SessionOptions
	HandshakeTimeout time.Duration
	Logger           *slog.Logger
	// RemoveWorker replaces the default removal (tombstone of the Worker
	// row, [store.Store.TombstoneWorker]). The Hub module installs the full
	// removal of Story 4.10 (bound keys, trust rows, audit record in one
	// transaction) later with [Service.SetRemoveWorker].
	RemoveWorker RemoveWorkerFunc
}

// Service is the Worker side of the Hub: identity, registry and the connect
// handler, wired once. The Hub module provides it to the other modules.
type Service struct {
	identity *Identity
	registry *Registry
	handler  *Handler
	store    Store
	limiter  Limiter
	rotator  *Rotator // nil when the store cannot hold a transition chain
	dataDir  string

	mu         sync.RWMutex
	ownerNames func(ctx context.Context, ownerID string) string
	pairings   *Pairings
}

// NewService loads or creates the Hub identity and builds the registry and
// the connect handler.
func NewService(ctx context.Context, o ServiceOptions) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("workers: the service needs a store")
	}
	id, err := LoadOrCreateIdentity(ctx, o.DataDir, o.Store, o.Logger)
	if err != nil {
		return nil, err
	}
	limiter := o.Limiter
	if limiter == nil {
		l, err := ratelimit.New(ratelimit.Config{})
		if err != nil {
			return nil, err
		}
		limiter = l
	}
	s := &Service{identity: id, registry: NewRegistry(), store: o.Store, limiter: limiter, dataDir: o.DataDir}
	if ts, ok := o.Store.(interface {
		TombstoneWorker(ctx context.Context, id, removedBy string, removedAt int64) error
	}); ok && o.RemoveWorker == nil {
		o.RemoveWorker = ts.TombstoneWorker
	}
	chain := o.Chain
	if rs, ok := o.Store.(RotationStore); ok {
		// The Hub rotates its identity (Story 4.9): the stored chain is sent
		// in connect mode unless the caller supplies its own source.
		if chain == nil {
			chain = ChainSource(rs)
		}
		s.rotator, err = NewRotator(RotatorOptions{Identity: id, DataDir: o.DataDir, Store: rs, Logger: o.Logger})
		if err != nil {
			return nil, err
		}
	}
	h, err := NewHandler(Config{
		Identity:         id,
		Repository:       o.Store,
		Registry:         s.registry,
		Limiter:          limiter,
		ClientIP:         o.ClientIP,
		Auditor:          o.Auditor,
		Chain:            chain,
		Policy:           o.Policy,
		RemoveWorker:     o.RemoveWorker,
		OwnerName:        s.ownerName,
		PairingPassword:  o.PairingPassword,
		Current:          o.Current,
		Session:          o.Session,
		HandshakeTimeout: o.HandshakeTimeout,
		Logger:           o.Logger,
	})
	if err != nil {
		return nil, err
	}
	s.handler = h
	return s, nil
}

// Identity returns the Hub identity.
func (s *Service) Identity() *Identity { return s.identity }

// Rotator returns the rotator of the Hub identity (Story 4.9), or nil when the
// store cannot hold a transition chain (a test double). The ops adapter of
// hub_identity_rotate calls [Rotator.Rotate]; Story 4.5 registers its
// invitation revoker with [Rotator.SetInvitations].
func (s *Service) Rotator() *Rotator { return s.rotator }

// Registry returns the registry of online Workers.
func (s *Service) Registry() *Registry { return s.registry }

// Handler returns the connect endpoint handler (mount it at
// transport.ConnectPath).
func (s *Service) Handler() *Handler { return s.handler }

// SetRemoveWorker replaces the removal that serves the worker_retire call of a
// Worker (Story 4.7, amendment A-16): the Hub module passes the removal of
// Story 4.10 (revoke the bound API keys, delete the trust rows, tombstone and
// write the audit record in one transaction), because the default is the bare
// tombstone. audited says that f writes that audit record itself. It is safe to
// call before and while the service serves.
func (s *Service) SetRemoveWorker(f RemoveWorkerFunc, audited bool) {
	s.handler.SetRemoveWorker(f, audited)
}

// HandleCallOp registers the serving of an op on the Workers' call streams
// ([Handler.HandleCallOp]): the extension point of Story 7.4. There is no
// second accept loop to add: the call streams of a session are read by the one
// loop of the connect handler.
func (s *Service) HandleCallOp(op string, fn CallOp) bool { return s.handler.HandleCallOp(op, fn) }

// SetOwnerNames sets how welcome.owner is rendered: the auth module passes
// the user name lookup. Without it the owner id is sent.
func (s *Service) SetOwnerNames(f func(ctx context.Context, ownerID string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerNames = f
}

func (s *Service) ownerName(ctx context.Context, ownerID string) string {
	s.mu.RLock()
	f := s.ownerNames
	s.mu.RUnlock()
	if f == nil {
		return ownerID
	}
	return f(ctx, ownerID)
}

// OwnerLookup returns the authz port that resolves a Worker's owner from the
// database on every call, with no cache (AD-11).
func (s *Service) OwnerLookup() authz.WorkerLookup { return ownerLookup{s.store} }

// shutdownWait bounds how long [Service.Close] waits for connection handlers.
const shutdownWait = 5 * time.Second

// Close stops accepting connections, ends the connections that are still in
// the handshake, closes every online session and waits for the connection
// handlers to return, so that nothing touches the store or the audit log
// after Close.
func (s *Service) Close() {
	s.handler.Shutdown()
	if p := s.Pairings(); p != nil {
		p.Close()
	}
	s.registry.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	if err := s.handler.Wait(ctx); err != nil {
		s.handler.log.Warn("workers: connection handlers did not stop in time", "error", err.Error())
	}
}

type ownerLookup struct{ st Store }

func (l ownerLookup) WorkerOwner(ctx context.Context, workerID string) (string, error) {
	owner, err := l.st.WorkerOwner(ctx, workerID)
	if errors.Is(err, store.ErrWorkerNotFound) {
		return "", authz.ErrNotFound
	}
	return owner, err
}

// streamAdmission returns the stream counters shared by the sessions of all
// Workers of this handler.
func (h *Handler) streamAdmission() *streamAdmission {
	h.admOnce.Do(func() { h.adm = newStreamAdmission(h.cfg.Streams, transport.ReleaseBound) })
	return h.adm
}
