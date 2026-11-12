package hubid

import (
	"bytes"
	"context"
	"errors"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// SetPassphrase seals the root under a passphrase and issues the first
// certificate (section 9.7). It is accepted only in B0 (a new Hub: the root
// is created here) and B1 (a migrated 0.6 identity: the plain identity.key is
// sealed and removed); in B2 and B3 it answers CodeConflict and reads,
// unseals, writes and counts nothing. The fingerprint of a migrated identity
// does not change.
func (s *Service) SetPassphrase(ctx context.Context, actor Actor, pass, confirm string, opts IssueOptions) (Result, error) {
	scope, err := actor.scope()
	if err != nil {
		return Result{}, err
	}
	if err := s.validatePassphrase(actor, pass, confirm); err != nil {
		return Result{}, err
	}
	days, err := s.validateOptions(opts)
	if err != nil {
		return Result{}, err
	}
	if err := s.lock(ctx); err != nil {
		return Result{}, err
	}
	defer s.unlock()
	res, resumed, err := s.begin(ctx, "passphrase_set")
	if res != nil || err != nil {
		if res != nil {
			return *res, err
		}
		return Result{}, err
	}
	if resumed {
		return s.resumeFirstIssuance(ctx, actor, scope, pass, days, opts)
	}
	st := s.State()
	if st != StatePristine && st != StatePlain {
		return Result{}, newErr(CodeConflict, "a sealed root exists; use re-issue certificate")
	}
	row, haveRow, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	var seed []byte
	if st == StatePlain {
		if err := s.gate(ctx, scope); err != nil {
			return Result{}, err
		}
		s.clearIntent(scope) // no passphrase is checked against a hash here
		if seed, err = identity.ReadKeyFileSeed(s.path(PlainFile)); err != nil {
			return Result{}, failClosed("read %s: %v", s.path(PlainFile), err)
		}
	} else if seed, err = identity.NewRootSeed(); err != nil {
		return Result{}, err
	}
	defer identity.WipeBytes(seed)
	sealed, err := s.seal.Seal(seed, pass)
	if err != nil {
		return Result{}, err
	}
	signer, err := identity.RootSignerFromSeed(seed)
	if err != nil {
		return Result{}, err
	}
	defer s.track(signer).Destroy()
	identity.WipeBytes(seed)
	if st == StatePlain && !bytes.Equal(signer.Public(), row.Pub) {
		return Result{}, failClosed("%s does not hold the identity this database records", s.path(PlainFile))
	}
	return s.bootstrap(ctx, actor, scope, signer, sealed, pass, st == StatePlain, haveRow, days, opts)
}

// bootstrap seals, records and issues the first certificate (BS1 to BS5).
func (s *Service) bootstrap(ctx context.Context, actor Actor, scope string, signer *identity.RootSigner, sealed []byte, pass string,
	fromPlain, _ bool, days int, opts IssueOptions) (Result, error) {
	rootPub := signer.Public()
	committed := false
	defer func() {
		if !committed && s.alive() { // BS1, BS2: an unrecorded sealed file depends on nothing
			s.remove(RootFile)
		}
	}()
	if s.exists(RootFile) || s.exists(RootNextFile) {
		return Result{}, newErr(CodeConflict, "a sealed root file exists; resolve it first")
	}
	if err := s.writeFileDurable(RootFile, sealed); err != nil {
		return Result{}, err
	}
	if err := s.point(PointBSRootWritten); err != nil {
		return Result{}, err
	}
	// Prove that the file on disk and the passphrase agree.
	back, err := s.readFile(RootFile)
	if err != nil {
		return Result{}, err
	}
	if !s.verifyReadback(back, pass, rootPub) {
		return Result{}, newErr(CodeConflict, "the sealed root could not be verified after writing")
	}
	rootSHA := sha(back)
	now := s.nowMs()
	audit := []store.AuditRow{s.auditRow(actor, "hub_identity.passphrase_set", fmtTarget(actor.Channel, 0, rootPub, nil))}
	// The change counts as unconfirmed from before its commit until the
	// confirmation below: with no snapshot yet, InvitationRoot would otherwise
	// offer the recorded root in the window between the two. Every exit that
	// does not leave a recorded pending operation clears it again.
	s.pendingChange.Store(true)
	defer func() { s.pendingChange.Store(s.unconfirmed != nil) }()
	if fromPlain {
		err = s.st.SealExistingIdentity(ctx, rootPub, rootSHA, now, audit)
	} else {
		err = s.st.CreateSealedIdentity(ctx, store.HubIdentityRow{Pub: rootPub, Fingerprint: fingerprint(rootPub), CreatedAt: now}, rootSHA, now, audit)
	}
	if err != nil {
		return Result{}, mapStateErr(err)
	}
	committed = true
	res := Result{Kind: "passphrase_set", RootFingerprint: fingerprint(rootPub)}
	if err := s.point(PointBSCommitted); err != nil {
		return Result{}, err
	}
	if err := s.confirm(2); err != nil { // BS3: keep both files
		s.setUnconfirmed(&pendingOp{kind: "passphrase_set", finish: func(context.Context) (Result, error) { return res, nil }})
		return Result{}, notDurable(err, "the passphrase")
	}
	s.pendingChange.Store(s.unconfirmed != nil) // confirmed: the recorded root may be offered
	if err := s.point(PointBSDurable); err != nil {
		return Result{}, err
	}
	if fromPlain { // BS4: rule C, only after the confirmation above
		for _, f := range []string{PlainFile, PlainNextFile} {
			if s.exists(f) {
				if err := s.shred(f); err != nil {
					s.log.Error("hub identity: the plain identity file could not be removed; it is removed at the next start", "file", s.path(f), "error", err)
				}
			}
		}
	}
	s.state.Store(int32(StateSealedNoCert))
	if err := s.point(PointBSPlainGone); err != nil {
		return Result{}, err
	}
	// BS5: the first certificate, with the root still in memory; the
	// passphrase is not asked twice.
	row, _, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	return s.activate(ctx, &activation{kind: "passphrase_set", actor: actor, scope: scope, row: row, days: days, floor: opts.SerialFloor, signer: signer})
}

// verifyReadback proves that the sealed file just written opens with the
// passphrase and holds the key with public key want. The signer made for the
// check is an independent copy of the private key: it is destroyed here,
// right after the comparison, not when the operation returns.
func (s *Service) verifyReadback(back []byte, pass string, want []byte) bool {
	check, err := s.seal.Open(back, pass)
	if check == nil {
		return false
	}
	defer s.track(check).Destroy()
	return err == nil && bytes.Equal(check.Public(), want)
}

// resumeFirstIssuance finishes a passphrase setup whose commit was confirmed
// by this very request (the unconfirmed-setup retry): the sealed root is
// opened with the passphrase of this request and the first certificate is
// issued with its options, like Reissue in state B2.
func (s *Service) resumeFirstIssuance(ctx context.Context, actor Actor, scope, pass string, days int, opts IssueOptions) (Result, error) {
	if s.State() != StateSealedNoCert {
		return Result{}, newErr(CodeConflict, "the Hub is not waiting for its first certificate")
	}
	row, _, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	signer, err := s.gateAndUnseal(ctx, actor, scope, pass, row)
	if err != nil {
		return Result{}, err
	}
	defer s.track(signer).Destroy()
	return s.activate(ctx, &activation{kind: "passphrase_set", actor: actor, scope: scope, row: row, days: days, floor: opts.SerialFloor, signer: signer})
}

func mapStateErr(err error) error {
	if err != nil && (errors.Is(err, store.ErrIdentityState)) {
		return &Error{Code: CodeConflict, Msg: "the Hub identity changed meanwhile", Err: err}
	}
	return err
}

// Reissue creates a new operational key and a new certificate under the same
// root (sections 4 and 9.2). In B2 it issues the first certificate. It needs
// the passphrase. The root, its fingerprint, the pins and the invitations are
// untouched; all Worker sessions are closed.
func (s *Service) Reissue(ctx context.Context, actor Actor, pass string, opts IssueOptions) (Result, error) {
	scope, err := actor.scope()
	if err != nil {
		return Result{}, err
	}
	days, err := s.validateOptions(opts)
	if err != nil {
		return Result{}, err
	}
	if len(pass) == 0 || len(pass) > MaxPassphraseBytes {
		return Result{}, newErr(CodeInvalidArgument, "the passphrase is missing or too long")
	}
	if err := s.lock(ctx); err != nil {
		return Result{}, err
	}
	defer s.unlock()
	if res, _, err := s.begin(ctx, "reissue"); res != nil || err != nil {
		if res != nil {
			return *res, err
		}
		return Result{}, err
	}
	if st := s.State(); st != StateSealedNoCert && st != StateEstablished {
		return Result{}, newErr(CodeConflict, "no sealed root yet; set the passphrase first")
	}
	row, _, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	signer, err := s.gateAndUnseal(ctx, actor, scope, pass, row)
	if err != nil {
		return Result{}, err
	}
	defer s.track(signer).Destroy()
	if err := s.point(PointAfterUnseal); err != nil {
		return Result{}, err
	}
	return s.activate(ctx, &activation{kind: "reissue", actor: actor, scope: scope, row: row, days: days, floor: opts.SerialFloor, signer: signer})
}

// RotateMaster replaces the root: a new root sealed under newPass, a
// transition record signed by the old root, a new operational key and a
// certificate under the new root. All invitation keys are revoked. Only in B3.
func (s *Service) RotateMaster(ctx context.Context, actor Actor, oldPass, newPass, confirm string, opts IssueOptions) (Result, error) {
	scope, err := actor.scope()
	if err != nil {
		return Result{}, err
	}
	if err := s.validatePassphrase(actor, newPass, confirm); err != nil {
		return Result{}, err
	}
	if newPass == oldPass {
		return Result{}, newErr(CodeInvalidArgument, "the new passphrase must differ from the current one")
	}
	days, err := s.validateOptions(opts)
	if err != nil {
		return Result{}, err
	}
	if len(oldPass) == 0 || len(oldPass) > MaxPassphraseBytes {
		return Result{}, newErr(CodeInvalidArgument, "the passphrase is missing or too long")
	}
	if err := s.lock(ctx); err != nil {
		return Result{}, err
	}
	defer s.unlock()
	if res, _, err := s.begin(ctx, "rotate"); res != nil || err != nil {
		if res != nil {
			return *res, err
		}
		return Result{}, err
	}
	if s.State() != StateEstablished {
		return Result{}, newErr(CodeConflict, "rotating the master key needs an established Hub (sealed root and certificate)")
	}
	chain, err := s.st.Transitions(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(chain) >= MaxChainLinks {
		return Result{}, newErr(CodeConflict, "the transition chain is full; replace the Hub identity instead of rotating again")
	}
	row, _, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	old, err := s.gateAndUnseal(ctx, actor, scope, oldPass, row)
	if err != nil {
		return Result{}, err
	}
	defer s.track(old).Destroy()
	if err := s.point(PointAfterUnseal); err != nil {
		return Result{}, err
	}
	if s.exists(RootNextFile) {
		return Result{}, newErr(CodeConflict, s.path(RootNextFile)+" exists; resolve it first")
	}
	seed, err := identity.NewRootSeed()
	if err != nil {
		return Result{}, err
	}
	defer identity.WipeBytes(seed)
	sealed, err := s.seal.Seal(seed, newPass)
	if err != nil {
		return Result{}, err
	}
	nsg, err := identity.RootSignerFromSeed(seed)
	if err != nil {
		return Result{}, err
	}
	defer s.track(nsg).Destroy()
	identity.WipeBytes(seed)
	staged := false
	defer func() {
		if !staged && s.alive() {
			s.remove(RootNextFile)
		}
	}()
	if err := s.writeFileDurable(RootNextFile, sealed); err != nil { // D1, D2
		return Result{}, err
	}
	back, err := s.readFile(RootNextFile)
	if err != nil {
		return Result{}, err
	}
	if !s.verifyReadback(back, newPass, nsg.Public()) {
		return Result{}, newErr(CodeConflict, "the new sealed root could not be verified after writing")
	}
	res, err := s.activate(ctx, &activation{kind: "rotate", actor: actor, scope: scope, row: row, days: days, floor: opts.SerialFloor,
		signer: old, newSigner: nsg, newRootSHA: sha(back), rotate: true})
	staged = true // activate removes RN itself while nothing is committed
	return res, err
}

// ChangePassphrase seals the same root again under a new passphrase (W1 to
// W4). No certificate, key, fingerprint, generation or session changes.
func (s *Service) ChangePassphrase(ctx context.Context, actor Actor, oldPass, newPass, confirm string) (Result, error) {
	scope, err := actor.scope()
	if err != nil {
		return Result{}, err
	}
	if err := s.validatePassphrase(actor, newPass, confirm); err != nil {
		return Result{}, err
	}
	if newPass == oldPass {
		return Result{}, newErr(CodeInvalidArgument, "the new passphrase must differ from the current one")
	}
	if len(oldPass) == 0 || len(oldPass) > MaxPassphraseBytes {
		return Result{}, newErr(CodeInvalidArgument, "the passphrase is missing or too long")
	}
	if err := s.lock(ctx); err != nil {
		return Result{}, err
	}
	defer s.unlock()
	if res, _, err := s.begin(ctx, "passphrase_change"); res != nil || err != nil {
		if res != nil {
			return *res, err
		}
		return Result{}, err
	}
	if st := s.State(); st != StateSealedNoCert && st != StateEstablished {
		return Result{}, newErr(CodeConflict, "no sealed root yet; set the passphrase first")
	}
	row, _, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Result{}, err
	}
	signer, err := s.gateAndUnseal(ctx, actor, scope, oldPass, row)
	if err != nil {
		return Result{}, err
	}
	defer s.track(signer).Destroy()
	if err := s.point(PointAfterUnseal); err != nil {
		return Result{}, err
	}
	if s.exists(RootNextFile) {
		return Result{}, newErr(CodeConflict, s.path(RootNextFile)+" exists; resolve it first")
	}
	sealed, err := s.seal.Reseal(signer, newPass)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed && s.alive() { // W1
			s.remove(RootNextFile)
		}
	}()
	if err := s.writeFileDurable(RootNextFile, sealed); err != nil {
		return Result{}, err
	}
	back, err := s.readFile(RootNextFile)
	if err != nil {
		return Result{}, err
	}
	if !s.verifyReadback(back, newPass, row.Pub) {
		return Result{}, newErr(CodeConflict, "the new sealed root could not be verified after writing")
	}
	signer.Destroy()
	if err := s.point(PointWStaged); err != nil {
		return Result{}, err
	}
	newSHA := sha(back)
	now := s.nowMs()
	err = s.st.CommitPassphraseChange(ctx, row.Pub, row.RootFileSHA256, newSHA, scope, now,
		[]store.AuditRow{s.auditRow(actor, "hub_identity.passphrase_change", fmtTarget(actor.Channel, row.CurrentSerial, row.Pub, nil))})
	if err != nil {
		return Result{}, mapStateErr(err)
	}
	committed = true
	res := Result{Kind: "passphrase_change", Serial: uint64(row.CurrentSerial), RootFingerprint: row.Fingerprint, Generation: s.Generation()}
	if err := s.point(PointWCommitted); err != nil {
		return Result{}, err
	}
	finish := func(context.Context) (Result, error) { return s.finishChange(res) }
	if err := s.confirm(2); err != nil { // W2
		s.setUnconfirmed(&pendingOp{kind: "passphrase_change", finish: finish})
		return Result{}, notDurable(err, "the passphrase change")
	}
	if err := s.point(PointWDurable); err != nil {
		return Result{}, err
	}
	return finish(ctx)
}

func (s *Service) finishChange(res Result) (Result, error) {
	if err := s.rename(RootNextFile, RootFile); err != nil { // D5
		s.log.Error("hub identity: the passphrase change is durable but the sealed root file could not be renamed; further identity changes are refused until a retry succeeds",
			"from", s.path(RootNextFile), "to", s.path(RootFile), "error", err)
		return res, &Error{Code: CodeFilesPending, Msg: "the sealed root file could not be renamed", Err: err}
	}
	if err := s.syncAfterRename(); err != nil { // D6
		return res, &Error{Code: CodeFilesPending, Msg: "the directory sync is pending", Err: err}
	}
	return res, s.point(PointWRenamed)
}
