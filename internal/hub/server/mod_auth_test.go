package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// authServices are the services the test probe grabs from the running Hub.
type authServices struct {
	authz   *authz.Authorizer
	audit   *audit.Log
	mgr     *session.Manager
	limiter *ratelimit.Limiter
	store   *store.Store
	setting *SettingsService
}

// authRegistry registers the modules of a real Hub up to the auth module and a
// probe module that serves GET /api/probe and POST /api/probe behind the
// session middleware, as the REST framework (Story 3.9) will.
func authRegistry(t *testing.T, out *authServices) *Registry {
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
	err := r.Register("probe", 60, func(_ context.Context, env *Env) error {
		mux, err := Require[*http.ServeMux](env)
		if err != nil {
			return err
		}
		mw, err := Require[*session.Middleware](env)
		if err != nil {
			return err
		}
		if out.authz, err = Require[*authz.Authorizer](env); err != nil {
			return err
		}
		if out.audit, err = Require[*audit.Log](env); err != nil {
			return err
		}
		if out.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		if out.limiter, err = Require[*ratelimit.Limiter](env); err != nil {
			return err
		}
		if out.store, err = Require[*store.Store](env); err != nil {
			return err
		}
		if out.setting, err = Require[*SettingsService](env); err != nil {
			return err
		}
		h := mw.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, _ := session.FromContext(r.Context())
			_, _ = io.WriteString(w, "user="+s.Username+" role="+string(s.Role))
		}))
		mux.Handle("GET /api/probe", h)
		mux.Handle("POST /api/probe", h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type hubClient struct {
	t    *testing.T
	base string
	c    *http.Client
	jar  *cookiejar.Jar
}

func newHubClient(t *testing.T, h *running, scheme string) *hubClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &hubClient{
		t: t, base: scheme + "://" + h.srv.Addr(), jar: jar,
		c: &http.Client{
			Jar: jar, Timeout: 30 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed test Hub
		},
	}
}

func (hc *hubClient) do(method, path string, body any, headers map[string]string) (int, string, *http.Response) {
	hc.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, hc.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.c.Do(req)
	if err != nil {
		hc.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

func (hc *hubClient) login(user, pw string) (int, string, string) {
	hc.t.Helper()
	code, body, _ := hc.do("POST", "/auth/login", map[string]string{"username": user, "password": pw}, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return code, body, out.CSRF
}

func (hc *hubClient) sessionCookie() *http.Cookie {
	u, _ := url.Parse(hc.base)
	for _, c := range hc.jar.Cookies(u) {
		if c.Name == session.CookieName {
			return c
		}
	}
	return nil
}

func TestAuthModuleSeedLoginLogoutOverTLS(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	var svc authServices
	dir := t.TempDir()
	h := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "https")

	// Not signed in: the protected route says unauthenticated.
	if code, _, _ := hc.do("GET", "/api/probe", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/probe without a session = %d", code)
	}
	// Wrong password.
	if code, body, _ := hc.login("root", "wrong"); code != http.StatusUnauthorized || !strings.Contains(body, `"unauthenticated"`) {
		t.Fatalf("wrong password = %d %s", code, body)
	}
	// Right password, by user name.
	code, body, csrf := hc.login("root", "integration-password-1")
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d %s", code, body)
	}
	ck := hc.sessionCookie()
	if ck == nil {
		t.Fatal("no session cookie in the jar")
	}
	if strings.Contains(h.logs.String(), "integration-password-1") || strings.Contains(h.logs.String(), ck.Value) {
		t.Errorf("a secret reached the log:\n%s", h.logs)
	}

	// Seed marker semantics: the admin exists with the admin role, and the
	// authorizer's user lookup is wired to the users table.
	u, err := svc.store.UserByIdentifier(context.Background(), "root")
	if err != nil || u.Role != "admin" || u.MustChangePassword {
		t.Fatalf("seeded user = %+v, %v", u, err)
	}
	if err := svc.authz.Decide(context.Background(), authz.NewSessionPrincipal(u.ID), ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: u.ID}); err != nil {
		t.Errorf("admin session principal is not allowed to manage: %v", err)
	}

	// The session works for GET; non-GET needs the CSRF header.
	if code, body, _ := hc.do("GET", "/api/probe", nil, nil); code != 200 || body != "user=root role=admin" {
		t.Fatalf("GET /api/probe = %d %q", code, body)
	}
	if code, body, _ := hc.do("POST", "/api/probe", nil, nil); code != http.StatusForbidden || !strings.Contains(body, `"forbidden"`) {
		t.Fatalf("POST without CSRF = %d %s", code, body)
	}
	if code, _, _ := hc.do("POST", "/api/probe", nil, map[string]string{session.CSRFHeader: "forged"}); code != http.StatusForbidden {
		t.Fatalf("POST with a forged CSRF token = %d", code)
	}
	if code, _, _ := hc.do("POST", "/api/probe", nil, map[string]string{session.CSRFHeader: csrf}); code != 200 {
		t.Fatalf("POST with CSRF = %d", code)
	}

	// Logout needs CSRF too, then the old cookie is dead.
	if code, _, _ := hc.do("POST", "/auth/logout", nil, nil); code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d", code)
	}
	if code, body, _ := hc.do("POST", "/auth/logout", nil, map[string]string{session.CSRFHeader: csrf}); code != 200 {
		t.Fatalf("logout = %d %s", code, body)
	}
	if hc.sessionCookie() != nil {
		t.Error("the cookie jar still holds the session after logout")
	}
	old := &http.Client{Transport: hc.c.Transport, Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", hc.base+"/api/probe", nil)
	req.AddCookie(ck)
	resp, err := old.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old cookie after logout = %d, want 401", resp.StatusCode)
	}

	// Every attempt is audited, none with a password.
	page, err := svc.audit.List(context.Background(), audit.Filter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var seed, ok, bad, out int
	for _, e := range page.Events {
		blob := e.Actor + e.Action + e.Target + e.Outcome
		if strings.Contains(blob, "integration-password") || strings.Contains(blob, "wrong") {
			t.Errorf("a password is in the audit log: %+v", e.Event)
		}
		switch {
		case e.Action == "user.seed":
			seed++
		case e.Action == "login" && e.Outcome == "success":
			ok++
		case e.Action == "login" && e.Outcome == "failed":
			bad++
		case e.Action == "logout" && e.Outcome == "success":
			out++
		}
	}
	if seed != 1 || ok != 1 || bad != 1 || out != 1 {
		t.Errorf("audit: seed=%d login ok=%d failed=%d logout=%d, want 1 each", seed, ok, bad, out)
	}
}

func TestAuthModuleRestartNeverResetsThePassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINION_ADMIN_PASSWORD", "first-password-123")
	var svc authServices
	h1 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	if code, body, _ := newHubClient(t, h1, "https").login("admin", "first-password-123"); code != 200 {
		t.Fatalf("login = %d %s", code, body)
	}
	h1.stop(t)

	// The operator changes the environment; the existing account is untouched.
	t.Setenv("MINION_ADMIN_PASSWORD", "second-password-456")
	h2 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	hc := newHubClient(t, h2, "https")
	if code, _, _ := hc.login("admin", "second-password-456"); code != http.StatusUnauthorized {
		t.Errorf("the new environment password works: %d (the seed must never reset a password)", code)
	}
	if code, body, _ := hc.login("admin", "first-password-123"); code != 200 {
		t.Errorf("the original password stopped working: %d %s", code, body)
	}
	if strings.Contains(h2.logs.String(), "initial admin account") {
		t.Errorf("the second start seeded again:\n%s", h2.logs)
	}
}

func TestAuthModuleGeneratedPasswordIsLoggedOnceAndMustChange(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "")
	dir := t.TempDir()
	var svc authServices
	h := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))

	logs := h.logs.String()
	if n := strings.Count(logs, "generated password"); n != 1 {
		t.Fatalf("the generated password was announced %d times:\n%s", n, logs)
	}
	var pw string
	for _, f := range strings.Fields(logs) {
		if v, ok := strings.CutPrefix(f, "password="); ok {
			pw = v
		}
	}
	if len(pw) != 24 || !strings.Contains(logs, "must_change_password=true") {
		t.Fatalf("log line lacks the password or the must-change mark:\n%s", logs)
	}
	hc := newHubClient(t, h, "https")
	code, body, _ := hc.login("admin", pw)
	if code != 200 || !strings.Contains(body, `"must_change_password":true`) {
		t.Fatalf("login with the generated password = %d %s", code, body)
	}
	h.stop(t)

	h2 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	if strings.Contains(h2.logs.String(), "password=") {
		t.Errorf("a later start printed a password:\n%s", h2.logs)
	}
}

func TestAuthModuleCookieIsNotSecureForPlainHTTPLoopback(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "loopback-password-1")
	var svc authServices
	h := startHub(t, &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://127.0.0.1:8080"}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "http")
	_, _, resp := hc.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "loopback-password-1"}, nil)
	var c *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == session.CookieName {
			c = x
		}
	}
	if c == nil || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %+v", c)
	}
}

func TestAuthModuleCookieIsSecureBehindAnHTTPSPublicURL(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "proxy-password-123")
	var svc authServices
	h := startHub(t, &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "https://hub.example.com"}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "http")
	_, _, resp := hc.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "proxy-password-123"}, nil)
	for _, x := range resp.Cookies() {
		if x.Name == session.CookieName {
			if !x.Secure {
				t.Fatalf("cookie behind an https public URL is not Secure: %+v", x)
			}
			return
		}
	}
	t.Fatal("no session cookie")
}

func TestAuthModuleRateLimitsRepeatedFailures(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "limited-password-123")
	var svc authServices
	h := startHub(t, &Config{DataDir: t.TempDir()}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "https")
	limited := false
	for range 25 {
		code, body, resp := hc.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "nope"}, nil)
		if code == http.StatusTooManyRequests {
			limited = true
			if !strings.Contains(body, `"rate_limited"`) || resp.Header.Get("Retry-After") == "" {
				t.Errorf("429 body %s, Retry-After %q", body, resp.Header.Get("Retry-After"))
			}
			break
		}
	}
	if !limited {
		t.Fatal("25 failed logins were never rate limited")
	}
	// The right password is refused while limited, and every attempt was audited.
	if code, _, _ := hc.login("admin", "limited-password-123"); code != http.StatusTooManyRequests {
		t.Errorf("right password while limited = %d", code)
	}
	page, err := svc.audit.List(context.Background(), audit.Filter{Action: "login", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	limitedEvents := 0
	for _, e := range page.Events {
		if e.Outcome == "rate_limited" {
			limitedEvents++
		}
	}
	if limitedEvents < 2 {
		t.Errorf("%d rate_limited audit events", limitedEvents)
	}
}

func TestAuthModuleSessionTTLSettingApplies(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "ttl-password-12345")
	t.Setenv("MINION_SESSION_TTL", "90m")
	var svc authServices
	h := startHub(t, &Config{DataDir: t.TempDir()}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "https")
	_, _, resp := hc.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "ttl-password-12345"}, nil)
	for _, x := range resp.Cookies() {
		if x.Name == session.CookieName {
			if d := time.Until(x.Expires); d < 85*time.Minute || d > 91*time.Minute {
				t.Errorf("cookie lifetime %v, want about 90m", d)
			}
			return
		}
	}
	t.Fatal("no session cookie")
}

func TestAuthModuleRejectsAnInvalidSetting(t *testing.T) {
	t.Setenv("MINION_SESSION_TTL", "1s")
	var svc authServices
	cfg := &Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}
	srv := New(cfg, authRegistry(t, &svc))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "session_ttl") {
		t.Fatalf("Run = %v, want a session_ttl error", err)
	}
}

func TestAuthModuleWiresTheUserLookupAndUnknownUsersAreDenied(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "lookup-password-123")
	var svc authServices
	startHub(t, &Config{DataDir: t.TempDir()}, authRegistry(t, &svc))
	err := svc.authz.Decide(context.Background(), authz.NewSessionPrincipal("ghost"),
		ops.MustActionSet(ops.ActionRead), authz.UserResource{ID: "ghost"})
	if err == nil {
		t.Fatal("an unknown user was allowed")
	}
	if errors.Is(err, authz.ErrNotWired) {
		t.Fatal("the auth module did not wire the user lookup")
	}
}

func TestAuthModuleInvalidPanelSessionTTLNeitherBlocksLoginNorStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINION_ADMIN_PASSWORD", "panel-ttl-password-1")
	var svc authServices
	h1 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	const bad = "1d-not-a-duration"
	if _, err := svc.setting.Set(context.Background(), admin, setArgs(config.KeySessionTTL, bad)); err != nil {
		t.Fatalf("the panel refused the value: %v", err)
	}

	checkLogin := func(h *running) {
		t.Helper()
		hc := newHubClient(t, h, "https")
		code, body, resp := hc.do("POST", "/auth/login", map[string]string{"username": "admin", "password": "panel-ttl-password-1"}, nil)
		if code != 200 {
			t.Fatalf("login with an invalid panel session_ttl = %d %s", code, body)
		}
		for _, c := range resp.Cookies() {
			if c.Name == session.CookieName {
				if d := time.Until(c.Expires); d < 23*time.Hour+55*time.Minute || d > 24*time.Hour+time.Minute {
					t.Errorf("cookie lifetime %v, want the default %v", d, config.DefaultSessionTTL)
				}
				return
			}
		}
		t.Fatal("no session cookie")
	}
	checkLogin(h1)
	logs := h1.logs.String()
	if !strings.Contains(logs, "key=session_ttl") || !strings.Contains(logs, "source=panel") || !strings.Contains(logs, "level=ERROR") {
		t.Errorf("no ERROR log naming the key and the source:\n%s", logs)
	}
	if strings.Contains(logs, bad) {
		t.Errorf("the invalid value is in the log:\n%s", logs)
	}
	h1.stop(t)

	// The next start on the same data directory works too.
	var svc2 authServices
	h2 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc2))
	checkLogin(h2)
	if !strings.Contains(h2.logs.String(), "source=panel") {
		t.Errorf("the restart did not log the fallback:\n%s", h2.logs)
	}
}

func TestAuthModuleRestartDoesNotNeedTheBootstrapSecretFile(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(secret, []byte("file-password-12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINION_ADMIN_PASSWORD", "")
	t.Setenv("MINION_ADMIN_PASSWORD_FILE", secret)
	var svc authServices
	h1 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	if code, body, _ := newHubClient(t, h1, "https").login("admin", "file-password-12345"); code != 200 {
		t.Fatalf("login = %d %s", code, body)
	}
	h1.stop(t)

	// The one-time secret is gone, the variable still points at it.
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	h2 := startHub(t, &Config{DataDir: dir}, authRegistry(t, &svc))
	if code, body, _ := newHubClient(t, h2, "https").login("admin", "file-password-12345"); code != 200 {
		t.Fatalf("login after the restart = %d %s", code, body)
	}

	// An empty data directory still needs it: the seed must not silently
	// generate a password when a configured file cannot be read.
	cfg := &Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Logger: testLogger(&syncBuf{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := New(cfg, authRegistry(t, &svc)).Run(ctx); err == nil {
		t.Fatal("a first start with an unreadable bootstrap secret file succeeded")
	}
}

func TestAuthModuleCrossSiteLoginIsRefusedOverTLS(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "cross-site-password-1")
	var svc authServices
	h := startHub(t, &Config{DataDir: t.TempDir()}, authRegistry(t, &svc))
	hc := newHubClient(t, h, "https")
	body := map[string]string{"username": "admin", "password": "cross-site-password-1"}
	for name, hdr := range map[string]map[string]string{
		"cross-site":     {"Sec-Fetch-Site": "cross-site"},
		"foreign origin": {"Origin": "https://evil.example"},
	} {
		code, out, resp := hc.do("POST", "/auth/login", body, hdr)
		if code != http.StatusForbidden || !strings.Contains(out, `"code":"forbidden"`) {
			t.Errorf("%s login = %d %s", name, code, out)
		}
		for _, c := range resp.Cookies() {
			if c.Name == session.CookieName {
				t.Errorf("%s login set a session cookie", name)
			}
		}
	}
	if code, out, _ := hc.do("POST", "/auth/login", body, map[string]string{"Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Errorf("same-origin login = %d %s", code, out)
	}
}
