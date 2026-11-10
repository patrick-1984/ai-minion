package local

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

var (
	adminActor = Actor{ID: "admin-1"}
	ctxBG      = context.Background()
)

// fakeAdminStore records the parameters of each call and answers from the
// configured errors, so that each Admin operation is tested without SQL.
type fakeAdminStore struct {
	mu        sync.Mutex
	users     map[string]store.UserRow
	err       error // returned by every mutating call when set
	created   []store.CreateUserParams
	resets    []store.ResetPasswordParams
	changes   []store.ChangePasswordParams
	roles     []store.SetLocalRoleParams
	deactives []store.DeactivateUserParams
	changed   bool
}

func newFakeAdminStore() *fakeAdminStore {
	return &fakeAdminStore{users: map[string]store.UserRow{}, changed: true}
}

func (f *fakeAdminStore) ListUsers(context.Context) ([]store.UserAdminRow, error) {
	var out []store.UserAdminRow
	for _, u := range f.users {
		out = append(out, store.UserAdminRow{UserRow: u})
	}
	return out, f.err
}

func (f *fakeAdminStore) UserByID(_ context.Context, id string) (store.UserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return store.UserRow{}, store.ErrNotFound
}

func (f *fakeAdminStore) CreateUser(_ context.Context, p store.CreateUserParams) error {
	f.created = append(f.created, p)
	return f.err
}

func (f *fakeAdminStore) ResetPassword(_ context.Context, p store.ResetPasswordParams) error {
	f.resets = append(f.resets, p)
	return f.err
}

func (f *fakeAdminStore) ChangePassword(_ context.Context, p store.ChangePasswordParams) (int64, error) {
	f.changes = append(f.changes, p)
	return p.ExpectedVersion + 1, f.err
}

func (f *fakeAdminStore) SetLocalRole(_ context.Context, p store.SetLocalRoleParams) (string, error) {
	f.roles = append(f.roles, p)
	return p.Role, f.err
}

func (f *fakeAdminStore) DeactivateUser(_ context.Context, p store.DeactivateUserParams) (bool, error) {
	f.deactives = append(f.deactives, p)
	return f.changed, f.err
}

func newAdmin(st AdminStore) *Admin {
	return &Admin{Store: st, Hasher: fastHasher(), Now: func() time.Time { return time.UnixMilli(1_700_000_000_000) }}
}

func TestCreateUserGeneratesATemporaryPassword(t *testing.T) {
	st := newFakeAdminStore()
	a := newAdmin(st)
	got, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann", Email: "ann@example.com", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	pw := got.TemporaryPassword.Reveal()
	if len(pw) != 24 {
		t.Errorf("temporary password has %d characters, want 24", len(pw))
	}
	if strings.Contains(fmt.Sprint(got.TemporaryPassword, got), pw) {
		t.Error("the temporary password is not redacted in formatted output")
	}
	if len(st.created) != 1 {
		t.Fatalf("store calls = %d", len(st.created))
	}
	p := st.created[0]
	u := p.User
	if p.Role != "admin" || u.Username != "ann" || u.Email != "ann@example.com" || !u.MustChangePassword || !u.Active || u.CredentialVersion != 1 {
		t.Errorf("params = %+v", p)
	}
	if u.PasswordHash == pw || strings.Contains(u.PasswordHash, pw) {
		t.Error("the store received the password in clear")
	}
	if ok, _, err := a.Hasher.Verify(ctxBG, pw, u.PasswordHash); err != nil || !ok {
		t.Errorf("the returned password does not match the stored hash: %v %v", ok, err)
	}
	if err := store.ParseID(u.ID); err != nil {
		t.Errorf("user id: %v", err)
	}
	if got.User.ID != u.ID || got.User.Role != "admin" {
		t.Errorf("returned user = %+v", got.User)
	}
	au := p.Audit
	if au == nil || au.Action != AuditActionUserCreate || au.Actor != "admin-1" || au.Target != u.ID || au.Outcome != OutcomeSuccess {
		t.Errorf("audit = %+v", au)
	}
	if strings.Contains(fmt.Sprintf("%+v", *au), pw) {
		t.Error("the audit row contains the password")
	}

	// Each account gets its own password, and the default role is user.
	other, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if other.TemporaryPassword.Reveal() == pw || st.created[1].Role != "user" {
		t.Errorf("second account: same password or wrong default role (%q)", st.created[1].Role)
	}
}

func TestCreateUserRefusals(t *testing.T) {
	st := newFakeAdminStore()
	a := newAdmin(st)
	if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: " "}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("blank user name = %v", err)
	}
	if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann", Email: "nope"}); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("bad e-mail = %v", err)
	}
	if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann", Role: "root"}); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("bad role = %v", err)
	}
	if len(st.created) != 0 {
		t.Error("a refused request reached the store")
	}
	for storeErr, want := range map[error]error{
		store.ErrUsernameTaken: ErrUsernameTaken,
		store.ErrEmailTaken:    ErrEmailTaken,
	} {
		st.err = storeErr
		if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann"}); !errors.Is(err, want) {
			t.Errorf("store %v -> %v, want %v", storeErr, err, want)
		}
	}
	st.err = errors.New("disk full")
	if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann"}); err == nil || errors.Is(err, ErrUsernameTaken) {
		t.Errorf("other store errors pass through, got %v", err)
	}
}

func TestResetPasswordReturnsANewTemporaryPassword(t *testing.T) {
	st := newFakeAdminStore()
	a := newAdmin(st)
	pw, err := a.ResetPassword(ctxBG, adminActor, "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.resets) != 1 {
		t.Fatalf("store calls = %d", len(st.resets))
	}
	p := st.resets[0]
	if p.UserID != "u-1" || p.NowMS != 1_700_000_000_000 {
		t.Errorf("params = %+v", p)
	}
	if ok, _, err := a.Hasher.Verify(ctxBG, pw.Reveal(), p.PasswordHash); err != nil || !ok {
		t.Errorf("the returned password does not match: %v %v", ok, err)
	}
	if p.Audit == nil || p.Audit.Action != AuditActionUserResetPassword || p.Audit.Target != "u-1" || p.Audit.Actor != "admin-1" {
		t.Errorf("audit = %+v", p.Audit)
	}
	for storeErr, want := range map[error]error{
		store.ErrNotFound:        ErrUserNotFound,
		store.ErrUserInactive:    ErrUserInactive,
		store.ErrNoLocalPassword: ErrNoLocalPassword,
	} {
		st.err = storeErr
		if _, err := a.ResetPassword(ctxBG, adminActor, "u-1"); !errors.Is(err, want) {
			t.Errorf("store %v -> %v, want %v", storeErr, err, want)
		}
	}
}

func TestAnActingAdminCannotResetOrChangeAPassword(t *testing.T) {
	st := newFakeAdminStore()
	st.users["u-1"] = store.UserRow{ID: "u-1", Active: true, PasswordHash: "x", CredentialVersion: 1}
	a := newAdmin(st)
	actAs := Actor{ID: "admin-1", ActingAs: "u-1"}
	if _, err := a.ResetPassword(ctxBG, actAs, "u-1"); !errors.Is(err, ErrActAsPasswordChange) {
		t.Errorf("reset while acting as = %v", err)
	}
	err := a.ChangePassword(ctxBG, actAs, PasswordChange{UserID: "u-1", OldPassword: "a", NewPassword: "b"})
	if !errors.Is(err, ErrActAsPasswordChange) {
		t.Errorf("change while acting as = %v", err)
	}
	if len(st.resets)+len(st.changes) != 0 {
		t.Error("a refused act-as call reached the store")
	}
}

func TestSetRole(t *testing.T) {
	st := newFakeAdminStore()
	a := newAdmin(st)
	eff, err := a.SetRole(ctxBG, adminActor, "u-1", "admin")
	if err != nil || eff != "admin" || len(st.roles) != 1 {
		t.Fatalf("SetRole = %q, %v", eff, err)
	}
	if p := st.roles[0]; p.UserID != "u-1" || p.Role != "admin" || p.Audit == nil || p.Audit.Action != AuditActionUserSetRole {
		t.Errorf("params = %+v / %+v", p, p.Audit)
	}
	if _, err := a.SetRole(ctxBG, adminActor, "u-1", "owner"); !errors.Is(err, ErrInvalidRole) || len(st.roles) != 1 {
		t.Errorf("bad role = %v", err)
	}
	st.err = store.ErrLastAdmin
	if _, err := a.SetRole(ctxBG, adminActor, "u-1", "user"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("last admin = %v", err)
	}
}

func TestDeactivatePassesTheKeyRevocationIntoTheTransaction(t *testing.T) {
	st := newFakeAdminStore()
	a := newAdmin(st)
	changed, err := a.Deactivate(ctxBG, adminActor, "u-1")
	if err != nil || !changed {
		t.Fatalf("Deactivate = %v, %v", changed, err)
	}
	if p := st.deactives[0]; len(p.Hooks) != 0 || p.UserID != "u-1" || p.Audit == nil || p.Audit.Action != AuditActionUserDeactivate {
		t.Errorf("params without a key store = %+v", p)
	}

	var revoked []string
	a.RevokeKeys = func(_ context.Context, _ *sql.Tx, id string) error { revoked = append(revoked, id); return nil }
	if _, err := a.Deactivate(ctxBG, adminActor, "u-2"); err != nil {
		t.Fatal(err)
	}
	hooks := st.deactives[1].Hooks
	if len(hooks) != 1 {
		t.Fatalf("hooks = %d", len(hooks))
	}
	_ = hooks[0](ctxBG, nil, "u-2")
	if len(revoked) != 1 || revoked[0] != "u-2" {
		t.Errorf("revoked = %v", revoked)
	}

	st.err, st.changed = store.ErrLastAdmin, false
	if changed, err := a.Deactivate(ctxBG, adminActor, "u-1"); !errors.Is(err, ErrLastAdmin) || changed {
		t.Errorf("last admin = %v, %v", changed, err)
	}
	st.err = store.ErrNotFound
	if _, err := a.Deactivate(ctxBG, adminActor, "u-9"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown = %v", err)
	}
}

func TestListUsers(t *testing.T) {
	st := newFakeAdminStore()
	st.users["u-1"] = store.UserRow{ID: "u-1", Username: "ann"}
	rows, err := newAdmin(st).ListUsers(ctxBG)
	if err != nil || len(rows) != 1 || rows[0].Username != "ann" {
		t.Errorf("ListUsers = %+v, %v", rows, err)
	}
}

// pwFixture is an Admin on a fake store with one account whose password is
// "old-password".
type pwFixture struct {
	st *fakeAdminStore
	a  *Admin
}

func newPWFixture(t *testing.T) *pwFixture {
	t.Helper()
	a := newAdmin(nil)
	hash, err := a.Hasher.Hash(ctxBG, "old-password")
	if err != nil {
		t.Fatal(err)
	}
	st := newFakeAdminStore()
	st.users["u-1"] = store.UserRow{ID: "u-1", Username: "ann", Active: true, PasswordHash: hash, CredentialVersion: 4, MustChangePassword: true}
	a.Store = st
	return &pwFixture{st: st, a: a}
}

func TestChangePasswordVerifiesTheOldPasswordAndComparesTheVersion(t *testing.T) {
	f := newPWFixture(t)
	err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, PasswordChange{
		UserID: "u-1", SessionID: "sess-1", OldPassword: "old-password", NewPassword: "a-brand-new-password"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.st.changes) != 1 {
		t.Fatalf("store calls = %d", len(f.st.changes))
	}
	p := f.st.changes[0]
	if p.UserID != "u-1" || p.ExpectedVersion != 4 || p.KeepSessionID != "sess-1" {
		t.Errorf("params = %+v (the version read before verification is compared; the caller's session is kept)", p)
	}
	if ok, _, _ := f.a.Hasher.Verify(ctxBG, "a-brand-new-password", p.PasswordHash); !ok {
		t.Error("the stored hash is not of the new password")
	}
	if p.Audit == nil || p.Audit.Action != AuditActionUserPasswordChange || p.Audit.Actor != "u-1" || p.Audit.Target != "u-1" {
		t.Errorf("audit = %+v", p.Audit)
	}
}

func TestChangePasswordRefusals(t *testing.T) {
	f := newPWFixture(t)
	req := PasswordChange{UserID: "u-1", OldPassword: "old-password", NewPassword: "a-brand-new-password"}
	wrong := req
	wrong.OldPassword = "not-the-password"
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, wrong); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("wrong old password = %v", err)
	}
	unknown := req
	unknown.UserID = "nobody"
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "nobody"}, unknown); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user = %v", err)
	}
	u := f.st.users["u-1"]
	u.Active = false
	f.st.users["u-1"] = u
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated = %v", err)
	}
	u.Active, u.PasswordHash = true, ""
	f.st.users["u-1"] = u
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req); !errors.Is(err, ErrNoLocalPassword) {
		t.Errorf("no local password = %v", err)
	}
	if len(f.st.changes) != 0 {
		t.Error("a refused change reached the store")
	}
}

func TestChangePasswordMapsALostRaceToAConcurrentChange(t *testing.T) {
	f := newPWFixture(t)
	f.st.err = store.ErrVersionConflict
	err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, PasswordChange{UserID: "u-1", OldPassword: "old-password", NewPassword: "a-brand-new-password"})
	if !errors.Is(err, ErrConcurrentChange) {
		t.Errorf("err = %v", err)
	}
}

// fakeBackoff records the account back-off calls of a password change.
type fakeBackoff struct {
	wait   time.Duration
	keys   []string
	fails  int
	resets int
}

func (b *fakeBackoff) Backoff(key string) time.Duration { b.keys = append(b.keys, key); return b.wait }
func (b *fakeBackoff) BackoffFail(string) time.Duration { b.fails++; return 0 }
func (b *fakeBackoff) BackoffReset(string)              { b.resets++ }

func TestChangePasswordBacksOffWrongOldPasswords(t *testing.T) {
	f := newPWFixture(t)
	bo := &fakeBackoff{}
	f.a.Backoff = bo
	req := PasswordChange{UserID: "u-1", OldPassword: "wrong-password", NewPassword: "a-brand-new-password"}
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req); !errors.Is(err, ErrWrongPassword) || bo.fails != 1 || bo.resets != 0 {
		t.Errorf("wrong password: err=%v fails=%d resets=%d", err, bo.fails, bo.resets)
	}
	// The back-off is checked before the user is read and again under the
	// account's lock; every call names the same per-account key.
	if len(bo.keys) == 0 {
		t.Error("the back-off was never consulted")
	}
	for _, k := range bo.keys {
		if k != "pwchange:u-1" {
			t.Errorf("back-off key = %v", bo.keys)
			break
		}
	}
	req.OldPassword = "old-password"
	if err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req); err != nil || bo.resets != 1 {
		t.Errorf("success: err=%v resets=%d", err, bo.resets)
	}

	// While the account is in back-off the password is not even checked.
	bo.wait = 30 * time.Second
	before := len(f.st.changes)
	err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req)
	var limited *RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter != 30*time.Second || len(f.st.changes) != before {
		t.Errorf("in back-off: err=%v", err)
	}
}

func TestChangePasswordWithTheRealLimiter(t *testing.T) {
	f := newPWFixture(t)
	lim, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.a.Backoff = lim
	req := PasswordChange{UserID: "u-1", OldPassword: "wrong-password", NewPassword: "a-brand-new-password"}
	var limited *RateLimitedError
	for i := 0; i < 50 && !errors.As(f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req), &limited); i++ {
	}
	if limited == nil {
		t.Fatal("repeated wrong old passwords were never rate limited")
	}
}

// Concurrent guesses must not all pass the back-off check before the first
// failure sets it: the account's changes run one at a time, so as many wrong
// guesses are verified as in a sequential run, and the rest are refused.
func TestConcurrentWrongOldPasswordsAreThrottled(t *testing.T) {
	req := PasswordChange{UserID: "u-1", OldPassword: "wrong-password", NewPassword: "a-brand-new-password"}
	newLimited := func() *pwFixture {
		f := newPWFixture(t)
		lim, err := ratelimit.New(ratelimit.Config{})
		if err != nil {
			t.Fatal(err)
		}
		f.a.Backoff = lim
		return f
	}

	seq := newLimited()
	allowed := 0
	for i := 0; i < 50; i++ {
		err := seq.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req)
		var limited *RateLimitedError
		if errors.As(err, &limited) {
			break
		}
		if !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("sequential guess = %v", err)
		}
		allowed++
	}
	if allowed == 0 || allowed >= 50 {
		t.Fatalf("sequential run verified %d guesses before the back-off", allowed)
	}

	f := newLimited()
	const n = 32
	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		verified, denied int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.a.ChangePassword(ctxBG, Actor{ID: "u-1"}, req)
			var limited *RateLimitedError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.As(err, &limited):
				denied++
			case errors.Is(err, ErrWrongPassword):
				verified++
			default:
				t.Errorf("guess = %v", err)
			}
		}()
	}
	wg.Wait()
	if verified != allowed || denied != n-allowed {
		t.Errorf("concurrent guesses: %d verified, %d refused; want %d and %d", verified, denied, allowed, n-allowed)
	}
}

func TestPasswordChangeLockHonoursTheContext(t *testing.T) {
	var k keyedLocks
	unlock, err := k.lock(ctxBG, "a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctxBG)
	cancel()
	if _, err := k.lock(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("waiting lock = %v", err)
	}
	unlock()
	if len(k.m) != 0 {
		t.Errorf("lock entries left: %d", len(k.m))
	}
}

// --- against the real store ------------------------------------------------

type realRig struct {
	st    *store.Store
	a     *Admin
	mgr   *session.Manager
	auth  *Authenticator
	audit *memAudit
}

// memAudit is the audit sink of the login authenticator.
type memAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (m *memAudit) Record(_ context.Context, ev audit.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

func newRealRig(t *testing.T) *realRig {
	t.Helper()
	st := openStore(t)
	h := fastHasher()
	lim, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ma := &memAudit{}
	return &realRig{
		st: st, a: &Admin{Store: st, Hasher: h}, mgr: session.NewManager(st),
		auth:  &Authenticator{Store: st, Hasher: h, Limiter: lim, Audit: ma},
		audit: ma,
	}
}

func (r *realRig) login(t *testing.T, ident, password string) (store.UserRow, error) {
	t.Helper()
	return r.auth.Authenticate(ctxBG, Credentials{Identifier: ident, Password: password, IP: netip.MustParseAddr("192.0.2.1")})
}

// seedAdmin creates the first admin through the admin service itself.
func (r *realRig) create(t *testing.T, name, role string) (store.UserRow, string) {
	t.Helper()
	c, err := r.a.CreateUser(ctxBG, Actor{ID: "system"}, CreateRequest{Username: name, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return c.User, c.TemporaryPassword.Reveal()
}

func (r *realRig) sessionFor(t *testing.T, u store.UserRow) session.Issued {
	t.Helper()
	cur, err := r.st.UserByID(ctxBG, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	is, err := r.mgr.Create(ctxBG, cur, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func (r *realRig) valid(is session.Issued) bool {
	_, err := r.mgr.Resolve(ctxBG, is.Token)
	return err == nil
}

func TestCreatedAccountSignsInWithItsTemporaryPasswordAndMustChangeIt(t *testing.T) {
	r := newRealRig(t)
	u, pw := r.create(t, "ann", "user")
	got, err := r.login(t, "ann", pw)
	if err != nil {
		t.Fatalf("login with the temporary password: %v", err)
	}
	if !got.MustChangePassword || got.Role != "user" || got.ID != u.ID {
		t.Errorf("signed-in user = %+v", got)
	}
	adm, apw := r.create(t, "boss", "admin")
	if got, err := r.login(t, "boss", apw); err != nil || got.Role != "admin" || got.ID != adm.ID {
		t.Errorf("admin = %+v, %v", got, err)
	}
	if _, err := r.login(t, "ann", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password = %v", err)
	}
}

func TestChangePasswordRevokesOtherSessionsOnly(t *testing.T) {
	r := newRealRig(t)
	u, pw := r.create(t, "ann", "user")
	here, there, third := r.sessionFor(t, u), r.sessionFor(t, u), r.sessionFor(t, u)
	bob, _ := r.create(t, "bob", "user")
	bobSession := r.sessionFor(t, bob)

	err := r.a.ChangePassword(ctxBG, Actor{ID: u.ID}, PasswordChange{
		UserID: u.ID, SessionID: here.ID, OldPassword: logx.Secret(pw), NewPassword: "my-own-new-password"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.valid(here) {
		t.Error("the session that changed the password was ended")
	}
	if r.valid(there) || r.valid(third) {
		t.Error("another session of the user survived the change")
	}
	if !r.valid(bobSession) {
		t.Error("a change of ann's password ended bob's session")
	}
	if _, err := r.login(t, "ann", pw); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the temporary password still works: %v", err)
	}
	got, err := r.login(t, "ann", "my-own-new-password")
	if err != nil || got.MustChangePassword || got.CredentialVersion != 2 {
		t.Errorf("login with the new password = %+v, %v", got, err)
	}
}

func TestResetPasswordRevokesAllSessionsAndFlagsMustChange(t *testing.T) {
	r := newRealRig(t)
	u, pw := r.create(t, "ann", "user")
	if err := r.a.ChangePassword(ctxBG, Actor{ID: u.ID}, PasswordChange{UserID: u.ID, OldPassword: logx.Secret(pw), NewPassword: "my-own-new-password"}); err != nil {
		t.Fatal(err)
	}
	s1, s2 := r.sessionFor(t, u), r.sessionFor(t, u)
	temp, err := r.a.ResetPassword(ctxBG, adminActor, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.valid(s1) || r.valid(s2) {
		t.Error("a session survived the reset")
	}
	if _, err := r.login(t, "ann", "my-own-new-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the old password still works: %v", err)
	}
	got, err := r.login(t, "ann", temp.Reveal())
	if err != nil || !got.MustChangePassword {
		t.Errorf("login with the reset password = %+v, %v", got, err)
	}
}

func TestDeactivatedAccountCannotSignIn(t *testing.T) {
	r := newRealRig(t)
	r.create(t, "boss", "admin")
	u, pw := r.create(t, "ann", "user")
	s := r.sessionFor(t, u)
	changed, err := r.a.Deactivate(ctxBG, adminActor, u.ID)
	if err != nil || !changed {
		t.Fatalf("Deactivate = %v, %v", changed, err)
	}
	if r.valid(s) {
		t.Error("a session survived the deactivation")
	}
	if _, err := r.login(t, "ann", pw); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("deactivated account signed in: %v", err)
	}
	if err := r.a.ChangePassword(ctxBG, Actor{ID: u.ID}, PasswordChange{UserID: u.ID, OldPassword: logx.Secret(pw), NewPassword: "another-password-1"}); !errors.Is(err, ErrUserInactive) {
		t.Errorf("password change of a deactivated account = %v", err)
	}
	if _, err := r.a.ResetPassword(ctxBG, adminActor, u.ID); !errors.Is(err, ErrUserInactive) {
		t.Errorf("reset of a deactivated account = %v", err)
	}
	if _, err := r.a.SetRole(ctxBG, adminActor, u.ID, "admin"); !errors.Is(err, ErrUserInactive) {
		t.Errorf("role change of a deactivated account = %v", err)
	}
	rows, err := r.a.ListUsers(ctxBG)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == u.ID && row.Active {
			t.Error("the list shows the account active")
		}
	}
}

func TestDeactivateRevokesKeysInTheSameTransaction(t *testing.T) {
	r := newRealRig(t)
	r.create(t, "boss", "admin")
	u, _ := r.create(t, "ann", "user")
	if _, err := r.st.DB().Exec(`CREATE TABLE fake_keys (owner TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.DB().Exec(`INSERT INTO fake_keys (owner) VALUES (?), (?)`, u.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	r.a.RevokeKeys = func(ctx context.Context, tx *sql.Tx, id string) error {
		_, err := tx.ExecContext(ctx, `UPDATE fake_keys SET revoked = 1 WHERE owner = ?`, id)
		return err
	}
	if _, err := r.a.Deactivate(ctxBG, adminActor, u.ID); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := r.st.DB().QueryRow(`SELECT COUNT(*) FROM fake_keys WHERE revoked = 0`).Scan(&live); err != nil || live != 0 {
		t.Errorf("%d keys left after deactivation, %v", live, err)
	}
}

func TestLastAdminCannotBeDemotedOrDeactivated(t *testing.T) {
	r := newRealRig(t)
	boss, _ := r.create(t, "boss", "admin")
	if _, err := r.a.SetRole(ctxBG, adminActor, boss.ID, "user"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote the last admin = %v", err)
	}
	if _, err := r.a.Deactivate(ctxBG, adminActor, boss.ID); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deactivate the last admin = %v", err)
	}
	ann, _ := r.create(t, "ann", "user")
	if eff, err := r.a.SetRole(ctxBG, adminActor, ann.ID, "admin"); err != nil || eff != "admin" {
		t.Fatalf("promote = %q, %v", eff, err)
	}
	if _, err := r.a.Deactivate(ctxBG, adminActor, boss.ID); err != nil {
		t.Errorf("deactivate with a second admin = %v", err)
	}
}

func TestAccountOperationsWriteAuditRowsWithTheActorAndTheTarget(t *testing.T) {
	r := newRealRig(t)
	admin := Actor{ID: "admin-1", ActingAs: ""}
	boss, _ := r.create(t, "boss", "admin")
	ann, pw := r.create(t, "ann", "user")
	if err := r.a.ChangePassword(ctxBG, Actor{ID: ann.ID}, PasswordChange{UserID: ann.ID, OldPassword: logx.Secret(pw), NewPassword: "my-own-new-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.ResetPassword(ctxBG, admin, ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.SetRole(ctxBG, admin, ann.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Deactivate(ctxBG, admin, ann.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := r.st.ListAudit(ctxBG, store.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ action, actor, target string }
	got := map[key]bool{}
	for _, row := range rows {
		got[key{row.Action, row.Actor, row.Target}] = true
		if row.Outcome != OutcomeSuccess {
			t.Errorf("row %+v", row)
		}
	}
	for _, want := range []key{
		{AuditActionUserCreate, "system", boss.ID},
		{AuditActionUserCreate, "system", ann.ID},
		{AuditActionUserPasswordChange, ann.ID, ann.ID},
		{AuditActionUserResetPassword, "admin-1", ann.ID},
		{AuditActionUserSetRole, "admin-1", ann.ID},
		{AuditActionUserDeactivate, "admin-1", ann.ID},
	} {
		if !got[want] {
			t.Errorf("no audit row %+v in %+v", want, rows)
		}
	}
}

// slowReads makes every UserByID wait until n callers have read, so that
// concurrent password changes all see the same credential version.
type slowReads struct {
	*store.Store
	barrier *sync.WaitGroup
}

func (s slowReads) UserByID(ctx context.Context, id string) (store.UserRow, error) {
	u, err := s.Store.UserByID(ctx, id)
	s.barrier.Done()
	s.barrier.Wait()
	return u, err
}

// Two password changes that read the same credential version race: one wins
// and the other fails as a concurrent change; the winner's password is the one
// that works afterwards.
func TestConcurrentPasswordChangesHaveOneWinner(t *testing.T) {
	r := newRealRig(t)
	u, pw := r.create(t, "ann", "user")
	var barrier sync.WaitGroup
	barrier.Add(2)
	a := &Admin{Store: slowReads{Store: r.st, barrier: &barrier}, Hasher: fastHasher()}

	passwords := []string{"first-new-password", "second-new-password"}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range passwords {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = a.ChangePassword(ctxBG, Actor{ID: u.ID}, PasswordChange{
				UserID: u.ID, OldPassword: logx.Secret(pw), NewPassword: logx.Secret(passwords[i])})
		}()
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("both changes succeeded: %v", errs)
			}
			winner = i
		case !errors.Is(err, ErrConcurrentChange):
			t.Errorf("loser %d got %v, want a concurrent-change error", i, err)
		}
	}
	if winner < 0 {
		t.Fatalf("neither change succeeded: %v", errs)
	}
	if _, err := r.login(t, "ann", passwords[winner]); err != nil {
		t.Errorf("the winner's password does not work: %v", err)
	}
	if _, err := r.login(t, "ann", passwords[1-winner]); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("the loser's password works: %v", err)
	}
	if cur, _ := r.st.UserByID(ctxBG, u.ID); cur.CredentialVersion != 2 {
		t.Errorf("credential version = %d, want one bump", cur.CredentialVersion)
	}
}

func TestGeneratedPasswordsAreNeverLoggedByTheAdmin(t *testing.T) {
	// The Admin has no logger: nothing it does can log a password. Using a
	// failing random source proves that no partial password is returned.
	a := newAdmin(newFakeAdminStore())
	a.Random = io.LimitReader(strings.NewReader("short"), 5)
	if _, err := a.CreateUser(ctxBG, adminActor, CreateRequest{Username: "ann"}); err == nil {
		t.Error("CreateUser succeeded without enough randomness")
	}
	if _, err := a.ResetPassword(ctxBG, adminActor, "u-1"); err == nil {
		t.Error("ResetPassword succeeded without enough randomness")
	}
}
