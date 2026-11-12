package hubid

import (
	"bytes"
	"context"
	"errors"
	"io/fs"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Start runs the recovery of section 9.5 without the passphrase, detects the
// bootstrap state and loads the operational key and certificate of state B3.
// It must succeed before the service is used and before any Worker listener
// opens. It fails closed (CodeFailClosed, naming the files) and generates no
// key when a recorded file is missing; a commit that cannot be proved durable
// refuses to start and keeps every file (CodeNotDurable).
func (s *Service) Start(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	// The obligation to sync the directory is memory only: a directory sync
	// that failed in a previous run is lost with the process, so a start
	// always syncs, also when every file name already matches.
	s.syncPending = true
	if err := s.reconcileIntents(ctx); err != nil {
		return err
	}
	if err := s.recoverLocked(ctx, true); err != nil {
		return err
	}
	s.started = true
	return nil
}

// recoverLocked is the recovery code shared by Start and by every identity
// change (section 9.2, step 1). The caller holds the mutex.
func (s *Service) recoverLocked(ctx context.Context, atStart bool) error {
	row, ok, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return err
	}
	if !ok {
		// A plain identity.key without a row is a 0.6 identity whose record
		// was never written (released behaviour: record it).
		if s.exists(PlainNextFile) {
			s.remove(PlainNextFile) // a rotation can only be pending next to a record
		}
		if s.exists(PlainFile) {
			pub, err := s.keyPub(PlainFile)
			if err != nil {
				return failClosed("%s is unreadable (%v); restore it from a backup", s.path(PlainFile), err)
			}
			if err := s.st.RecordPlainIdentity(ctx, store.HubIdentityRow{Pub: pub, Fingerprint: fingerprint(pub), CreatedAt: s.nowMs()}); err != nil {
				return err
			}
			return s.recoverLocked(ctx, atStart)
		}
		for _, f := range []string{OpFile} {
			if s.exists(f) {
				return failClosed("%s exists but this Hub records no identity; nothing is generated or deleted", s.path(f))
			}
		}
		// BS1: a sealed root that no row records never signed anything.
		for _, f := range []string{RootFile, RootNextFile, OpNextFile} {
			s.remove(f)
		}
		s.state.Store(int32(StatePristine))
		return s.checkSyncPending()
	}
	if !row.RootSealed {
		return s.recoverPlain(ctx, row)
	}
	return s.recoverSealed(ctx, row, atStart)
}

// recoverPlain handles B1 (the root is the plain identity.key).
func (s *Service) recoverPlain(ctx context.Context, row store.HubIdentityV2) error {
	// A 0.6 rotation that committed but did not rename identity.key.next.
	if s.exists(PlainNextFile) {
		pub, err := s.keyPub(PlainNextFile)
		switch {
		case err != nil:
			return failClosed("%s is unreadable (%v); remove it if %s is the identity this database records", s.path(PlainNextFile), err, s.path(PlainFile))
		case bytes.Equal(pub, row.Pub):
			if err := s.confirm(1); err != nil {
				return notDurableStart(err)
			}
			if err := s.rename(PlainNextFile, PlainFile); err != nil {
				return failClosed("complete the interrupted rotation: %v", err)
			}
			if err := s.syncAfterRename(); err != nil {
				return failClosed("sync the data directory: %v", err)
			}
		default:
			s.remove(PlainNextFile)
		}
	}
	pub, err := s.keyPub(PlainFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return failClosed("%s is missing but this Hub's database records the identity %s; restore it from your backup, a new key is not generated",
			s.path(PlainFile), row.Fingerprint)
	case err != nil:
		return failClosed("%s is unreadable (%v)", s.path(PlainFile), err)
	case !bytes.Equal(pub, row.Pub):
		return failClosed("%s holds another key than the identity %s this database records; restore the file that belongs to this database", s.path(PlainFile), row.Fingerprint)
	}
	// BS2: files of a sealing whose transaction did not commit.
	for _, f := range []string{RootFile, RootNextFile, OpFile, OpNextFile} {
		s.remove(f)
	}
	s.state.Store(int32(StatePlain))
	return s.checkSyncPending()
}

func notDurableStart(err error) error {
	return &Error{Code: CodeNotDurable, Msg: "the database cannot prove a recorded change is durable; every file is kept, start again once it can", Err: err}
}

// recoverSealed handles B2 and B3 (rules A, B and C of section 9.5).
func (s *Service) recoverSealed(ctx context.Context, row store.HubIdentityV2, atStart bool) error {
	// D3: nothing may rename a pending file over a recorded one, or delete
	// plain residue, before the commit that recorded this state is confirmed
	// on disk. The confirmation runs lazily, before the first such action.
	confirmed := false
	need := func() error {
		if confirmed {
			return nil
		}
		if err := s.confirm(1); err != nil {
			return notDurableStart(err)
		}
		confirmed = true
		return nil
	}
	// Rule B: the sealed root is the file whose hash is recorded.
	rk, rn := s.fileSHA(RootFile), s.fileSHA(RootNextFile)
	rkOK := rk != nil && bytes.Equal(rk, row.RootFileSHA256)
	rnOK := !rkOK && rn != nil && bytes.Equal(rn, row.RootFileSHA256)
	switch {
	case rkOK:
		if s.exists(RootNextFile) {
			s.remove(RootNextFile)
		}
	case rnOK:
		if err := need(); err != nil {
			return err
		}
		if err := s.rename(RootNextFile, RootFile); err != nil {
			return failClosed("rename %s over %s: %v", s.path(RootNextFile), s.path(RootFile), err)
		}
		if err := s.syncAfterRename(); err != nil {
			return failClosed("sync the data directory: %v", err)
		}
	case !atStart:
		// A running Hub keeps serving; the unseal step of the operation
		// checks the hash and answers with the one generic failure that
		// counts for the lockout (section 10.5). Nothing is touched here.
		s.log.Error("hub identity: the sealed root file does not match the database record", "file", s.path(RootFile))
	default:
		return failClosed("neither %s nor %s matches the sealed root this database records (SHA-256 %x); restore the file from a backup, nothing is generated and no plain identity file is removed",
			s.path(RootFile), s.path(RootNextFile), row.RootFileSHA256)
	}
	// Rule C: plain residue goes only when a sealed candidate is proved to
	// match the recorded hash (above) and the commit that recorded it is
	// confirmed on disk. Without a matching candidate the plain file may be the
	// only copy of the root and stays.
	for _, f := range []string{PlainFile, PlainNextFile} {
		if !s.exists(f) {
			continue
		}
		if !rkOK && !rnOK {
			s.log.Error("hub identity: the plain identity file is kept because no sealed root file matches the database record", "file", s.path(f))
			continue
		}
		if err := need(); err != nil {
			return err
		}
		if err := s.shred(f); err != nil {
			return failClosed("remove the plain residue %s: %v", s.path(f), err)
		}
		s.log.Info("hub identity: removed the plain identity file of a sealed root", "file", s.path(f))
	}
	if err := s.point("recover.root-done"); err != nil {
		return err
	}
	// Rule A and the bootstrap states.
	if len(row.CurrentCert) == 0 {
		// B2: no certificate was ever served, so no op key can be protected.
		s.remove(OpFile)
		s.remove(OpNextFile)
		s.state.Store(int32(StateSealedNoCert))
		if s.snap.Load() != nil && s.alive() {
			return newErr(CodeConflict, "the in-memory certificate has no database record; restart the Hub")
		}
		return s.checkSyncPending()
	}
	cert, err := identity.ParseHubCert(row.CurrentCert)
	if err == nil {
		err = cert.Verify(row.Pub)
	}
	if err != nil {
		return failClosed("the certificate recorded in the database does not verify under the recorded root: %v", err)
	}
	kOK := s.keyMatches(OpFile, cert.OpPub)
	nExists := s.exists(OpNextFile)
	nOK := nExists && s.keyMatches(OpNextFile, cert.OpPub)
	switch {
	case nOK && !kOK:
		if err := need(); err != nil {
			return err
		}
		if err := s.rename(OpNextFile, OpFile); err != nil {
			return failClosed("rename %s over %s: %v", s.path(OpNextFile), s.path(OpFile), err)
		}
		if err := s.syncAfterRename(); err != nil {
			return failClosed("sync the data directory: %v", err)
		}
		kOK = true
	case nOK && kOK:
		s.remove(OpNextFile)
	case nExists:
		s.remove(OpNextFile) // matches nothing: it never signed or served anything
	}
	if !kOK {
		return failClosed("%s is missing or does not hold the operational key of the recorded certificate (public key %x) and %s cannot complete it; "+
			"restore it from a backup, nothing is generated", s.path(OpFile), []byte(cert.OpPub), s.path(OpNextFile))
	}
	s.state.Store(int32(StateEstablished))
	if s.snap.Load() == nil {
		k, err := identity.LoadKey(s.path(OpFile))
		if err != nil {
			return failClosed("load %s: %v", s.path(OpFile), err)
		}
		s.snap.Store(&Snapshot{RootPub: bytes.Clone(row.Pub), OpKey: k, Cert: cert, Generation: s.gen.Load()})
	} else if cur := s.snap.Load(); cur.Cert.SHA256() != cert.SHA256() {
		return newErr(CodeConflict, "the database records another certificate than the one in use; restart the Hub")
	}
	return s.checkSyncPending()
}

// checkSyncPending retries a directory sync that failed after a rename (D6).
func (s *Service) checkSyncPending() error {
	if !s.syncPending {
		return nil
	}
	if err := s.syncDir(); err != nil {
		return &Error{Code: CodeConflict, Msg: "a directory sync after a rename is still pending", Err: err}
	}
	s.syncPending = false
	return nil
}

func (s *Service) keyMatches(name string, pub []byte) bool {
	got, err := s.keyPub(name)
	return err == nil && bytes.Equal(got, pub)
}
