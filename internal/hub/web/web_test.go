package web

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// fakeSessions is a SessionLayer for unit tests: the header X-Test-Session
// "user:role[:must]" attaches a session, no header means anonymous.
type fakeSessions struct{}

func (fakeSessions) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Test-Session"); v != "" {
			parts := strings.Split(v, ":")
			s := &session.Session{
				ID: "s1", UserID: "u-" + parts[0], Username: parts[0], Role: authz.Role(parts[1]),
				MustChangePassword: len(parts) > 2 && parts[2] == "must", CSRFToken: "csrf-" + parts[0],
			}
			// Further parts: "fresh" (a local sign-in just now), "oidc" (the
			// session started through the provider "corp").
			for _, extra := range parts[min(2, len(parts)):] {
				switch extra {
				case "fresh":
					s.AuthAt, s.AuthMethod = time.Now(), session.AuthMethodLocal
				case "oidc":
					s.AuthMethod = "oidc:corp"
				case "oidcfresh":
					s.AuthAt, s.AuthMethod = time.Now(), "oidc:corp"
				}
			}
			r = r.WithContext(session.WithSession(r.Context(), s))
		}
		next.ServeHTTP(w, r)
	})
}

// testTemplates is the embedded template set plus probe.html, a page whose
// content prints .Data and .Title.
func testTemplates(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(EmbeddedTemplates(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(EmbeddedTemplates(), p)
		m[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	m["probe.html"] = &fstest.MapFile{Data: []byte(`{{define "content"}}<h1>{{.Title}}</h1><p id="data">{{.Data}}</p>{{end}}`)}
	return m
}

// testContextKey is the inspector context key of the test Servers: a fixed key, the way the Hub
// gives one derived from its persisted data key (a Server cannot be built without one).
var testContextKey = bytes.Repeat([]byte{0x5a}, MinContextKey)

func newServer(t *testing.T, reg *Registry) *Server {
	t.Helper()
	s, err := New(Config{Registry: reg, Sessions: fakeSessions{}, Templates: testTemplates(t), ContextKey: testContextKey,
		Secure: func(*http.Request) bool { return false }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustHave(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body does not contain %q:\n%s", w, body)
		}
	}
}

func mustNotHave(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, w := range bad {
		if strings.Contains(body, w) {
			t.Errorf("body contains %q:\n%s", w, body)
		}
	}
}

// AC 1: no session -> redirect to /login, which renders the form and one
// button per registered provider.
func TestRootRedirectsAnonymousToLogin(t *testing.T) {
	s := newServer(t, NewRegistry())
	rec := get(t, s, "/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET / = %d %q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}
	// Any other path, existing or not, gives the same answer: no route leaks.
	for _, p := range []string{"/nope", "/account/password"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("GET %s = %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestLoginPageStructure(t *testing.T) {
	s := newServer(t, NewRegistry())
	rec := get(t, s, "/login")
	if rec.Code != 200 {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body,
		`<title>Sign in - Minion</title>`,
		`<form method="post" action="/auth/login" data-json-action="/auth/login" data-next="/">`,
		`name="username"`, `autocomplete="username"`,
		`name="password"`, `type="password"`, `autocomplete="current-password"`,
		`<div id="banner-slot"></div>`, `<div id="flash-area" aria-live="polite"></div>`,
		`<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`, `src="/static/htmx.min.js"`, `src="/static/sse.js"`, `src="/static/forms.js"`,
	)
	// No provider registered: no provider area; no navigation or CSRF for an
	// anonymous page.
	mustNotHave(t, body, `class="providers"`, `<nav`, `hx-headers`, `csrf-token`)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestLoginPageShowsOneButtonPerProvider(t *testing.T) {
	reg := NewRegistry()
	for _, p := range []LoginProvider{
		{ID: "oidc", Label: "Authentik <SSO>", Path: "/auth/oidc/start", Order: 2},
		{ID: "demo", Label: "Demo", Path: "/auth/demo/start", Order: 1},
	} {
		if err := reg.RegisterLoginProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, newServer(t, reg), "/login").Body.String()
	if n := strings.Count(body, `class="btn btn-secondary"`); n != 2 {
		t.Errorf("%d provider buttons, want 2:\n%s", n, body)
	}
	if strings.Index(body, "Sign in with Demo") > strings.Index(body, "Sign in with Authentik") {
		t.Errorf("providers are not sorted by Order:\n%s", body)
	}
	mustHave(t, body, `href="/auth/oidc/start"`, `Sign in with Authentik &lt;SSO&gt;`)
	mustNotHave(t, body, `<SSO>`)
}

func TestLoginProviderValidation(t *testing.T) {
	reg := NewRegistry()
	bad := []LoginProvider{
		{ID: "", Label: "x", Path: "/a"},
		{ID: "UP", Label: "x", Path: "/a"},
		{ID: "ok", Label: "", Path: "/a"},
		{ID: "ok", Label: "x", Path: "https://evil.example/a"},
		{ID: "ok", Label: "x", Path: "//evil.example/a"},
		{ID: "ok", Label: "x", Path: "relative"},
	}
	for _, p := range bad {
		if err := reg.RegisterLoginProvider(p); err == nil {
			t.Errorf("RegisterLoginProvider(%+v) accepted", p)
		}
	}
	good := LoginProvider{ID: "ok", Label: "x", Path: "/a"}
	if err := reg.RegisterLoginProvider(good); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterLoginProvider(good); err == nil {
		t.Error("a duplicate provider ID was accepted")
	}
}

func TestLoginNextIsSameOriginOnly(t *testing.T) {
	s := newServer(t, NewRegistry())
	for next, want := range map[string]string{
		"/sessions?x=1":       `data-next="/sessions?x=1"`,
		"https://evil.test/":  `data-next="/"`,
		"//evil.test/":        `data-next="/"`,
		`/\evil.test`:         `data-next="/"`,
		"javascript:alert(1)": `data-next="/"`,
	} {
		body := get(t, s, "/login?next="+url.QueryEscape(next)).Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("next=%q: want %s in\n%s", next, want, body)
		}
	}
}

func TestSignedInLoginRedirects(t *testing.T) {
	s := newServer(t, NewRegistry())
	rec := get(t, s, "/login?next=/x", "X-Test-Session", "ann:user")
	if rec.Code != 303 || rec.Header().Get("Location") != "/x" {
		t.Errorf("signed in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = get(t, s, "/login", "X-Test-Session", "ann:user:must")
	if rec.Code != 303 || rec.Header().Get("Location") != "/account/password" {
		t.Errorf("must change: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// AC 2: a must-change account is forced to /account/password first.
func TestMustChangeForcedToPasswordPage(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterPage(Page{Pattern: "/probe", Title: "Probe", Template: "probe.html"}); err != nil {
		t.Fatal(err)
	}
	s := newServer(t, reg)
	for _, p := range []string{"/", "/probe", "/missing"} {
		rec := get(t, s, p, "X-Test-Session", "ann:user:must")
		if rec.Code != 303 || rec.Header().Get("Location") != "/account/password" {
			t.Errorf("GET %s = %d %q, want 303 /account/password", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// htmx cannot follow a redirect to a page: it gets HX-Redirect.
	rec := get(t, s, "/probe", "X-Test-Session", "ann:user:must", "HX-Request", "true")
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") != "/account/password" {
		t.Errorf("htmx: %d %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	rec = get(t, s, "/account/password", "X-Test-Session", "ann:user:must")
	if rec.Code != 200 {
		t.Fatalf("GET /account/password = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(),
		"You must choose a new password",
		`data-json-action="`+PasswordChangeURL+`"`,
		`name="old_password"`, `name="new_password"`, `name="confirm_password"`,
		`data-match="new_password,confirm_password"`)
	// A normal account may open the page too, without the notice.
	rec = get(t, s, "/account/password", "X-Test-Session", "ann:user")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "You must choose") {
		t.Errorf("normal account: %d\n%s", rec.Code, rec.Body)
	}
}

// Without JavaScript a native submit must never put a password into the URL:
// every form with a password field posts (not GET) to a fixed path.
func TestPasswordFormsNeverSubmitViaGET(t *testing.T) {
	s := newServer(t, NewRegistry())
	formRe := regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)
	pages := []struct{ path, session string }{
		{"/login", ""},
		{"/account/password", "ann:user"},
	}
	for _, pg := range pages {
		var rec *httptest.ResponseRecorder
		if pg.session == "" {
			rec = get(t, s, pg.path)
		} else {
			rec = get(t, s, pg.path, "X-Test-Session", pg.session)
		}
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", pg.path, rec.Code)
		}
		n := 0
		for _, m := range formRe.FindAllStringSubmatch(rec.Body.String(), -1) {
			if !strings.Contains(m[2], `type="password"`) {
				continue
			}
			n++
			attrs := strings.ToLower(m[1])
			if !strings.Contains(attrs, `method="post"`) {
				t.Errorf("%s: password form lacks method=\"post\": <form%s>", pg.path, m[1])
			}
			if !regexp.MustCompile(`action="/[^"?#]*"`).MatchString(attrs) {
				t.Errorf("%s: password form lacks a fixed same-origin action: <form%s>", pg.path, m[1])
			}
		}
		if n == 0 {
			t.Errorf("%s: no password form found", pg.path)
		}
	}
}

// AC 3: registered pages appear in the navigation, filtered by role, without
// editing the layout.
func TestNavFilteredByRole(t *testing.T) {
	reg := NewRegistry()
	for _, p := range []Page{
		{Pattern: "/sessions", Title: "Sessions"},
		{Pattern: "/admin/users", Title: "Users", Roles: []authz.Role{authz.RoleAdmin}},
	} {
		if err := reg.RegisterPage(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []NavItem{
		{Label: "Users", Path: "/admin/users", Order: 20, Roles: []authz.Role{authz.RoleAdmin}},
		{Label: "Sessions", Path: "/sessions", Order: 10},
		{Label: "Audit", Path: "/admin/audit", Order: 30, Roles: []authz.Role{authz.RoleAdmin}},
	} {
		if err := reg.RegisterNav(n); err != nil {
			t.Fatal(err)
		}
	}
	s := newServer(t, reg)

	user := get(t, s, "/sessions", "X-Test-Session", "ann:user").Body.String()
	mustHave(t, user, `<a href="/sessions" aria-current="page"><span>Sessions</span></a>`)
	mustNotHave(t, user, "Users", "Audit", "/admin/")

	admin := get(t, s, "/sessions", "X-Test-Session", "root:admin").Body.String()
	mustHave(t, admin, `href="/admin/users"><span>Users</span></a>`, `href="/admin/audit"><span>Audit</span></a>`)
	iS, iU, iA := strings.Index(admin, ">Sessions<"), strings.Index(admin, ">Users<"), strings.Index(admin, ">Audit<")
	if iS >= iU || iU >= iA {
		t.Errorf("navigation is not in Order:\n%s", admin)
	}
	mustHave(t, admin, "Administrator", `<span class="identity-name">root</span>`)
	// The administration group is only there for an administrator.
	mustHave(t, admin, `id="nav-admin"`)
	mustNotHave(t, user, `id="nav-admin"`, "Administrator")

	// The role filter of a page answers 403 with the layout, never the page.
	rec := get(t, s, "/admin/users", "X-Test-Session", "ann:user")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowed") {
		t.Errorf("user on admin page = %d", rec.Code)
	}
	if rec := get(t, s, "/admin/users", "X-Test-Session", "root:admin"); rec.Code != 200 {
		t.Errorf("admin on admin page = %d", rec.Code)
	}
}

func TestNavActiveMatching(t *testing.T) {
	for _, c := range []struct {
		nav, path string
		want      bool
	}{
		{"/", "/", true}, {"/", "/sessions", false},
		{"/sessions", "/sessions", true}, {"/sessions", "/sessions/x", true},
		{"/sessions", "/sessionsx", false}, {"/admin/", "/admin/users", true},
	} {
		if got := navActive(c.nav, c.path); got != c.want {
			t.Errorf("navActive(%q,%q) = %v", c.nav, c.path, got)
		}
	}
}

func TestRegistryValidation(t *testing.T) {
	reg := NewRegistry()
	badPages := []Page{
		{Pattern: ""},
		{Pattern: "relative"},
		{Pattern: "POST /x"},
		{Pattern: "/api/v1/x"},
		{Pattern: "/login"},
		{Pattern: "/static/x"},
		{Pattern: "/auth/x"},
		{Pattern: "/account/password"},
		{Pattern: "/ok", Template: "../etc/passwd"},
		{Pattern: "/ok", Template: "layout.html"},
		{Pattern: "/ok", Template: "x.txt"},
		{Pattern: "/ok", Roles: []authz.Role{"root"}},
	}
	for _, p := range badPages {
		if err := reg.RegisterPage(p); err == nil {
			t.Errorf("RegisterPage(%+v) accepted", p)
		}
	}
	if err := reg.RegisterPage(Page{Pattern: "/ok"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterPage(Page{Pattern: "GET /ok"}); err == nil {
		t.Error("a duplicate page was accepted")
	}
	if err := reg.RegisterRoute(Route{Pattern: "GET /ok", Handler: http.NotFoundHandler()}); err == nil {
		t.Error("a route on a page's pattern was accepted")
	}
	if err := reg.RegisterRoute(Route{Pattern: "POST /ok"}); err == nil {
		t.Error("a route without a handler was accepted")
	}
	if err := reg.RegisterNav(NavItem{Label: "", Path: "/x"}); err == nil {
		t.Error("nav without a label accepted")
	}
	if err := reg.RegisterNav(NavItem{Label: "x", Path: "http://evil/x"}); err == nil {
		t.Error("nav with an external path accepted")
	}
	if err := reg.RegisterBanner(nil); err == nil {
		t.Error("a nil banner source was accepted")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the package-level RegisterPage did not panic on invalid input")
			}
		}()
		RegisterPage(Page{Pattern: "nope"})
	}()
}

func TestNewFailsOnMissingTemplateAndConflict(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/x", Template: "missing.html"})
	if _, err := New(Config{Registry: reg, Sessions: fakeSessions{}, ContextKey: testContextKey}); err == nil || !strings.Contains(err.Error(), "missing.html") {
		t.Errorf("New with a missing template: %v", err)
	}
	if _, err := New(Config{Registry: NewRegistry()}); err == nil {
		t.Error("New without a session layer succeeded")
	}
	// There is no process-random fallback: without a context key (or with a short one) there is no Server.
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{1}, MinContextKey-1)} {
		if _, err := New(Config{Registry: NewRegistry(), Sessions: fakeSessions{}, ContextKey: key}); err == nil || !strings.Contains(err.Error(), "context key") {
			t.Errorf("New with a %d-byte context key: %v", len(key), err)
		}
	}
	// Two registrations that net/http would reject as conflicting.
	reg = NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/a/{x}"})
	_ = reg.RegisterPage(Page{Pattern: "/{y}/b"})
	if _, err := New(Config{Registry: reg, Sessions: fakeSessions{}, ContextKey: testContextKey}); err == nil {
		t.Error("New with conflicting patterns succeeded")
	}
}

// AC 4: rendered values are escaped, and htmx requests carry the CSRF header.
func TestEscaping(t *testing.T) {
	const evil = `<script>alert("x")</script>`
	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/probe", Title: evil, Template: "probe.html",
		Data: func(*http.Request) (any, error) { return evil, nil }})
	_ = reg.RegisterNav(NavItem{Label: evil, Path: "/probe"})
	_ = reg.RegisterBanner(func(*http.Request) []Banner {
		return []Banner{{Kind: `"><script>`, Text: evil}}
	})
	s := newServer(t, reg)
	req := httptest.NewRequest("GET", "/probe", nil)
	req.Header.Set("X-Test-Session", "<b>ann</b>:user")
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: flashValue(t, Flash{Level: FlashWarning, Text: evil})})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status %d\n%s", rec.Code, body)
	}
	mustNotHave(t, body, "<script>alert", `<script>"`, "<b>ann</b>", `"><script>`)
	if n := strings.Count(body, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;"); n != 7 {
		t.Errorf("escaped payload appears %d times, want 7 (title, breadcrumb, h1, data, nav, banner, flash):\n%s", n, body)
	}
	mustHave(t, body, `<span class="identity-name">&lt;b&gt;ann&lt;/b&gt;</span>`, "User", `class="banner banner-info"`)
}

func TestCSRFHeaderForHTMX(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/probe", Title: "P", Template: "probe.html"})
	s := newServer(t, reg)
	body := get(t, s, "/probe", "X-Test-Session", "ann:user").Body.String()
	mustHave(t, body,
		`hx-headers='{"X-Minion-CSRF":"csrf-ann"}'`,
		`<meta name="csrf-token" content="csrf-ann">`,
		`<form data-json-action="/auth/logout" data-next="/login">`)
}

// The layout has a banner slot and a flash area.
func TestBannerSlotAndFlash(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterBanner(func(r *http.Request) []Banner {
		return []Banner{
			{Kind: "warning", Text: "Acting as ann", Action: &BannerAction{Label: "Stop", URL: "/api/v1/ops/act_as_stop"}},
			{Kind: "danger", Text: "bad action", Action: &BannerAction{Label: "Go", URL: "https://evil.test/x"}},
		}
	})
	s := newServer(t, reg)

	// Anonymous pages show no banners.
	mustNotHave(t, get(t, s, "/login").Body.String(), "Acting as")

	rec := get(t, s, "/", "X-Test-Session", "root:admin")
	body := rec.Body.String()
	mustHave(t, body,
		`<div class="banner banner-warning" role="status"><span>Acting as ann</span>`,
		`<form data-json-action="/api/v1/ops/act_as_stop"><button type="submit" class="btn-secondary">Stop</button></form>`,
		`banner-danger`)
	mustNotHave(t, body, "evil.test")
	mustHave(t, body, `<main id="content"`)

	// A flash is shown once and the cookie is cleared.
	w := httptest.NewRecorder()
	AddFlash(w, httptest.NewRequest("GET", "/", nil), FlashWarning, "Backup is older than a week")
	cookie := w.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Test-Session", "root:admin")
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	mustHave(t, rec.Body.String(), `<div class="flash flash-warning" role="status">Backup is older than a week</div>`)
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the flash cookie was not cleared")
	}
}

// Two AddFlash calls on one response must both reach the browser: it keeps
// only one cookie per name, so the second call has to extend the first.
func TestAddFlashTwiceInOneResponse(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: flashCookie, Value: flashValue(t, Flash{Level: FlashInfo, Text: "carried"})})
	AddFlash(w, r, FlashWarning, "first")
	AddFlash(w, r, FlashDanger, "second")
	var got []*http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == flashCookie {
			got = append(got, c)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d flash Set-Cookie lines, want 1", len(got))
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(got[0])
	list := readFlashes(r2)
	if len(list) != 3 || list[0].Text != "carried" || list[1].Text != "first" || list[2].Text != "second" || list[2].Level != FlashDanger {
		t.Errorf("flashes = %+v, want carried, first, second", list)
	}
}

func flashValue(t *testing.T, f ...Flash) string {
	t.Helper()
	w := httptest.NewRecorder()
	for _, x := range f {
		AddFlash(w, httptest.NewRequest("GET", "/", nil), x.Level, x.Text)
	}
	return w.Result().Cookies()[0].Value
}

func TestFlashLimitsAndMalformedCookie(t *testing.T) {
	var carried *http.Cookie
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		if carried != nil {
			r.AddCookie(carried)
		}
		w := httptest.NewRecorder()
		AddFlash(w, r, "bogus-level", strings.Repeat("x", 100))
		carried = w.Result().Cookies()[0]
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(carried)
	if got := readFlashes(r); len(got) != maxFlashes || got[0].Level != FlashInfo {
		t.Errorf("after 6 flashes: %d kept, first %+v", len(got), got[0])
	}
	for _, bad := range []string{"!!!", "e30", "bm90LWpzb24"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: flashCookie, Value: bad})
		if got := readFlashes(req); len(got) != 0 {
			t.Errorf("malformed flash cookie %q gave %v", bad, got)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: flashValue(t, Flash{Level: "bogus", Text: strings.Repeat("\u00e9", 1000)})})
	got := readFlashes(req)
	if len(got) != 1 || got[0].Level != FlashInfo || len([]rune(got[0].Text)) != maxFlashRunes {
		t.Errorf("flash normalisation: %+v", got)
	}
}

func TestPageDataErrors(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/workers/{id}", Title: "W", Data: func(r *http.Request) (any, error) {
		switch r.PathValue("id") {
		case "missing":
			return nil, ErrNotFound()
		case "boom":
			return nil, io.ErrUnexpectedEOF
		}
		return nil, nil
	}})
	s := newServer(t, reg)
	h := []string{"X-Test-Session", "ann:user"}
	if rec := get(t, s, "/workers/missing", h...); rec.Code != 404 || !strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("missing: %d", rec.Code)
	}
	rec := get(t, s, "/workers/boom", h...)
	if rec.Code != 500 {
		t.Errorf("boom: %d", rec.Code)
	}
	mustNotHave(t, rec.Body.String(), "unexpected EOF")
	if rec := get(t, s, "/workers/w1", h...); rec.Code != 200 {
		t.Errorf("ok: %d", rec.Code)
	}
	if rec := get(t, s, "/unknown", h...); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

func TestRoutesAreGated(t *testing.T) {
	reg := NewRegistry()
	_ = reg.RegisterRoute(Route{Pattern: "POST /fragment", Roles: []authz.Role{authz.RoleAdmin},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "frag") })})
	s := newServer(t, reg)
	post := func(hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/fragment", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
		t.Errorf("anonymous POST = %d %s", rec.Code, rec.Body)
	}
	if rec := post("X-Test-Session", "ann:user"); rec.Code != 403 {
		t.Errorf("user POST = %d", rec.Code)
	}
	if rec := post("X-Test-Session", "root:admin"); rec.Code != 200 || rec.Body.String() != "frag" {
		t.Errorf("admin POST = %d %s", rec.Code, rec.Body)
	}
	if rec := post("X-Test-Session", "root:admin:must"); rec.Header().Get("Location") != "/account/password" {
		t.Errorf("must-change POST = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHomeDefaultAndOverride(t *testing.T) {
	s := newServer(t, NewRegistry())
	rec := get(t, s, "/", "X-Test-Session", "ann:user")
	mustHave(t, rec.Body.String(), "Welcome, ann")

	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/{$}", Title: "Dashboard", Template: "probe.html"})
	s = newServer(t, reg)
	rec = get(t, s, "/", "X-Test-Session", "ann:user")
	mustHave(t, rec.Body.String(), "<h1>Dashboard</h1>")
	mustNotHave(t, rec.Body.String(), "Welcome")

	// A bare "/" is a catch-all in net/http and would turn every unknown path
	// into the home page, so it is rejected; "/{$}" is the home page.
	reg = NewRegistry()
	err := reg.RegisterPage(Page{Pattern: "/", Title: "Dashboard", Template: "probe.html"})
	if err == nil || !strings.Contains(err.Error(), "{$}") {
		t.Errorf(`RegisterPage("/") = %v, want an error that names "/{$}"`, err)
	}
	if err := reg.RegisterRoute(Route{Pattern: "GET /", Handler: http.NotFoundHandler()}); err == nil {
		t.Error(`RegisterRoute("GET /") succeeded`)
	}
	// An unknown path still answers 404 for a signed-in user.
	for _, r := range []*Registry{NewRegistry(), reg} {
		s = newServer(t, r)
		rec = get(t, s, "/unknown", "X-Test-Session", "ann:user")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /unknown = %d, want 404", rec.Code)
		}
	}
}

// An anonymous htmx request is sent to the login page with the page the user
// is on as next, not the fragment URL.
func TestHTMXLoginRedirectUsesCurrentURL(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterPage(Page{Pattern: "/fragment", Title: "F", Template: "probe.html"}); err != nil {
		t.Fatal(err)
	}
	s := newServer(t, reg)
	for _, c := range []struct{ current, want string }{
		{"https://hub.example/sessions?x=1", "/login?next=%2Fsessions%3Fx%3D1"},
		{"https://evil.example//evil.test/x", "/login"},
		{"https://hub.example/", "/login"},
		{"::bad url::", "/login"},
		{"", "/login"},
	} {
		hdr := []string{"HX-Request", "true"}
		if c.current != "" {
			hdr = append(hdr, "HX-Current-URL", c.current)
		}
		rec := get(t, s, "/fragment", hdr...)
		if rec.Code != 200 || rec.Header().Get("HX-Redirect") != c.want {
			t.Errorf("HX-Current-URL %q: %d HX-Redirect %q, want %q", c.current, rec.Code, rec.Header().Get("HX-Redirect"), c.want)
		}
	}
}

// AddFlash takes the Secure attribute of the cookie from the Server that
// serves the request, so handlers registered by other stories need no value.
func TestAddFlashSecureFollowsServer(t *testing.T) {
	for _, secure := range []bool{false, true} {
		reg := NewRegistry()
		if err := reg.RegisterRoute(Route{Pattern: "POST /flashme", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			AddFlash(w, r, FlashInfo, "saved")
			w.WriteHeader(http.StatusNoContent)
		})}); err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{Registry: reg, Sessions: fakeSessions{}, Templates: testTemplates(t), ContextKey: testContextKey,
			Secure: func(*http.Request) bool { return secure }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/flashme", nil)
		req.Header.Set("X-Test-Session", "ann:user")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		cs := rec.Result().Cookies()
		if len(cs) != 1 || cs[0].Name != flashCookie || cs[0].Secure != secure || !cs[0].HttpOnly {
			t.Errorf("Server secure=%v: cookies %+v", secure, cs)
		}
	}
	// Without a Server in front, the safe default applies.
	w := httptest.NewRecorder()
	AddFlash(w, httptest.NewRequest("GET", "/", nil), FlashInfo, "x")
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("no Server: cookies %+v, want Secure", c)
	}
}

// A form without its own error element (sign out, banner actions) must still
// report a failed request: run the real forms.js against a DOM stub.
func TestFormsJSReportsFailedLogout(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "forms_failed_test.js"))
	if err != nil {
		t.Fatal(err)
	}
	forms, err := filepath.Abs(filepath.Join("static", "forms.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, forms).CombinedOutput()
	if err != nil {
		t.Fatalf("forms.js: %v\n%s", err, out)
	}
}

// The change-password form is the only browser caller of a write operation:
// the body forms.js builds from the rendered page, with no field added by a
// test helper, must pass the operation's own validation (it needs a UUIDv7
// request_id the form fields do not carry).
func TestChangePasswordFormPayloadValidates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	s := newServer(t, NewRegistry())
	rec := get(t, s, "/account/password", "X-Test-Session", "ann:user:must")
	if rec.Code != 200 {
		t.Fatalf("GET /account/password = %d", rec.Code)
	}
	var m string
	for _, f := range regexp.MustCompile(`(?s)<form([^>]*)>(.*?)</form>`).FindAllStringSubmatch(rec.Body.String(), -1) {
		if strings.Contains(f[2], `name="old_password"`) {
			m = f[1]
		}
	}
	if m == "" {
		t.Fatal("no change-password form on the page")
	}
	dataset := map[string]string{}
	for _, a := range regexp.MustCompile(`data-([a-z-]+)(?:="([^"]*)")?`).FindAllStringSubmatch(m, -1) {
		parts := strings.Split(a[1], "-")
		for i := 1; i < len(parts); i++ {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
		dataset[strings.Join(parts, "")] = a[2]
	}
	spec, err := json.Marshal(map[string]any{
		"dataset": dataset,
		"fields": map[string]string{
			"old_password": "old-password-123", "new_password": "new-password-456", "confirm_password": "new-password-456",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("testdata", "forms_payload_test.js"))
	if err != nil {
		t.Fatal(err)
	}
	forms, err := filepath.Abs(filepath.Join("static", "forms.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, forms, string(spec)).Output()
	if err != nil {
		t.Fatalf("forms.js: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &body); err != nil {
		t.Fatalf("payload %q: %v", out, err)
	}
	if _, ok := body["confirm_password"]; ok {
		t.Errorf("confirm_password was sent: %v", body)
	}
	var args ops.PasswordChangeArgs
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(out)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if err := args.Validate(); err != nil {
		t.Fatalf("the form payload is rejected by the operation: %v", err)
	}
	if !ops.ValidUUIDv7(args.RequestID) {
		t.Errorf("request_id %q is not a UUIDv7", args.RequestID)
	}
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got byte for byte with testdata/<name>.golden; -update
// rewrites the file.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run go test -update after checking the change)\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func TestGoldenLoginAnonymous(t *testing.T) {
	rec := get(t, newServer(t, NewRegistry()), "/login")
	if rec.Code != 200 {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	golden(t, "login_anonymous", rec.Body.String())
}

func TestGoldenLoginProviders(t *testing.T) {
	reg := NewRegistry()
	for _, p := range []LoginProvider{
		{ID: "oidc", Label: "Authentik SSO", Path: "/auth/oidc/start", Order: 2},
		{ID: "demo", Label: "Demo", Path: "/auth/demo/start", Order: 1},
	} {
		if err := reg.RegisterLoginProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	rec := get(t, newServer(t, reg), "/login?next=/sessions")
	golden(t, "login_providers", rec.Body.String())
}

func TestGoldenSignedInPage(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []NavItem{
		{Label: "Sessions", Path: "/sessions", Order: 10},
		{Label: "Users", Path: "/admin/users", Order: 20, Roles: []authz.Role{authz.RoleAdmin}},
	} {
		if err := reg.RegisterNav(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterPage(Page{Pattern: "/sessions", Title: "Sessions", Template: "probe.html"}); err != nil {
		t.Fatal(err)
	}
	_ = reg.RegisterBanner(func(*http.Request) []Banner {
		return []Banner{{Kind: "warning", Text: "Acting as ann", Action: &BannerAction{Label: "Stop", URL: "/api/v1/ops/act_as_stop"}}}
	})
	s := newServer(t, reg)
	w := httptest.NewRecorder()
	AddFlash(w, httptest.NewRequest("GET", "/", nil), FlashDanger, "Backup is older than a week")
	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("X-Test-Session", "root:admin")
	req.AddCookie(w.Result().Cookies()[0])
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /sessions = %d", rec.Code)
	}
	golden(t, "signed_in_page", rec.Body.String())
}

// Static assets are embedded with go:embed and served without a session.
func TestStaticAssetsEmbedded(t *testing.T) {
	s := newServer(t, NewRegistry())
	for path, want := range map[string]string{
		"/static/htmx.min.js": `version:"2.0.11"`,
		"/static/sse.js":      "Server Sent Events Extension",
		"/static/minion.css":  "--accent",
		"/static/forms.js":    "X-Minion-CSRF",
		"/static/favicon.svg": "<svg",
	} {
		rec := get(t, s, path)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, missing %q", path, rec.Code, want)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") && !strings.HasPrefix(ct, "text/css") && !strings.HasPrefix(ct, "image/svg+xml") {
			t.Errorf("GET %s Content-Type = %q", path, ct)
		}
		// Revalidation with the ETag answers 304.
		rec2 := get(t, s, path, "If-None-Match", rec.Header().Get("ETag"))
		if rec2.Code != http.StatusNotModified {
			t.Errorf("GET %s with ETag = %d", path, rec2.Code)
		}
	}
	for _, p := range []string{"/static/", "/static/README.md", "/static/../layout.html", "/static/nope.js"} {
		if rec := get(t, s, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/static/forms.js", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST static = %d", rec.Code)
	}
}

func TestEmbeddedFilesPresent(t *testing.T) {
	for _, f := range []string{"layout.html", "login.html", "change_password.html"} {
		if _, err := fs.Stat(EmbeddedTemplates(), f); err != nil {
			t.Errorf("template %s is not embedded: %v", f, err)
		}
	}
	if _, err := fs.Stat(EmbeddedStatic(), "README.md"); err == nil {
		t.Error("static/README.md must not be embedded or served")
	}
}

// No template marks content as safe or inlines a script.
func TestTemplatesDoNotBypassEscaping(t *testing.T) {
	err := fs.WalkDir(EmbeddedTemplates(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(EmbeddedTemplates(), p)
		for _, bad := range []string{"| safe", "template.HTML", "<script>"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- SSE ----

func TestSSEFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := NewSSE(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := sse.Send(Message{ID: "7", Event: "session", Data: "line1\r\nline2\nline3"}); err != nil {
		t.Fatal(err)
	}
	if err := sse.Send(Message{Data: ""}); err != nil {
		t.Fatal(err)
	}
	if err := sse.Comment("keep\nalive"); err != nil {
		t.Fatal(err)
	}
	if err := sse.Retry(1500 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := "id: 7\nevent: session\ndata: line1\ndata: line2\ndata: line3\n\n" +
		"data: \n\n" +
		": keep alive\n\n" +
		"retry: 1500\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("stream:\n%q\nwant\n%q", got, want)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "text/event-stream" || !strings.Contains(h.Get("Cache-Control"), "no-cache") || h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers: %v", h)
	}
	if !rec.Flushed {
		t.Error("the stream was not flushed")
	}
}

func TestSSERefusesFieldInjection(t *testing.T) {
	sse, err := NewSSE(httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{
		{Event: "a\ndata: forged"}, {ID: "1\nevent: x"}, {Event: "a\rb"}, {ID: "a\x00b"},
	} {
		if err := sse.Send(m); err == nil {
			t.Errorf("Send(%+v) accepted", m)
		}
	}
}

type noFlushWriter struct{ h http.Header }

func (w *noFlushWriter) Header() http.Header         { return w.h }
func (w *noFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *noFlushWriter) WriteHeader(int)             {}

func TestSSEWithoutFlusher(t *testing.T) {
	if _, err := NewSSE(&noFlushWriter{h: http.Header{}}); err != ErrSSEUnsupported {
		t.Errorf("NewSSE without a flusher = %v, want ErrSSEUnsupported", err)
	}
}

func TestSSEOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, err := NewSSE(w)
		if err != nil {
			return
		}
		_ = sse.Send(Message{Event: "ping", Data: "one"})
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := io.ReadAtLeast(resp.Body, buf, len("event: ping\ndata: one\n\n"))
	if string(buf[:n]) != "event: ping\ndata: one\n\n" {
		t.Errorf("read %q", buf[:n])
	}
}

// ---- integration: the real session middleware and login endpoints ----

type stack struct {
	srv    *httptest.Server
	client *http.Client
	st     *store.Store
}

// newStack wires a real store, the local login handler, the session
// middleware and the web UI behind one mux, as the Hub's modules do.
func newStack(t *testing.T, name string, u store.UserRow) *stack {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hasher := local.NewHasher(local.WithParams(local.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}))
	hash, err := hasher.Hash(ctx, "pw-"+name)
	if err != nil {
		t.Fatal(err)
	}
	u.ID, u.Username, u.PasswordHash, u.Active, u.CredentialVersion = store.NewID(), name, hash, true, 1
	u.CreatedAt = time.Now().UnixMilli()
	u.UpdatedAt = u.CreatedAt
	if _, err := st.SeedAdmin(ctx, store.SeedAdminParams{User: u, Marker: "admin_seed"}); err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(st)
	mw := &session.Middleware{Manager: mgr}
	alog := audit.New(st)
	h := &local.Handler{
		Auth:     &local.Authenticator{Store: st, Hasher: hasher, Limiter: limiter, Audit: alog},
		Sessions: mgr, Audit: alog,
		TTL:      func(context.Context) (time.Duration, error) { return time.Hour, nil },
		Secure:   func(*http.Request) bool { return false },
		ClientIP: func(*http.Request) netip.Addr { return netip.MustParseAddr("127.0.0.1") },
	}
	mux := http.NewServeMux()
	if err := h.Register(mux, mw); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	_ = reg.RegisterPage(Page{Pattern: "/probe", Title: "Probe", Template: "probe.html",
		Data: func(*http.Request) (any, error) { return "probe-data", nil }})
	_ = reg.RegisterNav(NavItem{Label: "Probe", Path: "/probe"})
	web, err := New(Config{Registry: reg, Sessions: mw, Templates: testTemplates(t), ContextKey: testContextKey,
		Secure: func(*http.Request) bool { return false }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/", web)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &stack{srv: srv, st: st, client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

func (s *stack) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := s.client.Get(s.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// login posts the way static/forms.js does: JSON, no CSRF header.
func (s *stack) login(t *testing.T, user, pw string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	resp, err := s.client.Post(s.srv.URL+"/auth/login", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestIntegrationLoginFlow(t *testing.T) {
	s := newStack(t, "root", store.UserRow{})

	// Following redirects, "/" ends at the login form.
	resp, body := s.get(t, "/")
	if resp.Request.URL.Path != "/login" || resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("GET / ended at %s with %d", resp.Request.URL, resp.StatusCode)
	}
	// A deep link keeps its destination in next.
	resp, body = s.get(t, "/probe?x=1")
	if resp.Request.URL.RequestURI() != "/login?next=%2Fprobe%3Fx%3D1" || !strings.Contains(body, `data-next="/probe?x=1"`) {
		t.Fatalf("deep link ended at %s", resp.Request.URL)
	}

	// Wrong password: the endpoint answers 401 and the browser stays out.
	if code, _ := s.login(t, "root", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}
	if resp, _ := s.get(t, "/"); resp.Request.URL.Path != "/login" {
		t.Fatalf("after a failed login GET / ended at %s", resp.Request.URL)
	}

	// Right password: pages open, with the navigation and the CSRF header.
	code, out := s.login(t, "root", "pw-root")
	csrf, _ := out["csrf_token"].(string)
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d %v", code, out)
	}
	resp, body = s.get(t, "/probe")
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/probe" {
		t.Fatalf("GET /probe = %d at %s", resp.StatusCode, resp.Request.URL)
	}
	mustHave(t, body, `hx-headers='{"X-Minion-CSRF":"`+csrf+`"}'`, "probe-data", `aria-current="page"><span>Probe</span></a>`, "Administrator")

	// The login page sends a signed-in browser on.
	if resp, _ := s.get(t, "/login"); resp.Request.URL.Path != "/" {
		t.Errorf("GET /login while signed in ended at %s", resp.Request.URL)
	}

	// Sign out with the CSRF header, as forms.js does; then "/" asks again.
	req, _ := http.NewRequest("POST", s.srv.URL+"/auth/logout", nil)
	req.Header.Set(session.CSRFHeader, csrf)
	lresp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = lresp.Body.Close()
	if lresp.StatusCode != 200 {
		t.Fatalf("logout = %d", lresp.StatusCode)
	}
	if resp, _ := s.get(t, "/"); resp.Request.URL.Path != "/login" {
		t.Errorf("after logout GET / ended at %s", resp.Request.URL)
	}
}

func TestIntegrationMustChangeAccount(t *testing.T) {
	s := newStack(t, "temp", store.UserRow{MustChangePassword: true})
	u, err := s.st.UserByIdentifier(context.Background(), "temp")
	if err != nil || !u.MustChangePassword {
		t.Fatalf("seeded user %+v %v", u, err)
	}
	code, out := s.login(t, "temp", "pw-temp")
	user, _ := out["user"].(map[string]any)
	if code != 200 || user["must_change_password"] != true {
		t.Fatalf("login = %d %v", code, out)
	}
	for _, p := range []string{"/", "/probe", "/login"} {
		resp, body := s.get(t, p)
		if resp.Request.URL.Path != "/account/password" || resp.StatusCode != 200 {
			t.Errorf("GET %s ended at %s (%d)", p, resp.Request.URL, resp.StatusCode)
		}
		mustHave(t, body, "You must choose a new password")
	}
	// Static assets stay reachable so that the page itself can load.
	if resp, _ := s.get(t, "/static/forms.js"); resp.StatusCode != 200 || resp.Request.URL.Path != "/static/forms.js" {
		t.Errorf("static asset for a must-change account: %d at %s", resp.StatusCode, resp.Request.URL)
	}
}
