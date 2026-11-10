package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Pairing and a second writer on one state directory (AD-8: a paired Worker
// refuses new codes). The second writer is a second Enrollments value over the
// same state directory and database, like the setup wizard, another
// `minion worker pair` or an invitation enrollment of the service.

func (f *pairFixture) otherWriter() *Enrollments { return NewEnrollments(f.st, f.enr.stateDir) }

func otherEnrollment(owner string) Enrollment {
	pub := make([]byte, 32)
	pub[0] = 9
	return Enrollment{Pin: identity.Pin{Pub: pub}, WorkerID: "w-other", Name: "pi", Owner: owner, Kind: "private"}
}

func (r *pairRun) requireAlreadyPaired(t *testing.T) {
	t.Helper()
	var pe *proto.Error
	if !errors.As(r.err, &pe) || pe.Code != proto.CodeAlreadyPaired {
		t.Fatalf("err = %v, want already_paired", r.err)
	}
}

func (f *pairFixture) requireEnrollmentOf(owner string) {
	f.t.Helper()
	enr, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok || enr.Owner != owner {
		f.t.Fatalf("enrollment = %+v ok=%v err=%v, want the one of %s", enr, ok, err, owner)
	}
}

func TestPairingStopsWithAlreadyPairedWhenAnotherWriterEnrollsDuringTheWait(t *testing.T) {
	var attemptEnded atomic.Bool
	wait := func(ctx context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		<-ctx.Done()
		attemptEnded.Store(true)
		return PairResult{}, context.Cause(ctx)
	}
	f := newPairFixture(t, wait)
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.EpochPoll = 10 * time.Millisecond }))
	eventually(t, func() bool { return len(f.shown()) == 1 })

	if err := f.otherWriter().Commit(context.Background(), otherEnrollment("bob")); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	r.requireAlreadyPaired(t)
	if !strings.Contains(r.err.Error(), "bob") {
		t.Errorf("message = %v, want the owner of the enrollment that exists", r.err)
	}
	if !attemptEnded.Load() {
		t.Error("the attempt in flight was not cancelled")
	}
	if f.dial.nCalls() != 1 {
		t.Errorf("%d attempts, want 1: a paired Worker is offered no further code", f.dial.nCalls())
	}
	if strings.Contains(f.out.String(), "paired to") {
		t.Errorf("console printed a pairing that did not happen:\n%s", f.out.String())
	}
	f.requireEnrollmentOf("bob")
}

func TestPairingNeverOverwritesAnEnrollmentWrittenBeforeItsCommit(t *testing.T) {
	var other *Enrollments
	racing := func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		// The Hub accepted the code, but another writer enrolled first.
		if err := other.Commit(context.Background(), otherEnrollment("bob")); err != nil {
			return PairResult{}, err
		}
		return pairOkStep(make([]byte, 32), "alice")(context.Background(), p)
	}
	f := newPairFixture(t, racing)
	other = f.otherWriter()
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.EpochPoll = time.Hour }))
	r.wait(t)
	r.requireAlreadyPaired(t)
	if strings.Contains(f.out.String(), "paired to") {
		t.Errorf("console printed a pairing that did not happen:\n%s", f.out.String())
	}
	f.requireEnrollmentOf("bob")
}

func TestPairingChecksTheEnrollmentAfterEveryFailure(t *testing.T) {
	var other *Enrollments
	failing := func(_ context.Context, _ PairParams) (PairResult, error) {
		if err := other.Commit(context.Background(), otherEnrollment("bob")); err != nil {
			return PairResult{}, err
		}
		return PairResult{}, errors.New("dial the hub: connection refused")
	}
	f := newPairFixture(t, failing, pairWaitStep(nil))
	other = f.otherWriter()
	r, _ := f.run(f.pairer())
	r.wait(t)
	r.requireAlreadyPaired(t)
	if f.dial.nCalls() != 1 {
		t.Errorf("%d attempts, want 1: the run ends at the failure, before the backoff wait", f.dial.nCalls())
	}
	f.requireEnrollmentOf("bob")
}

func TestPairingAfterAResetOfTheStateStopsInsteadOfUsingAStaleKey(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.EpochPoll = 10 * time.Millisecond }))
	eventually(t, func() bool { return len(f.shown()) == 1 })
	// `--new-code` in another process replaces the key and wipes: the generation moves
	// and nobody is paired.
	if _, err := RenewWorkerIdentity(context.Background(), f.otherWriter()); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	if !errors.Is(r.err, errPairingStateChanged) {
		t.Fatalf("err = %v, want the state-changed error", r.err)
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("nothing may be stored")
	}
}

// A pairing that has not stored anything yet waits for an invitation
// enrollment that holds the enrollment lock, sees what it committed, and stops.
func TestPairingWaitsForTheEnrollmentLockAndSeesAnInvitationEnrollment(t *testing.T) {
	f := newPairFixture(t, pairWaitStep(nil))
	release, err := lockEnrollment(context.Background(), filepath.Join(f.enr.stateDir, enrollLockName))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.run(f.pairer())
	time.Sleep(100 * time.Millisecond)
	if _, err := f.enr.LoadKey(); !errors.Is(err, ErrNoWorkerKey) {
		t.Fatalf("a Worker key was created while the enrollment lock was held: %v", err)
	}
	if f.dial.nCalls() != 0 {
		t.Fatal("the pairing connected while an enrollment held the lock")
	}
	if err := f.otherWriter().Commit(context.Background(), otherEnrollment("bob")); err != nil {
		t.Fatal(err)
	}
	release()
	r.wait(t)
	r.requireAlreadyPaired(t)
	if _, err := f.enr.LoadKey(); !errors.Is(err, ErrNoWorkerKey) {
		t.Fatalf("a paired Worker got a new key: %v", err)
	}
}

func TestRenewWorkerIdentityWaitsForTheEnrollmentLock(t *testing.T) {
	f := newPairFixture(t)
	old, err := f.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	release, err := lockEnrollment(context.Background(), filepath.Join(f.enr.stateDir, enrollLockName))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := RenewWorkerIdentity(context.Background(), f.otherWriter()); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the key was replaced while an enrollment held the lock (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	if k, err := f.enr.LoadKey(); err != nil || string(k.Public()) != string(old.Public()) {
		t.Fatal("the key changed under the lock")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if k, err := f.enr.LoadKey(); err != nil || string(k.Public()) == string(old.Public()) {
		t.Fatal("the key was not replaced after the lock was released")
	}
}

// An unauthenticated peer that answers pairing_code_invalid before the
// confirmation (the code stays) must not reset the delay that its exposed
// guesses earn.
func TestUnexposedErrorsDoNotResetTheDelayOfExposedGuesses(t *testing.T) {
	invalid := proto.NewError(proto.CodePairingCodeInvalid, "wrong")
	f := newPairFixture(t,
		pairExposedStep(invalid), pairWaitStep(invalid),
		pairExposedStep(invalid), pairWaitStep(invalid),
		pairExposedStep(invalid), pairWaitStep(nil))
	f.run(f.pairer())
	got := f.waitDelays(5)
	// exposed (penalty), unexposed (ordinary), exposed, unexposed, exposed.
	want := []time.Duration{750 * time.Millisecond, 750 * time.Millisecond, 1500 * time.Millisecond, 750 * time.Millisecond, 3 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want %v (the exposure penalty must keep growing)", got, want)
		}
	}
}

// The commit records the Hub address that authenticated the enrollment; a
// replacement of the identity retires the old key there, whatever the
// configuration says by then.
func TestCommitRecordsTheHubAddressAndRenewRetiresTheOldKeyThere(t *testing.T) {
	f := newPairFixture(t, pairOkStep(make([]byte, 32), "alice"))
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	enr, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok || enr.HubURL != "https://hub.example:8443" {
		t.Fatalf("enrollment = %+v ok=%v err=%v, want the Hub address of the pairing", enr, ok, err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	pending, err := f.enr.RetiredStore().List()
	if err != nil || len(pending) != 1 || pending[0].HubURL != "https://hub.example:8443" {
		t.Fatalf("pending = %+v err=%v, want the Hub of the enrollment, not the configured one", pending, err)
	}
	// The record ends with the enrollment: a later enrollment without an
	// address does not inherit it.
	if _, err := os.Stat(f.enr.hubURLPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Hub address of a wiped enrollment survived: %v", err)
	}
	if err := f.enr.Commit(context.Background(), otherEnrollment("bob")); err != nil {
		t.Fatal(err)
	}
	if enr, _, _ := f.enr.Load(context.Background()); enr.HubURL != "" {
		t.Errorf("HubURL = %q, want empty for an enrollment that recorded none", enr.HubURL)
	}
}

func TestCommitIfRefusesAMovedGeneration(t *testing.T) {
	f := newPairFixture(t)
	gen, _, _, _, err := f.enr.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.otherWriter().Commit(context.Background(), otherEnrollment("bob")); err != nil {
		t.Fatal(err)
	}
	if err := f.enr.CommitIf(context.Background(), gen, otherEnrollment("alice")); !errors.Is(err, ErrEnrollmentChanged) {
		t.Fatalf("CommitIf = %v, want ErrEnrollmentChanged", err)
	}
	f.requireEnrollmentOf("bob")
}

// An invitation enrollment records the Hub it was made at, so that replacing
// the identity later (with another Hub configured by then) retires the old key
// at that Hub and not at the configured one.
func TestInviteEnrollmentRecordsTheHubAndRenewRetiresTheOldKeyThere(t *testing.T) {
	f := newPairFixture(t)
	invite, _ := newInviteKey(t)
	log := &stepLog{}
	e, err := NewInviteEnroller(InviteEnrollerConfig{
		State: f.enr, Dialer: &fakeDialer{log: log}, HubURL: "https://hub-a.example:8443", Name: "pi", Sleep: noSleep,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Enroll(context.Background(), invite); err != nil {
		t.Fatal(err)
	}
	enr, ok, err := f.enr.Load(context.Background())
	if err != nil || !ok || enr.HubURL != "https://hub-a.example:8443" {
		t.Fatalf("enrollment = %+v ok=%v err=%v, want the Hub of the invitation", enr, ok, err)
	}
	if _, err := RenewWorkerIdentity(context.Background(), f.enr); err != nil {
		t.Fatal(err)
	}
	pending, err := f.enr.RetiredStore().List()
	if err != nil || len(pending) != 1 || pending[0].HubURL != "https://hub-a.example:8443" {
		t.Fatalf("pending = %+v err=%v, want Hub A", pending, err)
	}
}
