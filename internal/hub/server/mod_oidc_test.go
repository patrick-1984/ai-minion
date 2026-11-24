package server

import (
	"context"
	"crypto/tls"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// oidcRegistry registers the modules of a real Hub up to the web module with
// the OIDC module in between, and grabs the OIDC services.
func oidcRegistry(t *testing.T, wr *web.Registry, handlers **api.OIDCHandlers, st **store.Store) *Registry {
	t.Helper()
	r := webRegistryWithoutWeb(t)
	registerOIDC(r, wr)
	registerWeb(r, wr)
	err := r.Register("oidc-probe", 60, func(_ context.Context, env *Env) error {
		var err error
		if handlers != nil {
			if *handlers, err = Require[*api.OIDCHandlers](env); err != nil {
				return err
			}
		}
		if st != nil {
			if *st, err = Require[*store.Store](env); err != nil {
				return err
			}
		}
		_, err = Require[*oidc.Service](env)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func webRegistryWithoutWeb(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	return r
}

func oidcEnv(t *testing.T, extra map[string]string) {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	secret := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(secret, []byte("client-secret-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The issuer does not answer: these tests are about the wiring, the flow
	// itself is tested against the fake issuer in internal/hub/auth/oidc.
	env := map[string]string{
		"MINION_OIDC_ISSUER":             "https://127.0.0.1:1",
		"MINION_OIDC_CLIENT_ID":          "minion",
		"MINION_OIDC_CLIENT_SECRET_FILE": secret,
		"MINION_OIDC_ROLE_CLAIM":         "groups",
		"MINION_OIDC_ADMIN_VALUES":       "admins",
		"MINION_OIDC_USER_VALUES":        "users",
		"MINION_OIDC_NAME":               "Acme",
		"MINION_OIDC_TIMEOUT":            "1s",
	}
	for k, v := range extra {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func getBodyWithCookie(t *testing.T, c *http.Client, url string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func sessionPage(t *testing.T) *web.Registry {
	t.Helper()
	wr := web.NewRegistry()
	if err := wr.RegisterPage(web.Page{Pattern: "/sessions", Title: "Sessions"}); err != nil {
		t.Fatal(err)
	}
	return wr
}

func TestOIDCModuleShowsTheProviderOfTheEnvironment(t *testing.T) {
	oidcEnv(t, nil)
	var handlers *api.OIDCHandlers
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, oidcRegistry(t, sessionPage(t), &handlers, nil))
	hc := newHubClient(t, h, "https")

	code, body, _ := hc.do("GET", "/login", nil, nil)
	if code != 200 || !strings.Contains(body, `href="/auth/oidc/default/login"`) || !strings.Contains(body, "Sign in with Acme") {
		t.Fatalf("login page = %d\n%s", code, body)
	}
	// The sign-in route is served; the IdP does not answer, so the page says so
	// and nothing is logged with the secret.
	code, body, _ = hc.do("GET", "/auth/oidc/default/login", nil, nil)
	if code != http.StatusBadGateway || !strings.Contains(body, "Identity provider unreachable") {
		t.Fatalf("sign-in with a dead IdP = %d\n%s", code, body)
	}
	code, body, _ = hc.do("GET", "/auth/oidc/missing/login", nil, nil)
	if code != http.StatusNotFound || !strings.Contains(body, "Unknown sign-in method") {
		t.Fatalf("unknown provider = %d", code)
	}
	if code, _, _ = hc.do("GET", "/auth/oidc/callback", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("callback without state = %d", code)
	}
	if strings.Contains(h.logs.String(), "client-secret-from-file") {
		t.Errorf("the client secret reached the log:\n%s", h.logs)
	}
	if strings.Contains(h.logs.String(), "DISABLED") {
		t.Errorf("an insecure warning without the flag:\n%s", h.logs)
	}

	// The operation handlers see the provider read-only, without its secret.
	list, err := handlers.ProviderList(context.Background(), ops.OIDCProviderListArgs{})
	if err != nil || len(list.Providers) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	p := list.Providers[0]
	if p.ID != "default" || !p.ReadOnly || p.Source != ops.OIDCSourceSettings || !p.ClientSecretSet || p.Name != "Acme" ||
		len(p.AdminValues) != 1 || p.AdminValues[0] != "admins" {
		t.Errorf("provider = %+v", p)
	}
}

func TestOIDCModuleWithoutProviderShowsNoButton(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, oidcRegistry(t, sessionPage(t), nil, nil))
	hc := newHubClient(t, h, "https")
	code, body, _ := hc.do("GET", "/login", nil, nil)
	if code != 200 || strings.Contains(body, "Sign in with") {
		t.Fatalf("login page = %d\n%s", code, body)
	}
	if code, _, _ = hc.do("GET", "/auth/oidc/default/login", nil, nil); code != http.StatusNotFound {
		t.Errorf("sign-in without a provider = %d", code)
	}
}

func TestOIDCModuleListsPanelProviders(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []store.OIDCProviderRow{
		{ID: "corp", Name: "Corp", Issuer: "https://corp.example", ClientID: "c", ClientSecret: "s", Enabled: true, CreatedAt: 1, UpdatedAt: 1},
		{ID: "off", Name: "Switched Off", Issuer: "https://off.example", ClientID: "c", Enabled: false, CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := st.PutOIDCProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReplaceRoleRules(ctx, "corp", "admin", []string{"ops"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	h := startHub(t, &Config{DataDir: dir}, oidcRegistry(t, sessionPage(t), nil, nil))
	hc := newHubClient(t, h, "https")
	_, body, _ := hc.do("GET", "/login", nil, nil)
	if !strings.Contains(body, `href="/auth/oidc/corp/login"`) || !strings.Contains(body, "Sign in with Corp") || strings.Contains(body, "Switched Off") {
		t.Fatalf("login page:\n%s", body)
	}
}

func TestInsecureFlagWarnsInTheLogAndOnAdminPages(t *testing.T) {
	oidcEnv(t, map[string]string{"MINION_OIDC_TLS_INSECURE_SKIP_VERIFY": "true"})
	var st *store.Store
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, oidcRegistry(t, sessionPage(t), nil, &st))
	hc := newHubClient(t, h, "https")

	// A warning at start, without the secret.
	logs := h.logs.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "TLS verification of the OIDC provider is DISABLED") {
		t.Fatalf("no warning at start:\n%s", logs)
	}

	// Permanently on the admin's pages.
	if code, _, csrf := hc.login("root", "integration-password-1"); code != 200 || csrf == "" {
		t.Fatalf("login = %d", code)
	}
	code, body, _ := hc.do("GET", "/sessions", nil, nil)
	if code != 200 || !strings.Contains(body, "TLS verification is turned off for the OIDC provider &#34;Acme&#34;") {
		t.Fatalf("admin page has no warning (%d):\n%s", code, body)
	}
	if code, body, _ = hc.do("GET", "/sessions", nil, nil); code != 200 || !strings.Contains(body, "turned off") {
		t.Fatalf("the warning is not permanent (%d)", code)
	}

	// A plain user does not see it: admin pages only.
	u := store.UserRow{ID: store.NewID(), Username: "plain", Active: true, CredentialVersion: 1}
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, credential_version, active, created_at, updated_at) VALUES (?, ?, 1, 1, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, u.ID, u.Username); err != nil {
		t.Fatal(err)
	}
	issued, err := session.NewManager(st).Create(context.Background(), u, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code, body = getBodyWithCookie(t, hc.c, hc.base+"/sessions", &http.Cookie{Name: session.CookieName, Value: issued.Token})
	if code != 200 || strings.Contains(body, "TLS verification is turned off") || !strings.Contains(body, "Sessions") {
		t.Fatalf("plain user page (%d):\n%s", code, body)
	}
}

func TestInsecureWarningForPanelProvider(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutOIDCProvider(context.Background(), store.OIDCProviderRow{ID: "lab", Name: "Lab", Issuer: "https://lab.example",
		ClientID: "c", InsecureSkipVerify: true, Enabled: true, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	h := startHub(t, &Config{DataDir: dir}, oidcRegistry(t, sessionPage(t), nil, nil))
	hc := newHubClient(t, h, "https")
	if !strings.Contains(h.logs.String(), "DISABLED") {
		t.Errorf("no warning at start:\n%s", h.logs)
	}
	hc.login("root", "integration-password-1")
	if _, body, _ := hc.do("GET", "/sessions", nil, nil); !strings.Contains(body, "OIDC provider &#34;Lab&#34;") {
		t.Errorf("no banner for the panel provider:\n%s", body)
	}
}

func TestInvalidOIDCEnvironmentStopsTheStart(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	t.Setenv("MINION_OIDC_ISSUER", "https://idp.example") // no client id
	srv := New(&Config{DataDir: hubstore.Dir(t), Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}, oidcRegistry(t, sessionPage(t), nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "OIDC settings") || !strings.Contains(err.Error(), "oidc_client_id") {
		t.Fatalf("Run = %v", err)
	}
}

// The issuer from the environment and the client id from the panel is a valid
// mix (AD-19): it must not stop the start.
func TestEnvironmentIssuerWithPanelClientIDStarts(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	t.Setenv("MINION_OIDC_ISSUER", "https://127.0.0.1:1")
	t.Setenv("MINION_OIDC_NAME", "Acme")
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSetting(context.Background(), config.KeyOIDCClientID, "panel-client", false, "test"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	h := startHub(t, &Config{DataDir: dir}, oidcRegistry(t, sessionPage(t), nil, nil))
	hc := newHubClient(t, h, "https")
	if code, body, _ := hc.do("GET", "/login", nil, nil); code != 200 || !strings.Contains(body, "Sign in with Acme") {
		t.Fatalf("login page (%d):\n%s", code, body)
	}
}

func TestOIDCSettingsAreLive(t *testing.T) {
	for _, key := range []string{config.KeyOIDCIssuer, config.KeyOIDCClientSecret, config.KeyOIDCAdminValues, config.KeyOIDCTLSInsecure} {
		if !IsLiveSetting(key) {
			t.Errorf("%s is not live", key)
		}
	}
}

// Through the composed Hub (A99): a session whose idle lifetime ran out is
// never served again. A page load the person makes on the Hub's own pages
// goes to the silent re-authentication with its provider (prompt=none) and
// back to the same page; a page load from another site or without evidence
// gets the sign-in page; htmx and polls get the quiet idle 401.
func TestExpiredOIDCSessionThroughTheHub(t *testing.T) {
	oidcEnv(t, nil)
	var st *store.Store
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, oidcRegistry(t, sessionPage(t), nil, &st))
	root, err := st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	tok := "expired-oidc-session-cookie-value-000000000"
	hash, _ := session.HashToken(tok)
	now := time.Now()
	if err := st.CreateSession(context.Background(), store.SessionRow{
		ID: "s-expired", TokenHash: hash, UserID: root.ID, CredentialVersion: root.CredentialVersion,
		CreatedAt: store.UnixMS(now.Add(-3 * time.Hour)), ExpiresAt: store.UnixMS(now.Add(-time.Hour)), AuthMethod: "oidc:default",
	}, store.UnixMS(now)); err != nil {
		t.Fatal(err)
	}
	base := "https://" + h.srv.Addr()
	c := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed test Hub
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	send := func(path string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: tok})
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	page := "/sessions?filter=a%2Fb"
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		code int
		loc  string
	}{
		{"page load", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate"},
			http.StatusSeeOther, "/auth/oidc/default/login?silent=1&next=" + url.QueryEscape(page)},
		{"typed address", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-User": "?1", "Sec-Fetch-Dest": "document"},
			http.StatusSeeOther, "/auth/oidc/default/login?silent=1&next=" + url.QueryEscape(page)},
		{"old browser with the Hub's Referer", map[string]string{"Accept": "text/html", "Referer": base + "/workers"},
			http.StatusSeeOther, "/auth/oidc/default/login?silent=1&next=" + url.QueryEscape(page)},
		{"page load from another site", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Dest": "document"},
			http.StatusSeeOther, "/login?next=" + url.QueryEscape(page)},
		{"old browser without evidence", map[string]string{"Accept": "text/html"},
			http.StatusSeeOther, "/login?next=" + url.QueryEscape(page)},
		{"htmx click", map[string]string{"Sec-Fetch-Site": "same-origin", "HX-Request": "true"}, http.StatusUnauthorized, ""},
		{"poll", map[string]string{"Sec-Fetch-Site": "same-origin", session.BackgroundHeader: "1"}, http.StatusUnauthorized, ""},
	} {
		resp := send(page, tc.hdr)
		if resp.StatusCode != tc.code || resp.Header.Get("Location") != tc.loc {
			t.Errorf("%s: %d %q, want %d %q", tc.name, resp.StatusCode, resp.Header.Get("Location"), tc.code, tc.loc)
		}
		if tc.code == http.StatusUnauthorized && resp.Header.Get(session.IdleHeader) != "idle" {
			t.Errorf("%s: no idle marker", tc.name)
		}
	}
	// None of it served or extended the expired session.
	if _, err := session.NewManager(st).Resolve(context.Background(), tok); err == nil {
		t.Error("the expired session resolves again")
	}
}
