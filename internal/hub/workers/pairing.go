package workers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Pairing limits (AD-8).
const (
	// PairingTTL is how long a code lives, and so how long the Hub holds a
	// Worker connection as pending.
	PairingTTL = 10 * time.Minute
	// PairingMaxAttempts is the number of failed claims after which the
	// pending pairing is dropped.
	PairingMaxAttempts = 5

	defaultMaxPending      = 1024
	defaultMaxPendingPerIP = 16
	defaultSweepInterval   = 10 * time.Second
	// maxWaitMargin is added to the lifetime of a code in MaxWait, so that the
	// expiry (which answers the Worker) always fires before the connection
	// deadline does.
	maxWaitMargin = 30 * time.Second
	// maxSuffixTries bounds the "-2", "-3" search for a free Worker name.
	maxSuffixTries = 1000
)

// PairingStore is the part of the database the pairing service uses;
// *store.Store implements it.
type PairingStore interface {
	Repository
	Pairing(ctx context.Context, id string) (store.PairingRow, error)
	PutPairing(ctx context.Context, p store.PairingRow) error
	AddPairingFailure(ctx context.Context, id string, max int) (store.PairingRow, error)
	CompletePairing(ctx context.Context, id, codeDigest string, w store.WorkerRow) error
	PairingCodeUsed(ctx context.Context, codeDigest string) (bool, error)
	PurgePairings(ctx context.Context, before int64) (int64, error)
}

// PairingOptions configures [Service.EnablePairing] and [NewPairings]. The
// zero value of every field is usable except Store when the service's store
// has no pending_pairings table.
type PairingOptions struct {
	// Store holds the pairing rows and creates the Worker. The default is
	// the Service's store.
	Store PairingStore
	// Limiter applies the "pairing" policy to claims, per user and per IP. The
	// default is the Service's limiter.
	Limiter Limiter
	// Auditor records created Workers, failed attempts and dropped codes.
	Auditor Auditor
	// Clock is the time source; tests inject a fake. The default is time.Now.
	Clock func() time.Time
	// TTL is the lifetime of a code; the default is PairingTTL.
	TTL time.Duration
	// MaxAttempts is the failed-claim limit; the default is
	// PairingMaxAttempts.
	MaxAttempts int
	// MaxPending bounds the Worker connections held as pending at once, and
	// MaxPendingPerIP those of one client network, so that unauthenticated
	// connections cannot hold unbounded memory.
	MaxPending      int
	MaxPendingPerIP int
	// SweepInterval is how often [Pairings.Start] expires pending pairings.
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// ClaimRequest is one pairing_claim call.
type ClaimRequest struct {
	// UserID is the claiming user: the owner of the new Worker. For an admin
	// who acts as another user it is that user.
	UserID string
	// Code is the pairing code as typed. It is never logged.
	Code string
	// Name optionally overrides the name the Worker announced.
	Name string
	// IP is the client address of the call, for the pairing limiter.
	IP netip.Addr
	// Actor and ActingAs are the audit identity of the call (AD-20): the
	// admin, or "key:<id>" for an API key, and the user an admin acts as.
	// Actor defaults to UserID.
	Actor    string
	ActingAs string
}

// ClaimResult describes the Worker a claim created.
type ClaimResult struct {
	WorkerID string
	// Name is the name the Worker got; it differs from the requested one when
	// the owner already had a Worker of that name.
	Name    string
	Renamed bool
}

// RateLimitError is a rate_limited refusal that says when to retry.
type RateLimitError struct {
	Err   *proto.Error
	After time.Duration
}

// Error implements error.
func (e *RateLimitError) Error() string { return e.Err.Error() }

// Unwrap lets errors.As find the *proto.Error.
func (e *RateLimitError) Unwrap() error { return e.Err }

// RetryAfter returns how long the caller must wait.
func (e *RateLimitError) RetryAfter() time.Duration { return e.After }

// Pairings is the Hub side of pairing codes (Stories 4.7 and 4.8, AD-8): the
// table of Worker connections that wait for their code (pending, in memory),
// the CPace responder side of a claim, the attempt counter and the creation of
// the Worker. It implements [PairingGate] and [ModeHandler]. Safe for
// concurrent use.
//
// The flow:
//
//  1. A Worker connects in pair_code mode with its public pairing id. The
//     handler calls [Pairings.Await] from inside the handshake; the Hub holds
//     the connection there, for at most the lifetime of the code, and has sent
//     nothing yet.
//  2. A user calls [Pairings.Claim] with the full code. The code becomes the
//     CPace password of the waiting connection and the handshake continues:
//     the Worker confirms first, so a wrong code costs one counted attempt and
//     reveals nothing.
//  3. On success the handler calls [Pairings.Handle], which creates the Worker
//     as private, owned by the claiming user, marks the code used in the same
//     transaction and sends welcome with the owner's name.
//
// Attempts, expiry and the used flag live in the pending_pairings table, so a Hub
// restart neither resets the counter nor revives a used code. The code itself
// and the password are never stored or logged.
type Pairings struct {
	store   PairingStore
	limiter Limiter
	auditor Auditor
	now     func() time.Time
	log     *slog.Logger

	ttl         time.Duration
	maxAttempts int
	maxPending  int
	maxPerIP    int
	sweepEvery  time.Duration

	mu     sync.Mutex
	live   map[string]*pending
	perIP  map[string]int
	closed bool

	stop    chan struct{}
	stopped sync.WaitGroup
	started bool
}

// pending is one Worker connection waiting for its code.
type pending struct {
	id      string
	ipKey   string
	expires time.Time
	claims  chan *claim // capacity 1; filled by Claim, drained by Await
	gone    chan struct{}

	// Guarded by Pairings.mu.
	goneErr error
	ended   bool
	cur     *claim // the claim that supplied the password
}

// claim is one claim in flight: the password and where the outcome goes.
type claim struct {
	userID   string // the owner of the new Worker
	actor    string // the audit actor (AD-20)
	actingAs string // the user an admin acts as, or empty
	name     string
	password []byte
	digest   string            // one-way digest of the code, recorded when it is used
	done     chan claimOutcome // capacity 1
	once     sync.Once
}

type claimOutcome struct {
	res ClaimResult
	err error
}

func (c *claim) finish(o claimOutcome) {
	c.once.Do(func() { c.done <- o })
}

// wipe zeroes the password. It is called only after the handshake that used
// it has ended.
func (c *claim) wipe() { clear(c.password) }

// NewPairings builds the pairing service. It does not start the expiry sweep;
// call [Pairings.Start], or [Pairings.Sweep] yourself.
func NewPairings(o PairingOptions) (*Pairings, error) {
	if o.Store == nil {
		return nil, errors.New("workers: the pairing service needs a store")
	}
	ps := &Pairings{
		store: o.Store, limiter: o.Limiter, auditor: o.Auditor, now: o.Clock, log: o.Logger,
		ttl: o.TTL, maxAttempts: o.MaxAttempts, maxPending: o.MaxPending, maxPerIP: o.MaxPendingPerIP,
		sweepEvery: o.SweepInterval,
		live:       map[string]*pending{}, perIP: map[string]int{}, stop: make(chan struct{}),
	}
	if ps.now == nil {
		ps.now = time.Now
	}
	if ps.log == nil {
		ps.log = slog.Default()
	}
	if ps.ttl <= 0 {
		ps.ttl = PairingTTL
	}
	if ps.maxAttempts <= 0 {
		ps.maxAttempts = PairingMaxAttempts
	}
	if ps.maxPending <= 0 {
		ps.maxPending = defaultMaxPending
	}
	if ps.maxPerIP <= 0 {
		ps.maxPerIP = defaultMaxPendingPerIP
	}
	if ps.sweepEvery <= 0 {
		ps.sweepEvery = defaultSweepInterval
	}
	return ps, nil
}

// MaxWait implements [PairingGate]: the lifetime of a code plus a margin.
func (ps *Pairings) MaxWait() time.Duration { return ps.ttl + maxWaitMargin }

// Pending returns the number of Worker connections that wait for a code.
func (ps *Pairings) Pending() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.live)
}

// IsPending reports whether a Worker connection waits for the pairing id.
func (ps *Pairings) IsPending(pairingID string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.live[pairingID] != nil
}

// sameIP is 1 when the pending entry belongs to the address key, else 0.
func sameIP(p *pending, ipKey string) int {
	if p != nil && p.ipKey == ipKey {
		return 1
	}
	return 0
}

func pairingErr(code proto.Code, message string) *proto.Error { return proto.NewError(code, message) }

func errExpired() *proto.Error {
	e := pairingErr(proto.CodePairingCodeInvalid, "the pairing code expired; the Worker shows a new one")
	// The reason token is what a Worker, which cannot yet trust this Hub, shows
	// instead of the message (transport peerReasons).
	e.Details = json.RawMessage(`{"reason":"expired"}`)
	return e
}

func errNoWorkerWaiting() *proto.Error {
	return pairingErr(proto.CodePairingCodeInvalid,
		"no Worker is waiting for this code; check the code, and that the Worker is running and can reach the Hub")
}

func errWorkerGone() *proto.Error {
	return pairingErr(proto.CodeWorkerOffline, "the Worker disconnected before pairing finished; it shows a new code")
}

func errAttemptsExceeded() *proto.Error {
	return pairingErr(proto.CodePairingAttemptsExceeded, "too many failed attempts; this code is no longer valid and the Worker shows a new one")
}

func errWrongCode(left int) *proto.Error {
	e := pairingErr(proto.CodePairingCodeInvalid, "the pairing code is wrong")
	e.Details, _ = json.Marshal(map[string]int{"attempts_left": left})
	return e
}

// PairingTicket binds one Worker connection to the pending entry it registered
// and to the claim that supplied its password. The handler keeps it for the
// whole connection: the handshake outcome and the creation of the Worker use
// only the ticket and never look the pairing id up again, so a claim made for
// another connection with the same id can never be applied to this one.
type PairingTicket struct {
	ps *Pairings
	p  *pending
	c  *claim
}

// Password returns the CPace password of the claim. It is valid until the
// handshake ends.
func (t *PairingTicket) Password() []byte { return t.c.password }

// Await implements [PairingGate]. See the type documentation for the flow. It
// returns the ticket of the connection once a user claimed its code.
func (ps *Pairings) Await(ctx context.Context, id string, ip netip.Addr) (*PairingTicket, error) {
	now := ps.now()
	ipKey := ratelimit.IPKey(ip)

	ps.mu.Lock()
	old := ps.live[id]
	switch {
	case ps.closed:
		ps.mu.Unlock()
		return nil, pairingErr(proto.CodeWorkerOffline, "the Hub is shutting down")
	case old != nil && (old.cur != nil || now.Before(old.expires)):
		// A live entry, or an expired one whose handshake is still running
		// (the sweep leaves it to finish as well): never displaced.
		ps.mu.Unlock()
		return nil, pairingErr(proto.CodePairingIDCollision, "a Worker is already waiting with this pairing id; it must show a new code")
	case old == nil && len(ps.live) >= ps.maxPending, ps.perIP[ipKey]-sameIP(old, ipKey) >= ps.maxPerIP:
		// A replacement frees the slot of the entry it removes, but only for
		// the address that held it: another address must have room of its own.
		ps.mu.Unlock()
		return nil, pairingErr(proto.CodeRateLimited, "too many Workers are waiting to pair; retry later")
	}
	if old != nil { // expired with no claim in flight, and the sweep has not run yet
		ps.endLocked(old, errExpired())
	}
	p := &pending{id: id, ipKey: ipKey, expires: now.Add(ps.ttl), claims: make(chan *claim, 1), gone: make(chan struct{})}
	ps.live[id] = p
	ps.perIP[ipKey]++
	ps.mu.Unlock()

	// The id is reserved; the row decides whether the code may be used.
	row, err := ps.loadRow(ctx, id, now)
	if err != nil {
		if ctx.Err() != nil {
			err = errWorkerGone() // the connection ended while the row was read
		}
		ps.mu.Lock()
		ps.endLocked(p, err)
		ps.mu.Unlock()
		return nil, err
	}
	ps.mu.Lock()
	if !p.ended {
		p.expires = store.FromUnixMS(row.ExpiresAt)
	}
	ps.mu.Unlock()

	select {
	case c := <-p.claims:
		return &PairingTicket{ps: ps, p: p, c: c}, nil
	case <-p.gone:
		ps.mu.Lock()
		err := p.goneErr
		ps.mu.Unlock()
		return nil, err
	case <-ctx.Done():
		ps.mu.Lock()
		ps.endLocked(p, errWorkerGone())
		ps.mu.Unlock()
		return nil, errWorkerGone()
	}
}

// loadRow returns the pending row of id, creating or restarting it where
// needed, and refuses an id whose code was used or dropped.
func (ps *Pairings) loadRow(ctx context.Context, id string, now time.Time) (store.PairingRow, error) {
	nowMS := store.UnixMS(now)
	row, err := ps.store.Pairing(ctx, id)
	switch {
	case errors.Is(err, store.ErrPairingNotFound):
	case err != nil:
		ps.log.Error("pairing: read the pairing row", "error", err.Error())
		return store.PairingRow{}, pairingErr(proto.CodeInternal, "")
	case row.ExpiresAt <= nowMS:
		// An expired row protects nothing: a new code may reuse its id.
	case row.Status == store.PairingUsed:
		return store.PairingRow{}, pairingErr(proto.CodePairingIDCollision, "this pairing id was already used; the Worker must show a new code")
	case row.Status == store.PairingDropped:
		return store.PairingRow{}, errAttemptsExceeded()
	default:
		return row, nil // a Worker that reconnects keeps its expiry and its failed attempts
	}
	row = store.PairingRow{ID: id, Status: store.PairingPending, CreatedAt: nowMS, ExpiresAt: store.UnixMS(now.Add(ps.ttl))}
	if err := ps.store.PutPairing(ctx, row); err != nil {
		ps.log.Error("pairing: write the pairing row", "error", err.Error())
		return store.PairingRow{}, pairingErr(proto.CodeInternal, "")
	}
	return row, nil
}

// HandshakeFailed is called by the handler when the handshake that used the
// ticket's password failed. A failed CPace confirmation is a counted attempt;
// any other failure (the Worker left, a timeout) is not. It acts only on the
// ticket's own entry: when that entry already ended, nothing is charged, since
// the row may by then belong to a later connection.
func (t *PairingTicket) HandshakeFailed(hsErr error) {
	ps, p, c := t.ps, t.p, t.c
	defer c.wipe()
	if !errors.Is(hsErr, transport.ErrPairingConfirm) {
		ps.end(p, errWorkerGone())
		return
	}
	ps.mu.Lock()
	ended := p.ended
	ps.mu.Unlock()
	if ended {
		return
	}
	ctx := context.Background()
	row, err := ps.store.AddPairingFailure(ctx, p.id, ps.maxAttempts)
	switch {
	case errors.Is(err, store.ErrPairingNotPending):
		ps.end(p, errAttemptsExceeded())
		return
	case err != nil:
		ps.log.Error("pairing: count a failed attempt", "error", err.Error())
		ps.end(p, pairingErr(proto.CodeInternal, ""))
		return
	}
	outcome := "wrong_code"
	result := error(errWrongCode(max(ps.maxAttempts-row.Attempts, 0)))
	if row.Status == store.PairingDropped {
		outcome = "attempts_exceeded"
		result = errAttemptsExceeded()
	}
	ps.record(ctx, audit.Event{Actor: c.actor, ActingAs: c.actingAs, Action: "pairing.attempt", Target: p.id, Outcome: outcome})
	ps.end(p, result)
}

// Claim runs a pairing_claim: the full code is the CPace password of the
// Worker that waits for its public id. It returns when the handshake and the
// creation of the Worker are done, or fails with pairing_code_invalid (wrong,
// unknown, expired or used; details.attempts_left after a wrong code),
// pairing_attempts_exceeded, worker_offline, rate_limited or already_paired.
func (ps *Pairings) Claim(ctx context.Context, req ClaimRequest) (ClaimResult, error) {
	if req.UserID == "" {
		return ClaimResult{}, pairingErr(proto.CodeForbidden, "not allowed")
	}
	if ps.limiter != nil {
		// One check covers the client address and the user (AD-19); the
		// attempts of one code are limited by the counter below.
		if ok, wait := ps.limiter.Allow(ratelimit.PolicyPairing, req.IP, "user:"+req.UserID); !ok {
			return ClaimResult{}, &RateLimitError{Err: pairingErr(proto.CodeRateLimited, "too many pairing attempts; retry later"), After: wait}
		}
	}
	code, err := identity.ParsePairingCode(req.Code)
	if err != nil {
		return ClaimResult{}, pairingErr(proto.CodePairingCodeInvalid, "this is not a pairing code; it looks like MN-XXXX-XXXX")
	}
	actor := req.Actor
	if actor == "" {
		actor = req.UserID
	}
	digest := codeDigest(code)
	if used, err := ps.store.PairingCodeUsed(ctx, digest); err != nil {
		ps.log.Error("pairing: read used codes", "error", err.Error())
		return ClaimResult{}, pairingErr(proto.CodeInternal, "")
	} else if used {
		return ClaimResult{}, pairingErr(proto.CodePairingCodeInvalid, "this code was already used; the Worker must show a new code")
	}
	c := &claim{userID: req.UserID, actor: actor, actingAs: req.ActingAs, name: req.Name, password: code.Password(), digest: digest, done: make(chan claimOutcome, 1)}

	now := ps.now()
	ps.mu.Lock()
	p := ps.live[code.PairingID()]
	switch {
	case p == nil || !now.Before(p.expires):
		ps.mu.Unlock()
		return ClaimResult{}, errNoWorkerWaiting()
	case p.cur != nil:
		ps.mu.Unlock()
		return ClaimResult{}, pairingErr(proto.CodeConflict, "another claim of this code is in progress; retry in a moment")
	}
	p.cur = c
	p.claims <- c // capacity 1 and p.cur was nil, so this never blocks
	ps.mu.Unlock()

	select {
	case o := <-c.done:
		return o.res, o.err
	case <-ctx.Done():
		return ClaimResult{}, ctx.Err()
	}
}

// Handle implements [ModeHandler] for pair_code. It runs after the handshake
// succeeded, which proves that the Worker and the typed code agree. It uses
// the connection's own ticket, never the entry that the pairing id names now,
// and refuses when that entry ended in the meantime.
func (ps *Pairings) Handle(ctx context.Context, c *Conn) error {
	t := c.pairing
	if t == nil || t.ps != ps {
		return c.Reject(pairingErr(proto.CodeInternal, ""))
	}
	p, cl := t.p, t.c
	ps.mu.Lock()
	ended, err := p.ended, p.goneErr
	ps.mu.Unlock()
	if ended {
		cl.wipe()
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = pairingErr(proto.CodeInternal, "")
		}
		return c.Reject(pe)
	}

	row, renamed, err := ps.create(ctx, c, p.id, cl)
	cl.wipe() // the handshake is over; the password is not needed any more
	if err != nil {
		ps.end(p, err)
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = pairingErr(proto.CodeInternal, "")
		}
		_ = c.Reject(pe)
		return fmt.Errorf("pair: %w", err)
	}
	ps.mu.Lock()
	ps.removeLocked(p)
	ps.mu.Unlock()
	ps.record(ctx, audit.Event{Actor: cl.actor, ActingAs: cl.actingAs, Action: "worker.pair", Target: row.ID, Outcome: auditPending})
	c.h.log.Info("worker paired", "worker_id", row.ID, "name", row.Name, "fingerprint", c.fingerprint())
	cl.finish(claimOutcome{res: ClaimResult{WorkerID: row.ID, Name: row.Name, Renamed: renamed}})
	return c.Welcome(ctx, row)
}

// create inserts the private Worker owned by the claiming user and marks the
// code used, in one transaction.
func (ps *Pairings) create(ctx context.Context, c *Conn, pairingID string, cl *claim) (store.WorkerRow, bool, error) {
	ctx = context.WithoutCancel(ctx) // the Worker row and the used flag go together or not at all
	pub := []byte(c.WorkerPub())
	if _, err := ps.store.TombstoneByPub(ctx, pub); err == nil {
		return store.WorkerRow{}, false, pairingErr(proto.CodeRevoked, "this Worker key was removed from the Hub; reset the Worker and pair again")
	} else if !errors.Is(err, store.ErrWorkerNotFound) {
		ps.log.Error("pairing: read tombstone", "error", err.Error())
		return store.WorkerRow{}, false, pairingErr(proto.CodeInternal, "")
	}
	if _, err := ps.store.WorkerByPub(ctx, pub); err == nil {
		return store.WorkerRow{}, false, pairingErr(proto.CodeAlreadyPaired, "this Worker is already paired with this Hub")
	} else if !errors.Is(err, store.ErrWorkerNotFound) {
		ps.log.Error("pairing: read worker", "error", err.Error())
		return store.WorkerRow{}, false, pairingErr(proto.CodeInternal, "")
	}

	base := cl.name
	if base == "" || !ops.ValidWorkerName(base) {
		base = CleanWorkerName(c.Worker().Name)
	}
	row := store.WorkerRow{
		Pub: pub, OwnerID: cl.userID, Kind: store.WorkerKindPrivate, CreatedAt: store.UnixMS(ps.now()),
		// F-02: a paired Worker waits for approval like any other new Worker.
		Pending: true, EnrollAddr: c.ClientIP().String(),
	}
	for i := 1; i <= maxSuffixTries; i++ {
		row.ID, row.Name = store.NewID(), suffixed(base, i)
		err := ps.store.CompletePairing(ctx, pairingID, cl.digest, row)
		switch {
		case err == nil:
			return row, i > 1, nil
		case errors.Is(err, store.ErrWorkerNameTaken):
			continue
		case errors.Is(err, store.ErrWorkerExists):
			return store.WorkerRow{}, false, pairingErr(proto.CodeAlreadyPaired, "this Worker is already paired with this Hub")
		case errors.Is(err, store.ErrPairingNotPending):
			return store.WorkerRow{}, false, errExpired()
		case errors.Is(err, store.ErrPairingCodeUsed):
			return store.WorkerRow{}, false, pairingErr(proto.CodePairingCodeInvalid, "this code was already used; the Worker must show a new code")
		}
		ps.log.Error("pairing: create the Worker", "error", err.Error())
		return store.WorkerRow{}, false, pairingErr(proto.CodeInternal, "")
	}
	return store.WorkerRow{}, false, pairingErr(proto.CodeConflict, "the owner has too many Workers with this name; pick another name")
}

// codeDigest is the one-way digest under which a used code is remembered after
// its pending row expired: domain separated SHA-256 of the canonical code.
func codeDigest(c identity.PairingCode) string {
	h := sha256.Sum256([]byte("ai-minion pairing code used v1\x00" + c.Canonical()))
	return hex.EncodeToString(h[:])
}

// suffixed returns base for n = 1 and base-n otherwise, cut so that the result
// stays within the Worker name limit.
func suffixed(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	return cutBytes(base, ops.MaxWorkerNameLen-len(suffix)) + suffix
}

// CleanWorkerName turns the name a Worker announced into a valid Worker name:
// whitespace, colons and control characters become hyphens, the result is cut
// to the limit, and an empty result is "worker".
func CleanWorkerName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError, r == ':', unicode.IsSpace(r), unicode.IsControl(r):
			r = '-'
		}
		b.WriteRune(r)
	}
	name := strings.Trim(cutBytes(b.String(), ops.MaxWorkerNameLen), "-")
	if name == "" || !ops.ValidWorkerName(name) {
		return "worker"
	}
	return name
}

// cutBytes cuts s to at most n bytes on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// end removes a pending pairing and tells its Worker connection (if it still
// waits) and its claimer (if one is in flight) why.
func (ps *Pairings) end(p *pending, err error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.endLocked(p, err)
}

func (ps *Pairings) endLocked(p *pending, err error) {
	if p.ended {
		return
	}
	p.ended = true
	p.goneErr = err
	close(p.gone)
	ps.removeLocked(p)
	if p.cur != nil {
		p.cur.finish(claimOutcome{err: err})
	}
}

func (ps *Pairings) removeLocked(p *pending) {
	if ps.live[p.id] == p {
		delete(ps.live, p.id)
		if ps.perIP[p.ipKey]--; ps.perIP[p.ipKey] <= 0 {
			delete(ps.perIP, p.ipKey)
		}
	}
}

// Sweep expires the pending pairings whose code is older than its lifetime
// (their Worker connections are answered and closed) and deletes the expired
// rows. A pairing whose handshake is running is left to finish. It returns the
// number of pairings it expired.
func (ps *Pairings) Sweep(ctx context.Context) int {
	now := ps.now()
	n := 0
	cutoff := store.UnixMS(now)
	ps.mu.Lock()
	for _, p := range ps.live {
		if p.cur == nil && !now.Before(p.expires) {
			ps.endLocked(p, errExpired())
			n++
		} else if p.cur != nil {
			// A handshake in flight still needs its row (to count a failed
			// attempt or to complete the pairing): keep it until it ends.
			cutoff = min(cutoff, store.UnixMS(p.expires))
		}
	}
	ps.mu.Unlock()
	if _, err := ps.store.PurgePairings(ctx, cutoff); err != nil {
		ps.log.Warn("pairing: cannot delete expired pairings", "error", err.Error())
	}
	return n
}

// Start runs [Pairings.Sweep] every SweepInterval until [Pairings.Close].
func (ps *Pairings) Start() {
	ps.mu.Lock()
	if ps.started || ps.closed {
		ps.mu.Unlock()
		return
	}
	ps.started = true
	ps.stopped.Add(1)
	ps.mu.Unlock()
	go func() {
		defer ps.stopped.Done()
		t := time.NewTicker(ps.sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ps.stop:
				return
			case <-t.C:
				ps.Sweep(context.Background())
			}
		}
	}()
}

// Close refuses new connections and claims, ends every pending pairing and
// stops the sweep. It is safe to call twice.
func (ps *Pairings) Close() {
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		return
	}
	ps.closed = true
	close(ps.stop)
	for _, p := range ps.live {
		ps.endLocked(p, pairingErr(proto.CodeWorkerOffline, "the Hub is shutting down"))
	}
	ps.mu.Unlock()
	ps.stopped.Wait()
}

func (ps *Pairings) record(ctx context.Context, ev audit.Event) {
	if ps.auditor == nil {
		return
	}
	if err := ps.auditor.Record(context.WithoutCancel(ctx), ev); err != nil {
		ps.log.Warn("pairing: cannot audit", "action", ev.Action, "error", err.Error())
	}
}

// EnablePairing turns on the pair_code mode of the connect endpoint: it builds
// the pairing service from the options (the store, limiter and auditor of the
// Service by default), registers it as the handler of the mode and as the gate
// that supplies the CPace password, and starts its expiry sweep, which
// [Service.Close] stops. Without it the Hub refuses pair_code with
// invalid_argument. It can be called once.
func (s *Service) EnablePairing(o PairingOptions) (*Pairings, error) {
	if o.Store == nil {
		ps, ok := s.store.(PairingStore)
		if !ok {
			return nil, errors.New("workers: the store has no pending_pairings table")
		}
		o.Store = ps
	}
	if o.Limiter == nil {
		o.Limiter = s.limiter
	}
	if o.Auditor == nil {
		o.Auditor = s.handler.cfg.Auditor
	}
	if o.Logger == nil {
		o.Logger = s.handler.log
	}
	s.mu.Lock()
	if s.pairings != nil {
		s.mu.Unlock()
		return nil, errors.New("workers: pairing is already enabled")
	}
	s.mu.Unlock()
	ps, err := NewPairings(o)
	if err != nil {
		return nil, err
	}
	if err := s.handler.Handle(transport.ModePairCode, ps); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.pairings = ps
	s.mu.Unlock()
	s.handler.setPairing(ps)
	ps.Start()
	return ps, nil
}

// Pairings returns the pairing service, or nil before [Service.EnablePairing].
func (s *Service) Pairings() *Pairings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pairings
}
