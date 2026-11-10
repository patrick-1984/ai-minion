package workers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// MaxChainLinks is the most links the Hub keeps in its transition chain. The
// whole chain travels in the Hub hello, a raw handshake frame of at most 64
// KiB (AD-6); a link is about 300 bytes of JSON, so this bound leaves a wide
// margin. Reaching it is a reason to re-enroll Workers on a fresh identity
// (the compromise path of AD-5) rather than to rotate again.
const MaxChainLinks = 128

// PendingKeyFile is the name, next to identity.key, of the new key between
// its creation and the completion of a rotation.
const PendingKeyFile = IdentityFile + ".next"

// RotateAction is the audit action of a Hub identity rotation.
const RotateAction = "hub_identity.rotate"

// ErrChainFull is returned by [Rotator.Rotate] when the transition chain has
// [MaxChainLinks] links.
var ErrChainFull = fmt.Errorf("workers: the transition chain already has %d links and cannot grow; "+
	"replace the Hub identity and re-enroll the Workers instead", MaxChainLinks)

// RotationStore is the persistence port of [Rotator] and of the chain source;
// *store.Store implements it.
type RotationStore interface {
	Transitions(ctx context.Context) ([]store.TransitionRow, error)
	RotateHubIdentity(ctx context.Context, p store.RotateParams) error
	// MakeDurable forces every committed transaction to disk and fails with
	// an error wrapping store.ErrCommitNotDurable when it cannot prove it.
	MakeDurable(ctx context.Context) error
}

// durableTimeout bounds one confirmation of a commit's durability. It runs
// on a context detached from the request, because a request that is
// canceled right after the commit must not leave the commit unconfirmed.
const durableTimeout = 30 * time.Second

// ErrRotationNotDurable is returned by [Rotator.Rotate] when the rotation is
// recorded in the database but the database could not prove it is on disk.
// The Hub keeps its previous identity and does not publish the new one until
// it is confirmed: Workers pin the new key the moment they see it, and a
// power loss that restored the old database would strand them. Calling
// Rotate again confirms the commit and completes the same rotation (it does
// not rotate a second time).
var ErrRotationNotDurable = errors.New("workers: the rotation is recorded but the database could not make it durable")

// InvitationRevoker revokes invitation keys. Rotation revokes every
// outstanding invitation key, because an invitation names the fingerprint of
// the Hub key it was created under (AD-5, AD-7). Story 4.5 provides the
// implementation; it must be idempotent.
type InvitationRevoker interface {
	// RevokeAllInvitations revokes every invitation key that is not yet
	// revoked and returns how many it revoked now.
	RevokeAllInvitations(ctx context.Context) (int, error)
}

// NoInvitations is the [InvitationRevoker] of a Hub without invitation keys.
type NoInvitations struct{}

// RevokeAllInvitations implements [InvitationRevoker]; there is nothing to do.
func (NoInvitations) RevokeAllInvitations(context.Context) (int, error) { return 0, nil }

// RotateActor is who asked for the rotation, as it is audited.
type RotateActor struct {
	// ID is the user or API key id of the admin.
	ID string
	// ActingAs is the user an admin acted as (R45); empty otherwise.
	ActingAs string
}

// RotateResult describes a completed rotation. It holds only public values.
type RotateResult struct {
	// Seq is the position of the new link in the chain (1 for the first).
	Seq uint64
	// OldFingerprint and NewFingerprint are the fingerprints of the replaced
	// and of the new Hub key.
	OldFingerprint identity.Fingerprint
	NewFingerprint identity.Fingerprint
	CreatedAt      time.Time
	// RevokedInvitations is how many invitation keys the rotation revoked.
	RevokedInvitations int
}

// RotatorOptions configures [NewRotator]. Identity, DataDir and Store are
// required.
type RotatorOptions struct {
	Identity *Identity
	// DataDir holds identity.key.
	DataDir string
	Store   RotationStore
	// Invitations revokes the outstanding invitation keys; the default is
	// [NoInvitations].
	Invitations InvitationRevoker
	// Now returns the current time; the default is time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Rotator performs the planned rotation of the Hub identity key (AD-5): it
// creates a new Ed25519 identity key, appends a transition record signed by
// the old key, switches the running Hub to the new key and revokes the
// outstanding invitation keys. Workers pinned to any key of the chain follow
// it on their next connect.
type Rotator struct {
	id    *Identity
	dir   string
	store RotationStore
	now   func() time.Time
	log   *slog.Logger

	mu          sync.Mutex // serializes rotations; guards invitations and unconfirmed
	invitations InvitationRevoker
	// unconfirmed is a rotation that committed but whose durability is not
	// confirmed: neither handshakes nor identity.key use its key yet.
	unconfirmed *unconfirmedRotation
}

// unconfirmedRotation is the state kept between a commit that is not known
// to be on disk and its confirmation.
type unconfirmedRotation struct {
	key *identity.Key
	res RotateResult
}

// NewRotator validates o and returns the rotator.
func NewRotator(o RotatorOptions) (*Rotator, error) {
	switch {
	case o.Identity == nil:
		return nil, errors.New("workers: the rotator needs the Hub identity")
	case o.DataDir == "":
		return nil, errors.New("workers: the rotator needs the data directory")
	case o.Store == nil:
		return nil, errors.New("workers: the rotator needs a store")
	}
	r := &Rotator{id: o.Identity, dir: o.DataDir, store: o.Store, invitations: o.Invitations, now: o.Now, log: o.Logger}
	if r.invitations == nil {
		r.invitations = NoInvitations{}
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// SetInvitations sets the revoker of invitation keys (Story 4.5 wires it).
func (r *Rotator) SetInvitations(inv InvitationRevoker) {
	if inv == nil {
		inv = NoInvitations{}
	}
	r.mu.Lock()
	r.invitations = inv
	r.mu.Unlock()
}

// Rotate performs one rotation on behalf of actor.
//
// Order of effects, so that no failure can strand a Worker:
//
//  1. Outstanding invitation keys are revoked (idempotent).
//  2. The new key is written, private, to identity.key.next.
//  3. One database transaction appends the transition record, replaces the
//     hub_identity row and writes the audit row. This is the commit point;
//     the store then forces the commit to disk (WAL checkpoint), because the
//     database runs with synchronous=NORMAL and nothing else may rely on the
//     new identity before it is durable. When the checkpoint fails (also when
//     the request was canceled meanwhile) it is retried on a detached
//     context; if that fails too the Hub keeps the old identity and the call
//     returns [ErrRotationNotDurable]: a later call confirms and completes
//     this rotation.
//  4. Once the commit is confirmed durable the running Hub switches to the
//     new key, then identity.key.next replaces identity.key. A crash between
//     3 and the rename is completed at the next start
//     ([LoadOrCreateIdentity], which confirms durability again); an earlier
//     rotation whose rename failed is completed before step 2 starts.
//  5. Invitation keys created since step 1 are revoked too.
//
// Before step 3 a failure leaves the old identity in force (revoked
// invitations stay revoked). After it the rotation is done and an error
// reports only what remains to finish.
func (r *Rotator) Rotate(ctx context.Context, actor RotateActor) (RotateResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ev := audit.Event{Actor: actor.ID, ActingAs: actor.ActingAs, Action: RotateAction, Outcome: "success"}
	if err := ev.Validate(); err != nil {
		return RotateResult{}, err
	}
	// A rotation whose commit was not confirmed durable is confirmed and
	// completed first; its result is the answer of this call.
	if r.unconfirmed != nil {
		return r.finishUnconfirmed(ctx)
	}
	chain, err := r.store.Transitions(ctx)
	if err != nil {
		return RotateResult{}, err
	}
	if len(chain) >= MaxChainLinks {
		return RotateResult{}, ErrChainFull
	}
	seq := uint64(1)
	if n := len(chain); n > 0 {
		seq = chain[n-1].Seq + 1
	}

	// An earlier rotation that committed but could not replace identity.key
	// leaves the current key only in identity.key.next: finish it first, or
	// refuse, so that this rotation never overwrites or deletes that file.
	if err := r.completePending(); err != nil {
		return RotateResult{}, err
	}

	revoked, err := r.invitations.RevokeAllInvitations(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("workers: revoke the invitation keys: %w", err)
	}

	oldKey := r.id.Key()
	newKey, err := identity.GenerateKey()
	if err != nil {
		return RotateResult{}, fmt.Errorf("workers: generate the new Hub identity: %w", err)
	}
	now := r.now()
	tr, err := identity.NewTransition(oldKey, newKey.Public(), seq, now)
	if err != nil {
		return RotateResult{}, fmt.Errorf("workers: sign the transition: %w", err)
	}
	if err := tr.Verify(); err != nil {
		return RotateResult{}, fmt.Errorf("workers: the new transition does not verify: %w", err)
	}
	pending := filepath.Join(r.dir, PendingKeyFile)
	if err := identity.SaveKey(pending, newKey); err != nil {
		return RotateResult{}, fmt.Errorf("workers: write the new Hub identity %s: %w", pending, err)
	}
	ev.Target = newKey.Fingerprint().String()
	err = r.store.RotateHubIdentity(ctx, store.RotateParams{
		Expect:     oldKey.Public(),
		Transition: rowOf(tr),
		Identity: store.HubIdentityRow{
			Pub: bytes.Clone(newKey.Public()), Fingerprint: newKey.Fingerprint().String(), CreatedAt: tr.CreatedAt.UnixMilli(),
		},
		Audit: &store.AuditRow{
			ID: store.NewID(), TS: tr.CreatedAt.UnixMilli(), Actor: ev.Actor, ActingAs: ev.ActingAs,
			Action: ev.Action, Target: ev.Target, Outcome: ev.Outcome,
		},
	})
	res := RotateResult{
		Seq: seq, OldFingerprint: oldKey.Fingerprint(), NewFingerprint: newKey.Fingerprint(),
		CreatedAt: tr.CreatedAt, RevokedInvitations: revoked,
	}
	switch {
	case errors.Is(err, store.ErrCommitNotDurable):
		// Committed, but the database could not prove the commit is on disk
		// (a checkpoint failure, or a request canceled between COMMIT and
		// the checkpoint). Retry the confirmation detached from the request.
		// Until it succeeds nothing may use the new key: not a handshake, not
		// identity.key. identity.key.next stays; the next start completes it
		// if the commit is on disk, or drops it if the commit was lost.
		if cerr := r.confirmDurable(); cerr != nil {
			r.unconfirmed = &unconfirmedRotation{key: newKey, res: res}
			r.log.Error("hub identity rotation committed but is not confirmed durable; keeping the previous identity",
				"seq", seq, "fingerprint", res.NewFingerprint.String(), "error", cerr)
			return RotateResult{}, fmt.Errorf("%w (%v); the Hub keeps its previous identity and %s is kept; "+
				"call the rotation again to confirm and complete it", ErrRotationNotDurable, cerr, PendingKeyFile)
		}
	case err != nil:
		// Not committed: the pending file holds only the new key, never the
		// current one (completePending ran above), so it can go.
		_ = os.Remove(pending)
		return RotateResult{}, fmt.Errorf("workers: record the rotation: %w", err)
	}
	return r.publish(ctx, newKey, res)
}

// confirmDurable asks the database to prove that every commit is on disk. It
// runs on a context that is detached from the request but bounded.
func (r *Rotator) confirmDurable() error {
	ctx, cancel := context.WithTimeout(context.Background(), durableTimeout)
	defer cancel()
	return r.store.MakeDurable(ctx)
}

// finishUnconfirmed confirms the durability of the rotation that committed
// without proof, then publishes it. It fails, leaving the old identity in
// force, while the proof is missing.
func (r *Rotator) finishUnconfirmed(ctx context.Context) (RotateResult, error) {
	u := r.unconfirmed
	if err := r.confirmDurable(); err != nil {
		return RotateResult{}, fmt.Errorf("%w (%v); the Hub keeps its previous identity and %s is kept; "+
			"call the rotation again to confirm and complete it", ErrRotationNotDurable, err, PendingKeyFile)
	}
	r.unconfirmed = nil
	return r.publish(ctx, u.key, u.res)
}

// publish makes a rotation whose commit is confirmed durable the identity of
// the running Hub: it switches the key, replaces identity.key with the
// pending file and revokes the invitation keys created meanwhile. The error
// reports only what remains to finish; the rotation is done.
func (r *Rotator) publish(ctx context.Context, newKey *identity.Key, res RotateResult) (RotateResult, error) {
	var errs []error
	r.id.swap(newKey)
	r.log.Info("hub identity rotated", "seq", res.Seq, "old_fingerprint", res.OldFingerprint.String(),
		"fingerprint", res.NewFingerprint.String(), "revoked_invitations", res.RevokedInvitations)

	pending := filepath.Join(r.dir, PendingKeyFile)
	if err := os.Rename(pending, filepath.Join(r.dir, IdentityFile)); err != nil {
		errs = append(errs, fmt.Errorf("workers: the rotation is recorded and active, but %s could not replace %s (%w); "+
			"it replaces it at the next start or before the next rotation", PendingKeyFile, IdentityFile, err))
	}
	more, err := r.invitations.RevokeAllInvitations(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("workers: the rotation is done, but revoking the invitation keys created meanwhile failed: %w", err))
	}
	res.RevokedInvitations += more
	return res, errors.Join(errs...)
}

// completePending finishes a committed rotation whose pending key file was
// never renamed. While this process runs, identity.key.next can only be the
// current key (a rotation that did not commit removes it): it then replaces
// identity.key, and when that fails the new rotation is refused. A pending
// file that is not the current key is the leftover of a rotation that never
// committed and is removed.
func (r *Rotator) completePending() error {
	pending := filepath.Join(r.dir, PendingKeyFile)
	key, err := identity.LoadKey(pending)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("workers: an earlier rotation left an unreadable %s (%w); resolve it before rotating again", pending, err)
	}
	if bytes.Equal(key.Public(), r.id.Key().Public()) {
		// identity.key is replaced only for a commit that is on disk: a
		// power loss must never leave the old database next to the new key.
		if err := r.confirmDurable(); err != nil {
			return fmt.Errorf("workers: an earlier rotation is recorded but the database cannot confirm it is durable (%w); "+
				"no new rotation starts until it can", err)
		}
		if err := os.Rename(pending, filepath.Join(r.dir, IdentityFile)); err != nil {
			return fmt.Errorf("workers: an earlier rotation is recorded but %s could not replace %s (%w); "+
				"no new rotation starts until it does", PendingKeyFile, IdentityFile, err)
		}
		r.log.Info("hub identity rotation completed before the next one", "fingerprint", key.Fingerprint().String())
		return nil
	}
	if err := os.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("workers: remove the abandoned %s: %w", pending, err)
	}
	return nil
}

func rowOf(t identity.Transition) store.TransitionRow {
	return store.TransitionRow{
		Seq: t.Seq, OldPub: bytes.Clone(t.OldPub), NewPub: bytes.Clone(t.NewPub),
		CreatedAt: t.CreatedAt.UnixMilli(), SigByOld: bytes.Clone(t.SigByOld),
	}
}

func transitionOf(r store.TransitionRow) identity.Transition {
	return identity.Transition{
		Seq: r.Seq, OldPub: identity.B64(bytes.Clone(r.OldPub)), NewPub: identity.B64(bytes.Clone(r.NewPub)),
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(), SigByOld: identity.B64(bytes.Clone(r.SigByOld)),
	}
}

// ChainSource returns the function the connect [Handler] calls for the
// transition chain it sends in connect mode: the whole stored chain, oldest
// link first. The Hub does not know which key a connecting Worker is pinned
// to before the handshake finished, so it sends every link and the Worker
// follows the chain from its own pin: the handshake skips the links before
// the pinned key and verifies the rest (transport.WorkerHandshake). The chain
// is read per connection, so a rotation is visible at once.
func ChainSource(repo interface {
	Transitions(ctx context.Context) ([]store.TransitionRow, error)
}) func(ctx context.Context) ([]identity.Transition, error) {
	return func(ctx context.Context) ([]identity.Transition, error) {
		rows, err := repo.Transitions(ctx)
		if err != nil {
			return nil, err
		}
		chain := make([]identity.Transition, len(rows))
		for i, r := range rows {
			chain[i] = transitionOf(r)
		}
		return chain, nil
	}
}

// chainUpTo returns the links of chain that lead to the key pub, the key the
// Hub signs its handshake with: a Worker applies the whole chain it receives
// and requires the result to be the key it meets, so a link whose new key the
// Hub does not use (yet) must not be sent. A chain that does not mention pub
// is returned unchanged, except that a pub equal to the first old key (no
// link applies) yields an empty chain.
func chainUpTo(chain []identity.Transition, pub []byte) []identity.Transition {
	for i, t := range chain {
		if bytes.Equal(t.NewPub, pub) {
			return chain[:i+1]
		}
	}
	if len(chain) > 0 && bytes.Equal(chain[0].OldPub, pub) {
		return nil
	}
	return chain
}

// recoverRotation completes or discards a rotation that was interrupted. The
// pending key file is the new identity when the database already records its
// public key (the transaction committed, the rename did not happen): it then
// replaces identity.key. Otherwise the transaction never committed and the
// pending file is dropped.
func recoverRotation(ctx context.Context, dir string, repo IdentityRepository, log *slog.Logger) error {
	pending := filepath.Join(dir, PendingKeyFile)
	key, err := identity.LoadKey(pending)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("workers: load the pending Hub identity %s: %w "+
			"(an interrupted rotation; remove the file if the Hub identity in %s is the one this database records)", pending, err, IdentityFile)
	}
	recorded, ok, err := repo.HubIdentity(ctx)
	if err != nil {
		return err
	}
	if ok && bytes.Equal(recorded.Pub, key.Public()) {
		// The record is visible, but a power loss could still take it away
		// (synchronous=NORMAL): force it to disk before identity.key is
		// replaced, or the old database would meet the new key.
		if d, can := repo.(interface{ MakeDurable(context.Context) error }); can {
			if err := d.MakeDurable(ctx); err != nil {
				return fmt.Errorf("workers: the interrupted Hub identity rotation is recorded but the database cannot confirm it is durable (%w); "+
					"%s stays; start again once the database can flush", err, PendingKeyFile)
			}
		}
		if err := os.Rename(pending, filepath.Join(dir, IdentityFile)); err != nil {
			return fmt.Errorf("workers: complete the interrupted Hub identity rotation: %w", err)
		}
		log.Info("hub identity rotation completed after an interrupted run", "fingerprint", key.Fingerprint().String())
		return nil
	}
	if err := os.Remove(pending); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("workers: remove the abandoned pending Hub identity %s: %w", pending, err)
	}
	log.Info("removed the pending Hub identity of a rotation that never committed", "file", pending)
	return nil
}
