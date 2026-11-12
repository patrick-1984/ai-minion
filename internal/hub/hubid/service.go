package hubid

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// File names of the data directory (section 3).
const (
	RootFile      = "root.key.age"
	RootNextFile  = "root.key.age.next"
	OpFile        = "op.key"
	OpNextFile    = "op.key.next"
	PlainFile     = "identity.key"
	PlainNextFile = "identity.key.next"
)

// Limits and defaults.
const (
	// DefaultValidityDays is the validity of an issuance without --validity.
	DefaultValidityDays = 180
	// MaxValidityDays is the longest validity the certificate format allows.
	MaxValidityDays = 400
	// MaxPassphraseBytes is the longest accepted passphrase.
	MaxPassphraseBytes = 1024
	// MaxOpsPerHour is the hourly allowance of each scope.
	MaxOpsPerHour = 10
	// MaxChainLinks mirrors workers.MaxChainLinks (a Hub hello carries the chain).
	MaxChainLinks = 128
	// WarnWindow is how long before not_after the warnings start.
	WarnWindow = 30 * 24 * time.Hour
	// UrgentWindow is the last stretch where the warning is an error.
	UrgentWindow = 7 * 24 * time.Hour
	// SystemActor is the actor of the automatic expiry events.
	SystemActor = "system:identity-expiry"

	dayMs         = int64(24 * 60 * 60 * 1000)
	farFloorDays  = 366
	durableTimout = 30 * time.Second
	maxFileRead   = 8192
)

// State is the bootstrap state of section 9.7.
type State int

// The bootstrap states.
const (
	// StateUnknown: the service was not started.
	StateUnknown State = iota
	// StatePristine (B0): no identity yet; set-passphrase creates it.
	StatePristine
	// StatePlain (B1): a migrated 0.6 identity; the root is the plain identity.key.
	StatePlain
	// StateSealedNoCert (B2): sealed root, no certificate; re-issue issues the first one.
	StateSealedNoCert
	// StateEstablished (B3): sealed root and certificate.
	StateEstablished
)

// String returns the name of the state ("B0 pristine", and so on).
func (s State) String() string {
	switch s {
	case StatePristine:
		return "B0 pristine"
	case StatePlain:
		return "B1 plain identity"
	case StateSealedNoCert:
		return "B2 sealed, no certificate"
	case StateEstablished:
		return "B3 established"
	}
	return "unknown"
}

// Scope is the passphrase scope of a caller.
type Scope = string

// Channel names of the audit events.
const (
	ChannelUI  = "ui"
	ChannelCLI = "cli"
)

// Actor says who asks and through which channel. Channel selects the
// lockout scope: ChannelUI is the web scope, ChannelCLI the local command
// scope. For the command the transport layer passes ID "local:<uid>" taken
// from the kernel credentials of the socket peer.
type Actor struct {
	ID      string
	Channel string
}

func (a Actor) scope() (string, error) {
	switch a.Channel {
	case ChannelUI:
		return store.ScopeWeb, nil
	case ChannelCLI:
		return store.ScopeCLI, nil
	}
	return "", newErr(CodeInvalidArgument, "unknown channel")
}

// Store is the persistence port; *store.Store implements it.
type Store interface {
	HubIdentityV2(ctx context.Context) (store.HubIdentityV2, bool, error)
	BeginUnsealAttempt(ctx context.Context, scope string, nowMS, maxPerHour int64) error
	ClearUnsealIntent(ctx context.Context, scope string) error
	RecordUnsealFailure(ctx context.Context, scope string, nowMS int64, actor, channel string) (store.ScopeState, error)
	CreateSealedIdentity(ctx context.Context, id store.HubIdentityRow, rootSHA256 []byte, nowMS int64, audit []store.AuditRow) error
	RecordPlainIdentity(ctx context.Context, id store.HubIdentityRow) error
	SealExistingIdentity(ctx context.Context, expectPub, rootSHA256 []byte, nowMS int64, audit []store.AuditRow) error
	CommitIssuance(ctx context.Context, p store.IssuanceParams) error
	CommitPassphraseChange(ctx context.Context, expectPub, expectOld, newSHA256 []byte, scope string, nowMS int64, audit []store.AuditRow) error
	MarkExpiryWarning(ctx context.Context, day int64, audit store.AuditRow) (bool, error)
	MarkExpired(ctx context.Context, serial int64, audit store.AuditRow) (bool, error)
	Transitions(ctx context.Context) ([]store.TransitionRow, error)
	MakeDurable(ctx context.Context) error
}

// Hooks are the test seams. Every field may be nil.
type Hooks struct {
	// Point is called at the named boundaries of the crash tables (the Point*
	// constants). A nil result continues. ErrCrash simulates the death of the
	// process (no cleanup, the service is dead). Any other error is an
	// ordinary failure at that boundary and takes the normal error path; a
	// panic is a panic.
	Point func(name string) error
	// Rename replaces os.Rename for key files.
	Rename func(from, to string) error
	// SyncDir replaces the directory sync.
	SyncDir func(dir string) error
	// MakeDurable replaces Store.MakeDurable.
	MakeDurable func(ctx context.Context) error
	// Signer sees every RootSigner the service creates, so a test can check
	// that Destroy ran on the exit path.
	Signer func(*identity.RootSigner)
}

// Options configures [New]. DataDir and Store are required.
type Options struct {
	DataDir string
	Store   Store
	// Now is the wall clock; the default is time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// Sealer encrypts and decrypts the root; the default is the age scrypt
	// implementation of internal/identity (work factor 18).
	Sealer Sealer
	Hooks  Hooks
	// LockWait is how long a request waits for the rotation mutex (default 5 s).
	LockWait time.Duration
	// MaxOpsPerHour overrides the hourly allowance per scope (default 10).
	MaxOpsPerHour int64
	// CloseSessions closes every registered Worker session. It is called
	// after every activation (D7). It can be set later with SetSessionCloser.
	CloseSessions func()
	// RejectPassphrase, when set, vetoes a new passphrase (for example when
	// it equals the administrator's login password, section 10.3).
	RejectPassphrase func(actor Actor, passphrase string) bool
}

// Snapshot is what a handshake takes once and together at admission: the
// operational key, its certificate, the root public key and the activation
// generation (section 9.6).
type Snapshot struct {
	RootPub    ed25519.PublicKey
	OpKey      *identity.Key
	Cert       *identity.HubCert
	Generation uint64
}

// pendingOp is an activation or change that committed but is not confirmed
// durable (D3): nothing may use it until finish succeeds.
type pendingOp struct {
	kind string
	// activation is true when the pending change is a certificate activation
	// (finish swaps, renames and closes the sessions), false for the sealing
	// of a root that still needs its first certificate.
	activation bool
	finish     func(ctx context.Context) (Result, error)
}

// Service is the Hub identity service.
type Service struct {
	dir   string
	st    Store
	now   func() time.Time
	log   *slog.Logger
	seal  Sealer
	hooks Hooks
	wait  time.Duration
	maxOp int64
	rejec func(Actor, string) bool

	sem chan struct{} // the rotation mutex; a channel so waiting can time out

	// The fields below are guarded by holding sem (or, for state and snap,
	// are atomics read by anyone).
	started     bool
	dead        bool
	unconfirmed *pendingOp
	syncPending bool
	debt        map[string]*scopeDebt
	closedGen   uint64 // generation whose expiry already closed the sessions

	state atomic.Int32
	snap  atomic.Pointer[Snapshot]
	// pendingChange mirrors unconfirmed != nil for readers without the mutex
	// (InvitationRoot): a committed change is not confirmed durable yet.
	pendingChange atomic.Bool
	gen           atomic.Uint64

	closerMu sync.Mutex
	closer   func()
	// onActivate is called after every activation, after the sessions closed.
	onActivate func()
}

// New validates o and returns a service that is not yet started.
func New(o Options) (*Service, error) {
	switch {
	case o.DataDir == "":
		return nil, errors.New("hubid: the data directory is required")
	case o.Store == nil:
		return nil, errors.New("hubid: the store is required")
	}
	s := &Service{dir: o.DataDir, st: o.Store, now: o.Now, log: o.Logger, seal: o.Sealer, hooks: o.Hooks,
		wait: o.LockWait, maxOp: o.MaxOpsPerHour, rejec: o.RejectPassphrase, sem: make(chan struct{}, 1), closer: o.CloseSessions}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.seal == nil {
		s.seal = AgeSealer{}
	}
	if s.wait <= 0 {
		s.wait = 5 * time.Second
	}
	if s.maxOp <= 0 {
		s.maxOp = MaxOpsPerHour
	}
	s.debt = map[string]*scopeDebt{}
	s.gen.Store(1)
	return s, nil
}

// Open is New followed by Start.
func Open(ctx context.Context, o Options) (*Service, error) {
	s, err := New(o)
	if err != nil {
		return nil, err
	}
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// SetSessionCloser sets the function that closes every registered Worker
// session (I3 wires the registry). It runs after every activation.
func (s *Service) SetSessionCloser(f func()) {
	s.closerMu.Lock()
	s.closer = f
	s.closerMu.Unlock()
}

// SetActivationHook sets a function that runs after every activation (first
// certificate, re-issue, rotation), after the sessions were closed. The Hub's
// expiry timer uses it to re-arm itself at the new certificate's not_after.
// The function must not call back into an identity operation synchronously.
func (s *Service) SetActivationHook(f func()) {
	s.closerMu.Lock()
	s.onActivate = f
	s.closerMu.Unlock()
}

func (s *Service) closeSessions() {
	s.closerMu.Lock()
	f := s.closer
	s.closerMu.Unlock()
	if f != nil {
		f()
	}
}

func (s *Service) notifyActivated() {
	s.closerMu.Lock()
	h := s.onActivate
	s.closerMu.Unlock()
	if h != nil {
		h()
	}
}

// State returns the bootstrap state found by the last recovery.
func (s *Service) State() State { return State(s.state.Load()) }

// Generation returns the activation generation (1 at start; it rises with
// every first issuance, re-issue and rotation, never with a passphrase change).
func (s *Service) Generation() uint64 { return s.gen.Load() }

// Current returns the (operational key, certificate, root, generation) set
// that a Worker handshake may use. It fails with ErrNotServing until the
// Hub is in state B3 and again once the certificate expired (now >= not_after).
func (s *Service) Current() (Snapshot, error) {
	sn := s.snap.Load()
	if sn == nil {
		return Snapshot{}, newErr(CodeNotServing, "the Hub has no certificate yet")
	}
	if s.now().UnixMilli() >= sn.Cert.NotAfterMs {
		return Snapshot{}, &Error{Code: CodeNotServing, Msg: "the Hub certificate has expired", Err: identity.ErrCertExpired}
	}
	return *sn, nil
}

// setUnconfirmed records (p != nil) or clears the change that committed but
// is not confirmed durable. Called with the mutex held.
func (s *Service) setUnconfirmed(p *pendingOp) {
	s.unconfirmed = p
	s.pendingChange.Store(p != nil)
}

// InvitationRoot returns the root fingerprint that a new invitation key may
// carry: the root of the serving snapshot, the one Workers verify in the
// handshake. While the recorded root differs from it (a rotation committed but
// not confirmed durable keeps the previous snapshot serving, D3) it refuses
// with CodeConflict, so that no key names an unconfirmed root. A Hub that has
// not served yet (B1, B2) uses the recorded root unless a change of it is
// unconfirmed; with no root at all (B0) it returns "".
func (s *Service) InvitationRoot(ctx context.Context) (string, error) {
	row, ok, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return "", err
	}
	if !ok || row.Fingerprint == "" {
		return "", nil
	}
	if sn := s.snap.Load(); sn != nil {
		if !bytes.Equal(sn.RootPub, row.Pub) {
			return "", newErr(CodeConflict, "a Hub identity change is recorded but not confirmed yet; repeat it to confirm, then create the invitation")
		}
		return fingerprint(sn.RootPub), nil
	}
	if s.pendingChange.Load() {
		return "", newErr(CodeConflict, "a Hub identity change is recorded but not confirmed yet; repeat it to confirm, then create the invitation")
	}
	return row.Fingerprint, nil
}

// Admit is the registry check of section 9.6: a connection that took its
// snapshot at generation gen may register only while gen is still current and
// the certificate of that generation has not expired. The registry calls it
// under its registration lock, in the same critical section that adds the
// session; the expiry timer (Tick) closes the sessions under the same lock,
// so a handshake that stalled across the expiry is refused here and one that
// registered before it is closed by Tick. It takes no other lock.
func (s *Service) Admit(gen uint64) bool {
	sn := s.snap.Load()
	return sn != nil && sn.Generation == gen && s.gen.Load() == gen && s.now().UnixMilli() < sn.Cert.NotAfterMs
}

func (s *Service) point(name string) error {
	if s.hooks.Point == nil {
		return nil
	}
	err := s.hooks.Point(name)
	if errors.Is(err, ErrCrash) {
		s.dead = true
	}
	return err
}

func (s *Service) alive() bool { return !s.dead }

func (s *Service) nowMs() int64 { return s.now().UnixMilli() }

func (s *Service) makeDurable(ctx context.Context) error {
	if s.hooks.MakeDurable != nil {
		return s.hooks.MakeDurable(ctx)
	}
	return s.st.MakeDurable(ctx)
}

// confirm tries to prove the commit durable on a context detached from the
// request, once (attempts = 1) or with the single retry of D3 (attempts = 2).
func (s *Service) confirm(attempts int) error {
	var err error
	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), durableTimout)
		err = s.makeDurable(ctx)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// lock takes the rotation mutex, waiting at most the configured time.
func (s *Service) lock(ctx context.Context) error {
	t := time.NewTimer(s.wait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-t.C:
		return newErr(CodeConflict, "another identity operation is running")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) unlock() { <-s.sem }

func sha(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func fingerprint(pub []byte) string {
	f, err := identity.FingerprintOf(pub)
	if err != nil {
		return ""
	}
	return f.String()
}

func (s *Service) auditRow(actor Actor, action, target string) store.AuditRow {
	return store.AuditRow{ID: store.NewID(), TS: s.nowMs(), Actor: actor.ID, Action: action, Target: target, Outcome: "success"}
}

func fmtTarget(channel string, serial int64, rootPub, opPub []byte) string {
	t := "channel=" + channel
	if serial > 0 {
		t += fmt.Sprintf(" serial=%d", serial)
	}
	if len(rootPub) > 0 {
		t += " root=" + fingerprint(rootPub)
	}
	if len(opPub) > 0 {
		t += " op=" + fingerprint(opPub)
	}
	return t
}
