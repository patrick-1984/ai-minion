package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	adminUserID  = "0190a1b2-c3d4-7e5f-8a6b-000000000001"
	otherUserID  = "0190a1b2-c3d4-7e5f-8a6b-000000000002"
	adminHeader  = "alice:admin"
	userHeader   = "bob:user"
	tempPassword = "TEMP-pass-0123456789abcdef"
)

var adminTime = ops.Timestamp(time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC))

// opCall is one recorded call of the fake operation caller.
type opCall struct {
	Op   string
	Args map[string]any
	User string // the user of the request the call ran for
}

// fakeOps is an OpCaller that answers from a table of handlers.
type fakeOps struct {
	mu       sync.Mutex
	calls    []opCall
	handlers map[string]func(args map[string]any) (any, error)
}

func newFakeOps() *fakeOps {
	return &fakeOps{handlers: map[string]func(map[string]any) (any, error){}}
}

func (f *fakeOps) on(op string, fn func(args map[string]any) (any, error)) *fakeOps {
	f.handlers[op] = fn
	return f
}

// reply makes an operation answer with a fixed value.
func (f *fakeOps) reply(op string, v any) *fakeOps {
	return f.on(op, func(map[string]any) (any, error) { return v, nil })
}

func (f *fakeOps) CallOp(r *http.Request, op string, args, result any) error {
	b, _ := json.Marshal(args)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	user := ""
	if s, ok := session.FromContext(r.Context()); ok {
		user = s.Username
	}
	f.mu.Lock()
	f.calls = append(f.calls, opCall{Op: op, Args: m, User: user})
	h := f.handlers[op]
	f.mu.Unlock()
	if h == nil {
		return &OpError{Status: 501, Code: "unsupported_op", Message: "the operation is not available on this Hub"}
	}
	v, err := h(m)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	jb, _ := json.Marshal(v)
	return json.Unmarshal(jb, result)
}

func (f *fakeOps) called(op string) []opCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []opCall
	for _, c := range f.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeOps) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var delivered = ops.WriteResult{Outcome: ops.OutcomeDelivered}

// connect installs c as the admin caller for the test.
func connect(t *testing.T, c OpCaller) {
	t.Helper()
	SetAdminOps(c)
	t.Cleanup(func() { SetAdminOps(nil) })
}

func adminServer(t *testing.T) *Server { return newServer(t, DefaultRegistry) }

func post(t *testing.T, h http.Handler, path, user string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	if user != "" {
		req.Header.Set("X-Test-Session", user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// mainOf returns the content of <main> of a page; golden files hold only that,
// so that other stories' navigation entries do not change them.
func mainOf(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<main id="content" tabindex="-1">`)
	j := strings.Index(body, "</main>")
	if i < 0 || j < i {
		t.Fatalf("no <main> in\n%s", body)
	}
	return body[i : j+len("</main>")]
}

// flashesOf decodes the flash messages that a response sets.
func flashesOf(rec *httptest.ResponseRecorder) []Flash {
	for _, c := range rec.Result().Cookies() {
		if c.Name == flashCookie {
			return decodeFlashes(c.Value)
		}
	}
	return nil
}

func sampleUsers() ops.UsersListResult {
	return ops.UsersListResult{Users: []ops.UserInfo{
		{ID: adminUserID, Username: "alice", Email: "alice@example.com", Role: "admin", AdminSources: []string{"local"},
			Active: true, HasPassword: true, CreatedAt: adminTime},
		{ID: otherUserID, Username: "bob", Role: "user", AdminSources: []string{}, Active: true, HasPassword: true,
			MustChangePassword: true, CreatedAt: adminTime},
		{ID: "0190a1b2-c3d4-7e5f-8a6b-000000000003", Username: "carol", Role: "admin", AdminSources: []string{"idp"},
			Active: false, CreatedAt: adminTime},
	}}
}

// -- access ------------------------------------------------------------------

var adminPaths = []string{pathAdminUsers, pathAdminUsers + "/" + otherUserID, pathAdminSettings, pathAdminOIDC, pathAdminAudit}

// Admin-only access: a user gets 404 (not 403) on every page and the pages do
// not even ask the Hub for data.
func TestAdminPagesAreNotFoundForUsers(t *testing.T) {
	f := newFakeOps()
	connect(t, f)
	s := adminServer(t)
	for _, p := range adminPaths {
		rec := get(t, s, p, "X-Test-Session", userHeader)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as user = %d, want 404", p, rec.Code)
		}
		mustNotHave(t, rec.Body.String(), "Settings", "Audit log", "<table")
	}
	if f.total() != 0 {
		t.Errorf("a user's request reached the Hub operations: %+v", f.calls)
	}
}

func TestAdminRoutesAreNotFoundForUsers(t *testing.T) {
	f := newFakeOps().reply(ops.OpSettingsSet, delivered).reply(ops.OpUserCreate, map[string]any{})
	connect(t, f)
	s := adminServer(t)
	for _, p := range []string{
		pathAdminUsers, pathAdminUsers + "/" + otherUserID + "/reset-password", pathAdminUsers + "/" + otherUserID + "/role",
		pathAdminUsers + "/" + otherUserID + "/deactivate", pathAdminUsers + "/" + otherUserID + "/act-as",
		pathAdminSettings + "/listen", pathAdminOIDC + "/providers", pathAdminOIDC + "/providers/x/delete",
	} {
		if rec := post(t, s, p, userHeader, url.Values{"value": {"x"}}); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s as user = %d, want 404", p, rec.Code)
		}
	}
	if f.total() != 0 {
		t.Errorf("a user's post reached the Hub operations: %+v", f.calls)
	}
}

func TestAdminPagesNeedASession(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	for _, p := range adminPaths {
		rec := get(t, s, p)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("GET %s anonymous = %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// While an admin acts as another user the API treats the session as that user,
// so the pages are closed instead of failing half way.
func TestAdminPagesAreClosedWhileActingAs(t *testing.T) {
	f := newFakeOps()
	connect(t, f)
	s := actAsServer(t, actAsSessions{target: "bob", expires: time.Now().Add(time.Hour)}, DefaultRegistry)
	if rec := get(t, s, pathAdminSettings); rec.Code != http.StatusNotFound {
		t.Errorf("GET settings while acting as = %d, want 404", rec.Code)
	}
	if f.total() != 0 {
		t.Error("the operations were called while acting as another user")
	}
}

func TestAdminPagesWithoutACallerAnswer503(t *testing.T) {
	SetAdminOps(nil)
	s := adminServer(t)
	rec := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET settings without a caller = %d, want 503", rec.Code)
	}
	if rec := post(t, s, pathAdminSettings+"/listen", adminHeader, url.Values{"value": {"x"}}); !strings.Contains(rec.Body.String(), "not connected") {
		t.Errorf("POST without a caller = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAdminNavigationIsForAdminsOnly(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	admin := get(t, s, "/", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, `<a href="/admin/users"><svg class="icon" aria-hidden="true" focusable="false"><use href="#i-users"/></svg><span>Users</span></a>`,
		`href="/admin/settings">`, `<span>Hub settings</span>`, `href="/admin/oidc">`, `<span>Identity &amp; sign-in</span>`,
		`href="/admin/audit">`, `<span>Audit</span>`, `<a href="/admin">Administration</a>`)
	// An ordinary user gets no administration link and no administration heading.
	user := get(t, s, "/", "X-Test-Session", userHeader).Body.String()
	mustNotHave(t, user, "/admin", "Administration", "Hub settings", "Identity &amp; sign-in")
}

func TestRefusedOperationLooksLikeAMissingPage(t *testing.T) {
	f := newFakeOps().on(ops.OpSettingsList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "no"}
	})
	connect(t, f)
	if rec := get(t, adminServer(t), pathAdminSettings, "X-Test-Session", adminHeader); rec.Code != http.StatusNotFound {
		t.Errorf("a forbidden settings_list = %d, want 404", rec.Code)
	}
	f.on(ops.OpSettingsList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 500, Code: "internal", Message: "an internal error occurred"}
	})
	rec := get(t, adminServer(t), pathAdminSettings, "X-Test-Session", adminHeader)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "an internal error occurred") {
		t.Errorf("a failing settings_list = %d\n%s", rec.Code, rec.Body)
	}
}

// -- settings ----------------------------------------------------------------

func sampleSettings() ops.SettingsListResult {
	return ops.SettingsListResult{Settings: []ops.SettingInfo{
		{Key: "listen", Help: "Address the Hub listens on", Env: "MINION_LISTEN", Value: ":8443",
			Source: ops.SettingSourceEnv, ReadOnly: true, Live: false},
		{Key: "tls", Help: "TLS mode", Env: "MINION_TLS", Value: "auto", Source: ops.SettingSourceDefault,
			Choices: []string{"auto", "off"}},
		{Key: "public_url", Help: "External URL of the Hub", Env: "MINION_PUBLIC_URL", Value: "https://hub.example.com",
			Source: ops.SettingSourcePanel, PendingRestart: true},
		{Key: "oidc_client_secret", Help: "Client secret", Env: "MINION_OIDC_CLIENT_SECRET", EnvFile: "MINION_OIDC_CLIENT_SECRET_FILE",
			Value: "MASKED-VALUE", Secret: true, Source: ops.SettingSourceEnv, ReadOnly: true},
		{Key: "smtp_password", Help: "SMTP password", Env: "MINION_SMTP_PASSWORD", EnvFile: "MINION_SMTP_PASSWORD_FILE",
			Value: "MASKED-VALUE", Secret: true, Source: ops.SettingSourcePanel},
		{Key: "backup_key", Help: "Backup key", Env: "MINION_BACKUP_KEY", EnvFile: "MINION_BACKUP_KEY_FILE", Secret: true,
			Source: ops.SettingSourceDefault},
		{Key: "data_dir", Help: "Data directory", Env: "MINION_DATA_DIR", Value: "/data", Source: ops.SettingSourceDefault, ReadOnly: true},
	}}
}

func settingsServer(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().reply(ops.OpSettingsList, sampleSettings())
	connect(t, f)
	return adminServer(t), f
}

// AC: every Hub setting is listed with its env name; env-set fields are greyed
// and read-only with "set by environment variable"; secrets are never shown
// back (only "set" / "not set").
func TestSettingsPageGolden(t *testing.T) {
	s, _ := settingsServer(t)
	rec := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET settings = %d\n%s", rec.Code, rec.Body)
	}
	golden(t, "admin_settings", mainOf(t, rec.Body.String()))
}

func TestSettingsListsEveryKeyWithItsEnvName(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	for _, st := range sampleSettings().Settings {
		mustHave(t, body, `<label for="v-`+st.Key+`" id="h-`+st.Key+`">`+st.Key+`</label>`, "<code>"+st.Env+"</code>")
		if st.EnvFile != "" {
			mustHave(t, body, "<code>"+st.EnvFile+"</code>")
		}
	}
}

func TestSettingsEnvSetFieldsAreReadOnly(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	// The env-set text field is disabled and carries no name, so it is never posted.
	mustHave(t, body,
		`<input type="text" id="v-listen" value=":8443" disabled aria-describedby="why-listen m-listen d-listen">`,
		`<span id="why-listen" class="s-locked">set by environment variable</span>`)
	// The editable field is a named input with Save and Use-the-default.
	mustHave(t, body,
		`<input type="text" id="v-public_url" name="value" value="https://hub.example.com"`,
		`<select id="v-tls" name="value" aria-describedby="m-tls d-tls">`, `<option value="auto" selected>auto</option>`)
	// data_dir is read-only for another reason: the panel cannot hold it.
	mustHave(t, body, `<span id="why-data_dir" class="s-locked">cannot be changed in the panel</span>`)
	// A locked setting offers no button.
	section := between(t, body, `id="h-listen"`, `</form>`)
	mustNotHave(t, section, "<button", `name="value"`)
}

func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("%q not found in\n%s", from, s)
	}
	rest := s[i:]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("%q not found after %q", to, from)
	}
	return rest[:j]
}

func TestSettingsNeverShowSecretsBack(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "MASKED-VALUE")
	mustHave(t, body,
		`Secret: set.`,     // env or panel holds it
		`Secret: not set.`, // nothing holds it
		`placeholder="set"`, `placeholder="not set"`)
	// The row of the secret set by the environment is read-only and empty.
	section := between(t, body, `id="h-oidc_client_secret"`, `</form>`)
	mustHave(t, section, `type="password" id="v-oidc_client_secret" value="" disabled`, "set by environment variable")
	mustNotHave(t, section, "<button")
}

func TestSettingsFlagsLiveAndPendingRestart(t *testing.T) {
	s, _ := settingsServer(t)
	body := get(t, s, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "A changed value waits for the next restart.", "Applies after the Hub restarts.")
}

func TestSettingsRowsForSecretsAndLocks(t *testing.T) {
	rows := settingRows(sampleSettings().Settings)
	by := map[string]SettingRow{}
	for _, r := range rows {
		by[r.Key] = r
	}
	if r := by["smtp_password"]; r.Value != "" || r.SecretState != "set" || r.Locked {
		t.Errorf("panel secret = %+v", r)
	}
	if r := by["backup_key"]; r.Value != "" || r.SecretState != "not set" {
		t.Errorf("unset secret = %+v", r)
	}
	if r := by["listen"]; !r.Locked || r.LockedText != EnvLockedText {
		t.Errorf("env setting = %+v", r)
	}
	if r := by["data_dir"]; !r.Locked || r.LockedText == EnvLockedText {
		t.Errorf("data_dir = %+v", r)
	}
	if len(rows) != len(sampleSettings().Settings) {
		t.Errorf("%d rows for %d settings", len(rows), len(sampleSettings().Settings))
	}
}

func TestSettingSetCallsTheOperationAndReloads(t *testing.T) {
	s, f := settingsServer(t)
	f.reply(ops.OpSettingsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered,
		Warnings: []ops.Warning{{Code: ops.WarnRestartRequired, Message: "takes effect after the Hub restarts"}}})
	rec := post(t, s, pathAdminSettings+"/public_url", adminHeader, url.Values{"value": {"https://new.example.com"}, "action": {"set"}})
	if rec.Code != 200 || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d, HX-Refresh %q", rec.Code, rec.Header().Get("HX-Refresh"))
	}
	calls := f.called(ops.OpSettingsSet)
	if len(calls) != 1 {
		t.Fatalf("settings_set calls = %d", len(calls))
	}
	a := calls[0].Args
	if a["key"] != "public_url" || a["value"] != "https://new.example.com" || a["unset"] != nil || !ops.ValidUUIDv7(a["request_id"].(string)) {
		t.Errorf("args = %v", a)
	}
	if calls[0].User != "alice" {
		t.Errorf("the call ran for %q, want the signed-in admin", calls[0].User)
	}
	fl := flashesOf(rec)
	if len(fl) != 2 || !strings.Contains(fl[0].Text, "public_url is saved") || fl[1].Level != FlashWarning ||
		fl[1].Text != "takes effect after the Hub restarts" {
		t.Errorf("flashes = %+v", fl)
	}
}

func TestSettingUnsetSendsUnsetWithoutValue(t *testing.T) {
	s, f := settingsServer(t)
	f.reply(ops.OpSettingsSet, delivered)
	rec := post(t, s, pathAdminSettings+"/tls", adminHeader, url.Values{"value": {"off"}, "action": {"unset"}})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d", rec.Code)
	}
	a := f.called(ops.OpSettingsSet)[0].Args
	if a["unset"] != true || a["value"] != nil {
		t.Errorf("args = %v", a)
	}
}

// An empty secret field must never wipe a secret by accident.
func TestSettingSecretNeedsAnExplicitRemove(t *testing.T) {
	s, f := settingsServer(t)
	f.reply(ops.OpSettingsSet, delivered)
	rec := post(t, s, pathAdminSettings+"/smtp_password", adminHeader, url.Values{"value": {""}, "secret": {"1"}, "action": {"set"}})
	if rec.Header().Get("HX-Refresh") != "" || !strings.Contains(rec.Body.String(), "use Remove") {
		t.Errorf("empty secret = %d %q", rec.Code, rec.Body)
	}
	if len(f.called(ops.OpSettingsSet)) != 0 {
		t.Error("an empty secret reached settings_set")
	}
	rec = post(t, s, pathAdminSettings+"/smtp_password", adminHeader, url.Values{"secret": {"1"}, "action": {"unset"}})
	if rec.Header().Get("HX-Refresh") != "true" || len(f.called(ops.OpSettingsSet)) != 1 {
		t.Errorf("explicit remove = %d", rec.Code)
	}
}

func TestSettingSetShowsTheConflictOfAnEnvSetting(t *testing.T) {
	s, f := settingsServer(t)
	f.on(ops.OpSettingsSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "set by environment variable"}
	})
	rec := post(t, s, pathAdminSettings+"/listen", adminHeader, url.Values{"value": {":1"}, "action": {"set"}})
	if rec.Code != 200 || rec.Header().Get("HX-Refresh") != "" {
		t.Errorf("POST = %d, HX-Refresh %q", rec.Code, rec.Header().Get("HX-Refresh"))
	}
	mustHave(t, rec.Body.String(), `<p class="form-error" role="alert">set by environment variable</p>`)
}

func TestSettingSetNamesTheRefusedField(t *testing.T) {
	s, f := settingsServer(t)
	f.on(ops.OpSettingsSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 400, Code: "invalid_argument", Message: "must be one of [auto off]", Field: "value"}
	})
	rec := post(t, s, pathAdminSettings+"/tls", adminHeader, url.Values{"value": {"x"}, "action": {"set"}})
	mustHave(t, rec.Body.String(), "value must be one of [auto off]")
}

func TestErrorTextsAreEscaped(t *testing.T) {
	s, f := settingsServer(t)
	f.on(ops.OpSettingsSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "<script>alert(1)</script>"}
	})
	body := post(t, s, pathAdminSettings+"/tls", adminHeader, url.Values{"value": {"x"}}).Body.String()
	mustNotHave(t, body, "<script>")
	mustHave(t, body, "&lt;script&gt;")
}

func TestAnUndeliveredWriteIsAFailure(t *testing.T) {
	s, f := settingsServer(t)
	f.reply(ops.OpSettingsSet, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "not_found"})
	rec := post(t, s, pathAdminSettings+"/tls", adminHeader, url.Values{"value": {"off"}})
	if rec.Header().Get("HX-Refresh") != "" || !strings.Contains(rec.Body.String(), "was not applied (not found)") {
		t.Errorf("POST = %d %q", rec.Code, rec.Body)
	}
}

// -- users -------------------------------------------------------------------

func usersServer(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().reply(ops.OpUsersList, sampleUsers())
	connect(t, f)
	return adminServer(t), f
}

func TestUsersPageGolden(t *testing.T) {
	s, _ := usersServer(t)
	rec := get(t, s, pathAdminUsers, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET users = %d\n%s", rec.Code, rec.Body)
	}
	golden(t, "admin_users", mainOf(t, rec.Body.String()))
}

func TestUsersPageListsAccountsAndOffersCreation(t *testing.T) {
	s, _ := usersServer(t)
	body := get(t, s, pathAdminUsers, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `hx-post="/admin/users"`, `name="username"`, `name="email"`, `name="role"`,
		`<th scope="row">alice</th>`, `<th scope="row">bob</th>`, `<th scope="row">carol</th>`,
		`href="/admin/users/`+otherUserID+`">Manage bob</a>`,
		"temporary", "deactivated", "(idp)")
}

func TestUserCreateShowsTheTemporaryPasswordOnce(t *testing.T) {
	s, f := usersServer(t)
	f.on(ops.OpUserCreate, func(map[string]any) (any, error) {
		return map[string]any{"outcome": "delivered", "user": ops.UserInfo{ID: otherUserID, Username: "dave"},
			"temporary_password": tempPassword}, nil
	})
	rec := post(t, s, pathAdminUsers, adminHeader, url.Values{"username": {" dave "}, "email": {"dave@example.com"}, "role": {"admin"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, `<code id="temporary-password">`+tempPassword+`</code>`, "Account dave created.", "shown once")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	a := f.called(ops.OpUserCreate)[0].Args
	if a["username"] != "dave" || a["email"] != "dave@example.com" || a["role"] != "admin" || !ops.ValidUUIDv7(a["request_id"].(string)) {
		t.Errorf("args = %v", a)
	}
	// The password is nowhere else: not in the page, not in a cookie or header.
	page := get(t, s, pathAdminUsers, "X-Test-Session", adminHeader)
	mustNotHave(t, page.Body.String(), tempPassword)
	if strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ";"), tempPassword) || rec.Header().Get("HX-Redirect") != "" {
		t.Errorf("the password leaked into the headers: %v", rec.Header())
	}
}

func TestUserCreateFailureShowsTheMessage(t *testing.T) {
	s, f := usersServer(t)
	f.on(ops.OpUserCreate, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "that username is taken"}
	})
	body := post(t, s, pathAdminUsers, adminHeader, url.Values{"username": {"alice"}}).Body.String()
	mustHave(t, body, `<p class="form-error" role="alert">that username is taken</p>`)
	mustNotHave(t, body, "temporary-password")
}

func userDetail(t *testing.T, s *Server) string {
	t.Helper()
	rec := get(t, s, pathAdminUsers+"/"+otherUserID, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET user = %d\n%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func sampleIdentities() map[string]any {
	return map[string]any{"identities": []IdentityInfo{
		{ID: "id-1", Provider: "okta", Name: "Okta", Issuer: "https://idp.example.com", Subject: "sub-123", Email: "bob@example.com"},
	}}
}

func TestUserDetailGolden(t *testing.T) {
	s, f := usersServer(t)
	f.reply(opIdentityList, sampleIdentities())
	golden(t, "admin_user_detail", mainOf(t, userDetail(t, s)))
}

func TestUserDetailOffersEveryAction(t *testing.T) {
	s, f := usersServer(t)
	f.reply(opIdentityList, sampleIdentities())
	body := userDetail(t, s)
	id := otherUserID
	mustHave(t, body,
		`hx-post="/admin/users/`+id+`/reset-password"`,
		`hx-post="/admin/users/`+id+`/role"`, `value="admin"`, "Make a local admin",
		`hx-post="/admin/users/`+id+`/act-as"`, `<option value="60" selected>60 minutes</option>`,
		`hx-post="/admin/users/`+id+`/deactivate"`,
		`hx-post="/admin/users/`+id+`/identities/id-1/detach"`, "sub-123", "https://idp.example.com", "Okta")
	if got := f.called(opIdentityList)[0].Args["user_id"]; got != id {
		t.Errorf("identity_list user_id = %v", got)
	}
}

func TestUserDetailForALocalAdminOffersRemoval(t *testing.T) {
	s, f := usersServer(t)
	f.reply(opIdentityList, map[string]any{"identities": []IdentityInfo{}})
	body := get(t, s, pathAdminUsers+"/"+adminUserID, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "Remove the local admin role", `name="role" value="user"`, "(local admin)",
		"No external identity is linked to this account.")
}

func TestUserDetailOfAnIdPAdminExplainsTheSource(t *testing.T) {
	s, f := usersServer(t)
	f.reply(opIdentityList, map[string]any{"identities": []IdentityInfo{}})
	body := get(t, s, pathAdminUsers+"/0190a1b2-c3d4-7e5f-8a6b-000000000003", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "admin through the identity provider")
	// A deactivated account without a password offers no reset, act-as or deactivate.
	mustNotHave(t, body, "/reset-password", "/act-as", "/deactivate", "/role")
}

func TestUserDetailUnknownIDIsNotFound(t *testing.T) {
	s, _ := usersServer(t)
	rec := get(t, s, pathAdminUsers+"/0190a1b2-c3d4-7e5f-8a6b-0000000000ff", "X-Test-Session", adminHeader)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown user = %d, want 404", rec.Code)
	}
}

func TestUserDetailWorksWithoutIdentityOperations(t *testing.T) {
	s, _ := usersServer(t) // identity_list answers unsupported_op
	mustHave(t, userDetail(t, s), "Linked identities are not available on this Hub.")
}

func TestUserDetailSurvivesAFailingIdentityList(t *testing.T) {
	s, f := usersServer(t)
	f.on(opIdentityList, func(map[string]any) (any, error) { return nil, errors.New("boom") })
	mustHave(t, userDetail(t, s), "The linked identities could not be loaded.")
}

func TestUserResetShowsANewTemporaryPassword(t *testing.T) {
	s, f := usersServer(t)
	f.reply(ops.OpUserResetPassword, map[string]any{"outcome": "delivered", "user_id": otherUserID, "temporary_password": tempPassword})
	rec := post(t, s, pathAdminUsers+"/"+otherUserID+"/reset-password", adminHeader, url.Values{"username": {"bob"}})
	mustHave(t, rec.Body.String(), "New temporary password for bob.", tempPassword)
	a := f.called(ops.OpUserResetPassword)[0].Args
	if a["user_id"] != otherUserID || !ops.ValidUUIDv7(a["request_id"].(string)) {
		t.Errorf("args = %v", a)
	}
}

func TestUserRoleDeactivateAndDetach(t *testing.T) {
	s, f := usersServer(t)
	f.reply(ops.OpUserSetRole, delivered).reply(ops.OpUserDeactivate, delivered).reply(opIdentityDetach, delivered)
	base := pathAdminUsers + "/" + otherUserID

	rec := post(t, s, base+"/role", adminHeader, url.Values{"role": {"admin"}})
	if rec.Header().Get("HX-Refresh") != "true" || flashesOf(rec)[0].Text != "The account is now a local admin." {
		t.Errorf("role = %d %+v", rec.Code, flashesOf(rec))
	}
	a := f.called(ops.OpUserSetRole)[0].Args
	if a["user_id"] != otherUserID || a["role"] != "admin" {
		t.Errorf("set_role args = %v", a)
	}

	rec = post(t, s, base+"/deactivate", adminHeader, nil)
	if rec.Header().Get("HX-Refresh") != "true" || f.called(ops.OpUserDeactivate)[0].Args["user_id"] != otherUserID {
		t.Errorf("deactivate = %d", rec.Code)
	}

	rec = post(t, s, base+"/identities/id-1/detach", adminHeader, nil)
	d := f.called(opIdentityDetach)[0].Args
	if rec.Header().Get("HX-Refresh") != "true" || d["user_id"] != otherUserID || d["identity_id"] != "id-1" || d["request_id"] == "" {
		t.Errorf("detach = %d %v", rec.Code, d)
	}
}

func TestUserLastAdminRefusalIsShown(t *testing.T) {
	s, f := usersServer(t)
	f.on(ops.OpUserDeactivate, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: "the last admin cannot be deactivated"}
	})
	rec := post(t, s, pathAdminUsers+"/"+adminUserID+"/deactivate", adminHeader, nil)
	mustHave(t, rec.Body.String(), "the last admin cannot be deactivated")
	if rec.Header().Get("HX-Refresh") != "" {
		t.Error("a refused change reloaded the page")
	}
}

func TestActAsStartRedirectsHome(t *testing.T) {
	s, f := usersServer(t)
	f.reply(ops.OpActAsStart, delivered)
	rec := post(t, s, pathAdminUsers+"/"+otherUserID+"/act-as", adminHeader, url.Values{"minutes": {"30"}})
	if rec.Header().Get("HX-Redirect") != "/" {
		t.Errorf("act-as = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
	a := f.called(ops.OpActAsStart)[0].Args
	if a["user_id"] != otherUserID || a["duration_minutes"] != float64(30) {
		t.Errorf("args = %v", a)
	}
	// An empty duration means the operation's default; a bad one is refused here.
	post(t, s, pathAdminUsers+"/"+otherUserID+"/act-as", adminHeader, url.Values{"minutes": {""}})
	if a := f.called(ops.OpActAsStart)[1].Args; a["duration_minutes"] != nil {
		t.Errorf("default args = %v", a)
	}
	rec = post(t, s, pathAdminUsers+"/"+otherUserID+"/act-as", adminHeader, url.Values{"minutes": {"abc"}})
	if !strings.Contains(rec.Body.String(), "number of minutes") || len(f.called(ops.OpActAsStart)) != 2 {
		t.Errorf("bad minutes = %q", rec.Body)
	}
}

// -- OIDC --------------------------------------------------------------------

func sampleOIDC(insecure bool) ops.OIDCProviderListResult {
	return ops.OIDCProviderListResult{
		InsecureTLS: insecure,
		Providers: []ops.OIDCProviderInfo{
			{ID: "default", Name: "Company SSO", Issuer: "https://sso.example.com", ClientID: "minion", ClientSecretSet: true,
				RoleClaim: "groups", AdminValues: []string{"minion-admins"}, UserValues: []string{}, Scopes: "openid email",
				Enabled: true, Source: ops.OIDCSourceSettings, ReadOnly: true},
			{ID: "lab", Name: "Lab Keycloak", Issuer: "https://kc.lab.example.com/realms/lab", ClientID: "minion-lab",
				RoleClaim: "roles", AdminValues: []string{"admin", "ops"}, UserValues: []string{"dev"},
				TLSInsecureSkipVerify: insecure, Enabled: true, Source: ops.OIDCSourcePanel},
		},
	}
}

func oidcServer(t *testing.T, insecure bool) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().reply(ops.OpOIDCProviderList, sampleOIDC(insecure))
	connect(t, f)
	return adminServer(t), f
}

func TestOIDCPageGolden(t *testing.T) {
	s, _ := oidcServer(t, false)
	rec := get(t, s, pathAdminOIDC, "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET oidc = %d\n%s", rec.Code, rec.Body)
	}
	golden(t, "admin_oidc", mainOf(t, rec.Body.String()))
}

func TestOIDCEnvProviderIsReadOnlyAndPanelProviderEditable(t *testing.T) {
	s, _ := oidcServer(t, false)
	body := get(t, s, pathAdminOIDC, "X-Test-Session", adminHeader).Body.String()
	locked := between(t, body, `aria-labelledby="p-default"`, `</section>`)
	mustHave(t, locked, OIDCLockedText, `id="p-default-issuer" name="issuer" value="https://sso.example.com" autocomplete="off" spellcheck="false" required maxlength="512" disabled`)
	mustNotHave(t, locked, "<button")
	editable := between(t, body, `aria-labelledby="p-lab"`, `</section>`)
	mustHave(t, editable, "Save the provider", "Save the role rules", "Delete the provider", "admin\nops", "dev")
	mustNotHave(t, editable, OIDCLockedText, " disabled")
}

func TestOIDCNeverShowsTheClientSecret(t *testing.T) {
	s, _ := oidcServer(t, false)
	body := get(t, s, pathAdminOIDC, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "Client secret (set)", "leave empty to keep the secret")
}

func TestOIDCInsecureTLSWarningIsPermanent(t *testing.T) {
	s, _ := oidcServer(t, true)
	body := get(t, s, pathAdminOIDC, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `id="insecure-tls-warning" role="alert"`, "TLS certificate verification is switched off",
		"TLS verification is off for this provider.")
	// It has no dismiss control.
	warn := between(t, body, `id="insecure-tls-warning"`, `</div>`)
	mustNotHave(t, warn, "<button", "dismiss")

	// No warning while verification is on.
	s2, _ := oidcServer(t, false)
	mustNotHave(t, get(t, s2, pathAdminOIDC, "X-Test-Session", adminHeader).Body.String(), "insecure-tls-warning", "switched off")
}

// The warning follows the providers even when the flag of the list is absent,
// and a disabled provider does not trigger it.
func TestOIDCInsecureTLSWarningFollowsEnabledProviders(t *testing.T) {
	p := oidcRows(ops.OIDCProviderListResult{Providers: []ops.OIDCProviderInfo{{ID: "a", TLSInsecureSkipVerify: true, Enabled: true}}})
	if !p.InsecureTLS {
		t.Error("an enabled insecure provider gives no warning")
	}
	p = oidcRows(ops.OIDCProviderListResult{Providers: []ops.OIDCProviderInfo{{ID: "a", TLSInsecureSkipVerify: true}}})
	if p.InsecureTLS {
		t.Error("a disabled provider gives a warning")
	}
}

func TestOIDCSaveMapsTheForm(t *testing.T) {
	s, f := oidcServer(t, false)
	f.reply(ops.OpOIDCProviderSet, delivered)
	rec := post(t, s, pathAdminOIDC+"/providers", adminHeader, url.Values{
		"id": {"lab"}, "name": {" Lab "}, "issuer": {"https://kc.example.com"}, "client_id": {"minion"},
		"client_secret": {"s3cret"}, "role_claim": {"roles"}, "scopes": {"openid email"},
		"ca_file": {"/etc/ca.pem"}, "tls_insecure_skip_verify": {"1"}, "enabled": {"1"},
	})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body)
	}
	a := f.called(ops.OpOIDCProviderSet)[0].Args
	want := map[string]any{"id": "lab", "name": "Lab", "issuer": "https://kc.example.com", "client_id": "minion",
		"client_secret": "s3cret", "role_claim": "roles", "scopes": "openid email", "ca_file": "/etc/ca.pem",
		"tls_insecure_skip_verify": true, "enabled": true}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("args[%s] = %v, want %v", k, a[k], v)
		}
	}
	// An unchecked box sends nothing: enabled=false and insecure off, and an empty secret is not sent.
	post(t, s, pathAdminOIDC+"/providers", adminHeader, url.Values{"id": {"lab"}, "name": {"Lab"}, "issuer": {"https://kc.example.com"}, "client_id": {"minion"}})
	a = f.called(ops.OpOIDCProviderSet)[1].Args
	if a["enabled"] != false || a["tls_insecure_skip_verify"] != nil || a["client_secret"] != nil {
		t.Errorf("unchecked args = %v", a)
	}
}

func TestOIDCSaveWarningsAndErrors(t *testing.T) {
	s, f := oidcServer(t, false)
	f.reply(ops.OpOIDCProviderSet, ops.WriteResult{Outcome: ops.OutcomeDelivered,
		Warnings: []ops.Warning{{Code: ops.WarnOIDCDiscoveryFailed, Message: "the provider was saved, but discovery failed"}}})
	rec := post(t, s, pathAdminOIDC+"/providers", adminHeader, url.Values{"id": {"lab"}})
	fl := flashesOf(rec)
	if len(fl) != 2 || fl[1].Level != FlashWarning || !strings.Contains(fl[1].Text, "discovery failed") {
		t.Errorf("flashes = %+v", fl)
	}
	f.on(ops.OpOIDCProviderSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 409, Code: "conflict", Message: OIDCLockedText}
	})
	rec = post(t, s, pathAdminOIDC+"/providers", adminHeader, url.Values{"id": {"default"}})
	mustHave(t, rec.Body.String(), OIDCLockedText)
}

func TestOIDCRolesSendsBothRules(t *testing.T) {
	s, f := oidcServer(t, false)
	f.reply(ops.OpRoleRuleSet, delivered)
	rec := post(t, s, pathAdminOIDC+"/providers/lab/roles", adminHeader, url.Values{
		"admin_values": {"admin\r\n  ops \r\n\r\n"}, "user_values": {""}})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("POST = %d %q", rec.Code, rec.Body)
	}
	calls := f.called(ops.OpRoleRuleSet)
	if len(calls) != 2 {
		t.Fatalf("role_rule_set calls = %d", len(calls))
	}
	if calls[0].Args["role"] != "admin" || calls[0].Args["provider_id"] != "lab" ||
		strings.Join(toStrings(calls[0].Args["values"]), ",") != "admin,ops" {
		t.Errorf("admin rule = %v", calls[0].Args)
	}
	if calls[1].Args["role"] != "user" || len(toStrings(calls[1].Args["values"])) != 0 {
		t.Errorf("user rule = %v", calls[1].Args)
	}
	if calls[0].Args["request_id"] == calls[1].Args["request_id"] {
		t.Error("the two writes share a request id")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestOIDCRolesStopsAtTheFirstFailure(t *testing.T) {
	s, f := oidcServer(t, false)
	f.on(ops.OpRoleRuleSet, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 404, Code: "not_found", Message: "no such OIDC provider"}
	})
	rec := post(t, s, pathAdminOIDC+"/providers/nope/roles", adminHeader, url.Values{"admin_values": {"a"}})
	mustHave(t, rec.Body.String(), "no such OIDC provider")
	if len(f.called(ops.OpRoleRuleSet)) != 1 {
		t.Error("the second rule was written after the first failed")
	}
}

// A bad value in the second rule must not leave the first rule changed: both
// are validated before the first write.
func TestOIDCRolesValidatesBothRulesBeforeWriting(t *testing.T) {
	for name, form := range map[string]url.Values{
		"long user value": {"admin_values": {"admin"}, "user_values": {strings.Repeat("x", 257)}},
		"101 user values": {"admin_values": {"admin"}, "user_values": {strings.Repeat("v\n", 101)}},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := oidcServer(t, false)
			f.reply(ops.OpRoleRuleSet, delivered)
			rec := post(t, s, pathAdminOIDC+"/providers/lab/roles", adminHeader, form)
			mustHave(t, rec.Body.String(), "Nothing was saved", "user values are not valid")
			if n := len(f.called(ops.OpRoleRuleSet)); n != 0 {
				t.Errorf("role_rule_set calls = %d, want 0", n)
			}
		})
	}
}

// If the second write fails after the first one was applied, the page says so.
func TestOIDCRolesReportsAPartialSave(t *testing.T) {
	s, f := oidcServer(t, false)
	n := 0
	f.on(ops.OpRoleRuleSet, func(map[string]any) (any, error) {
		n++
		if n == 2 {
			return nil, &OpError{Status: 500, Code: "internal", Message: "the store is busy"}
		}
		return delivered, nil
	})
	rec := post(t, s, pathAdminOIDC+"/providers/lab/roles", adminHeader, url.Values{"admin_values": {"a"}, "user_values": {"b"}})
	mustHave(t, rec.Body.String(), "The admin rule is saved, but the user rule is not", "the store is busy")
}

// Forms that carry a secret must never fall back to GET without htmx.
func TestAdminFormsPostNatively(t *testing.T) {
	forms := regexp.MustCompile(`<form[^>]*>`)
	// Each fixture connects its own fake caller, so the pages are fetched one
	// after the other.
	pages := map[string]string{}
	us, _ := usersServer(t)
	pages["users"] = get(t, us, pathAdminUsers, "X-Test-Session", adminHeader).Body.String()
	pages["user"] = userDetail(t, us)
	os, _ := oidcServer(t, false)
	pages["oidc"] = get(t, os, pathAdminOIDC, "X-Test-Session", adminHeader).Body.String()
	ss, _ := settingsServer(t)
	pages["settings"] = get(t, ss, pathAdminSettings, "X-Test-Session", adminHeader).Body.String()
	for name, page := range pages {
		body := mainOf(t, page)
		tags := forms.FindAllString(body, -1)
		if len(tags) == 0 {
			t.Errorf("%s: no forms found", name)
		}
		for _, tag := range tags {
			if !strings.Contains(tag, `method="post"`) {
				t.Errorf("%s: form without a native post method: %s", name, tag)
			}
		}
	}
}

func TestOIDCDelete(t *testing.T) {
	s, f := oidcServer(t, false)
	f.reply(ops.OpOIDCProviderSet, delivered)
	rec := post(t, s, pathAdminOIDC+"/providers/lab/delete", adminHeader, nil)
	a := f.called(ops.OpOIDCProviderSet)[0].Args
	if rec.Header().Get("HX-Refresh") != "true" || a["id"] != "lab" || a["delete"] != true {
		t.Errorf("delete = %d %v", rec.Code, a)
	}
}

// -- audit -------------------------------------------------------------------

func sampleAudit() ops.AuditListResult {
	return ops.AuditListResult{
		Events: []ops.AuditEvent{
			{ID: "e3", TS: ops.Timestamp(time.Date(2026, 11, 9, 12, 0, 5, 0, time.UTC)), Actor: adminUserID, Action: "settings.set", Target: "public_url", Outcome: "ok"},
			{ID: "e2", TS: ops.Timestamp(time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)), Actor: adminUserID, ActingAs: otherUserID, Action: "worker.delete", Target: "w1", Outcome: "success"},
			{ID: "e1", TS: ops.Timestamp(time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)), Actor: "anonymous", Action: "auth.login", Outcome: "failed"},
		},
		NextCursor: "CUR2",
	}
}

func auditServer(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().reply(ops.OpAuditList, sampleAudit())
	connect(t, f)
	return adminServer(t), f
}

func TestAuditPageGolden(t *testing.T) {
	s, _ := auditServer(t)
	rec := get(t, s, pathAdminAudit+"?actor=abc&action=settings.set", "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET audit = %d\n%s", rec.Code, rec.Body)
	}
	golden(t, "admin_audit", mainOf(t, rec.Body.String()))
}

// AC: events are listed newest first (the order of the operation is kept).
func TestAuditListsEventsNewestFirst(t *testing.T) {
	s, _ := auditServer(t)
	body := get(t, s, pathAdminAudit, "X-Test-Session", adminHeader).Body.String()
	i3, i2, i1 := strings.Index(body, "2026-11-09 12:00:05"), strings.Index(body, "2026-11-09 11:00:00"), strings.Index(body, "2026-11-08 09:00:00")
	if i3 <= 0 || i3 >= i2 || i2 >= i1 {
		t.Errorf("event order wrong: %d %d %d", i3, i2, i1)
	}
	mustHave(t, body, "settings.set", "public_url", "worker.delete", otherUserID, "auth.login", "failed")
}

func TestAuditFiltersBecomeArguments(t *testing.T) {
	s, f := auditServer(t)
	q := url.Values{"actor": {" " + adminUserID + " "}, "action": {"settings.set"}, "from": {"2026-11-01"}, "to": {"2026-11-09"},
		"limit": {"25"}, "cursor": {"CUR1"}}
	get(t, s, pathAdminAudit+"?"+q.Encode(), "X-Test-Session", adminHeader)
	a := f.called(ops.OpAuditList)[0].Args
	want := map[string]any{"actor": adminUserID, "action": "settings.set", "from": "2026-11-01T00:00:00.000Z",
		"to": "2026-11-10T00:00:00.000Z", "limit": float64(25), "cursor": "CUR1"}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("args[%s] = %v, want %v", k, a[k], v)
		}
	}
	// No filter at all sends no filter members.
	get(t, s, pathAdminAudit, "X-Test-Session", adminHeader)
	a = f.called(ops.OpAuditList)[1].Args
	if a["actor"] != nil || a["from"] != nil || a["cursor"] != nil || a["limit"] != float64(ops.DefaultAuditListLimit) {
		t.Errorf("default args = %v", a)
	}
}

func TestAuditAcceptsRFC3339Times(t *testing.T) {
	args, _, err := auditArgs(url.Values{"from": {"2026-11-09T14:30:00+02:00"}})
	if err != nil || args.From == nil || args.From.Time() != time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC) {
		t.Errorf("from = %v, err %v", args.From, err)
	}
}

func TestAuditPaging(t *testing.T) {
	s, _ := auditServer(t)
	body := get(t, s, pathAdminAudit+"?actor=abc&limit=25", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `href="/admin/audit?actor=abc&amp;cursor=CUR2&amp;limit=25" rel="next">Older events</a>`)
	mustNotHave(t, body, "Newest events")
	// On a later page there is a way back to the newest events, filter kept.
	body = get(t, s, pathAdminAudit+"?actor=abc&cursor=CUR1", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<a href="/admin/audit?actor=abc">Newest events</a>`)
}

func TestAuditLastPageHasNoOlderLink(t *testing.T) {
	f := newFakeOps().reply(ops.OpAuditList, ops.AuditListResult{Events: sampleAudit().Events})
	connect(t, f)
	body := get(t, adminServer(t), pathAdminAudit, "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "Older events")
}

func TestAuditEmptyResult(t *testing.T) {
	f := newFakeOps().reply(ops.OpAuditList, ops.AuditListResult{})
	connect(t, f)
	mustHave(t, get(t, adminServer(t), pathAdminAudit, "X-Test-Session", adminHeader).Body.String(), "No events match.")
}

func TestAuditBadFiltersAreExplainedWithoutCallingTheHub(t *testing.T) {
	s, f := auditServer(t)
	for q, want := range map[string]string{
		"from=yesterday": "Enter dates as",
		"to=2026-13-40":  "Enter dates as",
		"limit=0":        "page size",
		"limit=9999":     "page size",
		"limit=abc":      "page size",
	} {
		rec := get(t, s, pathAdminAudit+"?"+q, "X-Test-Session", adminHeader)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("?%s = %d, missing %q", q, rec.Code, want)
		}
	}
	if f.total() != 0 {
		t.Errorf("a bad filter reached the Hub: %+v", f.calls)
	}
}

func TestAuditShowsTheOperationsRefusalOfAFilter(t *testing.T) {
	s, f := auditServer(t)
	f.on(ops.OpAuditList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 400, Code: "invalid_argument", Message: "must be after from", Field: "to"}
	})
	body := get(t, s, pathAdminAudit+"?from=2026-11-09&to=2026-11-01", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, "to must be after from")
}

func TestAuditEscapesEventFields(t *testing.T) {
	f := newFakeOps().reply(ops.OpAuditList, ops.AuditListResult{Events: []ops.AuditEvent{
		{ID: "e", TS: adminTime, Actor: "<b>x</b>", Action: `"><script>1</script>`, Outcome: "ok"}}})
	connect(t, f)
	body := get(t, adminServer(t), pathAdminAudit+"?actor=%22%3E%3Cscript%3E", "X-Test-Session", adminHeader).Body.String()
	mustNotHave(t, body, "<script>", "<b>x</b>")
}

// -- HandlerCaller -----------------------------------------------------------

func TestHandlerCallerForwardsTheSessionAndDecodes(t *testing.T) {
	var got *http.Request
	var body []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":[{"id":"x","username":"u","role":"user","admin_sources":[],"active":true,"has_password":true,"must_change_password":false,"created_at":"2026-11-09T08:30:00.000Z"}]}`)
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.Header.Set("Cookie", "minion_session=tok")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "10.0.0.1:5555"
	req = req.WithContext(session.WithSession(req.Context(), &session.Session{Username: "alice", Role: authz.RoleAdmin, CSRFToken: "csrf-1"}))
	var res ops.UsersListResult
	if err := NewHandlerCaller(h).CallOp(req, ops.OpUsersList, ops.UsersListArgs{}, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Users) != 1 || res.Users[0].Username != "u" {
		t.Errorf("result = %+v", res)
	}
	if got.Method != "POST" || got.URL.Path != "/api/v1/ops/users_list" || got.Header.Get("Cookie") != "minion_session=tok" ||
		got.Header.Get("X-Minion-CSRF") != "csrf-1" || got.Header.Get("X-Forwarded-For") != "203.0.113.9" ||
		got.RemoteAddr != "10.0.0.1:5555" || got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("forwarded request = %s %s %v %s", got.Method, got.URL, got.Header, got.RemoteAddr)
	}
	if string(body) != "{}" {
		t.Errorf("body = %q", body)
	}
}

func TestHandlerCallerDecodesTheErrorEnvelope(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_argument","message":"is required","details":{"field":"client_id"}}}`)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req = req.WithContext(session.WithSession(req.Context(), &session.Session{CSRFToken: "c"}))
	err := NewHandlerCaller(h).CallOp(req, "oidc_provider_set", map[string]string{}, nil)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Status != 400 || oe.Code != "invalid_argument" || oe.Field != "client_id" || opMessage(oe) != "client id is required" {
		t.Fatalf("err = %#v", err)
	}
	// An answer that is no envelope carries no detail of the body.
	h = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "panic: secret stack trace")
	}
	err = NewHandlerCaller(h).CallOp(req, "x", nil, nil)
	if !errors.As(err, &oe) || strings.Contains(oe.Message, "secret") || oe.Status != 500 {
		t.Errorf("err = %#v", err)
	}
}

func TestHandlerCallerNeedsASession(t *testing.T) {
	called := false
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	err := NewHandlerCaller(h).CallOp(httptest.NewRequest("GET", "/", nil), "users_list", nil, nil)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Status != 401 || called {
		t.Errorf("err = %v, called %v", err, called)
	}
}

// -- integration: the real REST pipeline ---------------------------------------

// restAuthn authenticates like the browser session of the Hub does: it needs
// the forwarded cookie and the CSRF token, and answers for admin alice.
type restAuthn struct{}

func (restAuthn) Authenticate(r *http.Request) (api.Identity, error) {
	if c, err := r.Cookie("minion_session"); err != nil || c.Value != "tok" {
		return api.Identity{}, api.NewError(proto.CodeUnauthenticated, "sign in first")
	}
	if r.Header.Get(session.CSRFHeader) != "csrf-alice" {
		return api.Identity{}, api.NewError(proto.CodeForbidden, "missing or invalid CSRF header")
	}
	return api.Identity{Principal: authz.NewSessionPrincipal(adminUserID), Actor: adminUserID, Via: api.ViaSession}, nil
}

// adminOnly allows the actions of the admin and nobody else.
type adminOnly struct{}

func (adminOnly) Decide(_ context.Context, p authz.Principal, _ ops.ActionSet, _ authz.Resource) error {
	if p.UserID != adminUserID {
		return api.NewError(proto.CodeForbidden, "admins only")
	}
	return nil
}

// restAccounts is an in-memory AccountService.
type restAccounts struct {
	mu    sync.Mutex
	users []store.UserAdminRow
}

func (a *restAccounts) ListUsers(context.Context) ([]store.UserAdminRow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]store.UserAdminRow(nil), a.users...), nil
}

func (a *restAccounts) CreateUser(_ context.Context, _ local.Actor, req local.CreateRequest) (local.CreatedUser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.Username == req.Username {
			return local.CreatedUser{}, proto.NewError(proto.CodeConflict, "that username is taken")
		}
	}
	role := req.Role
	if role == "" {
		role = "user"
	}
	row := store.UserRow{ID: otherUserID, Username: req.Username, Email: req.Email, Role: role, Active: true,
		MustChangePassword: true, PasswordHash: "h", CreatedAt: 1_700_000_000_000}
	a.users = append(a.users, store.UserAdminRow{UserRow: row, AdminSources: []string{}})
	return local.CreatedUser{User: row, TemporaryPassword: logx.Secret(tempPassword)}, nil
}

func (a *restAccounts) ResetPassword(context.Context, local.Actor, string) (logx.Secret, error) {
	return "", errors.New("unused")
}
func (a *restAccounts) SetRole(context.Context, local.Actor, string, string) (string, error) {
	return "", errors.New("unused")
}
func (a *restAccounts) Deactivate(context.Context, local.Actor, string) (bool, error) {
	return false, errors.New("unused")
}
func (a *restAccounts) ChangePassword(context.Context, local.Actor, local.PasswordChange) error {
	return errors.New("unused")
}

// Integration: an HTTP admin flow creates a user and changes a setting through
// the real REST dispatcher; the pages see the result on the next load.
func TestAdminFlowOverTheRESTPipeline(t *testing.T) {
	accounts := &restAccounts{users: []store.UserAdminRow{{
		UserRow:      store.UserRow{ID: adminUserID, Username: "alice", Role: "admin", Active: true, PasswordHash: "h", CreatedAt: 1_700_000_000_000},
		AdminSources: []string{"local"},
	}}}
	var settingsMu sync.Mutex
	panel := map[string]string{}
	handlers := api.NewRegistry(ops.Default)
	if err := api.RegisterAccounts(handlers, accounts); err != nil {
		t.Fatal(err)
	}
	if err := api.HandleTyped(handlers, ops.OpSettingsList, func(context.Context, *api.Call, ops.SettingsListArgs) (ops.SettingsListResult, error) {
		settingsMu.Lock()
		defer settingsMu.Unlock()
		info := ops.SettingInfo{Key: "public_url", Env: "MINION_PUBLIC_URL", Source: ops.SettingSourceDefault}
		if v, ok := panel["public_url"]; ok {
			info.Value, info.Source = v, ops.SettingSourcePanel
		}
		return ops.SettingsListResult{Settings: []ops.SettingInfo{info}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.HandleTyped(handlers, ops.OpSettingsSet, func(_ context.Context, _ *api.Call, a ops.SettingsSetArgs) (ops.WriteResult, error) {
		settingsMu.Lock()
		defer settingsMu.Unlock()
		panel[a.Key] = a.Value
		return ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: []ops.Warning{{Code: ops.WarnRestartRequired, Message: "takes effect after the Hub restarts"}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	if _, err := api.NewRouter(&api.Dispatcher{Catalog: ops.Default, Handlers: handlers, Auth: restAuthn{}, Authz: adminOnly{}}, root); err != nil {
		t.Fatal(err)
	}
	web := adminServer(t)
	root.Handle("/", web)
	connect(t, NewHandlerCaller(root))

	// The page request carries the cookie like a browser; the fake session layer
	// of the web server only needs the header.
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("X-Test-Session", adminHeader)
		req.AddCookie(&http.Cookie{Name: "minion_session", Value: "tok"})
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, req)
		return rec
	}

	// Create a user: the temporary password comes back once.
	rec := do("POST", pathAdminUsers, url.Values{"username": {"dave"}, "email": {"dave@example.com"}, "role": {"user"}})
	mustHave(t, rec.Body.String(), "Account dave created.", tempPassword)
	// The list shows the account on the next load, without the password.
	rec = do("GET", pathAdminUsers, nil)
	mustHave(t, rec.Body.String(), `<th scope="row">dave</th>`, "dave@example.com")
	mustNotHave(t, rec.Body.String(), tempPassword)
	// A duplicate is refused with the message of the API.
	rec = do("POST", pathAdminUsers, url.Values{"username": {"dave"}})
	mustHave(t, rec.Body.String(), "that username is taken")
	// A validation error names the field.
	rec = do("POST", pathAdminUsers, url.Values{"username": {" "}})
	mustHave(t, rec.Body.String(), "username must")

	// Change a setting: reload with a flash, then the page shows the new value.
	rec = do("POST", pathAdminSettings+"/public_url", url.Values{"value": {"https://hub.example.com"}, "action": {"set"}})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("settings POST = %d %q", rec.Code, rec.Body)
	}
	if fl := flashesOf(rec); len(fl) != 2 || !strings.Contains(fl[0].Text, "public_url is saved") {
		t.Errorf("flashes = %+v", fl)
	}
	rec = do("GET", pathAdminSettings, nil)
	mustHave(t, rec.Body.String(), `value="https://hub.example.com"`, "Source: panel.")

	// Not signed in at the API (no cookie): the page answers 404, never data.
	req := httptest.NewRequest("GET", pathAdminSettings, nil)
	req.Header.Set("X-Test-Session", adminHeader)
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a page without the session cookie = %d, want 404", rec.Code)
	}
}
