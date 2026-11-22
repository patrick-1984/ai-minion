package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Timing of the connector.
const (
	// DefaultIdlePoll is how often an idle connector (no Hub URL, or not
	// enrolled) looks at the enrollment state again, besides the wake-up that
	// [Enrollments.Changed] gives within this process.
	DefaultIdlePoll = 5 * time.Second
	// DefaultHandlerGrace bounds how long a lost connection waits for the
	// link handlers to wind down before the reconnect starts.
	DefaultHandlerGrace = 2 * time.Second
	// DefaultEpochPoll is how often a connection checks whether another
	// process (`minion worker pair --new-code`, a reset) changed the
	// enrollment, so that the old Hub's connection is dropped promptly.
	DefaultEpochPoll = time.Second
)

// ErrLinkLost is the cause of the context of a [Link] whose connection ended.
// A handler that has an op in flight when it sees it reports the op as
// outcome unknown (AD-12, AD-15): the Worker cannot tell the Hub the result,
// and nothing is retried.
var ErrLinkLost = errors.New("agent: the connection to the Hub was lost")

// errEnrollmentChanged is the cause of the context of a connection attempt
// that was dropped because the enrollment was committed or wiped meanwhile
// (a reset, a re-enrollment): whatever the old Hub says no longer applies.
var errEnrollmentChanged = errors.New("agent: the enrollment changed")

// HubWelcome is the welcome frame, the Hub's answer to an enrolled Worker
// (AD-6, AD-12). proto_min and proto_max are the Hub's supported range.
type HubWelcome struct {
	UsagePolicy int    `json:"usage_policy,omitempty"`
	Type        string `json:"type"`
	WorkerID    string `json:"worker_id"`
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	// OwnerID is the stable id of the Worker's owner (protocol 2.0). The
	// receiver compares it with the source_worker_owner_id of an origin for
	// pipe_accept_from=owner-workers; Owner is only the display name.
	OwnerID string `json:"owner_id"`
	Kind    string `json:"kind"`
	// Pending (F-02) is set while the Hub has not approved this Worker yet:
	// it is connected, but gets no operations and its calls are refused (except
	// retiring itself) until its owner or an admin approves it.
	Pending bool      `json:"pending,omitempty"`
	Policy  HubPolicy `json:"policy"`
	// HubTLS is what the Hub states about the TLS of this connection
	// (v0.11.0); nil from an older Hub.
	HubTLS *HubTLSInfo `json:"hub_tls,omitempty"`
	// MasterSessionV1 confirms support for the Worker master control lane.
	MasterSessionV1 bool `json:"master_session_v1,omitempty"`
	proto.Range
}

// Reply is the control frame the Hub sends inside the channel right after the
// handshake: exactly one of welcome, revoke and not_enrolled. A Hub error
// frame is returned by [Dialer.Connect] as a *proto.Error instead.
type Reply struct {
	// Type is proto.TypeWelcome, proto.TypeRevoke or proto.TypeNotEnrolled.
	Type string
	// Welcome is set for a welcome.
	Welcome HubWelcome
	// RevokeSig is the signature of a revoke, purpose "revoke" over the
	// transcript hash of this connection.
	RevokeSig []byte
}

// Session is the part of the transport session that link handlers use: the Hub
// opens op streams (accepted here), the Worker opens call and event streams.
// *transport.Session implements it.
type Session interface {
	Accept(ctx context.Context) (*transport.Stream, error)
	OpenCall(ctx context.Context) (*transport.Stream, error)
	OpenEvents(ctx context.Context) (*transport.Stream, error)
	Done() <-chan struct{}
	Err() error
	Close() error
}

// ConnectParams is everything one connection attempt needs.
type ConnectParams struct {
	// Facts are the display facts of the hello (build, platform, update block).
	Facts transport.WorkerFacts
	// HubURL is the configured Hub URL (any of https, http, wss, ws).
	HubURL string
	// Key is the Worker's identity key.
	Key *identity.Key
	// Name is the Worker's display name for the hello.
	Name string
	// Ops are the operations the Worker announces (AD-4).
	Ops []string
	// Features are the proto.Feature* names the Worker announces in the
	// signed hello (spine AM-13).
	Features []string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Pin is the stored Hub pin.
	Pin identity.Pin
	// Ratchet is the stored certificate memory (max_serial and the hash of
	// that certificate) scoped to the pinned root.
	Ratchet transport.Ratchet
	// Clock supplies the wall clock and the monotonic clock of the window
	// check; nil means the system clocks.
	Clock transport.WallMono
	// Session tunes the yamux session (tests inject a clock).
	Session transport.SessionOptions
	// Logger receives the debug lines of the attempt (URL, TLS peer,
	// protocol); nil writes nothing.
	Logger *slog.Logger
}

// Attempt is the result of a connection whose Hub was authenticated.
type Attempt struct {
	// Pin is the pin to rely on: the stored pin after applying the transition
	// chain of the Hub (AD-5).
	Pin identity.Pin
	// PinChanged is true when Pin differs from the stored pin; the connector
	// rewrites the stored pin before it relies on the connection.
	PinChanged bool
	// PinPassed are the keys the verified chain went through between the
	// stored pin and Pin (neither end included).
	PinPassed []ed25519.PublicKey
	// OpPub is the operational key of the certificate validated in this
	// handshake: the only key whose revoke counts on this connection.
	OpPub ed25519.PublicKey
	// Ratchet is the certificate memory after this connection and RatchetChanged
	// says that it differs from the stored one (a commit is needed also when only
	// the serial rose). The connector stores it with the pin before it relies on
	// the connection.
	Ratchet        transport.Ratchet
	RatchetChanged bool
	// Expiry is the end of this connection (Hub certificate not_after, fixed at
	// the window check). Nil means no enforcement: only a test double that does
	// not model the certificate leaves it nil; the production dialer always sets
	// it.
	Expiry *transport.Expiry
	// TranscriptHash identifies this handshake; a revoke is signed over it.
	TranscriptHash []byte
	// Version is the protocol version the Hub chose.
	Version proto.Version
	// Reply is the Hub's control frame.
	Reply Reply
	// Session is the open session; set for a welcome only. For every other
	// reply the Dialer has closed the connection.
	Session Session
	// TLS is what the dial saw of TLS (display only, A-15).
	TLS transport.TLSSeen
}

// Dialer is the transport port of the connector: it dials the Hub, runs the
// AD-6 handshake in connect mode, reads the control frame and, for a welcome,
// starts the yamux session. The production implementation is [NewHubDialer];
// tests use a fake. The dial accepts any TLS certificate, because trust is the
// handshake and never TLS (A-15).
type Dialer interface {
	Connect(ctx context.Context, p ConnectParams) (*Attempt, error)
}

// PolicySink receives the Hub policy from welcome. *Policy implements it.
type PolicySink interface {
	SetHubPreviewOnly(on bool)
}

// Link is one welcomed connection to the Hub. Handlers that serve it (the op
// server and the event stream, Story 5.2) get it from [Connector.OnLink].
type Link struct {
	// Welcome is the Hub's welcome of this connection.
	Welcome HubWelcome
	// Version is the protocol version the Hub chose.
	Version proto.Version
	// Session is the yamux session of this connection.
	Session Session

	ctx context.Context
	// pin, transcript and gen are what a live revoke is checked against
	// (Story 4.10): the pinned Hub key, the transcript hash of this
	// connection and the enrollment generation it was started from.
	pin        identity.Pin
	transcript []byte
	gen        uint64
	// opPub is the operational key of the certificate of this connection: the
	// signer of a revoke on it (section 12.4).
	opPub ed25519.PublicKey
}

// Context ends when the connection is lost or the Worker stops. For a lost
// connection its cause is [ErrLinkLost]; in-flight Hub ops then end with
// outcome unknown and the local socket keeps serving (AD-12).
func (l *Link) Context() context.Context { return l.ctx }

// LinkHandler serves one [Link]. It runs in its own goroutine and must return
// soon after the link's context ends.
type LinkHandler func(l *Link)

// ConnState is the connector's state.
type ConnState string

// The connector states.
const (
	// ConnIdle: nothing to connect to, because no Hub URL is configured or
	// the Worker is not enrolled. The connector waits and keeps running.
	ConnIdle ConnState = "idle"
	// ConnConnecting: dialing, handshaking or waiting for the Hub's answer.
	ConnConnecting ConnState = "connecting"
	// ConnConnected: welcomed and serving.
	ConnConnected ConnState = "connected"
	// ConnBackoff: waiting before the next attempt.
	ConnBackoff ConnState = "backoff"
)

// Status is a snapshot of the connector for `minion worker status`.
type Status struct {
	State ConnState
	// Reason says why the connector is idle ("no Hub configured", "not enrolled").
	Reason string
	// LastError is the last failure, empty after a successful connection.
	LastError   string
	LastErrorAt time.Time
	// ConnectedSince is set while connected.
	ConnectedSince time.Time
	// Attempts counts connection attempts since start.
	Attempts uint64
	// Welcome is the welcome of the current connection (zero unless connected).
	Welcome HubWelcome
	// Alarm is "hub_identity_changed" while the Hub answers with a root the
	// pin does not accept; cleared by a connection.
	Alarm string
	// TLSPath and TLSLine are the TLS path state and text of the latest
	// connection (TLSPath*); empty before the first one.
	TLSPath string
	TLSLine string
}

// Hub link states of [Connector.HubLink].
const (
	HubLinkConnected           = "connected"
	HubLinkRegistrationPending = "registration_pending"
	HubLinkDisconnected        = "disconnected"
	HubLinkNone                = "none"
)

// HubLink is the coarse usability of the Hub connection: connected, waiting
// for the local usage registration (the Hub answers but the link is closed
// until the owner accepts it) or for the Hub's approval of the Worker (a
// welcomed connection that refuses operations), disconnected, or none (Solo:
// no Hub is configured).
func (c *Connector) HubLink() string {
	if c.cfg.HubURL == "" {
		return HubLinkNone
	}
	st := c.Status()
	if st.State == ConnConnected && st.Welcome.Pending {
		return HubLinkRegistrationPending
	}
	if st.State == ConnConnected {
		return HubLinkConnected
	}
	if c.regPending.Load() && c.regGenIsCurrent() {
		return HubLinkRegistrationPending
	}
	return HubLinkDisconnected
}

// regGenIsCurrent reports whether the enrollment the pending registration
// belongs to is still the stored one: a wipe or a new enrollment ends it.
func (c *Connector) regGenIsCurrent() bool {
	if c.cfg.Enrollment == nil {
		return true
	}
	cur, _ := c.cfg.Enrollment.Watch()
	return cur == c.regGen.Load()
}

// ReasonNotEnrolled is the Status.Reason of an idle connector without an
// enrollment.
const ReasonNotEnrolled = "not enrolled"

// ReasonNoHubConfigured is the idle reason for a Worker running in Solo mode.
const ReasonNoHubConfigured = "no Hub configured"

// ReasonQuarantined is the Status.Reason of an idle connector whose stored
// hub_check is mismatch: it does not connect.
const ReasonQuarantined = "the Hub does not match the fingerprint you gave; see minion worker verify-hub"

// AlarmHubIdentityChanged is the value of Status.Alarm while the Hub answers
// with a root key that the pin does not accept.
const AlarmHubIdentityChanged = "hub_identity_changed"

// ConnectorConfig configures a [Connector]. Dialer, Enrollment and Key are
// required.
type ConnectorConfig struct {
	ConsumeUsageIntent     func(context.Context, string, string, string, ed25519.PublicKey, int) error
	UsageConsent           func(context.Context, string) string
	UsageConnectionAllowed func(context.Context, string) (bool, error)
	// OnWelcomeStored, when set, is called after an authenticated welcome was
	// stored and before the usage registration decides whether the link is
	// served: the read-back receiver drops a document of another owner there.
	OnWelcomeStored func(w HubWelcome, hubPub ed25519.PublicKey)
	// OnHubPinChanged, when set, is called after a different verified Hub pin
	// was committed, before the reply is handled: what was published for the
	// previous pin is withdrawn.
	OnHubPinChanged func()
	// HubURL is the configured Hub URL; empty means the Worker runs alone and
	// the connector stays idle.
	HubURL string
	// Name is the Worker's display name for the hello.
	Name string
	// Ops returns the operations to announce. It is called for every attempt,
	// so ops that later modules register are announced too. Nil announces none.
	Ops func() []string
	// Features returns the proto.Feature* names to announce, called for every
	// attempt like Ops. Nil announces none, which is what a Worker without the
	// safeguards says (the Hub then refuses the requests that rely on them).
	Features func() []string
	// Facts returns the display facts of the hello; nil sends none.
	Facts func() transport.WorkerFacts
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Key returns the Worker's identity key; it is called for every attempt.
	// An error is retried with the backoff (the connector never creates a key
	// and never deletes anything).
	Key func() (*identity.Key, error)
	// Out is the console for the messages a person must act on (the
	// approval box); nil prints nothing there.
	Out io.Writer
	// Enrollment is the pin and enrollment state.
	Enrollment *Enrollments
	// Policy receives the Hub policy; nil ignores it.
	Policy PolicySink
	// Dialer is the transport.
	Dialer Dialer
	// Clock is the time source of the backoff and the idle wait; nil means
	// the system clock. It also arms the expiry timer and tick of a connection.
	Clock transport.Clock
	// WallMono supplies the wall and the monotonic clock of the certificate
	// window and of the expiry checks; nil means the system clocks. Tests move
	// the two independently.
	WallMono transport.WallMono
	// Backoff produces the reconnect delays (AD-3: capped at 60 s, never
	// gives up); nil means the default sequence.
	Backoff *transport.Backoff
	// Session tunes the yamux session.
	Session transport.SessionOptions
	// Pause is the pause switch (minion worker pause, tray W3b); nil never
	// pauses. While it is set the connector does not dial: it idles with
	// ReasonPaused, and a change wakes its backoff, idle and connection waits.
	Pause *PauseGate
	// IdlePoll, HandlerGrace and EpochPoll override the defaults.
	IdlePoll     time.Duration
	HandlerGrace time.Duration
	EpochPoll    time.Duration
	// Logger receives operational messages: never secrets, terminal content or
	// message bodies (AD-20). Nil discards them.
	Logger *slog.Logger
}

// Connector keeps the Worker connected to its Hub forever (R4, R78, NFR5): it
// dials out in connect mode with the stored pin, retries with the AD-3
// backoff without any time limit, and reconnects after every loss and every
// restart. Nothing on the channel expires, so a Worker that was offline for a
// year reconnects like one that was offline for a second (AD-6, R94). It
// deletes nothing, except on a valid signed revoke (AD-9).
//
// The connector is a state machine over three ports ([Dialer],
// [EnrollmentRepo] through [Enrollments], [PolicySink]) and a [transport.Clock],
// so that it is unit-tested without a network and without waiting.
type Connector struct {
	cfg        ConnectorConfig
	usageShown atomic.Bool
	// regPending is true while the latest welcomed connection was closed by
	// the usage registration gate (A72).
	regPending atomic.Bool
	// regGen is the enrollment generation regPending was set for.
	regGen atomic.Uint64
	// pendingShown makes the approval box appear once per process, not on
	// every reconnect.
	connectedShown atomic.Bool // the connected box is shown once per process (A61)
	pendingShown   atomic.Bool
	log            *slog.Logger
	clk            transport.Clock
	bo             *transport.Backoff

	mu       sync.Mutex
	status   Status
	handlers []namedHandler
	cur      *running
	lastLog  string // last failure that was logged at Warn, to keep a long outage quiet
	idleLog  string // last idle reason that was logged
	seenGen  uint64 // enrollment generation whose policy was last applied
	// alarmShown makes box M5 appear once per streak of identity refusals.
	alarmShown bool
	// lastTLSPath is the TLS path state of the previous connection of this
	// process ("" before the first).
	lastTLSPath string
}

type namedHandler struct {
	name string
	h    LinkHandler
}

// running is the link that is being served.
type running struct {
	link   *Link
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
}

// facts returns the display facts of the hello, or none.
func (c *Connector) facts() transport.WorkerFacts {
	if c.cfg.Facts == nil {
		return transport.WorkerFacts{}
	}
	return c.cfg.Facts()
}

// factsFor returns the facts of a connection of the enrollment enr: the
// configured facts and how this Worker checked the Hub (hub_check).
func (c *Connector) factsFor(enr Enrollment) transport.WorkerFacts {
	f := c.facts()
	f.HubCheck = enr.Check()
	if c.cfg.UsageConsent != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		f.UsageConsent = c.cfg.UsageConsent(ctx, enr.WorkerID)
		cancel()
	}
	return f
}

// notePending reports whether the approval box is still to be shown.
func (c *Connector) notePending() bool { return c.pendingShown.CompareAndSwap(false, true) }

// NewConnector validates cfg and returns a connector. [Connector.Run] starts it.
func NewConnector(cfg ConnectorConfig) (*Connector, error) {
	switch {
	case cfg.Dialer == nil:
		return nil, errors.New("agent: the connector needs a dialer")
	case cfg.Enrollment == nil:
		return nil, errors.New("agent: the connector needs the enrollment state")
	case cfg.Key == nil:
		return nil, errors.New("agent: the connector needs a key source")
	}
	if cfg.IdlePoll <= 0 {
		cfg.IdlePoll = DefaultIdlePoll
	}
	if cfg.HandlerGrace <= 0 {
		cfg.HandlerGrace = DefaultHandlerGrace
	}
	if cfg.EpochPoll <= 0 {
		cfg.EpochPoll = DefaultEpochPoll
	}
	c := &Connector{cfg: cfg, log: cfg.Logger, clk: cfg.Clock, bo: cfg.Backoff}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.clk == nil {
		c.clk = transport.SystemClock
	}
	if c.bo == nil {
		c.bo = &transport.Backoff{}
	}
	c.status.State = ConnIdle
	return c, nil
}

// Status returns a snapshot of the connector.
func (c *Connector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// OnLink registers a handler that serves every welcomed connection, now and
// after each reconnect. When a connection is already up the handler starts for
// it at once. The name is used in logs.
func (c *Connector) OnLink(name string, h LinkHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append(c.handlers, namedHandler{name: name, h: h})
	if c.cur != nil {
		c.startHandler(c.cur, namedHandler{name: name, h: h})
	}
}

// startHandler runs h for r. The caller holds c.mu, which is what keeps
// wg.Add before the wg.Wait of the end of the link.
func (c *Connector) startHandler(r *running, nh namedHandler) {
	if r.link.Welcome.Pending && !servesWhilePending(nh.name) {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				c.log.Error("hub link handler panicked", "handler", nh.name, "panic", fmt.Sprint(p))
			}
		}()
		nh.h(r.link)
	}()
}

// servesWhilePending says which link handlers run for a Worker that the Hub has
// not approved yet (F-02). The Hub registers no event or turn channel for it and
// refuses what it opens, so only the handlers that wait for the Hub (the op
// server) and the call client (whose calls the Hub answers with worker_pending,
// and which a retirement uses) run. The approval ends the connection, and the
// reconnect starts everything.
func servesWhilePending(name string) bool { return name != "events" && name != "turns" }

// applyStoredPolicy applies the Hub policy of the last welcome before the
// first connection: a restriction the Hub delivered stays in force while the
// Hub is unreachable and after a restart, and the next welcome replaces it.
func (c *Connector) applyStoredPolicy(ctx context.Context) error {
	if c.cfg.Policy == nil {
		return nil
	}
	enr, ok, err := c.cfg.Enrollment.Load(ctx)
	if err != nil {
		return err
	}
	c.cfg.Policy.SetHubPreviewOnly(ok && enr.Policy.PreviewOnly)
	c.setWorkerID(ok, enr.WorkerID)
	return nil
}

// setWorkerID tells the policy the id the Hub gave this Worker, when the policy
// keeps one (*Policy does); "" for a Worker that is not enrolled.
func (c *Connector) setWorkerID(enrolled bool, id string) {
	if s, ok := c.cfg.Policy.(interface{ SetWorkerID(string) }); ok {
		if !enrolled {
			id = ""
		}
		s.SetWorkerID(id)
	}
	if !enrolled {
		c.setOwnerID("")
	}
}

// setOwnerID tells the policy the stable id of this Worker's owner, from the
// last welcome ("" when the Worker is not enrolled). It is connection state and
// is never stored: a Worker that has not been welcomed knows no owner, so
// pipe_accept_from=owner-workers accepts nobody until then.
func (c *Connector) setOwnerID(id string) {
	if s, ok := c.cfg.Policy.(interface{ SetOwnerID(string) }); ok {
		s.SetOwnerID(id)
	}
}

// Run connects and reconnects until ctx ends, then closes the connection and
// returns. It never gives up.
func (c *Connector) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if wait := c.cycle(ctx); wait {
			c.setState(ConnBackoff)
			d := c.bo.Next()
			c.log.Debug("hub connector backoff", "delay", d.String())
			_, pauseCh := c.cfg.Pause.Paused()
			select {
			case <-c.clk.After(d):
			case <-pauseCh:
				// A pause (or resume) during the backoff takes effect at once.
			case <-ctx.Done():
				return
			}
		}
	}
}

// errPaused is the cause of the context of an attempt or connection that a
// pause ended.
var errPaused = errors.New("agent: the Worker was paused")

// cycle runs one pass of the state machine and reports whether the backoff
// wait follows.
//
// The generation, its change channel and the enrollment are taken as one
// snapshot, so that a commit or wipe after the read is never missed: it ends
// the idle wait and drops the attempt or connection that relies on the old
// enrollment. The generation is shared with other processes, so a re-pairing
// by `minion worker pair --new-code` ends the connection within the epoch poll
// interval, and the old Hub's revoke can no longer delete the replacement.
func (c *Connector) cycle(ctx context.Context) (backoff bool) {
	gen, moved, enr, enrolled, err := c.cfg.Enrollment.Snapshot(ctx)
	if err != nil {
		c.fail("cannot read the enrollment", err)
		return true
	}
	c.syncPolicy(gen, enr, enrolled)
	paused, pauseCh := c.cfg.Pause.Paused()
	switch {
	case paused:
		// No Hub traffic at all while paused, whatever else holds.
		c.idle(ctx, ReasonPaused, moved, pauseCh)
		return false
	case c.cfg.HubURL == "":
		c.idle(ctx, ReasonNoHubConfigured, moved, pauseCh)
		return false
	case !enrolled:
		c.idle(ctx, ReasonNotEnrolled, moved, pauseCh)
		return false
	case enr.Check() == store.HubCheckMismatch:
		// The stored quarantine: the operator said this is not the Hub to
		// trust. Leaving it needs a person (verify-hub, pairing again, a
		// reset, or a setting that matches).
		c.idle(ctx, ReasonQuarantined, moved, pauseCh)
		return false
	}
	c.clearIdleLog()
	// The attempt is admitted under the pause gate before anything else (the
	// key load included): a pause that follows waits for it to end, and one
	// that came first refuses it.
	wctx, endWork, ok := c.cfg.Pause.beginWork(ctx)
	if !ok {
		return false
	}
	defer endWork()
	key, err := c.cfg.Key()
	if err != nil {
		c.fail("cannot load the Worker key", err)
		return true
	}

	actx, cancel := context.WithCancelCause(wctx)
	defer cancel(nil)
	go func() {
		// A real ticker, not the injected clock: this is a check against another
		// process, which no fake clock can advance.
		tick := time.NewTicker(c.cfg.EpochPoll)
		defer tick.Stop()
		for {
			select {
			case <-moved:
				cancel(errEnrollmentChanged)
				return
			case <-pauseCh:
				if p, _ := c.cfg.Pause.Paused(); p {
					cancel(errPaused)
					return
				}
				_, pauseCh = c.cfg.Pause.Paused()
			case <-tick.C:
				if c.cfg.Enrollment.Moved(gen) {
					cancel(errEnrollmentChanged)
					return
				}
			case <-actx.Done():
				return
			}
		}
	}()
	backoff = c.attempt(actx, gen, enr, key)
	if ctx.Err() == nil && errors.Is(context.Cause(actx), errPaused) {
		c.log.Info("hub connection ended: the Worker is paused")
		return false
	}
	if ctx.Err() == nil && errors.Is(context.Cause(actx), errEnrollmentChanged) {
		c.log.Debug("hub connector: the enrollment changed; starting over", "enrollment_generation", gen)
		// Start over with the new enrollment at once, without a backoff.
		return false
	}
	return backoff
}

// syncPolicy applies the stored Hub policy when the enrollment was committed
// or wiped since the last time: a reset must not leave the old Hub's
// restriction in force, and a re-enrollment brings its own. Between such
// changes the policy is left alone, so that a restriction that could not be
// stored yet is kept (see [Connector.storeWelcome]).
func (c *Connector) syncPolicy(gen uint64, enr Enrollment, enrolled bool) {
	if c.cfg.Policy == nil {
		return
	}
	c.mu.Lock()
	changed := gen != c.seenGen
	c.seenGen = gen
	c.mu.Unlock()
	if changed {
		c.cfg.Policy.SetHubPreviewOnly(enrolled && enr.Policy.PreviewOnly)
		c.setWorkerID(enrolled, enr.WorkerID)
	}
}

// idle records the idle state and waits for a change of the enrollment, a
// poll interval or the end of ctx.
func (c *Connector) idle(ctx context.Context, reason string, moved, pauseCh <-chan struct{}) {
	c.mu.Lock()
	c.status.State, c.status.Reason = ConnIdle, reason
	first := c.idleLog != reason
	c.idleLog = reason
	c.mu.Unlock()
	if first {
		c.log.Info("hub connector idle", "reason", reason)
	}
	select {
	case <-moved:
	case <-pauseCh:
	case <-c.clk.After(c.cfg.IdlePoll):
	case <-ctx.Done():
	}
}

func (c *Connector) clearIdleLog() {
	c.mu.Lock()
	c.idleLog = ""
	c.mu.Unlock()
}

func (c *Connector) setState(s ConnState) {
	c.mu.Lock()
	c.status.State = s
	c.status.Reason = ""
	if s != ConnConnected {
		c.status.ConnectedSince = time.Time{}
		c.status.Welcome = HubWelcome{}
	}
	c.mu.Unlock()
}

// fail records a failed attempt. The first failure of a streak is logged at
// Warn, repeats of the same failure at Debug, so that a Hub that is down for a
// week does not fill the log.
// The error text never carries a URL's credential, path or query (errString).
func (c *Connector) fail(what string, err error) { c.failMsg(what + ": " + errString(err)) }

func (c *Connector) failMsg(msg string) {
	c.mu.Lock()
	c.status.LastError, c.status.LastErrorAt = msg, c.clk.Now()
	repeat := c.lastLog == msg
	c.lastLog = msg
	c.mu.Unlock()
	if repeat {
		c.log.Debug("hub connection failed again", "error", msg)
		return
	}
	c.log.Warn("cannot connect to the hub; retrying with backoff", "error", msg)
}

// attempt makes one connection attempt and serves it. It reports whether the
// backoff wait follows (false only after a valid revoke, which leaves nothing
// to connect to).
func (c *Connector) attempt(ctx context.Context, gen uint64, enr Enrollment, key *identity.Key) (backoff bool) {
	// The gate is read again right before the dial: a pause that came while
	// the key loaded is final, whether or not its cancellation has arrived.
	if paused, _ := c.cfg.Pause.Paused(); paused {
		return false
	}
	c.mu.Lock()
	c.status.State, c.status.Reason = ConnConnecting, ""
	c.status.Attempts++
	n := c.status.Attempts
	c.mu.Unlock()
	c.log.Debug("hub connector dialing", "hub", hubLabel(c.cfg.HubURL), "attempt", n,
		"enrolled", true, "worker_id", enr.WorkerID, "enrollment_generation", gen, "pin_chain_position", enr.Pin.Seq)

	att, err := c.cfg.Dialer.Connect(ctx, ConnectParams{
		HubURL:    c.cfg.HubURL,
		Key:       key,
		Name:      c.cfg.Name,
		Ops:       c.ops(),
		Features:  c.features(),
		Facts:     c.factsFor(enr),
		Protocols: c.cfg.Protocols,
		Pin:       enr.Pin,
		Ratchet:   enr.Ratchet,
		Clock:     c.cfg.WallMono,
		Session:   c.cfg.Session,
		Logger:    c.log,
	})
	if err != nil {
		if ctx.Err() == nil {
			var re *transport.RefusalError
			if errors.As(err, &re) && (re.Reason == transport.ReasonCertSerialTooLow || re.Reason == transport.ReasonCertSerialConflict) {
				c.refuseSerial(re)
				return true
			}
			var hk *transport.HubKeyError
			if errors.As(err, &hk) && (errors.Is(err, transport.ErrPinMismatch) || errors.Is(err, transport.ErrChainInvalid)) {
				c.identityChanged(enr, hk, err)
			}
			c.fail("connect", err)
		}
		return true
	}
	c.log.Debug("hub handshake done", "reply", att.Reply.Type, "protocol", att.Version.String(),
		"pin_changed", att.PinChanged, "ratchet_changed", att.RatchetChanged)
	if att.Reply.Type == proto.TypeRevoke {
		return c.onRevoke(ctx, gen, att)
	}
	closeSession := func() {
		if att.Session != nil {
			_ = att.Session.Close()
		}
	}
	// Nothing an answer at or after the certificate's not_after says is
	// committed (section 11.3): both clocks are read now, and again inside the
	// guard of every write below, immediately before it.
	live := c.liveFunc(att.Expiry)
	if !live() {
		closeSession()
		c.expiredAnswer()
		return true
	}
	// The Hub is authenticated (pin after its transition chain, certificate,
	// sig_hub). The pin the Hub proved and the certificate ratchet are stored in
	// one transaction before anything relies on this connection (step 8), also
	// when only the serial rose.
	if att.PinChanged || att.RatchetChanged {
		if err := c.cfg.Enrollment.CommitRatchetIfLive(ctx, gen, att.Pin, att.Ratchet, live, att.PinPassed...); err != nil {
			closeSession()
			if errors.Is(err, ErrEnrollmentChanged) {
				return false
			}
			if errors.Is(err, errGuardExpired) {
				c.expiredAnswer()
				return true
			}
			c.fail("cannot store the new Hub pin", err)
			return true
		}
		if att.PinChanged {
			c.log.Info("hub key pin advanced along the transition chain", "chain_position", att.Pin.Seq)
			// A document bound to the previous pin goes now, whatever the
			// Hub's reply is (a restored Hub may answer not_enrolled).
			if c.cfg.OnHubPinChanged != nil {
				c.cfg.OnHubPinChanged()
			}
		}
	}
	switch att.Reply.Type {
	case proto.TypeWelcome:
		if att.Session == nil {
			c.fail("connect", errors.New("welcome without a session"))
			return true
		}
		return c.serve(ctx, gen, att)
	case proto.TypeNotEnrolled:
		// Non-destructive (AD-9): the Hub does not know this key, for example
		// after it restored an older backup. Keep everything and retry.
		closeSession()
		c.failMsg("not_enrolled: the hub does not know this Worker; the pin and the enrollment are kept")
		return true
	default:
		closeSession()
		c.fail("connect", fmt.Errorf("unexpected control frame %q", att.Reply.Type))
		return true
	}
}

// identityChanged reports a Hub that answers with a root key the pin does not
// accept: alarm state, one Error log line and box M5 once per streak. The pin
// is kept and the connector keeps retrying; nothing is changed.
func (c *Connector) identityChanged(enr Enrollment, hk *transport.HubKeyError, err error) {
	c.mu.Lock()
	c.status.Alarm = AlarmHubIdentityChanged
	first := !c.alarmShown
	c.alarmShown = true
	c.mu.Unlock()
	if !first {
		return
	}
	reason := "It is another key, and the Hub showed no valid record of a master key rotation from the paired key to it."
	if errors.Is(err, transport.ErrChainInvalid) {
		reason = "The Hub sent a record of a master key rotation that does not verify."
	}
	paired, answered := fingerprintOfPub(enr.Pin.Pub), fingerprintOfPub(hk.Presented)
	c.log.Error("hub identity changed; not connecting", "hub", hubLabel(c.cfg.HubURL),
		"paired_with", string(paired), "paired_with_words", paired.Words(),
		"answered", string(answered), "answered_words", answered.Words(), "reason", reason)
	lines := []string{
		hubLabel(c.cfg.HubURL) + " answered with a Hub key that this",
		"Worker does not accept.",
	}
	lines = append(lines, boxField("   paired with:", paired.Words())...)
	lines = append(lines, fullIndent+string(paired))
	lines = append(lines, boxField("   answered now:", answered.Words())...)
	lines = append(lines, fullIndent+string(answered))
	lines = append(lines, boxWrap(reason, boxWidth)...)
	lines = append(lines,
		"Nothing was changed. The Worker keeps its pairing, keeps",
		"retrying and connects only to the Hub it is paired with.",
		"Read the fingerprint on the Hub host:",
		"    minion hub identity --words",
		"- It shows the \"paired with\" words: something between this",
		"  computer and the Hub answers in its place. Check the proxy",
		"  and the network. Do nothing on the Worker.",
		"- It shows other words because the Hub was reinstalled or",
		"  restored with another identity: pair again with",
		"  minion worker pair --new-code --hub-fingerprint <value>",
	)
	writeConsoleBox(c.cfg.Out, "HUB IDENTITY CHANGED - NOT CONNECTING", lines...)
}

// features returns the features to announce.
func (c *Connector) features() []string {
	if c.cfg.Features == nil {
		return nil
	}
	return c.cfg.Features()
}

// ops returns the operations to announce.
func (c *Connector) ops() []string {
	if c.cfg.Ops == nil {
		return nil
	}
	return c.cfg.Ops()
}

// onRevoke handles a revoke. Only a revoke that the pinned Hub key signed for
// this connection's transcript wipes the pairing (AD-9); anything else is
// ignored and logged, and the Worker keeps its state and retries.
func (c *Connector) onRevoke(ctx context.Context, gen uint64, att *Attempt) (backoff bool) {
	if att.Session != nil {
		_ = att.Session.Close()
	}
	if att.Expiry != nil && att.Expiry.Expired(c.wallMono()) {
		// A revoke at or after the cut-off is not applied; the pairing stays.
		c.log.Warn("ignored a revoke that arrived at or after the Hub certificate's not_after; nothing was deleted")
		c.failMsg("ignored a revoke after the Hub certificate expired")
		return true
	}
	var live func() bool
	if att.Expiry != nil {
		exp, clk := *att.Expiry, c.wallMono()
		live = func() bool { return !exp.Expired(clk) }
	}
	_, backoff = c.applyRevoke(ctx, gen, att.OpPub, att.TranscriptHash, att.Reply.RevokeSig, live)
	return backoff
}

// liveFunc returns the expiry check of a connection: true while the Hub
// certificate has not reached its end on either clock. A nil Expiry (a test
// double) is always live.
func (c *Connector) liveFunc(exp *transport.Expiry) func() bool {
	if exp == nil {
		return func() bool { return true }
	}
	e, clk := *exp, c.wallMono()
	return func() bool { return !e.Expired(clk) }
}

// expiredAnswer records an answer of the Hub that is not applied because the
// certificate of its connection ended first; nothing was stored.
func (c *Connector) expiredAnswer() {
	c.log.Warn("ignored the Hub's answer: its certificate reached not_after before the answer was applied; nothing was stored")
	c.failMsg("ignored an answer after the Hub certificate expired")
}

func (c *Connector) wallMono() transport.WallMono {
	if c.cfg.WallMono != nil {
		return c.cfg.WallMono
	}
	return transport.SystemWallMono
}

// refuseSerial records a refusal of a Hub certificate by the serial ratchet
// (section 6): one local line with the certificate's serial, this Worker's
// max_serial and the exact command. Nothing is stored and nothing is sent to the
// Hub.
func (c *Connector) refuseSerial(re *transport.RefusalError) {
	msg := re.Error()
	c.mu.Lock()
	c.status.LastError, c.status.LastErrorAt = msg, c.clk.Now()
	repeat := c.lastLog == msg
	c.lastLog = msg
	c.mu.Unlock()
	if repeat {
		c.log.Debug("hub certificate refused again", "reason", re.Reason)
		return
	}
	c.log.Warn("refused the Hub certificate: its serial is behind what this Worker has already seen; nothing was stored",
		"reason", re.Reason, "cert_serial", re.Serial, "max_serial", re.MaxSerial, "command", re.Command())
}

// applyRevoke checks a revoke against the pinned Hub key and the transcript of
// the connection it arrived on, and wipes the pairing when it is valid. It is
// shared by the revoke that answers a connect and the one that arrives over a
// live connection ([Connector.HandleRevoke]). It reports whether the backoff
// wait follows: false after a wipe or a replaced enrollment, true when the
// revoke was ignored or the wipe failed. wiped is true only when this call
// deleted the pairing.
//
// live, when not nil, is checked inside the enrollment guard right before the
// wipe (after the signature check, any acknowledgement and the wait for the
// lock): a revoke at or after the certificate's not_after is not applied.
func (c *Connector) applyRevoke(ctx context.Context, gen uint64, opPub ed25519.PublicKey, transcript, sig []byte, live func() bool) (wiped, backoff bool) {
	if err := identity.Verify(opPub, identity.PurposeRevoke, transcript, sig); err != nil {
		c.log.Warn("ignored a revoke that the operational key of this connection did not sign for it; nothing was deleted")
		c.mu.Lock()
		c.status.LastError, c.status.LastErrorAt = "ignored an invalid revoke", c.clk.Now()
		c.mu.Unlock()
		return false, true
	}
	if _, err := c.cfg.Enrollment.WipeIfLive(ctx, gen, live); err != nil {
		if errors.Is(err, errGuardExpired) {
			c.log.Warn("ignored a revoke that reached the wipe at or after the Hub certificate's not_after; nothing was deleted")
			c.failMsg("ignored a revoke after the Hub certificate expired")
			return false, true
		}
		if errors.Is(err, ErrEnrollmentChanged) {
			// The enrollment this Hub revoked is gone already (a reset or a
			// re-enrollment): the replacement is not this Hub's to delete.
			c.log.Info("ignored a revoke for an enrollment that was replaced meanwhile; nothing was deleted")
			return false, false
		}
		c.fail("cannot wipe the enrollment after a revoke", err)
		return false, true
	}
	if c.cfg.Policy != nil {
		c.cfg.Policy.SetHubPreviewOnly(false)
		c.setWorkerID(false, "")
	}
	c.mu.Lock()
	c.status.LastError, c.status.LastErrorAt = "", time.Time{}
	c.lastLog = ""
	c.mu.Unlock()
	c.log.Info("removed by the hub: the pin and the enrollment were wiped; the sessions are untouched")
	return true, false
}

// serve runs a welcomed connection until it ends. It reports true: the
// backoff wait always follows a connection, so that a Hub that accepts and
// drops at once is not hammered.
func (c *Connector) serve(ctx context.Context, gen uint64, att *Attempt) (backoff bool) {
	w := att.Reply.Welcome
	if w.WorkerID == "" {
		_ = att.Session.Close()
		c.fail("connect", errors.New("the welcome has no worker id"))
		return true
	}
	if err := c.storeWelcome(ctx, gen, w, c.liveFunc(att.Expiry)); err != nil {
		_ = att.Session.Close()
		if errors.Is(err, ErrEnrollmentChanged) {
			return false
		}
		if errors.Is(err, errGuardExpired) {
			c.expiredAnswer()
			return true
		}
		// The connection is not accepted as healthy before the welcome is
		// stored: the retry goes through the backoff, with the new
		// restriction already in force.
		c.fail("cannot store the welcome data", err)
		return true
	}
	if c.cfg.OnWelcomeStored != nil {
		c.cfg.OnWelcomeStored(w, att.Pin.Pub)
	}
	if c.cfg.ConsumeUsageIntent != nil {
		if err := c.cfg.ConsumeUsageIntent(ctx, c.cfg.HubURL, w.WorkerID, w.Owner, att.Pin.Pub, w.UsagePolicy); err != nil {
			_ = att.Session.Close()
			c.fail("cannot apply local usage enrollment", err)
			return true
		}
	}
	if w.UsagePolicy > 0 && c.cfg.UsageConnectionAllowed != nil {
		allowed, checkErr := c.cfg.UsageConnectionAllowed(ctx, w.WorkerID)
		if checkErr != nil || !allowed || w.UsagePolicy != ops.UsageVersion {
			_ = att.Session.Close()
			msg := "Usage registration pending. Run minion worker config usage on this Worker to review the Hub's monitoring policy and accept this enrollment."
			if w.UsagePolicy != ops.UsageVersion {
				msg = "The Hub requires a newer usage registration policy; update this Worker."
			}
			c.regGen.Store(gen)
			c.regPending.Store(true)
			if c.usageShown.CompareAndSwap(false, true) {
				lines := strings.SplitAfter(ops.UsageDisclosure, ". ")
				writeConsoleBox(c.cfg.Out, "MINION WORKER REGISTRATION", append(lines, msg)...)
			}
			c.failMsg(msg)
			return true
		}
		c.usageShown.Store(false)
	}
	c.regPending.Store(false)
	c.bo.Reset()

	// The link context does not inherit ctx's cancellation, so that its cause
	// is always ErrLinkLost (or the stop cause), never the cause of a dropped
	// attempt.
	base := context.WithoutCancel(ctx)
	var guard *expiryGuard
	sess := att.Session
	if att.Expiry != nil {
		guard = &expiryGuard{exp: *att.Expiry, clk: c.wallMono(), log: c.log, onExpire: func() { _ = att.Session.Close() }}
		base = withGuard(base, guard)
		sess = guardedSession{Session: att.Session, g: guard}
	}
	linkCtx, cancel := context.WithCancelCause(base)
	r := &running{link: &Link{
		Welcome: w, Version: att.Version, Session: sess, ctx: linkCtx,
		pin: att.Pin, transcript: att.TranscriptHash, gen: gen, opPub: att.OpPub,
	}, cancel: cancel}
	if guard != nil {
		go guard.watch(linkCtx, c.clk)
		if guard.check() {
			// Accepted at exactly not_after: the connection admits nothing.
			c.log.Info("the Hub certificate expires now; no operation is admitted")
		}
	}
	tlsState, tlsLine := TLSPath(att.TLS, w.HubTLS)
	c.mu.Lock()
	c.status.State, c.status.Reason = ConnConnected, ""
	c.status.ConnectedSince, c.status.Welcome = c.clk.Now(), w
	c.status.LastError, c.status.LastErrorAt = "", time.Time{}
	c.status.Alarm, c.alarmShown = "", false
	c.status.TLSPath, c.status.TLSLine = tlsState, tlsLine
	prevTLS := c.lastTLSPath
	c.lastTLSPath = tlsState
	c.lastLog = ""
	c.cur = r
	for _, nh := range c.handlers {
		c.startHandler(r, nh)
	}
	c.mu.Unlock()
	// A61: the Hub key words stand in the box, not only in a log line: they
	// are compared by eye with the key in the Hub's top bar.
	hubKey := func() []string {
		f, err := identity.FingerprintOf(att.Pin.Pub)
		if err != nil {
			return nil
		}
		return append([]string{"Check that the Hub's top bar shows this Hub key:"}, fingerprintLines("Hub key:", f)...)
	}
	if w.Pending && c.notePending() {
		lines := hubKey()
		lines = append(lines, "",
			"This Worker is waiting for approval in the Hub: "+hubLabel(c.cfg.HubURL),
			"The owner or an admin approves it on the Workers page",
			"(or: minion hub worker approve). Approve only if the Hub shows:",
			"",
		)
		if k, err := c.cfg.Key(); err == nil {
			lines = append(lines, fingerprintLines("Worker fingerprint:", k.Fingerprint())...)
		}
		writeConsoleBox(c.cfg.Out, "MINION WORKER WAITING FOR APPROVAL", lines...)
	} else if !w.Pending && c.connectedShown.CompareAndSwap(false, true) {
		lines := hubKey()
		lines = append(lines, "", "Connected to "+hubLabel(c.cfg.HubURL)+" as "+w.Name+" (owner "+w.Owner+").")
		writeConsoleBox(c.cfg.Out, "MINION WORKER CONNECTED", lines...)
	}
	if w.Pending {
		c.log.Warn("this Worker is waiting for approval: ask its owner or an admin to approve it in the Hub (Workers page, or `minion hub worker approve`); until then the Hub sends it no operations",
			"worker_id", w.WorkerID, "name", w.Name)
	}
	c.log.Info("connected to hub",
		"hub", hubLabel(c.cfg.HubURL), "hub_fingerprint", hubFingerprint(att.Pin),
		"worker_id", w.WorkerID, "name", w.Name, "owner", w.Owner, "kind", w.Kind,
		"preview_only", w.Policy.PreviewOnly, "protocol", att.Version.String())
	if att.TLS.Seen() && (prevTLS == "" || prevTLS != tlsState) {
		c.log.Info("hub connection", "tls_path", tlsState, "tls_name", att.TLS.Name, "tls_issuer", att.TLS.Issuer,
			"tls_trusted", att.TLS.Trusted, "tls", tlsLine)
		if prevTLS != "" {
			c.log.Warn("the TLS path to the Hub changed", "was", prevTLS, "now", tlsState)
		}
	}

	c.log.Debug("hub welcome", "worker_id", w.WorkerID, "pending_approval", w.Pending,
		"protocol", att.Version.String(), "hub_proto_min", w.Min.String(), "hub_proto_max", w.Max.String())
	connectedAt := c.clk.Now()

	select {
	case <-att.Session.Done():
	case <-ctx.Done():
	}
	c.log.Debug("hub connection ended", "reason", causeText(att.Session.Err()),
		"stopping", ctx.Err() != nil, "connected_for", c.clk.Now().Sub(connectedAt).String())
	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
	cause := ErrLinkLost
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errEnrollmentChanged) {
		cause = context.Cause(ctx)
	}
	cancel(cause)
	_ = att.Session.Close()

	switch {
	case errors.Is(context.Cause(ctx), errEnrollmentChanged):
		c.log.Info("the enrollment changed; dropped the connection to the old hub")
	case ctx.Err() == nil:
		c.log.Warn("lost the connection to the hub; reconnecting", "reason", causeText(att.Session.Err()))
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-c.clk.After(c.cfg.HandlerGrace):
		c.log.Warn("hub link handlers did not stop in time", "grace", c.cfg.HandlerGrace.String())
	}
	return true
}

// storeWelcome stores what the welcome said and applies its Hub policy. A
// restriction is enforced in memory before it is stored, and it stays in force
// when storing fails: the Worker never runs less restricted than the Hub asked
// just because SQLite said no (after a restart it would). A lifted restriction
// takes effect only once it is stored, so that memory and disk never disagree
// in the permissive direction.
//
// live is read before anything is applied and again inside the write's guard:
// a welcome at or after the certificate's end changes neither the policy nor
// the stored data (errGuardExpired). The restriction is enforced inside that
// guard, after the generation and expiry checks, so that a welcome that
// expires (or whose enrollment moves) while the write waits for the lock never
// touches the policy.
func (c *Connector) storeWelcome(ctx context.Context, gen uint64, w HubWelcome, live func() bool) error {
	if !live() {
		return errGuardExpired
	}
	var restrict func()
	if c.cfg.Policy != nil && w.Policy.PreviewOnly {
		restrict = func() { c.cfg.Policy.SetHubPreviewOnly(true) }
	}
	if err := c.cfg.Enrollment.ApplyWelcomeIfLive(ctx, gen, w, live, restrict); err != nil {
		return err
	}
	if c.cfg.Policy != nil {
		c.cfg.Policy.SetHubPreviewOnly(w.Policy.PreviewOnly)
		c.setWorkerID(true, w.WorkerID)
		c.setOwnerID(w.OwnerID)
	}
	return nil
}

func causeText(err error) string {
	if err == nil {
		return "closed"
	}
	return errString(err)
}

// hubLabel is the Hub URL without credentials, path or query, for logs.
func hubLabel(raw string) string {
	if s := logx.SafeURL(raw); s != "" && s != "[url]" {
		return s
	}
	return "hub"
}

// hubFingerprint is the fingerprint of the pinned Hub key.
func hubFingerprint(p identity.Pin) string {
	f, err := identity.FingerprintOf(p.Pub)
	if err != nil {
		return ""
	}
	return f.String()
}

// Compile-time checks that the production types fit the ports.
var (
	_ Session    = (*transport.Session)(nil)
	_ PolicySink = (*Policy)(nil)
)
