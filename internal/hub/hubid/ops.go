package hubid

import (
	"bytes"
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Names of the injection points (Hooks.Point), one per boundary of the crash
// tables. The comment names the table row that a crash at the point shows.
const (
	PointStaged        = "a.staged"       // A1: N staged and synced, transaction not committed
	PointSigned        = "a.signed"       // after the last signature, signers destroyed
	PointPreCommit     = "a.pre-commit"   // immediately before the transaction
	PointCommitted     = "a.committed"    // A2: committed, not yet confirmed
	PointDurable       = "a.durable"      // A3: confirmed, nothing swapped or renamed
	PointSwapped       = "a.swapped"      // A5: swapped (sessions closed by D7), nothing renamed
	PointRKRenamed     = "x.rk-renamed"   // X4: RN renamed over RK, N not yet over K
	PointRenamed       = "a.renamed"      // A4/X5: all renamed
	PointRNStaged      = "x.rn-staged"    // X1: RN and N staged
	PointWStaged       = "w.rn-staged"    // W1
	PointWCommitted    = "w.committed"    // W2
	PointWDurable      = "w.durable"      // W3
	PointWRenamed      = "w.renamed"      // W4
	PointBSRootWritten = "bs.rk-written"  // BS1/BS2: RK written, row not committed
	PointBSCommitted   = "bs.committed"   // BS3: row committed, not confirmed
	PointBSDurable     = "bs.durable"     // BS4: confirmed, plain residue still present
	PointBSPlainGone   = "bs.plain-gone"  // after the plain file was removed, before the first certificate
	PointAfterUnseal   = "after-unseal"   // between the unseal and the signing (panic tests)
	PointBeforeStaging = "before-staging" // after the gate and the unseal, before any file is written
)

// IssueOptions are the per-issuance options (section 6): they apply to that
// issuance only and nothing is stored.
type IssueOptions struct {
	// ValidityDays is 1..400; 0 means DefaultValidityDays.
	ValidityDays int
	// SerialFloor: the new serial must be greater than it (0 = none).
	SerialFloor int64
	// AllowFarFloor allows a floor above now + 366 days.
	AllowFarFloor bool
}

// Result describes a completed operation. It holds only public values.
type Result struct {
	// Kind is "passphrase_set", "reissue", "rotate" or "passphrase_change".
	Kind            string
	Serial          uint64
	NotBefore       time.Time
	NotAfter        time.Time
	RootFingerprint string
	OpFingerprint   string
	// Generation is the activation generation after the operation.
	Generation uint64
}

// validatePassphrase applies the rules of section 10.3 that need no secret
// state. The message never contains the passphrase or its length.
func (s *Service) validatePassphrase(actor Actor, pass, confirm string) error {
	switch {
	case utf8.RuneCountInString(pass) < identity.MinPassphraseRunes:
		return newErr(CodeInvalidArgument, "the passphrase is too short")
	case len(pass) > MaxPassphraseBytes:
		return newErr(CodeInvalidArgument, "the passphrase is too long")
	case pass != confirm:
		return newErr(CodeInvalidArgument, "the confirmation does not match the passphrase")
	case s.rejec != nil && s.rejec(actor, pass):
		return newErr(CodeInvalidArgument, "this passphrase is not allowed")
	}
	return nil
}

func (s *Service) validateOptions(o IssueOptions) (int, error) {
	days := o.ValidityDays
	if days == 0 {
		days = DefaultValidityDays
	}
	if days < 1 || days > MaxValidityDays {
		return 0, newErr(CodeInvalidArgument, "the validity must be 1 to 400 days")
	}
	if o.SerialFloor < 0 {
		return 0, newErr(CodeInvalidArgument, "the serial floor must not be negative")
	}
	if !o.AllowFarFloor && o.SerialFloor > s.nowMs()+farFloorDays*dayMs {
		return 0, newErr(CodeInvalidArgument, "the serial floor is more than 366 days ahead; repeat with the far-floor allowance if it is intended")
	}
	return days, nil
}

// allocateSerial is the rule of section 6: max(last_issued + 1, now_ms,
// floor + 1), checked.
func allocateSerial(lastIssued, nowMs, floor int64) (uint64, error) {
	const maxSerial = int64(identity.MaxHubCertSerial)
	if lastIssued >= maxSerial || floor >= maxSerial {
		return 0, newErr(CodeInvalidArgument, "the serial space is exhausted")
	}
	serial := lastIssued + 1
	if nowMs > serial {
		serial = nowMs
	}
	if floor+1 > serial {
		serial = floor + 1
	}
	return uint64(serial), nil
}

// begin starts every identity change: it completes an unconfirmed one (D3),
// runs the recovery and returns a completed Result when the call is the
// repetition of the pending command.
//
// resumed is true when the pending change was the sealing of a passphrase_set
// (not its first-certificate activation, which finishes below like any other
// activation: generation bump, swap, session close) and the call is its repetition: the setup is now confirmed and recovered (state B2), but the
// first certificate was never issued. The caller must then issue it with the
// passphrase of this request; nothing secret was kept between the calls.
func (s *Service) begin(ctx context.Context, kind string) (res *Result, resumed bool, err error) {
	if s.dead {
		return nil, false, newErr(CodeFailClosed, "the service stopped; restart the Hub")
	}
	if !s.started {
		return nil, false, newErr(CodeFailClosed, "the service was not started")
	}
	if p := s.unconfirmed; p != nil {
		if err := s.confirm(1); err != nil {
			code := CodeConflict // every other identity change is refused with conflict
			if p.kind == kind {
				code = CodeNotDurable
			}
			return nil, false, &Error{Code: code, Msg: "the previous " + p.kind + " is recorded but not confirmed durable; repeat the same command to confirm it", Err: err}
		}
		s.setUnconfirmed(nil)
		if p.kind == "passphrase_set" && kind == p.kind && !p.activation {
			// The sealed root is confirmed; the repetition goes on to issue
			// the first certificate (recovery first: it removes plain residue).
			if err := s.recoverLocked(ctx, false); err != nil {
				return nil, false, err
			}
			return nil, true, nil
		}
		r, err := p.finish(ctx)
		if rerr := s.recoverLocked(ctx, false); err == nil {
			err = rerr
		}
		if p.kind == kind {
			return &r, false, err
		}
		if err != nil {
			return nil, false, err
		}
		return nil, false, newErr(CodeConflict, "a pending "+p.kind+" was confirmed and completed; repeat your command")
	}
	if err := s.recoverLocked(ctx, false); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == CodeFailClosed {
			// A running Hub refuses further changes with conflict until the
			// pending file operation succeeds (D5, D6).
			return nil, false, &Error{Code: CodeConflict, Msg: e.Msg, Err: e.Err}
		}
		return nil, false, err
	}
	return nil, false, nil
}

// gateAndUnseal runs, in the order of section 8.1 item 6, the lock and
// allowance gate, then the key derivation (section 9.2 step 2). A refusal by
// the gate consumes nothing; an attempt that reaches the derivation consumes
// allowance and, if it fails, counts as a failure.
func (s *Service) gateAndUnseal(ctx context.Context, actor Actor, scope, pass string, row store.HubIdentityV2) (*identity.RootSigner, error) {
	if err := s.gate(ctx, scope); err != nil {
		return nil, err
	}
	sealed, err := s.readFile(RootFile)
	if err != nil || !bytes.Equal(sha(sealed), row.RootFileSHA256) {
		s.seal.Dummy(pass) // early failure: one bounded dummy derivation
		return nil, s.failPassphrase(actor, scope)
	}
	signer, err := s.seal.Open(sealed, pass)
	if signer != nil && s.hooks.Signer != nil {
		s.hooks.Signer(signer)
	}
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrWrongPassphrase), errors.Is(err, identity.ErrSealedRootPayload):
		// The key derivation already ran in the sealer: no second one.
		return nil, s.failPassphrase(actor, scope)
	default:
		// Refused before any derivation (not an age file, not exactly one
		// scrypt recipient at factor 18, passphrase too short): the one
		// bounded dummy derivation keeps the time equal.
		s.seal.Dummy(pass)
		return nil, s.failPassphrase(actor, scope)
	}
	if !bytes.Equal(signer.Public(), row.Pub) {
		signer.Destroy()
		return nil, s.failPassphrase(actor, scope)
	}
	s.clearIntent(scope) // the right passphrase: the attempt is not a failure
	return signer, nil
}

// clearIntent clears the pending-attempt mark of a scope after an attempt that
// ended without a failure. Best effort and not confirmed: if the clear is lost
// the next start counts the attempt as a failure (conservative).
func (s *Service) clearIntent(scope string) {
	if !s.alive() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), durableTimout)
	defer cancel()
	if err := s.st.ClearUnsealIntent(ctx, scope); err != nil {
		s.log.Error("hub identity: could not clear the passphrase attempt mark; the next start counts the attempt as a failure", "error", err)
	}
}

// reconcileIntents runs at start: an attempt that began (its mark is durable,
// it was confirmed before the derivation) and was never settled counts as a
// failure of its scope, with the lock rule and the audit rows. The caller
// holds the mutex.
func (s *Service) reconcileIntents(ctx context.Context) error {
	row, ok, err := s.st.HubIdentityV2(ctx)
	if err != nil || !ok {
		return err
	}
	for _, sc := range []struct {
		scope, channel string
		st             store.ScopeState
	}{{store.ScopeWeb, ChannelUI, row.Web}, {store.ScopeCLI, ChannelCLI, row.CLI}} {
		if !sc.st.Pending {
			continue
		}
		s.log.Warn("hub identity: an unsettled passphrase attempt was found at start; it counts as a failure", "scope", sc.scope)
		if _, err := s.st.RecordUnsealFailure(ctx, sc.scope, s.nowMs(), SystemActor, sc.channel); err != nil {
			return err
		}
		if err := s.confirm(2); err != nil {
			s.debtOf(sc.scope).unconfirmed = true
		}
	}
	return nil
}

// failureRecord is a passphrase failure that is not yet proven on disk.
type failureRecord struct {
	actor, channel string
	at             int64
}

// scopeDebt is what the service owes the database for one scope: failures it
// could not record and commits it could not confirm. While a scope owes
// anything the scope is treated as locked (conservative: a failure or an
// attempt that may be lost must not be lost silently). It is settled, in
// order, at the start of the next attempt of that scope. Guarded by the
// rotation mutex.
type scopeDebt struct {
	unrecorded  []failureRecord
	unconfirmed bool
}

func (s *Service) debtOf(scope string) *scopeDebt {
	d := s.debt[scope]
	if d == nil {
		d = &scopeDebt{}
		s.debt[scope] = d
	}
	return d
}

// unproven is the answer while a scope cannot prove its counters durable: the
// same one as a lock, with a retry time one base lock period ahead.
func (s *Service) unproven(cause error) error {
	return &Error{Code: CodeRateLimited, Msg: "Too many attempts. Try again later.", RetryAt: time.UnixMilli(s.nowMs() + store.LockBase), Err: cause}
}

// settle records the failures that could not be recorded and confirms the
// commits that could not be confirmed (section 7, D3 for the lockout state).
func (s *Service) settle(scope string) error {
	d := s.debt[scope]
	if d == nil {
		return nil
	}
	for len(d.unrecorded) > 0 {
		r := d.unrecorded[0]
		ctx, cancel := context.WithTimeout(context.Background(), durableTimout)
		_, err := s.st.RecordUnsealFailure(ctx, scope, r.at, r.actor, r.channel)
		cancel()
		if err != nil {
			return s.unproven(err)
		}
		d.unrecorded = d.unrecorded[1:]
		d.unconfirmed = true
	}
	if d.unconfirmed {
		if err := s.confirm(1); err != nil {
			return s.unproven(err)
		}
		d.unconfirmed = false
	}
	delete(s.debt, scope)
	return nil
}

// gate is the lock and allowance check; it maps the store's limit errors.
// The counted attempt is confirmed durable before the derivation starts; if it
// cannot be confirmed the attempt stays counted (consumed), nothing is
// derived, and the scope is treated as locked until the commit is proved.
func (s *Service) gate(ctx context.Context, scope string) error {
	if err := s.settle(scope); err != nil {
		return err
	}
	err := s.st.BeginUnsealAttempt(ctx, scope, s.nowMs(), s.maxOp)
	var le *store.LimitError
	if errors.As(err, &le) {
		return &Error{Code: CodeRateLimited, Msg: "Too many attempts. Try again later.", RetryAt: time.UnixMilli(le.Until), Allowance: !le.Locked, Err: err}
	}
	if err != nil {
		return err
	}
	if err := s.confirm(1); err != nil {
		s.debtOf(scope).unconfirmed = true
		s.clearIntent(scope) // no derivation will run for this attempt
		s.log.Error("hub identity: a counted passphrase attempt could not be confirmed durable; the attempt is consumed and the scope is treated as locked", "error", err)
		return s.unproven(err)
	}
	return nil
}

// failPassphrase records the failure (counter, lock and audit rows in one
// transaction), confirms it durable and returns the one generic answer of
// section 10.5. A failure that cannot be recorded or confirmed is owed to the
// database (scopeDebt) and locks the scope until it is settled.
func (s *Service) failPassphrase(actor Actor, scope string) error {
	if !s.alive() {
		return newErr(CodePassphraseInvalid, errPassphraseRejected)
	}
	at := s.nowMs()
	ctx, cancel := context.WithTimeout(context.Background(), durableTimout)
	_, err := s.st.RecordUnsealFailure(ctx, scope, at, actor.ID, actor.Channel)
	cancel()
	switch {
	case err != nil:
		s.log.Error("hub identity: could not record a passphrase failure; the scope is treated as locked until it is recorded", "error", err)
		d := s.debtOf(scope)
		d.unrecorded = append(d.unrecorded, failureRecord{actor: actor.ID, channel: actor.Channel, at: at})
	default:
		if cerr := s.confirm(2); cerr != nil {
			s.log.Error("hub identity: a passphrase failure is recorded but not confirmed durable; the scope is treated as locked until it is", "error", cerr)
			s.debtOf(scope).unconfirmed = true
		}
	}
	return newErr(CodePassphraseInvalid, errPassphraseRejected)
}

func (s *Service) track(sg *identity.RootSigner) *identity.RootSigner {
	if s.hooks.Signer != nil && sg != nil {
		s.hooks.Signer(sg)
	}
	return sg
}

// notDurable builds the D3 answer.
func notDurable(err error, what string) error {
	return &Error{Code: CodeNotDurable, Msg: what + " is recorded but the database could not prove it is on disk; the Hub keeps its previous state; repeat the same command to confirm and complete it", Err: err}
}
