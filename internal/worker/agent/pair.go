package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Pairing code lifecycle (AD-8, Story 4.7).
//
// The Worker shows a code MN-XXXX-XXXX, connects to the Hub in pair_code mode
// with the public pairing id (the first four characters) and waits. A user
// types the whole code into the Hub, which runs the CPace responder with it.
// The code is the only secret that is ever printed (AD-20): on the console and
// in the log, nowhere else.
const (
	// PairCodeTTL is how long a code is valid, counted from the moment it is
	// shown. The Hub drops a pending pairing after the same time (AD-8).
	PairCodeTTL = 10 * time.Minute
	// pairHandshakeMargin is added to the code lifetime for the handshake
	// deadline, so that the Worker's own expiry (which makes a fresh code)
	// always fires before the transport gives up.
	pairHandshakeMargin = 30 * time.Second
	// pairHeldReset is how long the Hub must have held a connection after the
	// hello for its end to count as progress for the backoff (a proxy idle
	// timeout, for example).
	pairHeldReset = 30 * time.Second
	// maxNewCodeTries bounds the search for a code whose pairing id differs
	// from the previous one.
	maxNewCodeTries = 16
	// commitLockWait bounds the wait for the enrollment lock before the commit
	// of a pairing the Hub already accepted.
	commitLockWait = 2 * time.Minute
)

// errCodeExpired is the cause of the context of an attempt whose code reached
// the end of its lifetime.
var errCodeExpired = errors.New("agent: the pairing code expired")

// ErrUnknownEnrollmentHub is returned when the identity of an enrolled Worker
// is to be replaced but its enrollment recorded no Hub address (an enrollment
// stored by an older version). Revoking the old key at a guessed Hub could
// leave it valid at the real one.
var ErrUnknownEnrollmentHub = errors.New("agent: the enrollment did not record which Hub it belongs to, so its key cannot be revoked there; remove this Worker at its Hub and run `minion worker reset`")

// ErrNoHubURL is returned when pairing is started without a Hub URL.
var ErrNoHubURL = errors.New("agent: no Hub URL is configured; give one with --server or `minion worker join <url>`")

// AlreadyPaired returns the already_paired error that `minion worker pair`
// reports for a paired Worker (AD-8: a paired Worker refuses new codes).
func AlreadyPaired(enr Enrollment) *proto.Error {
	msg := "this Worker is already paired"
	if enr.Owner != "" {
		msg += " to " + enr.Owner
	}
	return proto.NewError(proto.CodeAlreadyPaired, msg+"; use `minion worker pair --new-code` to pair it again with a new identity")
}

// PairParams is everything one pairing connection needs.
type PairParams struct {
	// HubURL is the configured Hub URL (any of https, http, wss, ws).
	HubURL string
	// Key is the Worker's identity key.
	Key *identity.Key
	// Name is the Worker's display name for the hello.
	Name string
	// Ops are the operations the Worker announces (AD-4).
	Ops []string
	// Features are the proto.Feature* names the Worker announces.
	Features []string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Code is the code this connection is about. It is the CPace password.
	Code identity.PairingCode
	// Timeout bounds the handshake, which includes the wait for the user to
	// type the code in the Hub.
	Timeout time.Duration
	// OnWaiting is called once the hello with the public pairing id has been
	// sent: from then on the Hub holds the connection as pending and a user
	// can claim the code. Nil is allowed.
	OnWaiting func()
	// Ratchet is the certificate memory of the pair_resume record (candidate
	// root, serial and hash); its non-empty Root also binds the Hub's root
	// (resume_root_mismatch). Empty for a first pairing.
	Ratchet transport.Ratchet
	// Clock supplies the wall and monotonic clock of the certificate window; nil
	// means the system clocks.
	Clock transport.WallMono
	// OnAuthenticated is called once the Hub is authenticated (certificate,
	// CPace confirmation and sig_hub verified) and before sig_worker is sent.
	// An error aborts the handshake without sig_worker. The pairer writes the
	// pair_resume record here, durably. Nil is allowed.
	OnAuthenticated func(transport.HubAuth) error
	// OnExposed is called right before the Worker sends its CPace
	// confirmation. From then on whoever is at the other end of this
	// connection can learn whether a guess of the code was right, even if it
	// never proves anything itself and answers with an unauthenticated error
	// frame such as pairing_code_invalid. The pairer retires the code of every
	// connection that got this far without pairing. Nil is allowed.
	OnExposed func()
}

// PairResult is the outcome of a successful pairing connection.
type PairResult struct {
	// Pin is the Hub key that CPace confirmation authenticated (position 0
	// of its transition chain).
	Pin identity.Pin
	// Welcome is the Hub's welcome, which names the owner.
	Welcome HubWelcome
	// Version is the protocol version the Hub chose.
	Version proto.Version
	// Ratchet is the certificate memory to commit with the enrollment, scoped
	// to Pin.Pub: the higher of the record's and this session's serial.
	Ratchet transport.Ratchet
	// Expiry is the end of the connection the welcome arrived on; the pairer
	// reads both clocks again right before it commits. Nil means no
	// enforcement (test doubles only).
	Expiry *transport.Expiry
}

// PairDialer is the transport port of pairing: it dials the Hub, runs the
// AD-6 handshake in pair_code mode and reads welcome. The production
// implementation is [NewPairDialer]; tests use a fake. An error from the Hub
// is returned as a *proto.Error. The dial accepts any TLS certificate, because
// trust is the CPace confirmation and never TLS (A-15).
type PairDialer interface {
	Pair(ctx context.Context, p PairParams) (PairResult, error)
}

// NewPairDialer returns the production [PairDialer]: WSS dial that accepts any
// certificate (A-15, no flags), the pair_code handshake whose Hub key is
// authenticated by CPace confirmation, and the welcome frame inside the
// channel. The connection is closed after welcome: the connector reconnects in
// connect mode with the pin that pairing stored.
func NewPairDialer() PairDialer {
	return wssPairDialer{dialTimeout: DefaultDialTimeout}
}

type wssPairDialer struct {
	dialTimeout time.Duration
	// afterReply is a test seam that runs right after the Hub's answer was read.
	afterReply func()
}

func (d wssPairDialer) Pair(ctx context.Context, p PairParams) (PairResult, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, d.dialTimeout)
	conn, err := transport.Dial(dialCtx, p.HubURL)
	cancelDial()
	if err != nil {
		if ctx.Err() == nil && dialCtx.Err() != nil {
			return PairResult{}, fmt.Errorf("dial the hub: no answer within %s", d.dialTimeout)
		}
		return PairResult{}, err
	}
	// The hello is one write; when it went out, the Hub has the pairing id.
	nc := &notifyConn{Conn: conn, onFirstWrite: p.OnWaiting, onConfirm: p.OnExposed}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	res, err := transport.WorkerHandshake(nc, transport.WorkerConfig{
		Key:         p.Key,
		Name:        p.Name,
		Ops:         p.Ops,
		Features:    p.Features,
		Protocols:   p.Protocols,
		Mode:        transport.ModePairCode,
		PairingCode: p.Code,
		Ratchet:     p.Ratchet,
		Clock:       p.Clock,
		Timeout:     p.Timeout,
		BeforeSigWorker: func(a transport.HubAuth) error {
			if a.Expiry.Expired(p.Clock) {
				return errEnrollExpired
			}
			if p.OnAuthenticated != nil {
				if err := p.OnAuthenticated(a); err != nil {
					return err
				}
			}
			// The durable record write can block: both clocks are read again
			// before sig_worker (the transport reads them once more right
			// before it sends the frame).
			if a.Expiry.Expired(p.Clock) {
				return errEnrollExpired
			}
			return nil
		},
	})
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return PairResult{}, context.Cause(ctx)
		}
		return PairResult{}, err
	}
	defer func() { _ = res.Channel.Close() }()
	// The channel is closed at the end of the certificate, and both clocks are
	// read again before the welcome is accepted.
	exp := res.Expiry
	guard, stopGuard := enrollGuard(&exp, p.Clock, func() { _ = res.Channel.Close() })
	defer stopGuard()
	reply, err := readReply(res.Channel)
	if !stop() {
		return PairResult{}, context.Cause(ctx)
	}
	if d.afterReply != nil {
		d.afterReply()
	}
	if guard.check() {
		return PairResult{}, errEnrollExpired
	}
	if err != nil {
		return PairResult{}, err
	}
	if reply.Type != proto.TypeWelcome {
		return PairResult{}, fmt.Errorf("the hub answered %q instead of welcome", reply.Type)
	}
	return PairResult{Pin: res.Pin, Welcome: reply.Welcome, Version: res.Version, Ratchet: res.Ratchet, Expiry: &exp}, nil
}

// notifyConn calls onFirstWrite after the first successful write and onConfirm
// before the handshake frame that carries the Worker's CPace confirmation is
// written (the transport writes every handshake frame in one Write: a uint32
// length and the JSON body).
type notifyConn struct {
	net.Conn
	once         sync.Once
	confirmOnce  sync.Once
	onFirstWrite func()
	onConfirm    func()
}

func (c *notifyConn) Write(b []byte) (int, error) {
	if c.onConfirm != nil && isConfirmFrame(b) {
		c.confirmOnce.Do(c.onConfirm)
	}
	n, err := c.Conn.Write(b)
	if err == nil && c.onFirstWrite != nil {
		c.once.Do(c.onFirstWrite)
	}
	return n, err
}

// isConfirmFrame reports whether b is a raw handshake frame of type
// pake.confirm.
func isConfirmFrame(b []byte) bool {
	if len(b) <= 4 {
		return false
	}
	var head struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(b[4:], &head) == nil && head.Type == proto.TypePakeConfirm
}

// PairerConfig configures a [Pairer]. HubURL, Enrollment and Dialer are
// required.
type PairerConfig struct {
	// HubURL is the Hub to pair with.
	HubURL string
	// Name is the Worker's display name for the hello.
	Name string
	// Ops returns the operations to announce; nil announces none.
	Ops func() []string
	// Features returns the proto.Feature* names to announce; nil announces none.
	Features func() []string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Enrollment holds the Worker key and receives the enrollment.
	Enrollment *Enrollments
	// Dialer is the transport.
	Dialer PairDialer
	// Clock is the time source of the code lifetime and the retry waits; nil
	// means the system clock.
	Clock transport.Clock
	// Backoff produces the delays between attempts; nil means the default
	// sequence.
	Backoff *transport.Backoff
	// TTL is the code lifetime; zero means [PairCodeTTL].
	TTL time.Duration
	// NewCode creates a code; nil means identity.NewPairingCode.
	NewCode func() identity.PairingCode
	// EpochPoll is how often a waiting pairing checks whether another process
	// changed the enrollment; zero means [DefaultEpochPoll].
	EpochPoll time.Duration
	// WallMono supplies the wall and monotonic clock of the certificate window;
	// nil means the system clocks.
	WallMono transport.WallMono
	// Out is the console: the code and the result are printed here. Nil
	// discards them.
	Out io.Writer
	// Logger receives the code too (the one place a secret is logged, AD-20)
	// and operational messages. Nil discards them.
	Logger *slog.Logger
}

// Pairer runs the Worker side of pairing codes (R7, R54, R55, R77): it keeps
// one code alive at a time, shows it, waits for the Hub, and replaces it when
// it is too old or the Hub rejected it. It is a state machine over the
// [PairDialer], [Enrollments] and [transport.Clock] ports, so that the code
// lifecycle is unit-tested without a network and without waiting.
type Pairer struct {
	cfg PairerConfig
	log *slog.Logger
	clk transport.Clock
	bo  *transport.Backoff
	ttl time.Duration
	out io.Writer

	// penalty paces the codes that follow a connection that exposed a CPace
	// confirmation. It is separate from bo and never reset during a run: an
	// unauthenticated peer must not be able to shorten the delay with an error
	// frame that counts as progress for bo.
	penalty *transport.Backoff

	lastFail string
}

// NewPairer validates cfg and returns a pairer.
func NewPairer(cfg PairerConfig) (*Pairer, error) {
	switch {
	case cfg.HubURL == "":
		return nil, ErrNoHubURL
	case cfg.Enrollment == nil:
		return nil, errors.New("agent: the pairer needs the enrollment state")
	case cfg.Dialer == nil:
		return nil, errors.New("agent: the pairer needs a dialer")
	}
	p := &Pairer{cfg: cfg, log: cfg.Logger, clk: cfg.Clock, bo: cfg.Backoff, ttl: cfg.TTL, out: cfg.Out}
	if p.log == nil {
		p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if p.clk == nil {
		p.clk = transport.SystemClock
	}
	if p.bo == nil {
		p.bo = &transport.Backoff{}
	}
	p.penalty = &transport.Backoff{Initial: p.bo.Initial, Max: p.bo.Max, Rand: p.bo.Rand}
	if p.ttl <= 0 {
		p.ttl = PairCodeTTL
	}
	if p.cfg.EpochPoll <= 0 {
		p.cfg.EpochPoll = DefaultEpochPoll
	}
	if p.out == nil {
		p.out = io.Discard
	}
	if p.cfg.NewCode == nil {
		p.cfg.NewCode = identity.NewPairingCode
	}
	return p, nil
}

// codeLife is the current code and when it was first shown. A code that was
// never shown (no Hub reachable yet) has no age.
type codeLife struct {
	mu      sync.Mutex
	code    identity.PairingCode
	shown   bool
	shownAt time.Time
	// exposed and attemptAuth are per attempt (beginAttempt resets them):
	// exposed is set when this attempt sent the CPace confirmation, attemptAuth
	// when the Hub proved its identity in this same attempt (certificate, CPace
	// confirmation and sig_hub). Only an attempt that authenticated the Hub
	// keeps the code after its connection ended before welcome (design section
	// 12.2 item 10); an exposed attempt that did not retires it, however the
	// code was authenticated by an earlier attempt or restored from a record.
	exposed     bool
	attemptAuth bool
	// authenticated says that a pair_resume record exists for this code (it was
	// written for it or restored with it); renew deletes the record.
	authenticated bool
	// restored is set for a code that was read from the record after a restart.
	restored bool
	// resume is the pair_resume record this code was restored from or that was
	// written for it; its expiry is the code's end.
	resume *store.PairResume
}

// expiry is until under the lock.
func (l *codeLife) expiry(ttl time.Duration) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.until(ttl)
}

// authenticate records that the Hub proved its identity for this code.
func (l *codeLife) authenticate(r store.PairResume) {
	l.mu.Lock()
	l.authenticated, l.attemptAuth = true, true
	l.resume = &r
	l.mu.Unlock()
}

// hasRecord reports whether a pair_resume record exists for this code.
func (l *codeLife) hasRecord() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.authenticated
}

// wasAuthenticated reports whether the Hub was authenticated in the latest
// attempt.
func (l *codeLife) wasAuthenticated() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attemptAuth
}

// beginAttempt starts a new attempt: exposure and authentication are
// per-attempt facts.
func (l *codeLife) beginAttempt() {
	l.mu.Lock()
	l.exposed, l.attemptAuth = false, false
	l.mu.Unlock()
}

// ratchet returns the certificate memory of the resume record, empty without one.
func (l *codeLife) ratchet() transport.Ratchet {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resume == nil {
		return transport.Ratchet{}
	}
	return transport.Ratchet{Root: l.resume.RootPub, MaxSerial: l.resume.Serial, CertSHA256: l.resume.CertSHA256}
}

// until returns the end of the code's life: shownAt+ttl, or the expiry of the
// record a restored code carries.
func (l *codeLife) until(ttl time.Duration) time.Time {
	if l.resume != nil && l.restored {
		return l.resume.ExpiresAt
	}
	return l.shownAt.Add(ttl)
}

// expose records that the Worker sent its CPace confirmation for this code.
func (l *codeLife) expose() {
	l.mu.Lock()
	l.exposed = true
	l.mu.Unlock()
}

// wasExposed reports whether the Worker sent its CPace confirmation for this
// code.
func (l *codeLife) wasExposed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exposed
}

// show marks the code as shown at now and reports whether this is the first
// time, together with the lifetime that remains.
func (l *codeLife) show(now time.Time, ttl time.Duration) (first bool, remaining time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.shown {
		l.shown, l.shownAt = true, now
		return true, l.until(ttl).Sub(now)
	}
	return false, l.until(ttl).Sub(now)
}

// remaining returns the lifetime left; ok is false for a code that was not
// shown yet.
func (l *codeLife) remaining(now time.Time, ttl time.Duration) (left time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.shown {
		return ttl, false
	}
	return l.until(ttl).Sub(now), true
}

// errPairingStateChanged is returned when the enrollment state changed during
// a pairing and the Worker is not paired now (it was reset, or its identity
// was replaced): the key this run holds may be stale, so nothing is stored.
var errPairingStateChanged = errors.New("the Worker's enrollment state changed while pairing; run the command again")

// Run pairs the Worker. It refuses a paired Worker with already_paired,
// persists the Worker key pair, and then shows codes until the Hub confirms
// one: it commits the pin and the enrollment, prints "paired to <user>" and
// returns the enrollment. It returns an error only when ctx ends, when the
// Worker is already paired, or for a failure that waiting cannot fix (a
// removed Worker key). A Hub that is down is retried with the AD-3 backoff
// without any time limit.
//
// "Not paired" and the enrollment generation are read together. If anything
// commits or wipes the enrollment afterwards (another `minion worker pair`, the
// setup wizard, an invitation enrollment of the service) the run ends with
// already_paired at once, cancels the attempt in flight, and its commit is
// conditional on that generation, so a paired Worker is never offered a code and
// an enrollment written meanwhile is never replaced (AD-8).
func (p *Pairer) Run(ctx context.Context) (Enrollment, error) {
	gen, moved, key, err := p.begin(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	life, notice := p.initialLife(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return Enrollment{}, err
		}
		if err := p.checkMoved(ctx, gen); err != nil {
			return Enrollment{}, err
		}
		if left, shown := life.remaining(p.clk.Now(), p.ttl); shown && left <= 0 {
			life, notice = p.renew(life), "The pairing code expired."
			continue
		}
		res, held, err := p.attempt(ctx, key, life, notice, gen, moved)
		notice = ""
		if err == nil {
			return p.finish(ctx, gen, res)
		}
		// Whatever comes next (a fatal error, the end of ctx, a moved
		// enrollment or another attempt), a code that this attempt exposed
		// without authenticating the Hub is retired first: its record goes, so
		// that neither this run nor a restart can bring it back.
		p.retireIfExposed(life, err)
		if ctx.Err() != nil {
			return Enrollment{}, ctx.Err()
		}
		if err := p.checkMoved(ctx, gen); err != nil {
			return Enrollment{}, err
		}
		next, reason, penalty, fatal := p.classify(life, err)
		if fatal != nil {
			return Enrollment{}, fatal
		}
		if next {
			life, notice = p.renew(life), reason
			if penalty {
				// The peer may have been an interceptor that tested a guess:
				// the next code is shown only after the retry delay, which
				// grows with every such connection (see classify).
				p.logFail(err)
				// The penalty delay is never reset, however long the peer held
				// the connection and whatever else happened in between:
				// holding it for pairHeldReset, or sending an error frame that
				// counts as progress for the ordinary backoff, would let an
				// interceptor guess once per pairHeldReset instead of once per
				// (capped) penalty delay.
				if _, err := p.penalty.Wait(ctx, p.clk); err != nil {
					return Enrollment{}, err
				}
			}
			continue
		}
		p.logFail(err)
		if p.progressed(err, held) {
			p.bo.Reset()
			p.clearFail()
		}
		if _, err := p.bo.Wait(ctx, p.clk); err != nil {
			return Enrollment{}, err
		}
	}
}

// initialLife returns the first code of a run. An unexpired pair_resume record
// from an earlier run (the Hub was authenticated, welcome never arrived) brings
// its code back: the owner types the same code again. An expired or unreadable
// record is deleted and a new code follows (design section 12.2).
func (p *Pairer) initialLife(ctx context.Context) (*codeLife, string) {
	rec, ok, err := p.cfg.Enrollment.LoadPairResume(ctx)
	if err != nil {
		p.log.Warn("cannot read the pairing resume record; a new code is used", "error", err.Error())
		return &codeLife{code: p.newCode(identity.PairingCode{})}, ""
	}
	if !ok {
		return &codeLife{code: p.newCode(identity.PairingCode{})}, ""
	}
	code, perr := identity.ParsePairingCode(rec.Code)
	if perr != nil || !rec.ExpiresAt.After(p.clk.Now()) || code.PairingID() != rec.PairingID {
		_ = p.cfg.Enrollment.DeletePairResume(ctx)
		return &codeLife{code: p.newCode(identity.PairingCode{})}, "The pairing code expired."
	}
	p.log.Info("resuming a pairing after an interrupted connection", "hub", hubLabel(p.cfg.HubURL))
	return &codeLife{code: code, restored: true, authenticated: true, resume: &rec},
		"Resuming pairing: enter the same code again in the Hub (Add worker)."
}

// begin checks, under the enrollment lock, that the Worker is not paired,
// takes the generation together with that check, and loads or creates the
// Worker key. The lock is the one an invitation enrollment holds, so no key is
// created while such an enrollment runs, and an enrollment it committed is seen
// here as already paired.
func (p *Pairer) begin(ctx context.Context) (gen uint64, moved <-chan struct{}, key *identity.Key, err error) {
	release, err := p.cfg.Enrollment.lockEnroll(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	defer release()
	gen, moved, enr, ok, err := p.cfg.Enrollment.Snapshot(ctx)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read the enrollment: %w", err)
	}
	if ok {
		return 0, nil, nil, AlreadyPaired(enr)
	}
	key, err = p.cfg.Enrollment.LoadOrCreateKey()
	if err != nil {
		return 0, nil, nil, err
	}
	return gen, moved, key, nil
}

// checkMoved returns nil while the enrollment generation is still gen. After a
// change it returns already_paired when the Worker is paired now, and
// [errPairingStateChanged] when it is not.
func (p *Pairer) checkMoved(ctx context.Context, gen uint64) error {
	if !p.cfg.Enrollment.Moved(gen) {
		return nil
	}
	return p.changed(ctx)
}

// changed describes an enrollment that moved under this run.
func (p *Pairer) changed(ctx context.Context) error {
	// A writer moves the generation before it stores the enrollment, both under
	// the state lock: read through Snapshot (which takes that lock) so that a
	// write in progress is never seen half done as "moved but not enrolled".
	_, _, enr, ok, err := p.cfg.Enrollment.Snapshot(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("read the enrollment: %w", err)
	case ok:
		p.log.Warn("the Worker was paired by another command while this one waited; pairing stopped")
		return AlreadyPaired(enr)
	}
	return errPairingStateChanged
}

// classify decides what an error of the dialer means for the code. next is
// true for a fresh code (with the reason to show), penalty asks for the retry
// delay before it; a non-nil fatal ends the run; otherwise the same code is
// tried again after the backoff.
//
// A code whose CPace confirmation the Worker already sent is never tried again.
// The Worker confirms first (AD-8), so its confirmation tells whoever holds the
// connection whether one guess of the code was right, and the answer that
// follows proves nothing: an honest Hub rejects a wrong code with
// pairing_code_invalid, and an interceptor that tested a guess can send the very
// same frame, or just drop the connection. Reusing the code after such an
// answer would give the interceptor one guess per connection, without ever
// reaching the Hub's five-attempt count (the Hub alone counts attempts, AD-8,
// and the real Hub stays as strict as before). So the Worker retires the code
// after every connection that exposed its confirmation and did not pair, and it
// does not reset the delay for that, not even after a connection that was held
// for a long time, and not after an unauthenticated error frame that counts as
// progress for the ordinary backoff (the penalty delay is a separate sequence): an interceptor gets one guess per code and a growing delay
// between codes (at most one per capped backoff delay), an honest user who
// mistypes sees a fresh code after a short delay.
//
// Reconciliation with AD-8 ("only the Hub counts attempts: 5 per code"): the
// Hub's count still applies to every code it sees, but a real Worker never
// reaches five attempts on one code, because it retires the code after the
// first exposed failure. Integrator note A-17 records this.
func (p *Pairer) classify(life *codeLife, err error) (next bool, reason string, penalty bool, fatal error) {
	if errors.Is(err, errCodeExpired) {
		return true, "The pairing code expired.", false, nil
	}
	if errors.Is(err, transport.ErrPairingConfirm) {
		// An honest Hub confirms only after it verified this Worker's tag,
		// so a bad confirmation means the peer does not know the code: the
		// connection may be intercepted (the WSS dial accepts any
		// certificate, A-15). Retrying with the same code would hand an
		// interceptor an online guess per attempt, so the run ends.
		p.log.Warn("the hub's pairing confirmation failed; pairing stopped, nothing was stored", "hub", hubLabel(p.cfg.HubURL))
		// The confirmation was exposed and the Hub was not authenticated: the
		// code (and a record that would bring it back) is retired.
		p.dropResume(life)
		return false, "", false, fmt.Errorf("the server could not prove that it knows the pairing code, so the connection may be intercepted; "+
			"nothing was stored and the code is not reused. Check the Hub address and the network, then run the command again for a new code: %w", err)
	}
	var pe *proto.Error
	isErr := errors.As(err, &pe)
	if isErr && pe.Code == proto.CodeRevoked {
		return false, "", false, fmt.Errorf("the Hub removed this Worker key: reset the Worker (`minion worker pair --new-code`) and pair again: %w", err)
	}
	if errors.Is(err, transport.ErrResumeRootMismatch) {
		// A Hub with another root answered a resume (even one that knows the
		// code and passes CPace): refused before sig_worker. The record and the
		// code are kept, nothing is pinned (section 11.2 step 4b).
		p.log.Warn("a Hub with a different identity answered the pairing resume; refused before anything was sent, the code and the record are kept",
			"hub", hubLabel(p.cfg.HubURL))
		return false, "", false, nil
	}
	if isErr && pe.Code == proto.CodeAlreadyPaired {
		return false, "", false, &knownKeyError{cause: err}
	}
	var refusal *transport.RefusalError
	serialRefused := errors.As(err, &refusal) &&
		(refusal.Reason == transport.ReasonCertSerialTooLow || refusal.Reason == transport.ReasonCertSerialConflict)
	if life.wasAuthenticated() || serialRefused {
		// A serial verdict is applied after sig_hub verified, so the Hub was
		// authenticated in this attempt although no sig_worker was sent.
		// The Hub proved its identity in THIS attempt (certificate, CPace
		// confirmation, sig_hub) and the connection ended before welcome: the
		// code stays until it expires. Only a peer that knows the code can
		// produce the confirmation, so no unauthenticated peer gets a guess. An
		// earlier attempt's authentication, or a restored record, does not count.
		return false, "", false, nil
	}
	if life.wasExposed() {
		reason = "The connection ended while the code was being checked, so a new code replaces it."
		if isErr {
			switch pe.Code {
			case proto.CodePairingCodeInvalid:
				reason = "The Hub did not accept the code. A new code replaces it."
			case proto.CodePairingAttemptsExceeded:
				reason = "The Hub dropped the pairing code after too many wrong attempts."
			}
		}
		p.log.Warn("the pairing code was retired after a connection that did not pair; a new code follows", "hub", hubLabel(p.cfg.HubURL))
		return true, reason, true, nil
	}
	if isErr {
		switch pe.Code {
		case proto.CodePairingAttemptsExceeded:
			return true, "The Hub dropped the pairing code after too many wrong attempts.", false, nil
		case proto.CodePairingIDCollision:
			return true, "Another Worker is already waiting with the same pairing id.", false, nil
		case proto.CodePairingCodeInvalid:
			// The code never reached the confirmation step (nothing about it
			// was exposed), so it stays until it is too old.
			if left, shown := life.remaining(p.clk.Now(), p.ttl); shown && left <= 0 {
				return true, "The pairing code expired.", false, nil
			}
		}
	}
	return false, "", false, nil
}

// knownKeyError is the Hub's already_paired answer after the pairing window
// ended: the Hub has a Worker row for this key. The text is a sentence for the
// operator, hence the capital letter.
type knownKeyError struct{ cause error }

func (e *knownKeyError) Error() string {
	return "The Hub already knows this Worker key. Ask the owner to use Re-issue pairing for this Worker. (" + e.cause.Error() + ")"
}

func (e *knownKeyError) Unwrap() error { return e.cause }

// progressed reports whether a failed attempt counts as progress for the
// backoff (AD-3): a wrong code typed in the Hub (a human is waiting, so a quick
// reconnect is wanted), or a connection that was held for pairHeldReset, such as
// one that a proxy idle timeout ended. A hello write alone is not progress:
// rate_limited, proto_incompatible, internal and shutdown refusals all arrive
// after it.
func (p *Pairer) progressed(err error, held time.Duration) bool {
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.CodePairingCodeInvalid {
		return true
	}
	return held >= pairHeldReset
}

// newCode creates a code whose pairing id differs from prev's.
func (p *Pairer) newCode(prev identity.PairingCode) identity.PairingCode {
	c := p.cfg.NewCode()
	for i := 0; i < maxNewCodeTries && prev.PairingID() != "" && c.PairingID() == prev.PairingID(); i++ {
		c = p.cfg.NewCode()
	}
	return c
}

// retireIfExposed retires the code of an attempt that sent the Worker's CPace
// confirmation and did not authenticate the Hub (no sig_hub, or no record
// written in this attempt): its pair_resume record is deleted at once, on
// every exit path. A serial verdict comes after sig_hub, so such an attempt
// did authenticate the Hub and keeps the code (section 12.2).
func (p *Pairer) retireIfExposed(life *codeLife, err error) {
	if !life.wasExposed() || life.wasAuthenticated() {
		return
	}
	var refusal *transport.RefusalError
	if errors.As(err, &refusal) &&
		(refusal.Reason == transport.ReasonCertSerialTooLow || refusal.Reason == transport.ReasonCertSerialConflict) {
		return
	}
	p.dropResume(life)
}

// dropResume deletes the pair_resume record of a code that is retired.
func (p *Pairer) dropResume(l *codeLife) {
	if !l.hasRecord() {
		return
	}
	if err := p.cfg.Enrollment.DeletePairResume(context.Background()); err != nil {
		p.log.Warn("cannot delete the pairing resume record of a retired code", "error", err.Error())
		return
	}
	l.mu.Lock()
	l.authenticated = false
	l.mu.Unlock()
}

func (p *Pairer) renew(old *codeLife) *codeLife {
	if old.hasRecord() {
		// The old code is abandoned (it expired, or an attempt exposed it
		// without authenticating the Hub): its record goes, so that the next
		// code starts clean and a restart cannot bring the code back.
		p.dropResume(old)
	}
	return &codeLife{code: p.newCode(old.code)}
}

// attempt makes one connection with the current code. The code is shown as
// soon as the Hub holds the connection (so that with no Hub reachable it
// appears after the first successful connection), and an attempt ends with
// errCodeExpired when the code reaches the end of its life.
func (p *Pairer) attempt(ctx context.Context, key *identity.Key, life *codeLife, notice string, gen uint64, moved <-chan struct{}) (PairResult, time.Duration, error) {
	life.beginAttempt()
	var heldMu sync.Mutex
	var waitingAt time.Time
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		// The attempt ends as soon as the enrollment moves, in this process
		// (moved) or in another one (a real ticker: no fake clock can advance
		// another process).
		tick := time.NewTicker(p.cfg.EpochPoll)
		defer tick.Stop()
		for {
			select {
			case <-moved:
				cancel(errEnrollmentChanged)
				return
			case <-tick.C:
				if p.cfg.Enrollment.Moved(gen) {
					cancel(errEnrollmentChanged)
					return
				}
			case <-actx.Done():
				return
			}
		}
	}()
	var ops []string
	if p.cfg.Ops != nil {
		ops = p.cfg.Ops()
	}
	var features []string
	if p.cfg.Features != nil {
		features = p.cfg.Features()
	}
	res, err := p.cfg.Dialer.Pair(actx, PairParams{
		HubURL:    p.cfg.HubURL,
		Key:       key,
		Name:      p.cfg.Name,
		Ops:       ops,
		Features:  features,
		Protocols: p.cfg.Protocols,
		Code:      life.code,
		Ratchet:   life.ratchet(),
		Clock:     p.cfg.WallMono,
		Timeout:   p.ttl + pairHandshakeMargin,
		OnExposed: life.expose,
		OnAuthenticated: func(a transport.HubAuth) error {
			// Durable before sig_worker: the Hub cannot create the Worker
			// before it has sig_worker, so the record precedes any Hub commit.
			rec := store.PairResume{
				Code: life.code.String(), PairingID: life.code.PairingID(), ExpiresAt: life.expiry(p.ttl),
				RootPub: a.Root, Serial: a.Ratchet.MaxSerial, CertSHA256: a.Ratchet.CertSHA256,
			}
			// The expiry is checked inside the enrollment guard, immediately
			// before the write (the wait for the lock is covered).
			exp, clk := a.Expiry, p.cfg.WallMono
			live := func() bool { return !exp.Expired(clk) }
			if err := p.cfg.Enrollment.SavePairResumeIfLive(actx, gen, rec, live); err != nil {
				if errors.Is(err, errGuardExpired) {
					return errEnrollExpired
				}
				return fmt.Errorf("store the pairing resume record: %w", err)
			}
			life.authenticate(rec)
			return nil
		},
		OnWaiting: func() {
			first, left := life.show(p.clk.Now(), p.ttl)
			if first {
				p.showCode(life.code, notice)
			}
			heldMu.Lock()
			if waitingAt.IsZero() {
				waitingAt = p.clk.Now()
			}
			heldMu.Unlock()
			go func() {
				select {
				case <-p.clk.After(max(left, 0)):
					cancel(errCodeExpired)
				case <-actx.Done():
				}
			}()
		},
	})
	heldMu.Lock()
	var held time.Duration
	if !waitingAt.IsZero() {
		held = p.clk.Now().Sub(waitingAt)
	}
	heldMu.Unlock()
	if err != nil && errors.Is(context.Cause(actx), errCodeExpired) && ctx.Err() == nil {
		return PairResult{}, held, errCodeExpired
	}
	return res, held, err
}

// showCode prints the code on the console and in the log: the one place a
// secret is printed (AD-20, NFR1).
func (p *Pairer) showCode(c identity.PairingCode, notice string) {
	var b strings.Builder
	if notice != "" {
		b.WriteString(notice + " ")
	}
	fmt.Fprintf(&b, "Pairing code: %s\n", c.String())
	fmt.Fprintf(&b, "Enter this code in the Hub (Add worker) within %s. Hub: %s\n", ttlText(p.ttl), hubLabel(p.cfg.HubURL))
	_, _ = io.WriteString(p.out, b.String())
	// The code is a secret that is printed for the human on purpose (AD-20):
	// the console above and the log (docker logs) are the only places.
	p.log.Info("pairing code", "pairing_code", c.String(), "valid_for", p.ttl.String(), "hub", hubLabel(p.cfg.HubURL))
}

func ttlText(d time.Duration) string {
	if d%time.Minute == 0 {
		m := int(d / time.Minute)
		if m == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", m)
	}
	return d.Round(time.Second).String()
}

// finish stores the enrollment (the pin of the CPace-authenticated Hub and
// what welcome said) and reports success only after it is committed (AD-7).
// The commit is conditional on gen and runs under the enrollment lock: if the
// Worker was paired meanwhile, nothing is overwritten and the result is
// already_paired, without "paired to".
func (p *Pairer) finish(ctx context.Context, gen uint64, res PairResult) (Enrollment, error) {
	w := res.Welcome
	if w.WorkerID == "" {
		return Enrollment{}, errors.New("the Hub's welcome has no worker id")
	}
	enr := Enrollment{
		Pin:      res.Pin,
		WorkerID: w.WorkerID,
		Name:     w.Name,
		Owner:    w.Owner,
		Kind:     w.Kind,
		Policy:   w.Policy,
		HubURL:   p.cfg.HubURL,
		Ratchet:  res.Ratchet,
	}
	// A commit must not fail because the caller's context just ended: the Hub
	// already created the Worker, and the pin is what lets this Worker
	// reconnect. The wait for the enrollment lock is bounded, though: an
	// invitation enrollment holds it for the length of its retries.
	cctx := context.WithoutCancel(ctx)
	lctx, cancel := context.WithTimeout(cctx, commitLockWait)
	release, err := p.cfg.Enrollment.lockEnroll(lctx)
	cancel()
	if err != nil {
		return Enrollment{}, fmt.Errorf("store the enrollment: %w", err)
	}
	defer release()
	// The certificate of the connection that carried the welcome must still be
	// valid when the commit happens, also after the wait for the locks.
	var live func() bool
	if res.Expiry != nil {
		exp, clk := *res.Expiry, p.cfg.WallMono
		if clk == nil {
			clk = transport.SystemWallMono
		}
		live = func() bool { return !exp.Expired(clk) }
	}
	if err := p.cfg.Enrollment.CommitIfLive(cctx, gen, enr, live); err != nil {
		if errors.Is(err, errGuardExpired) {
			p.log.Warn("the Hub certificate expired before the pairing was committed; nothing was stored", "hub", hubLabel(p.cfg.HubURL))
			return Enrollment{}, errEnrollExpired
		}
		if errors.Is(err, ErrEnrollmentChanged) {
			p.log.Warn("the Hub accepted the code, but the Worker was paired meanwhile; the existing enrollment was kept",
				"hub", hubLabel(p.cfg.HubURL), "worker_id", w.WorkerID)
			return Enrollment{}, p.changed(cctx)
		}
		return Enrollment{}, fmt.Errorf("store the enrollment: %w", err)
	}
	_, _ = fmt.Fprintf(p.out, "paired to %s\n", w.Owner)
	p.log.Info("paired with the hub", "hub", hubLabel(p.cfg.HubURL), "hub_fingerprint", hubFingerprint(res.Pin),
		"worker_id", w.WorkerID, "name", w.Name, "owner", w.Owner, "kind", w.Kind)
	return enr, nil
}

// logFail logs the first failure of a streak at Warn and repeats at Debug.
func (p *Pairer) logFail(err error) {
	msg := err.Error()
	repeat := p.lastFail == msg
	p.lastFail = msg
	if repeat {
		p.log.Debug("pairing connection failed again", "error", msg)
		return
	}
	p.log.Warn("pairing connection failed; retrying with backoff", "error", msg)
}

func (p *Pairer) clearFail() { p.lastFail = "" }

// RenewWorkerIdentity is `minion worker pair --new-code` on a Worker (AD-8): it
// creates a new Worker key, wipes the pin and the enrollment, and keeps the old
// identity as a pending revocation, so that pairing starts from scratch with a
// new identity and the old enrollment is revoked at its Hub (see [Retirer]).
// The Hub to revoke at is the one recorded with the enrollment
// ([Enrollment.HubURL]); an enrollment that did not record it (stored by an
// older version) is refused with [ErrUnknownEnrollmentHub], because no other
// address is known to be the Hub that holds the old key. The tmux sessions, the share selection and the session
// counter stay.
//
// The steps run in this order, each safe to repeat after a crash:
//
//  1. the old key, the old pin and the Hub URL are saved as a pending
//     revocation (nothing else has changed yet);
//  2. the enrollment is wiped, which advances the enrollment generation under
//     the cross-process lock: the running service (another process) drops its
//     connection to the old Hub within its epoch poll, and nothing it still
//     holds, such as an old connection's revoke, can touch the state after
//     this point;
//  3. the new key replaces the old one. The service is idle meanwhile (it has
//     no enrollment), so it never connects to the old Hub with the new key.
//
// A crash after step 2 leaves a Worker without an enrollment that still has the
// old key and the pending revocation; running the command again repairs it.
// renewAfterLoad is a test seam that runs right after the identity replacement
// read the enrollment; nil in production.
var renewAfterLoad func()

func RenewWorkerIdentity(ctx context.Context, e *Enrollments) (*identity.Key, error) {
	// The enrollment lock covers the whole replacement: no pairing or
	// invitation enrollment creates or replaces a key, or commits, meanwhile.
	release, err := e.lockEnroll(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	k, err := identity.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("create the new Worker key: %w", err)
	}
	// The state lock (the one the connector's ratchet and welcome commits take)
	// is held from the read of the enrollment through the durable retired record
	// to the wipe: a ratchet the connector commits either lands before the read,
	// and is carried, or finds the generation moved and stores nothing.
	_, err = e.ReplaceIdentity(ctx, func(enr Enrollment, enrolled bool) error {
		if renewAfterLoad != nil {
			renewAfterLoad()
		}
		if !enrolled {
			return nil
		}
		var old *identity.Key
		switch lk, kerr := e.LoadKey(); {
		case kerr == nil:
			old = lk
		case errors.Is(kerr, ErrNoWorkerKey):
			// Nothing to authenticate a revocation with, and nobody can use a
			// key that does not exist: the identity is replaced regardless.
			return nil
		default:
			return fmt.Errorf("read the old Worker key: %w", kerr)
		}
		// The Hub that authenticated this enrollment, never whatever the
		// configuration says by now.
		if enr.HubURL == "" {
			return ErrUnknownEnrollmentHub
		}
		if err := e.RetiredStore().Add(RetiredIdentity{
			Key: old, HubURL: enr.HubURL, Pin: enr.Pin, WorkerID: enr.WorkerID, RetiredAt: time.Now(),
			MaxSerial: enr.Ratchet.MaxSerial, CertSHA256: enr.Ratchet.CertSHA256, RatchetSeeded: true,
		}); err != nil {
			return fmt.Errorf("keep the old identity for its revocation: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrUnknownEnrollmentHub) {
			return nil, ErrUnknownEnrollmentHub
		}
		return nil, err
	}
	keyInitMu.Lock()
	err = identity.SaveKey(e.KeyPath(), k)
	keyInitMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("save the new Worker key %s: %w", e.KeyPath(), err)
	}
	return k, nil
}
