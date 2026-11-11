package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

// F-07: revocation follows the person.

func sha256Of(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func addKey(t *testing.T, s *Store, owner string, issuerVersion int64) (APIKeyRow, error) {
	t.Helper()
	k := APIKeyRow{ID: NewID(), OwnerID: owner, Name: "k", Hash: sha256Of(NewID()), Hint: "abcdefgh", Actions: []string{"read"},
		CreatedAt: 1, Scopes: []APIKeyScopeRow{{Kind: "user", UserID: owner}}, IssuerCredentialVersion: issuerVersion}
	return k, s.CreateAPIKey(context.Background(), k, nil)
}

func liveKeys(t *testing.T, s *Store, owner string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE owner_id = ? AND revoked_at IS NULL`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResetPasswordRevokesTheKeysInTheSameTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	other := newAccount(t, s, "bob", "user")
	for _, o := range []string{id, id, other} {
		if _, err := addKey(t, s, o, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: id, PasswordHash: "h2", NowMS: 9}); err != nil {
		t.Fatal(err)
	}
	if n := liveKeys(t, s, id); n != 0 {
		t.Errorf("%d keys survived the reset", n)
	}
	if n := liveKeys(t, s, other); n != 1 {
		t.Errorf("another user's keys = %d, want 1", n)
	}

	// keep_api_keys keeps them.
	if _, err := addKey(t, s, id, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: id, PasswordHash: "h3", KeepAPIKeys: true, NowMS: 10}); err != nil {
		t.Fatal(err)
	}
	if n := liveKeys(t, s, id); n != 1 {
		t.Errorf("keys after a reset with keep_api_keys = %d, want 1", n)
	}

	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: NewID(), PasswordHash: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestChangePasswordRevokesTheKeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	sess := addSession(t, s, id)
	if _, err := addKey(t, s, id, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 1, PasswordHash: "n", KeepSessionID: sess.ID, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	if n := liveKeys(t, s, id); n != 0 {
		t.Errorf("%d keys survived the password change", n)
	}
	// A refused change (stale version) revokes nothing.
	if _, err := addKey(t, s, id, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: id, ExpectedVersion: 1, PasswordHash: "m", NowMS: 6}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale change: %v", err)
	}
	if n := liveKeys(t, s, id); n != 1 {
		t.Errorf("a refused change revoked keys: %d", n)
	}
}

// The issuance race: a key request authenticated before a reset or change
// carries the old credential version and cannot insert after it.
func TestAKeyCreatedFromAStaleSessionIsRefused(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := newAccount(t, s, "ann", "user")
	u, _ := s.UserByID(ctx, id)
	stale := u.CredentialVersion
	if _, err := addKey(t, s, id, stale); err != nil {
		t.Fatalf("current version refused: %v", err)
	}
	if err := s.ResetPassword(ctx, ResetPasswordParams{UserID: id, PasswordHash: "h2", NowMS: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := addKey(t, s, id, stale); !errors.Is(err, ErrStaleIssuer) {
		t.Fatalf("a key from the session authenticated before the reset: %v", err)
	}
	if n := liveKeys(t, s, id); n != 0 {
		t.Errorf("live keys = %d", n)
	}
	u, _ = s.UserByID(ctx, id)
	if _, err := addKey(t, s, id, u.CredentialVersion); err != nil {
		t.Errorf("the new version was refused: %v", err)
	}
}

func TestAccountsWithoutGrantsHaveNoRoleAndLocalGrantsComeWithCredentials(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	// An IdP account whose IdP roles are gone has no role at all.
	uid := idpAccount(t, s, issA, "gone", "user")
	if _, _, err := s.ReplaceIDPRoles(ctx, uid, nil); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "" || !u.Active {
		t.Fatalf("account without grants: role %q active %v", u.Role, u.Active)
	}
	// A first local password is a local grant: the account keeps access
	// without the IdP.
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "h", NowMS: 2}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "user" {
		t.Errorf("role after a first password = %q", u.Role)
	}
	// Restore through the IdP does not touch the local grant, and losing the
	// IdP roles again leaves the local user grant.
	if _, _, err := s.ReplaceIDPRoles(ctx, uid, []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "admin" {
		t.Errorf("role with the IdP admin grant = %q", u.Role)
	}
	if _, _, err := s.ReplaceIDPRoles(ctx, uid, nil); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "user" {
		t.Errorf("role after losing the IdP roles with a local grant = %q", u.Role)
	}
}

func TestAnIdPAdminGrantNeedsARefreshToken(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := OIDCSyncParams{Issuer: "https://idp", Subject: "adm", ProviderID: "p", PreferredUsername: "adm",
		Roles: []string{"admin"}, CreateIfMissing: true, NowMS: 10}
	// Only the admin role and no token: nothing is created.
	res, err := s.SyncOIDCLogin(ctx, p)
	if err != nil || res.Found || !res.AdminWithheld {
		t.Fatalf("tokenless admin sign-in: %+v, %v", res, err)
	}
	if has, _ := s.HasUsers(ctx); has {
		t.Fatal("an account was created for a tokenless admin")
	}
	// admin + user and no token: the user role is granted, admin withheld.
	p.Roles = []string{"admin", "user"}
	res, err = s.SyncOIDCLogin(ctx, p)
	if err != nil || !res.Created || !res.AdminWithheld || res.User.Role != "user" {
		t.Fatalf("admin+user without a token: %+v, %v", res, err)
	}
	// With a token the admin grant is made.
	p.RefreshToken = "rt"
	p.NowMS = 20
	res, err = s.SyncOIDCLogin(ctx, p)
	if err != nil || res.AdminWithheld || res.User.Role != "admin" {
		t.Fatalf("admin with a token: %+v, %v", res, err)
	}
	// A later sign-in that brings no new token keeps the admin grant while a
	// token is stored.
	p.RefreshToken, p.NowMS = "", 30
	res, err = s.SyncOIDCLogin(ctx, p)
	if err != nil || res.AdminWithheld || res.User.Role != "admin" {
		t.Fatalf("admin with a stored token: %+v, %v", res, err)
	}
	// Once the token is gone (suspension), a tokenless sign-in withholds admin.
	if _, err := s.DB().Exec(`UPDATE external_identities SET refresh_token_enc = NULL`); err != nil {
		t.Fatal(err)
	}
	p.NowMS = 40
	res, err = s.SyncOIDCLogin(ctx, p)
	if err != nil || !res.AdminWithheld || res.User.Role != "user" {
		t.Fatalf("admin after the token is gone: %+v, %v", res, err)
	}
}

// The v0.5 database fixture: a tokenless IdP admin with an everything key, a
// token-holding IdP admin, a local user without role rows and an offboarded
// IdP-only account.
func TestMigration0019OnAV05Database(t *testing.T) {
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		newest = e.Name()
		if e.Name() >= "0019" {
			continue
		}
		b, err := hubMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	if newest < "0019" {
		t.Fatalf("newest migration = %s", newest)
	}
	dir := t.TempDir()
	old, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := old.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	user := func(id, name, pw string) {
		exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`, id, name, pw)
	}
	ident := func(id, uid, sub string, token []byte) {
		exec(`INSERT INTO external_identities (id, user_id, issuer, subject, provider_id, refresh_token_enc, created_at, last_login_at) VALUES (?, ?, 'https://idp', ?, 'p', ?, 1, 1)`,
			id, uid, sub, token)
	}
	user("local-noroles", "local", "hash")       // a v0.5 local user: effective user without rows
	user("idp-offboarded", "offboarded", "")     // IdP-only, all roles lost
	user("idp-admin-tokenless", "tokenless", "") // IdP admin, no refresh token
	user("idp-admin-token", "withtoken", "")     // IdP admin, usable token
	user("linked-admin", "linked", "hash")       // local admin + tokenless IdP admin
	ident("i1", "idp-offboarded", "s1", nil)
	ident("i2", "idp-admin-tokenless", "s2", nil)
	sealed, err := old.Box().Seal(identitiesTable, identitiesTokenCol, "i3", []byte("refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	ident("i3", "idp-admin-token", "s3", sealed)
	user("idp-admin-damaged", "damaged", "") // IdP admin whose stored token is garbage
	ident("i5", "idp-admin-damaged", "s5", []byte("sealed-but-not-decryptable-garbage-bytes-0123456789"))
	user("pw-idp", "pwidp", "hash") // v0.5 SetFirstPassword: password plus only IdP rows
	ident("i6", "pw-idp", "s6", nil)
	exec(`INSERT INTO user_roles VALUES ('pw-idp', 'user', 'idp'), ('idp-admin-damaged', 'admin', 'idp'), ('idp-admin-damaged', 'user', 'idp')`)
	ident("i4", "linked-admin", "s4", []byte{})
	exec(`INSERT INTO user_roles VALUES ('idp-admin-tokenless', 'admin', 'idp'), ('idp-admin-tokenless', 'user', 'idp'),
	  ('idp-admin-token', 'admin', 'idp'), ('linked-admin', 'admin', 'local'), ('linked-admin', 'admin', 'idp')`)
	exec(`INSERT INTO api_keys (id, owner_id, name, hash, hint, actions, created_at) VALUES ('key1', 'idp-admin-tokenless', 'all', zeroblob(32), 'abcdefgh', 'read,manage', 1)`)
	exec(`INSERT INTO api_key_scopes (key_id, kind, user_id, worker_id, session_name) VALUES ('key1', 'everything', '', '', '')`)
	_ = old.Close()

	cur, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	ctx := context.Background()
	role := func(id string) string {
		t.Helper()
		u, err := cur.UserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u.Role
	}
	// A local user without role rows keeps access through an explicit grant.
	if got := role("local-noroles"); got != "user" {
		t.Errorf("local user without role rows: %q, want user", got)
	}
	var src string
	if err := cur.DB().QueryRow(`SELECT source FROM user_roles WHERE user_id = 'local-noroles'`).Scan(&src); err != nil || src != "local" {
		t.Errorf("explicit grant = %q, %v", src, err)
	}
	// An IdP-only account an IdP offboarded stays without any grant.
	if got := role("idp-offboarded"); got != "" {
		t.Errorf("offboarded IdP account: %q, want no role", got)
	}
	// The tokenless IdP admin is suspended: the admin row is gone, the user
	// row stays, the account and its key stay.
	if got := role("idp-admin-tokenless"); got != "user" {
		t.Errorf("tokenless IdP admin: %q, want user", got)
	}
	// A usable token keeps the admin grant.
	if got := role("idp-admin-token"); got != "admin" {
		t.Errorf("IdP admin with a token: %q, want admin", got)
	}
	// Local grants are preserved.
	if got := role("linked-admin"); got != "admin" {
		t.Errorf("linked local admin: %q, want admin", got)
	}
	var idpAdmins int
	if err := cur.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = 'linked-admin' AND source = 'idp'`).Scan(&idpAdmins); err != nil || idpAdmins != 0 {
		t.Errorf("linked admin's tokenless IdP grant not suspended: %d, %v", idpAdmins, err)
	}
	if key, err := cur.APIKeyByID(ctx, "key1"); err != nil || key.RevokedAt != 0 {
		t.Errorf("the everything key must stay (inert through its owner's role): %+v, %v", key, err)
	}
	// A password account that has only IdP rows keeps local access.
	if got := role("pw-idp"); got != "user" {
		t.Errorf("password account with IdP rows only: %q", got)
	}
	var localN int
	if err := cur.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = 'pw-idp' AND source = 'local' AND role = 'user'`).Scan(&localN); err != nil || localN != 1 {
		t.Errorf("local grant for the password account = %d, %v", localN, err)
	}
	// An IdP admin with an undecryptable token is suspended at start.
	if got := role("idp-admin-damaged"); got != "user" {
		t.Errorf("damaged-token IdP admin: %q, want user (admin suspended)", got)
	}
	// One audit event per suspended admin grant.
	var n int
	if err := cur.DB().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'role.suspend' AND actor = 'system'`).Scan(&n); err != nil || n != 3 {
		t.Errorf("suspension audit events = %d, %v; want 3", n, err)
	}

	// Applying the migration SQL again leaves identical tables.
	snap := func() string {
		var out strings.Builder
		for _, q := range []string{`SELECT user_id||role||source FROM user_roles ORDER BY 1`, `SELECT id||action||target FROM audit_events ORDER BY 1`} {
			rows, err := cur.DB().Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var v string
				_ = rows.Scan(&v)
				out.WriteString(v + "\n")
			}
			_ = rows.Close()
			out.WriteString("--\n")
		}
		return out.String()
	}
	before := snap()
	mig, err := hubMigrations.ReadFile("migrations/0019_effective_grants.sql") // idempotence of 0019 itself; later migrations are not re-runnable by design
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := cur.DB().Exec(string(mig)); err != nil {
			t.Fatal(err)
		}
	}
	if after := snap(); after != before {
		t.Errorf("migration is not idempotent:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A damaged refresh token (it does not decrypt) or an empty plaintext is not a
// usable token: the IdP admin grant is suspended at start with an audit event,
// and a tokenless-looking sign-in withholds admin.
func TestStartupSuspendsIdPAdminsWithUnusableTokens(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	good, err := s.Box().Seal(identitiesTable, identitiesTokenCol, "ig", []byte("rt"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := s.Box().Seal(identitiesTable, identitiesTokenCol, "ie", []byte{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, name string
		tok      []byte
	}{{"ug", "good", good}, {"ud", "damaged", []byte("0123456789012345678901234567890123456789")}, {"ue", "empty", empty}} {
		exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at) VALUES (?, ?, '', 1, 1)`, c.id, c.name)
		exec(`INSERT INTO external_identities (id, user_id, issuer, subject, provider_id, refresh_token_enc, created_at, last_login_at) VALUES (?, ?, 'i', ?, 'p', ?, 1, 1)`,
			"i"+c.id[1:], c.id, c.name, c.tok)
		exec(`INSERT INTO user_roles VALUES (?, 'admin', 'idp'), (?, 'user', 'idp')`, c.id, c.id)
	}
	_ = s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for id, want := range map[string]string{"ug": "admin", "ud": "user", "ue": "user"} {
		u, err := s.UserByID(ctx, id)
		if err != nil || u.Role != want {
			t.Errorf("%s: role %q, %v; want %q", id, u.Role, err, want)
		}
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'role.suspend' AND target IN ('ud', 'ue')`).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit events = %d, %v; want 2", n, err)
	}
	// A sign-in without a new token does not restore admin on a damaged token.
	res, err := s.SyncOIDCLogin(ctx, OIDCSyncParams{Issuer: "i", Subject: "damaged", Roles: []string{"admin", "user"}, NowMS: 50})
	if err != nil || !res.AdminWithheld || res.User.Role != "user" {
		t.Errorf("sign-in with a damaged stored token: %+v, %v", res, err)
	}
}
