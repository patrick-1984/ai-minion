package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The worker-scoped operate ops have the shape of share_get and share_set
// (A-8): accepted action operate on the Worker named by worker_id. Story 4.15
// declares the real ones; the test declares its own in a private catalog.
type probeShareArgs struct {
	WorkerID string `json:"worker_id"`
}

type probeShareResult struct {
	Mode string `json:"mode"`
}

type probeShareSetArgs struct {
	ops.WriteArgs
	WorkerID string `json:"worker_id"`
	Mode     string `json:"mode"`
}

const (
	probeShareGet = "probe_share_get"
	probeShareSet = "probe_share_set"
)

func apiTestCatalog(t *testing.T) *ops.Registry {
	t.Helper()
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpAuditList, ops.OpSessionsList} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	operate := ops.MustActionSet(ops.ActionOperate)
	cat.Register(ops.Op{Name: probeShareGet, Summary: "Read the share rule.", Scope: ops.ScopeWorker, Actions: operate,
		Args: reflect.TypeFor[probeShareArgs](), Result: reflect.TypeFor[probeShareResult]()})
	cat.Register(ops.Op{Name: probeShareSet, Summary: "Set the share rule.", Scope: ops.ScopeWorker, Actions: operate, Write: true,
		Guards: []ops.Guard{{Name: "probe_guard", Class: ops.GuardKeep, Reads: "probe.state"}},
		Args:   reflect.TypeFor[probeShareSetArgs](), Result: reflect.TypeFor[ops.WriteResult]()})
	return cat
}

// fakeWorkers is the authz WorkerLookup that Story 4.3 provides for real.
type fakeWorkers map[string]string

func (f fakeWorkers) WorkerOwner(_ context.Context, id string) (string, error) {
	if o, ok := f[id]; ok {
		return o, nil
	}
	return "", authz.ErrNotFound
}

// keyScope grants actions per Worker, like the stored scope of Story 6.1.
type keyScope map[string][]ops.Action

func (s keyScope) Actions(res authz.ResolvedResource) []ops.Action { return s[res.WorkerID] }

// fakeKeys is the api.KeyAuthenticator that Story 6.1 provides for real.
type fakeKeys struct {
	mu    sync.Mutex
	known map[string]api.KeyIdentity
}

func (f *fakeKeys) AuthenticateKey(_ context.Context, token string) (api.KeyIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ki, ok := f.known[token]; ok {
		return ki, nil
	}
	return api.KeyIdentity{}, api.ErrInvalidKey
}

type apiHub struct {
	h       *running
	hc      *hubClient
	st      *store.Store
	audit   *audit.Log
	workers fakeWorkers
	keys    *fakeKeys
	mgr     *session.Manager
	authn   *api.Authenticator
}

func startAPIHub(t *testing.T) *apiHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "api-integration-pw-1")
	ah := &apiHub{workers: fakeWorkers{}, keys: &fakeKeys{known: map[string]api.KeyIdentity{}}}
	cat := apiTestCatalog(t)
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
	registerAPI(r, cat)
	// The modules of later stories, in miniature: the Worker lookup (4.3), the
	// key authenticator (6.1) and the share handlers (4.15).
	err := r.Register("probe", 60, func(_ context.Context, env *Env) error {
		var err error
		az, err := Require[*authz.Authorizer](env)
		if err != nil {
			return err
		}
		if err := az.SetWorkerLookup(ah.workers); err != nil {
			return err
		}
		if ah.authn, err = Require[*api.Authenticator](env); err != nil {
			return err
		}
		if err := ah.authn.SetKeyAuthenticator(ah.keys); err != nil {
			return err
		}
		if ah.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ah.audit, err = Require[*audit.Log](env); err != nil {
			return err
		}
		if ah.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		reg, err := Require[*api.Registry](env)
		if err != nil {
			return err
		}
		if err := api.HandleTyped(reg, probeShareGet, func(context.Context, *api.Call, probeShareArgs) (probeShareResult, error) {
			return probeShareResult{Mode: "select"}, nil
		}); err != nil {
			return err
		}
		return api.HandleTyped(reg, probeShareSet, func(context.Context, *api.Call, probeShareSetArgs) (ops.WriteResult, error) {
			return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	ah.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	ah.hc = newHubClient(t, ah.h, "https")
	return ah
}

// user creates a plain user and returns its id.
func (ah *apiHub) user(t *testing.T, name string) string {
	t.Helper()
	id := store.NewID()
	if _, err := ah.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, name); err != nil {
		t.Fatal(err)
	}
	return id
}

// login starts a browser session of the user and returns its cookie value and
// CSRF token.
func (ah *apiHub) login(t *testing.T, ident string) (token, csrf string) {
	t.Helper()
	row, err := ah.st.UserByIdentifier(context.Background(), ident)
	if err != nil {
		t.Fatal(err)
	}
	is, err := ah.mgr.Create(context.Background(), row, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return is.Token, is.CSRFToken
}

type apiResp struct {
	code int
	body string
	err  struct {
		Code    proto.Code     `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
}

func (ah *apiHub) call(t *testing.T, method, path string, body any, headers map[string]string) apiResp {
	t.Helper()
	code, text, resp := ah.hc.do(method, path, body, headers)
	out := apiResp{code: code, body: text}
	if code >= 400 {
		var env struct {
			Error *struct {
				Code    proto.Code     `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &env); err != nil || env.Error == nil || env.Error.Details == nil {
			t.Fatalf("%s %s = %d without an AD-18 envelope: %s", method, path, code, text)
		}
		out.err.Code, out.err.Message, out.err.Details = env.Error.Code, env.Error.Message, env.Error.Details
		if code != proto.HTTPStatus(env.Error.Code) {
			t.Errorf("%s %s: status %d does not match code %s", method, path, code, env.Error.Code)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("error Content-Type = %q", ct)
		}
	}
	return out
}

func asSession(token, csrf string) map[string]string {
	return map[string]string{"Cookie": session.CookieName + "=" + token, session.CSRFHeader: csrf}
}

func asKey(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

var rfc3339ms = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestAPIAuditListIsForAdminsOnly(t *testing.T) {
	ah := startAPIHub(t)
	alice := ah.user(t, "alice")
	_ = alice
	adminTok, adminCSRF := ah.login(t, "root")
	aliceTok, aliceCSRF := ah.login(t, "alice")

	// No credentials: unauthenticated, in the envelope.
	if r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, nil); r.code != 401 || r.err.Code != proto.CodeUnauthenticated {
		t.Errorf("anonymous = %d %s", r.code, r.body)
	}
	// A user is authenticated but not allowed: forbidden (a user holds read
	// on their own user resource, so the answer is not "not found").
	if r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, asSession(aliceTok, aliceCSRF)); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("user = %d %s", r.code, r.body)
	}
	// The admin reads the log, with RFC 3339 ms timestamps; unknown request
	// fields are ignored.
	r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{"limit": 5, "added_in_a_later_minor": true}, asSession(adminTok, adminCSRF))
	if r.code != 200 {
		t.Fatalf("admin = %d %s", r.code, r.body)
	}
	var list ops.AuditListResult
	var raw struct{ Events []map[string]any }
	if err := json.Unmarshal([]byte(r.body), &list); err != nil || len(list.Events) == 0 {
		t.Fatalf("result = %s (%v)", r.body, err)
	}
	_ = json.Unmarshal([]byte(r.body), &raw)
	for _, e := range raw.Events {
		if ts, _ := e["ts"].(string); !rfc3339ms.MatchString(ts) {
			t.Errorf("ts = %q, want RFC 3339 UTC with milliseconds", ts)
		}
	}
	// The same through the resource alias and GET with query arguments.
	r = ah.call(t, "GET", "/api/v1/audit?limit=1&action=user.seed", nil, asSession(adminTok, ""))
	if r.code != 200 || !strings.Contains(r.body, `"user.seed"`) {
		t.Errorf("alias = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "GET", "/api/v1/audit", nil, asSession(aliceTok, "")); r.code != 403 {
		t.Errorf("alias as user = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "GET", "/api/v1/audit?limit=abc", nil, asSession(adminTok, "")); r.code != 400 || r.err.Details["field"] != "limit" {
		t.Errorf("bad query = %d %s", r.code, r.body)
	}
}

func TestAPICSRFAndSessions(t *testing.T) {
	ah := startAPIHub(t)
	tok, csrf := ah.login(t, "root")
	if r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, map[string]string{"Cookie": session.CookieName + "=" + tok}); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("POST without CSRF = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, asSession(tok, "forged")); r.code != 403 {
		t.Errorf("POST with a forged CSRF token = %d", r.code)
	}
	if r := ah.call(t, "POST", "/api/v1/ops/audit_list", map[string]any{}, asSession(tok, csrf)); r.code != 200 {
		t.Errorf("POST with CSRF = %d %s", r.code, r.body)
	}
	// GET needs no CSRF header.
	if r := ah.call(t, "GET", "/api/v1/ops/audit_list", nil, map[string]string{"Cookie": session.CookieName + "=" + tok}); r.code != 200 {
		t.Errorf("GET = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "GET", "/api/v1/ops/audit_list", nil, map[string]string{"Cookie": session.CookieName + "=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}); r.code != 401 {
		t.Errorf("unknown session = %d", r.code)
	}
}

func TestAPIUnsupportedOpAndUnknownRoutes(t *testing.T) {
	ah := startAPIHub(t)
	tok, csrf := ah.login(t, "root")
	// sessions_list is in the catalog but nothing registered a handler.
	if r := ah.call(t, "POST", "/api/v1/ops/sessions_list", map[string]any{}, asSession(tok, csrf)); r.code != 501 || r.err.Code != proto.CodeUnsupportedOp {
		t.Errorf("op without handler = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "GET", "/api/v1/nope", nil, asSession(tok, "")); r.code != 404 || r.err.Code != proto.CodeNotFound {
		t.Errorf("unknown route = %d %s", r.code, r.body)
	}
	// audit_list is a read op; settings_set-like writes would refuse GET. The
	// probe write op shows the 405 envelope.
	if r := ah.call(t, "GET", "/api/v1/ops/"+probeShareSet, nil, asSession(tok, "")); r.code != 405 || r.err.Code != proto.CodeMethodNotAllowed {
		t.Errorf("GET on a write = %d %s", r.code, r.body)
	}
	// /healthz stays outside the API.
	if code, _, _ := ah.hc.do("GET", "/healthz", nil, nil); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
}

// The worker-scoped operate op of the A-8 shape: owner allowed, foreign owner
// denied, admin allowed, API key without operate scope on that Worker denied.
func TestAPIWorkerScopedOperateOps(t *testing.T) {
	ah := startAPIHub(t)
	aliceID, bobID := ah.user(t, "alice"), ah.user(t, "bob")
	wAlice, wBob := store.NewID(), store.NewID()
	ah.workers[wAlice], ah.workers[wBob] = aliceID, bobID

	adminTok, adminCSRF := ah.login(t, "root")
	aliceTok, aliceCSRF := ah.login(t, "alice")
	bobTok, bobCSRF := ah.login(t, "bob")

	get := func(headers map[string]string, worker string) apiResp {
		return ah.call(t, "GET", "/api/v1/ops/"+probeShareGet+"?worker_id="+worker, nil, headers)
	}
	set := func(headers map[string]string, worker string) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/"+probeShareSet, map[string]any{
			"request_id": ops.NewUUIDv7(), "worker_id": worker, "mode": "all",
		}, headers)
	}

	// Owner and admin: allowed.
	for name, h := range map[string]map[string]string{"owner": asSession(aliceTok, aliceCSRF), "admin": asSession(adminTok, adminCSRF)} {
		if r := get(h, wAlice); r.code != 200 || r.body != `{"mode":"select"}`+"\n" {
			t.Errorf("%s get = %d %s", name, r.code, r.body)
		}
		if r := set(h, wAlice); r.code != 200 || !strings.Contains(r.body, `"outcome":"delivered"`) {
			t.Errorf("%s set = %d %s", name, r.code, r.body)
		}
	}
	// A foreign owner is denied and learns nothing about the Worker.
	for _, r := range []apiResp{get(asSession(bobTok, bobCSRF), wAlice), set(asSession(bobTok, bobCSRF), wAlice)} {
		if r.code != 404 || r.err.Code != proto.CodeNotFound {
			t.Errorf("foreign owner = %d %s", r.code, r.body)
		}
	}
	// An unknown Worker answers the same as a foreign one.
	if r := get(asSession(bobTok, bobCSRF), store.NewID()); r.code != 404 {
		t.Errorf("unknown worker = %d %s", r.code, r.body)
	}
	if r := get(asSession(aliceTok, aliceCSRF), ""); r.code != 400 || r.err.Details["field"] != "worker_id" {
		t.Errorf("missing worker_id = %d %s", r.code, r.body)
	}

	// API keys: scope intersected with the owner's permission.
	ah.keys.mu.Lock()
	ah.keys.known["mn_alice_read_only"] = api.KeyIdentity{KeyID: "k-read", Principal: authz.NewAPIKeyPrincipal(aliceID, keyScope{wAlice: {ops.ActionRead}}, "")}
	ah.keys.known["mn_alice_operate"] = api.KeyIdentity{KeyID: "k-op", Principal: authz.NewAPIKeyPrincipal(aliceID, keyScope{wAlice: {ops.ActionRead, ops.ActionOperate}}, "")}
	ah.keys.known["mn_alice_other_worker"] = api.KeyIdentity{KeyID: "k-other", Principal: authz.NewAPIKeyPrincipal(aliceID, keyScope{wBob: {ops.ActionOperate}}, "")}
	ah.keys.known["mn_alice_bound"] = api.KeyIdentity{KeyID: "k-bound", Principal: authz.NewAPIKeyPrincipal(aliceID, keyScope{wAlice: {ops.ActionOperate}}, wAlice)}
	ah.keys.mu.Unlock()

	if r := get(asKey("mn_alice_read_only"), wAlice); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("key without operate scope: get = %d %s", r.code, r.body)
	}
	if r := set(asKey("mn_alice_read_only"), wAlice); r.code != 403 {
		t.Errorf("key without operate scope: set = %d %s", r.code, r.body)
	}
	if r := get(asKey("mn_alice_operate"), wAlice); r.code != 200 {
		t.Errorf("key with operate scope: get = %d %s", r.code, r.body)
	}
	if r := set(asKey("mn_alice_operate"), wAlice); r.code != 200 {
		t.Errorf("key with operate scope: set = %d %s", r.code, r.body)
	}
	// A key scoped to another Worker has nothing on this one.
	if r := get(asKey("mn_alice_other_worker"), wAlice); r.code != 404 {
		t.Errorf("key scoped to another worker = %d %s", r.code, r.body)
	}
	// Even with the owner's scope it cannot reach a Worker of another owner.
	if r := get(asKey("mn_alice_other_worker"), wBob); r.code != 404 {
		t.Errorf("key beyond the owner's permissions = %d %s", r.code, r.body)
	}
	// A key bound to a Worker identity works on call streams only (AD-11).
	if r := get(asKey("mn_alice_bound"), wAlice); r.code != 403 {
		t.Errorf("worker-bound key on REST = %d %s", r.code, r.body)
	}

	// Every write above was audited without content: the admin's and owner's
	// successes, the denials and the key calls.
	page, err := ah.audit.List(context.Background(), audit.Filter{Action: probeShareSet, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	actors := map[string]bool{}
	for _, e := range page.Events {
		counts[e.Outcome]++
		actors[e.Actor] = true
		if e.Target != wAlice && e.Target != wBob {
			t.Errorf("audit target = %q", e.Target)
		}
	}
	if counts["success"] != 3 || counts["denied"] != 2 || !actors[aliceID] || !actors["key:k-op"] || !actors["key:k-read"] {
		t.Errorf("audit outcomes = %v actors = %v", counts, actors)
	}
	for _, e := range page.Events {
		if strings.Contains(e.Actor+e.Action+e.Target+e.Outcome, "mn_alice") {
			t.Errorf("a key token is in the audit log: %+v", e)
		}
	}
}

func TestAPIDeactivatedOrDemotedUsersLoseAccessImmediately(t *testing.T) {
	ah := startAPIHub(t)
	aliceID := ah.user(t, "alice")
	w := store.NewID()
	ah.workers[w] = aliceID
	ah.keys.mu.Lock()
	ah.keys.known["mn_alice_operate"] = api.KeyIdentity{KeyID: "k", Principal: authz.NewAPIKeyPrincipal(aliceID, keyScope{w: {ops.ActionOperate}}, "")}
	ah.keys.mu.Unlock()
	if r := ah.call(t, "GET", "/api/v1/ops/"+probeShareGet+"?worker_id="+w, nil, asKey("mn_alice_operate")); r.code != 200 {
		t.Fatalf("before = %d %s", r.code, r.body)
	}
	// Deactivating the owner revokes the key's permission on the next request:
	// authz recomputes from SQLite on every call.
	if _, err := ah.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, aliceID); err != nil {
		t.Fatal(err)
	}
	if r := ah.call(t, "GET", "/api/v1/ops/"+probeShareGet+"?worker_id="+w, nil, asKey("mn_alice_operate")); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("after deactivation = %d %s", r.code, r.body)
	}
}

func TestAPIFailedBearersAreLimitedAndAuditedWithoutTheToken(t *testing.T) {
	ah := startAPIHub(t)
	const secret = "mn_thisisasecretkeythatdoesnotexist0123456789abcdefgh"
	burst := 10 // ratelimit.DefaultPolicies()[apikey_fail].IP.Burst
	var limited int
	for i := 0; i < burst+3; i++ {
		r := ah.call(t, "GET", "/api/v1/ops/audit_list", nil, asKey(secret))
		switch r.err.Code {
		case proto.CodeUnauthenticated:
		case proto.CodeRateLimited:
			limited++
		default:
			t.Fatalf("attempt %d = %d %s", i, r.code, r.body)
		}
	}
	if limited == 0 {
		t.Error("the apikey_fail limiter never applied")
	}
	_, _, resp := ah.hc.do("GET", "/api/v1/ops/audit_list", nil, asKey(secret))
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("limited response = %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	page, err := ah.audit.List(context.Background(), audit.Filter{Action: api.AuditActionKeyAuth, Limit: 50})
	if err != nil || len(page.Events) == 0 {
		t.Fatalf("audit = %+v, %v", page.Events, err)
	}
	for _, e := range page.Events {
		if e.Actor != "anonymous" || e.Outcome != api.AuditOutcomeFailed && e.Outcome != api.AuditOutcomeLimited || strings.Contains(e.Target+e.Action, "thisisasecret") {
			t.Errorf("event = %+v", e)
		}
	}
	if strings.Contains(ah.h.logs.String(), "thisisasecret") {
		t.Errorf("the token reached the log:\n%s", ah.h.logs)
	}
}

func TestAPIModuleWithoutKeyModuleRejectsEveryBearer(t *testing.T) {
	t.Setenv("MINION_ADMIN_PASSWORD", "api-integration-pw-2")
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r), registerStore(r),
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
	registerAPI(r, apiTestCatalog(t))
	h := startHub(t, &Config{DataDir: t.TempDir()}, r)
	hc := newHubClient(t, h, "https")
	code, body, _ := hc.do("GET", "/api/v1/ops/audit_list", nil, map[string]string{"Authorization": "Bearer mn_whatever"})
	if code != http.StatusUnauthorized || !strings.Contains(body, `"unauthenticated"`) {
		t.Errorf("bearer without a key module = %d %s", code, body)
	}
}

func TestAPIModuleNeedsItsServices(t *testing.T) {
	r := NewRegistry()
	registerAPI(r, ops.NewRegistry())
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"api"`) {
		t.Errorf("err = %v", err)
	}
}

func TestAPIModuleIsRegisteredAfterAuth(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("api") < 0 || idx("api") < idx("auth") || idx("api") < idx("audit") || idx("api") < idx("authz") {
		t.Errorf("modules = %v", names)
	}
}

// usersScope grants actions per user, like the "chosen users" scope of Story
// 6.1; everythingScope grants every action everywhere.
type usersScope map[string][]ops.Action

func (s usersScope) Actions(res authz.ResolvedResource) []ops.Action { return s[res.UserID] }

type everythingScope struct{}

func (everythingScope) Actions(authz.ResolvedResource) []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}
}

// audit_list returns the events of every user, so an admin's key limited to
// chosen resources must not read it, even when it grants manage on the
// admin's own user resource.
func TestAPIAuditListNeedsAnEverythingScopeForKeys(t *testing.T) {
	ah := startAPIHub(t)
	ah.user(t, "alice")
	admin, err := ah.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	ah.keys.mu.Lock()
	ah.keys.known["mn_admin_self_only"] = api.KeyIdentity{KeyID: "k-self", Principal: authz.NewAPIKeyPrincipal(admin.ID,
		usersScope{admin.ID: {ops.ActionRead, ops.ActionManage}}, "")}
	ah.keys.known["mn_admin_one_worker"] = api.KeyIdentity{KeyID: "k-worker", Principal: authz.NewAPIKeyPrincipal(admin.ID,
		keyScope{store.NewID(): {ops.ActionRead, ops.ActionManage}}, "")}
	ah.keys.known["mn_admin_everything"] = api.KeyIdentity{KeyID: "k-all", Principal: authz.NewAPIKeyPrincipal(admin.ID, everythingScope{}, "")}
	ah.keys.mu.Unlock()

	// The self-only key passes the dispatcher's decision on the admin's own
	// user resource and is stopped by the handler; the Worker key has no
	// scope on that resource at all, so the decision hides it.
	for key, want := range map[string]proto.Code{"mn_admin_self_only": proto.CodeForbidden, "mn_admin_one_worker": proto.CodeNotFound} {
		for _, req := range [][2]string{{"POST", "/api/v1/ops/audit_list"}, {"GET", "/api/v1/audit"}} {
			if r := ah.call(t, req[0], req[1], nil, asKey(key)); r.code != proto.HTTPStatus(want) || r.err.Code != want {
				t.Errorf("%s %s with %s = %d %s, want %s", req[0], req[1], key, r.code, r.body, want)
			}
		}
	}
	for _, req := range [][2]string{{"POST", "/api/v1/ops/audit_list"}, {"GET", "/api/v1/audit"}} {
		if r := ah.call(t, req[0], req[1], nil, asKey("mn_admin_everything")); r.code != 200 || !strings.Contains(r.body, `"events"`) {
			t.Errorf("%s %s with an everything scope = %d %s", req[0], req[1], r.code, r.body)
		}
	}
}

// Modules wrap the root handler through Root: the wrapper sees the requests a
// ServeMux answers itself (redirects), and Wrap is refused once the Hub runs.
func TestRootWrapsEveryRequestIncludingMuxRedirects(t *testing.T) {
	r := NewRegistry()
	if err := registerCore(r); err != nil {
		t.Fatal(err)
	}
	var root *Root
	err := r.Register("wrapper", 10, func(_ context.Context, env *Env) error {
		var err error
		if root, err = Require[*Root](env); err != nil {
			return err
		}
		return root.Wrap(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("X-Wrapped", "yes")
				next.ServeHTTP(w, req)
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: t.TempDir(), TLSOff: true}, r)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get("http://" + h.srv.Addr() + "/some//where")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("X-Wrapped") != "yes" {
		t.Errorf("redirect = %d, X-Wrapped = %q; the wrapper must see mux redirects", resp.StatusCode, resp.Header.Get("X-Wrapped"))
	}
	if err := root.Wrap(func(next http.Handler) http.Handler { return next }); err == nil {
		t.Error("Wrap succeeded after the Hub started")
	}
}
