package hubid

import (
	"context"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// activation is one run of the procedure of section 9.2 from step 3 on. The
// caller has unsealed the root(s) and holds the rotation mutex.
type activation struct {
	kind  string // Result.Kind and pending kind
	actor Actor
	scope string
	row   store.HubIdentityV2
	days  int
	floor int64
	// signer holds the root that is current before the operation: it signs
	// the certificate (first issuance, re-issue) or the transition (rotation).
	signer *identity.RootSigner
	// newSigner and newRootSHA are set in a rotation: the new root, whose
	// sealed file is already staged at root.key.age.next.
	newSigner  *identity.RootSigner
	newRootSHA []byte
	rotate     bool
}

// activated is what the swap needs; it survives an unconfirmed commit.
type activated struct {
	kind    string
	rootPub []byte
	opKey   *identity.Key
	cert    *identity.HubCert
	rotate  bool
}

// activate stages a fresh operational key, signs, commits in one transaction
// with the audit rows, proves the commit durable, swaps the in-memory set,
// renames the staged files and closes the sessions (steps 3 to 8).
func (s *Service) activate(ctx context.Context, a *activation) (Result, error) {
	if s.exists(OpNextFile) {
		return Result{}, newErr(CodeConflict, s.path(OpNextFile)+" exists; resolve it first")
	}
	committed := false
	defer func() {
		// Before the commit nothing of this run ever signed or served anything.
		if !committed && s.alive() {
			s.remove(OpNextFile)
			if a.rotate {
				s.remove(RootNextFile)
			}
		}
	}()
	opKey, err := identity.GenerateKey()
	if err != nil {
		return Result{}, err
	}
	// Step 3: stage N (D1, D2).
	if err := identity.SaveKey(s.path(OpNextFile), opKey); err != nil {
		return Result{}, err
	}
	if err := s.syncDir(); err != nil { // D6, before the commit: nothing changes
		return Result{}, err
	}
	stagePoint := PointStaged
	if a.rotate {
		stagePoint = PointRNStaged
	}
	if err := s.point(stagePoint); err != nil {
		return Result{}, err
	}

	// Step 4: allocate the serial and sign. Every signer is destroyed right
	// after the last signature, and again by the callers' defers.
	nowMs := s.nowMs()
	serial, err := allocateSerial(a.row.LastIssued, nowMs, a.floor)
	if err != nil {
		return Result{}, err
	}
	notAfter := nowMs + int64(a.days)*dayMs
	certSigner := a.signer
	var rot *store.RotationParams
	if a.rotate {
		certSigner = a.newSigner
		chain, err := s.st.Transitions(ctx)
		if err != nil {
			return Result{}, err
		}
		if len(chain) >= MaxChainLinks {
			return Result{}, newErr(CodeConflict, "the transition chain is full; replace the Hub identity instead of rotating again")
		}
		seq := uint64(1)
		if n := len(chain); n > 0 {
			seq = chain[n-1].Seq + 1
		}
		at := time.UnixMilli(nowMs)
		tr, err := identity.NewTransitionBy(a.signer, a.newSigner.Public(), seq, at)
		if err != nil {
			return Result{}, err
		}
		if err := tr.Verify(); err != nil {
			return Result{}, err
		}
		newPub := a.newSigner.Public()
		rot = &store.RotationParams{
			Transition:    store.TransitionRow{Seq: tr.Seq, OldPub: []byte(tr.OldPub), NewPub: []byte(tr.NewPub), CreatedAt: tr.CreatedAt.UnixMilli(), SigByOld: []byte(tr.SigByOld)},
			Identity:      store.HubIdentityRow{Pub: newPub, Fingerprint: fingerprint(newPub), CreatedAt: tr.CreatedAt.UnixMilli()},
			NewRootSHA256: a.newRootSHA, MaxLinks: MaxChainLinks,
		}
	}
	cert, err := certSigner.IssueHubCert(opKey.Public(), serial, nowMs, notAfter)
	if err != nil {
		return Result{}, err
	}
	rootPub := certSigner.Public()
	a.signer.Destroy()
	if a.newSigner != nil {
		a.newSigner.Destroy()
	}
	if err := s.point(PointSigned); err != nil {
		return Result{}, err
	}

	// Step 5: the transaction, with the audit row.
	action := "hub_identity.reissue"
	if a.rotate {
		action = "hub_identity.rotate"
	}
	if err := s.point(PointPreCommit); err != nil {
		return Result{}, err
	}
	op := "reissue"
	if a.rotate {
		op = "rotate"
	}
	err = s.st.CommitIssuance(ctx, store.IssuanceParams{
		ExpectPub: a.row.Pub, ExpectRootSH: a.row.RootFileSHA256, Serial: int64(serial), Cert: cert.Bytes(), NowMS: nowMs,
		Scope: a.scope, Op: op, Rotation: rot,
		Audit: []store.AuditRow{s.auditRow(a.actor, action, fmtTarget(a.actor.Channel, int64(serial), rootPub, opKey.Public()))},
	})
	switch {
	case errors.Is(err, store.ErrIdentityState), errors.Is(err, store.ErrChainFull), errors.Is(err, store.ErrRotationConflict):
		return Result{}, &Error{Code: CodeConflict, Msg: "the Hub identity changed meanwhile", Err: err}
	case err != nil:
		return Result{}, err
	}
	committed = true
	pa := &activated{kind: a.kind, rootPub: rootPub, opKey: opKey, cert: cert, rotate: a.rotate}
	if err := s.point(PointCommitted); err != nil {
		return Result{}, err
	}

	// D3: nothing may use the new state before the commit is confirmed.
	if err := s.confirm(2); err != nil {
		s.unconfirmed = &pendingOp{kind: a.kind, activation: true, finish: func(ctx context.Context) (Result, error) { return s.finishActivation(ctx, pa) }}
		s.log.Error("hub identity change committed but is not confirmed durable; keeping the previous state", "serial", serial, "error", err)
		return Result{}, notDurable(err, "the change")
	}
	if err := s.point(PointDurable); err != nil {
		return Result{}, err
	}
	return s.finishActivation(ctx, pa)
}

// finishActivation is steps 6 to 8: swap, rename, close sessions.
func (s *Service) finishActivation(_ context.Context, pa *activated) (res Result, err error) {
	gen := s.gen.Add(1)
	s.snap.Store(&Snapshot{RootPub: pa.rootPub, OpKey: pa.opKey, Cert: pa.cert, Generation: gen})
	s.state.Store(int32(StateEstablished))
	defer func() { // D7: guaranteed after every swap, also on error and panic
		if s.alive() {
			s.closeSessions()
			s.notifyActivated()
		}
	}()
	res = Result{
		Kind: pa.kind, Serial: pa.cert.Serial, NotBefore: time.UnixMilli(pa.cert.NotBeforeMs).UTC(), NotAfter: time.UnixMilli(pa.cert.NotAfterMs).UTC(),
		RootFingerprint: fingerprint(pa.rootPub), OpFingerprint: fingerprint(pa.opKey.Public()), Generation: gen,
	}
	if err := s.point(PointSwapped); err != nil {
		return res, err
	}
	if pa.rotate {
		if rerr := s.rename(RootNextFile, RootFile); rerr != nil { // D5
			s.log.Error("hub identity: the change is active but the sealed root file could not be renamed; further identity changes are refused until a retry succeeds",
				"from", s.path(RootNextFile), "to", s.path(RootFile), "error", rerr)
			return res, &Error{Code: CodeFilesPending, Msg: "the sealed root file could not be renamed", Err: rerr}
		}
		if serr := s.syncAfterRename(); serr != nil { // D6 after the root rename too
			s.log.Error("hub identity: directory sync after the sealed root rename failed; it is retried at the next identity request and at start", "error", serr)
			return res, &Error{Code: CodeFilesPending, Msg: "the directory sync is pending", Err: serr}
		}
		if err := s.point(PointRKRenamed); err != nil {
			return res, err
		}
	}
	if rerr := s.rename(OpNextFile, OpFile); rerr != nil { // D5
		s.log.Error("hub identity: the change is active but the operational key file could not be renamed; further identity changes are refused until a retry succeeds",
			"from", s.path(OpNextFile), "to", s.path(OpFile), "error", rerr)
		return res, &Error{Code: CodeFilesPending, Msg: "the operational key file could not be renamed", Err: rerr}
	}
	if serr := s.syncAfterRename(); serr != nil { // D6 after a rename
		s.log.Error("hub identity: directory sync after a rename failed; it is retried at the next identity request and at start", "error", serr)
		return res, &Error{Code: CodeFilesPending, Msg: "the directory sync is pending", Err: serr}
	}
	if err := s.point(PointRenamed); err != nil {
		return res, err
	}
	return res, nil
}
