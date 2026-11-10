package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// newAccount creates an active local account and returns its id.
func newAccount(t *testing.T, s *Store, name, role string) string {
	t.Helper()
	id := NewID()
	err := s.CreateUser(context.Background(), CreateUserParams{
		User: UserRow{ID: id, Username: name, PasswordHash: "hash-" + name, MustChangePassword: true, CredentialVersion: 1, CreatedAt: 1000, UpdatedAt: 1000},
		Role: role,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// addSession stores a session of the user at the user's current version.
func addSession(t *testing.T, s *Store, userID string) SessionRow {
	t.Helper()
	u, err := s.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	row := SessionRow{ID: NewID(), TokenHash: NewID(), UserID: userID, CredentialVersion: u.CredentialVersion, CreatedAt: 1, ExpiresAt: 1 << 50}
	if err := s.CreateSession(context.Background(), row, 2); err != nil {
		t.Fatal(err)
	}
	return row
}

func sessionCount(t *testing.T, s *Store, userID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditActions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.ListAudit(context.Background(), AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Action)
	}
	return out
}

func TestCreateUserWritesTheAccountAndItsLocalRole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	admin := newAccount(t, s, "Root", "admin")
	user := newAccount(t, s, "ann", "user")

	a, err := s.UserByID(ctx, admin)
	if err != nil || a.Role != "admin" || !a.Active || !a.MustChangePassword || a.CredentialVersion != 1 {
		t.Fatalf("admin = %+v, %v", a, err)
	}
	if u, _ := s.UserByID(ctx, user); u.Role != "user" {
		t.Errorf("user role = %q", u.Role)
	}
	var source string
	if err := s.db.QueryRow(`SELECT source FROM user_roles WHERE user_id = ? AND role = 'admin'`, admin).Scan(&source); err != nil || source != "local" {
		t.Errorf("admin grant source = %q, %v", source, err)
	}

	err = s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "ROOT"}, Role: "user"})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("duplicate user name (case-insensitive) = %v", err)
	}
	if err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "bob", Email: "b@example.com"}, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	err = s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "bob2", Email: "B@EXAMPLE.COM"}, Role: "user"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Errorf("duplicate e-mail = %v", err)
	}
	if err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "x"}, Role: "owner"}); err == nil {
		t.Error("an unknown role was accepted")
	}
}

// A sign-in identifier is a user name or an e-mail address, and a user name
// wins; so no user name may equal another account's e-mail address, and no
// e-mail may equal another account's user name (both case-insensitive).
func TestCreateUserRefusesAUserNameThatIsAnotherAccountsEmail(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	bob := NewID()
	if err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: bob, Username: "bob", Email: "bob@example.com"}, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "BOB@example.com"}, Role: "user"})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("user name equal to an e-mail = %v", err)
	}
	if u, err := s.UserByIdentifier(ctx, "bob@example.com"); err != nil || u.ID != bob {
		t.Errorf("sign-in identifier resolves to %+v, %v", u, err)
	}

	err = s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: NewID(), Username: "carol", Email: "BOB"}, Role: "user"})
	if !errors.Is(err, ErrEmailTaken) {
		t.Errorf("e-mail equal to a user name = %v", err)
	}
	if u, err := s.UserByIdentifier(ctx, "bob"); err != nil || u.ID != bob {
		t.Errorf("sign-in identifier resolves to %+v, %v", u, err)
	}
	rows, err := s.ListUsers(ctx)
	if err != nil || len(rows) != 1 {
		t.Errorf("accounts after the refusals = %d, %v", len(rows), err)
	}
}

func TestCreateUserAuditRollsBackWithTheAccount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// An audit row that violates the table (duplicate primary key) fails the
	// transaction: no account is left behind.
	row := &AuditRow{ID: NewID(), TS: 1, Actor: "a", Action: "user.create", Target: "t", Outcome: "success"}
	if err := s.InsertAudit(ctx, nil, *row); err != nil {
		t.Fatal(err)
	}
	id := NewID()
	err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: id, Username: "ghost"}, Role: "user", Audit: row})
	if err == nil {
		t.Fatal("create with a failing audit row succeeded")
	}
	if _, err := s.UserByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("the account survived a failed audit write: %v", err)
	}
}

func TestListUsersReportsRoleSources(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := newAccount(t, s, "alpha", "admin")
	b := newAccount(t, s, "Beta", "user")
	c := newAccount(t, s, "gamma", "user")
	if _, err := s.db.Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'idp'), (?, 'admin', 'idp')`, a, c); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Username)
	}
	if !reflect.DeepEqual(got, []string{"alpha", "Beta", "gamma"}) {
		t.Errorf("order = %v", got)
	}
	want := map[string][]string{a: {"idp", "local"}, b: nil, c: {"idp"}}
	for _, r := range rows {
		if !reflect.DeepEqual(r.AdminSources, want[r.ID]) {
			t.Errorf("%s sources = %v, want %v", r.Username, r.AdminSources, want[r.ID])
		}
	}
	if rows[2].Role != "admin" || rows[1].Role != "user" {
		t.Errorf("roles = %q %q", rows[1].Role, rows[2].Role)
	}
}

func TestResetPasswordSetsMustChangeAndRevokesEverySession(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 1, PasswordHash: "own-hash", NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, id); u.MustChangePassword || u.CredentialVersion != 2 {
		t.Fatalf("after change = %+v", u)
	}
	addSession(t, s, id)
	addSession(t, s, id)
	other := newAccount(t, s, "bob", "user")
	addSession(t, s, other)

	err := s.ResetPassword(ctx, ResetPasswordParams{UserID: id, PasswordHash: "temp-hash", NowMS: 9,
		Audit: &AuditRow{ID: NewID(), TS: 9, Actor: "root", Action: "user.reset_password", Target: id, Outcome: "success"}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByID(ctx, id)
	if u.PasswordHash != "temp-hash" || !u.MustChangePassword || u.CredentialVersion != 3 || u.UpdatedAt != 9 {
		t.Errorf("after reset = %+v", u)
	}
	if n := sessionCount(t, s, id); n != 0 {
		t.Errorf("%d sessions survived the reset", n)
	}
	if n := sessionCount(t, s, other); n != 1 {
		t.Errorf("another user's sessions = %d", n)
	}
	if got := auditActions(t, s); !reflect.DeepEqual(got, []string{"user.reset_password"}) {
		t.Errorf("audit = %v", got)
	}
}

func TestResetPasswordRefusals(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: NewID(), PasswordHash: "h"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user = %v", err)
	}
	noPw := NewID()
	if err := s.CreateUser(ctx, CreateUserParams{User: UserRow{ID: noPw, Username: "idp-only"}, Role: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: noPw, PasswordHash: "h"}); !errors.Is(err, ErrNoLocalPassword) {
		t.Errorf("account without a password = %v", err)
	}
	gone := newAccount(t, s, "gone", "user")
	newAccount(t, s, "keeper", "admin")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: gone}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: gone, PasswordHash: "h"}); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated account = %v", err)
	}
}

func TestChangePasswordBumpsTheVersionAndKeepsOnlyTheCallersSession(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	keep := addSession(t, s, id)
	addSession(t, s, id)
	addSession(t, s, id)

	next, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 1, PasswordHash: "new", KeepSessionID: keep.ID, NowMS: 7,
		Audit: &AuditRow{ID: NewID(), TS: 7, Actor: id, Action: "user.password_change", Target: id, Outcome: "success"}})
	if err != nil || next != 2 {
		t.Fatalf("ChangePassword = %d, %v", next, err)
	}
	u, _ := s.UserByID(ctx, id)
	if u.PasswordHash != "new" || u.MustChangePassword || u.CredentialVersion != 2 {
		t.Errorf("user = %+v", u)
	}
	if n := sessionCount(t, s, id); n != 1 {
		t.Fatalf("%d sessions left, want only the caller's", n)
	}
	sess, user, err := s.SessionByTokenHash(ctx, keep.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	// The kept session carries the new version, so the session check still
	// accepts it.
	if sess.CredentialVersion != user.CredentialVersion {
		t.Errorf("kept session version %d, user version %d", sess.CredentialVersion, user.CredentialVersion)
	}
	if got := auditActions(t, s); !reflect.DeepEqual(got, []string{"user.password_change"}) {
		t.Errorf("audit = %v", got)
	}
}

func TestChangePasswordComparesTheCredentialVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 7, PasswordHash: "x"}); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale version = %v", err)
	}
	if u, _ := s.UserByID(ctx, id); u.PasswordHash != "hash-ann" || u.CredentialVersion != 1 {
		t.Errorf("a refused change modified the user: %+v", u)
	}
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: NewID(), ExpectedVersion: 1, PasswordHash: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user = %v", err)
	}
	newAccount(t, s, "keeper", "admin")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 2, PasswordHash: "x"}); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated user = %v", err)
	}
}

// Two changes that read the same version race: exactly one wins, the other
// gets ErrVersionConflict, and the stored version moved by one.
func TestChangePasswordRaceHasExactlyOneWinner(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	const n = 8
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		wins    int
		losses  int
		unknown []error
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 1, PasswordHash: "hash-" + string(rune('a'+i)), NowMS: 1})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrVersionConflict):
				losses++
			default:
				unknown = append(unknown, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 || losses != n-1 || len(unknown) != 0 {
		t.Fatalf("wins=%d losses=%d other errors=%v", wins, losses, unknown)
	}
	if u, _ := s.UserByID(ctx, id); u.CredentialVersion != 2 {
		t.Errorf("credential version = %d, want 2", u.CredentialVersion)
	}
}

func TestSetLocalRole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root := newAccount(t, s, "root", "admin")
	ann := newAccount(t, s, "ann", "user")

	eff, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: ann, Role: "admin", NowMS: 3})
	if err != nil || eff != "admin" {
		t.Fatalf("promote = %q, %v", eff, err)
	}
	if u, _ := s.UserByID(ctx, ann); u.Role != "admin" || u.UpdatedAt != 3 {
		t.Errorf("ann = %+v", u)
	}
	eff, err = s.SetLocalRole(ctx, SetLocalRoleParams{UserID: ann, Role: "user", NowMS: 4})
	if err != nil || eff != "user" {
		t.Fatalf("demote = %q, %v", eff, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'local'`, ann).Scan(&n); err != nil || n != 1 {
		t.Errorf("local grants of ann = %d, %v (want exactly the role itself)", n, err)
	}

	// The last active admin cannot be demoted ...
	if _, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: root, Role: "user"}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote the last admin = %v", err)
	}
	if u, _ := s.UserByID(ctx, root); u.Role != "admin" {
		t.Error("a refused demotion changed the role")
	}
	// ... but can once another admin exists.
	if _, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: ann, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: root, Role: "user"}); err != nil {
		t.Errorf("demote with another admin present = %v", err)
	}
	if _, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: NewID(), Role: "user"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user = %v", err)
	}
	if _, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: ann, Role: "root"}); err == nil {
		t.Error("an unknown role was accepted")
	}
}

func TestSetLocalRoleNeverTouchesIdPGrants(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newAccount(t, s, "root", "admin")
	ann := newAccount(t, s, "ann", "admin")
	if _, err := s.db.Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'idp')`, ann); err != nil {
		t.Fatal(err)
	}
	eff, err := s.SetLocalRole(ctx, SetLocalRoleParams{UserID: ann, Role: "user"})
	if err != nil || eff != "admin" {
		t.Fatalf("effective role = %q, %v (the IdP grant must stay)", eff, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'idp'`, ann).Scan(&n); err != nil || n != 1 {
		t.Errorf("idp grants = %d, %v", n, err)
	}
	// With an IdP admin grant standing, dropping the local one never leaves
	// the Hub without an admin, so the last-admin guard does not fire.
	solo := openTest(t)
	only := newAccount(t, solo, "only", "admin")
	if _, err := solo.db.Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'idp')`, only); err != nil {
		t.Fatal(err)
	}
	if _, err := solo.SetLocalRole(ctx, SetLocalRoleParams{UserID: only, Role: "user"}); err != nil {
		t.Errorf("demote the local grant of an idp admin = %v", err)
	}
}

func TestDeactivateUser(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newAccount(t, s, "root", "admin")
	ann := newAccount(t, s, "ann", "user")
	addSession(t, s, ann)
	addSession(t, s, ann)

	// Workers stay with their owner.
	if err := s.InsertWorker(ctx, nil, worker(NewID(), 1, ann, "laptop")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertWorker(ctx, nil, worker(NewID(), 2, ann, "server")); err != nil {
		t.Fatal(err)
	}

	var hookUser string
	changed, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: ann, NowMS: 11,
		Hooks: []TxHook{func(_ context.Context, tx *sql.Tx, uid string) error {
			hookUser = uid
			// The hook sees the transaction: the account is already inactive.
			var active int
			if err := tx.QueryRow(`SELECT active FROM users WHERE id = ?`, uid).Scan(&active); err != nil || active != 0 {
				t.Errorf("inside the hook: active=%d err=%v", active, err)
			}
			return nil
		}},
		Audit: &AuditRow{ID: NewID(), TS: 11, Actor: "root", Action: "user.deactivate", Target: ann, Outcome: "success"}})
	if err != nil || !changed {
		t.Fatalf("DeactivateUser = %v, %v", changed, err)
	}
	if hookUser != ann {
		t.Errorf("hook user = %q", hookUser)
	}
	u, _ := s.UserByID(ctx, ann)
	if u.Active || u.CredentialVersion != 2 || u.UpdatedAt != 11 {
		t.Errorf("after deactivation = %+v", u)
	}
	if n := sessionCount(t, s, ann); n != 0 {
		t.Errorf("%d sessions survived", n)
	}
	var owned int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM workers WHERE owner_id = ?`, ann).Scan(&owned); err != nil || owned != 2 {
		t.Errorf("Workers owned after deactivation = %d, %v (they stay with the account)", owned, err)
	}
	if got := auditActions(t, s); !reflect.DeepEqual(got, []string{"user.deactivate"}) {
		t.Errorf("audit = %v", got)
	}

	// A second deactivation succeeds and changes nothing, hooks included.
	hookUser = ""
	changed, err = s.DeactivateUser(ctx, DeactivateUserParams{UserID: ann, Hooks: []TxHook{func(context.Context, *sql.Tx, string) error {
		hookUser = "called"
		return nil
	}}})
	if err != nil || changed || hookUser != "" {
		t.Errorf("repeat = %v, %v, hook %q", changed, err, hookUser)
	}
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: NewID()}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user = %v", err)
	}
}

func TestDeactivateUserRollsBackWhenAHookFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newAccount(t, s, "root", "admin")
	ann := newAccount(t, s, "ann", "user")
	addSession(t, s, ann)
	boom := errors.New("key store down")
	_, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: ann, Hooks: []TxHook{func(context.Context, *sql.Tx, string) error { return boom }}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if u, _ := s.UserByID(ctx, ann); !u.Active || u.CredentialVersion != 1 {
		t.Errorf("a failed hook left the account changed: %+v", u)
	}
	if n := sessionCount(t, s, ann); n != 1 {
		t.Errorf("sessions = %d, want the session kept", n)
	}
}

func TestDeactivateRefusesTheLastActiveAdmin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root := newAccount(t, s, "root", "admin")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: root}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("deactivate the only admin = %v", err)
	}
	if u, _ := s.UserByID(ctx, root); !u.Active {
		t.Error("the refused deactivation changed the account")
	}
	// An inactive admin does not count as another admin.
	second := newAccount(t, s, "second", "admin")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: second}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: root}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deactivate the admin next to an inactive one = %v", err)
	}
}
