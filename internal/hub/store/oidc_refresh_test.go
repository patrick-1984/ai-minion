package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// refreshFixture is one OIDC account "carol" with IdP roles admin and user, a
// local grant, one session and the refresh token "rt-1", signed in at 1000.
type refreshFixture struct {
	*Store
	userID, identityID string
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	p := syncParams("rs", "admin", "user")
	p.RefreshToken = "rt-1"
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'user', 'local')`, res.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, SessionRow{ID: "s1", TokenHash: "hash-s1", UserID: res.User.ID, CredentialVersion: 1, CreatedAt: 1000, ExpiresAt: 1 << 40}, 1000); err != nil {
		t.Fatal(err)
	}
	f := &refreshFixture{Store: s, userID: res.User.ID}
	if err := s.DB().QueryRow(`SELECT id FROM external_identities WHERE user_id = ?`, f.userID).Scan(&f.identityID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *refreshFixture) target(t *testing.T) OIDCRefreshTarget {
	t.Helper()
	targets, unreadable, err := f.ListOIDCRefreshTargets(context.Background())
	if err != nil || len(targets) != 1 || len(unreadable) != 0 {
		t.Fatalf("targets = %v, unreadable = %v, err = %v", targets, unreadable, err)
	}
	return targets[0]
}

func (f *refreshFixture) update(tg OIDCRefreshTarget) OIDCRefreshUpdate {
	return OIDCRefreshUpdate{IdentityID: tg.IdentityID, UserID: tg.UserID, ExpectedToken: tg.RefreshToken,
		ExpectedLastLoginAt: tg.LastLoginAt, NowMS: 2000}
}

func (f *refreshFixture) token(t *testing.T) string {
	t.Helper()
	var sealed []byte
	if err := f.DB().QueryRow(`SELECT refresh_token_enc FROM external_identities WHERE id = ?`, f.identityID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 {
		return ""
	}
	plain, err := f.Box().Open("external_identities", "refresh_token", f.identityID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func (f *refreshFixture) roles(t *testing.T, source string) []string {
	t.Helper()
	rows, err := f.DB().Query(`SELECT role FROM user_roles WHERE user_id = ? AND source = ? ORDER BY role`, f.userID, source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	return out
}

func (f *refreshFixture) sessions(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, f.userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestListOIDCRefreshTargets(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	tg := f.target(t)
	if tg.UserID != f.userID || tg.IdentityID != f.identityID || tg.Issuer != "https://idp" || tg.Subject != "rs" ||
		tg.ProviderID != "default" || tg.LastLoginAt != 1000 || tg.RefreshToken != "rt-1" {
		t.Errorf("target = %#v", tg)
	}

	// An account without a token, a deactivated account and a damaged token
	// are not targets; the damaged one is reported by id, never by value.
	nt := syncParams("no-token", "user")
	nt.PreferredUsername = "dave"
	if _, err := f.SyncOIDCLogin(ctx, nt); err != nil {
		t.Fatal(err)
	}
	off := syncParams("off", "user")
	off.PreferredUsername, off.RefreshToken = "erin", "rt-off"
	offRes, err := f.SyncOIDCLogin(ctx, off)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, offRes.User.ID); err != nil {
		t.Fatal(err)
	}
	bad := syncParams("bad", "user")
	bad.PreferredUsername, bad.RefreshToken = "frank", "rt-bad"
	badRes, err := f.SyncOIDCLogin(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.DB().Exec(`UPDATE external_identities SET refresh_token_enc = ? WHERE user_id = ?`, []byte("garbage-not-sealed-data-garbage-garbage"), badRes.User.ID); err != nil {
		t.Fatal(err)
	}
	targets, unreadable, err := f.ListOIDCRefreshTargets(ctx)
	if err != nil || len(targets) != 1 || targets[0].IdentityID != f.identityID {
		t.Fatalf("targets = %v, err = %v", targets, err)
	}
	if len(unreadable) != 1 {
		t.Fatalf("unreadable = %v", unreadable)
	}
}

func TestOIDCRefreshTargetRedactsTheToken(t *testing.T) {
	tg := OIDCRefreshTarget{IdentityID: "i", UserID: "u", ProviderID: "p", RefreshToken: "very-secret-refresh-token"}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "t", tg)
	for _, out := range []string{buf.String(), fmt.Sprintf("%v %+v %#v %s", tg, tg, tg, tg)} {
		if strings.Contains(out, "very-secret-refresh-token") {
			t.Errorf("token leaked: %s", out)
		}
	}
}

func TestApplyOIDCRefreshReplacesIDPRolesOnly(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	u := f.update(f.target(t))
	u.ReplaceRoles, u.Roles = true, []string{"user", "user"}
	var inTx OIDCRefreshResult
	u.OnApplied = func(_ context.Context, ex Execer, res OIDCRefreshResult) error {
		inTx = res
		return f.InsertAudit(ctx, ex, AuditRow{ID: NewID(), TS: 1, Actor: "system", Action: "role.sync", Target: res.UserID, Outcome: "changed"})
	}
	res, err := f.ApplyOIDCRefresh(ctx, u)
	if err != nil || !res.Applied || !res.RolesChanged() || res.UserID != f.userID ||
		!slices.Equal(res.RolesBefore, []string{"admin", "user"}) || !slices.Equal(res.RolesAfter, []string{"user"}) {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !inTx.Applied {
		t.Error("OnApplied did not see the result")
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"user"}) {
		t.Errorf("idp roles = %v", got)
	}
	if got := f.roles(t, "local"); !slices.Equal(got, []string{"user"}) {
		t.Errorf("local roles = %v", got)
	}
	if f.token(t) != "rt-1" || f.sessions(t) != 1 {
		t.Errorf("token %q, sessions %d: a role change that keeps a role keeps both", f.token(t), f.sessions(t))
	}
	var n int
	if err := f.DB().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'role.sync'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows = %d, %v", n, err)
	}
}

func TestApplyOIDCRefreshRollsBackWhenOnAppliedFails(t *testing.T) {
	f := newRefreshFixture(t)
	u := f.update(f.target(t))
	u.ReplaceRoles, u.Roles, u.NewToken = true, []string{"user"}, "rt-2"
	u.OnApplied = func(context.Context, Execer, OIDCRefreshResult) error { return errors.New("audit down") }
	if _, err := f.ApplyOIDCRefresh(context.Background(), u); err == nil {
		t.Fatal("no error")
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"admin", "user"}) || f.token(t) != "rt-1" {
		t.Errorf("idp roles %v, token %q: not rolled back", got, f.token(t))
	}
}

func TestApplyOIDCRefreshLastIDPRoleEndsSessionsAndBumpsTheVersion(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	before, err := f.UserByID(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	u := f.update(f.target(t))
	u.ReplaceRoles = true // no roles: the last IdP role goes
	res, err := f.ApplyOIDCRefresh(ctx, u)
	if err != nil || !res.Applied || res.SessionsEnded != 1 || len(res.RolesAfter) != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if f.sessions(t) != 0 {
		t.Error("the session survived")
	}
	after, _ := f.UserByID(ctx, f.userID)
	if after.CredentialVersion != before.CredentialVersion+1 {
		t.Errorf("credential_version %d -> %d", before.CredentialVersion, after.CredentialVersion)
	}
	if f.token(t) != "rt-1" {
		t.Error("fresh claims without roles must keep the token")
	}
	if got := f.roles(t, "local"); !slices.Equal(got, []string{"user"}) {
		t.Errorf("local roles = %v", got)
	}
}

func TestApplyOIDCRefreshSealsARotatedToken(t *testing.T) {
	f := newRefreshFixture(t)
	u := f.update(f.target(t))
	u.NewToken = "rt-2-secret"
	res, err := f.ApplyOIDCRefresh(context.Background(), u)
	if err != nil || !res.Applied || res.RolesChanged() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if f.token(t) != "rt-2-secret" {
		t.Errorf("token = %q", f.token(t))
	}
	var raw []byte
	if err := f.DB().QueryRow(`SELECT refresh_token_enc FROM external_identities WHERE id = ?`, f.identityID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("rt-2-secret")) {
		t.Error("the rotated token is stored in the clear")
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"admin", "user"}) {
		t.Errorf("an update that only stores a token changed the roles: %v", got)
	}
	// The next listing returns the new token.
	if tg := f.target(t); tg.RefreshToken != "rt-2-secret" {
		t.Errorf("target token = %q", tg.RefreshToken)
	}
}

func TestApplyOIDCRefreshSuspendDropsTheTokenAndTheRoles(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	u := f.update(f.target(t))
	u.Suspend, u.ReplaceRoles, u.Roles, u.NewToken = true, false, []string{"admin"}, "ignored"
	res, err := f.ApplyOIDCRefresh(ctx, u)
	if err != nil || !res.Applied || res.SessionsEnded != 1 || !slices.Equal(res.RolesBefore, []string{"admin", "user"}) || len(res.RolesAfter) != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := f.roles(t, "idp"); len(got) != 0 {
		t.Errorf("idp roles = %v", got)
	}
	if f.token(t) != "" {
		t.Errorf("token = %q, want it dropped", f.token(t))
	}
	if f.sessions(t) != 0 {
		t.Error("the session survived the suspension")
	}
	if got := f.roles(t, "local"); !slices.Equal(got, []string{"user"}) {
		t.Errorf("local roles = %v", got)
	}
	// Nothing is left to refresh: the account is no target any more.
	if targets, _, err := f.ListOIDCRefreshTargets(ctx); err != nil || len(targets) != 0 {
		t.Errorf("targets after suspension = %v, %v", targets, err)
	}
}

func TestApplyOIDCRefreshSuspendOfAnAccountWithoutIDPRolesStillDropsTheToken(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	if _, err := f.DB().Exec(`DELETE FROM user_roles WHERE user_id = ? AND source = 'idp'`, f.userID); err != nil {
		t.Fatal(err)
	}
	u := f.update(f.target(t))
	u.Suspend = true
	res, err := f.ApplyOIDCRefresh(ctx, u)
	if err != nil || !res.Applied || res.SessionsEnded != 0 || f.token(t) != "" || f.sessions(t) != 1 {
		t.Fatalf("res = %+v, token %q, sessions %d, err = %v", res, f.token(t), f.sessions(t), err)
	}
}

func TestApplyOIDCRefreshStaleTokenIsRefused(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	stale := f.target(t)

	// A sign-in stored a new token while the refresh with the old one ran.
	p := syncParams("rs", "admin", "user")
	p.RefreshToken, p.NowMS = "rt-new", 3000
	if _, err := f.SyncOIDCLogin(ctx, p); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(u *OIDCRefreshUpdate){
		"suspend":       func(u *OIDCRefreshUpdate) { u.Suspend = true },
		"replace":       func(u *OIDCRefreshUpdate) { u.ReplaceRoles = true },
		"store a token": func(u *OIDCRefreshUpdate) { u.NewToken = "rt-x" },
	} {
		u := f.update(stale)
		u.ExpectedLastLoginAt = 3000 // only the token differs
		mod(&u)
		res, err := f.ApplyOIDCRefresh(ctx, u)
		if err != nil || res.Applied {
			t.Errorf("%s: res = %+v, err = %v; a stale token must not apply", name, res, err)
		}
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"admin", "user"}) || f.token(t) != "rt-new" || f.sessions(t) != 1 {
		t.Errorf("idp roles %v, token %q, sessions %d", got, f.token(t), f.sessions(t))
	}
}

// A sign-in that reuses the stored token, or returns none, leaves the token as
// it was; the revision of the identity still tells the refresh that a sign-in
// happened since it started.
func TestApplyOIDCRefreshAfterALaterSignInWithTheSameTokenIsRefused(t *testing.T) {
	for name, token := range map[string]string{"token omitted": "", "token reused": "rt-1"} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshFixture(t)
			ctx := context.Background()
			stale := f.target(t)

			// The sign-in demotes the account to user and does not change the token.
			p := syncParams("rs", "user")
			p.RefreshToken, p.NowMS = token, 3000
			if _, err := f.SyncOIDCLogin(ctx, p); err != nil {
				t.Fatal(err)
			}
			if f.token(t) != "rt-1" {
				t.Fatalf("setup: token = %q", f.token(t))
			}

			// The refresh that started before it must not promote the account again.
			u := f.update(stale)
			u.ReplaceRoles, u.Roles = true, []string{"admin", "user"}
			if res, err := f.ApplyOIDCRefresh(context.Background(), u); err != nil || res.Applied {
				t.Fatalf("res = %+v, err = %v; a refresh older than a sign-in must not apply", res, err)
			}
			if got := f.roles(t, "idp"); !slices.Equal(got, []string{"user"}) {
				t.Errorf("idp roles = %v, want the sign-in's [user]", got)
			}
			// A suspension from before the sign-in must not undo it either.
			u = f.update(stale)
			u.Suspend = true
			if res, err := f.ApplyOIDCRefresh(ctx, u); err != nil || res.Applied || f.token(t) != "rt-1" {
				t.Fatalf("suspend: res = %+v, token %q, err = %v", res, f.token(t), err)
			}
		})
	}
}

// Two sign-ins with the same timestamp (or a clock that stepped back) and an
// unchanged token must still advance the identity revision, so a refresh
// captured between them is refused.
func TestApplyOIDCRefreshAfterASignInWithTheSameTimestampIsRefused(t *testing.T) {
	for name, nowMS := range map[string]int64{"same millisecond": 1000, "clock stepped back": 500} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshFixture(t) // signed in at 1000 with admin and user
			ctx := context.Background()
			stale := f.target(t)

			p := syncParams("rs", "user")
			p.NowMS = nowMS
			if _, err := f.SyncOIDCLogin(ctx, p); err != nil {
				t.Fatal(err)
			}
			if after := f.target(t); after.LastLoginAt <= stale.LastLoginAt || after.RefreshToken != "rt-1" {
				t.Fatalf("revision %d -> %d, token %q; a sign-in must advance the revision", stale.LastLoginAt, after.LastLoginAt, after.RefreshToken)
			}
			u := f.update(stale)
			u.ReplaceRoles, u.Roles = true, []string{"admin", "user"}
			if res, err := f.ApplyOIDCRefresh(ctx, u); err != nil || res.Applied {
				t.Fatalf("res = %+v, err = %v; a refresh older than a sign-in must not apply", res, err)
			}
			if got := f.roles(t, "idp"); !slices.Equal(got, []string{"user"}) {
				t.Errorf("idp roles = %v, want the sign-in's [user]", got)
			}
			u = f.update(stale)
			u.Suspend = true
			if res, err := f.ApplyOIDCRefresh(ctx, u); err != nil || res.Applied || f.sessions(t) != 1 || f.token(t) != "rt-1" {
				t.Fatalf("suspend: res = %+v, sessions %d, token %q, err = %v", res, f.sessions(t), f.token(t), err)
			}
			// A refresh that read the new revision still applies.
			u = f.update(f.target(t))
			u.ReplaceRoles, u.Roles = true, []string{"user"}
			if res, err := f.ApplyOIDCRefresh(ctx, u); err != nil || !res.Applied {
				t.Fatalf("current revision: res = %+v, err = %v", res, err)
			}
		})
	}
}

func TestApplyOIDCRefreshRefusesGoneAndInactiveAccounts(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	tg := f.target(t)

	other := f.update(tg)
	other.IdentityID = "no-such-identity"
	other.ReplaceRoles = true
	if res, err := f.ApplyOIDCRefresh(ctx, other); err != nil || res.Applied {
		t.Errorf("unknown identity: %+v, %v", res, err)
	}
	wrongUser := f.update(tg)
	wrongUser.UserID = "someone-else"
	wrongUser.ReplaceRoles = true
	if res, err := f.ApplyOIDCRefresh(ctx, wrongUser); err != nil || res.Applied {
		t.Errorf("identity of another user: %+v, %v", res, err)
	}
	if _, err := f.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, f.userID); err != nil {
		t.Fatal(err)
	}
	u := f.update(tg)
	u.ReplaceRoles = true
	if res, err := f.ApplyOIDCRefresh(ctx, u); err != nil || res.Applied {
		t.Errorf("inactive user: %+v, %v", res, err)
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"admin", "user"}) {
		t.Errorf("idp roles = %v", got)
	}
}

func TestApplyOIDCRefreshValidatesRolesBeforeWriting(t *testing.T) {
	f := newRefreshFixture(t)
	u := f.update(f.target(t))
	u.ReplaceRoles, u.Roles = true, []string{"root"}
	if _, err := f.ApplyOIDCRefresh(context.Background(), u); err == nil {
		t.Fatal("an unknown role was accepted")
	}
	if got := f.roles(t, "idp"); !slices.Equal(got, []string{"admin", "user"}) {
		t.Errorf("idp roles = %v", got)
	}
}
