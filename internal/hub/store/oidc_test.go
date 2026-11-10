package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
)

func provider(id string) OIDCProviderRow {
	return OIDCProviderRow{
		ID: id, Name: "Acme", Issuer: "https://idp.example", ClientID: "cid", ClientSecret: "very-secret-value",
		RoleClaim: "groups", Scopes: "openid email", Enabled: true, CreatedAt: 10, UpdatedAt: 10,
	}
}

func TestOIDCProviderSecretIsSealedAndKeptOnUpdate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutOIDCProvider(ctx, provider("acme")); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.DB().QueryRow(`SELECT client_secret_enc FROM oidc_providers WHERE id = 'acme'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte("very-secret-value")) {
		t.Fatal("client secret is not encrypted")
	}
	got, err := s.GetOIDCProvider(ctx, "acme")
	if err != nil || got.ClientSecret != "very-secret-value" || !got.Enabled || got.RoleClaim != "groups" {
		t.Fatalf("got %+v, %v", got, err)
	}

	// An update without a secret keeps the stored one and the creation time.
	upd := provider("acme")
	upd.ClientSecret, upd.Name, upd.CreatedAt, upd.UpdatedAt, upd.Enabled, upd.InsecureSkipVerify = "", "Renamed", 99, 99, false, true
	if err := s.PutOIDCProvider(ctx, upd); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetOIDCProvider(ctx, "acme")
	if got.ClientSecret != "very-secret-value" || got.Name != "Renamed" || got.CreatedAt != 10 || got.UpdatedAt != 99 || got.Enabled || !got.InsecureSkipVerify {
		t.Fatalf("after update: %+v", got)
	}
	// A new secret replaces it.
	upd.ClientSecret = "rotated"
	if err := s.PutOIDCProvider(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetOIDCProvider(ctx, "acme"); got.ClientSecret != "rotated" {
		t.Fatalf("secret = %q", got.ClientSecret)
	}
	// A sealed value moved to another row does not open there.
	if _, err := s.DB().Exec(`UPDATE oidc_providers SET client_secret_enc = (SELECT client_secret_enc FROM oidc_providers WHERE id='acme')`); err != nil {
		t.Fatal(err)
	}
	other := provider("other")
	other.ClientSecret = ""
	if err := s.PutOIDCProvider(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE oidc_providers SET client_secret_enc = (SELECT client_secret_enc FROM oidc_providers WHERE id='acme') WHERE id = 'other'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOIDCProvider(ctx, "other"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a copied secret opened: %v", err)
	}
}

func TestOIDCProviderRedactsInLogs(t *testing.T) {
	p := provider("acme")
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "p", p)
	for _, out := range []string{buf.String(), fmt.Sprintf("%v %+v %#v %s", p, p, p, p)} {
		if strings.Contains(out, "very-secret-value") {
			t.Fatalf("secret leaked: %s", out)
		}
	}
}

func TestOIDCProviderListDeleteAndValidation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, id := range []string{"b", "a"} {
		if err := s.PutOIDCProvider(ctx, provider(id)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListOIDCProviders(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "a" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.PutOIDCProvider(ctx, OIDCProviderRow{ID: "x"}); err == nil {
		t.Error("an incomplete provider was stored")
	}
	if _, err := s.GetOIDCProvider(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get unknown: %v", err)
	}
	if err := s.ReplaceRoleRules(ctx, "a", "admin", []string{"g1"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteOIDCProvider(ctx, "a"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.DeleteOIDCProvider(ctx, "a"); ok {
		t.Error("deleted twice")
	}
	if rules, _ := s.ListRoleRules(ctx, "a"); len(rules) != 0 {
		t.Errorf("rules outlived the provider: %v", rules)
	}
}

func TestRoleRules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutOIDCProvider(ctx, provider("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRoleRules(ctx, "a", "admin", []string{"g1", "g2", "g1", ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceRoleRules(ctx, "a", "user", []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	rules, _ := s.ListRoleRules(ctx, "a")
	want := []RoleRuleRow{{"a", "admin", "g1"}, {"a", "admin", "g2"}, {"a", "user", "u1"}}
	if !slices.Equal(rules, want) {
		t.Fatalf("rules = %v", rules)
	}
	// Replacing one role leaves the other; no value removes the rule.
	if err := s.ReplaceRoleRules(ctx, "a", "admin", nil); err != nil {
		t.Fatal(err)
	}
	if rules, _ = s.ListRoleRules(ctx, ""); !slices.Equal(rules, want[2:]) {
		t.Fatalf("rules = %v", rules)
	}
	if err := s.ReplaceRoleRules(ctx, "missing", "admin", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
	if err := s.ReplaceRoleRules(ctx, "a", "root", nil); err == nil {
		t.Error("unknown role accepted")
	}
}

func TestOIDCLoginTransactionIsSingleUseAndExpires(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	tx := OIDCLoginTx{StateHash: HashValue("s1"), BindingHash: HashValue("b1"), ProviderID: "p", Nonce: "n",
		CodeVerifier: "verifier-value", NextPath: "/x", CreatedAt: 100, ExpiresAt: 200}
	if err := s.CreateOIDCLoginTx(ctx, tx, 100); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.DB().QueryRow(`SELECT code_verifier_enc FROM oidc_login_transactions`).Scan(&raw); err != nil || bytes.Contains(raw, []byte("verifier-value")) {
		t.Fatalf("verifier stored in clear: %v", err)
	}
	got, err := s.ConsumeOIDCLoginTx(ctx, tx.StateHash, 150)
	if err != nil || got != tx {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	if _, err := s.ConsumeOIDCLoginTx(ctx, tx.StateHash, 150); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second use: %v", err)
	}
	// Expired rows are refused and removed; creating purges them.
	if err := s.CreateOIDCLoginTx(ctx, tx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOIDCLoginTx(ctx, tx.StateHash, 200); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	old := tx
	old.StateHash = HashValue("s2")
	_ = s.CreateOIDCLoginTx(ctx, old, 100)
	fresh := tx
	fresh.StateHash, fresh.ExpiresAt = HashValue("s3"), 1000
	if err := s.CreateOIDCLoginTx(ctx, fresh, 500); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM oidc_login_transactions`).Scan(&n)
	if n != 1 {
		t.Errorf("expired transactions were not purged: %d", n)
	}
}

func syncParams(sub string, roles ...string) OIDCSyncParams {
	return OIDCSyncParams{Issuer: "https://idp", Subject: sub, ProviderID: "default", PreferredUsername: "carol",
		Roles: roles, CreateIfMissing: true, NowMS: 1000}
}

func TestSyncOIDCLogin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Unknown identity without CreateIfMissing creates nothing.
	p := syncParams("s1", "user")
	p.CreateIfMissing = false
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || res.Found {
		t.Fatalf("no-create: %+v, %v", res, err)
	}
	if has, _ := s.HasUsers(ctx); has {
		t.Fatal("a user was created")
	}

	res, err = s.SyncOIDCLogin(ctx, syncParams("s1", "admin", "user", "admin"))
	if err != nil || !res.Created || res.User.Username != "carol" || res.User.Role != "admin" ||
		!slices.Equal(res.RolesAfter, []string{"admin", "user"}) || !res.RolesChanged() {
		t.Fatalf("first sync: %+v, %v", res, err)
	}
	if res.User.PasswordHash != "" || res.User.MustChangePassword || !res.User.Active {
		t.Errorf("OIDC account has a password state: %+v", res.User)
	}
	// Same roles: unchanged. Different roles: replaced; local rows stay.
	if _, err := s.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, res.User.ID); err != nil {
		t.Fatal(err)
	}
	res2, err := s.SyncOIDCLogin(ctx, syncParams("s1", "admin", "user"))
	if err != nil || res2.Created || res2.RolesChanged() || res2.User.ID != res.User.ID {
		t.Fatalf("second sync: %+v, %v", res2, err)
	}
	res3, err := s.SyncOIDCLogin(ctx, syncParams("s1"))
	if err != nil || !slices.Equal(res3.RolesBefore, []string{"admin", "user"}) || len(res3.RolesAfter) != 0 || res3.User.Role != "admin" {
		t.Fatalf("empty sync (local admin stays): %+v, %v", res3, err)
	}
	// Roles are validated before anything is written.
	if _, err := s.SyncOIDCLogin(ctx, syncParams("s1", "root")); err == nil {
		t.Error("unknown role accepted")
	}
	if _, err := s.SyncOIDCLogin(ctx, OIDCSyncParams{Issuer: "x"}); err == nil {
		t.Error("identity without subject accepted")
	}
}

func TestSyncOIDCLoginUserNames(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// A local account with the same name is not merged with the OIDC account.
	if _, err := s.SeedAdmin(ctx, SeedAdminParams{User: UserRow{ID: NewID(), Username: "Carol", CreatedAt: 1, UpdatedAt: 1}, Marker: "m"}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"carol-2", "carol-3"} {
		res, err := s.SyncOIDCLogin(ctx, syncParams(fmt.Sprintf("sub-%d", i), "user"))
		if err != nil || res.User.Username != want {
			t.Fatalf("name = %q (%v), want %s", res.User.Username, err, want)
		}
	}
	p := syncParams("weird", "user")
	p.PreferredUsername = "  <b>x</b>\n\x00 "
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || res.User.Username != "bxb" {
		t.Fatalf("sanitised name = %q, %v", res.User.Username, err)
	}
	p = syncParams("nameless", "user")
	p.PreferredUsername = "<>"
	if res, err = s.SyncOIDCLogin(ctx, p); err != nil || !strings.HasPrefix(res.User.Username, "oidc-") {
		t.Fatalf("fallback name = %q, %v", res.User.Username, err)
	}
	p = syncParams("long", "user")
	p.PreferredUsername = strings.Repeat("a", 200)
	if res, err = s.SyncOIDCLogin(ctx, p); err != nil || len(res.User.Username) > 64 {
		t.Fatalf("long name: %d, %v", len(res.User.Username), err)
	}
}

func TestSyncOIDCLoginRefreshTokenAndInactive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := syncParams("s1", "user")
	p.RefreshToken = "refresh-1"
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	read := func() string {
		var id string
		var sealed []byte
		if err := s.DB().QueryRow(`SELECT id, refresh_token_enc FROM external_identities WHERE user_id = ?`, res.User.ID).Scan(&id, &sealed); err != nil {
			t.Fatal(err)
		}
		if len(sealed) == 0 {
			return ""
		}
		plain, err := s.Box().Open("external_identities", "refresh_token", id, sealed)
		if err != nil {
			t.Fatal(err)
		}
		return string(plain)
	}
	if read() != "refresh-1" {
		t.Fatal("refresh token not stored")
	}
	// A login without a refresh token keeps the old one; a new one replaces it.
	p.RefreshToken = ""
	if _, err := s.SyncOIDCLogin(ctx, p); err != nil || read() != "refresh-1" {
		t.Fatalf("token after a login without one: %q, %v", read(), err)
	}
	p.RefreshToken = "refresh-2"
	_, _ = s.SyncOIDCLogin(ctx, p)
	if read() != "refresh-2" {
		t.Fatal("token not replaced")
	}

	// A deactivated account is left untouched.
	if _, err := s.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, res.User.ID); err != nil {
		t.Fatal(err)
	}
	p.Roles, p.RefreshToken = []string{"admin"}, "refresh-3"
	res2, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || !res2.Inactive || res2.User.Role != "user" || read() != "refresh-2" {
		t.Fatalf("inactive: %+v, token %q, %v", res2, read(), err)
	}
}

func TestSyncOIDCLoginIsOneTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	res, err := s.SyncOIDCLogin(ctx, syncParams("s1", "admin"))
	if err != nil {
		t.Fatal(err)
	}
	// A trigger that fails the role insert makes the whole sync roll back: the
	// old role stays and nothing is half replaced.
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_idp BEFORE INSERT ON user_roles WHEN NEW.source = 'idp' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncOIDCLogin(ctx, syncParams("s1", "user")); err == nil {
		t.Fatal("expected a failure")
	}
	if _, err := s.DB().Exec(`DROP TRIGGER fail_idp`); err != nil {
		t.Fatal(err)
	}
	var roles []string
	rows, _ := s.DB().Query(`SELECT role FROM user_roles WHERE user_id = ? AND source = 'idp'`, res.User.ID)
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		roles = append(roles, r)
	}
	_ = rows.Close()
	if !slices.Equal(roles, []string{"admin"}) {
		t.Fatalf("roles after a failed sync: %v", roles)
	}
}

func TestSyncOIDCLoginConcurrentFirstLoginsCreateOneAccount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SyncOIDCLogin(ctx, syncParams("same", "user")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("users = %d", n)
	}
}

func TestReplaceIDPRoles(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	res, _ := s.SyncOIDCLogin(ctx, syncParams("s1", "admin"))
	before, after, err := s.ReplaceIDPRoles(ctx, res.User.ID, []string{"user"})
	if err != nil || !slices.Equal(before, []string{"admin"}) || !slices.Equal(after, []string{"user"}) {
		t.Fatalf("%v %v %v", before, after, err)
	}
	if _, _, err := s.ReplaceIDPRoles(ctx, res.User.ID, []string{"x"}); err == nil {
		t.Error("unknown role accepted")
	}
}

func TestExternalIdentityIsUniquePerIssuerAndSubject(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	res, _ := s.SyncOIDCLogin(ctx, syncParams("s1", "user"))
	_, err := s.DB().Exec(`INSERT INTO external_identities (id, user_id, issuer, subject, created_at, last_login_at) VALUES (?, ?, 'https://idp', 's1', 1, 1)`, NewID(), res.User.ID)
	if err == nil {
		t.Fatal("a second identity with the same issuer and subject was accepted")
	}
	// The same subject at another issuer is another identity.
	p := syncParams("s1", "user")
	p.Issuer = "https://other-idp"
	res2, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || res2.User.ID == res.User.ID {
		t.Fatalf("other issuer: %+v, %v", res2, err)
	}
}

func TestSyncOIDCLoginWithNoRolesEndsTheSessionsOfThatAccountOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, err := s.SyncOIDCLogin(ctx, syncParams("a", "user"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SyncOIDCLogin(ctx, syncParams("b", "user"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, a.User.ID); err != nil {
		t.Fatal(err)
	}
	add := func(id, userID string) {
		t.Helper()
		if err := s.CreateSession(ctx, SessionRow{ID: id, TokenHash: "hash-" + id, UserID: userID, CredentialVersion: 1, CreatedAt: 1000, ExpiresAt: 1 << 40}, 1000); err != nil {
			t.Fatal(err)
		}
	}
	count := func(userID string) int {
		t.Helper()
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	add("a1", a.User.ID)
	add("a2", a.User.ID)
	add("b1", b.User.ID)

	// A sync that keeps a role keeps the sessions.
	res, err := s.SyncOIDCLogin(ctx, syncParams("a", "admin"))
	if err != nil || res.SessionsEnded != 0 || count(a.User.ID) != 2 {
		t.Fatalf("sync with a role: %+v, sessions %d, %v", res, count(a.User.ID), err)
	}

	// No role at all: that account's sessions end, local grants stay, others stay.
	res, err = s.SyncOIDCLogin(ctx, syncParams("a"))
	if err != nil || res.SessionsEnded != 2 || count(a.User.ID) != 0 || count(b.User.ID) != 1 {
		t.Fatalf("sync without a role: %+v, a=%d b=%d, %v", res, count(a.User.ID), count(b.User.ID), err)
	}
	var local int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'local'`, a.User.ID).Scan(&local); err != nil || local != 1 {
		t.Fatalf("local grant: %d, %v", local, err)
	}

	// An unknown identity without a role creates nothing and ends nothing.
	p := syncParams("stranger")
	p.CreateIfMissing = false
	if res, err := s.SyncOIDCLogin(ctx, p); err != nil || res.Found || res.SessionsEnded != 0 || count(b.User.ID) != 1 {
		t.Fatalf("unknown identity: %+v, %v", res, err)
	}

	// A deactivated account is left unchanged, sessions included.
	if _, err := s.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, b.User.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := s.SyncOIDCLogin(ctx, syncParams("b")); err != nil || !res.Inactive || res.SessionsEnded != 0 || count(b.User.ID) != 1 {
		t.Fatalf("inactive account: %+v, %v", res, err)
	}
}

func TestSyncOIDCLoginOnRolesChangedRunsInTheTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	calls := 0
	p := syncParams("s1", "user")
	p.OnRolesChanged = func(ctx context.Context, ex Execer, res OIDCSyncResult) error {
		calls++
		return s.InsertAudit(ctx, ex, AuditRow{ID: NewID(), TS: 1, Actor: res.User.ID, Action: "role.sync", Target: res.User.ID, Outcome: "changed"})
	}
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || calls != 1 {
		t.Fatalf("first sync: %d calls, %v", calls, err)
	}
	// Unchanged roles: no callback.
	if _, err := s.SyncOIDCLogin(ctx, p); err != nil || calls != 1 {
		t.Fatalf("unchanged sync: %d calls, %v", calls, err)
	}
	// A failing callback rolls the role change back.
	p2 := syncParams("s1", "admin")
	p2.OnRolesChanged = func(context.Context, Execer, OIDCSyncResult) error { return errors.New("audit down") }
	if _, err := s.SyncOIDCLogin(ctx, p2); err == nil {
		t.Fatal("expected a failure")
	}
	var role string
	if err := s.DB().QueryRow(`SELECT role FROM user_roles WHERE user_id = ? AND source = 'idp'`, res.User.ID).Scan(&role); err != nil || role != "user" {
		t.Fatalf("role after a failed audit: %q, %v", role, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d, %v", n, err)
	}
}

func TestSyncOIDCLoginWithNoRolesInvalidatesSessionsIssuedFromAStaleSnapshot(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	stale, err := s.SyncOIDCLogin(ctx, syncParams("a", "user"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SyncOIDCLogin(ctx, syncParams("b", "user"))
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := s.SyncOIDCLogin(ctx, syncParams("a"))
	if err != nil {
		t.Fatal(err)
	}
	if revoked.User.CredentialVersion <= stale.User.CredentialVersion {
		t.Fatalf("credential version %d -> %d: not raised", stale.User.CredentialVersion, revoked.User.CredentialVersion)
	}
	// A session issued from the snapshot taken before the revocation carries
	// the old version, which no longer matches the account.
	if err := s.CreateSession(ctx, SessionRow{ID: "late", TokenHash: "hash-late", UserID: stale.User.ID,
		CredentialVersion: stale.User.CredentialVersion, CreatedAt: 1000, ExpiresAt: 1 << 40}, 1000); err != nil {
		t.Fatal(err)
	}
	row, user, err := s.SessionByTokenHash(ctx, "hash-late")
	if err != nil || row.CredentialVersion == user.CredentialVersion {
		t.Fatalf("stale session still matches: %+v %+v, %v", row, user, err)
	}
	// Another account and a sync that keeps a role keep their version.
	if got, err := s.SyncOIDCLogin(ctx, syncParams("b", "admin")); err != nil || got.User.CredentialVersion != other.User.CredentialVersion {
		t.Fatalf("other account: %+v, %v", got.User, err)
	}
}
