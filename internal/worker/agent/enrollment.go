package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// WorkerKeyFile is the name of the Worker's identity key file inside the state
// directory (AD-5): one long-lived Ed25519 key, separate from any TLS key,
// stored as a private file (mode 0600, or a protected DACL on Windows).
const WorkerKeyFile = "worker.key"

// ErrNoWorkerKey is returned by [Enrollments.LoadKey] when the Worker has not
// created its key pair yet.
var ErrNoWorkerKey = errors.New("agent: the Worker has no identity key yet")

// HubPolicy is the Hub-side policy that welcome delivers (AD-12). The Worker
// combines it with its local policy, and local restrictions always win.
type HubPolicy struct {
	PreviewOnly bool `json:"preview_only"`
}

// Enrollment is the Worker's pairing with its Hub: the pinned Hub key and what
// the Hub said in welcome.
type Enrollment struct {
	// Pin is the Hub's public key and its position in the transition chain.
	Pin identity.Pin
	// WorkerID, Name, Owner and Kind come from welcome; they are empty until
	// the first welcome arrives (enrollment commits the pin first, AD-7).
	WorkerID string
	Name     string
	Owner    string
	Kind     string
	// Policy is the Hub policy from the last welcome.
	Policy HubPolicy
	// HubURL is the address of the Hub that authenticated this enrollment, as
	// the enrolling command used it. `minion worker pair --new-code` revokes
	// the old identity there, whatever the configuration says by then. It is
	// empty for an enrollment that did not record it (an invitation, or one
	// stored by an older version); the configured address is the fallback.
	HubURL string
	// Ratchet is the Hub certificate memory scoped to Pin.Pub: the highest
	// serial seen and the hash of that certificate (design section 6). Its
	// Root is Pin.Pub for a stored enrollment.
	Ratchet transport.Ratchet
	// EnrolledAt is when the enrollment was first stored (set by the store).
	EnrolledAt time.Time
}

// Welcome returns the welcome data stored in e.
func (e Enrollment) Welcome() store.WelcomeInfo {
	return store.WelcomeInfo{
		WorkerID: e.WorkerID, Name: e.Name, Owner: e.Owner, Kind: e.Kind, PreviewOnly: e.Policy.PreviewOnly,
	}
}

// EnrollmentRepo is the persistence port of [Enrollments]; *store.Store
// implements it.
type EnrollmentRepo interface {
	SaveEnrollment(ctx context.Context, e store.Enrollment) error
	LoadEnrollment(ctx context.Context) (store.Enrollment, error)
	UpdatePin(ctx context.Context, pub ed25519.PublicKey, seq uint64) error
	UpdatePinRatchet(ctx context.Context, pub ed25519.PublicKey, seq, maxSerial uint64, certSHA256 []byte) error
	SavePairResume(ctx context.Context, r store.PairResume) error
	LoadPairResume(ctx context.Context) (store.PairResume, error)
	DeletePairResume(ctx context.Context) (bool, error)
	UpdateWelcome(ctx context.Context, w store.WelcomeInfo) error
	WipeEnrollment(ctx context.Context) (bool, error)
}

// Enrollments is the one API for the Worker's identity key, its Hub pin and
// its enrollment state. The connector reads and rewrites it on every
// connection; enrollment (invitation, pairing code), reset and removal use it
// to commit and wipe. It never expires or refreshes anything: the pin and the
// enrollment end only through [Enrollments.Wipe] (AD-9, R94).
//
// The enrollment has a generation that [Enrollments.Commit] and
// [Enrollments.Wipe] advance. A connection attempt remembers the generation it
// started from, and the conditional writers ([Enrollments.RewritePinIf],
// [Enrollments.ApplyWelcomeIf], [Enrollments.WipeIf]) refuse to write once the
// generation has moved: an old Hub's welcome, pin or revoke can neither revive
// nor delete a pairing that was reset or replaced meanwhile.
//
// The generation is shared between processes: it lives in a small file in the
// state directory (see enrollment_epoch.go) that every writer advances under a
// cross-process lock before it touches the enrollment. `minion worker pair` (a
// separate process with its own Enrollments value) therefore invalidates the
// running service's connection and its conditional writers, and the service
// notices the change on its next [Enrollments.Moved] check.
//
// Safe for concurrent use, also by several processes on one state directory.
type Enrollments struct {
	repo     EnrollmentRepo
	stateDir string
	changed  chan struct{}

	mu    sync.Mutex    // serializes the writers of this process and guards gen and moved
	gen   uint64        // the generation last seen (the epoch file, or in memory without a state directory)
	moved chan struct{} // closed (and replaced) when gen changes

	unannounced bool // gen was advanced by this process and moved was not closed yet
}

// ErrEnrollmentChanged is returned by the conditional writers when the
// enrollment was committed or wiped after the caller read it.
var ErrEnrollmentChanged = errors.New("agent: the enrollment changed meanwhile")

// keyInitMu serializes the creation of a Worker key in this process, whatever
// Enrollments value asks: two first callers must end up with one identity.
var keyInitMu sync.Mutex

// NewEnrollments returns the enrollment API over repo, with the key file in
// stateDir.
func NewEnrollments(repo EnrollmentRepo, stateDir string) *Enrollments {
	return &Enrollments{repo: repo, stateDir: stateDir, changed: make(chan struct{}, 1), moved: make(chan struct{})}
}

// KeyPath returns the path of the Worker key file.
func (e *Enrollments) KeyPath() string { return filepath.Join(e.stateDir, WorkerKeyFile) }

// LoadKey loads the Worker key pair. It returns [ErrNoWorkerKey] when none
// exists yet; a key file that other users can read is refused (AD-5, A-11).
func (e *Enrollments) LoadKey() (*identity.Key, error) {
	k, err := identity.LoadKey(e.KeyPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoWorkerKey
	}
	if err != nil {
		return nil, fmt.Errorf("agent: load the Worker key %s: %w", e.KeyPath(), err)
	}
	return k, nil
}

// LoadOrCreateKey returns the Worker key pair and creates and saves it first
// when it does not exist. Enrollment calls it before it connects: the key pair
// is persisted before the first handshake, so a retry after a crash presents
// the same identity (AD-7).
func (e *Enrollments) LoadOrCreateKey() (*identity.Key, error) {
	// Load, generate and save are one step: without the lock two first callers
	// would both succeed with different keys and only the last save would
	// survive on disk.
	keyInitMu.Lock()
	defer keyInitMu.Unlock()
	k, err := e.LoadKey()
	if !errors.Is(err, ErrNoWorkerKey) {
		return k, err
	}
	k, err = identity.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("agent: create the Worker key: %w", err)
	}
	if err := identity.SaveKey(e.KeyPath(), k); err != nil {
		return nil, fmt.Errorf("agent: save the Worker key %s: %w", e.KeyPath(), err)
	}
	return k, nil
}

// lockEnroll takes the cross-process enrollment lock of the state directory
// (<state>/.enroll.lock, the lock that an invitation enrollment holds from the
// key creation to the commit, Story 4.6). Everything that creates or replaces
// the Worker key, or commits an enrollment, takes it first, so that no Worker
// key is created or replaced while an invitation enrollment runs. The order is
// always this lock first, then the state lock of [Enrollments.locked]. Without
// a state directory (unit tests) it is a no-op.
func (e *Enrollments) lockEnroll(ctx context.Context) (release func(), err error) {
	if e.stateDir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create the state directory %s: %w", e.stateDir, err)
	}
	return lockEnrollment(ctx, filepath.Join(e.stateDir, enrollLockName))
}

// Load returns the stored enrollment. ok is false when the Worker is not
// enrolled.
func (e *Enrollments) Load(ctx context.Context) (enr Enrollment, ok bool, err error) {
	row, err := e.repo.LoadEnrollment(ctx)
	if errors.Is(err, store.ErrNotEnrolled) {
		return Enrollment{}, false, nil
	}
	if err != nil {
		return Enrollment{}, false, err
	}
	return Enrollment{
		HubURL:     e.loadHubURL(),
		Pin:        identity.Pin{Pub: row.HubPub, Seq: row.PinSeq},
		Ratchet:    transport.Ratchet{Root: row.HubPub, MaxSerial: row.MaxSerial, CertSHA256: row.CertSHA256},
		WorkerID:   row.WorkerID,
		Name:       row.Name,
		Owner:      row.Owner,
		Kind:       row.Kind,
		Policy:     HubPolicy{PreviewOnly: row.PreviewOnly},
		EnrolledAt: row.EnrolledAt,
	}, true, nil
}

// Commit stores the pin and the welcome data together, replacing an earlier
// enrollment, and wakes a connector that waits for one. Enrollment calls it
// and reports success only after it returned (AD-7).
func (e *Enrollments) Commit(ctx context.Context, enr Enrollment) error {
	return e.locked(ctx, func() error { return e.commit(ctx, enr) })
}

// CommitIf is [Enrollments.Commit] for the enrollment of generation gen: it
// stores nothing and returns [ErrEnrollmentChanged] when the generation has
// moved, whoever moved it. A pairing that started on an unpaired Worker uses it,
// so that it can never replace an enrollment that another pairing, the setup
// wizard or an invitation stored meanwhile.
func (e *Enrollments) CommitIf(ctx context.Context, gen uint64, enr Enrollment) error {
	return e.lockedIf(ctx, gen, func() error { return e.commit(ctx, enr) })
}

// commit moves the generation and stores enr. The caller holds the locks.
func (e *Enrollments) commit(ctx context.Context, enr Enrollment) error {
	// The generation moves first: whatever still runs on the old
	// enrollment (in this process or another) is stale before the new one
	// is stored. A failed save leaves a moved generation, which only makes
	// the connectors start over.
	if err := e.bumpLocked(); err != nil {
		return err
	}
	if err := e.saveHubURL(enr.HubURL); err != nil {
		return err
	}
	return e.repo.SaveEnrollment(ctx, store.Enrollment{
		HubPub:      enr.Pin.Pub,
		PinSeq:      enr.Pin.Seq,
		WorkerID:    enr.WorkerID,
		Name:        enr.Name,
		Owner:       enr.Owner,
		Kind:        enr.Kind,
		PreviewOnly: enr.Policy.PreviewOnly,
		MaxSerial:   enr.Ratchet.MaxSerial,
		CertSHA256:  enr.Ratchet.CertSHA256,
	})
}

// RewritePin atomically replaces the pin after a transition chain was applied
// (AD-5). The new pin must move forward in the chain.
func (e *Enrollments) RewritePin(ctx context.Context, pin identity.Pin) error {
	return e.locked(ctx, func() error { return e.repo.UpdatePin(ctx, pin.Pub, pin.Seq) })
}

// RewritePinIf is [Enrollments.RewritePin] for the enrollment of generation
// gen; it returns [ErrEnrollmentChanged] when the generation has moved.
func (e *Enrollments) RewritePinIf(ctx context.Context, gen uint64, pin identity.Pin) error {
	return e.lockedIf(ctx, gen, func() error { return e.repo.UpdatePin(ctx, pin.Pub, pin.Seq) })
}

// CommitRatchetIf is the commit of step 8 of the connect handshake: for the
// enrollment of generation gen it rewrites the pin (if it moved along the
// chain) and the certificate ratchet in ONE transaction. It returns
// [ErrEnrollmentChanged] when the generation has moved. It is needed also when
// only the serial rose.
func (e *Enrollments) CommitRatchetIf(ctx context.Context, gen uint64, pin identity.Pin, r transport.Ratchet) error {
	return e.lockedIf(ctx, gen, func() error {
		return e.repo.UpdatePinRatchet(ctx, pin.Pub, pin.Seq, r.MaxSerial, r.CertSHA256)
	})
}

// SavePairResumeIfLive is [Enrollments.SavePairResumeIf] with an expiry check
// inside the guard, immediately before the write: nothing is stored
// (errGuardExpired) when live reports false. A nil live always passes.
func (e *Enrollments) SavePairResumeIfLive(ctx context.Context, gen uint64, r store.PairResume, live func() bool) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.repo.SavePairResume(ctx, r)
	})
}

// CommitRatchetIfLive is [Enrollments.CommitRatchetIf] with an expiry check
// inside the guard, immediately before the write.
func (e *Enrollments) CommitRatchetIfLive(ctx context.Context, gen uint64, pin identity.Pin, r transport.Ratchet, live func() bool) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.repo.UpdatePinRatchet(ctx, pin.Pub, pin.Seq, r.MaxSerial, r.CertSHA256)
	})
}

// ApplyWelcomeIfLive is [Enrollments.ApplyWelcomeIf] with an expiry check
// inside the guard, immediately before the write.
func (e *Enrollments) ApplyWelcomeIfLive(ctx context.Context, gen uint64, w HubWelcome, live func() bool) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.applyWelcome(ctx, w)
	})
}

// CommitLive is [Enrollments.Commit] with an expiry check inside the guard,
// immediately before the commit (an invitation enrollment, whose welcome must
// still be covered by the Hub certificate when it is stored).
func (e *Enrollments) CommitLive(ctx context.Context, enr Enrollment, live func() bool) error {
	return e.locked(ctx, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.commit(ctx, enr)
	})
}

// SavePairResumeIf stores the pairing resume record durably for the pairing of
// generation gen (design section 12.2); the pairer sends sig_worker only after
// it returned.
func (e *Enrollments) SavePairResumeIf(ctx context.Context, gen uint64, r store.PairResume) error {
	return e.lockedIf(ctx, gen, func() error { return e.repo.SavePairResume(ctx, r) })
}

// LoadPairResume returns the pairing resume record; ok is false when none exists.
func (e *Enrollments) LoadPairResume(ctx context.Context) (r store.PairResume, ok bool, err error) {
	r, err = e.repo.LoadPairResume(ctx)
	if errors.Is(err, store.ErrNoPairResume) {
		return store.PairResume{}, false, nil
	}
	return r, err == nil, err
}

// DeletePairResume removes the pairing resume record (expiry, or an abandoned
// identity).
func (e *Enrollments) DeletePairResume(ctx context.Context) error {
	_, err := e.repo.DeletePairResume(ctx)
	return err
}

// ApplyWelcome stores what the latest welcome said.
func (e *Enrollments) ApplyWelcome(ctx context.Context, w HubWelcome) error {
	return e.locked(ctx, func() error { return e.applyWelcome(ctx, w) })
}

// ApplyWelcomeIf is [Enrollments.ApplyWelcome] for the enrollment of
// generation gen; it returns [ErrEnrollmentChanged] when the generation has
// moved.
func (e *Enrollments) ApplyWelcomeIf(ctx context.Context, gen uint64, w HubWelcome) error {
	return e.lockedIf(ctx, gen, func() error { return e.applyWelcome(ctx, w) })
}

func (e *Enrollments) applyWelcome(ctx context.Context, w HubWelcome) error {
	return e.repo.UpdateWelcome(ctx, store.WelcomeInfo{
		WorkerID: w.WorkerID, Name: w.Name, Owner: w.Owner, Kind: w.Kind, PreviewOnly: w.Policy.PreviewOnly,
	})
}

// Wipe deletes the pin and the enrollment and nothing else: the key pair, the
// share selection, the session counter and the tmux sessions stay. It reports
// whether an enrollment existed and is idempotent. Only a signed revoke from
// the Hub and `minion worker reset` call it (AD-9).
func (e *Enrollments) Wipe(ctx context.Context) (wiped bool, err error) {
	err = e.locked(ctx, func() (err error) { wiped, err = e.wipe(ctx); return })
	return wiped, err
}

// ReplaceIdentity is the wipe of `minion worker pair --new-code`. With the
// state lock held throughout, it reloads the enrollment, runs keep (which
// stores the old identity durably as a pending revocation), deletes the pairing
// resume record of the old key, and wipes. An error from keep, or a record that
// cannot be deleted, aborts before the wipe. Holding the lock across all steps
// is what keeps a concurrent ratchet commit of the connector from being lost
// between the read and the wipe (see the lock hierarchy in enrollment_epoch.go).
func (e *Enrollments) ReplaceIdentity(ctx context.Context, keep func(enr Enrollment, enrolled bool) error) (wiped bool, err error) {
	err = e.locked(ctx, func() error {
		enr, enrolled, err := e.Load(ctx)
		if err != nil {
			return fmt.Errorf("read the enrollment: %w", err)
		}
		if err := keep(enr, enrolled); err != nil {
			return err
		}
		if _, err := e.repo.DeletePairResume(ctx); err != nil {
			return fmt.Errorf("delete the pairing resume record: %w", err)
		}
		var werr error
		wiped, werr = e.wipe(ctx)
		if werr != nil {
			return fmt.Errorf("wipe the old enrollment: %w", werr)
		}
		return nil
	})
	return wiped, err
}

// WipeIf is [Enrollments.Wipe] for the enrollment of generation gen; it
// returns [ErrEnrollmentChanged] when the generation has moved, so that a
// revoke of an old Hub cannot delete a replacement enrollment.
func (e *Enrollments) WipeIf(ctx context.Context, gen uint64) (wiped bool, err error) {
	err = e.lockedIf(ctx, gen, func() (err error) { wiped, err = e.wipe(ctx); return })
	return wiped, err
}

// errGuardExpired is returned by [Enrollments.WipeIfLive] and
// [Enrollments.CommitIfLive] when the Hub certificate of the connection ended
// before the change was applied.
var errGuardExpired = errors.New("agent: the Hub certificate expired before the change was applied")

// WipeIfLive is [Enrollments.WipeIf] with an expiry check inside the guard:
// live is called with the enrollment lock held, immediately before the wipe,
// and the wipe does not happen (errGuardExpired) when it reports false. The
// time spent waiting for the lock is thereby covered. A nil live always passes.
func (e *Enrollments) WipeIfLive(ctx context.Context, gen uint64, live func() bool) (wiped bool, err error) {
	err = e.lockedIf(ctx, gen, func() (err error) {
		if live != nil && !live() {
			return errGuardExpired
		}
		wiped, err = e.wipe(ctx)
		return
	})
	return wiped, err
}

// CommitIfLive is [Enrollments.CommitIf] with an expiry check inside the guard:
// live is called with the locks held, immediately before the commit, and
// nothing is stored (errGuardExpired) when it reports false.
func (e *Enrollments) CommitIfLive(ctx context.Context, gen uint64, enr Enrollment, live func() bool) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.commit(ctx, enr)
	})
}

// wipe moves the generation and deletes the enrollment. The caller holds the
// locks.
func (e *Enrollments) wipe(ctx context.Context) (bool, error) {
	if err := e.bumpLocked(); err != nil {
		return false, err
	}
	_ = e.saveHubURL("") // the address belongs to the enrollment that ends here
	return e.repo.WipeEnrollment(ctx)
}

// Watch returns the current generation of the enrollment and a channel that
// is closed when [Enrollments.Commit] or [Enrollments.Wipe] advances it, in this
// process or (once [Enrollments.Moved] or another Watch noticed) in another
// one. A caller takes it before it reads the enrollment, so that a change after
// the read is never missed; [Enrollments.Snapshot] does both atomically.
func (e *Enrollments) Watch() (gen uint64, changed <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.syncLocked()
	return e.gen, e.moved
}

// Changed delivers a value after [Enrollments.Commit] or [Enrollments.Wipe]
// ran in this process, so that an idle connector starts at once. One pending
// signal is kept.
func (e *Enrollments) Changed() <-chan struct{} { return e.changed }

func (e *Enrollments) signal() {
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

var _ EnrollmentRepo = (*store.Store)(nil)

// enrollmentHubFile holds the Hub address of the stored enrollment. It lives
// next to the epoch file, not in worker.db, because it is written under the same
// lock and needs no schema change; it is only read for an enrolled Worker and is
// replaced or removed by every commit and wipe.
const enrollmentHubFile = "enrollment.hub"

func (e *Enrollments) hubURLPath() string { return filepath.Join(e.stateDir, enrollmentHubFile) }

// saveHubURL records the Hub address of the enrollment being stored; an empty
// address removes the record. The caller holds the locks.
func (e *Enrollments) saveHubURL(url string) error {
	if e.stateDir == "" {
		return nil
	}
	if url == "" {
		if err := os.Remove(e.hubURLPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agent: remove the enrollment Hub address: %w", err)
		}
		return nil
	}
	if err := identity.WritePrivate(e.hubURLPath(), []byte(url+"\n")); err != nil {
		return fmt.Errorf("agent: store the enrollment Hub address: %w", err)
	}
	return nil
}

// loadHubURL returns the recorded Hub address, or "" when there is none.
func (e *Enrollments) loadHubURL() string {
	if e.stateDir == "" {
		return ""
	}
	raw, err := os.ReadFile(e.hubURLPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
