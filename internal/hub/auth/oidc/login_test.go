package oidc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// ---- harness ----------------------------------------------------------------

type staticSource struct {
	mu   sync.Mutex
	cfgs []Config
	err  error
}

func (s *staticSource) OIDCProviders(context.Context) ([]Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cfgs), s.err
}

func (s *staticSource) set(fn func(c *Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfgs[0])
}

type recAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *recAudit) Record(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recAudit) RecordTx(ctx context.Context, _ store.Execer, ev audit.Event) error {
	return r.Record(ctx, ev)
}

func (r *recAudit) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *recAudit) has(action, outcome string) bool {
	for _, e := range r.all() {
		if e.Action == action && e.Outcome == outcome {
			return true
		}
	}
	return false
}

type allowAll struct{}

func (allowAll) Allow(ratelimit.Policy, netip.Addr, string) (bool, time.Duration) { return true, 0 }

type denyAll struct{}

func (denyAll) Allow(ratelimit.Policy, netip.Addr, string) (bool, time.Duration) {
	return false, 3 * time.Second
}

type testHub struct {
	t      *testing.T
	st     *store.Store
	idp    *fakeIdP
	cfgs   *staticSource
	svc    *Service
	srv    *httptest.Server
	aud    *recAudit
	logs   *bytes.Buffer
	client *http.Client // the browser: cookie jar, never follows redirects
	idpCl  *http.Client // the browser's view of the IdP: trusts the private CA
}

func baseConfig(idp *fakeIdP) Config {
	return Config{
		ID: "default", Name: "Acme SSO", Source: SourceSettings,
		Issuer: idp.Issuer(), ClientID: idp.ClientID, ClientSecret: logx.Secret(idp.ClientSecret),
		RoleClaim: "groups", AdminValues: []string{"minion-admins"}, UserValues: []string{"minion-users"},
		CAFile: idp.CAFile, Timeout: 2 * time.Second,
	}
}

func newTestHub(t *testing.T, mod func(c *Config)) *testHub {
	t.Helper()
	idp := newFakeIdP(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := baseConfig(idp)
	if mod != nil {
		mod(&cfg)
	}
	h := &testHub{t: t, st: st, idp: idp, cfgs: &staticSource{cfgs: []Config{cfg}}, aud: &recAudit{}, logs: &bytes.Buffer{}}
	h.svc = &Service{
		Configs: h.cfgs, Store: st, Sessions: session.NewManager(st), Limiter: allowAll{}, Audit: h.aud,
		Log:      slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TTL:      func(context.Context) (time.Duration, error) { return time.Hour, nil },
		Secure:   func(*http.Request) bool { return false },
		ClientIP: func(*http.Request) netip.Addr { return netip.MustParseAddr("192.0.2.1") },
	}
	mux := http.NewServeMux()
	h.svc.Register(mux)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	h.svc.BaseURL = func(*http.Request) string { return h.srv.URL }

	h.client = newBrowser(t, nil)
	pem, err := os.ReadFile(idp.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	h.idpCl = newBrowser(t, pool)
	return h
}

func newBrowser(t *testing.T, roots *x509.CertPool) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar, Timeout: 10 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type reply struct {
	status   int
	location string
	body     string
	cookies  []*http.Cookie
}

func (h *testHub) get(cl *http.Client, rawURL string) reply {
	h.t.Helper()
	resp, err := cl.Get(rawURL)
	if err != nil {
		h.t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(b), cookies: resp.Cookies()}
}

func (r reply) cookie(name string) *http.Cookie {
	for _, c := range r.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// begin starts a sign-in and walks the browser to the IdP and back: it
// returns the callback URL (with code and state) and the authorize URL.
func (h *testHub) begin(startPath string) (callback, authorize string) {
	h.t.Helper()
	start := h.get(h.client, h.srv.URL+startPath)
	if start.status != http.StatusFound {
		h.t.Fatalf("start = %d %s", start.status, start.body)
	}
	authz := h.get(h.idpCl, start.location)
	if authz.status != http.StatusFound {
		h.t.Fatalf("authorize = %d %s", authz.status, authz.body)
	}
	return authz.location, start.location
}

func (h *testHub) signIn() reply {
	h.t.Helper()
	cb, _ := h.begin(LoginPath("default"))
	return h.get(h.client, cb)
}

func (h *testHub) idpRoles(userID string) []string {
	h.t.Helper()
	rows, err := h.st.DB().Query(`SELECT role FROM user_roles WHERE user_id = ? AND source = 'idp' ORDER BY role`, userID)
	if err != nil {
		h.t.Fatal(err)
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

func (h *testHub) countUsers() int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *testHub) sessionOf(r reply) (*session.Session, bool) {
	h.t.Helper()
	c := r.cookie(session.CookieName)
	if c == nil || c.Value == "" {
		return nil, false
	}
	s, err := session.NewManager(h.st).Resolve(context.Background(), c.Value)
	if err != nil {
		h.t.Fatalf("session does not resolve: %v", err)
	}
	return s, true
}

func (h *testHub) groups(g ...string) {
	h.idp.set(func(f *fakeIdP) {
		f.claims["groups"] = anySlice(g)
	})
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

// ---- the flow -----------------------------------------------------------------

func TestFullLoginCreatesAccountWithIdPRole(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "alice"; f.refreshToken = "rt-secret-value" })

	cb, authorize := h.begin(LoginPath("default") + "?next=%2Fworkers")

	// The authorization request uses PKCE (S256), state, nonce and the public URL.
	u, _ := url.Parse(authorize)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || len(q.Get("state")) < 32 || len(q.Get("nonce")) < 32 {
		t.Errorf("authorization request lacks PKCE, state or nonce: %s", u.RawQuery)
	}
	if got, want := q.Get("redirect_uri"), h.srv.URL+CallbackPath; got != want {
		t.Errorf("redirect_uri = %q, want %q", got, want)
	}
	if q.Get("response_type") != "code" || q.Get("client_id") != "minion" || !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		t.Errorf("authorization request: %s", u.RawQuery)
	}

	r := h.get(h.client, cb)
	if r.status != http.StatusSeeOther || r.location != "/workers" {
		t.Fatalf("callback = %d -> %q\n%s", r.status, r.location, r.body)
	}
	sess, ok := h.sessionOf(r)
	if !ok {
		t.Fatal("no session cookie")
	}
	if sess.Username != "alice" || sess.Role != "admin" || sess.MustChangePassword {
		t.Errorf("session = %+v", sess)
	}
	if c := r.cookie(session.CookieName); !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie flags: %+v", c)
	}
	if got := h.idpRoles(sess.UserID); !slices.Equal(got, []string{"admin"}) {
		t.Errorf("idp roles = %v", got)
	}
	if !h.aud.has(AuditActionLogin, OutcomeSuccess) {
		t.Errorf("no success audit: %+v", h.aud.all())
	}
	// A sealed refresh token is stored, and the plaintext is not in the file.
	var sealed []byte
	var identityID string
	if err := h.st.DB().QueryRow(`SELECT id, refresh_token_enc FROM external_identities WHERE user_id = ?`, sess.UserID).Scan(&identityID, &sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, []byte("rt-secret-value")) {
		t.Fatal("refresh token is missing or stored in clear")
	}
	plain, err := h.st.Box().Open("external_identities", "refresh_token", identityID, sealed)
	if err != nil || string(plain) != "rt-secret-value" {
		t.Fatalf("refresh token does not open: %q, %v", plain, err)
	}
	// The binding cookie is cleared, and no secret reached the log.
	if c := r.cookie(BindingCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("binding cookie not cleared: %+v", c)
	}
	for _, secret := range []string{"s3cret-for-tests", "rt-secret-value"} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("secret %q reached the log:\n%s", secret, h.logs)
		}
	}
}

func TestIdentityIsIssuerAndSubject(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "bob" })
	r1 := h.signIn()
	s1, _ := h.sessionOf(r1)

	// The same subject with another display name is the same account.
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "robert" })
	s2, _ := h.sessionOf(h.signIn())
	if s1.UserID != s2.UserID || s2.Username != "bob" {
		t.Errorf("same subject gave %s/%s and %s/%s", s1.UserID, s1.Username, s2.UserID, s2.Username)
	}

	// Another subject with the same name is a new account with a free name; it
	// is never merged by name.
	h.idp.set(func(f *fakeIdP) { f.subject = "user-2"; f.claims["preferred_username"] = "bob" })
	s3, _ := h.sessionOf(h.signIn())
	if s3.UserID == s1.UserID || s3.Username != "bob-2" {
		t.Errorf("new subject gave %s/%s", s3.UserID, s3.Username)
	}
	if h.countUsers() != 2 {
		t.Errorf("users = %d", h.countUsers())
	}
}

func TestIdentityWithoutPreferredUsernameGetsGeneratedName(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	s, _ := h.sessionOf(h.signIn())
	if !strings.HasPrefix(s.Username, "oidc-") {
		t.Errorf("username = %q", s.Username)
	}
}

func TestUnsafeNextBecomesRoot(t *testing.T) {
	for _, next := range []string{"//evil.example", "https://evil.example/x", "/\\evil", "evil", "/a\nb", ""} {
		if got := safeNext(next); got != "/" {
			t.Errorf("safeNext(%q) = %q", next, got)
		}
	}
	for _, next := range []string{"/", "/workers", "/workers?x=1"} {
		if got := safeNext(next); got != next {
			t.Errorf("safeNext(%q) = %q", next, got)
		}
	}
	h := newTestHub(t, nil)
	h.groups("minion-users")
	cb, _ := h.begin(LoginPath("default") + "?next=" + url.QueryEscape("//evil.example"))
	if r := h.get(h.client, cb); r.location != "/" {
		t.Errorf("redirect = %q", r.location)
	}
}

// ---- role rules -----------------------------------------------------------------

func TestNoMatchingRuleMeansNoAccess(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"no rule matches": {"groups": []any{"strangers"}},
		"claim missing":   {"email": "x@example.com"},
		"claim empty":     {"groups": []any{}},
		"claim blank":     {"groups": ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHub(t, nil)
			h.idp.set(func(f *fakeIdP) { f.claims = claims })
			r := h.signIn()
			if r.status != http.StatusForbidden {
				t.Fatalf("callback = %d, want 403\n%s", r.status, r.body)
			}
			mustContain(t, r.body, "No access", "Back to sign in")
			if _, ok := h.sessionOf(r); ok {
				t.Error("a session was issued")
			}
			if h.countUsers() != 0 {
				t.Error("an account was created for a sign-in without access")
			}
			if !h.aud.has(AuditActionLogin, OutcomeDenied) {
				t.Errorf("audit: %+v", h.aud.all())
			}
		})
	}
}

func TestRolesAreReplacedAtEachLoginWithoutTouchingLocalGrants(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	s1, _ := h.sessionOf(h.signIn())
	// A local grant on the same account.
	if _, err := h.st.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, s1.UserID); err != nil {
		t.Fatal(err)
	}

	h.groups("minion-admins")
	s2, _ := h.sessionOf(h.signIn())
	if got := h.idpRoles(s1.UserID); !slices.Equal(got, []string{"admin"}) || s2.Role != "admin" {
		t.Fatalf("after promotion: idp roles %v, role %s", got, s2.Role)
	}

	// Both rules at once; the replacement is exact, not additive.
	h.groups("minion-admins", "minion-users")
	h.signIn()
	if got := h.idpRoles(s1.UserID); !slices.Equal(got, []string{"admin", "user"}) {
		t.Fatalf("both roles: %v", got)
	}
	h.groups("minion-users")
	h.signIn()
	if got := h.idpRoles(s1.UserID); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("after demotion: %v", got)
	}
	var local int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'local' AND role = 'admin'`, s1.UserID).Scan(&local); err != nil || local != 1 {
		t.Fatalf("local grant touched: %d, %v", local, err)
	}
	if !h.aud.has(AuditActionRoleSync, OutcomeChanged) {
		t.Errorf("role changes were not audited: %+v", h.aud.all())
	}
}

func TestDemotionAppliesEvenToTheLastAdmin(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	s1, _ := h.sessionOf(h.signIn())
	if s1.Role != "admin" {
		t.Fatalf("role = %s", s1.Role)
	}
	var admins int
	count := func() {
		if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE role = 'admin'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
	}
	count()
	if admins != 1 {
		t.Fatalf("admins = %d", admins)
	}

	// The only admin now only has the user rule: demoted although last.
	h.groups("minion-users")
	s2, _ := h.sessionOf(h.signIn())
	count()
	if admins != 0 || s2.Role != "user" {
		t.Fatalf("admins = %d, role = %s", admins, s2.Role)
	}

	// And with no rule at all the access is gone, roles included.
	h.groups("minion-admins")
	h.signIn()
	h.groups("strangers")
	r := h.signIn()
	count()
	if r.status != http.StatusForbidden || admins != 0 || len(h.idpRoles(s1.UserID)) != 0 {
		t.Fatalf("status %d, admins %d, idp roles %v", r.status, admins, h.idpRoles(s1.UserID))
	}
}

func TestNoAccessEndsTheEarlierSessions(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	first := h.signIn()
	tok := first.cookie(session.CookieName)
	if tok == nil {
		t.Fatalf("no session cookie (status %d)", first.status)
	}
	s1, _ := h.sessionOf(first)
	// A local grant on the same account must survive.
	if _, err := h.st.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, s1.UserID); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(h.st)
	if _, err := mgr.Resolve(context.Background(), tok.Value); err != nil {
		t.Fatalf("the first session does not resolve before: %v", err)
	}

	// The user is removed from every allowed group.
	h.groups("strangers")
	r := h.signIn()
	if r.status != http.StatusForbidden || r.cookie(session.CookieName) != nil {
		t.Fatalf("status %d, cookie %v", r.status, r.cookie(session.CookieName))
	}
	if _, err := mgr.Resolve(context.Background(), tok.Value); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("the earlier session still resolves: %v", err)
	}
	var sessions, local int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, s1.UserID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions left: %d, %v", sessions, err)
	}
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM user_roles WHERE user_id = ? AND source = 'local' AND role = 'admin'`, s1.UserID).Scan(&local); err != nil || local != 1 {
		t.Fatalf("local grant touched: %d, %v", local, err)
	}
	if got := h.idpRoles(s1.UserID); len(got) != 0 {
		t.Fatalf("idp roles left: %v", got)
	}
}

func TestAuditFailureLeavesRolesAndEarlierSessionsAsTheyWere(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	first := h.signIn()
	tok := first.cookie(session.CookieName)
	if tok == nil {
		t.Fatalf("no session cookie (status %d)", first.status)
	}
	s1, _ := h.sessionOf(first)

	// The user now gains the admin rule, but the role-change audit cannot be
	// written: the promotion must not stay.
	h.groups("minion-admins")
	cb, _ := h.begin(LoginPath("default"))
	h.aud.mu.Lock()
	h.aud.err = errors.New("audit down")
	h.aud.mu.Unlock()
	r := h.get(h.client, cb)
	if r.status != http.StatusInternalServerError {
		t.Fatalf("status %d", r.status)
	}
	if got := h.idpRoles(s1.UserID); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("roles changed without an audit row: %v", got)
	}
	s, err := session.NewManager(h.st).Resolve(context.Background(), tok.Value)
	if err != nil || s.Role != "user" {
		t.Fatalf("the earlier session: %+v, %v", s, err)
	}
}

func TestSeedAdminIsLocalAndUntouchedByDemotion(t *testing.T) {
	h := newTestHub(t, nil)
	if _, err := h.st.SeedAdmin(context.Background(), store.SeedAdminParams{
		User:   store.UserRow{ID: store.NewID(), Username: "root", CreatedAt: 1, UpdatedAt: 1},
		Marker: "admin_seed",
	}); err != nil {
		t.Fatal(err)
	}
	h.groups("minion-admins")
	h.signIn()
	h.groups("minion-users")
	h.signIn()
	root, err := h.st.UserByIdentifier(context.Background(), "root")
	if err != nil || root.Role != "admin" {
		t.Fatalf("seed admin: %+v, %v", root, err)
	}
}

func TestInactiveAccountIsRefusedAndKeepsItsRoles(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	s, _ := h.sessionOf(h.signIn())
	if _, err := h.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, s.UserID); err != nil {
		t.Fatal(err)
	}
	h.groups("minion-users")
	r := h.signIn()
	if r.status != http.StatusForbidden {
		t.Fatalf("status %d", r.status)
	}
	mustContain(t, r.body, "deactivated")
	if got := h.idpRoles(s.UserID); !slices.Equal(got, []string{"admin"}) {
		t.Errorf("roles of an inactive account changed: %v", got)
	}
	if !h.aud.has(AuditActionLogin, OutcomeInactive) {
		t.Errorf("audit: %+v", h.aud.all())
	}
}

// ---- state, nonce, ID token -----------------------------------------------------

func TestCallbackStateChecks(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")

	t.Run("missing state", func(t *testing.T) {
		if r := h.get(h.client, h.srv.URL+CallbackPath+"?code=x"); r.status != http.StatusBadRequest {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		r := h.get(h.client, h.srv.URL+CallbackPath+"?code=x&state=forged")
		if r.status != http.StatusBadRequest {
			t.Errorf("status %d", r.status)
		}
		mustContain(t, r.body, "Sign-in failed")
	})
	t.Run("state of another sign-in", func(t *testing.T) {
		cb1, _ := h.begin(LoginPath("default"))
		cb2, _ := h.begin(LoginPath("default"))
		// The code of sign-in 2 with the state of sign-in 1 is a mismatch: the
		// state names a transaction whose nonce and verifier are not the code's.
		u1, _ := url.Parse(cb1)
		u2, _ := url.Parse(cb2)
		q := u2.Query()
		q.Set("state", u1.Query().Get("state"))
		u2.RawQuery = q.Encode()
		if r := h.get(h.client, u2.String()); r.status == http.StatusSeeOther {
			t.Errorf("a mismatched state signed in")
		}
		if h.countUsers() != 0 {
			t.Error("an account was created")
		}
	})
	t.Run("replay", func(t *testing.T) {
		cb, _ := h.begin(LoginPath("default"))
		if r := h.get(h.client, cb); r.status != http.StatusSeeOther {
			t.Fatalf("first use = %d %s", r.status, r.body)
		}
		if r := h.get(h.client, cb); r.status != http.StatusBadRequest {
			t.Errorf("second use = %d", r.status)
		}
	})
	t.Run("another browser", func(t *testing.T) {
		cb, _ := h.begin(LoginPath("default"))
		other := newBrowser(t, nil) // no binding cookie
		r := h.get(other, cb)
		if r.status != http.StatusBadRequest {
			t.Errorf("status %d", r.status)
		}
		mustContain(t, r.body, "another browser")
	})
	t.Run("expired", func(t *testing.T) {
		cb, _ := h.begin(LoginPath("default"))
		h.svc.Now = func() time.Time { return time.Now().Add(loginTxTTL + time.Minute) }
		defer func() { h.svc.Now = nil }()
		if r := h.get(h.client, cb); r.status != http.StatusBadRequest {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("idp error", func(t *testing.T) {
		cb, _ := h.begin(LoginPath("default"))
		u, _ := url.Parse(cb)
		r := h.get(h.client, h.srv.URL+CallbackPath+"?error=access_denied%3Cscript%3E&state="+u.Query().Get("state"))
		if r.status != http.StatusBadGateway || strings.Contains(r.body, "script") {
			t.Errorf("status %d body %s", r.status, r.body)
		}
	})
}

func TestNonceMismatchIsRefused(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	h.idp.set(func(f *fakeIdP) { f.wrongNonce = true })
	r := h.signIn()
	if r.status != http.StatusUnauthorized {
		t.Fatalf("status %d\n%s", r.status, r.body)
	}
	if _, ok := h.sessionOf(r); ok || h.countUsers() != 0 {
		t.Error("a nonce mismatch signed in")
	}
}

func TestIDTokenIsRequired(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	h.idp.set(func(f *fakeIdP) { f.omitIDToken = true })
	r := h.signIn()
	if r.status != http.StatusBadGateway {
		t.Fatalf("status %d", r.status)
	}
	mustContain(t, r.body, "no ID token")
	if h.countUsers() != 0 {
		t.Error("an account was created without an ID token")
	}
}

func TestIDTokenIsVerified(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	t.Run("wrong audience", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.claims["aud"] = "someone-else" })
		defer h.idp.set(func(f *fakeIdP) { delete(f.claims, "aud") })
		if r := h.signIn(); r.status != http.StatusUnauthorized {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("expired", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.claims["exp"] = time.Now().Add(-time.Hour).Unix() })
		defer h.idp.set(func(f *fakeIdP) { delete(f.claims, "exp") })
		if r := h.signIn(); r.status != http.StatusUnauthorized {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.claims["iss"] = "https://evil.example" })
		defer h.idp.set(func(f *fakeIdP) { delete(f.claims, "iss") })
		if r := h.signIn(); r.status != http.StatusUnauthorized {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("no subject", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.subject = "" })
		defer h.idp.set(func(f *fakeIdP) { f.subject = "user-1" })
		if r := h.signIn(); r.status != http.StatusUnauthorized {
			t.Errorf("status %d", r.status)
		}
	})
	t.Run("signed with another key", func(t *testing.T) {
		other := newFakeIdP(t) // its key is not in our IdP's key set
		forged := func() string {
			return h.idp.signWith(other.key, map[string]any{"iss": h.idp.Issuer(), "aud": "minion"}, "n", "mallory", map[string]any{"groups": []any{"minion-admins"}})
		}()
		// Verify directly: the provider must refuse a token its key set cannot check.
		prov, err := Discover(context.Background(), baseConfig(h.idp))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prov.VerifyIDToken(context.Background(), forged, "n"); !errors.Is(err, ErrInvalidIDToken) {
			t.Errorf("forged token: %v", err)
		}
	})
	if h.countUsers() != 0 {
		t.Error("an invalid ID token created an account")
	}
}

// ---- failures change no roles -----------------------------------------------------

func TestFailuresDoNotChangeRoles(t *testing.T) {
	h := newTestHub(t, func(c *Config) { c.Timeout = 300 * time.Millisecond })
	h.groups("minion-admins")
	s, _ := h.sessionOf(h.signIn())

	check := func(t *testing.T) {
		t.Helper()
		if got := h.idpRoles(s.UserID); !slices.Equal(got, []string{"admin"}) {
			t.Errorf("roles changed to %v", got)
		}
	}
	// From now on the IdP would demote, but each failure must keep the roles.
	h.groups("minion-users")

	t.Run("token endpoint fails", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.failToken = true })
		defer h.idp.set(func(f *fakeIdP) { f.failToken = false })
		r := h.signIn()
		if r.status != http.StatusBadGateway {
			t.Errorf("status %d", r.status)
		}
		mustContain(t, r.body, "Nothing was changed")
		check(t)
	})
	t.Run("token endpoint exceeds the deadline", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.tokenDelay = 700 * time.Millisecond })
		defer h.idp.set(func(f *fakeIdP) { f.tokenDelay = 0 })
		start := time.Now()
		r := h.signIn()
		if r.status != http.StatusBadGateway || time.Since(start) > 1500*time.Millisecond {
			t.Errorf("status %d after %s", r.status, time.Since(start))
		}
		check(t)
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.wrongNonce = true })
		defer h.idp.set(func(f *fakeIdP) { f.wrongNonce = false })
		h.signIn()
		check(t)
	})
	t.Run("no ID token", func(t *testing.T) {
		h.idp.set(func(f *fakeIdP) { f.omitIDToken = true })
		defer h.idp.set(func(f *fakeIdP) { f.omitIDToken = false })
		h.signIn()
		check(t)
	})
}

func TestDiscoveryHasADeadline(t *testing.T) {
	h := newTestHub(t, func(c *Config) { c.Timeout = 200 * time.Millisecond })
	h.idp.set(func(f *fakeIdP) { f.discoveryDelay = 700 * time.Millisecond })
	start := time.Now()
	r := h.get(h.client, h.srv.URL+LoginPath("default"))
	if r.status != http.StatusBadGateway || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("status %d after %s", r.status, time.Since(start))
	}
	mustContain(t, r.body, "Identity provider unreachable", "Nothing was changed")
	if strings.Contains(r.body, h.idp.Issuer()) {
		t.Error("the error page names the IdP address")
	}
	// A recovered IdP works at the next try: a failed discovery is not cached.
	h.idp.set(func(f *fakeIdP) { f.discoveryDelay = 0 })
	if r := h.get(h.client, h.srv.URL+LoginPath("default")); r.status != http.StatusFound {
		t.Errorf("after recovery: %d", r.status)
	}
}

// ---- provider, CA file, insecure flag ---------------------------------------------

func TestCAFileInsecureFlagAndTrust(t *testing.T) {
	idp := newFakeIdP(t)
	base := baseConfig(idp)

	t.Run("private CA file", func(t *testing.T) {
		if _, err := Discover(context.Background(), base); err != nil {
			t.Fatalf("with the CA file: %v", err)
		}
	})
	t.Run("no CA file and no flag", func(t *testing.T) {
		c := base
		c.CAFile = ""
		_, err := Discover(context.Background(), c)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("an unknown CA was trusted: %v", err)
		}
	})
	t.Run("insecure flag skips verification", func(t *testing.T) {
		c := base
		c.CAFile, c.InsecureSkipVerify = "", true
		if _, err := Discover(context.Background(), c); err != nil {
			t.Fatalf("with the insecure flag: %v", err)
		}
	})
	t.Run("bad CA files", func(t *testing.T) {
		c := base
		c.CAFile = idp.CAFile + ".missing"
		if _, err := Discover(context.Background(), c); err == nil {
			t.Error("missing CA file accepted")
		}
		junk := t.TempDir() + "/junk.pem"
		_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
		c.CAFile = junk
		if _, err := Discover(context.Background(), c); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
			t.Errorf("junk CA file: %v", err)
		}
	})
	t.Run("issuer mismatch", func(t *testing.T) {
		c := base
		c.Issuer = idp.Issuer() + "/other"
		if _, err := Discover(context.Background(), c); err == nil {
			t.Error("a discovery document of another issuer was accepted")
		}
	})
}

func TestConfigValidate(t *testing.T) {
	ok := Config{ID: "default", Issuer: "https://idp.example", ClientID: "c"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"no id":            func(c *Config) { c.ID = "" },
		"no issuer":        func(c *Config) { c.Issuer = "" },
		"file issuer":      func(c *Config) { c.Issuer = "file:///etc/passwd" },
		"no host":          func(c *Config) { c.Issuer = "https://" },
		"no client id":     func(c *Config) { c.ClientID = "" },
		"scopes no openid": func(c *Config) { c.Scopes = []string{"profile"} },
	} {
		c := ok
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if (Config{}).timeout() != DefaultTimeout || DefaultTimeout != 10*time.Second {
		t.Errorf("default timeout = %s", DefaultTimeout)
	}
}

func TestCleartextIssuerIsRefusedBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(plain.Close)
	c := Config{ID: "default", Issuer: plain.URL, ClientID: "c", Timeout: time.Second}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http issuer: %v", err)
	}
	if _, err := Discover(context.Background(), c); err == nil {
		t.Fatal("discovery over plain http was attempted")
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests reached the plain http issuer", hits.Load())
	}
	// Only the explicit insecure flag, which warns permanently, allows it.
	c.InsecureSkipVerify = true
	if err := c.Validate(); err != nil {
		t.Fatalf("with the insecure flag: %v", err)
	}
}

func TestCleartextDiscoveredEndpointsAreRefused(t *testing.T) {
	idp := newFakeIdP(t)
	idp.set(func(f *fakeIdP) { f.tokenURL = "http://idp.example/token" })
	c := baseConfig(idp)
	if _, err := Discover(context.Background(), c); err == nil || !strings.Contains(err.Error(), "token_endpoint") {
		t.Fatalf("plain http token endpoint accepted: %v", err)
	}
	c.InsecureSkipVerify = true
	if _, err := Discover(context.Background(), c); err != nil {
		t.Fatalf("with the insecure flag: %v", err)
	}
}

func TestProviderIsCachedAndRediscoveredOnChange(t *testing.T) {
	h := newTestHub(t, nil)
	var calls atomic.Int32
	h.svc.Discover = func(ctx context.Context, c Config) (*Provider, error) {
		calls.Add(1)
		return Discover(ctx, c)
	}
	for range 3 {
		if r := h.get(h.client, h.srv.URL+LoginPath("default")); r.status != http.StatusFound {
			t.Fatalf("status %d", r.status)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("discoveries = %d, want 1", calls.Load())
	}
	h.cfgs.set(func(c *Config) { c.ClientID = "minion"; c.Scopes = []string{"openid"} })
	h.get(h.client, h.srv.URL+LoginPath("default"))
	if calls.Load() != 2 {
		t.Errorf("discoveries after a change = %d, want 2", calls.Load())
	}
}

func TestUnknownOrDisabledProvider(t *testing.T) {
	h := newTestHub(t, nil)
	if r := h.get(h.client, h.srv.URL+LoginPath("nope")); r.status != http.StatusNotFound {
		t.Errorf("unknown: %d", r.status)
	}
	h.cfgs.set(func(c *Config) { c.Disabled = true })
	if r := h.get(h.client, h.srv.URL+LoginPath("default")); r.status != http.StatusNotFound {
		t.Errorf("disabled: %d", r.status)
	}
	h.cfgs.mu.Lock()
	h.cfgs.err = errors.New("db down")
	h.cfgs.mu.Unlock()
	if r := h.get(h.client, h.srv.URL+LoginPath("default")); r.status != http.StatusInternalServerError {
		t.Errorf("source error: %d", r.status)
	}
}

func TestRateLimit(t *testing.T) {
	h := newTestHub(t, nil)
	h.svc.Limiter = denyAll{}
	r := h.get(h.client, h.srv.URL+LoginPath("default"))
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("start: %d", r.status)
	}
	if r := h.get(h.client, h.srv.URL+CallbackPath+"?state=x&code=y"); r.status != http.StatusTooManyRequests {
		t.Fatalf("callback: %d", r.status)
	}
	if !h.aud.has(AuditActionLogin, OutcomeRateLimited) {
		t.Error("rate limit not audited")
	}
}

func TestAuditFailureRevokesTheSession(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	cb, _ := h.begin(LoginPath("default"))
	h.aud.mu.Lock()
	h.aud.err = errors.New("audit down")
	h.aud.mu.Unlock()
	r := h.get(h.client, cb)
	if r.status != http.StatusInternalServerError || r.cookie(session.CookieName) != nil {
		t.Fatalf("status %d, cookie %v", r.status, r.cookie(session.CookieName))
	}
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil || n != 0 {
		t.Errorf("sessions left behind: %d, %v", n, err)
	}
}

func TestStartAnswersWithRedirectAndBindingCookie(t *testing.T) {
	h := newTestHub(t, nil)
	r := h.get(h.client, h.srv.URL+LoginPath("default"))
	if r.status != http.StatusFound || !strings.HasPrefix(r.location, h.idp.Issuer()+"/authorize?") {
		t.Fatalf("%d %s", r.status, r.location)
	}
	c := r.cookie(BindingCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth/oidc" || len(c.Value) < 32 {
		t.Fatalf("binding cookie: %+v", c)
	}
	// Only hashes are stored, never the state itself.
	u, _ := url.Parse(r.location)
	state := u.Query().Get("state")
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM oidc_login_transactions WHERE state_hash = ?`, store.HashValue(state)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("transaction row: %d, %v", n, err)
	}
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM oidc_login_transactions WHERE state_hash = ? OR binding_hash = ?`, state, c.Value).Scan(&n); err != nil || n != 0 {
		t.Fatalf("state or cookie stored in clear: %d, %v", n, err)
	}
}

// A callback that was allowed pauses before it issues its session; a second
// callback of the same account finds no matching role and revokes the access.
// The paused callback must not hand out a session that works.
func TestConcurrentCallbackCannotRestoreAccessAfterRevocation(t *testing.T) {
	h := newTestHub(t, nil)
	h.groups("minion-users")
	first := h.signIn()
	s1, ok := h.sessionOf(first)
	if !ok {
		t.Fatalf("no first session (status %d)", first.status)
	}

	// Two sign-ins are prepared while the user still has a role: A will carry
	// the allowed claims, B is started later with claims that match no rule.
	cbA, _ := h.begin(LoginPath("default"))
	browserB := newBrowser(t, nil)
	startB := h.get(browserB, h.srv.URL+LoginPath("default"))
	authzB := h.get(h.idpCl, startB.location)
	if startB.status != http.StatusFound || authzB.status != http.StatusFound {
		t.Fatalf("start B = %d, authorize B = %d", startB.status, authzB.status)
	}

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.svc.TTL = func(context.Context) (time.Duration, error) {
		// Only callback A reaches the session step with a role; it is paused
		// here, after its role sync committed and before the session exists.
		once.Do(func() {
			close(reached)
			<-release
		})
		return time.Hour, nil
	}

	type result struct {
		r reply
	}
	done := make(chan result, 1)
	go func() {
		// Not h.get: t.Fatalf must not run on this goroutine.
		resp, err := h.client.Get(cbA)
		if err != nil {
			done <- result{r: reply{status: -1, body: err.Error()}}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		done <- result{r: reply{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(b), cookies: resp.Cookies()}}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("callback A never reached the session step")
	}

	// B: the IdP now reports no matching group, and B's callback completes.
	h.groups("strangers")
	rB := h.get(browserB, authzB.location)
	if rB.status != http.StatusForbidden || rB.cookie(session.CookieName) != nil {
		t.Fatalf("callback B = %d, cookie %v\n%s", rB.status, rB.cookie(session.CookieName), rB.body)
	}
	close(release)

	var rA reply
	select {
	case res := <-done:
		rA = res.r
	case <-time.After(10 * time.Second):
		t.Fatal("callback A did not finish")
	}
	// A may be refused or may hand out a cookie, but nothing it hands out may
	// resolve to a session, and the account has no access.
	mgr := session.NewManager(h.st)
	if c := rA.cookie(session.CookieName); c != nil && c.Value != "" {
		if s, err := mgr.Resolve(context.Background(), c.Value); !errors.Is(err, session.ErrNoSession) {
			t.Fatalf("a session issued after the revocation resolves: %+v, %v", s, err)
		}
	}
	if len(h.idpRoles(s1.UserID)) != 0 {
		t.Fatalf("idp roles left: %v", h.idpRoles(s1.UserID))
	}
	var live int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.user_id = ? AND s.credential_version = u.credential_version`, s1.UserID).Scan(&live); err != nil || live != 0 {
		t.Fatalf("valid sessions left: %d, %v", live, err)
	}
}
