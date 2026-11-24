package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/worker/presence"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// OwnerID is the stable Hub id of the owner (Owner is its display name).
	OwnerID string
	Kind    string
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
	// HubCheck is how the pinned Hub was checked (store.HubCheck*; "" means
	// none), HubCheckAt when, and HubCheckAnchor the full fingerprint the
	// operator checked ("" when none was). They belong to the pin: a pin that
	// moves along a signed transition chain keeps them, a new enrollment sets
	// them again.
	HubCheck       string
	HubCheckAt     time.Time
	HubCheckAnchor string
	// HubCheckLineage are the full fingerprints (separated by spaces) of the
	// keys the pin moved through by verified signed transitions since the
	// anchor was checked, the current pin not included (TUF: each one is an
	// authenticated predecessor of the pin).
	HubCheckLineage string
	// EnrolledAt is when the enrollment was first stored (set by the store).
	EnrolledAt time.Time
}

// Check returns HubCheck, with "" read as none.
func (e Enrollment) Check() string {
	if e.HubCheck == "" {
		return store.HubCheckNone
	}
	return e.HubCheck
}

// Welcome returns the welcome data stored in e.
func (e Enrollment) Welcome() store.WelcomeInfo {
	return store.WelcomeInfo{
		WorkerID: e.WorkerID, Name: e.Name, Owner: e.Owner, OwnerID: e.OwnerID, Kind: e.Kind, PreviewOnly: e.Policy.PreviewOnly,
	}
}

// EnrollmentRepo is the persistence port of [Enrollments]; *store.Store
// implements it.
type EnrollmentRepo interface {
	SaveEnrollment(ctx context.Context, e store.Enrollment) error
	LoadEnrollment(ctx context.Context) (store.Enrollment, error)
	UpdatePin(ctx context.Context, pub ed25519.PublicKey, seq uint64, passed ...ed25519.PublicKey) error
	UpdatePinRatchet(ctx context.Context, pub ed25519.PublicKey, seq, maxSerial uint64, certSHA256 []byte, passed ...ed25519.PublicKey) error
	SavePairResume(ctx context.Context, r store.PairResume) error
	LoadPairResume(ctx context.Context) (store.PairResume, error)
	DeletePairResume(ctx context.Context) (bool, error)
	DeletePairResumeIf(ctx context.Context, pairingID, code string) (bool, error)
	UpdateWelcome(ctx context.Context, w store.WelcomeInfo) error
	SetHubCheck(ctx context.Context, pub ed25519.PublicKey, check, anchor string) error
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

	wipeHook atomic.Pointer[func()] // called on an enrollment wipe; see SetWipeHook
}

// SetWipeHook registers what a running daemon does when the enrollment is
// wiped (a signed revoke, a reset, a new identity): it clears the stored usage
// document. Without a hook (the CLI processes) a wipe removes the usage
// documents of the profile from disk, and leaves the presence record, which a
// running daemon of that profile owns.
func (e *Enrollments) SetWipeHook(f func()) {
	e.wipeHook.Store(&f)
}

func (e *Enrollments) clearPublished() {
	var hook func()
	if p := e.wipeHook.Load(); p != nil {
		hook = *p
	}
	switch {
	case hook != nil:
		hook()
	case e.stateDir != "":
		if l, err := presenceLayout(e.stateDir); err == nil {
			_ = presence.ClearUsageFiles(l) // best effort: a wipe must not fail on it
		}
	}
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
		OwnerID:    row.OwnerID,
		Kind:       row.Kind,
		Policy:     HubPolicy{PreviewOnly: row.PreviewOnly},
		HubCheck:   row.HubCheck,
		HubCheckAt: row.HubCheckAt,
		EnrolledAt: row.EnrolledAt,

		HubCheckAnchor:  row.HubCheckAnchor,
		HubCheckLineage: row.HubCheckLineage,
	}, true, nil
}

// SetHubCheck records how the pinned Hub was checked (store.HubCheck*) and
// the fingerprint that was checked (anchor, "" for none), but only while pin
// is still the stored pin: a check of one pin never marks another
// (store.ErrNotEnrolled otherwise). Entering or leaving the mismatch state
// moves the generation, so a running connector starts over and applies it.
//
// A stored mismatch (the quarantine) is never left this way: the change is
// refused with [ErrQuarantined], under the lock, so that a start that read
// the enrollment before `verify-hub` stored the mismatch cannot undo it. Only
// [VerifyHub] with the full pinned key leaves it (and --forget removes it).
func (e *Enrollments) SetHubCheck(ctx context.Context, pin identity.Pin, check, anchor string) error {
	return e.setHubCheck(ctx, pin, check, anchor, false)
}

// ErrQuarantined is returned by [Enrollments.SetHubCheck] for a change that
// would leave the stored mismatch.
var ErrQuarantined = errors.New("agent: this Worker is quarantined (hub_check mismatch)")

func (e *Enrollments) setHubCheck(ctx context.Context, pin identity.Pin, check, anchor string, leaveQuarantine bool) error {
	return e.locked(ctx, func() error {
		cur, ok, err := e.Load(ctx)
		if err != nil {
			return err
		}
		if !ok || !pin.Pub.Equal(cur.Pin.Pub) {
			return store.ErrNotEnrolled
		}
		if cur.Check() == store.HubCheckMismatch && check != store.HubCheckMismatch && !leaveQuarantine {
			return ErrQuarantined
		}
		if (cur.Check() == store.HubCheckMismatch) != (check == store.HubCheckMismatch) {
			if err := e.bumpLocked(); err != nil {
				return err
			}
		}
		return e.repo.SetHubCheck(ctx, pin.Pub, check, anchor)
	})
}

// ForgetPin removes the enrollment when its pinned Hub key is exactly pinned
// (`minion worker verify-hub --forget --pinned`): the Worker must pair or
// enroll again and never re-trusts the disputed pin. It returns
// store.ErrNotEnrolled when the Worker is not paired and errPinnedDiffers
// when the stored pin is another key.
func (e *Enrollments) ForgetPin(ctx context.Context, pinned identity.Fingerprint) error {
	return e.locked(ctx, func() error {
		cur, ok, err := e.Load(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return store.ErrNotEnrolled
		}
		if !pinned.Matches(cur.Pin.Pub) {
			return ErrPinnedDiffers
		}
		if _, err := e.repo.DeletePairResume(ctx); err != nil {
			return fmt.Errorf("delete the pairing resume record: %w", err)
		}
		// The removal is recorded first: a start must not enroll again by
		// itself from an invitation key that is still configured (a mounted
		// file, an environment variable, a service's command line), which
		// would restore this pairing with the same Worker key.
		if err := e.markForgotten(forgottenKeys(pinned, cur)...); err != nil {
			return err
		}
		_, err = e.wipe(ctx)
		return err
	})
}

// ForgottenHubFile is the file in the state directory that records the Hub
// keys removed with `minion worker verify-hub --forget` (one full fingerprint
// per line). While it exists, a start does not enroll from a configured
// invitation key; a new pairing or enrollment by an explicit command (worker
// pair, the setup wizard) and `minion worker reset` remove it.
const ForgottenHubFile = "hub-forgotten"

// forgottenKeys are the keys of the removed pairing: the pin and the keys it
// was reached from (the checked anchor and the signed lineage), so the record
// names the Hub's root as well as its current key.
func forgottenKeys(pinned identity.Fingerprint, cur Enrollment) []string {
	keys := []string{string(pinned)}
	if cur.HubCheckAnchor != "" {
		keys = append(keys, cur.HubCheckAnchor)
	}
	return append(keys, strings.Fields(cur.HubCheckLineage)...)
}

func (e *Enrollments) markForgotten(keys ...string) error {
	if e.stateDir == "" {
		return nil
	}
	prev, _ := ForgottenHubs(e.stateDir)
	lines := prev
	for _, k := range keys {
		if !slices.Contains(lines, k) {
			lines = append(lines, k)
		}
	}
	if err := identity.WritePrivate(filepath.Join(e.stateDir, ForgottenHubFile), []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return fmt.Errorf("agent: record the removed Hub key: %w", err)
	}
	return nil
}

// ForgottenHubs returns the Hub keys removed with --forget on the state
// directory (none when the file does not exist).
func ForgottenHubs(stateDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, ForgottenHubFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(raw)), nil
}

// ClearForgottenHubs removes the record of removed Hub keys (an explicit new
// enrollment, or a reset).
func ClearForgottenHubs(stateDir string) error {
	if stateDir == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(stateDir, ForgottenHubFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agent: remove %s: %w", ForgottenHubFile, err)
	}
	return nil
}

// ErrPinnedDiffers is returned by [Enrollments.ForgetPin] when the given
// fingerprint is not the stored pin.
var ErrPinnedDiffers = errors.New("agent: the fingerprint is not the pinned Hub key")

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
	if err := protectEnrollmentFromRollback(e.stateDir); err != nil {
		return err
	}
	if err := e.repo.SaveEnrollment(ctx, store.Enrollment{
		HubPub:      enr.Pin.Pub,
		PinSeq:      enr.Pin.Seq,
		WorkerID:    enr.WorkerID,
		Name:        enr.Name,
		Owner:       enr.Owner,
		OwnerID:     enr.OwnerID,
		Kind:        enr.Kind,
		PreviewOnly: enr.Policy.PreviewOnly,
		MaxSerial:   enr.Ratchet.MaxSerial,
		CertSHA256:  enr.Ratchet.CertSHA256,

		HubCheck:        enr.Check(),
		HubCheckAt:      enr.HubCheckAt,
		HubCheckAnchor:  enr.HubCheckAnchor,
		HubCheckLineage: enr.HubCheckLineage,
	}); err != nil {
		return err
	}
	// An explicit new enrollment ends the record of a removed pairing, but
	// only once it is durably stored (the store commits with
	// synchronous=FULL): a failed or interrupted save keeps the record, so a
	// start can never enroll again from an invitation key of the removed Hub.
	return ClearForgottenHubs(e.stateDir)
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
// passed are the keys the verified chain went through between the stored pin
// and pin (they join the lineage of a checked enrollment).
func (e *Enrollments) CommitRatchetIfLive(ctx context.Context, gen uint64, pin identity.Pin, r transport.Ratchet, live func() bool,
	passed ...ed25519.PublicKey) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		return e.repo.UpdatePinRatchet(ctx, pin.Pub, pin.Seq, r.MaxSerial, r.CertSHA256, passed...)
	})
}

// ApplyWelcomeIfLive is [Enrollments.ApplyWelcomeIf] with an expiry check
// inside the guard, immediately before the write. before, when not nil, runs
// inside the guard after the generation and expiry checks passed and before
// the write: a change it makes (the connector enforces a new restriction
// there) happens only for a welcome that is still current and live, and it
// stays when the write itself fails.
func (e *Enrollments) ApplyWelcomeIfLive(ctx context.Context, gen uint64, w HubWelcome, live func() bool, before func()) error {
	return e.lockedIf(ctx, gen, func() error {
		if live != nil && !live() {
			return errGuardExpired
		}
		if before != nil {
			before()
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

// DeletePairResumeIf deletes the pairing resume record of a retired code under
// the state guard, only while the generation is still gen and only when the
// stored record is the one identified by pairingID and code. A moved generation
// returns [ErrEnrollmentChanged]; a record that is not this one (a successor
// pairing saved its own after `--new-code`) is kept and no error is returned.
func (e *Enrollments) DeletePairResumeIf(ctx context.Context, gen uint64, pairingID, code string) error {
	return e.lockedIf(ctx, gen, func() error {
		_, err := e.repo.DeletePairResumeIf(ctx, pairingID, code)
		return err
	})
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
		WorkerID: w.WorkerID, Name: w.Name, Owner: w.Owner, OwnerID: w.OwnerID, Kind: w.Kind, PreviewOnly: w.Policy.PreviewOnly,
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
	// A deliberate removal (a signed revoke of the Hub, a reset, a new
	// identity) during an update trial: a rollback must not restore the
	// pre-migration database with the removed pairing (rollbackTrustHold).
	if err := protectEnrollmentFromRollback(e.stateDir); err != nil {
		return false, err
	}
	if err := e.bumpLocked(); err != nil {
		return false, err
	}
	_ = e.saveHubURL("") // the address belongs to the enrollment that ends here
	// The row goes first: the cleanup of the published document may wait for a
	// reader (Windows) for longer than the caller's deadline, and the pairing
	// must be gone by then. The generation moved already, so nothing new is
	// published meanwhile.
	wiped, err := e.repo.WipeEnrollment(ctx)
	e.clearPublished()
	return wiped, err
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
