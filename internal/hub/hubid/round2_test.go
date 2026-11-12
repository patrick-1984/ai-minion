package hubid

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"filippo.io/age"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// --- Finding 1: plain residue is never removed without a matching sealed file ---

func plainBytes(t *testing.T, e *env) []byte {
	t.Helper()
	return mustRead(t, e, PlainFile)
}

// bs4 returns an env in state BS4: committed and confirmed sealing, the plain
// identity.key still present, root.key.age written.
func bs4(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.plain()
	s := e.svc()
	e.crashAt(PointBSDurable)
	_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	wantCode(t, err, ErrCrash)
	if !e.has(PlainFile) || !e.has(RootFile) || !e.row().RootSealed {
		t.Fatal("not BS4")
	}
	return e
}

func TestBS4KeepsThePlainRootUntilASealedFileMatches(t *testing.T) {
	t.Run("RK missing, no RN", func(t *testing.T) {
		e := bs4(t)
		want := plainBytes(t, e)
		rm(e, RootFile)
		wantCode(t, e.startErr(), ErrFailClosed)
		if !e.has(PlainFile) || !bytes.Equal(plainBytes(t, e), want) {
			t.Fatal("the plain root was destroyed")
		}
	})
	t.Run("RK damaged, no RN", func(t *testing.T) {
		e := bs4(t)
		want := plainBytes(t, e)
		mustWrite(t, e, RootFile, []byte("damaged"))
		err := e.startErr()
		wantCode(t, err, ErrFailClosed)
		if !contains(err.Error(), "no plain identity file is removed") {
			t.Fatalf("message: %v", err)
		}
		if !e.has(PlainFile) || !bytes.Equal(plainBytes(t, e), want) {
			t.Fatal("the plain root was destroyed")
		}
	})
	t.Run("a running Hub keeps it too", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		k, _ := identityKey(t)
		if err := saveKey(e, PlainFile, k); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, e, RootFile, []byte("damaged"))
		_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
		wantCode(t, err, ErrPassphraseInvalid)
		if !e.has(PlainFile) {
			t.Fatal("the plain root was destroyed by a running Hub")
		}
	})
	t.Run("RK damaged, RN holds the recorded file: it is renamed and the residue goes", func(t *testing.T) {
		e := bs4(t)
		good := mustRead(t, e, RootFile)
		mustWrite(t, e, RootNextFile, good)
		mustWrite(t, e, RootFile, []byte("damaged"))
		s := e.svc()
		if e.has(PlainFile) || e.has(RootNextFile) || !bytes.Equal(mustRead(t, e, RootFile), good) || s.State() != StateSealedNoCert {
			t.Fatal("the sealed .next variant was not recovered")
		}
	})
	t.Run("residue next to a pending passphrase change (W3 with RN)", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		e.crashAt(PointWDurable)
		_, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
		wantCode(t, err, ErrCrash)
		k, _ := identityKey(t) // plain residue next to the pending sealed .next
		_ = saveKey(e, PlainFile, k)
		_ = saveKey(e, PlainNextFile, k)
		if !e.has(RootNextFile) || !e.has(PlainFile) {
			t.Fatal("RN and the residue must exist before the start")
		}
		s2 := e.svc()
		if e.has(RootNextFile) || e.has(PlainFile) || e.has(PlainNextFile) || s2.State() != StateEstablished {
			t.Fatal("recovery")
		}
		if _, ok := e.rootOpens(pw2); !ok {
			t.Fatal("RN was not renamed over RK")
		}
	})
	t.Run("a failing confirmation keeps everything", func(t *testing.T) {
		e := bs4(t)
		e.makeDurable = func(context.Context) error { return errors.New("checkpoint failed") }
		wantCode(t, e.startErr(), ErrNotDurable)
		if !e.has(PlainFile) || !e.has(RootFile) {
			t.Fatal("a file was touched")
		}
	})
}

// --- Finding 2: lockout and allowance changes are confirmed durable ---

type flakyStore struct {
	Store
	mu         sync.Mutex
	failRecord bool
}

func (f *flakyStore) RecordUnsealFailure(ctx context.Context, scope string, nowMS int64, actor, channel string) (store.ScopeState, error) {
	f.mu.Lock()
	fail := f.failRecord
	f.mu.Unlock()
	if fail {
		return store.ScopeState{}, errors.New("database is busy")
	}
	return f.Store.RecordUnsealFailure(ctx, scope, nowMS, actor, channel)
}

func TestACountedAttemptThatCannotBeConfirmedIsConsumedAndNothingIsDerived(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	opened := e.fk.opened
	failing := true
	e.makeDurable = func(context.Context) error {
		if failing {
			return errors.New("checkpoint failed")
		}
		return nil
	}
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{}) // the correct passphrase
	re := rateLimited(t, err)
	if re.Allowance || re.RetryAt.IsZero() {
		t.Fatalf("answer %+v", re)
	}
	if e.fk.opened != opened {
		t.Fatal("a derivation ran although the attempt was not confirmed")
	}
	if st := e.scopeStatus(s, ui); st.AttemptsThisHour != 1 {
		t.Fatalf("the attempt must stay consumed: %+v", st)
	}
	// The scope stays locked while the commit cannot be proved; nothing more is counted.
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	_ = rateLimited(t, err)
	if st := e.scopeStatus(s, ui); st.AttemptsThisHour != 1 || e.fk.opened != opened {
		t.Fatalf("a locked scope counted or derived: %+v", st)
	}
	// The other scope is independent.
	if _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); err == nil {
		t.Fatal("the cli request must fail while durability fails (its own gate)")
	}
	failing = false
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatalf("after the confirmation works: %v", err)
	}
}

func TestAFailureThatCannotBeConfirmedLocksTheScopeUntilItIs(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	calls, failFrom := 0, 2 // call 1 is the gate; the confirmations of the failure record fail
	failing := true
	e.makeDurable = func(context.Context) error {
		calls++
		if failing && calls >= failFrom {
			return errors.New("checkpoint failed")
		}
		return nil
	}
	_, err := s.Reissue(bg, ui, pw2, IssueOptions{}) // wrong passphrase
	wantCode(t, err, ErrPassphraseInvalid)
	opened := e.fk.opened
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{}) // correct, but the failure is unproven
	_ = rateLimited(t, err)
	if e.fk.opened != opened {
		t.Fatal("derived while the failure was unproven")
	}
	failing = false
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatalf("after it was proved: %v", err)
	}
	if n := len(e.audits(store.ActionUnsealFailed)); n != 1 {
		t.Fatalf("audit rows of the failure: %d", n)
	}
}

func TestAFailureThatCannotBeRecordedIsRecordedLaterAndLocksMeanwhile(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.st}
	e.opts = func(o *Options) { o.Store = fs }
	s := e.established()
	fs.failRecord = true
	_, err := s.Reissue(bg, ui, pw2, IssueOptions{})
	wantCode(t, err, ErrPassphraseInvalid)
	if st := e.scopeStatus(s, ui); st.Failures != 0 {
		t.Fatalf("the failure was recorded although the store failed: %+v", st)
	}
	opened := e.fk.opened
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	_ = rateLimited(t, err)
	if e.fk.opened != opened {
		t.Fatal("derived while a failure was owed")
	}
	fs.failRecord = false
	// The owed failure is recorded first (with its audit row), then the correct passphrase works and resets.
	e.clock = e.clock.Add(time.Second)
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audits(store.ActionUnsealFailed)); n != 1 {
		t.Fatalf("the owed failure was not recorded: %d rows", n)
	}
}

// --- Finding 3: the plain root never lives in an identity.Key at start ---

func TestB1RecoveryDerivesThePublicKeyThroughDestroyedSigners(t *testing.T) {
	e := newEnv(t)
	k := e.plain()
	e.svc()
	if len(e.signers) == 0 {
		t.Fatal("the public key was not derived through a signer")
	}
	e.allSignersDestroyed()
	if string(e.row().Pub) != string(k.Public()) {
		t.Fatal("identity changed")
	}
	// A pending 0.6 rotation file is checked the same way.
	k2, _ := identity.GenerateKey()
	_ = saveKey(e, PlainNextFile, k2)
	n := len(e.signers)
	e.svc()
	if len(e.signers) <= n {
		t.Fatal("identity.key.next was not read through a signer")
	}
	e.allSignersDestroyed()
}

// --- Finding 4: readback signers die right after their comparison ---

func TestReadbackSignersAreDestroyedAtOnceAndEverySignerBeforeTheSigningBoundary(t *testing.T) {
	checkAt := func(e *env, names ...string) (seen *int) {
		seen = new(int)
		e.point = func(n string) error {
			for _, w := range names {
				if n == w {
					*seen++
					e.allSignersDestroyed()
				}
			}
			return nil
		}
		return seen
	}
	lastDestroyed := func(e *env, at string) func(string) error {
		return func(n string) error {
			if n != at {
				return nil
			}
			e.fk.mu.Lock()
			last := e.fk.last
			e.fk.mu.Unlock()
			if last == nil {
				e.t.Fatal("no readback signer")
			}
			if _, err := last.Sign(identity.PurposeHubCert, []byte("x")); !errors.Is(err, identity.ErrSignerDestroyed) {
				e.t.Fatalf("the readback signer is still usable at %s", at)
			}
			return nil
		}
	}
	t.Run("set passphrase B0", func(t *testing.T) {
		e := newEnv(t)
		s := e.svc()
		e.point = lastDestroyed(e, PointStaged)
		seen := 0
		prev := e.point
		e.point = func(n string) error {
			if n == PointSigned {
				seen++
				e.allSignersDestroyed()
			}
			return prev(n)
		}
		if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil || seen != 1 {
			t.Fatalf("%v seen %d", err, seen)
		}
	})
	t.Run("set passphrase B1", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		seen := checkAt(e, PointSigned)
		if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil || *seen != 1 {
			t.Fatalf("%v seen %d", err, *seen)
		}
	})
	t.Run("reissue", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		seen := checkAt(e, PointSigned)
		if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil || *seen != 1 {
			t.Fatalf("%v seen %d", err, *seen)
		}
	})
	t.Run("rotate: the readback signer is gone while the old root is still needed", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		e.point = lastDestroyed(e, PointRNStaged)
		seen := 0
		prev := e.point
		e.point = func(n string) error {
			if n == PointSigned {
				seen++
				e.allSignersDestroyed()
			}
			return prev(n)
		}
		if _, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{}); err != nil || seen != 1 {
			t.Fatalf("%v seen %d", err, seen)
		}
	})
	t.Run("change passphrase", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		seen := checkAt(e, PointWStaged)
		if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil || *seen != 1 {
			t.Fatalf("%v seen %d", err, *seen)
		}
	})
}

// --- Findings 5 and 6: exactly one derivation per attempt (real age files) ---

// countingSealer is the production sealer that also counts the dummy
// derivations (a dummy is the second derivation a bad file must not cause).
type countingSealer struct {
	AgeSealer
	mu    sync.Mutex
	dummy int
}

func (c *countingSealer) Dummy(pass string) {
	c.mu.Lock()
	c.dummy++
	c.mu.Unlock()
	// The real dummy costs a second; counting is enough for the test.
}

func ageFile(t *testing.T, plaintext []byte, wf int, pass string) []byte {
	t.Helper()
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(wf)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(plaintext)
	_ = w.Close()
	return buf.Bytes()
}

func TestInvalidSealedFilesCostExactlyOneDerivation(t *testing.T) {
	if testing.Short() {
		t.Skip("real scrypt derivations")
	}
	seed := bytes.Repeat([]byte{7}, 32)
	good := ageFile(t, seed, identity.SealedRootWorkFactor, pw1)
	damaged := bytes.Clone(good)
	damaged[len(damaged)-1] ^= 1
	for _, tc := range []struct {
		name      string
		file      []byte
		wantDummy int
	}{
		{"payload damaged after a valid header (derivation ran)", damaged, 0},
		{"plaintext of 33 bytes (derivation ran)", ageFile(t, append(bytes.Clone(seed), 1), identity.SealedRootWorkFactor, pw1), 0},
		{"wrong passphrase on a valid file (derivation ran)", ageFile(t, seed, identity.SealedRootWorkFactor, pw3), 0},
		{"work factor 17 (refused before)", ageFile(t, seed, 17, pw1), 1},
		{"work factor 10 (refused before)", ageFile(t, seed, 10, pw1), 1},
		{"not an age file (refused before)", []byte("this is not an age file at all, only text"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			cs := &countingSealer{}
			e.established() // with the fake sealer; its file is replaced below
			e.opts = func(o *Options) { o.Sealer = cs }
			s := e.svc()
			mustWrite(t, e, RootFile, tc.file)
			if _, err := e.st.DB().Exec(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(tc.file)); err != nil {
				t.Fatal(err)
			}
			_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
			wantCode(t, err, ErrPassphraseInvalid)
			if cs.dummy != tc.wantDummy {
				t.Fatalf("dummy derivations = %d, want %d", cs.dummy, tc.wantDummy)
			}
			if st := e.scopeStatus(s, ui); st.Failures != 1 {
				t.Fatalf("the failure was not counted once: %+v", st)
			}
		})
	}
}

// --- Finding 7: directory sync obligations ---

func TestStartSyncsTheDirectoryEvenWhenEveryNameMatches(t *testing.T) {
	e := newEnv(t)
	e.established()
	calls := 0
	e.syncDir = func(string) error { calls++; return nil }
	e.svc()
	if calls == 0 {
		t.Fatal("a start with matching names did not sync the directory")
	}
	// A failing sync refuses the start; the next start retries it.
	e.syncDir = func(string) error { return errors.New("EIO") }
	if err := e.startErr(); err == nil {
		t.Fatal("started although the directory could not be synced")
	}
	e.syncDir = nil
	if _, err := e.svc().Current(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySyncFailureKeepsThePendingState(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	broken := true
	e.rename = func(_, _ string) error {
		if broken {
			return errors.New("EIO")
		}
		return nil
	}
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrFilesPending)
	broken = false
	// The next request completes the rename in its recovery; the sync after it fails.
	e.syncDir = func(string) error { return errors.New("EIO") }
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	if !s.syncPending {
		t.Fatal("the failed sync after the recovery rename was forgotten")
	}
	if e.has(OpNextFile) {
		t.Fatal("the rename was not done")
	}
	// Still failing: refused; the file names already match, only the sync is owed.
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrConflict)
	e.syncDir = nil
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.syncPending {
		t.Fatal("still pending after a successful sync")
	}
}

func TestStartRecoveryRenameSyncFailureIsNotForgotten(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	e.crashAt(PointDurable)
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	wantCode(t, err, ErrCrash)
	e.syncDir = func(string) error { return errors.New("EIO") }
	s2, err := New(e.options())
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Start(bg); err == nil {
		t.Fatal("started with an unsynced recovery rename")
	}
	if !s2.syncPending {
		t.Fatal("pending state lost")
	}
	e.syncDir = nil
	if err := s2.Start(bg); err != nil || s2.syncPending {
		t.Fatalf("retry: %v pending %v", err, s2.syncPending)
	}
}

func TestRotationSyncsAfterTheRootRenameBeforeTheOperationalKeyRename(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	type ev struct{ rn, n bool }
	var events []ev
	e.syncDir = func(string) error {
		events = append(events, ev{e.has(RootNextFile), e.has(OpNextFile)})
		return nil
	}
	e.rename = func(_, to string) error {
		if strings.HasSuffix(to, OpFile) {
			return errors.New("EIO") // the operational-key rename fails
		}
		return nil
	}
	_, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
	wantCode(t, err, ErrFilesPending)
	found := false
	for _, x := range events {
		if !x.rn && x.n { // root renamed, op key still staged: a sync happened here
			found = true
		}
	}
	if !found {
		t.Fatalf("no directory sync between the two renames: %+v", events)
	}
}

// --- Finding 9: admission rechecks expiry ---

func TestAdmissionRechecksCertificateExpiry(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	snap, err := s.Current() // a handshake takes its pair before the expiry
	if err != nil {
		t.Fatal(err)
	}
	e.clock = time.UnixMilli(snap.Cert.NotAfterMs - 1)
	if !s.Admit(snap.Generation) {
		t.Fatal("refused one millisecond before the expiry")
	}
	e.clock = time.UnixMilli(snap.Cert.NotAfterMs) // the handshake stalls past not_after
	closes := e.closes
	if _, err := s.Tick(bg); err != nil || e.closes != closes+1 {
		t.Fatalf("tick: %v closes %d", err, e.closes-closes)
	}
	if s.Admit(snap.Generation) {
		t.Fatal("a stalled handshake registered after the certificate expired")
	}
	// Later ticks do not close again, so admission is the only gate: still refused.
	_, _ = s.Tick(bg)
	if s.Admit(snap.Generation) || e.closes != closes+1 {
		t.Fatal("admission or closing changed")
	}
	// A re-issue serves again and a handshake of the new generation is admitted.
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	cur, err := s.Current()
	if err != nil || !s.Admit(cur.Generation) || s.Admit(snap.Generation) {
		t.Fatalf("after the re-issue: %v", err)
	}
}

// --- Finding 10: expiry processing is serialized with activation ---

func TestReissueRacingAnExpiryTickIsNotClosedByTheTick(t *testing.T) {
	e := newEnv(t)
	e.opts = func(o *Options) { o.LockWait = 5 * time.Second }
	s := e.established()
	old := e.cert()
	e.clock = time.UnixMilli(old.NotAfterMs).Add(time.Hour)
	reached, release := make(chan struct{}), make(chan struct{})
	e.point = func(n string) error {
		if n == PointAfterUnseal {
			close(reached)
			<-release
		}
		return nil
	}
	closes := e.closes
	var wg sync.WaitGroup
	var rerr, terr error
	wg.Add(1)
	go func() { defer wg.Done(); _, rerr = s.Reissue(bg, ui, pw1, IssueOptions{}) }()
	<-reached
	wg.Add(1)
	go func() { defer wg.Done(); _, terr = s.Tick(bg) }()
	time.Sleep(150 * time.Millisecond) // the tick is waiting for the activation
	close(release)
	wg.Wait()
	if rerr != nil || terr != nil {
		t.Fatalf("reissue %v tick %v", rerr, terr)
	}
	if rows := e.audits("hub_identity.expired"); len(rows) != 0 {
		t.Fatalf("the tick reported the expiry of a certificate that was replaced: %d rows", len(rows))
	}
	if e.closes != closes+1 {
		t.Fatalf("sessions closed %d times, want only the activation's close", e.closes-closes)
	}
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkExpiredIsFencedToTheCurrentSerial(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	oldSerial := int64(e.cert().Serial)
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	row := store.AuditRow{ID: store.NewID(), TS: 1, Actor: SystemActor, Action: "hub_identity.expired", Target: "x", Outcome: "success"}
	if first, err := e.st.MarkExpired(bg, oldSerial, row); err != nil || first {
		t.Fatalf("an obsolete serial was marked: %v %v", first, err)
	}
	if len(e.audits("hub_identity.expired")) != 0 {
		t.Fatal("audit row written for an obsolete serial")
	}
	row.ID = store.NewID()
	if first, err := e.st.MarkExpired(bg, int64(e.cert().Serial), row); err != nil || !first {
		t.Fatalf("the current serial: %v %v", first, err)
	}
}

// --- Coverage: real reopen of the Store (A, X, W, BS, BS5) ---

// A restart that keeps the commit: the Store is closed and a new one opened
// on the same files.
func TestRealReopenRecoveryKeepsTheCommit(t *testing.T) {
	t.Run("A2 reissue committed", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		old := e.cert()
		e.crashAt(PointCommitted)
		_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.reopen()
		s2 := e.svc()
		snap, err := s2.Current()
		if err != nil || snap.Cert.Serial == old.Serial || e.has(OpNextFile) || string(e.opPub(OpFile)) != string(snap.Cert.OpPub) {
			t.Fatalf("recovery: %v", err)
		}
	})
	t.Run("X4 rotation, root renamed and op key not", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		e.crashAt(PointRKRenamed)
		_, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.reopen()
		s2 := e.svc()
		if e.has(RootNextFile) || e.has(OpNextFile) {
			t.Fatal("pending files left")
		}
		if got, ok := e.rootOpens(pw2); !ok || string(got) != string(e.row().Pub) {
			t.Fatal("new root")
		}
		if snap, err := s2.Current(); err != nil || string(snap.RootPub) != string(e.row().Pub) {
			t.Fatalf("serving: %v", err)
		}
	})
	t.Run("W3 passphrase change confirmed, not renamed", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		e.crashAt(PointWDurable)
		_, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
		wantCode(t, err, ErrCrash)
		e.reopen()
		e.svc()
		if _, ok := e.rootOpens(pw2); !ok || e.has(RootNextFile) {
			t.Fatal("RN not renamed over RK")
		}
	})
	t.Run("BS3 setup committed from B1", func(t *testing.T) {
		e := newEnv(t)
		e.plain()
		s := e.svc()
		e.crashAt(PointBSCommitted)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.reopen()
		s2 := e.svc()
		if e.has(PlainFile) || s2.State() != StateSealedNoCert {
			t.Fatal("recovery")
		}
	})
	t.Run("BS5 first certificate committed, no previous op.key", func(t *testing.T) {
		for _, point := range []string{PointCommitted, PointDurable, PointSwapped} {
			e := newEnv(t)
			s := e.svc()
			e.crashAt(point)
			_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
			wantCode(t, err, ErrCrash)
			if e.has(OpFile) || !e.has(OpNextFile) || len(e.row().CurrentCert) == 0 {
				t.Fatalf("%s: durable facts (K %v N %v)", point, e.has(OpFile), e.has(OpNextFile))
			}
			e.reopen()
			s2 := e.svc()
			snap, err := s2.Current()
			if err != nil || s2.State() != StateEstablished || e.has(OpNextFile) || string(e.opPub(OpFile)) != string(snap.Cert.OpPub) {
				t.Fatalf("%s: recovery %v", point, err)
			}
			if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A restart that lost the commit: the older database replaces the new one
// while every file written since stays.
func TestRealReopenRecoveryWithAnOlderDatabase(t *testing.T) {
	t.Run("A2 reissue commit lost", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		old, oldOp := e.cert(), e.opPub(OpFile)
		snap := e.snapshotDB()
		e.crashAt(PointCommitted)
		_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.restoreDB(snap)
		s2 := e.svc()
		cur, err := s2.Current()
		if err != nil || cur.Cert.Serial != old.Serial || e.has(OpNextFile) || string(e.opPub(OpFile)) != string(oldOp) {
			t.Fatalf("recovery: %v", err)
		}
		if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("X2 rotation commit lost", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		oldRoot := e.row().Pub
		snap := e.snapshotDB()
		e.crashAt(PointCommitted)
		_, err := s.RotateMaster(bg, ui, pw1, pw2, pw2, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.restoreDB(snap)
		e.svc()
		if e.has(RootNextFile) || e.has(OpNextFile) {
			t.Fatal("staged files left")
		}
		if got, ok := e.rootOpens(pw1); !ok || string(got) != string(oldRoot) {
			t.Fatal("the old root is damaged")
		}
	})
	t.Run("W2 passphrase change commit lost", func(t *testing.T) {
		e := newEnv(t)
		s := e.established()
		snap := e.snapshotDB()
		e.crashAt(PointWCommitted)
		_, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2)
		wantCode(t, err, ErrCrash)
		e.restoreDB(snap)
		s2 := e.svc()
		if e.has(RootNextFile) {
			t.Fatal("RN left")
		}
		if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("BS3 setup commit lost from B1", func(t *testing.T) {
		e := newEnv(t)
		k := e.plain()
		s := e.svc()
		snap := e.snapshotDB()
		e.crashAt(PointBSCommitted)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.restoreDB(snap)
		s2 := e.svc()
		if e.has(RootFile) || !e.has(PlainFile) || s2.State() != StatePlain || string(e.opPub(PlainFile)) != string(k.Public()) {
			t.Fatal("recovery")
		}
	})
	t.Run("BS3 setup commit lost from B0", func(t *testing.T) {
		e := newEnv(t)
		s := e.svc()
		snap := e.snapshotDB()
		e.crashAt(PointBSCommitted)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.restoreDB(snap)
		s2 := e.svc()
		if e.has(RootFile) || e.has(OpNextFile) || s2.State() != StatePristine {
			t.Fatal("recovery")
		}
	})
	t.Run("BS5 first certificate commit lost, no previous op.key", func(t *testing.T) {
		e := newEnv(t)
		s := e.svc()
		var snap string
		e.point = func(n string) error {
			if n == PointBSPlainGone { // the sealed row is committed and confirmed, no certificate yet
				snap = e.snapshotDB()
			}
			return nil
		}
		e.crashAtAfter(PointCommitted)
		_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
		wantCode(t, err, ErrCrash)
		e.point = nil
		if snap == "" || e.has(OpFile) || !e.has(OpNextFile) {
			t.Fatal("durable facts")
		}
		e.restoreDB(snap)
		s2 := e.svc()
		if s2.State() != StateSealedNoCert || e.has(OpNextFile) || e.has(OpFile) {
			t.Fatalf("recovery: state %v", s2.State())
		}
		if _, err := s2.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

// crashAtAfter keeps the current hook and adds a crash at the named point.
func (e *env) crashAtAfter(name string) {
	prev := e.point
	e.point = func(n string) error {
		if prev != nil {
			if err := prev(n); err != nil {
				return err
			}
		}
		if n == name {
			return ErrCrash
		}
		return nil
	}
}

// --- Coverage: a realistic 0.6.3 database through the whole B1 setup ---

// openV063 opens the database at the end of the schema before migration 0021.
func openV063(t *testing.T, dir string) *store.Store {
	t.Helper()
	src, err := os.ReadDir(filepath.Join("..", "store", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, f := range src {
		if !strings.HasSuffix(f.Name(), ".sql") || f.Name() >= "0021" {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", "store", "migrations", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files["migrations/"+f.Name()] = &fstest.MapFile{Data: b}
	}
	st, err := store.Open(dir, store.WithMigrations(files, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestA063DatabaseKeepsItsInvitationsThroughTheFullB1Setup(t *testing.T) {
	e := newEnv(t)
	_ = e.st.Close()
	// Replace the empty current-schema database by a released 0.6.3 one.
	for _, f := range []string{store.DBFile, store.DBFile + "-wal", store.DBFile + "-shm"} {
		_ = os.Remove(filepath.Join(e.dir, f))
	}
	old := openV063(t, e.dir)
	ctx := context.Background()
	admin := store.UserRow{ID: store.NewID(), Username: "owner", Email: "owner@example.test", PasswordHash: "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$aGFzaA", CreatedAt: 1_700_000_000_000, UpdatedAt: 1_700_000_000_000}
	if created, err := old.SeedAdmin(ctx, store.SeedAdminParams{User: admin, Marker: "admin_seed"}); err != nil || !created {
		t.Fatalf("seed admin: %v %v", created, err)
	}
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(e.dir, PlainFile), k); err != nil {
		t.Fatal(err)
	}
	if err := old.PutHubIdentity(ctx, store.HubIdentityRow{Pub: k.Public(), Fingerprint: k.Fingerprint().String(), CreatedAt: 1_700_000_000_000}); err != nil {
		t.Fatal(err)
	}
	inv := []store.InvitationRow{
		{ID: store.NewID(), SecretHash: bytes.Repeat([]byte{1}, 32), Name: "laptops", Mode: store.InvitationReusable, Kind: store.WorkerKindPrivate, TTL: "unlimited", CreatorID: admin.ID, CreatedAt: 1_700_000_001_000},
		{ID: store.NewID(), SecretHash: bytes.Repeat([]byte{2}, 32), Name: "one shot", Mode: store.InvitationOneTime, Kind: store.WorkerKindPrivate, TTL: "1w", CreatorID: admin.ID, CreatedAt: 1_700_000_002_000, ExpiresAt: 4_000_000_000_000, MaxUses: 1},
	}
	for _, r := range inv {
		if err := old.InsertInvitation(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	// The 0.7 Hub opens it (migration 0021) and the administrator sets the passphrase.
	st, err := store.Open(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	s := e.svc()
	if s.State() != StatePlain || !e.row().MigratedFrom06 {
		t.Fatalf("state %v migrated %v", s.State(), e.row().MigratedFrom06)
	}
	fp := e.row().Fingerprint
	res, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	if err != nil || s.State() != StateEstablished || res.RootFingerprint != fp {
		t.Fatalf("setup: %+v %v", res, err)
	}
	if e.has(PlainFile) {
		t.Fatal("plain residue")
	}
	got, err := e.st.ListInvitations(bg, admin.ID)
	if err != nil || len(got) != len(inv) {
		t.Fatalf("invitations: %d %v", len(got), err)
	}
	for _, g := range got {
		if g.Revoked || g.RevokedAt != 0 {
			t.Fatalf("invitation %q was revoked by the setup", g.Name)
		}
	}
	if e.row().Fingerprint != fp || !e.row().RootSealed {
		t.Fatal("identity")
	}
	// The same after a restart on reopened files.
	e.reopen()
	e.svc()
	got, _ = e.st.ListInvitations(bg, admin.ID)
	if len(got) != len(inv) || got[0].Revoked || got[1].Revoked {
		t.Fatal("invitations after the restart")
	}
}
