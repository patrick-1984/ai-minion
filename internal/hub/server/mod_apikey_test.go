package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// startKeyHub starts a Hub with the real API-key module (Story 6.1) and the
// probe share ops of mod_api_test.go, which stand for Story 4.15's worker-scoped
// operate ops. The Worker lookup is the fake of Story 4.3.
func startKeyHub(t *testing.T) *apiHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "key-integration-pw-1")
	ah := &apiHub{workers: fakeWorkers{}}
	cat := apiTestCatalog(t)
	for _, n := range []string{ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
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
	err := r.Register("probe", apikeyModuleOrder-1, func(_ context.Context, env *Env) error {
		az, err := Require[*authz.Authorizer](env)
		if err != nil {
			return err
		}
		if err := az.SetWorkerLookup(ah.workers); err != nil {
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
	registerAPIKeyModule(r)
	ah.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	ah.hc = newHubClient(t, ah.h, "https")
	return ah
}

// worker enrolls a fake Worker owned by ownerID.
func (ah *apiHub) worker(ownerID string) string {
	id := store.NewID()
	ah.workers[id] = ownerID
	return id
}

func (ah *apiHub) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := ah.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// newKey creates a key as the session and returns its text and description.
func (ah *apiHub) newKey(t *testing.T, sess map[string]string, body map[string]any) (string, ops.APIKeyInfo) {
	t.Helper()
	r := ah.call(t, "POST", "/api/v1/ops/apikey_create", body, sess)
	if r.code != 200 {
		t.Fatalf("apikey_create = %d %s", r.code, r.body)
	}
	var res ops.APIKeyCreateResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ParseAPIKey(res.Key); err != nil {
		t.Fatalf("returned key %q: %v", res.Key, err)
	}
	return res.Key, res.Info
}

func workerGrant(id string) []map[string]any {
	return []map[string]any{{"kind": "worker", "worker_id": id}}
}

func TestKeyAuthenticatesRESTCallsWithScopeAndOwnerPermissions(t *testing.T) {
	ah := startKeyHub(t)
	alice, bob := ah.user(t, "alice"), ah.user(t, "bob")
	wAlice, wAlice2, wBob := ah.worker(alice), ah.worker(alice), ah.worker(bob)
	tok, csrf := ah.login(t, "alice")
	sess := asSession(tok, csrf)

	operateKey, _ := ah.newKey(t, sess, map[string]any{"name": "operate one", "resources": workerGrant(wAlice), "actions": []string{"operate"}})
	readKey, _ := ah.newKey(t, sess, map[string]any{"name": "read one", "resources": workerGrant(wAlice), "actions": []string{"read"}})

	share := func(key, worker string) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": worker}, asKey(key))
	}
	set := func(key, worker string) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/"+probeShareSet, map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": worker, "mode": "all"}, asKey(key))
	}
	// operate on the chosen Worker: allowed (the share_get/share_set shape, A-8).
	if r := share(operateKey, wAlice); r.code != 200 {
		t.Errorf("operate key on its Worker = %d %s", r.code, r.body)
	}
	if r := set(operateKey, wAlice); r.code != 200 {
		t.Errorf("operate key set = %d %s", r.code, r.body)
	}
	// The same key on the owner's other Worker: no scope there.
	if r := share(operateKey, wAlice2); r.code != 404 || r.err.Code != proto.CodeNotFound {
		t.Errorf("operate key on another own Worker = %d %s", r.code, r.body)
	}
	// On another user's Worker: the owner has nothing there, nor does the key.
	if r := share(operateKey, wBob); r.code != 404 {
		t.Errorf("operate key on a foreign Worker = %d %s", r.code, r.body)
	}
	// A read-only key holds no operate: forbidden, not not_found.
	if r := share(readKey, wAlice); r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("read key on operate op = %d %s", r.code, r.body)
	}
	if r := set(readKey, wAlice); r.code != 403 {
		t.Errorf("read key set = %d %s", r.code, r.body)
	}
}

func TestAdminEverythingKeyAndOwnerDemotion(t *testing.T) {
	ah := startKeyHub(t)
	bob := ah.user(t, "bob")
	wBob := ah.worker(bob)
	boss := ah.user(t, "boss")
	ah.exec(t, `INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, boss)
	tok, csrf := ah.login(t, "boss")
	key, info := ah.newKey(t, asSession(tok, csrf), map[string]any{
		"name": "root key", "resources": []map[string]any{{"kind": "everything"}}, "actions": []string{"read", "operate", "manage"},
	})
	if info.OwnerID != boss || len(info.Resources) != 1 || info.Resources[0].Kind != "everything" {
		t.Fatalf("info = %+v", info)
	}

	// The admin key reads the audit log and operates on a foreign Worker.
	if r := ah.call(t, "GET", "/api/v1/audit?limit=1", nil, asKey(key)); r.code != 200 {
		t.Fatalf("admin key audit = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": wBob}, asKey(key)); r.code != 200 {
		t.Fatalf("admin key on a foreign Worker = %d %s", r.code, r.body)
	}
	// Not even an admin key with everything can mint, list or revoke keys.
	if r := ah.call(t, "POST", "/api/v1/ops/apikey_create", map[string]any{"name": "child", "resources": []map[string]any{{"kind": "everything"}}, "actions": []string{"read"}}, asKey(key)); r.code != 403 {
		t.Errorf("admin key creating a key = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "GET", "/api/v1/ops/apikey_list?all=true", nil, asKey(key)); r.code != 403 {
		t.Errorf("admin key listing keys = %d %s", r.code, r.body)
	}
	// Demoting the owner reduces the key on the very next request.
	ah.exec(t, `DELETE FROM user_roles WHERE user_id = ?`, boss)
	if r := ah.call(t, "GET", "/api/v1/audit?limit=1", nil, asKey(key)); r.code != 403 {
		t.Errorf("demoted owner's key audit = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": wBob}, asKey(key)); r.code != 404 {
		t.Errorf("demoted owner's key on a foreign Worker = %d %s", r.code, r.body)
	}
	// Deactivating the owner ends the key altogether.
	ah.exec(t, `UPDATE users SET active = 0 WHERE id = ?`, boss)
	if r := ah.call(t, "GET", "/api/v1/audit?limit=1", nil, asKey(key)); r.code != 401 || r.err.Code != proto.CodeUnauthenticated {
		t.Errorf("deactivated owner's key = %d %s", r.code, r.body)
	}
}

func TestExpiredAndRevokedKeysAreUnauthenticated(t *testing.T) {
	ah := startKeyHub(t)
	alice := ah.user(t, "alice")
	w := ah.worker(alice)
	tok, csrf := ah.login(t, "alice")
	sess := asSession(tok, csrf)
	call := func(key string) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": w}, asKey(key))
	}

	expiring, expInfo := ah.newKey(t, sess, map[string]any{
		"name": "short", "resources": workerGrant(w), "actions": []string{"operate"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if r := call(expiring); r.code != 200 {
		t.Fatalf("live key = %d %s", r.code, r.body)
	}
	ah.exec(t, `UPDATE api_keys SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Second).UnixMilli(), expInfo.ID)
	if r := call(expiring); r.code != 401 || r.err.Code != proto.CodeUnauthenticated {
		t.Errorf("expired key = %d %s", r.code, r.body)
	}

	revoked, info := ah.newKey(t, sess, map[string]any{"name": "doomed", "resources": workerGrant(w), "actions": []string{"operate"}})
	if r := call(revoked); r.code != 200 {
		t.Fatalf("key before revoke = %d %s", r.code, r.body)
	}
	if r := ah.call(t, "POST", "/api/v1/ops/apikey_revoke", map[string]any{"request_id": ops.NewUUIDv7(), "id": info.ID}, sess); r.code != 200 {
		t.Fatalf("apikey_revoke = %d %s", r.code, r.body)
	}
	if r := call(revoked); r.code != 401 {
		t.Errorf("revoked key = %d %s", r.code, r.body)
	}
	// An unknown key is the same answer and shares the apikey_fail budget.
	if r := call(identity.NewAPIKey().String()); r.code != 401 {
		t.Errorf("unknown key = %d %s", r.code, r.body)
	}
}

func TestWorkerBoundKeyIsForbiddenOnREST(t *testing.T) {
	ah := startKeyHub(t)
	alice := ah.user(t, "alice")
	w := ah.worker(alice)
	tok, csrf := ah.login(t, "alice")
	key, info := ah.newKey(t, asSession(tok, csrf), map[string]any{
		"name": "bound", "resources": workerGrant(w), "actions": []string{"message", "read"}, "bound_worker_id": w,
	})
	if info.BoundWorkerID != w {
		t.Fatalf("info = %+v", info)
	}
	r := ah.call(t, "POST", "/api/v1/ops/"+probeShareGet, map[string]any{"worker_id": w}, asKey(key))
	if r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("bound key on REST = %d %s", r.code, r.body)
	}
	// The check for the call stream (Story 7.4) is on the same service.
	svc := serviceOf(t, ah)
	if _, err := svc.AuthenticateCall(context.Background(), key, w); err != nil {
		t.Errorf("bound key on its Worker's call stream: %v", err)
	}
	if _, err := svc.AuthenticateCall(context.Background(), key, store.NewID()); err == nil {
		t.Error("bound key accepted on another Worker's call stream")
	}
}

// serviceOf builds a service on the Hub's store the way the module does.
func serviceOf(t *testing.T, ah *apiHub) *apikey.Service {
	t.Helper()
	az := authz.New(userLookup{st: ah.st}, ah.workers)
	svc, err := apikey.New(apikey.Config{Repo: ah.st, Authz: az, Users: userLookup{st: ah.st}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestKeysCannotManageKeysAndUsersSeeOnlyTheirOwn(t *testing.T) {
	ah := startKeyHub(t)
	alice, bob := ah.user(t, "alice"), ah.user(t, "bob")
	wAlice, wBob := ah.worker(alice), ah.worker(bob)
	aTok, aCSRF := ah.login(t, "alice")
	bTok, bCSRF := ah.login(t, "bob")
	rTok, rCSRF := ah.login(t, "root")

	aliceKey, aliceInfo := ah.newKey(t, asSession(aTok, aCSRF), map[string]any{"name": "a", "resources": workerGrant(wAlice), "actions": []string{"read", "operate", "message"}})
	_, bobInfo := ah.newKey(t, asSession(bTok, bCSRF), map[string]any{"name": "b", "resources": workerGrant(wBob), "actions": []string{"read"}})

	// A key, even a full one of the owner, cannot create, list or revoke keys.
	for name, call := range map[string]func() apiResp{
		"create": func() apiResp {
			return ah.call(t, "POST", "/api/v1/ops/apikey_create", map[string]any{"name": "x", "resources": workerGrant(wAlice), "actions": []string{"read"}}, asKey(aliceKey))
		},
		"list": func() apiResp { return ah.call(t, "GET", "/api/v1/ops/apikey_list", nil, asKey(aliceKey)) },
	} {
		if r := call(); r.code != 403 && r.code != 404 {
			t.Errorf("key calling apikey_%s = %d %s", name, r.code, r.body)
		}
	}

	list := func(sess map[string]string, query string) ([]ops.APIKeyInfo, apiResp) {
		r := ah.call(t, "GET", "/api/v1/ops/apikey_list"+query, nil, sess)
		var res ops.APIKeyListResult
		if r.code == 200 {
			if err := json.Unmarshal([]byte(r.body), &res); err != nil {
				t.Fatal(err)
			}
		}
		return res.Keys, r
	}
	if keys, r := list(asSession(aTok, ""), ""); r.code != 200 || len(keys) != 1 || keys[0].ID != aliceInfo.ID {
		t.Errorf("alice's list = %d %s", r.code, r.body)
	} else if strings.Contains(r.body, aliceKey) {
		t.Error("the listing shows the key itself")
	}
	if _, r := list(asSession(aTok, ""), "?owner_id="+bob); r.code != 404 {
		t.Errorf("alice listing bob = %d %s", r.code, r.body)
	}
	if _, r := list(asSession(aTok, ""), "?all=true"); r.code != 403 {
		t.Errorf("alice listing all = %d %s", r.code, r.body)
	}
	if keys, r := list(asSession(rTok, ""), "?all=true"); r.code != 200 || len(keys) != 2 {
		t.Errorf("admin listing all = %d %s", r.code, r.body)
	}
	if keys, r := list(asSession(rTok, ""), "?owner_id="+bob); r.code != 200 || len(keys) != 1 || keys[0].ID != bobInfo.ID {
		t.Errorf("admin listing bob = %d %s", r.code, r.body)
	}
	// Alice cannot revoke bob's key; the admin can.
	rev := func(sess map[string]string, id string) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/apikey_revoke", map[string]any{"request_id": ops.NewUUIDv7(), "id": id}, sess)
	}
	if r := rev(asSession(aTok, aCSRF), bobInfo.ID); r.code != 404 {
		t.Errorf("alice revoking bob's key = %d %s", r.code, r.body)
	}
	if r := rev(asSession(rTok, rCSRF), bobInfo.ID); r.code != 200 {
		t.Errorf("admin revoking bob's key = %d %s", r.code, r.body)
	}
}

func TestCreateGrantLimitsOverREST(t *testing.T) {
	ah := startKeyHub(t)
	alice, bob := ah.user(t, "alice"), ah.user(t, "bob")
	wAlice, wBob := ah.worker(alice), ah.worker(bob)
	aTok, aCSRF := ah.login(t, "alice")
	sess := asSession(aTok, aCSRF)
	create := func(body map[string]any) apiResp {
		return ah.call(t, "POST", "/api/v1/ops/apikey_create", body, sess)
	}
	for name, body := range map[string]map[string]any{
		"everything":      {"name": "k", "resources": []map[string]any{{"kind": "everything"}}, "actions": []string{"read"}},
		"manage":          {"name": "k", "resources": workerGrant(wAlice), "actions": []string{"manage"}},
		"foreign worker":  {"name": "k", "resources": workerGrant(wBob), "actions": []string{"read"}},
		"foreign user":    {"name": "k", "resources": []map[string]any{{"kind": "user", "user_id": bob}}, "actions": []string{"read"}},
		"foreign session": {"name": "k", "resources": []map[string]any{{"kind": "session", "worker_id": wBob, "session_name": "main"}}, "actions": []string{"read"}},
	} {
		if r := create(body); r.code != 403 || r.err.Code != proto.CodeForbidden {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
	if r := create(map[string]any{"name": "k", "resources": workerGrant(wAlice), "actions": []string{"read"}, "expires_at": "2001-01-01T00:00:00.000Z"}); r.code != 400 || r.err.Details["field"] != "expires_at" {
		t.Errorf("past expiry: %d %s", r.code, r.body)
	}
	// A GET cannot mint a key.
	if r := ah.call(t, "GET", "/api/v1/ops/apikey_create?name=k&actions=read", nil, sess); r.code == 200 {
		t.Errorf("GET apikey_create = %d %s", r.code, r.body)
	}
	// No audit row names a key text, and the lifecycle is audited.
	key, info := ah.newKey(t, sess, map[string]any{"name": "ok", "resources": workerGrant(wAlice), "actions": []string{"read"}})
	events, err := ah.st.ListAudit(context.Background(), store.AuditQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	created, denied := false, 0
	for _, e := range events {
		if strings.Contains(e.Actor+e.Action+e.Target+e.Outcome, key) {
			t.Errorf("audit row holds the key: %+v", e)
		}
		if e.Action == apikey.AuditActionCreate && e.Outcome == "denied" && e.Actor == alice && e.Target == alice {
			denied++
		}
		if e.Action == apikey.AuditActionCreate && e.Target == info.ID && e.Actor == alice {
			created = true
		}
	}
	if !created {
		t.Error("no apikey.create audit row")
	}
	// Every refused grant left a denied row (AD-20); the five forbidden
	// requests above each produced one, and the past expiry is a typing error.
	if denied != 5 {
		t.Errorf("denied apikey.create rows = %d, want 5", denied)
	}
}

func TestFailedKeysAreAuditedWithoutTheTokenAndRateLimited(t *testing.T) {
	ah := startKeyHub(t)
	bad := identity.NewAPIKey().String()
	for i := 0; i < 3; i++ {
		if r := ah.call(t, "GET", "/api/v1/ops/audit_list", nil, asKey(bad)); r.code != 401 {
			t.Fatalf("attempt %d = %d %s", i, r.code, r.body)
		}
	}
	events, err := ah.st.ListAudit(context.Background(), store.AuditQuery{Action: api.AuditActionKeyAuth, Limit: 50})
	if err != nil || len(events) != 3 {
		t.Fatalf("apikey.auth rows = %d, %v", len(events), err)
	}
	for _, e := range events {
		if strings.Contains(e.Actor+e.Target+e.Outcome, bad[3:]) || e.Outcome != "failed" {
			t.Errorf("audit row = %+v", e)
		}
	}
}
