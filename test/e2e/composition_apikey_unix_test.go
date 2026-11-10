//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Story 9.5, authenticated message-only key. The chain under test is the
// production one, with nothing standing in for a decision:
//
//	API key issued by apikey.Service and stored in the Hub database
//	  -> HTTP "Authorization: Bearer" on POST /api/v1/ops/send_text
//	  -> api.Authenticator (key verification against the store, owner state)
//	  -> api.Dispatcher + authz.Authorizer (scope, actions, resource)
//	  -> workers.Router (effective actions of the key)
//	  -> the real Hub connection and the real Worker op server
//	  -> a real tmux pane.
//
// The users and the Worker owners come from the real store; only the account
// rows are seeded. The older composition cases that hand actions to the
// Worker directly (composition_ops_unix_test.go) stay as the separate, low
// level evidence of the Worker-side guard.

// storeUsers is the store-backed user port (the Hub's own adapter is
// unexported in the server package): current state on every call, no cache.
type storeUsers struct{ st *store.Store }

func (u storeUsers) User(ctx context.Context, id string) (authz.UserState, error) {
	row, err := u.st.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return authz.UserState{}, authz.ErrNotFound
	}
	if err != nil {
		return authz.UserState{}, err
	}
	return authz.UserState{Role: authz.Role(row.Role), Active: row.Active}, nil
}

// keyAdapter is the same two-line mapping as the Hub's module: the key
// service's invalid-key error is the REST framework's.
type keyAdapter struct{ svc *apikey.Service }

func (k keyAdapter) AuthenticateKey(ctx context.Context, token string) (api.KeyIdentity, error) {
	id, err := k.svc.AuthenticateKey(ctx, token)
	if errors.Is(err, apikey.ErrInvalidKey) {
		return api.KeyIdentity{}, api.ErrInvalidKey
	}
	if err != nil {
		return api.KeyIdentity{}, err
	}
	return api.KeyIdentity{KeyID: id.KeyID, Principal: id.Principal}, nil
}

type keyAPI struct {
	url   string
	keys  *apikey.Service
	be    *cutBackend // counts the op streams the Router opens; never cuts here
	owner string
}

func startKeyAPI(t *testing.T, h *connHub) *keyAPI {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	owner := store.NewID()
	if err := h.st.CreateUser(ctx, store.CreateUserParams{
		User: store.UserRow{ID: owner, Username: "keyowner", CreatedAt: now, UpdatedAt: now}, Role: "user",
	}); err != nil {
		t.Fatal(err)
	}
	users := storeUsers{st: h.st}
	az := authz.New(users, h.service().OwnerLookup())
	be := &cutBackend{Backend: workers.RegistryBackend(h.service().Registry()), svc: h.service()}
	rt, err := workers.NewRouter(workers.RouterConfig{
		Backend: be, Directory: workers.StoreDirectory{Store: h.st}, Authz: az,
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := apikey.New(apikey.Config{Repo: h.st, Authz: az, Users: users})
	if err != nil {
		t.Fatal(err)
	}
	handlers := api.NewRegistry(ops.Default)
	pp, err := workers.NewPipe(workers.PipeConfig{
		Store: h.st.Pipe(), Authz: az, Router: rt, Trust: authz.NewTrust(api.TrustStore(h.st), nil),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.RegisterWorkerOpsWithPipe(handlers, rt, pp); err != nil {
		t.Fatal(err)
	}
	if err := api.RegisterTargetResolution(handlers, rt); err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New(ratelimit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	authn := &api.Authenticator{Sessions: session.NewManager(h.st), Limiter: limiter, Audit: audit.New(h.st)}
	if err := authn.SetKeyAuthenticator(keyAdapter{svc: keys}); err != nil {
		t.Fatal(err)
	}
	d := &api.Dispatcher{Catalog: ops.Default, Handlers: handlers, Auth: authn, Authz: az}
	mux := http.NewServeMux()
	router, err := api.NewRouter(d, mux)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router.Handler(mux))
	t.Cleanup(srv.Close)
	return &keyAPI{url: srv.URL, keys: keys, be: be, owner: owner}
}

// enrolFor enrols a Worker key owned by the real user.
func (k *keyAPI) enrolFor(t *testing.T, h *connHub, pub []byte, name string) store.WorkerRow {
	t.Helper()
	w := store.WorkerRow{
		ID: store.NewID(), Pub: pub, Name: name, OwnerID: k.owner,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}
	if err := h.st.InsertWorker(context.Background(), nil, w); err != nil {
		t.Fatal(err)
	}
	return w
}

// issue creates a key as its owner (a session principal) and returns the
// secret the Hub shows once.
func (k *keyAPI) issue(t *testing.T, name string, res []ops.KeyResource, actions ...ops.Action) string {
	t.Helper()
	out, err := k.keys.Create(context.Background(), authz.NewSessionPrincipal(k.owner), ops.APIKeyCreateArgs{
		Name: name, Resources: res, Actions: actions,
	})
	if err != nil {
		t.Fatalf("issue %s: %v", name, err)
	}
	return out.Key
}

type apiAnswer struct {
	status int
	body   []byte
}

func (a apiAnswer) code() string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.body, &env)
	return env.Error.Code
}

func (k *keyAPI) sendText(t *testing.T, bearer string, args ops.SendTextArgs) apiAnswer {
	t.Helper()
	return k.call(t, ops.OpSendText, bearer, args)
}

func (k *keyAPI) call(t *testing.T, op, bearer string, args any) apiAnswer {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, k.url+"/api/v1/ops/"+op, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return apiAnswer{status: resp.StatusCode, body: b}
}

func TestAuthenticatedKeysThroughTheHubAPIReachTheNativeShellPaneOnlyWithOperate(t *testing.T) {
	h := newConnHub(t)
	k := startKeyAPI(t, h) // creates the real owner the Workers and keys belong to
	w := startRestrictWorker(t, h)
	row := k.enrolFor(t, h, w.key.Public(), "pi")
	id := row.ID
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(id) })
	eventually(t, "the Worker to be connected", func() bool { return w.conn.Status().State == wagent.ConnConnected })
	otherKey, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	other := k.enrolFor(t, h, otherKey.Public(), "other") // enrolled, never online

	shell := w.srv.NewSession("shellwork") // the default shell
	time.Sleep(500 * time.Millisecond)
	onShell := func(text string, force bool) ops.SendTextArgs {
		return sendTextArgs(id, "shellwork", text, ops.NewUUIDv7(), force)
	}
	paneHas := func(text string) bool {
		time.Sleep(200 * time.Millisecond)
		return strings.Contains(w.srv.Capture(shell), text)
	}
	forbiddenBefore := func(name string, a apiAnswer) {
		t.Helper()
		if a.status != http.StatusForbidden || a.code() != string(proto.CodeForbidden) {
			t.Fatalf("%s: HTTP %d %s, want 403 forbidden", name, a.status, a.body)
		}
	}
	opens := func() int32 { return k.be.opens.Load() }

	worker := []ops.KeyResource{{Kind: ops.KeyResourceWorker, WorkerID: id}}

	// 1. No credential and bad credentials: refused, nothing reaches a Worker.
	unknown := identity.NewAPIKey().String() // well formed, never stored
	for name, bearer := range map[string]string{"no key": "", "malformed key": "mn_not-a-key", "unknown key": unknown} {
		a := k.sendText(t, bearer, onShell("authmark", false))
		if a.status != http.StatusUnauthorized {
			t.Fatalf("%s: HTTP %d %s, want 401", name, a.status, a.body)
		}
	}
	if n := opens(); n != 0 {
		t.Fatalf("unauthenticated calls opened %d Worker streams", n)
	}

	// 2. A key scoped to another Worker, and a read-only key: refused by the
	// Hub before any stream to the Worker is opened.
	wrongWorker := k.issue(t, "other-worker", []ops.KeyResource{{Kind: ops.KeyResourceWorker, WorkerID: other.ID}}, ops.ActionOperate, ops.ActionRead)
	// The Hub does not confirm that a Worker outside the key's scope exists.
	if a := k.sendText(t, wrongWorker, onShell("scopemark", true)); a.status != http.StatusNotFound || a.code() != string(proto.CodeNotFound) {
		t.Fatalf("wrong-Worker key: HTTP %d %s, want 404 not_found", a.status, a.body)
	}
	readOnly := k.issue(t, "read-only", worker, ops.ActionRead)
	forbiddenBefore("read-only key", k.sendText(t, readOnly, onShell("readmark", true)))
	if n := opens(); n != 0 {
		t.Fatalf("refused calls opened %d Worker streams; the Hub must refuse first", n)
	}
	if paneHas("scopemark") || paneHas("readmark") {
		t.Fatal("a refused key reached the shell")
	}

	// 3. A message-only key. send_text is an operate op, so the Hub itself
	// refuses it, before any stream. pipe_send is the op a message-only key
	// may call: it passes authentication and the Hub, the Router sends the
	// Worker the key's effective actions (message only) and the Worker denies
	// the shell pane, with or without force.
	msgOnly := k.issue(t, "message-only", worker, ops.ActionMessage)
	for _, force := range []bool{false, true} {
		forbiddenBefore("message-only key on send_text", k.sendText(t, msgOnly, onShell("denymark", force)))
	}
	if n := opens(); n != 0 {
		t.Fatalf("message-only send_text opened %d Worker streams; the Hub must refuse first", n)
	}
	for _, force := range []bool{false, true} {
		before := opens()
		a := k.call(t, ops.OpPipeSend, msgOnly, ops.PipeSendArgs{
			WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force},
			Target:    ops.Target{WorkerID: id, SessionName: "shellwork"}, Text: "denymark",
		})
		forbiddenBefore("message-only key on pipe_send", a)
		if !strings.Contains(string(a.body), "agent") {
			t.Fatalf("the denial is not the Worker's message-versus-operate guard: %s", a.body)
		}
		if opens() <= before {
			t.Fatalf("force=%v: the Worker was never asked; the denial must come from the Worker", force)
		}
	}
	if paneHas("denymark") {
		t.Fatal("a message-only key typed into a shell")
	}

	// 4. An explicit operate key: refusal of the shell without force, then
	// exactly one effect with force.
	operate := k.issue(t, "operate", worker, ops.ActionOperate, ops.ActionRead)
	a := k.sendText(t, operate, onShell("forcemark", false))
	var refused ops.WriteResult
	if a.status != http.StatusOK || json.Unmarshal(a.body, &refused) != nil ||
		refused.Outcome != ops.OutcomeFailedNotSent || refused.Reason != "shell_pane" || !refused.Forceable {
		t.Fatalf("operate key without force: HTTP %d %s", a.status, a.body)
	}
	if paneHas("forcemark") {
		t.Fatal("a refused write reached the shell")
	}
	a = k.sendText(t, operate, onShell("forcemark", true))
	var forced ops.WriteResult
	if a.status != http.StatusOK || json.Unmarshal(a.body, &forced) != nil ||
		forced.Outcome != ops.OutcomeDelivered || !forced.Forced {
		t.Fatalf("operate key with force: HTTP %d %s", a.status, a.body)
	}
	w.srv.WaitFor(shell, "forcemark", 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(w.srv.Capture(shell), "forcemark"); n != 1 {
		t.Fatalf("the pane shows the forced text %d times, want exactly one effect:\n%s", n, w.srv.Capture(shell))
	}
}
