package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// ---- the decision table -------------------------------------------------------

func TestDecideLinkTable(t *testing.T) {
	allowed := LinkPolicy{LinkingAllowed: true, Domains: []string{"example.com", "Corp.Example"}}
	ok := LinkTarget{Active: true}
	cases := []struct {
		name string
		in   LinkInput
		want LinkReason
	}{
		{"all conditions hold", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: ok}, ReasonLinked},
		{"allow-list entries are normalised", LinkInput{Policy: allowed, Domain: "corp.example", EmailVerified: true, Target: ok}, ReasonLinked},
		{"provider flag off", LinkInput{Policy: LinkPolicy{Domains: []string{"example.com"}}, Domain: "example.com", EmailVerified: true, Target: ok}, ReasonLinkingOff},
		{"zero policy", LinkInput{Domain: "example.com", EmailVerified: true, Target: ok}, ReasonLinkingOff},
		{"auto-create does not allow linking", LinkInput{Policy: LinkPolicy{AutoCreate: true, Domains: []string{"example.com"}}, Domain: "example.com", EmailVerified: true, Target: ok}, ReasonLinkingOff},
		{"e-mail not verified", LinkInput{Policy: allowed, Domain: "example.com", Target: ok}, ReasonEmailUnverified},
		{"domain not on the list", LinkInput{Policy: allowed, Domain: "evil.example", EmailVerified: true, Target: ok}, ReasonDomainNotAllowed},
		{"subdomain is not the domain", LinkInput{Policy: allowed, Domain: "sub.example.com", EmailVerified: true, Target: ok}, ReasonDomainNotAllowed},
		{"parent domain is not the domain", LinkInput{Policy: LinkPolicy{LinkingAllowed: true, Domains: []string{"sub.example.com"}}, Domain: "example.com", EmailVerified: true, Target: ok}, ReasonDomainNotAllowed},
		{"empty allow-list", LinkInput{Policy: LinkPolicy{LinkingAllowed: true}, Domain: "example.com", EmailVerified: true, Target: ok}, ReasonDomainNotAllowed},
		{"empty domain", LinkInput{Policy: LinkPolicy{LinkingAllowed: true, Domains: []string{""}}, EmailVerified: true, Target: ok}, ReasonDomainNotAllowed},
		{"deactivated account", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: LinkTarget{}}, ReasonInactive},
		{"another identity at the issuer", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: LinkTarget{Active: true, IssuerLinked: true}}, ReasonIssuerLinked},
		{"admin without consent", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: LinkTarget{Active: true, Admin: true}}, ReasonAdminNoConsent},
		{"admin with consent", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: LinkTarget{Active: true, Admin: true, AdminConsent: true}}, ReasonLinked},
		{"consent of a plain user changes nothing", LinkInput{Policy: allowed, Domain: "example.com", EmailVerified: true, Target: LinkTarget{Active: true, AdminConsent: true}}, ReasonLinked},
		// The first failing condition is the one reported.
		{"flag before verification", LinkInput{Policy: LinkPolicy{Domains: []string{"example.com"}}, Domain: "example.com", Target: ok}, ReasonLinkingOff},
		{"verification before domain", LinkInput{Policy: allowed, Domain: "evil.example", Target: ok}, ReasonEmailUnverified},
		{"domain before admin consent", LinkInput{Policy: allowed, Domain: "evil.example", EmailVerified: true, Target: LinkTarget{Active: true, Admin: true}}, ReasonDomainNotAllowed},
	}
	for _, c := range cases {
		if got := DecideLink(c.in); got != c.want {
			t.Errorf("%s: DecideLink = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDecideLinkExhaustive(t *testing.T) {
	// Every combination of the boolean inputs: the link is made exactly when
	// all of the conditions hold, and never otherwise.
	for mask := 0; mask < 1<<7; mask++ {
		bit := func(i int) bool { return mask&(1<<i) != 0 }
		in := LinkInput{
			Policy:        LinkPolicy{LinkingAllowed: bit(0)},
			EmailVerified: bit(1),
			Target:        LinkTarget{Active: bit(2), IssuerLinked: bit(3), Admin: bit(4), AdminConsent: bit(5)},
			Domain:        "example.com",
		}
		if bit(6) {
			in.Policy.Domains = []string{"example.com"}
		}
		want := bit(0) && bit(1) && bit(6) && bit(2) && !bit(3) && (!bit(4) || bit(5))
		if got := DecideLink(in) == ReasonLinked; got != want {
			t.Errorf("mask %07b: linked = %v, want %v (%+v)", mask, got, want, in)
		}
	}
}

func TestParseEmailClaim(t *testing.T) {
	good := map[string][2]string{
		"ann@example.com":        {"ann@example.com", "example.com"},
		"  Ann@Example.COM ":     {"ann@example.com", "example.com"},
		"first.last+tag@a.b.org": {"first.last+tag@a.b.org", "a.b.org"},
	}
	for in, want := range good {
		email, domain, ok := ParseEmailClaim(map[string]any{"email": in})
		if !ok || email != want[0] || domain != want[1] {
			t.Errorf("ParseEmailClaim(%q) = %q, %q, %v", in, email, domain, ok)
		}
	}
	bad := []any{nil, 42, true, []any{"a@b.c"}, "", "   ", "no-at-sign", "@example.com", "ann@", "a@b@example.com",
		"ann @example.com", "Ann <ann@example.com>", "ann@example.com, bob@example.com", "ann@exa\x00mple.com", "a\nb@example.com",
		"ann@example.com;x", "\"ann\"@example.com", "ann@example.com\u00a0x", "ann@\xffexample.com", strings.Repeat("a", 250) + "@example.com"}
	for _, in := range bad {
		if e, d, ok := ParseEmailClaim(map[string]any{"email": in}); ok {
			t.Errorf("ParseEmailClaim(%#v) = %q, %q, true; want false", in, e, d)
		}
	}
	// A Unicode fold must not move the "@": U+0130 and U+212A shrink when
	// lower-cased, which once made the checked domain a suffix of the real one
	// and the slice go out of range.
	for _, in := range []string{"\u0130@evil.com", "\u212a\u212a\u212a\u212a@a.b", "a\u0130@x.example"} {
		email, domain, ok := ParseEmailClaim(map[string]any{"email": in})
		if !ok || !strings.HasSuffix(email, "@"+domain) || domain != in[strings.IndexByte(in, '@')+1:] {
			t.Errorf("ParseEmailClaim(%q) = %q, %q, %v: the domain is not the one after the @", in, email, domain, ok)
		}
	}
	if e, _, ok := ParseEmailClaim(map[string]any{"email": "\u0130@evil.com"}); ok && e == "i@evil.com" {
		t.Error("U+0130 was folded into the ASCII address i@evil.com")
	}
	for _, in := range []string{"ann@ex\u00e4mple.com", "ann@example.\u212aom"} {
		if e, d, ok := ParseEmailClaim(map[string]any{"email": in}); ok {
			t.Errorf("ParseEmailClaim(%q) = %q, %q, true; a non-ASCII domain must be refused", in, e, d)
		}
	}
	if _, _, ok := ParseEmailClaim(map[string]any{}); ok {
		t.Error("a missing claim parsed")
	}
	if _, _, ok := ParseEmailClaim(nil); ok {
		t.Error("nil claims parsed")
	}
}

func TestEmailVerifiedIsOnlyTheBooleanTrue(t *testing.T) {
	for v, want := range map[any]bool{true: true, false: false, "true": false, "True": false, 1: false, 1.0: false, nil: false} {
		if got := emailVerified(map[string]any{"email_verified": v}); got != want {
			t.Errorf("email_verified %#v = %v, want %v", v, got, want)
		}
	}
	if emailVerified(map[string]any{}) {
		t.Error("a missing claim counts as verified")
	}
}

// ---- the sign-in with a linker ------------------------------------------------

type linkHub struct {
	*testHub
	localID string
}

// newLinkHub starts a Hub with the fake issuer and a Linker, a local account
// "ann" (password, e-mail ann@example.com) and an IdP that grants the user
// role and says ann@example.com, verified. The provider's policy is the
// given one.
func newLinkHub(t *testing.T, pol store.LinkPolicyRow) *linkHub {
	t.Helper()
	h := &linkHub{testHub: newTestHub(t, nil)}
	h.svc.Linker = &Linker{Store: h.st, Audit: h.aud, Log: h.svc.Log}
	h.localID = h.addLocal("ann", "ann@example.com", "user")
	h.policy(pol)
	h.groups("minion-users")
	h.claims("ann@example.com", true)
	return h
}

func (h *linkHub) addLocal(name, email, role string) string {
	h.t.Helper()
	id := store.NewID()
	err := h.st.CreateUser(context.Background(), store.CreateUserParams{
		User: store.UserRow{ID: id, Username: name, Email: email, PasswordHash: "local-hash-" + name, CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1},
		Role: role,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *linkHub) policy(p store.LinkPolicyRow) {
	h.t.Helper()
	p.ProviderID = "default"
	if err := h.st.PutLinkPolicy(context.Background(), store.PutLinkPolicyParams{Policy: p}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *linkHub) claims(email string, verified any) {
	h.idp.set(func(f *fakeIdP) {
		f.claims["email"] = email
		f.claims["email_verified"] = verified
	})
}

func (h *linkHub) identitiesOf(userID string) []store.IdentityRow {
	h.t.Helper()
	rows, err := h.st.ListIdentities(context.Background(), userID)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *linkHub) passwordHash(userID string) string {
	h.t.Helper()
	u, err := h.st.UserByID(context.Background(), userID)
	if err != nil {
		h.t.Fatal(err)
	}
	return u.PasswordHash
}

var linkingOn = store.LinkPolicyRow{LinkingAllowed: true, Domains: []string{"example.com"}}

func TestVerifiedAllowedEmailLinksTheIdentityToTheLocalAccount(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	r := h.signIn()
	if r.status != http.StatusSeeOther {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	sess, ok := h.sessionOf(r)
	if !ok || sess.UserID != h.localID || sess.Username != "ann" {
		t.Fatalf("session = %+v, %v; want the local account", sess, ok)
	}
	if n := h.countUsers(); n != 1 {
		t.Errorf("users = %d, want 1: no new account", n)
	}
	ids := h.identitiesOf(h.localID)
	if len(ids) != 1 || ids[0].Issuer != h.idp.Issuer() || ids[0].Subject != "user-1" || ids[0].ProviderID != "default" {
		t.Fatalf("identities = %+v", ids)
	}
	if got := h.idpRoles(h.localID); !slices.Equal(got, []string{"user"}) {
		t.Errorf("idp roles = %v", got)
	}
	// The local password is untouched: both ways in stay open (R67).
	if got := h.passwordHash(h.localID); got != "local-hash-ann" {
		t.Errorf("password hash = %q", got)
	}
	if !h.aud.has(AuditActionIdentityLink, OutcomeSuccess) {
		t.Errorf("no identity.link audit: %+v", h.aud.all())
	}
	for _, e := range h.aud.all() {
		if e.Action == AuditActionIdentityLink && (e.Target == "" || strings.Contains(e.Target+e.Actor, "@")) {
			t.Errorf("link audit carries an address or no target: %+v", e)
		}
	}
}

func TestLaterSignInsMatchByIssuerAndSubjectOnly(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	if r := h.signIn(); r.status != http.StatusSeeOther {
		t.Fatalf("first sign-in = %d\n%s", r.status, r.body)
	}
	// Linking is turned off, the domain is gone and the IdP now sends another,
	// unverified address: the identity still signs in to the same account.
	h.policy(store.LinkPolicyRow{})
	h.claims("somebody.else@evil.example", false)
	r := h.signIn()
	sess, ok := h.sessionOf(r)
	if r.status != http.StatusSeeOther || !ok || sess.UserID != h.localID {
		t.Fatalf("second sign-in = %d %+v\n%s", r.status, sess, r.body)
	}
	if n := h.countUsers(); n != 1 {
		t.Errorf("users = %d", n)
	}
	if n := len(h.identitiesOf("")); n != 1 {
		t.Errorf("identities = %d", n)
	}
	links := 0
	for _, e := range h.aud.all() {
		if e.Action == AuditActionIdentityLink {
			links++
		}
	}
	if links != 1 {
		t.Errorf("link audit rows = %d, want exactly the first one", links)
	}
}

func TestNoLinkAndAnExplanationWhenAConditionFails(t *testing.T) {
	cases := []struct {
		name   string
		policy store.LinkPolicyRow
		email  string
		verify any
		want   string
	}{
		{"linking not allowed for the provider", store.LinkPolicyRow{Domains: []string{"example.com"}}, "ann@example.com", true, "turned off"},
		{"no policy at all", store.LinkPolicyRow{}, "ann@example.com", true, "turned off"},
		{"e-mail not verified", linkingOn, "ann@example.com", false, "not verified"},
		{"e-mail_verified is a string", linkingOn, "ann@example.com", "true", "not verified"},
		{"e-mail_verified missing", linkingOn, "ann@example.com", nil, "not verified"},
		{"domain not on the allow-list", store.LinkPolicyRow{LinkingAllowed: true, Domains: []string{"corp.example"}}, "ann@example.com", true, "e-mail domain may not be linked"},
		{"empty allow-list", store.LinkPolicyRow{LinkingAllowed: true}, "ann@example.com", true, "e-mail domain may not be linked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLinkHub(t, c.policy)
			if c.verify == nil {
				h.idp.set(func(f *fakeIdP) { f.claims["email"] = c.email; delete(f.claims, "email_verified") })
			} else {
				h.claims(c.email, c.verify)
			}
			r := h.signIn()
			if r.status != http.StatusForbidden || r.cookie("minion_session") != nil {
				t.Fatalf("callback = %d, want 403 and no session\n%s", r.status, r.body)
			}
			if !strings.Contains(r.body, "Account not linked") || !strings.Contains(r.body, "No account was created") ||
				!strings.Contains(strings.ReplaceAll(r.body, "&#39;", "'"), c.want) {
				t.Errorf("explanation does not say %q:\n%s", c.want, r.body)
			}
			if strings.Contains(r.body, "ann@example.com") {
				t.Errorf("the page repeats the address:\n%s", r.body)
			}
			if n := h.countUsers(); n != 1 {
				t.Errorf("users = %d: a refused link created an account", n)
			}
			if n := len(h.identitiesOf("")); n != 0 {
				t.Errorf("identities = %d, want 0", n)
			}
			if !h.aud.has(AuditActionIdentityLink, OutcomeDenied) {
				t.Errorf("no denied audit: %+v", h.aud.all())
			}
			if h.passwordHash(h.localID) != "local-hash-ann" {
				t.Error("the local account was changed")
			}
		})
	}
}

func TestAutoCreateMakesASeparateAccountWhenLinkingIsRefused(t *testing.T) {
	h := newLinkHub(t, store.LinkPolicyRow{AutoCreate: true, Domains: []string{"example.com"}}) // linking itself is off
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "ann" })
	r := h.signIn()
	sess, ok := h.sessionOf(r)
	if r.status != http.StatusSeeOther || !ok {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	if sess.UserID == h.localID {
		t.Fatal("the sign-in reached the local account")
	}
	if n := h.countUsers(); n != 2 {
		t.Errorf("users = %d, want 2", n)
	}
	if sess.Username == "ann" {
		t.Errorf("the new account took the local user name: %+v", sess)
	}
	if n := len(h.identitiesOf(h.localID)); n != 0 {
		t.Errorf("the local account got %d identities", n)
	}
	if n := len(h.identitiesOf(sess.UserID)); n != 1 {
		t.Errorf("the new account has %d identities", n)
	}
	if !h.aud.has(AuditActionIdentityLink, OutcomeDenied) {
		t.Error("the refused link was not audited")
	}
}

func TestLinkingIntoAnAdminAccountNeedsThePanelConsent(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	adm := h.addLocal("root", "root@example.com", "admin")
	h.claims("root@example.com", true)

	r := h.signIn()
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "administrator account") {
		t.Fatalf("without consent: %d\n%s", r.status, r.body)
	}
	if n := len(h.identitiesOf(adm)); n != 0 || h.countUsers() != 2 {
		t.Fatalf("identities = %d, users = %d after the refusal", n, h.countUsers())
	}

	if _, err := h.st.SetAdminLinkConsent(context.Background(), store.AdminLinkConsentParams{UserID: adm, Granted: true, ActorID: adm, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	r = h.signIn()
	sess, ok := h.sessionOf(r)
	if r.status != http.StatusSeeOther || !ok || sess.UserID != adm {
		t.Fatalf("with consent: %d %+v\n%s", r.status, sess, r.body)
	}
	if n := len(h.identitiesOf(adm)); n != 1 {
		t.Errorf("identities = %d, want 1", n)
	}

	// A plain user does not need it (it was linked above without consent).
	if n := len(h.identitiesOf(h.localID)); n != 0 {
		t.Errorf("the other account got %d identities", n)
	}
}

func TestNoLinkWhenNoLocalAccountHasTheAddress(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	h.claims("new.person@example.com", true)
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "newbie" })
	r := h.signIn()
	sess, ok := h.sessionOf(r)
	if r.status != http.StatusSeeOther || !ok || sess.UserID == h.localID || sess.Username != "newbie" {
		t.Fatalf("callback = %d %+v\n%s", r.status, sess, r.body)
	}
	if n := h.countUsers(); n != 2 {
		t.Errorf("users = %d", n)
	}
}

func TestNoLinkWithoutAnAddressOrWithAMalformedOne(t *testing.T) {
	for _, email := range []string{"", "ann", "ann@", "Ann <ann@example.com>"} {
		h := newLinkHub(t, linkingOn)
		h.claims(email, true)
		r := h.signIn()
		if r.status != http.StatusSeeOther {
			t.Fatalf("email %q: callback = %d\n%s", email, r.status, r.body)
		}
		if n := len(h.identitiesOf(h.localID)); n != 0 {
			t.Errorf("email %q linked the local account", email)
		}
	}
}

func TestAUserNameNeverLinks(t *testing.T) {
	// Another account's user name equals the claim (CreateUser refuses that
	// now; the row is written directly): only the e-mail column links.
	h := newLinkHub(t, linkingOn)
	if _, err := h.st.DB().Exec(`INSERT INTO users (id, username, email, password_hash, credential_version, active, created_at, updated_at)
VALUES (?, 'ann@example.com', '', 'x', 1, 1, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, store.NewID()); err != nil {
		t.Fatal(err)
	}
	h.claims("ann@example.com", true)
	r := h.signIn()
	sess, _ := h.sessionOf(r)
	if r.status != http.StatusSeeOther || sess == nil || sess.UserID != h.localID {
		t.Fatalf("callback = %d %+v", r.status, sess)
	}
	if n := len(h.identitiesOf(h.localID)); n != 1 || h.countUsers() != 2 {
		t.Errorf("identities = %d, users = %d", n, h.countUsers())
	}
}

func TestNoLinkWhenTheRolesGiveNoAccess(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	h.groups("some-other-group")
	r := h.signIn()
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "No access") {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	if n := len(h.identitiesOf("")); n != 0 {
		t.Errorf("identities = %d: an identity without access was linked", n)
	}
	if h.aud.has(AuditActionIdentityLink, OutcomeDenied) || h.aud.has(AuditActionIdentityLink, OutcomeSuccess) {
		t.Errorf("link audit for a sign-in without access: %+v", h.aud.all())
	}
}

func TestNoLinkIntoADeactivatedAccount(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	if _, err := h.st.DeactivateUser(context.Background(), store.DeactivateUserParams{UserID: h.localID, NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	r := h.signIn()
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "deactivated") {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	if n := len(h.identitiesOf("")); n != 0 || h.countUsers() != 1 {
		t.Errorf("identities = %d users = %d", n, h.countUsers())
	}
}

func TestASecondSubjectAtTheSameIssuerDoesNotTakeTheAccountOver(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	if r := h.signIn(); r.status != http.StatusSeeOther {
		t.Fatalf("first sign-in = %d\n%s", r.status, r.body)
	}
	// Another account of the same IdP carries the same, verified address.
	h.idp.set(func(f *fakeIdP) { f.subject = "user-2" })
	r := h.signIn()
	if r.status != http.StatusForbidden || !strings.Contains(r.body, "already linked") {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	if ids := h.identitiesOf(h.localID); len(ids) != 1 || ids[0].Subject != "user-1" {
		t.Errorf("identities = %+v", ids)
	}
}

func TestADetachedIdentityLinksAgainOnlyThroughTheRules(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	h.signIn()
	ids := h.identitiesOf(h.localID)
	admin := &LinkAdmin{Store: h.st}
	if err := admin.Detach(context.Background(), LinkActor{ID: "adm"}, ids[0].ID); err != nil {
		t.Fatal(err)
	}
	h.policy(store.LinkPolicyRow{}) // linking off now
	if r := h.signIn(); r.status != http.StatusForbidden {
		t.Fatalf("sign-in after detach with linking off = %d\n%s", r.status, r.body)
	}
	h.policy(linkingOn)
	if r := h.signIn(); r.status != http.StatusSeeOther {
		t.Fatalf("sign-in after detach with linking on = %d\n%s", r.status, r.body)
	}
	if n := len(h.identitiesOf(h.localID)); n != 1 {
		t.Errorf("identities = %d", n)
	}
}

func TestAStoreFailureEndsTheSignInWithoutLinking(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	h.svc.Linker = &Linker{Store: failingLinkStore{err: errors.New("disk on fire")}, Audit: h.aud}
	r := h.signIn()
	if r.status != http.StatusInternalServerError || r.cookie("minion_session") != nil {
		t.Fatalf("callback = %d\n%s", r.status, r.body)
	}
	if strings.Contains(r.body, "disk on fire") {
		t.Error("the error text reached the page")
	}
	if n := len(h.identitiesOf("")); n != 0 || h.countUsers() != 1 {
		t.Errorf("identities = %d users = %d", n, h.countUsers())
	}
}

type failingLinkStore struct{ err error }

func (f failingLinkStore) IdentityExists(context.Context, string, string) (bool, error) {
	return false, f.err
}

func (f failingLinkStore) LinkCandidateByEmail(context.Context, string) (store.LinkCandidate, error) {
	return store.LinkCandidate{}, f.err
}

func (f failingLinkStore) GetLinkPolicy(context.Context, string) (store.LinkPolicyRow, error) {
	return store.LinkPolicyRow{}, f.err
}

func (f failingLinkStore) LinkIdentity(context.Context, store.LinkIdentityParams) (store.IdentityRow, error) {
	return store.IdentityRow{}, f.err
}

// ---- the Linker without a browser ----------------------------------------------

// raceStore wraps a store and lets a test change the world after the
// decision read it.
type raceStore struct {
	*store.Store
	afterCandidate func()
	afterPolicy    func()
}

func (r raceStore) GetLinkPolicy(ctx context.Context, providerID string) (store.LinkPolicyRow, error) {
	p, err := r.Store.GetLinkPolicy(ctx, providerID)
	if r.afterPolicy != nil {
		r.afterPolicy()
	}
	return p, err
}

func (r raceStore) LinkCandidateByEmail(ctx context.Context, email string) (store.LinkCandidate, error) {
	c, err := r.Store.LinkCandidateByEmail(ctx, email)
	if r.afterCandidate != nil {
		r.afterCandidate()
	}
	return c, err
}

func TestAChangeBetweenDecisionAndWriteIsRefused(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	// The admin consent is withdrawn after the decision read it.
	adm := h.addLocal("root", "root@example.com", "admin")
	if _, err := h.st.SetAdminLinkConsent(context.Background(), store.AdminLinkConsentParams{UserID: adm, Granted: true, ActorID: adm, NowMS: 1}); err != nil {
		t.Fatal(err)
	}
	l := &Linker{Store: raceStore{Store: h.st, afterCandidate: func() {
		if _, err := h.st.SetAdminLinkConsent(context.Background(), store.AdminLinkConsentParams{UserID: adm, Granted: false, NowMS: 2}); err != nil {
			t.Error(err)
		}
	}}, Audit: h.aud}
	ident := Identity{Issuer: "https://idp.example", Subject: "s", Claims: map[string]any{"email": "root@example.com", "email_verified": true}}
	ref, err := l.Resolve(context.Background(), Config{ID: "default"}, ident, true)
	if err != nil || ref == nil || ref.Reason != ReasonChanged {
		t.Fatalf("Resolve = %+v, %v; want a refusal (changed)", ref, err)
	}
	if n := len(h.identitiesOf(adm)); n != 0 {
		t.Errorf("identities = %d", n)
	}
}

func TestAUnicodeFoldedAddressDoesNotMatchAnAsciiAccountThroughAnotherDomain(t *testing.T) {
	// Only vil.com is allowed. The IdP says U+0130 + "@evil.com"; folding it to
	// "i@evil.com" and cutting at the old offset would have checked "vil.com"
	// and linked the identity to the local account i@evil.com.
	h := newLinkHub(t, store.LinkPolicyRow{LinkingAllowed: true, Domains: []string{"vil.com"}})
	victim := h.addLocal("victim", "i@evil.com", "user")
	h.claims("\u0130@evil.com", true)
	l := &Linker{Store: h.st, Audit: h.aud}
	ref, err := l.Resolve(context.Background(), Config{ID: "default"},
		Identity{Issuer: "https://idp.example", Subject: "s", Claims: map[string]any{"email": "\u0130@evil.com", "email_verified": true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ref != nil {
		t.Errorf("refusal = %+v; the address matches no account, so the sign-in goes on", ref)
	}
	if n := len(h.identitiesOf(victim)); n != 0 {
		t.Errorf("the identity was linked to the account i@evil.com (%d identities)", n)
	}
}

func TestAPolicyRevokedAfterTheDecisionIsNotLinkedThrough(t *testing.T) {
	for name, revoke := range map[string]store.LinkPolicyRow{
		"linking switched off": {LinkingAllowed: false, Domains: []string{"example.com"}},
		"domain removed":       {LinkingAllowed: true, Domains: []string{"corp.example"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLinkHub(t, linkingOn)
			ident := Identity{Issuer: "https://idp.example", Subject: "s", Claims: map[string]any{"email": "ann@example.com", "email_verified": true}}
			l := &Linker{Store: raceStore{Store: h.st, afterPolicy: func() { h.policy(revoke) }}, Audit: h.aud}
			ref, err := l.Resolve(context.Background(), Config{ID: "default"}, ident, true)
			if err != nil || ref == nil || ref.Reason != ReasonChanged {
				t.Fatalf("Resolve = %+v, %v; want a refusal (changed)", ref, err)
			}
			if n := len(h.identitiesOf(h.localID)); n != 0 {
				t.Errorf("identities = %d after the policy was revoked", n)
			}
		})
	}
}

func TestAConcurrentFirstSignInThatLinkedTheIdentityFirstContinues(t *testing.T) {
	h := newLinkHub(t, linkingOn)
	ident := Identity{Issuer: "https://idp.example", Subject: "s", Claims: map[string]any{"email": "ann@example.com", "email_verified": true}}
	l := &Linker{Store: raceStore{Store: h.st, afterCandidate: func() {
		// The other sign-in links the identity between our read and write.
		_, err := h.st.LinkIdentity(context.Background(), store.LinkIdentityParams{UserID: h.localID, Email: "ann@example.com",
			Domain: "example.com", Issuer: ident.Issuer, Subject: ident.Subject, ProviderID: "default", NowMS: 3})
		if err != nil {
			t.Error(err)
		}
	}}, Audit: h.aud}
	ref, err := l.Resolve(context.Background(), Config{ID: "default"}, ident, true)
	if err != nil || ref != nil {
		t.Fatalf("Resolve = %+v, %v; want to continue", ref, err)
	}
}

// ---- administration -----------------------------------------------------------

type fixedHasher struct{}

func (fixedHasher) Hash(_ context.Context, password string) (string, error) {
	return "hashed:" + password, nil
}

func newLinkAdmin(t *testing.T, st *store.Store) *LinkAdmin {
	t.Helper()
	n := 0
	return &LinkAdmin{
		Store: st, Hasher: fixedHasher{},
		NewPassword: func() (string, error) { n++; return fmt.Sprintf("temp-password-%d", n), nil },
		Now:         func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func idpAccount(t *testing.T, st *store.Store, subject string, roles ...string) string {
	t.Helper()
	res, err := st.SyncOIDCLogin(context.Background(), store.OIDCSyncParams{
		Issuer: "https://idp.example", Subject: subject, ProviderID: "default", PreferredUsername: "idp-" + subject,
		Roles: roles, CreateIfMissing: true, NowMS: 5, RefreshToken: "refresh-token-" + subject,
	})
	if err != nil || !res.Created {
		t.Fatalf("sync = %+v, %v", res, err)
	}
	return res.User.ID
}

func auditRows(t *testing.T, st *store.Store) []store.AuditRow {
	t.Helper()
	rows, err := st.ListAudit(context.Background(), store.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rows)
	return rows
}

func TestAdminSetsALocalPasswordOnAnOIDCAccountAndTheUserSignsInLocally(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	uid := idpAccount(t, st, "sub-1", "user")
	admin := newLinkAdmin(t, st)
	admin.Hasher = local.NewHasher(local.WithParams(local.Params{MemoryKiB: 8, Iterations: 1, Parallelism: 1}))
	hasher := admin.Hasher.(*local.Hasher)

	pw, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, uid, "Idp.User@Example.com")
	if err != nil || pw.Reveal() != "temp-password-1" {
		t.Fatalf("SetPassword = %q, %v", pw.Reveal(), err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", pw, pw, pw), "temp-password-1") {
		t.Error("the password prints in clear")
	}
	u, _ := st.UserByID(ctx, uid)
	if u.PasswordHash == "" || strings.Contains(u.PasswordHash, "temp-password-1") || !u.MustChangePassword {
		t.Fatalf("account = %+v", u)
	}

	// The user signs in locally by user name and by e-mail address.
	limiter, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	auth := &local.Authenticator{Store: st, Hasher: hasher, Limiter: limiter, Audit: h0{}}
	for _, ident := range []string{u.Username, "idp.user@example.com"} {
		got, err := auth.Authenticate(ctx, local.Credentials{Identifier: ident, Password: pw.Reveal(), IP: netip.MustParseAddr("192.0.2.9")})
		if err != nil || got.ID != uid {
			t.Errorf("local sign-in by %q = %+v, %v", ident, got, err)
		}
	}
	// The identity and the IdP roles are untouched.
	if n := len(mustIdentities(t, st, uid)); n != 1 {
		t.Errorf("identities = %d", n)
	}

	// The audit row names the account and holds no password.
	rows := auditRows(t, st)
	if len(rows) != 1 || rows[0].Action != AuditActionSetPassword || rows[0].Target != uid || rows[0].Actor != "adm" || rows[0].Outcome != "success" {
		t.Fatalf("audit = %+v", rows)
	}

	// A second call is a reset, not a first password.
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, uid, ""); !errors.Is(err, ErrHasLocalPassword) {
		t.Errorf("second SetPassword err = %v", err)
	}
}

type h0 struct{}

func (h0) Record(context.Context, audit.Event) error { return nil }

func mustIdentities(t *testing.T, st *store.Store, userID string) []store.IdentityRow {
	t.Helper()
	rows, err := st.ListIdentities(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSetPasswordErrors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	admin := newLinkAdmin(t, st)
	uid := idpAccount(t, st, "sub-1", "user")
	other := store.NewID()
	if err := st.CreateUser(ctx, store.CreateUserParams{User: store.UserRow{ID: other, Username: "bob", Email: "bob@example.com", PasswordHash: "h", CredentialVersion: 1}, Role: "user"}); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm", ActingAs: uid}, uid, ""); !errors.Is(err, ErrActAsNotAllowed) {
		t.Errorf("act-as err = %v", err)
	}
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, store.NewID(), ""); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user err = %v", err)
	}
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, uid, "bob@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("taken e-mail err = %v", err)
	}
	dead := idpAccount(t, st, "sub-2", "user")
	if _, err := st.DeactivateUser(ctx, store.DeactivateUserParams{UserID: dead, NowMS: 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, dead, ""); !errors.Is(err, ErrUserInactive) {
		t.Errorf("deactivated err = %v", err)
	}
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, other, ""); !errors.Is(err, ErrHasLocalPassword) {
		t.Errorf("account with a password err = %v", err)
	}
	if rows := auditRows(t, st); len(rows) != 0 {
		t.Errorf("failed calls wrote audit rows: %+v", rows)
	}
}

func TestAdminListsAndDetachesIdentitiesWithAudit(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	admin := newLinkAdmin(t, st)
	a := idpAccount(t, st, "sub-a", "user")
	b := idpAccount(t, st, "sub-b", "user")
	if _, err := admin.SetPassword(ctx, LinkActor{ID: "adm"}, a, ""); err != nil {
		t.Fatal(err)
	}

	all, err := admin.ListIdentities(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	one, err := admin.ListIdentities(ctx, a)
	if err != nil || len(one) != 1 || one[0].UserID != a || one[0].Subject != "sub-a" || one[0].Issuer != "https://idp.example" || one[0].ProviderID != "default" {
		t.Fatalf("one = %+v, %v", one, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", all), "refresh-token") {
		t.Error("the listing carries a refresh token")
	}
	if none, err := admin.ListIdentities(ctx, store.NewID()); err != nil || none == nil || len(none) != 0 {
		t.Errorf("unknown user = %#v, %v", none, err)
	}

	// b has no password and one identity: it cannot be detached.
	idsB, _ := admin.ListIdentities(ctx, b)
	if err := admin.Detach(ctx, LinkActor{ID: "adm"}, idsB[0].ID); !errors.Is(err, ErrLastSignInMethod) {
		t.Errorf("detach of the only way in err = %v", err)
	}
	if err := admin.Detach(ctx, LinkActor{ID: "adm"}, store.NewID()); !errors.Is(err, ErrIdentityNotFound) {
		t.Errorf("unknown identity err = %v", err)
	}
	// a has a password now: detach works, the account stays.
	if err := admin.Detach(ctx, LinkActor{ID: "adm"}, one[0].ID); err != nil {
		t.Fatal(err)
	}
	if u, err := st.UserByID(ctx, a); err != nil || !u.Active || u.PasswordHash == "" {
		t.Errorf("account after detach = %+v, %v", u, err)
	}
	if left, _ := admin.ListIdentities(ctx, a); len(left) != 0 {
		t.Errorf("identities after detach = %+v", left)
	}
	rows := auditRows(t, st)
	if len(rows) != 2 || rows[1].Action != AuditActionIdentityDetach || rows[1].Target != one[0].ID || rows[1].Actor != "adm" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestAdminLinkConsentAndPolicy(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	admin := newLinkAdmin(t, st)
	adm := store.NewID()
	if err := st.CreateUser(ctx, store.CreateUserParams{User: store.UserRow{ID: adm, Username: "root", PasswordHash: "h", CredentialVersion: 1}, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	usr := idpAccount(t, st, "sub", "user")

	changed, err := admin.SetConsent(ctx, LinkActor{ID: adm}, adm, true)
	if err != nil || !changed {
		t.Fatalf("consent = %v, %v", changed, err)
	}
	if changed, err = admin.SetConsent(ctx, LinkActor{ID: adm}, adm, true); err != nil || changed {
		t.Errorf("repeated consent = %v, %v", changed, err)
	}
	if _, err := admin.SetConsent(ctx, LinkActor{ID: adm}, usr, true); !errors.Is(err, ErrNotAdminAccount) {
		t.Errorf("consent for a user err = %v", err)
	}
	if _, err := admin.SetConsent(ctx, LinkActor{ID: adm}, store.NewID(), true); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("consent for an unknown account err = %v", err)
	}
	if _, err := admin.SetConsent(ctx, LinkActor{ID: adm, ActingAs: usr}, adm, false); !errors.Is(err, ErrActAsNotAllowed) {
		t.Errorf("act-as consent err = %v", err)
	}
	if ok, _ := st.AdminLinkConsent(ctx, adm); !ok {
		t.Error("consent is off")
	}

	// Policy.
	exists := func(_ context.Context, id string) (bool, error) { return id == "default" || id == "acme", nil }
	admin.ProviderExists = exists
	err = admin.SetPolicy(ctx, LinkActor{ID: adm}, ops.OIDCLinkPolicySetArgs{ProviderID: "acme", LinkingAllowed: true, Domains: []string{"@Example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.SetPolicy(ctx, LinkActor{ID: adm}, ops.OIDCLinkPolicySetArgs{ProviderID: "ghost"}); !errors.Is(err, ErrUnknownProviderID) {
		t.Errorf("unknown provider err = %v", err)
	}
	pols, err := admin.ListPolicies(ctx)
	if err != nil || len(pols) != 1 || pols[0].ProviderID != "acme" || !pols[0].LinkingAllowed || pols[0].AutoCreate || !slices.Equal(pols[0].Domains, []string{"example.com"}) {
		t.Fatalf("policies = %+v, %v", pols, err)
	}
	var actions []string
	for _, r := range auditRows(t, st) {
		actions = append(actions, r.Action+"/"+r.Target)
	}
	want := []string{AuditActionLinkConsent + "/" + adm, AuditActionLinkPolicy + "/acme"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}

func TestLinkAdminPasswordGenerationFailure(t *testing.T) {
	st := openStore(t)
	uid := idpAccount(t, st, "sub", "user")
	a := newLinkAdmin(t, st)
	a.NewPassword = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := a.SetPassword(context.Background(), LinkActor{ID: "adm"}, uid, ""); err == nil {
		t.Fatal("SetPassword succeeded without a password")
	}
	if u, _ := st.UserByID(context.Background(), uid); u.PasswordHash != "" {
		t.Error("the account got a password hash")
	}
}
