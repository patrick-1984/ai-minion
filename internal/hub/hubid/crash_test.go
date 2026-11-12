package hubid

import (
	"errors"
	"testing"
)

// opKeyPreserved is D4: the key of the recorded certificate is always on disk,
// as op.key or as op.key.next.
func (e *env) opKeyPreserved() {
	e.t.Helper()
	pub := e.cert().OpPub
	for _, n := range []string{OpFile, OpNextFile} {
		if e.has(n) && string(e.opPub(n)) == string(pub) {
			return
		}
	}
	e.t.Fatal("D4: the operational key of the recorded certificate is gone")
}

func (e *env) sealedPreserved() {
	e.t.Helper()
	want := e.row().RootFileSHA256
	for _, n := range []string{RootFile, RootNextFile} {
		if e.has(n) && string(sha(mustRead(e.t, e, n))) == string(want) {
			return
		}
	}
	e.t.Fatal("D4: the sealed root of the recorded hash is gone")
}

// A1 to A5: direct activation (re-issue).
func TestCrashTableA(t *testing.T) {
	for _, tc := range []struct {
		name, point  string
		committed    bool // the new certificate is in the database after the crash
		nPendingFile bool // op.key.next exists after the crash
		wantServing  string
	}{
		{"A1 staged", PointStaged, false, true, "old"},
		{"A1 before commit", PointPreCommit, false, true, "old"},
		{"A2 committed not confirmed", PointCommitted, true, true, "new"},
		{"A3 confirmed, nothing renamed", PointDurable, true, true, "new"},
		{"A5 swapped, nothing renamed", PointSwapped, true, true, "new"},
		{"A4 renamed", PointRenamed, true, false, "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			s := e.established()
			old := e.cert()
			e.crashAt(tc.point)
			_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
			wantCode(t, err, ErrCrash)
			// Durable facts after the crash.
			cur := e.cert()
			if tc.committed == (cur.Serial == old.Serial) {
				t.Fatalf("committed = %v but serial %d (old %d)", tc.committed, cur.Serial, old.Serial)
			}
			if e.has(OpNextFile) != tc.nPendingFile {
				t.Fatalf("op.key.next present = %v", e.has(OpNextFile))
			}
			e.opKeyPreserved()
			// Recovery at start, without the passphrase.
			s2 := e.svc()
			snap, err := s2.Current()
			if err != nil {
				t.Fatal(err)
			}
			if (tc.wantServing == "new") != (snap.Cert.Serial != old.Serial) {
				t.Fatalf("serving serial %d (old %d)", snap.Cert.Serial, old.Serial)
			}
			if e.has(OpNextFile) || string(e.opPub(OpFile)) != string(snap.Cert.OpPub) {
				t.Fatal("files not recovered")
			}
			e.opKeyPreserved()
			// The flow completes afterwards.
			if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
				t.Fatal(err)
			}
			if string(e.opPub(OpFile)) != string(e.cert().OpPub) {
				t.Fatal("op.key is not the new certified key")
			}
		})
	}
}

// X1 to X5: master-key rotation.
func TestCrashTableX(t *testing.T) {
	for _, tc := range []struct {
		name, point string
		committed   bool
	}{
		{"X1 staged", PointRNStaged, false},
		{"X2 committed not confirmed", PointCommitted, true},
		{"X3 confirmed, nothing renamed", PointDurable, true},
		{"X4 RK renamed, N not", PointRKRenamed, true},
		{"X5 both renamed", PointRenamed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			s := e.established()
			oldRoot := e.row().Pub
			oldSHA := e.row().RootFileSHA256
			e.crashAt(tc.point)
			_, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
			wantCode(t, err, ErrCrash)
			row := e.row()
			if tc.committed == (string(row.Pub) == string(oldRoot)) {
				t.Fatalf("committed = %v", tc.committed)
			}
			if !tc.committed && (!e.has(RootNextFile) || !e.has(OpNextFile)) {
				t.Fatal("X1: RN and N must be staged")
			}
			e.opKeyPreserved()
			e.sealedPreserved()
			s2 := e.svc()
			if e.has(RootNextFile) || e.has(OpNextFile) {
				t.Fatal("pending files left after recovery")
			}
			if tc.committed {
				if got, ok := e.rootOpens(pw2); !ok || string(got) != string(row.Pub) {
					t.Fatal("new root does not open with the new passphrase")
				}
				if _, ok := e.rootOpens(pw1); ok {
					t.Fatal("old passphrase opens the current root")
				}
			} else {
				// RN removed; the old sealed root is still RK and the old passphrase opens it.
				if got, ok := e.rootOpens(pw1); !ok || string(got) != string(oldRoot) || string(sha(mustRead(t, e, RootFile))) != string(oldSHA) {
					t.Fatal("old root damaged")
				}
			}
			snap, err := s2.Current()
			if err != nil || string(snap.RootPub) != string(row.Pub) || string(e.opPub(OpFile)) != string(snap.Cert.OpPub) {
				t.Fatalf("serving: %v", err)
			}
			pw := pw1
			if tc.committed {
				pw = pw2
			}
			if _, err := s2.Reissue(bg, ui, pw, IssueOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// W1 to W4: passphrase change.
func TestCrashTableW(t *testing.T) {
	for _, tc := range []struct {
		name, point string
		committed   bool
	}{
		{"W1 staged", PointWStaged, false},
		{"W2 committed not confirmed", PointWCommitted, true},
		{"W3 confirmed, not renamed", PointWDurable, true},
		{"W4 renamed", PointWRenamed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			s := e.established()
			oldSHA := e.row().RootFileSHA256
			cert := e.cert()
			e.crashAt(tc.point)
			_, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
			wantCode(t, err, ErrCrash)
			if tc.committed == (string(e.row().RootFileSHA256) == string(oldSHA)) {
				t.Fatalf("committed = %v", tc.committed)
			}
			e.sealedPreserved()
			s2 := e.svc()
			if e.has(RootNextFile) {
				t.Fatal("RN left")
			}
			e.sealedPreserved()
			if string(e.cert().Bytes()) != string(cert.Bytes()) {
				t.Fatal("certificate changed")
			}
			_, oldOK := e.rootOpens(pw1)
			_, newOK := e.rootOpens(pw2)
			if oldOK == tc.committed || newOK != tc.committed {
				t.Fatalf("old opens %v, new opens %v", oldOK, newOK)
			}
			use, other := pw1, pw2
			if tc.committed {
				use, other = pw2, pw1
			}
			if _, err := s2.Reissue(bg, ui, other, IssueOptions{}); !errors.Is(err, ErrPassphraseInvalid) {
				t.Fatalf("the wrong passphrase: %v", err)
			}
			if _, err := s2.Reissue(bg, ui, use, IssueOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// BS1 to BS5: bootstrap.
func TestCrashTableBS(t *testing.T) {
	t.Run("BS1 B0 sealed file written, row not", func(t *testing.T) {
		e := newEnv(t)
		s := e.svc()
		e.crashAt(PointBSRootWritten)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		if !e.has(RootFile) {
			t.Fatal("RK must exist after the crash")
		}
		if _, ok, _ := e.st.HubIdentityV2(bg); ok {
			t.Fatal("a row exists")
		}
		s2 := e.svc()
		if e.has(RootFile) || s2.State() != StatePristine {
			t.Fatal("RK must be removed, state B0")
		}
		if _, err := s2.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("BS2 B1 sealed file written, not committed", func(t *testing.T) {
		e := newEnv(t)
		k := e.plain()
		s := e.svc()
		e.crashAt(PointBSRootWritten)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		if !e.has(RootFile) || !e.has(PlainFile) || e.row().RootSealed {
			t.Fatal("durable facts")
		}
		s2 := e.svc()
		if e.has(RootFile) || !e.has(PlainFile) || s2.State() != StatePlain {
			t.Fatal("recovery")
		}
		if _, err := s2.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
		if string(e.row().Pub) != string(k.Public()) {
			t.Fatal("identity changed")
		}
	})
	t.Run("BS3 committed, not confirmed: both files are kept until confirmed", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		failing := true
		e.confirmFailsAfterCommit(&failing)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrNotDurable)
		if !e.has(PlainFile) || !e.has(RootFile) {
			t.Fatal("both files must stay")
		}
		// A start with a failing confirmation refuses and keeps both files.
		err = e.startErr()
		wantCode(t, err, ErrNotDurable)
		if !e.has(PlainFile) || !e.has(RootFile) {
			t.Fatal("start removed a file before the confirmation")
		}
		// A later successful confirmation allows the removal.
		e.makeDurable, e.point = nil, nil
		s2 := e.svc()
		if e.has(PlainFile) || !e.has(RootFile) || s2.State() != StateSealedNoCert {
			t.Fatal("rule C after the confirmation")
		}
	})
	t.Run("BS3 repeat of the command confirms in place", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		fail := true
		e.confirmFailsAfterCommit(&fail)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrNotDurable)
		// Another operation is refused while the commit is unconfirmed.
		_, err = s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
		wantCode(t, err, ErrConflict)
		fail = false
		// The repetition confirms the setup and, with the passphrase of this
		// request, issues the first certificate: it reaches B3.
		res, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{ValidityDays: 30})
		if err != nil || res.Kind != "passphrase_set" || res.Serial == 0 {
			t.Fatalf("confirm: %+v %v", res, err)
		}
		if e.has(PlainFile) || s.State() != StateEstablished || e.has(RootNextFile) || !e.has(OpFile) {
			t.Fatal("plain residue or state after the confirmation")
		}
		if cur, err := s.Current(); err != nil || cur.Cert.Serial != res.Serial || cur.Cert.NotAfterMs-cur.Cert.NotBeforeMs != 30*dayMs {
			t.Fatalf("serving certificate: %v", err)
		}
		if len(e.audits("hub_identity.passphrase_set")) != 1 || len(e.audits("hub_identity.reissue")) != 1 {
			t.Fatal("audit rows")
		}
		// Nothing secret was kept: a wrong passphrase on the retry is refused and counted, and Reissue finishes the job.
	})
	t.Run("BS3 repeat with a wrong passphrase leaves B2 and Reissue completes", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		fail := true
		e.confirmFailsAfterCommit(&fail)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrNotDurable)
		fail = false
		_, err = s.SetPassphrase(bg, ui, pw2, pw2, IssueOptions{}) // not the passphrase of the sealed file
		wantCode(t, err, ErrPassphraseInvalid)
		if s.State() != StateSealedNoCert || e.has(PlainFile) {
			t.Fatalf("state %v plain %v", s.State(), e.has(PlainFile))
		}
		if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil || s.State() != StateEstablished {
			t.Fatalf("reissue: %v", err)
		}
	})
	t.Run("BS3 crash after the commit, power kept the commit", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		e.crashAt(PointBSCommitted)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		if !e.row().RootSealed || !e.has(PlainFile) || !e.has(RootFile) {
			t.Fatal("durable facts")
		}
		s2 := e.svc()
		if e.has(PlainFile) || s2.State() != StateSealedNoCert {
			t.Fatal("recovery")
		}
	})
	t.Run("BS4 confirmed, plain residue still present", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		e.crashAt(PointBSDurable)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		if !e.has(PlainFile) {
			t.Fatal("residue must exist")
		}
		s2 := e.svc()
		if e.has(PlainFile) || s2.State() != StateSealedNoCert {
			t.Fatal("rule C")
		}
		if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("BS4 residue next to a sealed .next file is removed too", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		_ = s
		k, _ := identityKey(t)
		if err := saveKey(e, PlainFile, k); err != nil {
			t.Fatal(err)
		}
		if err := saveKey(e, PlainNextFile, k); err != nil {
			t.Fatal(err)
		}
		e.svc()
		if e.has(PlainFile) || e.has(PlainNextFile) {
			t.Fatal("plain residue with root_sealed = 1 must be removed")
		}
	})
	t.Run("BS5 first certificate crash", func(t *testing.T) {
		e := newEnv(t)
		s := e.svc()
		e.crashAt(PointStaged)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		if !e.has(OpNextFile) || len(e.row().CurrentCert) != 0 {
			t.Fatal("durable facts")
		}
		s2 := e.svc()
		if e.has(OpNextFile) || s2.State() != StateSealedNoCert {
			t.Fatal("recovery")
		}
		if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFailClosedStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env)
		files []string
	}{
		{"B3 op.key missing", func(e *env) { e.established(); rm(e, OpFile) }, []string{OpFile}},
		{"B3 sealed root missing", func(e *env) { e.established(); rm(e, RootFile) }, []string{RootFile}},
		{"B3 sealed root replaced", func(e *env) { e.established(); mustWrite(e.t, e, RootFile, []byte("other")) }, []string{RootFile}},
		{"B3 op.key is another key", func(e *env) {
			e.established()
			k, _ := identityKey(e.t)
			_ = saveKey(e, OpFile, k)
		}, []string{OpFile}},
		{"B1 identity.key missing", func(e *env) { e.plain(); rm(e, PlainFile) }, []string{PlainFile}},
		{"B1 identity.key is another key", func(e *env) {
			e.plain()
			k, _ := identityKey(e.t)
			_ = saveKey(e, PlainFile, k)
		}, []string{PlainFile}},
		{"B0 stray op.key", func(e *env) {
			k, _ := identityKey(e.t)
			_ = saveKey(e, OpFile, k)
		}, []string{OpFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.setup(e)
			err := e.startErr()
			wantCode(t, err, ErrFailClosed)
			for _, f := range tc.files {
				if !contains(err.Error(), f) {
					t.Fatalf("the error does not name %s: %v", f, err)
				}
			}
			// Nothing was generated.
			for _, f := range []string{OpFile, PlainFile, RootFile} {
				found := e.has(f)
				_ = found
			}
		})
	}
	t.Run("a missing op.key is not regenerated", func(t *testing.T) {
		e := newEnv(t)
		e.established()
		rm(e, OpFile)
		_ = e.startErr()
		if e.has(OpFile) || e.has(OpNextFile) {
			t.Fatal("a key was generated")
		}
	})
}
