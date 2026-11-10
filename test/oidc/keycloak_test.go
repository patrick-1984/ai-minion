//go:build keycloak

// Package keycloak_test signs in to an in-process Hub through a real Keycloak
// (Story 6.10, AD-25, AD-10). The fake issuer of internal/hub/auth/oidc proves
// the protocol handling; this test proves that the same code works with a
// common open-source IdP: discovery, the authorization-code flow with PKCE,
// the group claim and the role rules.
//
// The tests need a running Keycloak with the realm in realm.json imported:
//
//	docker run -d --name keycloak -p 127.0.0.1:8080:8080 \
//	  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
//	  -v "$PWD/test/oidc/realm.json:/opt/keycloak/data/import/realm.json:ro" \
//	  quay.io/keycloak/keycloak:26.2.5 start-dev --import-realm
//	go test -tags keycloak -count=1 ./test/oidc/...
//
// The workflow .github/workflows/oidc.yml does exactly that. Environment:
//
//	MINION_KEYCLOAK_URL            base URL of Keycloak (default http://127.0.0.1:8080)
//	MINION_KEYCLOAK_ADMIN_USER     admin console user (default admin)
//	MINION_KEYCLOAK_ADMIN_PASSWORD admin console password (default admin)
//	MINION_REQUIRE_KEYCLOAK=1      fail instead of skipping when Keycloak is down
//
// The Hub listens on 127.0.0.1:18765, the redirect URI that realm.json
// registers. The browser is a plain HTTP client: it posts the login form of
// Keycloak directly (no JavaScript, no real browser).
//
// Keycloak runs on plain HTTP here, so the provider sets the explicit insecure
// flag of R96 (the only way to use an http issuer). TLS trust is covered by the
// fake-issuer tests with a private CA.
package keycloak_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// Values that realm.json defines.
const (
	realmName    = "minion"
	clientID     = "minion-hub"
	clientSecret = "keycloak-test-client-secret"
	hubAddr      = "127.0.0.1:18765"

	groupAdmins = "minion-admins"
	groupUsers  = "minion-users"
)

var passwords = map[string]string{
	"alice": "alice-test-password", // member of minion-admins
	"bob":   "bob-test-password",   // member of minion-users
	"carol": "carol-test-password", // member of no group
}

// ---- Keycloak ---------------------------------------------------------------

type keycloak struct {
	base       string // no trailing slash
	adminUser  string
	adminPass  string
	httpClient *http.Client
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// startKeycloak returns the Keycloak under test, waiting up to two minutes for
// the realm to be served. Without a Keycloak the test is skipped, unless
// MINION_REQUIRE_KEYCLOAK=1 (CI) turns that into a failure.
func startKeycloak(t *testing.T) *keycloak {
	t.Helper()
	k := &keycloak{
		base:       strings.TrimSuffix(envOr("MINION_KEYCLOAK_URL", "http://127.0.0.1:8080"), "/"),
		adminUser:  envOr("MINION_KEYCLOAK_ADMIN_USER", "admin"),
		adminPass:  envOr("MINION_KEYCLOAK_ADMIN_PASSWORD", "admin"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	wait := 2 * time.Minute
	if os.Getenv("MINION_REQUIRE_KEYCLOAK") != "1" {
		wait = 3 * time.Second
	}
	deadline := time.Now().Add(wait)
	var last error
	for {
		last = k.discoveryReady()
		if last == nil {
			return k
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if os.Getenv("MINION_REQUIRE_KEYCLOAK") == "1" {
		t.Fatalf("Keycloak at %s is not ready: %v", k.base, last)
	}
	t.Skipf("no Keycloak at %s (%v); see the package comment", k.base, last)
	return nil
}

func (k *keycloak) issuer() string { return k.base + "/realms/" + realmName }

func (k *keycloak) tokenURL() string { return k.issuer() + "/protocol/openid-connect/token" }

func (k *keycloak) discoveryReady() error {
	resp, err := k.httpClient.Get(k.issuer() + "/.well-known/openid-configuration")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery answered %d", resp.StatusCode)
	}
	return nil
}

// adminToken returns a short-lived token of the Keycloak admin console.
func (k *keycloak) adminToken(t *testing.T) string {
	t.Helper()
	resp, err := k.httpClient.PostForm(k.base+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {k.adminUser}, "password": {k.adminPass},
	})
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		t.Fatalf("admin token: status %d", resp.StatusCode)
	}
	return out.AccessToken
}

// admin calls the admin REST API of the realm and decodes a JSON answer into
// out when out is not nil.
func (k *keycloak) admin(t *testing.T, method, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(method, k.base+"/admin/realms/"+realmName+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+k.adminToken(t))
	resp, err := k.httpClient.Do(req)
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	body := readAll(t, resp)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("admin %s %s: status %d: %s", method, path, resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("admin %s %s: %v", method, path, err)
		}
	}
}

func (k *keycloak) userID(t *testing.T, username string) string {
	t.Helper()
	var users []struct{ ID, Username string }
	k.admin(t, http.MethodGet, "/users?exact=true&username="+url.QueryEscape(username), &users)
	for _, u := range users {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("Keycloak has no user %q: is realm.json imported?", username)
	return ""
}

func (k *keycloak) groupID(t *testing.T, name string) string {
	t.Helper()
	var groups []struct{ ID, Name string }
	k.admin(t, http.MethodGet, "/groups?exact=true&search="+url.QueryEscape(name), &groups)
	for _, g := range groups {
		if g.Name == name {
			return g.ID
		}
	}
	t.Fatalf("Keycloak has no group %q: is realm.json imported?", name)
	return ""
}

// setMember adds or removes a user from a group.
func (k *keycloak) setMember(t *testing.T, username, group string, member bool) {
	t.Helper()
	method := http.MethodDelete
	if member {
		method = http.MethodPut
	}
	k.admin(t, method, "/users/"+k.userID(t, username)+"/groups/"+k.groupID(t, group), nil)
}

// refresh trades a refresh token for fresh claims at the token endpoint, as a
// background role refresh does (AD-10). It returns the claims of the new ID
// token.
func (k *keycloak) refresh(t *testing.T, refreshToken string) map[string]any {
	t.Helper()
	resp, err := k.httpClient.PostForm(k.tokenURL(), url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken},
		"client_id": {clientID}, "client_secret": {clientSecret},
	})
	if err != nil {
		t.Fatalf("refresh grant: %v", err)
	}
	body := readAll(t, resp)
	var out struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out.IDToken == "" {
		t.Fatalf("refresh grant: status %d (the answer is not logged: it holds tokens)", resp.StatusCode)
	}
	claims, err := jwtClaims(out.IDToken)
	if err != nil {
		t.Fatalf("refresh grant: %v", err)
	}
	return claims
}

// jwtClaims decodes the payload of a JWT without verifying it. The token comes
// straight from the token endpoint of the Keycloak under test; the Hub's own
// verification is exercised by the sign-in tests.
func jwtClaims(raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("the ID token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("the ID token payload: %w", err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("the ID token claims: %w", err)
	}
	return claims, nil
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- the headless browser -----------------------------------------------------

// browser is an HTTP client with a cookie jar that never follows redirects, so
// that every hop of the flow is visible.
type browser struct{ *http.Client }

func newBrowser(t *testing.T) browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return browser{&http.Client{
		Jar: jar, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type page struct {
	status   int
	location string
	body     string
	cookies  []*http.Cookie
}

func (b browser) do(t *testing.T, req *http.Request) page {
	t.Helper()
	resp, err := b.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %s", req.Method, safeURL(req.URL), transportCause(err))
	}
	body := readAll(t, resp)
	return page{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(body), cookies: resp.Cookies()}
}

// safeURL returns scheme, host and path of u. The query is dropped: it carries
// the authorization code and the state of a callback or the session_code of a
// Keycloak form, and neither belongs in test output (AD-20).
func safeURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// transportCause names the failure of a transport error without the URL that
// *url.Error carries (that URL includes the query, see safeURL).
func transportCause(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func (b browser) get(t *testing.T, rawURL string) page {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.do(t, req)
}

func (b browser) postForm(t *testing.T, rawURL string, form url.Values) page {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(t, req)
}

func (p page) cookie(name string) *http.Cookie {
	for _, c := range p.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var (
	formTagRE    = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	actionAttrRE = regexp.MustCompile(`(?is)\baction\s*=\s*"([^"]*)"`)
	feedbackRE   = regexp.MustCompile(`(?is)<span[^>]*id="input-error[^"]*"[^>]*>(.*?)</span>`)
)

// loginAction returns the URL that the login form of Keycloak posts to, or ""
// when the page has no login form.
func loginAction(body string) string {
	for _, tag := range formTagRE.FindAllString(body, -1) {
		if !strings.Contains(tag, "kc-form-login") && !strings.Contains(tag, "login-actions/authenticate") {
			continue
		}
		if m := actionAttrRE.FindStringSubmatch(tag); m != nil {
			return html.UnescapeString(m[1])
		}
	}
	return ""
}

// resolve returns loc relative to base.
func resolve(base, loc string) string {
	b, err := url.Parse(base)
	if err != nil {
		return loc
	}
	l, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	return b.ResolveReference(l).String()
}

// authenticate walks the browser from the authorization URL through the
// Keycloak login form (a direct form post with the user's name and password) to
// the redirect back to the Hub, and returns that callback URL. It never follows
// the last redirect: the Hub receives it from the caller.
func authenticate(t *testing.T, b browser, authorizeURL, username, password string) string {
	t.Helper()
	next := authorizeURL
	for hop := 0; hop < 10; hop++ {
		p := b.get(t, next)
		switch {
		case p.status >= 300 && p.status < 400:
			loc := resolve(next, p.location)
			if strings.HasPrefix(loc, "http://"+hubAddr+oidc.CallbackPath) {
				return loc
			}
			next = loc
			continue
		case p.status != http.StatusOK:
			t.Fatalf("the authorization endpoint answered %d", p.status)
		}
		action := loginAction(p.body)
		if action == "" {
			t.Fatalf("the Keycloak page at hop %d has no login form (is the client in realm.json imported?)", hop)
		}
		post := b.postForm(t, resolve(next, action), url.Values{
			"username": {username}, "password": {password}, "credentialId": {""},
		})
		if post.status == http.StatusOK {
			msg := "no message"
			if m := feedbackRE.FindStringSubmatch(post.body); m != nil {
				msg = strings.TrimSpace(html.UnescapeString(m[1]))
			}
			t.Fatalf("Keycloak refused the login of %q: %s", username, msg)
		}
		if post.status < 300 || post.status >= 400 {
			t.Fatalf("the login form answered %d", post.status)
		}
		loc := resolve(action, post.location)
		if strings.HasPrefix(loc, "http://"+hubAddr+oidc.CallbackPath) {
			return loc
		}
		next = loc
	}
	t.Fatal("the authorization flow did not reach the Hub callback within 10 hops")
	return ""
}

// ---- the Hub under test ---------------------------------------------------------

type allowAll struct{}

func (allowAll) Allow(ratelimit.Policy, netip.Addr, string) (bool, time.Duration) { return true, 0 }

// capturingStore records the refresh token that each sign-in hands to the
// store, by subject, so that the test can play the background refresh with the
// very token the Hub keeps (the token is sealed in the database).
type capturingStore struct {
	oidc.Store
	mu     sync.Mutex
	tokens map[string]string
}

func (c *capturingStore) SyncOIDCLogin(ctx context.Context, p store.OIDCSyncParams) (store.OIDCSyncResult, error) {
	if p.RefreshToken != "" {
		c.mu.Lock()
		c.tokens[p.Subject] = p.RefreshToken
		c.mu.Unlock()
	}
	return c.Store.SyncOIDCLogin(ctx, p)
}

func (c *capturingStore) refreshTokenOf(subject string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[subject]
}

type hub struct {
	t     *testing.T
	kc    *keycloak
	st    *store.Store
	cs    *capturingStore
	cfg   oidc.Config
	srv   *httptest.Server
	audit *audit.Log
}

// signInResult is what a sign-in produced.
type signInResult struct {
	status   int
	location string
	body     string
	session  *session.Session // nil when no session was issued
	token    string           // the session cookie value; test-only, never logged
}

func startHub(t *testing.T, kc *keycloak) *hub {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := &hub{t: t, kc: kc, st: st, audit: audit.New(st)}
	h.cs = &capturingStore{Store: st, tokens: map[string]string{}}
	h.cfg = oidc.Config{
		ID: oidc.DefaultProviderID, Name: "Keycloak", Source: oidc.SourceSettings,
		Issuer: kc.issuer(), ClientID: clientID, ClientSecret: logx.Secret(clientSecret),
		RoleClaim: "groups", AdminValues: []string{groupAdmins}, UserValues: []string{groupUsers},
		// Plain HTTP Keycloak: only the explicit insecure flag allows it (R96).
		InsecureSkipVerify: true,
		Timeout:            15 * time.Second,
	}
	svc := &oidc.Service{
		Configs:  staticConfigs{h.cfg},
		Store:    h.cs,
		Sessions: session.NewManager(st),
		Limiter:  allowAll{},
		Audit:    h.audit,
		// The Hub shows generic error pages; the real cause (discovery,
		// issuer mismatch, code exchange, ID token) goes only to its log, which
		// is redacted (AD-20). Keep it in the test output so that a failed run
		// can be diagnosed. The server closes in t.Cleanup and waits for
		// in-flight requests, so nothing logs after the test ends.
		Log:      slog.New(slog.NewTextHandler(t.Output(), nil)),
		BaseURL:  func(*http.Request) string { return "http://" + hubAddr },
		TTL:      func(context.Context) (time.Duration, error) { return time.Hour, nil },
		Secure:   func(*http.Request) bool { return false },
		ClientIP: func(*http.Request) netip.Addr { return netip.MustParseAddr("192.0.2.1") },
	}
	mux := http.NewServeMux()
	svc.Register(mux)

	ln, err := net.Listen("tcp", hubAddr)
	if err != nil {
		t.Fatalf("listen on %s (the redirect URI of realm.json): %v", hubAddr, err)
	}
	h.srv = httptest.NewUnstartedServer(mux)
	h.srv.Listener = ln
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

type staticConfigs []oidc.Config

func (s staticConfigs) OIDCProviders(context.Context) ([]oidc.Config, error) { return s, nil }

// signIn drives the whole authorization-code flow for a user: start at the
// Hub, log in at Keycloak with a direct form post, hand the callback to the
// Hub. Every sign-in uses fresh browsers, so no earlier Keycloak session
// single-signs the user on.
func (h *hub) signIn(username string) signInResult {
	h.t.Helper()
	hubBrowser, kcBrowser := newBrowser(h.t), newBrowser(h.t)

	start := hubBrowser.get(h.t, h.srv.URL+oidc.LoginPath(oidc.DefaultProviderID))
	if start.status != http.StatusFound {
		h.t.Fatalf("the Hub did not start the sign-in: %d %s", start.status, start.body)
	}
	if !strings.HasPrefix(start.location, h.kc.issuer()+"/protocol/openid-connect/auth") {
		h.t.Fatalf("the Hub redirected to %q, not to the Keycloak authorization endpoint", safeURL(mustParse(h.t, start.location)))
	}
	q := mustParse(h.t, start.location).Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" {
		h.t.Fatalf("the authorization request lacks PKCE, state or nonce: %s", q.Encode())
	}

	callback := authenticate(h.t, kcBrowser, start.location, username, passwords[username])
	done := hubBrowser.get(h.t, callback)

	res := signInResult{status: done.status, location: done.location, body: done.body}
	if c := done.cookie(session.CookieName); c != nil && c.Value != "" && c.MaxAge >= 0 {
		s, err := session.NewManager(h.st).Resolve(context.Background(), c.Value)
		if err != nil {
			h.t.Fatalf("the session of %q does not resolve: %v", username, err)
		}
		res.session, res.token = s, c.Value
	}
	return res
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// idpRoles returns the source-idp roles of a Hub account, sorted.
func (h *hub) idpRoles(userID string) []string {
	h.t.Helper()
	rows, err := h.st.DB().Query(`SELECT role FROM user_roles WHERE user_id = ? AND source = ? ORDER BY role`, userID, oidc.SourceIDP)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *hub) countUsers() int {
	h.t.Helper()
	var n int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// subjectOf returns the Keycloak subject that a Hub account is linked to.
func (h *hub) subjectOf(userID string) string {
	h.t.Helper()
	var sub string
	err := h.st.DB().QueryRow(`SELECT subject FROM external_identities WHERE user_id = ?`, userID).Scan(&sub)
	if err != nil {
		h.t.Fatalf("the account %s has no external identity: %v", userID, err)
	}
	return sub
}

// refreshRoles stands in for the background role refresher of Story 6.3
// (AD-10), which is not on main yet. It plays the refresh-token grant and the
// role-rule evaluation itself, then replaces the source-idp roles of the
// account through the store (the call that the 6.3 refresher is planned to
// make; no Hub code calls it yet). It must be replaced by the 6.3 refresher
// (internal/hub/auth/oidc/refresh.go) once that story is merged, so that the
// test exercises the real persisted-token retrieval and token verification.
func (h *hub) refreshRoles(userID string) (claims map[string]any, before, after []string) {
	h.t.Helper()
	token := h.cs.refreshTokenOf(h.subjectOf(userID))
	if token == "" {
		h.t.Fatal("the sign-in stored no refresh token: does Keycloak issue one for this client?")
	}
	claims = h.kc.refresh(h.t, token)
	roles := h.cfg.Rules().Evaluate(claims)
	before, after, err := h.st.ReplaceIDPRoles(context.Background(), userID, roles)
	if err != nil {
		h.t.Fatalf("replace the IdP roles: %v", err)
	}
	return claims, before, after
}

// ---- tests --------------------------------------------------------------------

func requireRoles(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func requireSignedIn(t *testing.T, who string, r signInResult, wantRole string) *session.Session {
	t.Helper()
	if r.status != http.StatusSeeOther || r.location != "/" {
		t.Fatalf("%s: the callback answered %d (location %q), want 303 to /: %s", who, r.status, r.location, r.body)
	}
	if r.session == nil {
		t.Fatalf("%s: no session was issued", who)
	}
	if got := string(r.session.Role); got != wantRole {
		t.Errorf("%s: session role = %q, want %q", who, got, wantRole)
	}
	if r.session.Username == "" {
		t.Errorf("%s: the account has no user name (preferred_username was not used)", who)
	}
	return r.session
}

func TestKeycloakAdminGroupGivesAdminRole(t *testing.T) {
	kc := startKeycloak(t)
	h := startHub(t, kc)

	s := requireSignedIn(t, "alice", h.signIn("alice"), "admin")
	requireRoles(t, "alice's IdP roles", h.idpRoles(s.UserID), "admin")
	if s.Username != "alice" {
		t.Errorf("alice's account is named %q", s.Username)
	}
}

func TestKeycloakUsersGroupGivesUserRole(t *testing.T) {
	kc := startKeycloak(t)
	h := startHub(t, kc)

	s := requireSignedIn(t, "bob", h.signIn("bob"), "user")
	requireRoles(t, "bob's IdP roles", h.idpRoles(s.UserID), "user")
}

func TestKeycloakUserWithoutGroupIsRefused(t *testing.T) {
	kc := startKeycloak(t)
	h := startHub(t, kc)

	r := h.signIn("carol")
	if r.status != http.StatusForbidden {
		t.Fatalf("carol: the callback answered %d, want 403: %s", r.status, r.body)
	}
	if r.session != nil {
		t.Error("carol got a session without a group")
	}
	if !strings.Contains(r.body, "No access") {
		t.Errorf("carol did not get the no-access page: %s", r.body)
	}
	if n := h.countUsers(); n != 0 {
		t.Errorf("a sign-in without a group created %d account(s), want none", n)
	}
}

func TestKeycloakGroupRemovalAndRefreshDropsRole(t *testing.T) {
	kc := startKeycloak(t)
	h := startHub(t, kc)

	first := h.signIn("bob")
	s := requireSignedIn(t, "bob", first, "user")
	requireRoles(t, "bob's IdP roles", h.idpRoles(s.UserID), "user")

	// Take bob out of the group at Keycloak and put him back afterwards, so
	// that the other tests (and a rerun) see the imported realm.
	kc.setMember(t, "bob", groupUsers, false)
	t.Cleanup(func() { kc.setMember(t, "bob", groupUsers, true) })

	claims, before, after := h.refreshRoles(s.UserID)
	if v, ok := claims["groups"]; ok && containsValue(v, groupUsers) {
		t.Fatalf("Keycloak still lists bob in %s after the removal: %v", groupUsers, v)
	}
	requireRoles(t, "roles before the refresh", before, "user")
	requireRoles(t, "roles after the refresh", after)
	requireRoles(t, "bob's IdP roles in the database", h.idpRoles(s.UserID))

	// The same decision at the next sign-in: no group, no access, and the
	// session of the first sign-in ends with it. (The sign-in, not the
	// stand-in refresh above, ends the session: the store's role replacement
	// leaves sessions alone. What the Story 6.3 refresher does to the sessions
	// of an account that lost its last role is for that story to decide and
	// test; see the follow-up issue.)
	r := h.signIn("bob")
	if r.status != http.StatusForbidden || r.session != nil {
		t.Fatalf("bob without a group signed in: status %d, session %v", r.status, r.session != nil)
	}
	if _, err := session.NewManager(h.st).Resolve(context.Background(), first.token); !errors.Is(err, session.ErrNoSession) {
		t.Errorf("the session of bob's first sign-in still resolves after the refused sign-in (err %v)", err)
	}
	requireRoles(t, "bob's IdP roles after the refused sign-in", h.idpRoles(s.UserID))
}

func containsValue(v any, want string) bool {
	list, ok := v.([]any)
	if !ok {
		s, isString := v.(string)
		return isString && s == want
	}
	for _, e := range list {
		if s, ok := e.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestKeycloakRealmServesTheGroupClaim guards the fixture: the realm must put
// the group names (not full paths) into the ID token under "groups", which is
// the claim the role rules read.
func TestKeycloakRealmServesTheGroupClaim(t *testing.T) {
	kc := startKeycloak(t)
	h := startHub(t, kc)

	s := requireSignedIn(t, "alice", h.signIn("alice"), "admin")
	claims, _, _ := h.refreshRoles(s.UserID)
	if !containsValue(claims["groups"], groupAdmins) {
		t.Errorf("the ID token of alice has groups = %v, want it to contain %q", claims["groups"], groupAdmins)
	}
	if claims["preferred_username"] != "alice" {
		t.Errorf("preferred_username = %v", claims["preferred_username"])
	}
	if got, want := fmt.Sprint(claims["iss"]), kc.issuer(); got != want {
		t.Errorf("iss = %q, want %q", got, want)
	}
}
