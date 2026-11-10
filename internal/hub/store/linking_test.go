package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

const (
	issA = "https://idp-a.example"
	issB = "https://idp-b.example"
)

// localWithEmail creates an active local account with a password and an
// e-mail address.
func localWithEmail(t *testing.T, s *Store, name, email, role string) string {
	t.Helper()
	id := NewID()
	err := s.CreateUser(context.Background(), CreateUserParams{
		User: UserRow{ID: id, Username: name, Email: email, PasswordHash: "hash-" + name, CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1},
		Role: role,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// idpAccount creates an account through an IdP sign-in (no password, no
// e-mail address) with the given IdP roles and returns its user id.
func idpAccount(t *testing.T, s *Store, issuer, subject string, roles ...string) string {
	t.Helper()
	res, err := s.SyncOIDCLogin(context.Background(), OIDCSyncParams{
		Issuer: issuer, Subject: subject, ProviderID: "p", PreferredUsername: "idp-" + subject,
		Roles: roles, CreateIfMissing: true, NowMS: 5,
	})
	if err != nil || !res.Created {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	return res.User.ID
}

func linkParams(userID, email, issuer, subject string) LinkIdentityParams {
	return LinkIdentityParams{UserID: userID, Email: email, Domain: "example.com", Issuer: issuer, Subject: subject, ProviderID: "p", NowMS: 50}
}

// openLinkTest opens a store whose provider "p" allows linking for
// example.com, the policy linkParams links under.
func openLinkTest(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	if err := s.PutLinkPolicy(context.Background(), PutLinkPolicyParams{
		Policy: LinkPolicyRow{ProviderID: "p", LinkingAllowed: true, Domains: []string{"example.com"}, UpdatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLinkIdentityChecksTheProviderPolicyInsideTheTransaction(t *testing.T) {
	ctx := context.Background()
	put := func(s *Store, p LinkPolicyRow) {
		t.Helper()
		p.ProviderID = "p"
		if err := s.PutLinkPolicy(ctx, PutLinkPolicyParams{Policy: p}); err != nil {
			t.Fatal(err)
		}
	}
	for name, change := range map[string]func(s *Store){
		"linking switched off": func(s *Store) { put(s, LinkPolicyRow{LinkingAllowed: false, Domains: []string{"example.com"}}) },
		"domain removed":       func(s *Store) { put(s, LinkPolicyRow{LinkingAllowed: true, Domains: []string{"corp.example"}}) },
		"policy row gone":      func(s *Store) { put(s, LinkPolicyRow{}) },
	} {
		t.Run(name, func(t *testing.T) {
			s := openLinkTest(t)
			id := localWithEmail(t, s, "ann", "ann@example.com", "user")
			change(s)
			if _, err := s.LinkIdentity(ctx, linkParams(id, "ann@example.com", issA, "s")); !errors.Is(err, ErrLinkRefused) {
				t.Fatalf("link under a revoked policy err = %v, want ErrLinkRefused", err)
			}
			if ok, _ := s.IdentityExists(ctx, issA, "s"); ok {
				t.Error("the identity was linked under a revoked policy")
			}
		})
	}
	// A provider with no policy at all, and another provider's policy, do not allow it.
	s := openLinkTest(t)
	id := localWithEmail(t, s, "ann", "ann@example.com", "user")
	p := linkParams(id, "ann@example.com", issA, "s")
	p.ProviderID = "other"
	if _, err := s.LinkIdentity(ctx, p); !errors.Is(err, ErrLinkRefused) {
		t.Fatalf("link through a provider without a policy err = %v", err)
	}
	p = linkParams(id, "ann@example.com", issA, "s")
	p.Domain = "Example.COM"
	if _, err := s.LinkIdentity(ctx, p); err != nil {
		t.Fatalf("link with a differently cased domain err = %v", err)
	}
}

func TestLinkCandidateByEmailMatchesOnlyTheEmailColumn(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	id := localWithEmail(t, s, "ann", "Ann@Example.com", "user")
	localWithEmail(t, s, "ann@other.example", "", "user") // a user name that looks like an address

	c, err := s.LinkCandidateByEmail(ctx, "ann@example.com")
	if err != nil || c.User.ID != id || c.AdminConsent || c.IssuerLinked {
		t.Fatalf("candidate = %+v, %v", c, err)
	}
	for _, e := range []string{"", "ann@other.example", "nobody@example.com"} {
		if _, err := s.LinkCandidateByEmail(ctx, e); !errors.Is(err, ErrNotFound) {
			t.Errorf("LinkCandidateByEmail(%q) err = %v, want ErrNotFound", e, err)
		}
	}
}

func TestLinkIdentityAttachesAndAuditsInOneTransaction(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	id := localWithEmail(t, s, "ann", "ann@example.com", "user")

	p := linkParams(id, "ANN@example.com", issA, "sub-1")
	p.OnLinked = func(ctx context.Context, ex Execer, row IdentityRow) error {
		return s.InsertAudit(ctx, ex, AuditRow{ID: NewID(), TS: 50, Actor: id, Action: "identity.link", Target: row.ID, Outcome: "success"})
	}
	row, err := s.LinkIdentity(ctx, p)
	if err != nil || row.UserID != id || row.Issuer != issA || row.Subject != "sub-1" || row.ID == "" {
		t.Fatalf("link = %+v, %v", row, err)
	}
	if ok, _ := s.IdentityExists(ctx, issA, "sub-1"); !ok {
		t.Error("identity does not exist after the link")
	}
	if got := auditActions(t, s); !slices.Equal(got, []string{"identity.link"}) {
		t.Errorf("audit = %v", got)
	}
	c, _ := s.LinkCandidateByEmail(ctx, "ann@example.com")
	if !c.IssuerLinked {
		t.Error("IssuerLinked is false after the link")
	}

	// A failing audit hook rolls the link back.
	id2 := localWithEmail(t, s, "bob", "bob@example.com", "user")
	p2 := linkParams(id2, "bob@example.com", issA, "sub-2")
	p2.OnLinked = func(context.Context, Execer, IdentityRow) error { return errors.New("audit down") }
	if _, err := s.LinkIdentity(ctx, p2); err == nil {
		t.Fatal("link succeeded although the audit hook failed")
	}
	if ok, _ := s.IdentityExists(ctx, issA, "sub-2"); ok {
		t.Error("the identity stayed after the audit failure")
	}
}

func TestLinkIdentityChecksTheConditionsInsideTheTransaction(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	ann := localWithEmail(t, s, "ann", "ann@example.com", "user")

	// Unknown account.
	if _, err := s.LinkIdentity(ctx, linkParams(NewID(), "x@example.com", issA, "s")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user err = %v", err)
	}
	// The e-mail address changed since the decision.
	if _, err := s.LinkIdentity(ctx, linkParams(ann, "other@example.com", issA, "s")); !errors.Is(err, ErrLinkRefused) {
		t.Errorf("changed e-mail err = %v", err)
	}
	// A second identity at the same issuer, or at another one: an account
	// holds at most one, so the refresh guard stays sound.
	if _, err := s.LinkIdentity(ctx, linkParams(ann, "ann@example.com", issA, "s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkIdentity(ctx, linkParams(ann, "ann@example.com", issA, "s2")); !errors.Is(err, ErrLinkRefused) {
		t.Errorf("second identity at the issuer err = %v", err)
	}
	if _, err := s.LinkIdentity(ctx, linkParams(ann, "ann@example.com", issB, "s2")); !errors.Is(err, ErrLinkRefused) {
		t.Errorf("identity at another issuer err = %v, want ErrLinkRefused", err)
	}
	// The identity is taken by another account.
	bob := localWithEmail(t, s, "bob", "bob@example.com", "user")
	if _, err := s.LinkIdentity(ctx, linkParams(bob, "bob@example.com", issA, "s1")); !errors.Is(err, ErrIdentityTaken) {
		t.Errorf("taken identity err = %v", err)
	}
	// A deactivated account.
	carol := localWithEmail(t, s, "carol", "carol@example.com", "user")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: carol, NowMS: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkIdentity(ctx, linkParams(carol, "carol@example.com", issA, "s3")); !errors.Is(err, ErrLinkRefused) {
		t.Errorf("deactivated account err = %v", err)
	}
}

func TestLinkIdentityIntoAnAdminNeedsTheConsent(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	adm := localWithEmail(t, s, "root", "root@example.com", "admin")

	if _, err := s.LinkIdentity(ctx, linkParams(adm, "root@example.com", issA, "s")); !errors.Is(err, ErrLinkRefused) {
		t.Fatalf("link into an admin without consent err = %v", err)
	}
	if c, _ := s.LinkCandidateByEmail(ctx, "root@example.com"); c.AdminConsent {
		t.Error("consent is on by default")
	}

	changed, err := s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: adm, Granted: true, ActorID: adm, NowMS: 9,
		Audit: &AuditRow{ID: NewID(), TS: 9, Actor: adm, Action: "admin.link_consent", Target: adm, Outcome: "success"}})
	if err != nil || !changed {
		t.Fatalf("grant = %v, %v", changed, err)
	}
	if c, _ := s.LinkCandidateByEmail(ctx, "root@example.com"); !c.AdminConsent {
		t.Error("consent not visible to the candidate lookup")
	}
	if ok, _ := s.AdminLinkConsent(ctx, adm); !ok {
		t.Error("AdminLinkConsent = false after the grant")
	}
	if _, err := s.LinkIdentity(ctx, linkParams(adm, "root@example.com", issA, "s")); err != nil {
		t.Fatalf("link with consent err = %v", err)
	}

	// Granting again changes and audits nothing.
	changed, err = s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: adm, Granted: true, ActorID: adm, NowMS: 10,
		Audit: &AuditRow{ID: NewID(), TS: 10, Actor: adm, Action: "admin.link_consent", Target: adm, Outcome: "success"}})
	if err != nil || changed {
		t.Errorf("second grant = %v, %v", changed, err)
	}
	if got := auditActions(t, s); !slices.Equal(got, []string{"admin.link_consent"}) {
		t.Errorf("audit = %v", got)
	}
	// Withdrawing it again forbids linking.
	if changed, err = s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: adm, Granted: false, NowMS: 11}); err != nil || !changed {
		t.Fatalf("withdraw = %v, %v", changed, err)
	}
	if _, err := s.LinkIdentity(ctx, linkParams(adm, "root@example.com", issB, "s")); !errors.Is(err, ErrLinkRefused) {
		t.Errorf("link after withdrawal err = %v", err)
	}
}

func TestAdminLinkConsentNeedsAnAdminAccount(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	user := localWithEmail(t, s, "ann", "ann@example.com", "user")
	if _, err := s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: user, Granted: true, NowMS: 1}); !errors.Is(err, ErrNotAdminAccount) {
		t.Errorf("consent for a user err = %v", err)
	}
	if _, err := s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: NewID(), Granted: true, NowMS: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("consent for an unknown account err = %v", err)
	}
}

func TestDetachIdentityDropsIdPRolesRevokesSessionsAndKeepsTheAccount(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	// A local admin that also gets the idp user role through a link.
	id := localWithEmail(t, s, "ann", "ann@example.com", "user")
	row, err := s.LinkIdentity(ctx, linkParams(id, "ann@example.com", issA, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceIDPRoles(ctx, id, []string{"user"}); err != nil {
		t.Fatal(err)
	}
	addSession(t, s, id)
	before, _ := s.UserByID(ctx, id)

	got, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: row.ID, NowMS: 60,
		Audit: &AuditRow{ID: NewID(), TS: 60, Actor: "adm", Action: "identity.detach", Target: row.ID, Outcome: "success"}})
	if err != nil || got.ID != row.ID {
		t.Fatalf("detach = %+v, %v", got, err)
	}
	if ok, _ := s.IdentityExists(ctx, issA, "s1"); ok {
		t.Error("identity still exists")
	}
	after, err := s.UserByID(ctx, id)
	if err != nil || !after.Active || after.CredentialVersion != before.CredentialVersion+1 {
		t.Errorf("account after detach = %+v, %v", after, err)
	}
	if n := sessionCount(t, s, id); n != 0 {
		t.Errorf("sessions = %d, want 0", n)
	}
	var idp int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'idp'`, id).Scan(&idp); err != nil || idp != 0 {
		t.Errorf("idp roles = %d, %v", idp, err)
	}
	var local int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'local'`, id).Scan(&local); err != nil || local != 1 {
		t.Errorf("local roles = %d, %v", local, err)
	}
	if got := auditActions(t, s); !slices.Equal(got, []string{"identity.detach"}) {
		t.Errorf("audit = %v", got)
	}
	if _, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: row.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("second detach err = %v", err)
	}
}

func TestDetachRefusesTheOnlySignInMethod(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	uid := idpAccount(t, s, issA, "only", "user")
	ids, _ := s.ListIdentities(ctx, uid)
	if len(ids) != 1 {
		t.Fatalf("identities = %+v", ids)
	}
	if _, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: ids[0].ID}); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("detach of the only identity err = %v", err)
	}
	if ok, _ := s.IdentityExists(ctx, issA, "only"); !ok {
		t.Fatal("the refused detach removed the identity")
	}

	// With a local password it is allowed.
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "h", NowMS: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: ids[0].ID, NowMS: 8}); err != nil {
		t.Fatalf("detach with a password err = %v", err)
	}

	// With a second identity it is allowed too.
	uid2 := idpAccount(t, s, issA, "two", "user")
	row, err := s.LinkIdentity(ctx, LinkIdentityParams{UserID: uid2, Email: "x@example.com", Domain: "example.com", Issuer: issB, Subject: "b", ProviderID: "p", NowMS: 1})
	if err == nil {
		t.Fatalf("link without an e-mail on the account succeeded: %+v", row)
	}
}

func TestDetachRefusesToDemoteTheLastAdmin(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	// An IdP admin with a local password and a second identity: the only admin.
	uid := idpAccount(t, s, issA, "root", "admin")
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "h", NowMS: 2}); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.ListIdentities(ctx, uid)
	if _, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: ids[0].ID, NowMS: 3}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("detach of the last admin's identity err = %v", err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "admin" {
		t.Errorf("role = %q after the refused detach", u.Role)
	}
	// Another admin exists: now it works.
	localWithEmail(t, s, "second", "second@example.com", "admin")
	if _, err := s.DetachIdentity(ctx, DetachIdentityParams{IdentityID: ids[0].ID, NowMS: 4}); err != nil {
		t.Fatalf("detach with another admin err = %v", err)
	}
	if u, _ := s.UserByID(ctx, uid); u.Role != "user" {
		t.Errorf("role = %q after the detach, want user", u.Role)
	}
}

func TestSetFirstPassword(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	uid := idpAccount(t, s, issA, "sub", "user")
	addSession(t, s, uid)
	other := localWithEmail(t, s, "bob", "bob@example.com", "user")
	_ = other

	// An address that another account uses, or that is another account's user
	// name, is refused and nothing changes.
	for _, e := range []string{"bob@example.com", "bob"} {
		err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "h", Email: e, NowMS: 2})
		if !errors.Is(err, ErrEmailTaken) {
			t.Errorf("e-mail %q err = %v, want ErrEmailTaken", e, err)
		}
	}
	if u, _ := s.UserByID(ctx, uid); u.PasswordHash != "" || u.Email != "" {
		t.Fatalf("a refused call changed the account: %+v", u)
	}

	err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "the-hash", Email: "Idp.User@example.com", NowMS: 3,
		Audit: &AuditRow{ID: NewID(), TS: 3, Actor: "adm", Action: "user.set_password", Target: uid, Outcome: "success"}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.UserByID(ctx, uid)
	if u.PasswordHash != "the-hash" || !u.MustChangePassword || u.Email != "Idp.User@example.com" {
		t.Errorf("account = %+v", u)
	}
	if n := sessionCount(t, s, uid); n != 1 {
		t.Errorf("sessions = %d; a first password must not end the IdP session", n)
	}
	// Sign-in by e-mail resolves to the account (case-insensitive).
	if got, err := s.UserByIdentifier(ctx, "idp.user@example.com"); err != nil || got.ID != uid {
		t.Errorf("UserByIdentifier = %+v, %v", got, err)
	}
	if got := auditActions(t, s); !slices.Equal(got, []string{"user.set_password"}) {
		t.Errorf("audit = %v", got)
	}

	// An account with a password uses a reset instead.
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: uid, PasswordHash: "x", NowMS: 4}); !errors.Is(err, ErrHasLocalPassword) {
		t.Errorf("second set err = %v", err)
	}
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: NewID(), PasswordHash: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user err = %v", err)
	}
	dead := idpAccount(t, s, issA, "dead", "user")
	if _, err := s.DeactivateUser(ctx, DeactivateUserParams{UserID: dead, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: dead, PasswordHash: "x"}); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated user err = %v", err)
	}
	// An account that has another address keeps it.
	withAddr := idpAccount(t, s, issA, "addr", "user")
	if _, err := s.db.Exec(`UPDATE users SET email = 'has@example.com' WHERE id = ?`, withAddr); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: withAddr, PasswordHash: "x", Email: "new@example.com"}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("replacing an address err = %v", err)
	}
	if err := s.SetFirstPassword(ctx, SetFirstPasswordParams{UserID: withAddr, PasswordHash: "x", Email: "HAS@example.com", NowMS: 6}); err != nil {
		t.Errorf("same address err = %v", err)
	}
}

func TestListIdentities(t *testing.T) {
	s := openLinkTest(t)
	ctx := context.Background()
	a := idpAccount(t, s, issA, "a", "user")
	b := idpAccount(t, s, issA, "b", "user")
	all, err := s.ListIdentities(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	one, err := s.ListIdentities(ctx, a)
	if err != nil || len(one) != 1 || one[0].UserID != a || one[0].Subject != "a" || one[0].ProviderID != "p" {
		t.Fatalf("one = %+v, %v", one, err)
	}
	if none, err := s.ListIdentities(ctx, NewID()); err != nil || len(none) != 0 {
		t.Errorf("unknown user = %+v, %v", none, err)
	}
	_ = b
}

func TestLinkPolicyDefaultsOffAndReplacesDomains(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	got, err := s.GetLinkPolicy(ctx, "acme")
	if err != nil || got.ProviderID != "acme" || got.LinkingAllowed || got.AutoCreate || len(got.Domains) != 0 {
		t.Fatalf("default policy = %+v, %v", got, err)
	}
	put := func(p LinkPolicyRow) {
		t.Helper()
		if err := s.PutLinkPolicy(ctx, PutLinkPolicyParams{Policy: p,
			Audit: &AuditRow{ID: NewID(), TS: 1, Actor: "adm", Action: "oidc.link_policy_set", Target: p.ProviderID, Outcome: "success"}}); err != nil {
			t.Fatal(err)
		}
	}
	put(LinkPolicyRow{ProviderID: "acme", LinkingAllowed: true, Domains: []string{" @Example.COM ", "corp.example", "example.com", ""}, UpdatedAt: 5})
	want := LinkPolicyRow{ProviderID: "acme", LinkingAllowed: true, Domains: []string{"corp.example", "example.com"}, UpdatedAt: 5}
	if got, _ = s.GetLinkPolicy(ctx, "acme"); !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}
	put(LinkPolicyRow{ProviderID: "other", AutoCreate: true, UpdatedAt: 6})
	put(LinkPolicyRow{ProviderID: "acme", AutoCreate: true, Domains: []string{"only.example"}, UpdatedAt: 7})
	got, _ = s.GetLinkPolicy(ctx, "acme")
	if got.LinkingAllowed || !got.AutoCreate || !slices.Equal(got.Domains, []string{"only.example"}) {
		t.Fatalf("replaced policy = %+v", got)
	}
	all, err := s.ListLinkPolicies(ctx)
	if err != nil || len(all) != 2 || all[0].ProviderID != "acme" || all[1].ProviderID != "other" {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if n := len(auditActions(t, s)); n != 3 {
		t.Errorf("audit rows = %d, want 3", n)
	}
	if err := s.PutLinkPolicy(ctx, PutLinkPolicyParams{}); err == nil {
		t.Error("a policy without a provider id was stored")
	}
}

func TestDeletingAProviderRemovesItsLinkPolicy(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.PutOIDCProvider(ctx, provider("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLinkPolicy(ctx, PutLinkPolicyParams{Policy: LinkPolicyRow{ProviderID: "acme", LinkingAllowed: true, Domains: []string{"example.com"}}}); err != nil {
		t.Fatal(err)
	}
	// The policy of the settings provider (not in oidc_providers) is not touched.
	if err := s.PutLinkPolicy(ctx, PutLinkPolicyParams{Policy: LinkPolicyRow{ProviderID: "default", LinkingAllowed: true}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteOIDCProvider(ctx, "acme"); err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if got, _ := s.GetLinkPolicy(ctx, "acme"); got.LinkingAllowed || len(got.Domains) != 0 {
		t.Errorf("policy survived the provider: %+v", got)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM oidc_link_domains WHERE provider_id = 'acme'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("domains = %d, %v", n, err)
	}
	if got, _ := s.GetLinkPolicy(ctx, "default"); !got.LinkingAllowed {
		t.Error("the policy of the settings provider was removed")
	}
}

func TestAdminLinkConsentGoesWithTheAccount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	other := localWithEmail(t, s, "other", "o@example.com", "admin")
	adm := localWithEmail(t, s, "root", "root@example.com", "admin")
	for _, id := range []string{adm, other} {
		if _, err := s.SetAdminLinkConsent(ctx, AdminLinkConsentParams{UserID: id, Granted: true, ActorID: id, NowMS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, adm); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_link_consent`).Scan(&n); err != nil || n != 1 {
		t.Errorf("consent rows = %d, %v", n, err)
	}
}
