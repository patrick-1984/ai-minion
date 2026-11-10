package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// startIdentityHub starts a Hub with the real workers, API-key and Hub identity
// modules behind the REST dispatcher.
func startIdentityHub(t *testing.T) (*apiHub, *workers.Service) {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "identity-integration-pw-1")
	ah := &apiHub{}
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpHubIdentityRotate, ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	var svc *workers.Service
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
	registerWorkersModule(r)
	registerAPI(r, cat)
	registerAPIKeyModule(r)
	registerHubIdentityModule(r)
	if err := r.Register("grab", hubIdentityModuleOrder+1, func(_ context.Context, env *Env) error {
		var err error
		if ah.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ah.audit, err = Require[*audit.Log](env); err != nil {
			return err
		}
		if ah.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		svc, err = Require[*workers.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ah.h = startHub(t, &Config{DataDir: t.TempDir()}, r)
	ah.hc = newHubClient(t, ah.h, "https")
	return ah, svc
}

func rotateBody() map[string]any { return map[string]any{"request_id": ops.NewUUIDv7()} }

func TestHubIdentityModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "hub_identity"
	}
	if !found {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
}

func TestHubIdentityModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerHubIdentityModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"hub_identity"`) {
		t.Errorf("err = %v", err)
	}
}

func TestAnAdminRotatesTheHubIdentityThroughTheREST(t *testing.T) {
	ah, svc := startIdentityHub(t)
	before := svc.Identity().Fingerprint()
	tok, csrf := ah.login(t, "root")

	r := ah.call(t, "POST", "/api/v1/ops/hub_identity_rotate", rotateBody(), asSession(tok, csrf))
	if r.code != 200 {
		t.Fatalf("hub_identity_rotate = %d %s", r.code, r.body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %s (%v)", r.body, err)
	}
	if after := svc.Identity().Fingerprint(); after == before {
		t.Error("the fingerprint did not change")
	}
	chain, err := ah.st.Transitions(context.Background())
	if err != nil || len(chain) != 1 || chain[0].Seq != 1 {
		t.Fatalf("chain = %v, %v", chain, err)
	}
	admin, err := ah.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ah.st.ListAudit(context.Background(), store.AuditQuery{Action: workers.RotateAction, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Actor != admin.ID || rows[0].Outcome != "success" {
		t.Fatalf("audit rows = %+v, %v", rows, err)
	}
}

func TestANonAdminCannotRotateTheHubIdentity(t *testing.T) {
	ah, svc := startIdentityHub(t)
	ah.user(t, "alice")
	before := svc.Identity().Fingerprint()
	tok, csrf := ah.login(t, "alice")
	r := ah.call(t, "POST", "/api/v1/ops/hub_identity_rotate", rotateBody(), asSession(tok, csrf))
	if r.code != 403 || r.err.Code != proto.CodeForbidden {
		t.Errorf("user = %d %s", r.code, r.body)
	}
	if svc.Identity().Fingerprint() != before {
		t.Error("a refused call rotated the identity")
	}
	if chain, _ := ah.st.Transitions(context.Background()); len(chain) != 0 {
		t.Errorf("chain = %v", chain)
	}
}

func TestOnlyAnEverythingKeyWithManageRotatesTheHubIdentity(t *testing.T) {
	ah, svc := startIdentityHub(t)
	admin, err := ah.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	tok, csrf := ah.login(t, "root")
	sess := asSession(tok, csrf)
	every := []map[string]any{{"kind": "everything"}}
	readKey, _ := ah.newKey(t, sess, map[string]any{"name": "read", "resources": every, "actions": []string{"read"}})
	userKey, _ := ah.newKey(t, sess, map[string]any{
		"name": "own user only", "resources": []map[string]any{{"kind": "user", "user_id": admin.ID}}, "actions": []string{"manage"},
	})
	manageKey, _ := ah.newKey(t, sess, map[string]any{"name": "manage", "resources": every, "actions": []string{"manage"}})
	before := svc.Identity().Fingerprint()

	for name, key := range map[string]string{"read-only key": readKey, "manage key limited to the admin user": userKey} {
		r := ah.call(t, "POST", "/api/v1/ops/hub_identity_rotate", rotateBody(), asKey(key))
		if r.code != 403 || r.err.Code != proto.CodeForbidden {
			t.Errorf("%s = %d %s", name, r.code, r.body)
		}
	}
	if svc.Identity().Fingerprint() != before {
		t.Fatal("a refused key rotated the identity")
	}
	if r := ah.call(t, "POST", "/api/v1/ops/hub_identity_rotate", rotateBody(), asKey(manageKey)); r.code != 200 {
		t.Fatalf("manage key = %d %s", r.code, r.body)
	}
	if svc.Identity().Fingerprint() == before {
		t.Error("the manage key did not rotate the identity")
	}
}

func TestAFullChainIsAConflictThatForceDoesNotOverride(t *testing.T) {
	ah, svc := startIdentityHub(t)
	for i := 1; i <= workers.MaxChainLinks; i++ {
		oldPub, newPub, sig := make([]byte, 32), make([]byte, 32), make([]byte, 64)
		for _, b := range [][]byte{oldPub, newPub, sig} {
			if _, err := rand.Read(b); err != nil {
				t.Fatal(err)
			}
		}
		ah.exec(t, `INSERT INTO hub_key_transitions (seq, old_pub, new_pub, created_at, sig_by_old) VALUES (?, ?, ?, 1, ?)`, i, oldPub, newPub, sig)
	}
	before := svc.Identity().Fingerprint()
	tok, csrf := ah.login(t, "root")
	for _, force := range []bool{false, true} {
		body := rotateBody()
		body["force"] = force
		r := ah.call(t, "POST", "/api/v1/ops/hub_identity_rotate", body, asSession(tok, csrf))
		if r.code != 409 || r.err.Code != proto.CodeConflict {
			t.Errorf("force=%v: %d %s", force, r.code, r.body)
		}
	}
	if svc.Identity().Fingerprint() != before {
		t.Error("a refused rotation changed the identity")
	}
}
