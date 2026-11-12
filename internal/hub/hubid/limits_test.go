package hubid

import (
	"errors"
	"testing"
	"time"
)

func (e *env) scopeStatus(s *Service, a Actor) ScopeStatus {
	e.t.Helper()
	st, err := s.Status(bg)
	if err != nil {
		e.t.Fatal(err)
	}
	if a.Channel == ChannelCLI {
		return st.CLI
	}
	return st.Web
}

func rateLimited(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRateLimited {
		t.Fatalf("error = %v, want rate_limited", err)
	}
	return e
}

func TestLockoutTransitionsAndScopeIndependence(t *testing.T) {
	e := newEnv(t)
	e.opts = func(o *Options) { o.MaxOpsPerHour = 1000 } // isolate the lockout from the allowance
	s := e.established()
	fail := func(a Actor) error { _, err := s.Reissue(bg, a, pw2, IssueOptions{}); return err }
	// Failures 1 to 4 do not lock; the fifth locks for 15 minutes.
	for i := 1; i <= 4; i++ {
		wantCode(t, fail(ui), ErrPassphraseInvalid)
		if st := e.scopeStatus(s, ui); st.Locked || st.Failures != int64(i) {
			t.Fatalf("after %d failures: %+v", i, st)
		}
	}
	t5 := e.clock
	wantCode(t, fail(ui), ErrPassphraseInvalid)
	st := e.scopeStatus(s, ui)
	if !st.Locked || !st.LockedUntil.Equal(t5.Add(15*time.Minute)) || st.Failures != 5 {
		t.Fatalf("after 5: %+v", st)
	}
	// During the lock: a correct passphrase is refused without a derivation;
	// counter, lock time and allowance do not change.
	opened, attempts := e.fk.opened, st.AttemptsThisHour
	e.clock = e.clock.Add(5 * time.Minute)
	re := rateLimited(t, func() error { _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); return err }())
	if re.Allowance || !re.RetryAt.Equal(st.LockedUntil) {
		t.Fatalf("retry %+v", re)
	}
	wantCode(t, fail(ui), ErrRateLimited)
	if st2 := e.scopeStatus(s, ui); st2.Failures != 5 || !st2.LockedUntil.Equal(st.LockedUntil) || st2.AttemptsThisHour != attempts || e.fk.opened != opened {
		t.Fatalf("a locked request changed state: %+v opened %d->%d", st2, opened, e.fk.opened)
	}
	// The command scope is independent: it still works, and its success does not touch the web counters.
	if _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); err != nil {
		t.Fatalf("cli during a web lock: %v", err)
	}
	if st3 := e.scopeStatus(s, ui); st3.Failures != 5 || !st3.Locked {
		t.Fatalf("web state after a cli success: %+v", st3)
	}
	// Lock expiry does not reset the count: the sixth failure does not lock.
	e.clock = st.LockedUntil
	wantCode(t, fail(ui), ErrPassphraseInvalid)
	if st4 := e.scopeStatus(s, ui); st4.Locked || st4.Failures != 6 {
		t.Fatalf("after 6: %+v", st4)
	}
	// The lock survives a restart; so do the counters.
	for i := 7; i <= 9; i++ {
		wantCode(t, fail(ui), ErrPassphraseInvalid)
	}
	wantCode(t, fail(ui), ErrPassphraseInvalid) // tenth: 30 minutes
	t10 := e.clock
	s = e.svc()
	st = e.scopeStatus(s, ui)
	if !st.Locked || st.Failures != 10 || !st.LockedUntil.Equal(t10.Add(30*time.Minute)) {
		t.Fatalf("after 10 and a restart: %+v", st)
	}
	_ = rateLimited(t, fail(ui))
	// A success resets both fields.
	e.clock = st.LockedUntil
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if st5 := e.scopeStatus(s, ui); st5.Locked || st5.Failures != 0 {
		t.Fatalf("after a success: %+v", st5)
	}
	// The reverse: a command lock does not block the web.
	for i := 0; i < 5; i++ {
		wantCode(t, fail(cli), ErrPassphraseInvalid)
	}
	if !e.scopeStatus(s, cli).Locked {
		t.Fatal("cli not locked")
	}
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatalf("web during a cli lock: %v", err)
	}
	// Lock lengths: 15, 30, 60, 120, 240, 240 minutes.
	e2 := newEnv(t)
	e2.opts = func(o *Options) { o.MaxOpsPerHour = 1000 }
	s2 := e2.established()
	want := []time.Duration{15, 30, 60, 120, 240, 240}
	for k, m := range want {
		for i := 0; i < 5; i++ {
			_, err := s2.Reissue(bg, ui, pw2, IssueOptions{})
			wantCode(t, err, ErrPassphraseInvalid)
		}
		st := e2.scopeStatus(s2, ui)
		if !st.Locked || !st.LockedUntil.Equal(e2.clock.Add(m*time.Minute)) {
			t.Fatalf("lock %d: %+v want %v", k+1, st, m*time.Minute)
		}
		e2.clock = st.LockedUntil
	}
	// The audit rows carry the counter and the lock end.
	if rows := e2.audits("hub_identity.locked"); len(rows) != 6 || rows[0].Actor != "admin-1" {
		t.Fatalf("locked rows %d", len(rows))
	}
	if rows := e2.audits("hub_identity.unseal_failed"); len(rows) != 30 {
		t.Fatalf("unseal_failed rows %d", len(rows))
	}
}

func TestHourlyAllowancePerScope(t *testing.T) {
	e := newEnv(t)
	s := e.established()
	start := e.clock
	for i := 0; i < 10; i++ {
		if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
			t.Fatalf("operation %d: %v", i+1, err)
		}
	}
	// The eleventh in the hour is refused without a derivation and consumes nothing.
	opened := e.fk.opened
	re := rateLimited(t, func() error { _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); return err }())
	if !re.Allowance || !re.RetryAt.Equal(start.Add(time.Hour)) || e.fk.opened != opened {
		t.Fatalf("eleventh: %+v opened %d->%d", re, opened, e.fk.opened)
	}
	if st := e.scopeStatus(s, ui); st.AttemptsThisHour != 10 {
		t.Fatalf("attempts = %d", st.AttemptsThisHour)
	}
	// Exhausted web allowance and a web lock still permit a correct command.
	if _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); err != nil {
		t.Fatalf("cli with an exhausted web allowance: %v", err)
	}
	// Window ends: the web works again; a failure that reaches the derivation consumes allowance.
	e.clock = start.Add(time.Hour)
	_, err := s.Reissue(bg, ui, pw2, IssueOptions{})
	wantCode(t, err, ErrPassphraseInvalid)
	if st := e.scopeStatus(s, ui); st.AttemptsThisHour != 1 || st.Failures != 1 {
		t.Fatalf("after a failure: %+v", st)
	}
	// And the reverse: an exhausted command allowance does not block the web.
	for i := 0; i < 10; i++ { // the command window restarted at the same instant
		if _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); err != nil {
			t.Fatalf("cli %d: %v", i, err)
		}
	}
	_ = rateLimited(t, func() error { _, err := s.Reissue(bg, cli, pw1, IssueOptions{}); return err }())
	if _, err := s.Reissue(bg, ui, pw1, IssueOptions{}); err != nil {
		t.Fatalf("web with an exhausted cli allowance: %v", err)
	}
	// Refusals for validation or state reasons never reach the gate.
	e3 := newEnv(t)
	s3 := e3.established()
	for i := 0; i < 20; i++ {
		_, _ = s3.SetPassphrase(bg, ui, pw2, pw2, IssueOptions{}) // conflict in B3
	}
	if st := e3.scopeStatus(s3, ui); st.AttemptsThisHour != 0 || st.Failures != 0 {
		t.Fatalf("refused requests were counted: %+v", st)
	}
}
