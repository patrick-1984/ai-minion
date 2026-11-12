package hubid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// --- F1: a first certificate whose commit is unconfirmed completes on retry ---

func TestUnconfirmedFirstCertificateCompletesOnRetryWithOneGenerationBump(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	committed := false
	e.point = func(n string) error {
		if n == PointCommitted {
			committed = true
		}
		return nil
	}
	e.makeDurable = func(context.Context) error {
		if committed {
			return errors.New("checkpoint failed")
		}
		return nil
	}
	_, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	wantCode(t, err, ErrNotDurable)
	if s.State() != StateSealedNoCert || s.Generation() != 1 {
		t.Fatalf("state %v gen %d", s.State(), s.Generation())
	}
	if _, err := s.Current(); err == nil {
		t.Fatal("served before the commit was confirmed")
	}
	committed = false // the checkpoint works again
	closes := e.closes
	res, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Kind != "passphrase_set" || res.Serial == 0 || res.Generation != 2 {
		t.Fatalf("result %+v", res)
	}
	if s.State() != StateEstablished || s.Generation() != 2 || e.closes != closes+1 {
		t.Fatalf("state %v gen %d closes %d", s.State(), s.Generation(), e.closes-closes)
	}
	sn, err := s.Current()
	if err != nil || sn.Cert.Serial != res.Serial || !s.Admit(sn.Generation) {
		t.Fatalf("not serving the committed certificate: %v", err)
	}
	if e.cert().Serial != res.Serial {
		t.Fatal("a second serial was issued")
	}
}

// --- F2: a started attempt is durable before the derivation ---

func failN(t *testing.T, s *Service, n int, pass string) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := s.Reissue(bg, ui, pass, IssueOptions{})
		wantCode(t, err, ErrPassphraseInvalid)
	}
}

func TestRestartAfterAFailedFailureRecordLocksAtTheFifth(t *testing.T) {
	e := newEnv(t)
	fs := &flakyStore{Store: e.st}
	e.opts = func(o *Options) { o.Store = fs }
	s := e.established()
	failN(t, s, 4, pw2)
	fs.failRecord = true
	failN(t, s, 1, pw2) // the fifth reaches the derivation; recording fails
	fs.failRecord = false
	if got := e.row().Web.Failures; got != 4 {
		t.Fatalf("failures in the database = %d, want 4 (the record failed)", got)
	}
	e.opts = nil
	s2 := e.svc() // restart: the unsettled attempt counts as a failure
	row := e.row()
	if row.Web.Failures != 5 || e.clock.UnixMilli() >= row.Web.LockedUntil || row.Web.Pending {
		t.Fatalf("after restart: %+v", row.Web)
	}
	if row.CLI != (store.ScopeState{}) {
		t.Fatalf("the cli scope changed: %+v", row.CLI)
	}
	opened := e.fk.opened
	_, err := s2.Reissue(bg, ui, pw1, IssueOptions{})
	_ = rateLimited(t, err)
	if e.fk.opened != opened {
		t.Fatal("derived although the web scope is locked")
	}
	if n := len(e.audits(store.ActionLocked)); n != 1 {
		t.Fatalf("lock audit rows: %d", n)
	}
	// The local scope is independent and still works.
	if _, err := s2.Reissue(bg, cli, pw1, IssueOptions{}); err != nil {
		t.Fatalf("cli: %v", err)
	}
}

func TestRestartAfterALostUnconfirmedFailureCommitStillCounts(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	failN(t, s, 4, pw2)
	var snap string
	calls := 0
	e.makeDurable = func(context.Context) error {
		calls++
		if calls == 1 { // the gate's confirmation: the attempt intent is on disk now
			snap = e.snapshotDB()
			return nil
		}
		return errors.New("checkpoint failed") // the failure record is never proved
	}
	failN(t, s, 1, pw2)
	e.makeDurable = nil
	e.restoreDB(snap) // power loss: only what was confirmed survived
	if row := e.row(); row.Web.Failures != 4 || !row.Web.Pending {
		t.Fatalf("durable state before restart: %+v", row.Web)
	}
	s2 := e.svc()
	if row := e.row(); row.Web.Failures != 5 || row.Web.Pending || e.clock.UnixMilli() >= row.Web.LockedUntil {
		t.Fatalf("after restart: %+v", row.Web)
	}
	_, err := s2.Reissue(bg, ui, pw1, IssueOptions{})
	_ = rateLimited(t, err)
}

func TestASuccessClearsTheAttemptIntentAndARestartAddsNothing(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	failN(t, s, 2, pw2)
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if row := e.row(); row.Web.Pending || row.Web.Failures != 0 {
		t.Fatalf("after success: %+v", row.Web)
	}
	// A success that does not reach a commit (a refused rotation argument
	// path is not reachable here) still clears the intent at the unseal.
	if _, err := s.ChangePassphrase(bg, ui, pw1, pw2, pw2); err != nil {
		t.Fatal(err)
	}
	failN(t, s, 1, pw1)
	e.reopen()
	s2 := e.svc()
	if row := e.row(); row.Web.Pending || row.Web.Failures != 1 {
		t.Fatalf("after restart: %+v", row.Web)
	}
	_ = s2
}

func TestALockRefusedRequestLeavesNoIntent(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	failN(t, s, 5, pw2)
	before := e.row().Web
	_, err := s.Reissue(bg, ui, pw1, IssueOptions{})
	_ = rateLimited(t, err)
	if after := e.row().Web; after != before || after.Pending {
		t.Fatalf("a refused request changed the scope: %+v -> %+v", before, after)
	}
}

// --- F3: expiry follows the serving snapshot ---

func TestExpiryWhileAnIssuanceIsUnconfirmedClosesSessionsAndRefusesAdmission(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	snap, err := s.Current()
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	e.confirmFailsAfterCommit(&fail)
	// A new certificate with a much later end is committed but not confirmed.
	_, err = s.Reissue(bg, ui, pw1, IssueOptions{ValidityDays: 400})
	wantCode(t, err, ErrNotDurable)
	if e.cert().NotAfterMs <= snap.Cert.NotAfterMs {
		t.Fatal("the unconfirmed certificate must outlive the serving one")
	}
	if cur, err := s.Current(); err != nil || cur.Generation != snap.Generation {
		t.Fatalf("serving changed: %v", err)
	}
	e.clock = time.UnixMilli(snap.Cert.NotAfterMs).Add(time.Minute)
	closes := e.closes
	if _, err := s.Tick(bg); err != nil {
		t.Fatal(err)
	}
	if e.closes != closes+1 {
		t.Fatalf("sessions closed %d times, want 1", e.closes-closes)
	}
	if s.Admit(snap.Generation) {
		t.Fatal("admitted with an expired serving certificate")
	}
	if _, err := s.Current(); err == nil {
		t.Fatal("served an expired certificate")
	}
	if _, err := s.Tick(bg); err != nil || e.closes != closes+1 {
		t.Fatalf("a second tick closed again: %v", err)
	}
}

// The activation hook runs once per activation, after the swap and the closing
// of the sessions, and sees the new generation.
func TestActivationHookRunsAfterEveryActivation(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	var gens []uint64
	s.SetActivationHook(func() {
		sn, err := s.Current()
		if err != nil {
			t.Errorf("hook: %v", err)
			return
		}
		gens = append(gens, sn.Generation)
	})
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 || gens[0] != 2 || gens[1] != 3 {
		t.Fatalf("hook generations %v, want [2 3]", gens)
	}
}
