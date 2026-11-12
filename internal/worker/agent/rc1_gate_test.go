package agent

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// RC1 release-gate reproducers (Worker side). Each test pins one guarded write
// site: the expiry check and the state reload must happen inside the lock that
// guards the write, immediately before it.

// holdStateLock takes the cross-process enrollment state lock of dir the way a
// second process would, and returns its release.
func holdStateLock(t *testing.T, dir string) func() {
	t.Helper()
	unlock, err := lockFile(context.Background(), filepath.Join(dir, enrollmentLockFile))
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

// --- Finding 1: every exposed, unauthenticated exit retires the code ---------

func TestEveryFatalExposedUnauthenticatedExitRetiresTheCodeAndItsRecord(t *testing.T) {
	root := bytes.Repeat([]byte{7}, 32)
	for name, fatal := range map[string]error{
		"revoked":        proto.NewError(proto.CodeRevoked, "removed"),
		"already_paired": protoAlreadyPaired(),
		"context ended":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPairFixture(t)
			f.codeSeq("B2C3D4E5", "C3D4E5F6")
			// A record from an earlier, authenticated run brings MN-K7Q4-XY12 back.
			rec := store.PairResume{Code: "MN-K7Q4-XY12", PairingID: "K7Q4", ExpiresAt: f.clk.Now().Add(5 * time.Minute),
				RootPub: root, Serial: 4, CertSHA256: bytes.Repeat([]byte{2}, 32)}
			if err := f.enr.repo.SavePairResume(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			exposed := make(chan struct{})
			step := pairExposedStep(fatal)
			if fatal == nil {
				step = func(ctx context.Context, p PairParams) (PairResult, error) {
					p.OnWaiting()
					p.OnExposed()
					close(exposed)
					<-ctx.Done()
					return PairResult{}, context.Cause(ctx)
				}
			}
			f.dial.steps = []pairStep{step}
			r, cancel := f.run(f.pairer())
			if fatal == nil {
				<-exposed
				cancel()
			}
			r.wait(t)
			if r.err == nil {
				t.Fatal("the run did not end with an error")
			}
			if _, ok := recordOf(t, f.enr); ok {
				t.Fatal("the resume record of an exposed, unauthenticated code survived the exit")
			}
			// A restart does not bring the exposed code back.
			f.dial.steps = []pairStep{pairWaitStep(nil)}
			f.run(f.pairer())
			eventually(t, func() bool { return f.dial.nCalls() == 2 })
			if got := f.dial.call(1).Code.String(); got == "MN-K7Q4-XY12" {
				t.Fatalf("the restart restored the exposed code %s", got)
			}
		})
	}
}

// --- Finding 2: identity replacement reads, saves and wipes under one lock --

func TestIdentityReplacementHoldsTheStateLockSoAConcurrentRatchetCommitIsNotLost(t *testing.T) {
	f := newPairFixture(t)
	if _, err := f.enr.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	root := bytes.Repeat([]byte{3}, 32)
	enr := Enrollment{Pin: identity.Pin{Pub: root}, WorkerID: "w", HubURL: "https://old-hub.example",
		Ratchet: transport.Ratchet{Root: root, MaxSerial: 10, CertSHA256: bytes.Repeat([]byte{1}, 32)}}
	if err := f.enr.Commit(context.Background(), enr); err != nil {
		t.Fatal(err)
	}
	gen, _, _, _, err := f.enr.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The running connector commits serial 20 right after the replacement read
	// serial 10.
	var commitErr error
	committed := make(chan struct{})
	renewAfterLoad = func() {
		go func() {
			defer close(committed)
			commitErr = f.enr.CommitRatchetIf(context.Background(), gen, identity.Pin{Pub: root},
				transport.Ratchet{Root: root, MaxSerial: 20, CertSHA256: bytes.Repeat([]byte{2}, 32)})
		}()
		select {
		case <-committed:
		case <-time.After(300 * time.Millisecond): // blocked by the lock: the fixed behaviour
		}
	}
	t.Cleanup(func() { renewAfterLoad = nil })
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	<-committed
	pending := loadRetired(t, f.enr.RetiredStore())
	floor := uint64(10)
	if commitErr == nil {
		floor = 20 // the connector's commit landed: the retired record must carry it
	}
	if pending.MaxSerial < floor {
		t.Fatalf("the retired record carries max_serial %d, but %d was committed (commit err %v): the higher floor was lost",
			pending.MaxSerial, floor, commitErr)
	}
}

// --- Finding 3: two retirers of one identity never regress the ratchet -----

func TestTwoRetirersOfOneIdentityNeverLowerTheRatchet(t *testing.T) {
	dir := t.TempDir()
	old, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	root := bytes.Repeat([]byte{5}, 32)
	rec := RetiredIdentity{Key: old, HubURL: "https://hub.example", Pin: identity.Pin{Pub: root}, WorkerID: "w-old",
		RetiredAt: time.Now(), MaxSerial: 10, CertSHA256: bytes.Repeat([]byte{1}, 32), RatchetSeeded: true}
	if err := NewRetiredStore(dir).Add(rec); err != nil {
		t.Fatal(err)
	}
	attempt := func(p ConnectParams, serial uint64) *Attempt {
		return &Attempt{Pin: p.Pin, Version: proto.Current, RatchetChanged: true,
			Ratchet: transport.Ratchet{Root: p.Pin.Pub, MaxSerial: serial, CertSHA256: bytes.Repeat([]byte{byte(serial)}, 32)},
			Reply:   Reply{Type: "not_a_disposition"}}
	}
	aDone := make(chan struct{})
	bDialing := make(chan struct{})
	// B (the service) validated against the stale floor 10 and saw serial 15.
	b := newRetirer(t, NewRetiredStore(dir), rec.HubURL, func(c *RetirerConfig) {
		c.Dialer = stubDialer{fn: func(p ConnectParams) (*Attempt, error) {
			close(bDialing)
			select {
			case <-aDone:
			case <-time.After(300 * time.Millisecond): // A waits for B's lock: the fixed behaviour
			}
			return attempt(p, 15), nil
		}}
	})
	// A (the CLI) saw serial 20.
	a := newRetirer(t, NewRetiredStore(dir), rec.HubURL, func(c *RetirerConfig) {
		c.Dialer = stubDialer{fn: func(p ConnectParams) (*Attempt, error) { return attempt(p, 20), nil }}
	})
	bRan := make(chan struct{})
	go func() { defer close(bRan); _, _ = b.RunOnce(context.Background()) }()
	<-bDialing
	if _, err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(aDone)
	<-bRan
	got := loadRetired(t, NewRetiredStore(dir))
	if got.MaxSerial != 20 {
		t.Fatalf("max_serial = %d after retirers saw 20 and 15, want 20", got.MaxSerial)
	}
}

func TestRetiredAdvanceIsMonotonic(t *testing.T) {
	dir := t.TempDir()
	old, _ := identity.GenerateKey()
	root := bytes.Repeat([]byte{5}, 32)
	rec := RetiredIdentity{Key: old, HubURL: "https://hub.example", Pin: identity.Pin{Pub: root}, RetiredAt: time.Now(),
		MaxSerial: 20, CertSHA256: bytes.Repeat([]byte{2}, 32), RatchetSeeded: true}
	rs := NewRetiredStore(dir)
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	stale := rec
	stale.MaxSerial = 10
	if err := rs.Advance(stale, rec.Pin, transport.Ratchet{Root: root, MaxSerial: 15, CertSHA256: bytes.Repeat([]byte{9}, 32)}, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadRetired(t, rs); got.MaxSerial != 20 || !bytes.Equal(got.CertSHA256, rec.CertSHA256) {
		t.Fatalf("Advance lowered the ratchet: %+v", got)
	}
	// Adding the same identity again (a repeated --new-code after a crash) keeps
	// the higher floor too.
	if err := rs.Add(stale); err != nil {
		t.Fatal(err)
	}
	if got := loadRetired(t, rs); got.MaxSerial != 20 {
		t.Fatalf("Add lowered the ratchet: %+v", got)
	}
}

// --- Finding 4: the invitation commit rechecks both clocks inside the guard -

type steppedInviteDialer func(context.Context, InviteParams) (*InviteOutcome, error)

func (f steppedInviteDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	return f(ctx, p)
}

func TestInviteCommitRechecksTheDeadlineAfterWaitingForTheLock(t *testing.T) {
	f := newPairFixture(t)
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	release := holdStateLock(t, f.enr.stateDir)
	d := steppedInviteDialer(func(context.Context, InviteParams) (*InviteOutcome, error) {
		// The welcome is live when it is returned; the deadline passes while the
		// commit waits for the lock another process holds.
		go func() {
			time.Sleep(100 * time.Millisecond)
			wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono())
			release()
		}()
		e := exp
		return &InviteOutcome{Pin: identity.Pin{Pub: make([]byte, 32)}, Welcome: HubWelcome{WorkerID: "w-1", Name: "pi"}, Expiry: &e}, nil
	})
	e, err := NewInviteEnroller(InviteEnrollerConfig{State: f.enr, Dialer: d, HubURL: "https://hub.example", Name: "pi", WallMono: wm, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	invite, err := identity.NewInviteKey(mustFingerprint(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Enroll(context.Background(), invite); !errors.Is(err, errEnrollExpired) {
		t.Fatalf("err = %v, want errEnrollExpired", err)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("an enrollment was committed after not_after")
	}
}

// --- Finding 5: a welcome after the deadline commits nothing ----------------

func TestConnectWelcomeAfterTheDeadlineCommitsNothing(t *testing.T) {
	for _, lockWait := range []bool{false, true} {
		name := "welcome arrived after not_after"
		if lockWait {
			name = "deadline passes while the commit waits for the lock"
		}
		t.Run(name, func(t *testing.T) {
			wm := newFakeWM()
			exp := wm.expiryAfter(time.Hour)
			past := func() { wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()) }
			f := newConnFixture(t, func(cfg *ConnectorConfig) { cfg.WallMono = wm })
			f.dial.script(func(p ConnectParams) (*Attempt, error) {
				if lockWait {
					release := holdStateLock(t, f.dir)
					go func() {
						time.Sleep(100 * time.Millisecond)
						past()
						release()
					}()
				} else {
					past()
				}
				e := exp
				w := okWelcome()
				w.Policy.PreviewOnly = true
				return &Attempt{
					Pin: p.Pin, OpPub: p.Pin.Pub, TranscriptHash: randomHash(), Version: proto.Current, Expiry: &e,
					Ratchet:        transport.Ratchet{Root: p.Pin.Pub, MaxSerial: 20, CertSHA256: bytes.Repeat([]byte{4}, 32)},
					RatchetChanged: true,
					Reply:          Reply{Type: proto.TypeWelcome, Welcome: w}, Session: newFakeSession(),
				}, nil
			})
			f.run()
			eventually(t, func() bool {
				st := f.conn.Status()
				return f.dial.count() >= 1 && (st.LastError != "" || st.State == ConnConnected || f.storedEnrollment().Ratchet.MaxSerial != 0)
			})
			got := f.storedEnrollment()
			if got.Ratchet.MaxSerial != 0 || got.WorkerID != "" || got.Owner != "" {
				t.Fatalf("an expired welcome was committed: %+v", got)
			}
			if f.policy.HubPreviewOnly() || f.policy.WorkerID() != "" {
				t.Fatal("an expired welcome changed the policy")
			}
			if f.conn.Status().State == ConnConnected {
				t.Fatal("an expired welcome was served")
			}
		})
	}
}

func TestHubDialerRefusesAControlFrameReadAfterTheDeadline(t *testing.T) {
	h := newStubHub(t)
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h.enrol(k.Public())
	wm := realClockWM()
	d := hubDialer{dialTimeout: 5 * time.Second, handshakeTimeout: 10 * time.Second,
		afterReply: func() { wm.set(time.UnixMilli(h.hub.Cert.NotAfterMs), wm.Mono()) }}
	att, err := d.Connect(context.Background(), ConnectParams{HubURL: h.url(), Key: k, Name: "pi",
		Pin: identity.Pin{Pub: h.key.Public()}, Clock: wm})
	if att != nil && att.Session != nil {
		_ = att.Session.Close()
	}
	if !errors.Is(err, ErrHubCertExpired) || att != nil {
		t.Fatalf("attempt %v err %v, want ErrHubCertExpired", att, err)
	}
}

// --- Finding 6: the deadline passes during the resume-record write ----------

// slowResumeRepo runs during the durable write of the pair_resume record.
type slowResumeRepo struct {
	EnrollmentRepo
	during func()
}

func (r *slowResumeRepo) SavePairResume(ctx context.Context, rec store.PairResume) error {
	if r.during != nil {
		r.during()
	}
	return r.EnrollmentRepo.SavePairResume(ctx, rec)
}

func TestNoSigWorkerWhenTheDeadlinePassesDuringTheResumeRecordWrite(t *testing.T) {
	h := newStubPairHub(t)
	wm := realClockWM()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wrote := make(chan struct{}, 8)
	repo := &slowResumeRepo{EnrollmentRepo: st, during: func() {
		wm.set(time.UnixMilli(h.hub.Cert.NotAfterMs), wm.Mono())
		wrote <- struct{}{}
	}}
	hp := pairerOn(t, NewEnrollments(repo, dir), h.url())
	hp.cfg.WallMono = wm
	hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the record was never written")
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.welcomed.Load() != 0 {
			t.Fatal("sig_worker was sent after the deadline passed during the record write")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
