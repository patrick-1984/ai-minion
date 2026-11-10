package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestWorkersRemoveModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == "workers_remove"
	}
	if !found {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
}

// startRemoveHub boots the real module set that serves worker_remove: store,
// settings, audit, authz, auth, workers, REST framework, API keys and the
// removal module, with no stand-in for any of them.
func startRemoveHub(t *testing.T) *apiHub {
	t.Helper()
	ah, _ := startRemoveHubWithService(t)
	return ah
}

// startRemoveHubWithService is startRemoveHub that also returns the workers
// service, so that a test can connect a Worker to the real endpoint.
func startRemoveHubWithService(t *testing.T) (*apiHub, *workers.Service) {
	t.Helper()
	var svc *workers.Service
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "remove-integration-pw-1")
	ah := &apiHub{}
	if _, ok := ops.Lookup(ops.OpWorkerRemove); !ok {
		t.Fatal("no op worker_remove")
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
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerAPIKeyModule(r)
	registerWorkersRemoveModule(r)
	if err := r.Register("grab", workersRemoveModuleOrder+1, func(_ context.Context, env *Env) error {
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

func TestWorkerRemoveByTheOwnerTombstonesTheWorkerAndRevokesItsBoundKey(t *testing.T) {
	ah := startRemoveHub(t)
	ctx := context.Background()
	alice, bob := ah.user(t, "alice"), ah.user(t, "bob")

	wk, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "pi", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	if err := ah.st.InsertWorker(ctx, nil, w); err != nil {
		t.Fatal(err)
	}
	mkKey := func(workerID string) string {
		id := store.NewID()
		sum := sha256.Sum256([]byte(id))
		err := ah.st.CreateAPIKey(ctx, store.APIKeyRow{
			ID: id, OwnerID: alice, Name: "bound " + id[len(id)-4:], Hash: sum[:], Hint: id[len(id)-8:],
			Actions: []string{"operate"}, BoundWorkerID: workerID, CreatedAt: time.Now().UnixMilli(),
			Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: workerID}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	boundKey, otherKey := mkKey(w.ID), mkKey(store.NewID())

	remove := func(user string) apiResp {
		tok, csrf := ah.login(t, user)
		return ah.call(t, "POST", "/api/v1/ops/"+ops.OpWorkerRemove,
			map[string]any{"request_id": ops.NewUUIDv7(), "worker_id": w.ID}, asSession(tok, csrf))
	}
	// Another user sees nothing there: the Worker is not removed.
	if r := remove("bob"); r.code != 404 || r.err.Code != proto.CodeNotFound {
		t.Fatalf("a foreign user's removal = %d %s", r.code, r.body)
	}
	if _, err := ah.st.WorkerByID(ctx, w.ID); err != nil {
		t.Fatalf("a foreign user removed the Worker: %v", err)
	}
	_ = bob

	r := remove("alice")
	if r.code != 200 {
		t.Fatalf("worker_remove by the owner = %d %s (the operation is not wired?)", r.code, r.body)
	}
	var res ops.WriteResult
	if err := json.Unmarshal([]byte(r.body), &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeDelivered {
		t.Errorf("outcome = %q, want delivered (%s)", res.Outcome, r.body)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != ops.WarnRevokeOnNextConnect {
		t.Errorf("warnings = %+v, want revoke_on_next_connect for an offline Worker", res.Warnings)
	}

	// Tombstone, no Worker row, and the bound key (only that one) revoked.
	tomb, err := ah.st.TombstoneByPub(ctx, wk.Public())
	if err != nil || tomb.WorkerID != w.ID || tomb.RemovedBy != alice {
		t.Errorf("tombstone = %+v, %v", tomb, err)
	}
	if _, err := ah.st.WorkerByID(ctx, w.ID); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the Worker row survived: %v", err)
	}
	if k, err := ah.st.APIKeyByID(ctx, boundKey); err != nil || k.RevokedAt == 0 {
		t.Errorf("the bound key was not revoked: %+v, %v", k, err)
	}
	if k, err := ah.st.APIKeyByID(ctx, otherKey); err != nil || k.RevokedAt != 0 {
		t.Errorf("the key of another Worker was touched: %+v, %v", k, err)
	}
	page, err := ah.audit.List(ctx, audit.Filter{Action: workers.AuditActionWorkerRemove})
	if err != nil || len(page.Events) != 1 || page.Events[0].Outcome != workers.AuditOutcomeOK || page.Events[0].Target != w.ID {
		t.Errorf("audit = %+v, %v", page.Events, err)
	}
}

func TestWorkersRemoveModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerWorkersRemoveModule(r)
	if err := NewKernel(r).Start(context.Background()); err == nil {
		t.Error("the module started without its services")
	}
}

// A Worker that retires itself on the real Hub goes through the same removal
// as worker_remove: its bound API key is revoked, its trust rows are deleted,
// it is tombstoned and the ok audit record is written in the transaction (and
// no second one by the call). A key bound to another Worker and a trust row of
// another Worker stay.
func TestWorkerRetireOnTheRealHubRevokesBoundKeysAndDeletesTrust(t *testing.T) {
	ah, svc := startRemoveHubWithService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alice := ah.user(t, "alice")

	wk, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	w := store.WorkerRow{ID: store.NewID(), Pub: wk.Public(), Name: "pi", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	other := store.WorkerRow{ID: store.NewID(), Pub: mustKey(t).Public(), Name: "pi2", OwnerID: alice,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}
	for _, row := range []store.WorkerRow{w, other} {
		if err := ah.st.InsertWorker(ctx, nil, row); err != nil {
			t.Fatal(err)
		}
	}
	mkKey := func(workerID string) string {
		id := store.NewID()
		sum := sha256.Sum256([]byte(id))
		err := ah.st.CreateAPIKey(ctx, store.APIKeyRow{
			ID: id, OwnerID: alice, Name: "bound " + id[len(id)-4:], Hash: sum[:], Hint: id[len(id)-8:],
			Actions: []string{"operate"}, BoundWorkerID: workerID, CreatedAt: time.Now().UnixMilli(),
			Scopes: []store.APIKeyScopeRow{{Kind: "worker", WorkerID: workerID}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	boundKey, otherKey := mkKey(w.ID), mkKey(other.ID)
	for _, id := range []string{w.ID, other.ID} {
		if err := ah.st.PutTrust(ctx, store.TrustRow{ReceiverID: alice, WorkerID: id, CreatedAt: time.Now().UnixMilli()}); err != nil {
			t.Fatal(err)
		}
	}

	conn, err := transport.Dial(ctx, "https://"+ah.h.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key: wk, Name: "pi", Ops: []string{"send_text"}, Mode: transport.ModeConnect,
		Pin: identity.Pin{Pub: svc.Identity().Key().Public()}, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(res.Channel, proto.StreamFrameLimit); err != nil { // welcome
		t.Fatal(err)
	}
	sess, err := transport.NewSession(res.Channel, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	st, err := sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	id := ops.NewUUIDv7()
	if err := st.WriteFrame(map[string]any{
		"type": proto.TypeOpRequest, "op": ops.OpWorkerRetire, "request_id": id,
		"args": map[string]any{"request_id": id, "force": false},
	}); err != nil {
		t.Fatal(err)
	}
	body, err := st.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if typ, _ := proto.FrameType(body); typ != proto.TypeOpResult {
		t.Fatalf("worker_retire answered %s", body)
	}

	if _, err := ah.st.WorkerByID(ctx, w.ID); !errors.Is(err, store.ErrWorkerNotFound) {
		t.Errorf("the Worker row survived: %v", err)
	}
	if tomb, err := ah.st.TombstoneByPub(ctx, wk.Public()); err != nil || tomb.WorkerID != w.ID || tomb.RemovedBy != "worker:"+w.ID {
		t.Errorf("tombstone = %+v, %v", tomb, err)
	}
	if k, err := ah.st.APIKeyByID(ctx, boundKey); err != nil || k.RevokedAt == 0 {
		t.Errorf("the key bound to the retired Worker is still usable: %+v, %v", k, err)
	}
	if k, err := ah.st.APIKeyByID(ctx, otherKey); err != nil || k.RevokedAt != 0 {
		t.Errorf("the key of another Worker was touched: %+v, %v", k, err)
	}
	rows, err := ah.st.ListTrust(ctx, alice)
	if err != nil || len(rows) != 1 || rows[0].WorkerID != other.ID {
		t.Errorf("trust rows after the retirement = %+v, %v; want only the other Worker's", rows, err)
	}
	// One ok record, written by the removal itself, and none by the call.
	page, err := ah.audit.List(ctx, audit.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var removes, retires int
	for _, ev := range page.Events {
		switch {
		case ev.Action == workers.AuditActionWorkerRemove && ev.Target == w.ID && ev.Outcome == workers.AuditOutcomeOK && ev.Actor == "worker:"+w.ID:
			removes++
		case ev.Action == ops.OpWorkerRetire:
			retires++
		}
	}
	if removes != 1 || retires != 0 {
		t.Errorf("audit: %d removal records, %d retire records; want 1 and 0 (%+v)", removes, retires, page.Events)
	}

	// A repeated retirement of a removed Worker is the idempotent success.
	rm, err := workers.NewRemover(workers.RemoveConfig{Store: ah.st, Auditor: ah.audit})
	if err != nil {
		t.Fatal(err)
	}
	if err := retireRemoval(rm)(ctx, w.ID, "worker:"+w.ID, 0); err != nil {
		t.Errorf("retiring an already removed Worker = %v, want success", err)
	}
}

func mustKey(t *testing.T) *identity.Key {
	t.Helper()
	k, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
