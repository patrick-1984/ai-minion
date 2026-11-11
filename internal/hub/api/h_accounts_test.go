package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	acctAdmin  = "0190a1b2-c3d4-7e5f-8a6b-000000000001"
	acctUser   = "0190a1b2-c3d4-7e5f-8a6b-000000000002"
	acctTarget = "0190a1b2-c3d4-7e5f-8a6b-000000000003"
	acctReq    = "0190a1b2-c3d4-7e5f-8a6b-0000000000aa"
)

var accountOps = []string{ops.OpUsersList, ops.OpUserCreate, ops.OpUserResetPassword, ops.OpUserSetRole, ops.OpUserDeactivate, ops.OpPasswordChange}

// fakeAccounts is the AccountService behind the handlers.
type fakeAccounts struct {
	mu    sync.Mutex
	calls []string
	actor local.Actor
	creq  local.CreateRequest
	pw    local.PasswordChange
	uid   string
	keep  bool
	role  string
	err   error
	rows  []store.UserAdminRow
}

func (f *fakeAccounts) record(name string, a local.Actor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	f.actor = a
}

func (f *fakeAccounts) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAccounts) ListUsers(context.Context) ([]store.UserAdminRow, error) {
	f.record("list", local.Actor{})
	return f.rows, f.err
}

func (f *fakeAccounts) CreateUser(_ context.Context, a local.Actor, r local.CreateRequest) (local.CreatedUser, error) {
	f.record("create", a)
	f.creq = r
	if f.err != nil {
		return local.CreatedUser{}, f.err
	}
	return local.CreatedUser{
		User:              store.UserRow{ID: acctTarget, Username: r.Username, Email: r.Email, Role: ifEmpty(r.Role, "user"), Active: true, MustChangePassword: true, PasswordHash: "h", CreatedAt: 1_700_000_000_000},
		TemporaryPassword: logx.Secret("TEMP-pass-0123456789abcdef"),
	}, nil
}

func ifEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (f *fakeAccounts) ResetPassword(_ context.Context, a local.Actor, id string, keepAPIKeys bool) (logx.Secret, error) {
	f.record("reset", a)
	f.uid, f.keep = id, keepAPIKeys
	return logx.Secret("RESET-pass-0123456789abcdef"), f.err
}

func (f *fakeAccounts) SetRole(_ context.Context, a local.Actor, id, role string) (string, error) {
	f.record("set_role", a)
	f.uid, f.role = id, role
	return role, f.err
}

func (f *fakeAccounts) Deactivate(_ context.Context, a local.Actor, id string) (bool, error) {
	f.record("deactivate", a)
	f.uid = id
	return true, f.err
}

func (f *fakeAccounts) ChangePassword(_ context.Context, a local.Actor, r local.PasswordChange) error {
	f.record("change", a)
	f.pw = r
	return f.err
}

type acctRig struct {
	*rig
	svc *fakeAccounts
}

func newAcctRig(t *testing.T, id Identity) *acctRig {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range accountOps {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("ops.Default lacks %s", n)
		}
		cat.Register(op)
	}
	r := &rig{
		authn: &fakeAuthn{id: id}, decide: &fakeDecider{}, audit: &fakeAudit{},
		reg: NewRegistry(cat), mux: http.NewServeMux(), logs: &strings.Builder{},
	}
	r.d = &Dispatcher{Catalog: cat, Handlers: r.reg, Auth: r.authn, Authz: r.decide, Audit: r.audit, Log: testLogger(r.logs)}
	var err error
	if r.router, err = NewRouter(r.d, r.mux); err != nil {
		t.Fatal(err)
	}
	svc := &fakeAccounts{}
	if err := RegisterAccounts(r.reg, svc); err != nil {
		t.Fatal(err)
	}
	return &acctRig{rig: r, svc: svc}
}

func sessionIdentity(user string) Identity {
	p := authz.NewSessionPrincipal(user)
	return Identity{Principal: p, Actor: user, Via: ViaSession, Session: &session.Session{ID: "sess-1", UserID: user}}
}

func actAsIdentity(admin, target string) Identity {
	p := authz.NewActAsPrincipal(admin, target, time.Now().Add(time.Hour))
	return Identity{Principal: p, Actor: admin, ActingAs: target, Via: ViaSession, Session: &session.Session{ID: "sess-1", UserID: target}}
}

type scopeOf []ops.Action

func (s scopeOf) Actions(authz.ResolvedResource) []ops.Action { return s }

func acctKeyIdentity(owner string, actions ...ops.Action) Identity {
	return Identity{Principal: authz.NewAPIKeyPrincipal(owner, scopeOf(actions), ""), Actor: "key:k1", Via: ViaAPIKey}
}

func body(m map[string]any) string {
	m["request_id"] = acctReq
	b, _ := json.Marshal(m)
	return string(b)
}

type errBody struct {
	Error struct {
		Code    proto.Code     `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error envelope: %s", w.Body)
	}
	return e
}

func TestRegisterAccountsRegistersTheSixOperations(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	names := r.reg.Names()
	for _, n := range accountOps {
		found := false
		for _, x := range names {
			found = found || x == n
		}
		if !found {
			t.Errorf("%s has no handler", n)
		}
	}
	if err := RegisterAccounts(r.reg, r.svc); err == nil {
		t.Error("registering twice succeeded")
	}
	if err := RegisterAccounts(NewRegistry(ops.NewRegistry()), r.svc); err == nil {
		t.Error("registering into a catalog without the operations succeeded")
	}
}

func TestUserCreateReturnsThePasswordOnceAndAuditsWithoutIt(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	w := r.do("POST", "/api/v1/ops/user_create", body(map[string]any{"username": "ann", "email": "ann@example.com", "role": "admin"}))
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	var got struct {
		Outcome           string       `json:"outcome"`
		Warnings          []any        `json:"warnings"`
		TemporaryPassword string       `json:"temporary_password"`
		User              ops.UserInfo `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != "delivered" || got.Warnings == nil || got.TemporaryPassword != "TEMP-pass-0123456789abcdef" {
		t.Errorf("body = %s", w.Body)
	}
	if got.User.ID != acctTarget || got.User.Username != "ann" || got.User.Role != "admin" || !got.User.MustChangePassword ||
		len(got.User.AdminSources) != 1 || got.User.AdminSources[0] != "local" || !got.User.HasPassword {
		t.Errorf("user = %+v", got.User)
	}
	if r.svc.creq != (local.CreateRequest{Username: "ann", Email: "ann@example.com", Role: "admin"}) {
		t.Errorf("request = %+v", r.svc.creq)
	}
	if r.svc.actor != (local.Actor{ID: acctAdmin}) {
		t.Errorf("actor = %+v", r.svc.actor)
	}
	// The dispatcher audits the write by op name, without arguments or secret.
	ev := r.audit.all()
	if len(ev) != 1 || ev[0].Action != ops.OpUserCreate || ev[0].Outcome != AuditOutcomeSuccess || ev[0].Actor != acctAdmin {
		t.Fatalf("audit = %+v", ev)
	}
	for _, e := range ev {
		if strings.Contains(e.Actor+e.Target+e.Action+e.Outcome, "TEMP-pass") {
			t.Error("the audit event holds the password")
		}
	}
	if strings.Contains(r.logs.String(), "TEMP-pass") {
		t.Errorf("the log holds the password: %s", r.logs)
	}
}

func TestUserCreateDefaultsToTheUserRole(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	w := r.do("POST", "/api/v1/ops/user_create", body(map[string]any{"username": "ann"}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"admin_sources":[]`) {
		t.Errorf("status = %d %s", w.Code, w.Body)
	}
	if r.svc.creq.Role != "" {
		t.Errorf("role passed = %q", r.svc.creq.Role)
	}
}

func TestUserCreateRefusesBadArgumentsBeforeTheService(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	for name, b := range map[string]string{
		"no request id": `{"username":"ann"}`,
		"no username":   body(map[string]any{}),
		"bad role":      body(map[string]any{"username": "ann", "role": "root"}),
		"bad email":     body(map[string]any{"username": "ann", "email": "x"}),
	} {
		w := r.do("POST", "/api/v1/ops/user_create", b)
		if w.Code != 400 || decodeErr(t, w).Error.Code != proto.CodeInvalidArgument {
			t.Errorf("%s = %d %s", name, w.Code, w.Body)
		}
	}
	if len(r.svc.called()) != 0 {
		t.Error("an invalid request reached the service")
	}
}

func TestAccountOperationsAreDecidedOnTheRightResource(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	for _, c := range []struct {
		path string
		body map[string]any
		res  authz.Resource
		act  string
	}{
		{"user_create", map[string]any{"username": "ann"}, authz.UserResource{ID: acctAdmin}, "manage"},
		{"user_reset_password", map[string]any{"user_id": acctTarget}, authz.UserResource{ID: acctTarget}, "manage"},
		{"user_set_role", map[string]any{"user_id": acctTarget, "role": "admin"}, authz.UserResource{ID: acctTarget}, "manage"},
		{"user_deactivate", map[string]any{"user_id": acctTarget}, authz.UserResource{ID: acctTarget}, "manage"},
		{"password_change", map[string]any{"old_password": "old-password", "new_password": "a-new-password"}, authz.UserResource{ID: acctAdmin}, "operate"},
	} {
		n := r.decide.count()
		if w := r.do("POST", "/api/v1/ops/"+c.path, body(c.body)); w.Code != 200 {
			t.Fatalf("%s = %d %s", c.path, w.Code, w.Body)
		}
		if r.decide.count() != n+1 {
			t.Fatalf("%s: authz.Decide not called exactly once", c.path)
		}
		got := r.decide.calls[n]
		if got.res != c.res || got.accepted.String() != c.act {
			t.Errorf("%s decided on %+v with %s, want %+v with %s", c.path, got.res, got.accepted, c.res, c.act)
		}
	}
	if w := r.do("GET", "/api/v1/ops/users_list", ""); w.Code != 200 {
		t.Fatalf("users_list = %d %s", w.Code, w.Body)
	}
	got := r.decide.calls[len(r.decide.calls)-1]
	if got.res != (authz.UserResource{ID: acctAdmin}) || got.accepted.String() != "manage" {
		t.Errorf("users_list decided on %+v with %s", got.res, got.accepted)
	}
}

func TestAccountAuthorisationDeniesBeforeTheHandlerRuns(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctUser))
	r.decide.fn = func(c decideCall) error {
		if c.accepted.Permits(ops.ActionManage) {
			return proto.NewError(proto.CodeForbidden, "not allowed")
		}
		return nil
	}
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"user_create", map[string]any{"username": "ann"}},
		{"user_reset_password", map[string]any{"user_id": acctTarget}},
		{"user_set_role", map[string]any{"user_id": acctUser, "role": "admin"}},
		{"user_deactivate", map[string]any{"user_id": acctTarget}},
	} {
		if w := r.do("POST", "/api/v1/ops/"+c.path, body(c.body)); w.Code != 403 {
			t.Errorf("%s as a plain user = %d %s", c.path, w.Code, w.Body)
		}
	}
	if w := r.do("GET", "/api/v1/ops/users_list", ""); w.Code != 403 {
		t.Errorf("users_list as a plain user = %d", w.Code)
	}
	if len(r.svc.called()) != 0 {
		t.Errorf("the service was reached: %v", r.svc.called())
	}
	// The refusals are audited as denied.
	for _, e := range r.audit.all() {
		if e.Outcome != AuditOutcomeDenied {
			t.Errorf("event %+v", e)
		}
	}
	// A user may change their own password.
	if w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "old-password", "new_password": "a-new-password"})); w.Code != 200 {
		t.Errorf("password_change as a plain user = %d %s", w.Code, w.Body)
	}
}

// The keep_api_keys argument reaches the service end to end: absent means the
// keys are revoked with the password, true keeps them (F-07).
func TestResetPasswordPassesKeepAPIKeysThroughTheAPI(t *testing.T) {
	for _, c := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"unchecked", map[string]any{"user_id": acctTarget}, false},
		{"explicitly false", map[string]any{"user_id": acctTarget, "keep_api_keys": false}, false},
		{"checked", map[string]any{"user_id": acctTarget, "keep_api_keys": true}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAcctRig(t, sessionIdentity(acctAdmin))
			r.svc.keep = !c.want // the fake must overwrite it
			w := r.do("POST", "/api/v1/ops/user_reset_password", body(c.args))
			if w.Code != 200 {
				t.Fatalf("reset = %d %s", w.Code, w.Body)
			}
			if r.svc.uid != acctTarget || r.svc.keep != c.want {
				t.Errorf("service got uid %q keep %v; want %q %v", r.svc.uid, r.svc.keep, acctTarget, c.want)
			}
		})
	}
}

func TestTargetedOperationsPassTheUserIDAndAudit(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	w := r.do("POST", "/api/v1/ops/user_reset_password", body(map[string]any{"user_id": acctTarget}))
	if w.Code != 200 {
		t.Fatalf("reset = %d %s", w.Code, w.Body)
	}
	var got struct {
		Outcome string `json:"outcome"`
		UserID  string `json:"user_id"`
		Pw      string `json:"temporary_password"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if r.svc.uid != acctTarget || got.Pw != "RESET-pass-0123456789abcdef" || got.UserID != acctTarget || got.Outcome != "delivered" {
		t.Errorf("reset: uid %q body %s", r.svc.uid, w.Body)
	}
	if w := r.do("POST", "/api/v1/ops/user_set_role", body(map[string]any{"user_id": acctTarget, "role": "admin"})); w.Code != 200 || r.svc.role != "admin" || r.svc.uid != acctTarget {
		t.Errorf("set_role = %d %s", w.Code, w.Body)
	}
	if w := r.do("POST", "/api/v1/ops/user_deactivate", body(map[string]any{"user_id": acctTarget})); w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"delivered"`) {
		t.Errorf("deactivate = %d %s", w.Code, w.Body)
	}
	for _, e := range r.audit.all() {
		if e.Target != acctTarget || e.Outcome != AuditOutcomeSuccess || e.Actor != acctAdmin {
			t.Errorf("audit %+v: the target is the affected account", e)
		}
	}
	if w := r.do("POST", "/api/v1/ops/user_deactivate", body(map[string]any{"user_id": "42"})); w.Code != 400 {
		t.Errorf("bad user id = %d", w.Code)
	}
}

func TestAccountServiceErrorsMapToCodes(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	cases := []struct {
		err    error
		status int
		code   proto.Code
		field  string
	}{
		{local.ErrUserNotFound, 404, proto.CodeNotFound, ""},
		{local.ErrUsernameTaken, 409, proto.CodeConflict, "username"},
		{local.ErrEmailTaken, 409, proto.CodeConflict, "email"},
		{local.ErrUserInactive, 409, proto.CodeConflict, ""},
		{local.ErrNoLocalPassword, 409, proto.CodeConflict, ""},
		{local.ErrLastAdmin, 409, proto.CodeConflict, ""},
		{local.ErrConcurrentChange, 409, proto.CodeConflict, ""},
		{local.ErrWrongPassword, 400, proto.CodeInvalidArgument, "old_password"},
		{local.ErrInvalidRole, 400, proto.CodeInvalidArgument, "role"},
		{local.ErrActAsPasswordChange, 403, proto.CodeForbidden, ""},
		{errors.New("disk /var/lib/minion.db is full"), 500, proto.CodeInternal, ""},
	}
	for _, c := range cases {
		r.svc.err = c.err
		w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "old-password", "new_password": "a-new-password"}))
		e := decodeErr(t, w)
		if w.Code != c.status || e.Error.Code != c.code {
			t.Errorf("%v -> %d %s", c.err, w.Code, w.Body)
		}
		if c.field != "" && e.Error.Details["field"] != c.field {
			t.Errorf("%v: details = %v", c.err, e.Error.Details)
		}
		if strings.Contains(w.Body.String(), "minion.db") {
			t.Errorf("an internal cause leaked: %s", w.Body)
		}
	}
	// A failed write is audited as failed.
	for _, e := range r.audit.all() {
		if e.Outcome != "failed" {
			t.Errorf("event %+v", e)
		}
	}

	r.svc.err = &local.RateLimitedError{RetryAfter: 31 * time.Second}
	w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "old-password", "new_password": "a-new-password"}))
	if w.Code != 429 || w.Header().Get("Retry-After") != "31" {
		t.Errorf("rate limited = %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestPasswordChangeIsRefusedForAnAdminActingAsTheUser(t *testing.T) {
	r := newAcctRig(t, actAsIdentity(acctAdmin, acctUser))
	w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "old-password", "new_password": "a-new-password"}))
	e := decodeErr(t, w)
	if w.Code != 403 || e.Error.Code != proto.CodeForbidden || !strings.Contains(e.Error.Message, "acting as") {
		t.Errorf("act-as password_change = %d %s", w.Code, w.Body)
	}
	if len(r.svc.called()) != 0 {
		t.Error("the service was reached")
	}
	// Resetting the password of the user one acts as is refused as well.
	w = r.do("POST", "/api/v1/ops/user_reset_password", body(map[string]any{"user_id": acctUser}))
	if w.Code != 403 || len(r.svc.called()) != 0 {
		t.Errorf("act-as reset = %d %s calls %v", w.Code, w.Body, r.svc.called())
	}
	evs := r.audit.all()
	if len(evs) != 2 || evs[0].Actor != acctAdmin || evs[0].ActingAs != acctUser || evs[0].Outcome != "failed" {
		t.Errorf("audit = %+v (the refusal names actor and acting_as)", evs)
	}
}

func TestPasswordChangeIsForBrowserSessionsOnly(t *testing.T) {
	r := newAcctRig(t, acctKeyIdentity(acctUser, ops.ActionOperate, ops.ActionRead))
	w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "old-password", "new_password": "a-new-password"}))
	if w.Code != 403 || len(r.svc.called()) != 0 {
		t.Errorf("API key password_change = %d %s", w.Code, w.Body)
	}
}

func TestPasswordChangePassesTheCallersSessionAndNeverEchoesPasswords(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctUser))
	w := r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "OLD-secret-1234", "new_password": "NEW-secret-5678"}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"outcome":"delivered"`) {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	p := r.svc.pw
	if p.UserID != acctUser || p.SessionID != "sess-1" || p.OldPassword.Reveal() != "OLD-secret-1234" || p.NewPassword.Reveal() != "NEW-secret-5678" {
		t.Errorf("request = %+v", p)
	}
	if r.svc.actor != (local.Actor{ID: acctUser}) {
		t.Errorf("actor = %+v", r.svc.actor)
	}
	for _, out := range []string{w.Body.String(), r.logs.String()} {
		if strings.Contains(out, "secret-") {
			t.Errorf("a password reached the response or the log: %s", out)
		}
	}
	// A wrong or too short new password fails validation before the service.
	r.svc.mu.Lock()
	r.svc.calls = nil
	r.svc.mu.Unlock()
	w = r.do("POST", "/api/v1/ops/password_change", body(map[string]any{"old_password": "OLD-secret-1234", "new_password": "short"}))
	if w.Code != 400 || len(r.svc.called()) != 0 {
		t.Errorf("short new password = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "short") && strings.Contains(w.Body.String(), "OLD") {
		t.Errorf("the error echoes a password: %s", w.Body)
	}
}

func TestUsersListAndKeyScope(t *testing.T) {
	rows := []store.UserAdminRow{
		{UserRow: store.UserRow{ID: acctAdmin, Username: "root", Role: "admin", Active: true, PasswordHash: "h", CreatedAt: 1_700_000_000_000}, AdminSources: []string{"local"}},
		{UserRow: store.UserRow{ID: acctUser, Username: "ann", Email: "a@example.com", Role: "user", Active: false, MustChangePassword: true, CreatedAt: 1_700_000_000_000}},
	}
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	r.svc.rows = rows
	w := r.do("GET", "/api/v1/ops/users_list", "")
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	var got ops.UsersListResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Users) != 2 {
		t.Fatalf("body = %s (%v)", w.Body, err)
	}
	if u := got.Users[0]; u.Username != "root" || u.Role != "admin" || !u.Active || !u.HasPassword || u.AdminSources[0] != "local" {
		t.Errorf("first = %+v", u)
	}
	if u := got.Users[1]; u.Active || u.HasPassword || !u.MustChangePassword || u.Email != "a@example.com" || len(u.AdminSources) != 0 {
		t.Errorf("second = %+v", u)
	}
	if strings.Contains(w.Body.String(), "password_hash") || strings.Contains(w.Body.String(), `"h"`) {
		t.Errorf("the list exposes a password hash: %s", w.Body)
	}
	var raw struct {
		Users []map[string]any `json:"users"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if ts, _ := raw.Users[0]["created_at"].(string); !strings.HasSuffix(ts, "Z") || !strings.Contains(ts, ".") {
		t.Errorf("created_at = %q, want RFC 3339 with milliseconds", ts)
	}

	// A key that does not reach the whole Hub cannot list or create users;
	// an everything scope can.
	narrow := newAcctRig(t, acctKeyIdentity(acctAdmin, ops.ActionRead))
	hub := newAcctRig(t, acctKeyIdentity(acctAdmin, ops.ActionManage, ops.ActionRead))
	hub.svc.rows = rows
	// keyIdentity grants the same actions on every resource, so the narrow
	// key only differs by not holding manage; the dispatcher's fake decider
	// allows everything here, and the handler's scope check must stop it.
	for _, c := range []struct{ method, path, b string }{
		{"GET", "/api/v1/ops/users_list", ""},
		{"POST", "/api/v1/ops/user_create", body(map[string]any{"username": "ann"})},
	} {
		if w := narrow.do(c.method, c.path, c.b); w.Code != 403 {
			t.Errorf("%s %s with a narrow key = %d %s", c.method, c.path, w.Code, w.Body)
		}
		if w := hub.do(c.method, c.path, c.b); w.Code != 200 {
			t.Errorf("%s %s with an everything key = %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if len(narrow.svc.called()) != 0 {
		t.Error("a narrow key reached the service")
	}
}

func TestWriteOpsRefuseGET(t *testing.T) {
	r := newAcctRig(t, sessionIdentity(acctAdmin))
	for _, n := range accountOps[1:] {
		if w := r.do("GET", "/api/v1/ops/"+n, ""); w.Code != 405 {
			t.Errorf("GET %s = %d, want 405 (a secret-bearing write is POST only)", n, w.Code)
		}
	}
	if len(r.svc.called()) != 0 {
		t.Error("a GET reached the service")
	}
}

// narrowScope grants the given actions on every resource except a user the
// key was never given, like a stored user:X scope.
type narrowScope []ops.Action

func (s narrowScope) Actions(r authz.ResolvedResource) []ops.Action {
	if r.UserID == acctTarget {
		return s
	}
	return nil
}

func TestTargetedAccountOperationsRefuseAKeyLimitedToSomeUsers(t *testing.T) {
	calls := []struct{ path, b string }{
		{"/api/v1/ops/user_reset_password", body(map[string]any{"user_id": acctTarget})},
		{"/api/v1/ops/user_set_role", body(map[string]any{"user_id": acctTarget, "role": "admin"})},
		{"/api/v1/ops/user_deactivate", body(map[string]any{"user_id": acctTarget})},
	}
	narrow := Identity{Principal: authz.NewAPIKeyPrincipal(acctAdmin, narrowScope{ops.ActionManage, ops.ActionRead}, ""), Actor: "key:k1", Via: ViaAPIKey}
	r := newAcctRig(t, narrow)
	for _, c := range calls {
		if w := r.do("POST", c.path, c.b); w.Code != 403 || decodeErr(t, w).Error.Code != proto.CodeForbidden {
			t.Errorf("%s with a user:X manage key = %d %s", c.path, w.Code, w.Body)
		}
	}
	if len(r.svc.called()) != 0 {
		t.Errorf("a narrow key reached the service: %v", r.svc.called())
	}
	// An everything key and a browser admin session still succeed.
	for name, id := range map[string]Identity{
		"everything key": acctKeyIdentity(acctAdmin, ops.ActionManage, ops.ActionRead),
		"admin session":  sessionIdentity(acctAdmin),
	} {
		r := newAcctRig(t, id)
		for _, c := range calls {
			if w := r.do("POST", c.path, c.b); w.Code != 200 {
				t.Errorf("%s: %s = %d %s", name, c.path, w.Code, w.Body)
			}
		}
	}
}
