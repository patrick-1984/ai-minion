package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// failingResumeRepo fails the write of the pair_resume record.
type failingResumeRepo struct {
	EnrollmentRepo
	fail  atomic.Bool
	calls atomic.Int32
}

func (r *failingResumeRepo) SavePairResume(ctx context.Context, rec store.PairResume) error {
	r.calls.Add(1)
	if r.fail.Load() {
		return errors.New("disk full")
	}
	return r.EnrollmentRepo.SavePairResume(ctx, rec)
}

// pairerOn builds a pairer over an existing enrollment state (a restart).
func pairerOn(t *testing.T, enr *Enrollments, hubURL string) *hubPairer {
	t.Helper()
	hp := &hubPairer{enr: enr, out: &logBuf{}, logs: &logBuf{}}
	p, err := NewPairer(PairerConfig{
		HubURL: hubURL, Name: "pi", Enrollment: enr, Dialer: NewPairDialer(),
		Backoff: &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		Out:     hp.out, Logger: slogTo(hp.logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	hp.Pairer = p
	return hp
}

func recordOf(t *testing.T, enr *Enrollments) (store.PairResume, bool) {
	t.Helper()
	r, ok, err := enr.LoadPairResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

func hashOf(c *identity.HubCert) []byte { h := sha256.Sum256(c.Bytes()); return h[:] }

func TestPairResumeRecordIsDurableBeforeSigWorkerAndSigWorkerFollowsAFailedWrite(t *testing.T) {
	h := newStubPairHub(t)
	var repo *failingResumeRepo
	var hp *hubPairer
	{
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		repo = &failingResumeRepo{EnrollmentRepo: st}
		hp = pairerOn(t, NewEnrollments(repo, dir), h.url())
	}
	// 1. The record is on disk when the Hub holds sig_worker.
	var atSig store.PairResume
	var had bool
	h.mu.Lock()
	h.afterAuth = func() { atSig, had = recordOf(t, hp.enr) }
	h.mu.Unlock()
	r := hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !had || atSig.Code != code || atSig.Serial != h.hub.Cert.Serial || !bytes.Equal(atSig.RootPub, h.key.Public()) ||
		!bytes.Equal(atSig.CertSHA256, hashOf(h.hub.Cert)) || atSig.ChainPosition != 0 {
		t.Fatalf("record at sig_worker: had %v %+v", had, atSig)
	}
	// The enrollment commit deleted it and carries the ratchet.
	if _, ok := recordOf(t, hp.enr); ok {
		t.Fatal("the record outlived the enrollment commit")
	}
	if e := r.enr; e.Ratchet.MaxSerial != h.hub.Cert.Serial || !bytes.Equal(e.Ratchet.CertSHA256, hashOf(h.hub.Cert)) {
		t.Fatalf("enrollment ratchet = %+v", e.Ratchet)
	}

	// 2. Error injection on the write: no sig_worker.
	h2 := newStubPairHub(t)
	repo2 := &failingResumeRepo{}
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repo2.EnrollmentRepo = st
	repo2.fail.Store(true)
	hp2 := pairerOn(t, NewEnrollments(repo2, dir), h2.url())
	hp2.start(t)
	code2 := hp2.code(t, 1)
	eventually(t, func() bool { return h2.lastID() == code2[3:7] })
	h2.claim(code2)
	eventually(t, func() bool { return repo2.calls.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if h2.welcomed.Load() != 0 {
		t.Fatal("sig_worker was sent although the record could not be written")
	}
	if _, ok := recordOf(t, hp2.enr); ok {
		t.Fatal("a record exists although the write failed")
	}
}

func TestLostWelcomeKeepsTheCodeAndTheSameCodeCompletes(t *testing.T) {
	h := newStubPairHub(t)
	h.dropBeforeWelcome.Store(1)
	hp := newHubPairer(t, h.url())
	r := hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code) // authenticated, then the connection dies before the welcome
	eventually(t, func() bool { _, ok := recordOf(t, hp.enr); return ok })
	// The Worker reconnects with the SAME code (and pairing id); the owner types it again.
	eventually(t, func() bool { return h.idCount() >= 2 })
	if ids := h.allIDs(); ids[0] != ids[1] {
		t.Fatalf("pairing ids = %v: the code must be kept", ids)
	}
	h.claim(code)
	r.wait(t)
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, hp.logs.String())
	}
	if n := len(codeRE.FindAllString(hp.out.String(), -1)); n != 1 {
		t.Fatalf("the console showed %d codes, want 1:\n%s", n, hp.out.String())
	}
	if _, ok := recordOf(t, hp.enr); ok {
		t.Fatal("record not deleted at the enrollment commit")
	}
}

func TestAnUnauthenticatedFailureStillRetiresTheCode(t *testing.T) {
	h := newStubPairHub(t)
	hp := newHubPairer(t, h.url())
	hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code[:8] + otherChar(code[8]) + code[9:])
	if fresh := hp.code(t, 2); fresh == code {
		t.Fatal("the code must be replaced after an unauthenticated failure")
	}
	if _, ok := recordOf(t, hp.enr); ok {
		t.Fatal("an unauthenticated peer produced a record")
	}
}

func TestRestartResumesTheSameCodeAndPairingID(t *testing.T) {
	h := newStubPairHub(t)
	h.dropBeforeWelcome.Store(1)
	hp := newHubPairer(t, h.url())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = hp.Run(ctx) }()
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	eventually(t, func() bool { _, ok := recordOf(t, hp.enr); return ok })
	cancel() // the Worker process ends
	<-done

	hp2 := pairerOn(t, hp.enr, h.url())
	r := hp2.start(t)
	got := hp2.code(t, 1)
	if got != code {
		t.Fatalf("resumed code %s, want %s", got, code)
	}
	if !strings.Contains(hp2.out.String(), "Resuming pairing: enter the same code again in the Hub (Add worker)") {
		t.Fatalf("console:\n%s", hp2.out.String())
	}
	eventually(t, func() bool { return h.idCount() >= 2 && h.lastID() == code[3:7] })
	h.claim(code)
	r.wait(t)
	if r.err != nil {
		t.Fatalf("err = %v\n%s", r.err, hp2.logs.String())
	}
}

func TestResumeAnsweredByAHubWithAnotherRootIsRefusedBeforeSigWorkerAndTheRecordStays(t *testing.T) {
	h := newStubPairHub(t)
	h.dropBeforeWelcome.Store(1)
	hp := newHubPairer(t, h.url())
	r := hp.start(t)
	code := hp.code(t, 1)
	eventually(t, func() bool { return h.lastID() == code[3:7] })
	h.claim(code)
	rec := func() store.PairResume {
		var out store.PairResume
		eventually(t, func() bool { var ok bool; out, ok = recordOf(t, hp.enr); return ok })
		return out
	}()
	// An impostor that learned the code (CPace would pass) answers the resume
	// with a certificate of its own root.
	imposter := hubcert.New(t)
	h.mu.Lock()
	h.hub = imposter
	h.mu.Unlock()
	welcomed := h.welcomed.Load()
	eventually(t, func() bool { return h.idCount() >= 2 })
	h.claim(code)
	eventually(t, func() bool { return strings.Contains(hp.logs.String(), "different identity") })
	time.Sleep(50 * time.Millisecond)
	if h.welcomed.Load() != welcomed {
		t.Fatal("sig_worker reached the impostor")
	}
	if got := recordOf2(t, hp.enr); !bytes.Equal(got.RootPub, rec.RootPub) || got.Code != code || got.Serial != rec.Serial {
		t.Fatalf("the record changed: %+v", got)
	}
	if _, ok, _ := hp.enr.Load(context.Background()); ok {
		t.Fatal("something was pinned")
	}
	if n := len(codeRE.FindAllString(hp.out.String(), -1)); n != 1 {
		t.Fatalf("the code changed (%d codes shown)", n)
	}
	select {
	case <-r.done:
		t.Fatalf("the pairing ended: %v", r.err)
	default:
	}
}

func recordOf2(t *testing.T, enr *Enrollments) store.PairResume {
	t.Helper()
	r, ok := recordOf(t, enr)
	if !ok {
		t.Fatal("the pairing resume record is gone")
	}
	return r
}

func TestResumeWithAHigherSerialUnderTheSameRootIsAcceptedAndALowerOneRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial uint64
		ok     bool
	}{
		{"higher serial", 9, true},
		{"lower serial", 2, false},
		{"equal serial, other bytes", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newStubPairHub(t)
			h.mu.Lock()
			h.hub = hubcert.New(t, hubcert.WithSerial(5))
			h.key = h.hub.Root
			h.mu.Unlock()
			h.dropBeforeWelcome.Store(1)
			hp := newHubPairer(t, h.url())
			r := hp.start(t)
			code := hp.code(t, 1)
			eventually(t, func() bool { return h.lastID() == code[3:7] })
			h.claim(code)
			eventually(t, func() bool { _, ok := recordOf(t, hp.enr); return ok })
			h.mu.Lock()
			h.hub.Reissue(t, tc.serial) // same root, new operational key
			h.mu.Unlock()
			eventually(t, func() bool { return h.idCount() >= 2 })
			h.claim(code)
			if tc.ok {
				r.wait(t)
				if r.err != nil {
					t.Fatal(r.err)
				}
				if r.enr.Ratchet.MaxSerial != tc.serial {
					t.Fatalf("ratchet = %+v, want serial %d", r.enr.Ratchet, tc.serial)
				}
				return
			}
			eventually(t, func() bool { return strings.Contains(hp.logs.String(), "serial-floor 5") })
			if h.welcomed.Load() != 0 {
				t.Fatal("sig_worker was sent to a Hub with a lower serial")
			}
			if got := recordOf2(t, hp.enr); got.Serial != 5 {
				t.Fatalf("record serial = %d", got.Serial)
			}
		})
	}
}

func TestAnExpiredRecordIsDeletedAndANewCodeIsShown(t *testing.T) {
	h := newStubPairHub(t)
	hp := newHubPairer(t, h.url())
	old := store.PairResume{Code: "MN-AAAA-BBBB", PairingID: "AAAA", ExpiresAt: time.Now().Add(-time.Minute),
		RootPub: h.key.Public(), Serial: 3, CertSHA256: bytes.Repeat([]byte{1}, 32)}
	if err := hp.enr.repo.SavePairResume(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	hp.start(t)
	if code := hp.code(t, 1); code == "MN-AAAA-BBBB" {
		t.Fatal("an expired code was shown again")
	}
	if _, ok := recordOf(t, hp.enr); ok {
		t.Fatal("the expired record was kept")
	}
}

func TestAlreadyPairedAfterExpiryTellsTheOwnerToReissue(t *testing.T) {
	h := newStubPairHub(t)
	h.rejectNext(protoAlreadyPaired())
	hp := newHubPairer(t, h.url())
	r := hp.start(t)
	r.wait(t)
	if r.err == nil || !strings.Contains(r.err.Error(), "The Hub already knows this Worker key. Ask the owner to use Re-issue pairing for this Worker.") {
		t.Fatalf("err = %v", r.err)
	}
}

func slogTo(w *logBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func protoAlreadyPaired() error { return proto.NewError(proto.CodeAlreadyPaired, "already paired") }

// pairAuthenticatedStep exposes the confirmation, authenticates the Hub (the
// resume record is written) and then fails with err.
func pairAuthenticatedStep(root []byte, err error) pairStep {
	return func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		p.OnExposed()
		if aerr := p.OnAuthenticated(transport.HubAuth{Root: root, Ratchet: transport.Ratchet{Root: root, MaxSerial: 4, CertSHA256: bytes.Repeat([]byte{2}, 32)}}); aerr != nil {
			return PairResult{}, aerr
		}
		return PairResult{}, err
	}
}

// An interceptor that replays the public certificate gets the Worker's CPace
// confirmation for a guess and disconnects before sig_hub. Authentication is a
// fact of one attempt: an earlier authenticated attempt, or a restored record,
// must not keep the code alive for an exposed attempt that never authenticated.
func TestAnExposedAttemptThatNeverAuthenticatedRetiresTheCodeAndItsRecord(t *testing.T) {
	root := bytes.Repeat([]byte{7}, 32)
	for _, name := range []string{"after an authenticated attempt", "after a restart from the record"} {
		t.Run(name, func(t *testing.T) {
			var steps []pairStep
			f := newPairFixture(t)
			f.codeSeq("K7Q4XY12", "B2C3D4E5")
			if name == "after a restart from the record" {
				rec := store.PairResume{Code: "MN-K7Q4-XY12", PairingID: "K7Q4", ExpiresAt: f.clk.Now().Add(5 * time.Minute),
					RootPub: root, Serial: 4, CertSHA256: bytes.Repeat([]byte{2}, 32)}
				if err := f.enr.repo.SavePairResume(context.Background(), rec); err != nil {
					t.Fatal(err)
				}
				steps = []pairStep{pairExposedStep(io.ErrUnexpectedEOF), pairWaitStep(nil)}
			} else {
				// Attempt 1 authenticates the Hub and loses the connection: the code stays.
				// Attempt 2 is an interceptor that takes the confirmation and drops.
				steps = []pairStep{pairAuthenticatedStep(root, io.ErrUnexpectedEOF), pairExposedStep(io.ErrUnexpectedEOF), pairWaitStep(nil)}
			}
			f.dial.steps = steps
			f.run(f.pairer())
			if name != "after a restart from the record" {
				eventually(t, func() bool { return f.dial.nCalls() == 1 })
				f.backoffStep() // the same code is retried after the ordinary backoff
				eventually(t, func() bool { return f.dial.nCalls() == 2 })
				if f.dial.call(1).Code.String() != "MN-K7Q4-XY12" {
					t.Fatalf("an authenticated Hub's code must be kept, got %s", f.dial.call(1).Code)
				}
			}
			// The exposed, unauthenticated attempt retires the code: after the penalty
			// delay the next attempt uses a fresh code, and the record is gone.
			eventually(t, func() bool { return len(f.delays()) >= 1 })
			f.backoffStep()
			last := len(steps) - 1
			eventually(t, func() bool { return f.dial.nCalls() == last+1 })
			if got := f.dial.call(last).Code.String(); got != "MN-B2C3-D4E5" {
				t.Fatalf("the code was reused after an unauthenticated exposure: %s", got)
			}
			if _, ok := recordOf(t, f.enr); ok {
				t.Fatal("the resume record of the retired code was kept")
			}
		})
	}
}

// --new-code replaces the identity: the resume record of the old key (its code
// and authenticated Hub) must not survive it, enrolled or not.
func TestReplacingTheIdentityDeletesThePairResumeRecord(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		name := "unpaired worker"
		if enrolled {
			name = "paired worker"
		}
		t.Run(name, func(t *testing.T) {
			f := newPairFixture(t)
			if _, err := f.enr.LoadOrCreateKey(); err != nil {
				t.Fatal(err)
			}
			if enrolled {
				enr := Enrollment{Pin: identity.Pin{Pub: bytes.Repeat([]byte{3}, 32)}, WorkerID: "w", HubURL: "https://hub.example"}
				if err := f.enr.Commit(context.Background(), enr); err != nil {
					t.Fatal(err)
				}
			}
			rec := store.PairResume{Code: "MN-K7Q4-XY12", PairingID: "K7Q4", ExpiresAt: time.Now().Add(5 * time.Minute),
				RootPub: bytes.Repeat([]byte{7}, 32), Serial: 4, CertSHA256: bytes.Repeat([]byte{2}, 32)}
			if err := f.enr.repo.SavePairResume(context.Background(), rec); err != nil {
				t.Fatal(err)
			}
			if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
				t.Fatal(err)
			}
			if _, ok := recordOf(t, f.enr); ok {
				t.Fatal("the resume record of the old identity survived --new-code")
			}
		})
	}
}
