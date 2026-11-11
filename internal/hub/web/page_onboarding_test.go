package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	onbNow       = time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)
	onbTime      = ops.Timestamp(onbNow)
	onbWeek      = ops.Timestamp(onbNow.Add(7 * 24 * time.Hour))
	onbUserID    = "0190a1b2-c3d4-7e5f-8a6b-0000000000b0"
	onbWorkerID  = "0190a1b2-c3d4-7e5f-8a6b-0000000000c1"
	onbWorker2ID = "0190a1b2-c3d4-7e5f-8a6b-0000000000c2"
	onbInviteID  = "0190a1b2-c3d4-7e5f-8a6b-0000000000d1"
	sampleKey    = "mnk_9f2c1ab7.m5xwy3dpnz2g65tfobqxgzljnrswc43foj2xizlsmvzwk5dm"
	sampleAPIKey = "mn_mfrggzdfmztwq2lknnwg23tpobyxe43uov3ho6dzpiyto5lq"
)

// onbSessions attaches a session with a real UUID as the user id, so that the
// ids the pages send pass the validation of the catalog.
type onbSessions struct {
	role   authz.Role
	target string
}

func (o onbSessions) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &session.Session{ID: "s1", UserID: onbUserID, Username: "bob", Role: o.role, CSRFToken: "csrf-bob"}
		if o.target != "" {
			s.ActAs = &session.ActAs{TargetID: adminUserID, TargetUsername: o.target, ExpiresAt: time.Now().Add(time.Hour)}
		}
		next.ServeHTTP(w, r.WithContext(session.WithSession(r.Context(), s)))
	})
}

func onbServer(t *testing.T, layer SessionLayer) *Server {
	t.Helper()
	onboardingNow = func() time.Time { return onbNow }
	t.Cleanup(func() { onboardingNow = time.Now })
	s := actAsServer(t, layer, DefaultRegistry)
	return s
}

func onbUser(t *testing.T) *Server  { return onbServer(t, onbSessions{role: authz.RoleUser}) }
func onbAdmin(t *testing.T) *Server { return onbServer(t, onbSessions{role: authz.RoleAdmin}) }

func onbPost(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func onbGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// -- pure logic --------------------------------------------------------------

func TestInviteCommands(t *testing.T) {
	got := inviteCommands("https://hub.example.com", sampleKey)
	if want := "minion worker up --server https://hub.example.com --invite-key " + sampleKey; got.OneLiner != want {
		t.Errorf("one-liner = %q, want %q", got.OneLiner, want)
	}
	golden(t, "onboarding_invite_commands", got.OneLiner+"\n---\n"+got.FileVariant+"---\n"+got.Compose)
	// The Compose snippet reads the key from a secret file and never holds it.
	if strings.Contains(got.Compose, sampleKey) || !strings.Contains(got.Compose, "MINION_INVITE_KEY_FILE: /run/secrets/minion_invite_key") {
		t.Errorf("compose snippet:\n%s", got.Compose)
	}
	// The Compose project and the file variant are tested for what they do in
	// page_invite_commands_test.go.
	if strings.Contains(got.FileVariant, "invite.key <<") || strings.Contains(got.FileVariant, "cat > invite.key") {
		t.Errorf("file variant writes to a predictable name:\n%s", got.FileVariant)
	}
}

func TestInviteCommandsQuoteTheServer(t *testing.T) {
	got := inviteCommands("https://hub.example.com/x y'z", sampleKey)
	if !strings.Contains(got.OneLiner, `--server 'https://hub.example.com/x y'\''z' `) {
		t.Errorf("one-liner = %q", got.OneLiner)
	}
	if !strings.Contains(got.Compose, `MINION_SERVER: "https://hub.example.com/x y'z"`) {
		t.Errorf("compose = %q", got.Compose)
	}
}

func TestShellArgAndYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		"https://hub:8443": "https://hub:8443", "a b": "'a b'", "it's": `'it'\''s'`, "$(x)": "'$(x)'", "": "''",
	} {
		if got := shellArg(in); got != want {
			t.Errorf("shellArg(%q) = %s, want %s", in, got, want)
		}
	}
	if got := yamlString(`a"b\<c>`); got != `"a\"b\\<c>"` {
		t.Errorf("yamlString = %s", got)
	}
}

func TestHubURLSources(t *testing.T) {
	req := func(host, proto string, tls bool) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		if !tls {
			r.TLS = nil
		}
		return r
	}
	t.Cleanup(func() { SetPublicURL(nil) })
	SetPublicURL(nil)
	if got := hubURL(req("hub.lan:8443", "", false)); got != "http://hub.lan:8443" {
		t.Errorf("request URL = %q", got)
	}
	if got := hubURL(req("hub.lan", "https", false)); got != "https://hub.lan" {
		t.Errorf("forwarded proto = %q", got)
	}
	if got := hubURL(req("evil host/\"", "", false)); got != placeholderHub {
		t.Errorf("invalid host = %q", got)
	}
	SetPublicURL(func(*http.Request) string { return "https://minion.example.org/" })
	if got := hubURL(req("hub.lan", "", false)); got != "https://minion.example.org" {
		t.Errorf("public URL = %q", got)
	}
	SetPublicURL(func(*http.Request) string { return "" })
	if got := hubURL(req("hub.lan", "", false)); got != "http://hub.lan" {
		t.Errorf("empty public URL = %q", got)
	}
}

func TestInvitationRows(t *testing.T) {
	one := 1
	rows := []ops.InvitationInfo{
		{ID: "a", Name: "build-farm", Mode: ops.InvitationReusable, Kind: ops.KindPrivate, Uses: 3, ExpiresAt: &onbWeek, Status: ops.InvitationActive},
		{ID: "b", Mode: ops.InvitationOneTime, Kind: ops.KindInfrastructure, Uses: 1, MaxUses: &one, Status: ops.InvitationExhausted},
	}
	a, b := invitationRow(rows[0]), invitationRow(rows[1])
	if a.Uses != "3" || a.Mode != "reusable" || a.Expires != "2026-11-16 08:30 UTC" || !a.CanRevoke {
		t.Errorf("row a = %+v", a)
	}
	if b.Uses != "1 of 1" || b.Expires != "never" || b.Name != "(no name)" || b.CanRevoke {
		t.Errorf("row b = %+v", b)
	}
}

func TestBuildAPIKeyArgs(t *testing.T) {
	user := pageUser{ID: onbUserID, Username: "bob"}
	admin := pageUser{ID: adminUserID, Username: "alice", Admin: true}
	type tc struct {
		name string
		form url.Values
		u    pageUser
		err  string
		chk  func(t *testing.T, a ops.APIKeyCreateArgs)
	}
	base := func(extra url.Values) url.Values {
		v := url.Values{"name": {"ci"}, "resource": {"user:" + onbUserID}, "action": {"read"}}
		for k, vs := range extra {
			v[k] = vs
		}
		return v
	}
	cases := []tc{
		{name: "account read, default 90 days", form: base(nil), u: user, chk: func(t *testing.T, a ops.APIKeyCreateArgs) {
			if a.Name != "ci" || len(a.Resources) != 1 || a.Resources[0] != (ops.KeyResource{Kind: "user", UserID: onbUserID}) {
				t.Errorf("args = %+v", a)
			}
			if got := a.ExpiresAt.Time(); !got.Equal(onbNow.Add(90 * 24 * time.Hour)) {
				t.Errorf("expires = %v", got)
			}
			if err := a.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		}},
		{name: "never expires for an admin", form: url.Values{"name": {"n"}, "resource": {"everything"}, "action": {"read"}, "expires": {"never"}}, u: admin,
			chk: func(t *testing.T, a ops.APIKeyCreateArgs) {
				if a.ExpiresAt != nil || !a.Never {
					t.Errorf("expires = %v never = %v", a.ExpiresAt, a.Never)
				}
			}},
		{name: "never refused for a user (F-07)", form: base(url.Values{"expires": {"never"}}), u: user, err: "Only an admin can create a key that never expires"},
		{name: "two workers two actions", form: url.Values{"name": {"w"}, "resource": {"worker:" + onbWorkerID, "worker:" + onbWorker2ID, "worker:" + onbWorkerID},
			"action": {"read", "operate", "read"}}, u: user, chk: func(t *testing.T, a ops.APIKeyCreateArgs) {
			if len(a.Resources) != 2 || len(a.Actions) != 2 || a.Actions[1] != ops.ActionOperate {
				t.Errorf("args = %+v", a)
			}
		}},
		{name: "one session", form: url.Values{"name": {"s"}, "resource": {"worker:" + onbWorkerID}, "action": {"read"}, "session": {"build"}}, u: user,
			chk: func(t *testing.T, a ops.APIKeyCreateArgs) {
				if want := (ops.KeyResource{Kind: "session", WorkerID: onbWorkerID, SessionName: "build"}); len(a.Resources) != 1 || a.Resources[0] != want {
					t.Errorf("args = %+v", a)
				}
				if err := a.Validate(); err != nil {
					t.Errorf("Validate: %v", err)
				}
			}},
		{name: "session needs one worker", form: base(url.Values{"session": {"build"}}), u: user, err: "exactly one Worker"},
		{name: "everything for an admin", form: url.Values{"name": {"all"}, "resource": {"everything"}, "action": {"read", "manage"}}, u: admin,
			chk: func(t *testing.T, a ops.APIKeyCreateArgs) {
				if a.Resources[0].Kind != "everything" || a.Actions[1] != ops.ActionManage {
					t.Errorf("args = %+v", a)
				}
			}},
		{name: "everything refused for a user", form: base(url.Values{"resource": {"everything"}}), u: user, err: "Only an admin"},
		{name: "manage refused for a user", form: base(url.Values{"action": {"manage"}}), u: user, err: "Only an admin"},
		{name: "everything alone", form: base(url.Values{"resource": {"everything", "user:" + adminUserID}}), u: admin, err: "cannot be combined"},
		{name: "no name", form: base(url.Values{"name": {"  "}}), u: user, err: "Give the key a name"},
		{name: "long name", form: base(url.Values{"name": {strings.Repeat("x", 65)}}), u: user, err: "at most 64"},
		{name: "no resource", form: base(url.Values{"resource": nil}), u: user, err: "at least one thing the key may reach"},
		{name: "unknown resource", form: base(url.Values{"resource": {"galaxy"}}), u: user, err: "from the list"},
		{name: "no action", form: base(url.Values{"action": nil}), u: user, err: "at least one thing the key may do"},
		{name: "unknown action", form: base(url.Values{"action": {"root"}}), u: user, err: "may do from the list"},
		{name: "unknown expiry", form: base(url.Values{"expires": {"5y"}}), u: user, err: "stays valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := buildAPIKeyArgs(c.form, c.u, onbNow)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.chk(t, a)
		})
	}
}

func TestScopeSummary(t *testing.T) {
	names := map[string]string{onbWorkerID: "pi"}
	k := ops.APIKeyInfo{
		Resources: []ops.KeyResource{{Kind: "user", UserID: onbUserID}, {Kind: "worker", WorkerID: onbWorkerID},
			{Kind: "session", WorkerID: onbWorker2ID, SessionName: "build"}},
		Actions: []ops.Action{ops.ActionRead, ops.ActionOperate},
	}
	want := "my account, Worker pi, session build on 0190a1b2: read, operate"
	if got := scopeSummary(k, onbUserID, names); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	all := ops.APIKeyInfo{Resources: []ops.KeyResource{{Kind: "everything"}}, Actions: []ops.Action{ops.ActionManage}, BoundWorkerID: onbWorkerID}
	if got := scopeSummary(all, onbUserID, names); got != "everything: manage (bound to Worker pi)" {
		t.Errorf("summary = %q", got)
	}
}

func TestAPIKeyRowStates(t *testing.T) {
	past := ops.Timestamp(onbNow.Add(-time.Hour))
	if r := apiKeyRow(ops.APIKeyInfo{Hint: "abcd2345", ExpiresAt: &past}, "", nil, onbNow); r.State != "expired" || !r.CanRevoke || r.Hint != "mn_abcd2345..." {
		t.Errorf("expired row = %+v", r)
	}
	if r := apiKeyRow(ops.APIKeyInfo{RevokedAt: &past}, "", nil, onbNow); r.State != "revoked" || r.CanRevoke {
		t.Errorf("revoked row = %+v", r)
	}
	if r := apiKeyRow(ops.APIKeyInfo{}, "", nil, onbNow); r.LastUsed != "never used" || r.Expires != "never" || r.State != "active" {
		t.Errorf("plain row = %+v", r)
	}
}

func TestPairingErrorText(t *testing.T) {
	for code, want := range map[proto.Code]string{
		proto.CodePairingCodeInvalid:      "not right or has expired",
		proto.CodePairingAttemptsExceeded: "no longer works",
		proto.CodeWorkerOffline:           "not connected",
		proto.CodeRateLimited:             "Wait a minute",
		proto.CodeAlreadyPaired:           "already paired",
		proto.CodeForbidden:               "Sign in again",
		proto.CodeInvalidArgument:         "the Hub says so",
	} {
		got := pairingErrorText(&OpError{Status: 400, Code: string(code), Message: "the Hub says so"})
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q does not contain %q", code, got, want)
		}
	}
	if got := pairingErrorText(errors.New("boom")); strings.Contains(got, "boom") {
		t.Errorf("a plain error leaks its text: %q", got)
	}
}

// -- access ------------------------------------------------------------------

var onbPaths = []string{pathInvitations, pathAPIKeys, pathAddWorker}

func TestOnboardingPagesNeedASession(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	for _, p := range onbPaths {
		rec := get(t, s, p)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("GET %s anonymous = %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	f := newFakeOps()
	connect(t, f)
	for _, p := range onbPaths {
		rec := post(t, s, p, "", url.Values{"name": {"x"}, "code": {"MN-7K2M-Q9XP"}})
		if rec.Header().Get("HX-Redirect") == "" && rec.Code < 300 {
			t.Errorf("POST %s anonymous = %d without a sign-in redirect", p, rec.Code)
		}
	}
	if f.total() != 0 {
		t.Errorf("an anonymous post reached the Hub: %+v", f.calls)
	}
}

func TestOnboardingPagesAreForEveryUserAndInTheNavigation(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpInvitationList, ops.InvitationListResult{}).reply(ops.OpAPIKeyList, ops.APIKeyListResult{}).
		reply(ops.OpWorkersList, ops.WorkersListResult{}))
	for _, role := range []authz.Role{authz.RoleUser, authz.RoleAdmin} {
		s := onbServer(t, onbSessions{role: role})
		for _, p := range onbPaths {
			rec := onbGet(t, s, p)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s as %s = %d", p, role, rec.Code)
			}
			mustHave(t, rec.Body.String(), `<a href="/invitations"`, `<a href="/api-keys"`, `<a href="/workers/add"`)
		}
	}
}

func TestOnboardingWithoutACallerAnswers503(t *testing.T) {
	SetAdminOps(nil)
	s := onbUser(t)
	if rec := onbGet(t, s, pathInvitations); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET without a caller = %d, want 503", rec.Code)
	}
	if rec := onbPost(t, s, pathInvitations, url.Values{"mode": {"one_time"}}); !strings.Contains(rec.Body.String(), "not connected") {
		t.Errorf("POST without a caller = %d %q", rec.Code, rec.Body.String())
	}
}

func TestOnboardingScriptRoute(t *testing.T) {
	s := onbUser(t)
	rec := onbGet(t, s, pathOnboardingJ)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("script = %d %v", rec.Code, rec.Header())
	}
	mustHave(t, rec.Body.String(), "data-copy", "navigator.clipboard.writeText")
	// The pages that need it load it from this origin; none inlines script.
	connect(t, newFakeOps().reply(ops.OpInvitationList, ops.InvitationListResult{}).reply(ops.OpAPIKeyList, ops.APIKeyListResult{}).
		reply(ops.OpWorkersList, ops.WorkersListResult{}))
	for _, p := range []string{pathInvitations, pathAPIKeys} {
		mustHave(t, onbGet(t, s, p).Body.String(), `<script src="/onboarding.js" defer></script>`)
	}
	if csp := onbGet(t, s, pathInvitations).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

// -- invitations page --------------------------------------------------------

func sampleInvitations() ops.InvitationListResult {
	one := 1
	return ops.InvitationListResult{Invitations: []ops.InvitationInfo{
		{ID: onbInviteID, Name: "build-farm", Mode: ops.InvitationReusable, Kind: ops.KindPrivate, TTL: ops.TTL1w, CreatorID: onbUserID,
			CreatedAt: onbTime, ExpiresAt: &onbWeek, Uses: 3, Status: ops.InvitationActive},
		{ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000d2", Name: "laptop", Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTLUnlimited,
			CreatorID: onbUserID, CreatedAt: onbTime, Uses: 0, MaxUses: &one, Status: ops.InvitationActive},
		{ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000d3", Name: "old", Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTL1h,
			CreatorID: onbUserID, CreatedAt: onbTime, ExpiresAt: &onbTime, Uses: 1, MaxUses: &one, Status: ops.InvitationExhausted},
	}}
}

func TestInvitationsPageGolden(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpInvitationList, sampleInvitations()))
	rec := onbGet(t, onbUser(t), pathInvitations)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	golden(t, "onboarding_invitations_user", mainOf(t, rec.Body.String()))
}

func TestInvitationsPageAdminGolden(t *testing.T) {
	f := newFakeOps().reply(ops.OpInvitationList, sampleInvitations())
	connect(t, f)
	rec := onbGet(t, onbAdmin(t), pathInvitations+"?all=1")
	golden(t, "onboarding_invitations_admin_all", mainOf(t, rec.Body.String()))
	if c := f.called(ops.OpInvitationList); len(c) != 1 || c[0].Args["all"] != true {
		t.Errorf("list calls = %+v", c)
	}
}

// AC: a user picks one-time/reusable, the TTLs and a name; the kind is for
// admins only.
func TestInvitationFormChoices(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpInvitationList, ops.InvitationListResult{}))
	user := mainOf(t, onbGet(t, onbUser(t), pathInvitations).Body.String())
	mustHave(t, user, `<option value="one_time" selected>`, `<option value="reusable">`, `name="name"`,
		`<option value="1h">1 hour</option>`, `<option value="4h">4 hours</option>`, `<option value="8h">8 hours</option>`,
		`<option value="24h" selected>24 hours</option>`, `<option value="1w">1 week</option>`,
		`name="max_uses"`, `max="100"`, "There are no invitation keys yet.")
	// F-02: a user cannot choose a key that is valid longer than a week.
	mustNotHave(t, user, `name="kind"`, "infrastructure", "every user", `<option value="1m">`, `<option value="unlimited">`)
	admin := mainOf(t, onbGet(t, onbAdmin(t), pathInvitations).Body.String())
	mustHave(t, admin, `<select id="inv-kind" name="kind">`, `<option value="infrastructure">`, `/invitations?all=1`,
		`<option value="1m">1 month</option>`, `<option value="unlimited">Unlimited</option>`, `blank is no limit`)
	mustNotHave(t, admin, `max="100"`)
}

func TestInvitationFormIsHiddenKindWhileActingAs(t *testing.T) {
	f := newFakeOps().reply(ops.OpInvitationList, ops.InvitationListResult{})
	connect(t, f)
	s := onbServer(t, onbSessions{role: authz.RoleAdmin, target: "carol"})
	body := onbGet(t, s, pathInvitations+"?all=1").Body.String()
	mustNotHave(t, body, `name="kind"`, "every user")
	if c := f.called(ops.OpInvitationList); len(c) != 1 || c[0].Args["all"] == true {
		t.Errorf("an admin acting as a user listed everybody's keys: %+v", c)
	}
}

func TestInvitationCreateShowsTheKeyOnce(t *testing.T) {
	create := ops.InvitationCreateResult{ID: onbInviteID, Key: sampleKey, Name: "build-farm", Mode: ops.InvitationReusable, Kind: ops.KindPrivate,
		TTL: ops.TTL1w, CreatedAt: onbTime, ExpiresAt: &onbWeek}
	f := newFakeOps().reply(ops.OpInvitationCreate, create).reply(ops.OpInvitationList, sampleInvitations())
	connect(t, f)
	SetPublicURL(func(*http.Request) string { return "https://hub.example.com" })
	t.Cleanup(func() { SetPublicURL(nil) })
	s := onbUser(t)

	rec := onbPost(t, s, pathInvitations, url.Values{"mode": {"reusable"}, "ttl": {"1w"}, "name": {" build-farm "}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d\n%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()
	golden(t, "onboarding_invitation_created", body)
	mustHave(t, body,
		`value="`+sampleKey+`"`, `data-copy="#invite-key"`,
		`minion worker up --server https://hub.example.com --invite-key `+sampleKey,
		"MINION_INVITE_KEY_FILE", "shown only once")
	if c := f.called(ops.OpInvitationCreate); len(c) != 1 || c[0].Args["mode"] != "reusable" || c[0].Args["ttl"] != "1w" || c[0].Args["name"] != "build-farm" {
		t.Errorf("create calls = %+v", c)
	}
	// Never again: not in the page, not in a flash, not in a cookie.
	page := onbGet(t, s, pathInvitations)
	mustNotHave(t, page.Body.String(), sampleKey, "m5xwy3dp")
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Value, "m5xwy3dp") {
			t.Errorf("the key reached cookie %s", c.Name)
		}
	}
}

func TestInvitationCreateForAdminSendsTheKindAndShowsErrors(t *testing.T) {
	f := newFakeOps().on(ops.OpInvitationCreate, func(a map[string]any) (any, error) {
		if a["kind"] == "infrastructure" {
			return nil, &OpError{Status: 403, Code: "forbidden", Message: "only an admin can create an infrastructure key"}
		}
		return nil, &OpError{Status: 400, Code: "invalid_argument", Message: "must be one of 1h, 4h, 8h, 24h, 1w, 1m, unlimited", Field: "ttl"}
	})
	connect(t, f)
	s := onbUser(t)
	rec := onbPost(t, s, pathInvitations, url.Values{"mode": {"one_time"}, "ttl": {"2d"}})
	mustHave(t, rec.Body.String(), `<p class="form-error" role="alert">ttl must be one of`)
	rec = onbPost(t, s, pathInvitations, url.Values{"mode": {"one_time"}, "ttl": {"1h"}, "kind": {"infrastructure"}})
	mustHave(t, rec.Body.String(), "only an admin can create an infrastructure key")
	if c := f.called(ops.OpInvitationCreate); len(c) != 2 || c[1].Args["kind"] != "infrastructure" {
		t.Errorf("calls = %+v", c)
	}
}

func TestInvitationRevoke(t *testing.T) {
	f := newFakeOps().reply(ops.OpInvitationRevoke, delivered)
	connect(t, f)
	s := onbUser(t)
	rec := onbPost(t, s, pathInvitations+"/"+onbInviteID+"/revoke", url.Values{})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("revoke = %d %q", rec.Code, rec.Body)
	}
	if fl := flashesOf(rec); len(fl) != 1 || !strings.Contains(fl[0].Text, "revoked") {
		t.Errorf("flashes = %+v", fl)
	}
	c := f.called(ops.OpInvitationRevoke)
	if len(c) != 1 || c[0].Args["id"] != onbInviteID || !ops.ValidUUIDv7(c[0].Args["request_id"].(string)) {
		t.Errorf("revoke calls = %+v", c)
	}
	// A refusal of the Hub is shown in place.
	f.on(ops.OpInvitationRevoke, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 404, Code: "not_found", Message: "no such invitation"}
	})
	mustHave(t, onbPost(t, s, pathInvitations+"/"+onbInviteID+"/revoke", url.Values{}).Body.String(), "no such invitation")
}

func TestInvitationRevokeButtonOnlyForActiveKeys(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpInvitationList, sampleInvitations()))
	body := onbGet(t, onbUser(t), pathInvitations).Body.String()
	if got := strings.Count(body, `hx-post="/invitations/`); got != 2 {
		t.Errorf("revoke forms = %d, want 2 (active keys only)", got)
	}
}

// -- API keys page -----------------------------------------------------------

func sampleAPIKeys() map[string]any {
	return map[string]any{"keys": []map[string]any{
		{"id": "0190a1b2-c3d4-7e5f-8a6b-0000000000e1", "name": "ci", "hint": "mfrggzdf", "owner_id": onbUserID,
			"resources": []ops.KeyResource{{Kind: "worker", WorkerID: onbWorkerID}}, "actions": []string{"read", "operate"},
			"created_at": onbTime, "expires_at": onbWeek, "last_used_at": ops.Timestamp(onbNow.Add(-time.Hour))},
		{"id": "0190a1b2-c3d4-7e5f-8a6b-0000000000e2", "name": "mcp", "hint": "mztwq2lk", "owner_id": onbUserID,
			"resources": []ops.KeyResource{{Kind: "user", UserID: onbUserID}}, "actions": []string{"read"}, "created_at": onbTime},
		{"id": "0190a1b2-c3d4-7e5f-8a6b-0000000000e3", "name": "gone", "hint": "nnwg23tp", "owner_id": onbUserID,
			"resources": []ops.KeyResource{{Kind: "session", WorkerID: onbWorker2ID, SessionName: "build"}}, "actions": []string{"message"},
			"created_at": onbTime, "revoked_at": onbTime},
	}}
}

func sampleWorkers() ops.WorkersListResult {
	return ops.WorkersListResult{Workers: []ops.WorkerInfo{
		{ID: onbWorkerID, Name: "pi", OwnerID: onbUserID, Owner: "bob", Kind: ops.WorkerKindPrivate, Online: true},
		{ID: onbWorker2ID, Name: "nuc", OwnerID: onbUserID, Owner: "bob", Kind: ops.WorkerKindPrivate},
	}}
}

func apiKeyOps() *fakeOps {
	return newFakeOps().reply(ops.OpAPIKeyList, sampleAPIKeys()).reply(ops.OpWorkersList, sampleWorkers())
}

func TestAPIKeysPageGolden(t *testing.T) {
	connect(t, apiKeyOps())
	rec := onbGet(t, onbUser(t), pathAPIKeys)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	golden(t, "onboarding_apikeys_user", mainOf(t, body))
	// The list shows hint, scope, expiry and last use; never a key.
	mustHave(t, body, "<code>mn_mfrggzdf...</code>", "Worker pi: read, operate", "my account: read",
		"2026-11-16 08:30 UTC", "2026-11-09 07:30 UTC", "never used", "revoked")
	mustNotHave(t, body, "mn_mfrggzdfmztwq")
}

// AC: pickers limited to what the user owns; admins see "everything" and manage.
func TestAPIKeyPickersFollowOwnership(t *testing.T) {
	connect(t, apiKeyOps())
	user := mainOf(t, onbGet(t, onbUser(t), pathAPIKeys).Body.String())
	mustHave(t, user, `value="user:`+onbUserID+`" checked`, `value="worker:`+onbWorkerID+`"`, `value="worker:`+onbWorker2ID+`"`,
		`value="read" checked`, `value="operate"`, `value="message"`)
	mustNotHave(t, user, `value="everything"`, `value="manage"`)
	admin := mainOf(t, onbGet(t, onbAdmin(t), pathAPIKeys).Body.String())
	mustHave(t, admin, `value="everything"`, `value="manage"`)
	mustNotHave(t, admin, `value="everything" checked`)
	acting := mainOf(t, onbGet(t, onbServer(t, onbSessions{role: authz.RoleAdmin, target: "carol"}), pathAPIKeys).Body.String())
	mustNotHave(t, acting, `value="everything"`, `value="manage"`)
	// While acting, the account choice is the target's.
	mustHave(t, acting, `value="user:`+adminUserID+`"`)
}

func TestAPIKeyPickersOnlyOfferOwnWorkers(t *testing.T) {
	foreign := sampleWorkers()
	foreign.Workers = append(foreign.Workers, ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000c9", Name: "theirs", OwnerID: otherUserID})
	connect(t, newFakeOps().reply(ops.OpAPIKeyList, sampleAPIKeys()).reply(ops.OpWorkersList, foreign))
	mustNotHave(t, onbGet(t, onbUser(t), pathAPIKeys).Body.String(), "theirs")
}

func TestAPIKeysPageWithoutTheWorkerList(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpAPIKeyList, sampleAPIKeys()).on(ops.OpWorkersList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 500, Code: "internal", Message: "x"}
	}))
	rec := onbGet(t, onbUser(t), pathAPIKeys)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(), "could not be loaded", `value="user:`+onbUserID+`"`)
}

func TestAPIKeyCreateShowsTheKeyOnce(t *testing.T) {
	res := ops.APIKeyCreateResult{Key: sampleAPIKey, Info: ops.APIKeyInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000e1", Name: "ci", Hint: "mfrggzdf",
		OwnerID: onbUserID, Resources: []ops.KeyResource{{Kind: "worker", WorkerID: onbWorkerID}}, Actions: []ops.Action{ops.ActionRead},
		CreatedAt: onbTime, ExpiresAt: &onbWeek}}
	f := apiKeyOps().reply(ops.OpAPIKeyCreate, res)
	connect(t, f)
	s := onbUser(t)
	rec := onbPost(t, s, pathAPIKeys, url.Values{"name": {"ci"}, "resource": {"worker:" + onbWorkerID}, "action": {"read"}, "expires": {"30d"}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d\n%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()
	golden(t, "onboarding_apikey_created", body)
	mustHave(t, body, `value="`+sampleAPIKey+`"`, `data-copy="#api-key"`, "shown only once", "mn_mfrggzdf...")
	c := f.called(ops.OpAPIKeyCreate)
	if len(c) != 1 {
		t.Fatalf("create calls = %+v", c)
	}
	if got := c[0].Args["expires_at"]; got != "2026-12-09T08:30:00.000Z" {
		t.Errorf("expires_at = %v", got)
	}
	// The next load lists the hint, not the key.
	mustNotHave(t, onbGet(t, s, pathAPIKeys).Body.String(), sampleAPIKey)
}

func TestAPIKeyCreateErrors(t *testing.T) {
	f := apiKeyOps().on(ops.OpAPIKeyCreate, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "you do not own that resource"}
	})
	connect(t, f)
	s := onbUser(t)
	// A form mistake is answered without calling the Hub.
	rec := onbPost(t, s, pathAPIKeys, url.Values{"name": {"ci"}, "action": {"read"}})
	mustHave(t, rec.Body.String(), `<p class="form-error" role="alert">Choose at least one thing the key may reach.</p>`)
	rec = onbPost(t, s, pathAPIKeys, url.Values{"name": {"ci"}, "resource": {"everything"}, "action": {"manage"}})
	mustHave(t, rec.Body.String(), "Only an admin")
	if len(f.called(ops.OpAPIKeyCreate)) != 0 {
		t.Error("a bad form reached the Hub")
	}
	// The Hub has the last word.
	rec = onbPost(t, s, pathAPIKeys, url.Values{"name": {"ci"}, "resource": {"worker:" + onbWorkerID}, "action": {"read"}})
	mustHave(t, rec.Body.String(), "you do not own that resource")
	mustNotHave(t, rec.Body.String(), "mn_")
}

// An admin who picks manage with narrow resources gets the explanation and no Hub call.
func TestAPIKeyAdminManageNeedsEverything(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"chosen user", url.Values{"name": {"m"}, "resource": {"user:" + adminUserID}, "action": {"manage"}}},
		{"chosen worker", url.Values{"name": {"m"}, "resource": {"worker:" + onbWorkerID}, "action": {"manage"}}},
		{"chosen session", url.Values{"name": {"m"}, "resource": {"worker:" + onbWorkerID}, "session": {"build"}, "action": {"manage"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := apiKeyOps()
			connect(t, f)
			rec := onbPost(t, onbAdmin(t), pathAPIKeys, c.form)
			mustHave(t, rec.Body.String(), "The manage action is available only for a key that reaches everything.")
			if n := len(f.called(ops.OpAPIKeyCreate)); n != 0 {
				t.Errorf("apikey_create called %d times", n)
			}
		})
	}
}

func TestAPIKeyRevoke(t *testing.T) {
	f := newFakeOps().reply(ops.OpAPIKeyRevoke, delivered)
	connect(t, f)
	id := "0190a1b2-c3d4-7e5f-8a6b-0000000000e1"
	rec := onbPost(t, onbUser(t), pathAPIKeys+"/"+id+"/revoke", url.Values{})
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("revoke = %d %q", rec.Code, rec.Body)
	}
	if c := f.called(ops.OpAPIKeyRevoke); len(c) != 1 || c[0].Args["id"] != id || c[0].User != "bob" {
		t.Errorf("revoke calls = %+v", c)
	}
}

// -- add worker --------------------------------------------------------------

func TestAddWorkerPageGolden(t *testing.T) {
	connect(t, newFakeOps())
	rec := onbGet(t, onbUser(t), pathAddWorker)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	golden(t, "onboarding_addworker", mainOf(t, rec.Body.String()))
}

// claimOps answers workers_list before and after the claim, like the Hub does.
func claimOps(claimed bool, claim func(map[string]any) (any, error)) *fakeOps {
	f := newFakeOps()
	f.on(ops.OpWorkersList, func(map[string]any) (any, error) {
		list := ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: onbWorkerID, Name: "pi", OwnerID: onbUserID}}}
		// Every request lists twice: before the claim and after it.
		f.mu.Lock()
		lists := 0
		for _, c := range f.calls {
			if c.Op == ops.OpWorkersList {
				lists++
			}
		}
		f.mu.Unlock()
		if claimed && lists%2 == 0 {
			list.Workers = append(list.Workers, ops.WorkerInfo{ID: onbWorker2ID, Name: "kitchen-nuc", OwnerID: onbUserID})
		}
		return list, nil
	})
	f.on(ops.OpPairingClaim, claim)
	return f
}

func TestAddWorkerSuccessNamesTheWorker(t *testing.T) {
	f := claimOps(true, func(map[string]any) (any, error) { return delivered, nil })
	connect(t, f)
	s := onbUser(t)
	for _, typed := range []string{"MN-7K2M-Q9XP", "mn-7k2m-q9xp", "mn7k2mq9xp", " 7k2m q9xp ", "MN-7K2M-Q9XP\n"} {
		rec := onbPost(t, s, pathAddWorker, url.Values{"code": {typed}})
		if rec.Code != 200 {
			t.Fatalf("%q: POST = %d", typed, rec.Code)
		}
		mustHave(t, rec.Body.String(), "<strong>kitchen-nuc is now in your space.</strong>")
		mustNotHave(t, rec.Body.String(), "7K2M", "form-error")
	}
	c := f.called(ops.OpPairingClaim)
	if len(c) != 5 {
		t.Fatalf("claims = %d", len(c))
	}
	for _, cl := range c {
		if cl.Args["code"] != "MN-7K2M-Q9XP" || !ops.ValidUUIDv7(cl.Args["request_id"].(string)) {
			t.Errorf("claim args = %+v", cl.Args)
		}
		if _, has := cl.Args["name"]; has {
			t.Errorf("a name was sent without being typed: %+v", cl.Args)
		}
	}
}

func TestAddWorkerSuccessGolden(t *testing.T) {
	f := claimOps(true, func(map[string]any) (any, error) {
		return ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: []ops.Warning{{Code: ops.WarnWorkerRenamed,
			Message: "the name was already in use in your space; the Worker is called kitchen-nuc"}}}, nil
	})
	connect(t, f)
	rec := onbPost(t, onbUser(t), pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}, "name": {"kitchen"}})
	golden(t, "onboarding_worker_added", rec.Body.String())
	if c := f.called(ops.OpPairingClaim); len(c) != 1 || c[0].Args["name"] != "kitchen" {
		t.Errorf("claims = %+v", c)
	}
}

func TestAddWorkerFallsBackToTheTypedName(t *testing.T) {
	f := newFakeOps().reply(ops.OpPairingClaim, delivered).on(ops.OpWorkersList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 500, Code: "internal", Message: "x"}
	})
	connect(t, f)
	s := onbUser(t)
	mustHave(t, onbPost(t, s, pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}, "name": {"attic"}}).Body.String(), "attic is now in your space.")
	mustHave(t, onbPost(t, s, pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}}).Body.String(), "The Worker is now in your space.")
}

func TestAddWorkerErrorsArePlainEnglishAndNeverEchoTheCode(t *testing.T) {
	cases := map[string]string{
		"pairing_code_invalid":      "not right or has expired",
		"pairing_attempts_exceeded": "Too many wrong attempts",
		"worker_offline":            "not connected to the Hub",
		"rate_limited":              "Wait a minute",
	}
	for code, want := range cases {
		f := claimOps(false, func(map[string]any) (any, error) {
			return nil, &OpError{Status: 400, Code: code, Message: "raw hub text MN-7K2M-Q9XP"}
		})
		connect(t, f)
		rec := onbPost(t, onbUser(t), pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}})
		body := rec.Body.String()
		mustHave(t, body, `<p class="form-error" role="alert">`, want)
		mustNotHave(t, body, "7K2M", "raw hub text", "is now in your space")
		if fl := flashesOf(rec); len(fl) != 0 {
			t.Errorf("%s: flashes = %+v", code, fl)
		}
	}
}

func TestAddWorkerRejectsMalformedInputWithoutCallingTheHub(t *testing.T) {
	f := newFakeOps().reply(ops.OpPairingClaim, delivered)
	connect(t, f)
	s := onbUser(t)
	for _, code := range []string{"", "MN-1234", "MN-1234-56789", "MN-UUUU-UUUU", "hello world!"} {
		rec := onbPost(t, s, pathAddWorker, url.Values{"code": {code}})
		mustHave(t, rec.Body.String(), "That is not a pairing code")
		if code != "" {
			mustNotHave(t, rec.Body.String(), code)
		}
	}
	rec := onbPost(t, s, pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}, "name": {"has space"}})
	mustHave(t, rec.Body.String(), "without spaces or colons")
	if f.total() != 0 {
		t.Errorf("the Hub was called: %+v", f.calls)
	}
}

func TestAddWorkerUndeliveredOutcome(t *testing.T) {
	f := newFakeOps().reply(ops.OpPairingClaim, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "worker_offline"}).
		reply(ops.OpWorkersList, ops.WorkersListResult{})
	connect(t, f)
	mustHave(t, onbPost(t, onbUser(t), pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}}).Body.String(), "could not be added (worker offline)")
}

func TestAddWorkerEscapesTheNameInTheAnswer(t *testing.T) {
	f := claimOps(false, func(map[string]any) (any, error) { return delivered, nil })
	connect(t, f)
	f.on(ops.OpWorkersList, func(map[string]any) (any, error) { return nil, &OpError{Status: 500, Code: "internal", Message: "x"} })
	rec := onbPost(t, onbUser(t), pathAddWorker, url.Values{"code": {"MN-7K2M-Q9XP"}, "name": {"<b>x</b>"}})
	mustNotHave(t, rec.Body.String(), "<b>x</b>")
	mustHave(t, rec.Body.String(), "&lt;b&gt;x&lt;/b&gt;")
}

// -- integration: the real REST pipeline --------------------------------------

// onbAuthn authenticates the browser session of user bob like the Hub does.
type onbAuthn struct{}

func (onbAuthn) Authenticate(r *http.Request) (api.Identity, error) {
	if c, err := r.Cookie("minion_session"); err != nil || c.Value != "tok" {
		return api.Identity{}, api.NewError(proto.CodeUnauthenticated, "sign in first")
	}
	if r.Header.Get(session.CSRFHeader) != "csrf-bob" {
		return api.Identity{}, api.NewError(proto.CodeForbidden, "missing or invalid CSRF header")
	}
	return api.Identity{Principal: authz.NewSessionPrincipal(onbUserID), Actor: onbUserID, Via: api.ViaSession}, nil
}

// onbDecider lets a user act on their own resources and refuses manage.
type onbDecider struct{}

func (onbDecider) Decide(_ context.Context, p authz.Principal, accepted ops.ActionSet, _ authz.Resource) error {
	for _, a := range accepted {
		if a == ops.ActionManage {
			return api.NewError(proto.CodeForbidden, "not allowed")
		}
	}
	if p.UserID != onbUserID {
		return api.NewError(proto.CodeForbidden, "not allowed")
	}
	return nil
}

// memInvites is an in-memory InvitationService.
type memInvites struct {
	mu   sync.Mutex
	list []ops.InvitationInfo
}

func (m *memInvites) Create(_ context.Context, c workers.InvitationCaller, a ops.InvitationCreateArgs) (ops.InvitationCreateResult, error) {
	if err := a.Validate(); err != nil {
		return ops.InvitationCreateResult{}, api.NewError(proto.CodeInvalidArgument, err.Error())
	}
	if a.EffectiveKind() == ops.KindInfrastructure && !c.Admin {
		return ops.InvitationCreateResult{}, api.NewError(proto.CodeForbidden, "only an admin can create an infrastructure key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := ops.NewUUIDv7()
	d, _ := ops.InvitationTTLDuration(a.TTL)
	var exp *ops.Timestamp
	if d > 0 {
		ts := ops.Timestamp(onbNow.Add(d))
		exp = &ts
	}
	info := ops.InvitationInfo{ID: id, Name: a.Name, Mode: a.Mode, Kind: a.EffectiveKind(), TTL: a.TTL, CreatorID: c.UserID,
		CreatedAt: onbTime, ExpiresAt: exp, Status: ops.InvitationActive}
	if a.Mode == ops.InvitationOneTime {
		one := 1
		info.MaxUses = &one
	}
	m.list = append(m.list, info)
	return ops.InvitationCreateResult{ID: id, Key: sampleKey, Name: a.Name, Mode: a.Mode, Kind: info.Kind, TTL: a.TTL, CreatedAt: onbTime, ExpiresAt: exp}, nil
}

func (m *memInvites) List(_ context.Context, c workers.InvitationCaller, _ bool) (ops.InvitationListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ops.InvitationInfo
	for _, i := range m.list {
		if i.CreatorID == c.UserID {
			out = append(out, i)
		}
	}
	return ops.InvitationListResult{Invitations: out}, nil
}

func (m *memInvites) Revoke(_ context.Context, _ workers.InvitationCaller, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == id {
			m.list[i].Status, m.list[i].Revoked = ops.InvitationRevoked, true
			return nil
		}
	}
	return api.NewError(proto.CodeNotFound, "no such invitation")
}

// memKeys is an in-memory KeyManager.
type memKeys struct {
	mu   sync.Mutex
	keys []ops.APIKeyInfo
}

func (m *memKeys) Create(_ context.Context, p authz.Principal, a ops.APIKeyCreateArgs) (ops.APIKeyCreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := ops.APIKeyInfo{ID: ops.NewUUIDv7(), Name: a.Name, Hint: "mfrggzdf", OwnerID: p.UserID, Resources: a.Resources, Actions: a.Actions,
		CreatedAt: onbTime, ExpiresAt: a.ExpiresAt}
	m.keys = append(m.keys, info)
	return ops.APIKeyCreateResult{Key: sampleAPIKey, Info: info}, nil
}

func (m *memKeys) List(context.Context, authz.Principal, ops.APIKeyListArgs) (ops.APIKeyListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ops.APIKeyListResult{Keys: append([]ops.APIKeyInfo(nil), m.keys...)}, nil
}

func (m *memKeys) Revoke(_ context.Context, _ authz.Principal, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.keys {
		if m.keys[i].ID == id {
			ts := onbTime
			m.keys[i].RevokedAt = &ts
			return nil
		}
	}
	return api.NewError(proto.CodeNotFound, "no such key")
}

// Integration: an HTTP flow creates an invitation key and an API key and adds a
// Worker through the real REST dispatcher; the pages show the results on the
// next load, and the secrets only in the answers that created them.
func TestOnboardingFlowOverTheRESTPipeline(t *testing.T) {
	invites, keys := &memInvites{}, &memKeys{}
	handlers := api.NewRegistry(ops.Default)
	if err := api.RegisterInvitationHandlers(handlers, invites, onbDecider{}); err != nil {
		t.Fatal(err)
	}
	if err := api.RegisterAPIKeyHandlers(handlers, keys); err != nil {
		t.Fatal(err)
	}
	var claimed []api.PairingClaim
	if err := api.RegisterPairing(handlers, api.PairingClaimFunc(func(_ context.Context, c api.PairingClaim) (api.PairingClaimed, error) {
		if c.Code != "MN-7K2M-Q9XP" {
			return api.PairingClaimed{}, proto.NewError(proto.CodePairingCodeInvalid, "wrong code")
		}
		claimed = append(claimed, c)
		return api.PairingClaimed{WorkerID: onbWorker2ID, Name: "kitchen-nuc"}, nil
	})); err != nil {
		t.Fatal(err)
	}
	listed := func() ops.WorkersListResult {
		out := sampleWorkers()
		if len(claimed) > 0 {
			out.Workers = append(out.Workers, ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000c3", Name: "kitchen-nuc", OwnerID: onbUserID})
		}
		return out
	}
	if err := api.HandleTyped(handlers, ops.OpWorkersList, func(context.Context, *api.Call, ops.WorkersListArgs) (ops.WorkersListResult, error) {
		return listed(), nil
	}); err != nil {
		t.Fatal(err)
	}
	root := http.NewServeMux()
	if _, err := api.NewRouter(&api.Dispatcher{Catalog: ops.Default, Handlers: handlers, Auth: onbAuthn{}, Authz: onbDecider{}}, root); err != nil {
		t.Fatal(err)
	}
	root.Handle("/", onbUser(t))
	connect(t, NewHandlerCaller(root))

	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, body)
		req.AddCookie(&http.Cookie{Name: "minion_session", Value: "tok"})
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		root.ServeHTTP(rec, req)
		return rec
	}

	// Invitation: create, see it in the list, never the key, revoke.
	rec := do("POST", pathInvitations, url.Values{"mode": {"reusable"}, "ttl": {"8h"}, "name": {"lab"}})
	mustHave(t, rec.Body.String(), "Invitation key \"lab\" created.", `value="`+sampleKey+`"`, "--invite-key "+sampleKey)
	rec = do("GET", pathInvitations, nil)
	mustHave(t, rec.Body.String(), `<th scope="row">lab</th>`, "reusable", "2026-11-09 16:30 UTC", "active")
	mustNotHave(t, rec.Body.String(), sampleKey)
	rec = do("POST", pathInvitations, url.Values{"mode": {"one_time"}, "ttl": {"3d"}})
	mustHave(t, rec.Body.String(), "ttl must be one of")
	rec = do("POST", pathInvitations, url.Values{"mode": {"one_time"}, "ttl": {"1h"}, "kind": {"infrastructure"}})
	mustHave(t, rec.Body.String(), "only an admin can create an infrastructure key")
	id := invites.list[0].ID
	if rec = do("POST", pathInvitations+"/"+id+"/revoke", url.Values{}); rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("revoke = %d %q", rec.Code, rec.Body)
	}
	rec = do("GET", pathInvitations, nil)
	mustHave(t, rec.Body.String(), "revoked")
	mustNotHave(t, rec.Body.String(), `hx-post="/invitations/`)

	// API key: pick the account and a Worker, see the key once.
	rec = do("GET", pathAPIKeys, nil)
	mustHave(t, rec.Body.String(), `value="user:`+onbUserID+`" checked`, `value="worker:`+onbWorkerID+`"`)
	rec = do("POST", pathAPIKeys, url.Values{"name": {"ci"}, "resource": {"worker:" + onbWorkerID}, "action": {"read", "operate"}, "expires": {"90d"}})
	mustHave(t, rec.Body.String(), `API key "ci" created.`, `value="`+sampleAPIKey+`"`, "Scope: Worker pi: read, operate")
	rec = do("GET", pathAPIKeys, nil)
	mustHave(t, rec.Body.String(), `<th scope="row">ci</th>`, "<code>mn_mfrggzdf...</code>", "Worker pi: read, operate")
	mustNotHave(t, rec.Body.String(), sampleAPIKey)
	if rec = do("POST", pathAPIKeys, url.Values{"name": {"x"}, "resource": {"everything"}, "action": {"read"}}); !strings.Contains(rec.Body.String(), "Only an admin") {
		t.Errorf("everything for a user = %q", rec.Body)
	}
	if rec = do("POST", pathAPIKeys+"/"+keys.keys[0].ID+"/revoke", url.Values{}); rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("revoke key = %d %q", rec.Code, rec.Body)
	}
	mustHave(t, do("GET", pathAPIKeys, nil).Body.String(), "revoked")

	// Add worker: a wrong code is refused in plain English, the right one pairs.
	rec = do("POST", pathAddWorker, url.Values{"code": {"MN-0000-0000"}})
	mustHave(t, rec.Body.String(), "not right or has expired")
	mustNotHave(t, rec.Body.String(), "0000-0000")
	rec = do("POST", pathAddWorker, url.Values{"code": {"mn7k2mq9xp"}, "name": {"kitchen"}})
	mustHave(t, rec.Body.String(), "kitchen-nuc is now in your space.")
	if len(claimed) != 1 || claimed[0].UserID != onbUserID || claimed[0].Name != "kitchen" {
		t.Errorf("claims = %+v", claimed)
	}

	// Without the session cookie the API refuses and the pages never show data.
	req := httptest.NewRequest("GET", pathInvitations, nil)
	rec = httptest.NewRecorder()
	root.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a page without the session cookie = %d, want 404", rec.Code)
	}
}

// F-01 (T2 round 2): the browser form binds a key to one of the user's
// Workers (a call key); the target scope is chosen like for any key.
func TestAPIKeyFormBindsAKeyToAWorker(t *testing.T) {
	u := pageUser{ID: onbUserID}
	form := url.Values{
		"name": {"call"}, "resource": {"worker:" + onbWorker2ID}, "action": {"message", "read"},
		"bound_worker": {onbWorkerID},
	}
	a, err := buildAPIKeyArgs(form, u, onbNow)
	if err != nil {
		t.Fatal(err)
	}
	if a.BoundWorkerID != onbWorkerID || len(a.Resources) != 1 || a.Resources[0].WorkerID != onbWorker2ID {
		t.Fatalf("args = %+v", a)
	}
	form.Del("bound_worker")
	if a, err = buildAPIKeyArgs(form, u, onbNow); err != nil || a.BoundWorkerID != "" {
		t.Fatalf("without the choice the key is unbound: %+v, %v", a, err)
	}
	ws := []ops.WorkerInfo{{ID: onbWorkerID, Name: "pi", OwnerID: onbUserID}, {ID: onbWorker2ID, Name: "other", OwnerID: "someone-else"}}
	ch := apiKeyBoundWorkerChoices(u, ws)
	if len(ch) != 1 || ch[0].Value != onbWorkerID {
		t.Fatalf("choices = %+v, want only the user's own Worker", ch)
	}
}
