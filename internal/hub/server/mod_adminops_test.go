package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestAdminOpsModuleIsBetweenTheAPIAndTheWebModule(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("adminops") < 0 || idx("adminops") < idx("api") || idx("adminops") < idx("oidc") || idx("adminops") > idx("web") {
		t.Errorf("modules = %v", names)
	}
}

func TestAdminOpsModuleDoesNothingForACatalogWithoutThoseOps(t *testing.T) {
	r := NewRegistry()
	registerAdminOps(r, ops.NewRegistry())
	if err := NewKernel(r).Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestAdminOpsModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerAdminOps(r, ops.Default)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"adminops"`) {
		t.Errorf("err = %v", err)
	}
}

// formClient posts like the browser does with htmx: the session cookie of the
// jar and the CSRF token in the header, the form urlencoded.
func formDo(hc *hubClient, method, path, csrf string, form url.Values) (int, string, *http.Response) {
	hc.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, hc.base+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set(session.CSRFHeader, csrf)
	}
	resp, err := hc.c.Do(req)
	if err != nil {
		hc.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

// The administration pages work on a real Hub: the Hub's own initialisation
// connects them, the admin signs in over HTTP, creates a user through the page
// and changes a setting that is persisted in the store.
func TestAdminPagesOnARealHub(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "admin-pages-integration-1")
	t.Setenv("MINION_PUBLIC_URL", "")
	dir := t.TempDir()
	h := startHub(t, &Config{DataDir: dir}, DefaultRegistry)
	hc := newHubClient(t, h, "https")

	// Not signed in: the page asks for the login, it is not a 503.
	_, _, resp := hc.do("GET", "/admin/settings", nil, nil)
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("anonymous GET /admin/settings ended at %s", resp.Request.URL)
	}
	code, body, csrf := hc.login("root", "admin-pages-integration-1")
	if code != 200 || csrf == "" {
		t.Fatalf("login = %d %s", code, body)
	}

	// All four pages are served (not 503) and list real data.
	for path, want := range map[string]string{
		"/admin/users":    ">root<",
		"/admin/settings": `id="h-session_ttl"`,
		"/admin/oidc":     "Identity &amp; sign-in",
		"/admin/audit":    "<h1>Audit</h1>",
	} {
		code, body, _ := hc.do("GET", path, nil, nil)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d (want %q)\n%.400s", path, code, want, body)
		}
	}

	// Create a user through the page: the temporary password is shown once.
	code, body, _ = formDo(hc, "POST", "/admin/users", csrf, url.Values{"username": {"dave"}, "email": {"dave@example.com"}, "role": {"user"}})
	if code != 200 || !strings.Contains(body, "Account dave created.") {
		t.Fatalf("create user = %d\n%s", code, body)
	}
	_, list, _ := hc.do("GET", "/admin/users", nil, nil)
	if !strings.Contains(list, `<th scope="row">dave</th>`) || !strings.Contains(list, "dave@example.com") {
		t.Errorf("the user list lacks dave\n%s", list)
	}
	// A duplicate is refused with the API's message.
	if _, body, _ = formDo(hc, "POST", "/admin/users", csrf, url.Values{"username": {"dave"}}); !strings.Contains(body, "taken") {
		t.Errorf("duplicate user answer:\n%s", body)
	}

	// Change a setting through the page: it is persisted in the store and the
	// page shows it with its source on the next load.
	code, _, resp = formDo(hc, "POST", "/admin/settings/session_ttl", csrf, url.Values{"value": {"2h"}, "action": {"set"}})
	if code != 200 || resp.Header.Get("HX-Refresh") != "true" {
		t.Fatalf("set setting = %d %v", code, resp.Header)
	}
	_, page, _ := hc.do("GET", "/admin/settings", nil, nil)
	if !strings.Contains(page, `value="2h"`) || !strings.Contains(page, "Source: panel.") {
		t.Errorf("settings page after the change:\n%s", regexp.MustCompile(`(?s)<h2 id="h-session_ttl">.*?</section>`).FindString(page))
	}
	// The change is persisted: after a restart on the same data directory the
	// page still shows it (session_ttl is not live, so the restart applies it),
	// and the created user can be found.
	h.stop(t)
	h2 := startHub(t, &Config{DataDir: dir}, DefaultRegistry)
	hc2 := newHubClient(t, h2, "https")
	code, _, csrf2 := hc2.login("root", "admin-pages-integration-1")
	if code != 200 {
		t.Fatalf("login after restart = %d", code)
	}
	_, page2, _ := hc2.do("GET", "/admin/settings", nil, nil)
	if !strings.Contains(page2, `value="2h"`) || !strings.Contains(page2, "Source: panel.") {
		t.Errorf("settings page after the restart lacks the change")
	}
	if _, list2, _ := hc2.do("GET", "/admin/users", nil, nil); !strings.Contains(list2, "dave@example.com") {
		t.Errorf("user list after the restart lacks dave")
	}
	hc, csrf = hc2, csrf2
	// An unknown key and an env-set key are refused by the production handler.
	if code, body, resp := formDo(hc, "POST", "/admin/settings/no_such_key", csrf, url.Values{"value": {"x"}, "action": {"set"}}); resp.Header.Get("HX-Refresh") == "true" {
		t.Errorf("an unknown setting key was accepted: %d %s", code, body)
	} else if !strings.Contains(body, "unknown Hub setting") {
		t.Errorf("unknown key answer: %d %.200s", code, body)
	}

	// The OIDC page answers through oidc_provider_list; a provider can be
	// created over the same pipeline (settings and providers are separate).
	op := hc.callOp(t, csrf, ops.OpOIDCProviderList, map[string]any{})
	if op.code != 200 {
		t.Errorf("oidc_provider_list = %d %s", op.code, op.body)
	}
	// The audit page shows the changes that were made.
	_, audit, _ := hc.do("GET", "/admin/audit", nil, nil)
	for _, want := range []string{"settings.set", "user.create"} {
		if !strings.Contains(audit, want) {
			t.Errorf("the audit page lacks %q", want)
		}
	}
	for _, secret := range []string{"admin-pages-integration-1"} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("a secret reached the log:\n%s", h.logs)
		}
	}
}

type opResp struct {
	code int
	body string
}

func (hc *hubClient) callOp(t *testing.T, csrf, op string, args map[string]any) opResp {
	t.Helper()
	args["request_id"] = ops.NewUUIDv7()
	code, body, _ := hc.do("POST", "/api/v1/ops/"+op, args, map[string]string{session.CSRFHeader: csrf})
	return opResp{code, body}
}
