package server

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
)

// webRegistry registers the modules of a real Hub up to the web module, with
// the given web registry.
func webRegistry(t *testing.T, wr *web.Registry) *Registry {
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
	registerWeb(r, wr)
	return r
}

func TestWebModuleServesLoginAndGatesPages(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	wr := web.NewRegistry()
	if err := wr.RegisterPage(web.Page{Pattern: "/sessions", Title: "Sessions"}); err != nil {
		t.Fatal(err)
	}
	if err := wr.RegisterNav(web.NavItem{Label: "Sessions", Path: "/sessions"}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: t.TempDir()}, webRegistry(t, wr))
	hc := newHubClient(t, h, "https")

	// Without a session "/" redirects to /login, which is the form.
	code, body, resp := hc.do("GET", "/", nil, nil)
	if code != 200 || resp.Request.URL.Path != "/login" || !strings.Contains(body, `name="password"`) {
		t.Fatalf("GET / = %d at %s", code, resp.Request.URL)
	}
	// A page of another story is gated the same way and keeps its target.
	_, _, resp = hc.do("GET", "/sessions", nil, nil)
	if got := resp.Request.URL.RequestURI(); got != "/login?next="+url.QueryEscape("/sessions") {
		t.Errorf("GET /sessions without a session ended at %s", got)
	}
	// The auth endpoints win over the catch-all.
	code, _, csrf := hc.login("root", "integration-password-1")
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d", code)
	}
	code, body, _ = hc.do("GET", "/sessions", nil, nil)
	if code != 200 || !strings.Contains(body, `aria-current="page"><span>Sessions</span></a>`) || !strings.Contains(body, `"X-Minion-CSRF":"`+csrf+`"`) {
		t.Fatalf("GET /sessions signed in = %d\n%s", code, body)
	}
	// Static assets are served, and the health endpoints still answer.
	if code, _, _ := hc.do("GET", "/static/htmx.min.js", nil, nil); code != 200 {
		t.Errorf("htmx = %d", code)
	}
	if code, _, _ := hc.do("GET", "/healthz", nil, nil); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
	if strings.Contains(h.logs.String(), "integration-password-1") {
		t.Errorf("a password reached the log:\n%s", h.logs)
	}
}
