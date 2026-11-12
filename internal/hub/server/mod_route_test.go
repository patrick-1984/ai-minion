package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestRouteModuleIsRegisteredAfterAPIAndWorkers(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("route") < 0 || idx("route") < idx("api") || idx("route") < idx("workers") {
		t.Errorf("modules = %v", names)
	}
}

func TestRouteModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerRouteModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"route"`) {
		t.Errorf("err = %v", err)
	}
}

// TestHubComposedFromTheRegistryRoutesWorkerOps composes the Hub the way the
// default registry does (core, store, settings, audit, authz, auth, workers,
// api, route) and checks that the Worker ops are answered by the router and no
// longer by the missing-handler path.
func TestHubComposedFromTheRegistryRoutesWorkerOps(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "route-integration-pw-1")
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
	registerHubIDModule(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerRouteModule(r)
	var (
		st  *store.Store
		mgr *session.Manager
	)
	if err := r.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		mgr, err = Require[*session.Manager](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: t.TempDir()}, r)
	hc := newHubClient(t, h, "https")

	admin, err := st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	is, err := mgr.Create(context.Background(), admin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin := asSession(is.Token, is.CSRFToken)

	key, _ := identity.GenerateKey()
	wid := store.NewID()
	if err := st.InsertWorker(context.Background(), nil, store.WorkerRow{
		ID: wid, Pub: key.Public(), Name: "lab", OwnerID: admin.ID, Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	code, body, _ := hc.do("GET", "/api/v1/ops/workers_list", nil, asAdmin)
	if code != 200 {
		t.Fatalf("workers_list = %d %s", code, body)
	}
	var list ops.WorkersListResult
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Workers) != 1 || list.Workers[0].ID != wid || list.Workers[0].Owner != "root" || list.Workers[0].Online {
		t.Errorf("workers = %+v", list.Workers)
	}

	// A Worker op reaches the router: the enrolled Worker is offline, which is
	// worker_offline, not unsupported_op.
	code, body, _ = hc.do("GET", "/api/v1/ops/sessions_list?worker_id="+wid, nil, asAdmin)
	var env struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	if code != proto.HTTPStatus(proto.CodeWorkerOffline) || env.Error.Code != proto.CodeWorkerOffline {
		t.Errorf("sessions_list = %d %s", code, body)
	}

	// A human-addressed request resolves through the running Hub: the target
	// "lab:minion-1" names the admin's own Worker, which is offline, so
	// the call reaches the router with the resolved id (worker_offline, not
	// invalid_argument from decoding a string as an object).
	for _, tc := range []struct {
		name, method, path, body string
		want                     proto.Code
	}{
		{"body", "POST", "/api/v1/ops/session_snapshot", `{"target":"lab:minion-1"}`, proto.CodeWorkerOffline},
		{"query", "GET", "/api/v1/ops/session_snapshot?target=lab:minion-1:0.1", "", proto.CodeWorkerOffline},
		{"worker id for the name", "POST", "/api/v1/ops/session_snapshot", `{"target":"` + wid + `:minion-1"}`, proto.CodeWorkerOffline},
		{"unknown Worker", "POST", "/api/v1/ops/session_snapshot", `{"target":"nope:minion-1"}`, proto.CodeTargetNotFound},
		{"malformed", "POST", "/api/v1/ops/session_snapshot", `{"target":"lab"}`, proto.CodeInvalidArgument},
	} {
		var payload any
		if tc.body != "" {
			payload = json.RawMessage(tc.body)
		}
		code, body, _ := hc.do(tc.method, tc.path, payload, asAdmin)
		var e struct {
			Error struct {
				Code proto.Code `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		if e.Error.Code != tc.want || code != proto.HTTPStatus(tc.want) {
			t.Errorf("%s: %d %s, want %s", tc.name, code, body, tc.want)
		}
	}
}
