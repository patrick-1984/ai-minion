package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// memActAs adds the act-as port to memStore. Like the real store it deletes
// an act-as row with its session and writes the audit row of EndActAs only
// when it deleted something.
type memActAs struct {
	*memStore
	mu2     sync.Mutex
	rows    map[string]store.ActAsRow
	audits  []store.AuditRow
	failGet error
	failEnd error
}

func newMemActAs(users ...store.UserRow) *memActAs {
	return &memActAs{memStore: newMemStore(users...), rows: map[string]store.ActAsRow{}}
}

func (m *memActAs) StartActAs(_ context.Context, row store.ActAsRow) error {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	if _, ok := m.rows[row.SessionID]; ok {
		return store.ErrActAsActive
	}
	m.rows[row.SessionID] = row
	return nil
}

func (m *memActAs) ActAsBySession(_ context.Context, id string) (store.ActAsInfo, error) {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	if m.failGet != nil {
		return store.ActAsInfo{}, m.failGet
	}
	row, ok := m.rows[id]
	if !ok {
		return store.ActAsInfo{}, store.ErrNotFound
	}
	t := m.users[row.TargetID]
	return store.ActAsInfo{ActAsRow: row, TargetUsername: t.Username, TargetActive: t.Active}, nil
}

func (m *memActAs) EndActAs(_ context.Context, id string) (bool, error) {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	if m.failEnd != nil {
		return false, m.failEnd
	}
	if _, ok := m.rows[id]; !ok {
		return false, nil
	}
	delete(m.rows, id)
	return true, nil
}

// EndActAsIf deletes the row only when it still is the period the caller
// read, and writes the audit row in the same step, like the real store.
func (m *memActAs) EndActAsIf(_ context.Context, row store.ActAsRow, audit store.AuditRow) (bool, error) {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	if m.failEnd != nil {
		return false, m.failEnd
	}
	if cur, ok := m.rows[row.SessionID]; !ok || cur != row {
		return false, nil
	}
	delete(m.rows, row.SessionID)
	m.audits = append(m.audits, audit)
	return true, nil
}

func (m *memActAs) UserByID(_ context.Context, id string) (store.UserRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		return u, nil
	}
	return store.UserRow{}, store.ErrNotFound
}

var bob = store.UserRow{ID: "u-bob", Username: "bob", Role: "user", Active: true, CredentialVersion: 1}

type actAsFixture struct {
	m     *Manager
	ms    *memActAs
	clk   *fakeClock
	admin Issued // alice's session
	sess  *Session
}

func newActAsFixture(t *testing.T) *actAsFixture {
	t.Helper()
	ms := newMemActAs(alice, bob)
	clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	m := NewManager(ms, WithClock(clk.Now))
	is, err := m.Create(context.Background(), alice, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Resolve(context.Background(), is.Token)
	if err != nil {
		t.Fatal(err)
	}
	return &actAsFixture{m: m, ms: ms, clk: clk, admin: is, sess: s}
}

func (f *actAsFixture) resolve(t *testing.T) *Session {
	t.Helper()
	s, err := f.m.Resolve(context.Background(), f.admin.Token)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewManagerDetectsTheActAsPort(t *testing.T) {
	if NewManager(newMemActAs()).actAs == nil {
		t.Error("a store with the act-as port did not enable act-as")
	}
	if NewManager(newMemStore()).actAs != nil {
		t.Error("a store without the act-as port enabled act-as")
	}
	_, err := NewManager(newMemStore(alice)).StartActAs(context.Background(), &Session{UserID: alice.ID}, bob.ID, 0)
	if !errors.Is(err, ErrActAsUnavailable) {
		t.Errorf("StartActAs without the port = %v, want ErrActAsUnavailable", err)
	}
}

func TestStartActAsResolvesTheSessionAsTheTarget(t *testing.T) {
	f := newActAsFixture(t)
	ctx := context.Background()
	if f.sess.ActAs != nil || f.sess.Principal().Kind != authz.KindBrowserSession {
		t.Fatalf("a new session is acting: %+v", f.sess)
	}
	got, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetID != bob.ID || got.TargetUsername != "bob" {
		t.Errorf("ActAs = %+v", got)
	}
	if want := f.clk.Now().Add(time.Hour); !got.ExpiresAt.Equal(want) {
		t.Errorf("default expiry = %v, want %v (one hour)", got.ExpiresAt, want)
	}

	s := f.resolve(t)
	if s.ActAs == nil || s.ActAs.TargetID != bob.ID || s.ActAs.TargetUsername != "bob" {
		t.Fatalf("resolved ActAs = %+v", s.ActAs)
	}
	// The session keeps the admin's identity for the banner and audit ...
	if s.UserID != alice.ID || s.Username != "alice" || s.Role != authz.RoleAdmin {
		t.Errorf("session = %+v, want it to stay the admin's", s)
	}
	// ... and acts with the target's permissions, for the actor.
	p := s.Principal()
	if p.Kind != authz.KindActAs || p.ActorID != alice.ID || p.UserID != bob.ID || !p.ActAsExpires.Equal(got.ExpiresAt) {
		t.Errorf("principal = %+v, want act-as: actor alice, user bob, until the expiry", p)
	}
}

func TestActAsDurationLimits(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		d  time.Duration
		ok bool
	}{
		{0, true}, // default
		{MinActAsDuration, true},
		{90 * time.Minute, true},
		{MaxActAsDuration, true},
		{MinActAsDuration - time.Second, false},
		{MaxActAsDuration + time.Second, false},
		{-time.Hour, false},
	} {
		f := newActAsFixture(t)
		_, err := f.m.StartActAs(ctx, f.sess, bob.ID, tc.d)
		if tc.ok && err != nil {
			t.Errorf("%v: %v", tc.d, err)
		}
		if !tc.ok && !errors.Is(err, ErrActAsDuration) {
			t.Errorf("%v: err = %v, want ErrActAsDuration", tc.d, err)
		}
		if !tc.ok && len(f.ms.rows) != 0 {
			t.Errorf("%v: a refused start stored a row", tc.d)
		}
	}
	if DefaultActAsDuration != time.Hour || MaxActAsDuration != 8*time.Hour {
		t.Errorf("limits = %v / %v, want 1h / 8h (AD-10)", DefaultActAsDuration, MaxActAsDuration)
	}
}

func TestStartActAsRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("a plain user cannot start", func(t *testing.T) {
		f := newActAsFixture(t)
		is, _ := f.m.Create(ctx, bob, time.Hour)
		s, _ := f.m.Resolve(ctx, is.Token)
		if _, err := f.m.StartActAs(ctx, s, alice.ID, 0); !errors.Is(err, ErrActAsNotAdmin) {
			t.Errorf("err = %v, want ErrActAsNotAdmin", err)
		}
	})
	t.Run("the role is read again from the store", func(t *testing.T) {
		f := newActAsFixture(t)
		demoted := alice
		demoted.Role = "user"
		f.ms.users[alice.ID] = demoted
		if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); !errors.Is(err, ErrActAsNotAdmin) {
			t.Errorf("a demoted admin: err = %v, want ErrActAsNotAdmin", err)
		}
	})
	t.Run("oneself", func(t *testing.T) {
		f := newActAsFixture(t)
		if _, err := f.m.StartActAs(ctx, f.sess, alice.ID, 0); !errors.Is(err, ErrActAsSelf) {
			t.Errorf("err = %v, want ErrActAsSelf", err)
		}
	})
	t.Run("unknown target", func(t *testing.T) {
		f := newActAsFixture(t)
		if _, err := f.m.StartActAs(ctx, f.sess, "u-ghost", 0); !errors.Is(err, ErrActAsTargetUnknown) {
			t.Errorf("err = %v, want ErrActAsTargetUnknown", err)
		}
	})
	t.Run("deactivated target", func(t *testing.T) {
		f := newActAsFixture(t)
		off := bob
		off.Active = false
		f.ms.users[bob.ID] = off
		if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); !errors.Is(err, ErrActAsTargetInactive) {
			t.Errorf("err = %v, want ErrActAsTargetInactive", err)
		}
	})
	t.Run("target without any grant", func(t *testing.T) {
		f := newActAsFixture(t)
		none := bob
		none.Role = ""
		f.ms.users[bob.ID] = none
		if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); !errors.Is(err, ErrActAsTargetNoAccess) {
			t.Errorf("err = %v, want ErrActAsTargetNoAccess", err)
		}
	})
	t.Run("no nesting", func(t *testing.T) {
		f := newActAsFixture(t)
		if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); err != nil {
			t.Fatal(err)
		}
		// A second request with a stale view of the session, and one with the
		// fresh view, both find the session acting.
		if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); !errors.Is(err, ErrActAsActive) {
			t.Errorf("stale session: err = %v, want ErrActAsActive", err)
		}
		if _, err := f.m.StartActAs(ctx, f.resolve(t), bob.ID, 0); !errors.Is(err, ErrActAsActive) {
			t.Errorf("fresh session: err = %v, want ErrActAsActive", err)
		}
	})
}

func TestActAsEndsAutomaticallyAtExpiry(t *testing.T) {
	f := newActAsFixture(t)
	ctx := context.Background()
	if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 30*time.Minute); err != nil {
		t.Fatal(err)
	}

	f.clk.Add(30*time.Minute - time.Millisecond)
	s := f.resolve(t)
	if s.ActAs == nil || s.Principal().Kind != authz.KindActAs {
		t.Fatalf("one millisecond before the expiry the session is not acting: %+v", s.ActAs)
	}
	if got := s.ActAs.MinutesLeft(f.clk.Now()); got != 1 {
		t.Errorf("MinutesLeft just before the expiry = %d, want 1", got)
	}

	f.clk.Add(time.Millisecond) // the expiry itself
	s = f.resolve(t)
	if s.ActAs != nil || s.Principal().Kind != authz.KindBrowserSession || s.Principal().UserID != alice.ID {
		t.Fatalf("at the expiry the session still acts: %+v", s.ActAs)
	}
	if len(f.ms.rows) != 0 {
		t.Error("the expired act-as row was not removed")
	}
	if len(f.ms.audits) != 1 {
		t.Fatalf("audit rows = %+v, want one act_as.expire", f.ms.audits)
	}
	a := f.ms.audits[0]
	if a.Actor != alice.ID || a.ActingAs != bob.ID || a.Action != "act_as.expire" || a.Target != bob.ID || a.Outcome != "success" || a.ID == "" {
		t.Errorf("expire audit = %+v, want actor alice, acting_as bob", a)
	}
	f.resolve(t) // a later request adds nothing
	if len(f.ms.audits) != 1 {
		t.Errorf("a later request wrote another audit row: %+v", f.ms.audits)
	}
}

func TestExpiredActAsIsNeverServedEvenWhenTheCleanupFails(t *testing.T) {
	f := newActAsFixture(t)
	ctx := context.Background()
	if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(time.Minute)
	f.ms.failEnd = errors.New("database is locked")
	s := f.resolve(t)
	if s.ActAs != nil {
		t.Fatal("an expired act-as was served because the cleanup failed")
	}
	f.ms.failEnd = nil
	f.resolve(t) // the next request retries the cleanup
	if len(f.ms.rows) != 0 || len(f.ms.audits) != 1 {
		t.Errorf("after the retry: %d rows, %d audits; want 0 and 1", len(f.ms.rows), len(f.ms.audits))
	}
}

// ActAs that cannot work any more is ended on the next request, audited once,
// and the session goes on as the plain session of its user.
func TestActAsEndsWhenTheTargetIsDeactivated(t *testing.T) {
	f := newActAsFixture(t)
	if _, err := f.m.StartActAs(context.Background(), f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	off := bob
	off.Active = false
	f.ms.users[bob.ID] = off

	s := f.resolve(t)
	if s.ActAs != nil || s.Principal().Kind != authz.KindBrowserSession || s.Principal().UserID != alice.ID {
		t.Fatalf("session after the target was deactivated: ActAs = %+v, principal = %+v", s.ActAs, s.Principal())
	}
	f.resolve(t) // a later request adds nothing
	f.requireOneEndAudit(t)
}

// F-07: a target that lost its last grant is denied everything, act_as_stop
// included, so the period ends and the admin goes on as itself.
func TestActAsEndsWhenTheTargetLosesEveryGrant(t *testing.T) {
	f := newActAsFixture(t)
	if _, err := f.m.StartActAs(context.Background(), f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	none := bob
	none.Role = ""
	f.ms.users[bob.ID] = none

	s := f.resolve(t)
	if s.ActAs != nil || s.Principal().Kind != authz.KindBrowserSession || s.Principal().UserID != alice.ID {
		t.Fatalf("session after the target lost its grants: ActAs = %+v, principal = %+v", s.ActAs, s.Principal())
	}
	f.resolve(t)
	f.requireOneEndAudit(t)
}

func TestActAsEndsWhenTheActorIsNoLongerAnAdmin(t *testing.T) {
	f := newActAsFixture(t)
	if _, err := f.m.StartActAs(context.Background(), f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	demoted := alice
	demoted.Role = "user"
	f.ms.users[alice.ID] = demoted

	s := f.resolve(t)
	if s.ActAs != nil || s.Principal().Kind != authz.KindBrowserSession || s.Principal().UserID != alice.ID {
		t.Fatalf("session after the actor was demoted: ActAs = %+v, principal = %+v", s.ActAs, s.Principal())
	}
	f.resolve(t)
	f.requireOneEndAudit(t)
}

func (f *actAsFixture) requireOneEndAudit(t *testing.T) {
	t.Helper()
	if len(f.ms.rows) != 0 {
		t.Error("the act-as row was not removed")
	}
	if len(f.ms.audits) != 1 {
		t.Fatalf("audit rows = %+v, want exactly one act_as.end", f.ms.audits)
	}
	if a := f.ms.audits[0]; a.Actor != alice.ID || a.ActingAs != bob.ID || a.Action != AuditActionActAsEnd || a.Target != bob.ID || a.Outcome != "success" || a.ID == "" {
		t.Errorf("end audit = %+v, want act_as.end with actor alice, acting_as bob", a)
	}
}

// Another request ends the expired period and starts a new one between the
// read and the clean-up of a stale request: the new period must survive and
// no expiry is recorded for it.
func TestStaleExpiryCleanupDoesNotEndANewPeriod(t *testing.T) {
	f := newActAsFixture(t)
	ctx := context.Background()
	if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.clk.Add(time.Minute)
	stale := f.ms.rows[f.sess.ID] // what the stale request read

	if _, err := f.m.StopActAs(ctx, f.sess); err != nil { // another request clears the row
		t.Fatal(err)
	}
	if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, time.Hour); err != nil { // and starts a new period
		t.Fatal(err)
	}
	fresh := f.ms.rows[f.sess.ID]

	// The stale request now runs its clean-up for the period it read.
	ended, err := f.ms.EndActAsIf(ctx, stale, store.AuditRow{ID: "x", Action: AuditActionActAsExpire})
	if err != nil || ended {
		t.Fatalf("stale EndActAsIf = %v, %v; want false, nil", ended, err)
	}
	if f.ms.rows[f.sess.ID] != fresh || len(f.ms.audits) != 0 {
		t.Fatalf("the new period was touched: rows = %+v, audits = %+v", f.ms.rows, f.ms.audits)
	}
	if s := f.resolve(t); s.ActAs == nil || s.Principal().Kind != authz.KindActAs {
		t.Fatalf("the new period is not served: %+v", s.ActAs)
	}
}

func TestResolveFailsClosedWhenTheActAsStateCannotBeRead(t *testing.T) {
	f := newActAsFixture(t)
	f.ms.failGet = errors.New("disk on fire")
	_, err := f.m.Resolve(context.Background(), f.admin.Token)
	if err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want the store error (not ErrNoSession, not a plain session)", err)
	}
}

func TestActAsRowOfAnotherUserIsIgnored(t *testing.T) {
	f := newActAsFixture(t)
	f.ms.rows[f.sess.ID] = store.ActAsRow{
		SessionID: f.sess.ID, ActorID: bob.ID, TargetID: alice.ID,
		StartedAt: store.UnixMS(f.clk.Now()), ExpiresAt: store.UnixMS(f.clk.Now().Add(time.Hour)),
	}
	if s := f.resolve(t); s.ActAs != nil {
		t.Errorf("a row whose actor is not the session's user was honoured: %+v", s.ActAs)
	}
}

func TestStopActAs(t *testing.T) {
	f := newActAsFixture(t)
	ctx := context.Background()
	if ended, err := f.m.StopActAs(ctx, f.sess); err != nil || ended {
		t.Fatalf("StopActAs while not acting = %v, %v; want false, nil", ended, err)
	}
	if _, err := f.m.StartActAs(ctx, f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	if ended, err := f.m.StopActAs(ctx, f.resolve(t)); err != nil || !ended {
		t.Fatalf("StopActAs = %v, %v; want true, nil", ended, err)
	}
	if s := f.resolve(t); s.ActAs != nil || s.Principal().Kind != authz.KindBrowserSession {
		t.Errorf("session still acts after the stop: %+v", s.ActAs)
	}
	// The admin can start again after leaving.
	if _, err := f.m.StartActAs(ctx, f.resolve(t), bob.ID, 0); err != nil {
		t.Errorf("start after stop: %v", err)
	}
	f.ms.failEnd = errors.New("disk")
	if _, err := f.m.StopActAs(ctx, f.sess); err == nil {
		t.Error("a failed stop was reported as success")
	}
}

func TestActAsMinutesLeft(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	a := ActAs{ExpiresAt: now.Add(90*time.Minute + time.Second)}
	for _, tc := range []struct {
		at   time.Duration
		want int
	}{
		{0, 91},
		{time.Second, 90},
		{90 * time.Minute, 1},
		{90*time.Minute + time.Second - time.Millisecond, 1},
		{90*time.Minute + time.Second, 0},
		{3 * time.Hour, 0},
	} {
		if got := a.MinutesLeft(now.Add(tc.at)); got != tc.want {
			t.Errorf("MinutesLeft at +%v = %d, want %d", tc.at, got, tc.want)
		}
	}
	if got := a.Remaining(now.Add(3 * time.Hour)); got != 0 {
		t.Errorf("Remaining after the expiry = %v, want 0", got)
	}
}

func TestPasswordChangeIsRefusedWhileActing(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	err := CheckPasswordChange(authz.NewActAsPrincipal(alice.ID, bob.ID, exp))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeForbidden {
		t.Fatalf("act-as: err = %v, want forbidden", err)
	}
	for name, p := range map[string]authz.Principal{
		"browser session": authz.NewSessionPrincipal(bob.ID),
		"admin session":   authz.NewSessionPrincipal(alice.ID),
	} {
		if err := CheckPasswordChange(p); err != nil {
			t.Errorf("%s: %v, want nil", name, err)
		}
	}
	// The principal of a resolved act-as session is refused end to end.
	f := newActAsFixture(t)
	if _, err := f.m.StartActAs(context.Background(), f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := CheckPasswordChange(f.resolve(t).Principal()); err == nil {
		t.Error("the password of the target can be changed from an act-as session")
	}
	// After the stop the admin changes their own password again.
	if _, err := f.m.StopActAs(context.Background(), f.sess); err != nil {
		t.Fatal(err)
	}
	if err := CheckPasswordChange(f.resolve(t).Principal()); err != nil {
		t.Errorf("after the stop: %v", err)
	}
}

func TestTrustIsEvaluatedForTheActor(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	if got := TrustUserID(authz.NewActAsPrincipal(alice.ID, bob.ID, exp)); got != alice.ID {
		t.Errorf("act-as trust user = %q, want the actor %q", got, alice.ID)
	}
	if got := TrustUserID(authz.NewSessionPrincipal(bob.ID)); got != bob.ID {
		t.Errorf("session trust user = %q, want %q", got, bob.ID)
	}
	if got := TrustUserID(authz.NewWorkerPrincipal("w-1")); got != "" {
		t.Errorf("worker identity trust user = %q, want none", got)
	}
	f := newActAsFixture(t)
	if _, err := f.m.StartActAs(context.Background(), f.sess, bob.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := TrustUserID(f.resolve(t).Principal()); got != alice.ID {
		t.Errorf("trust user of a resolved act-as session = %q, want alice", got)
	}
}
