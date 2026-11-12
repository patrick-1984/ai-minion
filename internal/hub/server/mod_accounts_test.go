package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type accountsHub struct {
	h     *running
	st    *store.Store
	mgr   *session.Manager
	admin *local.Admin

	hookMu sync.Mutex
	hook   store.TxHook
}

// setKeyHook sets what the probe module's RevokeKeys hook runs.
func (ah *accountsHub) setKeyHook(h store.TxHook) {
	ah.hookMu.Lock()
	defer ah.hookMu.Unlock()
	ah.hook = h
}

// startAccountsHub starts a Hub with the auth, REST and accounts modules over
// a catalog of the account operations and audit_list.
func startAccountsHub(t *testing.T) *accountsHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "root-integration-pw-1")
	cat := ops.NewRegistry()
	for _, n := range []string{ops.OpAuditList, ops.OpUsersList, ops.OpUserCreate, ops.OpUserResetPassword, ops.OpUserSetRole, ops.OpUserDeactivate, ops.OpPasswordChange, ops.OpAPIKeyCreate, ops.OpAPIKeyList, ops.OpAPIKeyRevoke} {
		op, ok := ops.Lookup(n)
		if !ok {
			t.Fatalf("no op %s", n)
		}
		cat.Register(op)
	}
	ah := &accountsHub{}
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
	registerAccounts(r, cat)
	registerAPIKeyModule(r) // authenticates the stored keys of the F-03 tests
	if err := r.Register("probe", 90, func(_ context.Context, env *Env) error {
		var err error
		az, err := Require[*authz.Authorizer](env)
		if err != nil {
			return err
		}
		if err := az.SetWorkerLookup(fakeWorkers{}); err != nil {
			return err
		}
		if ah.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if ah.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		// A later module (the API key store of Story 6.1 will be one) gets the
		// admin service from the accounts module and sets RevokeKeys during
		// init, before the Hub serves.
		if ah.admin, err = Require[*local.Admin](env); err != nil {
			return err
		}
		ah.admin.RevokeKeys = func(ctx context.Context, tx *sql.Tx, userID string) error {
			ah.hookMu.Lock()
			h := ah.hook
			ah.hookMu.Unlock()
			if h == nil {
				return nil
			}
			return h(ctx, tx, userID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ah.h = startHub(t, &Config{DataDir: hubstore.Dir(t)}, r)
	return ah
}

// actor is one signed-in client: its own cookie jar and CSRF token.
type actor struct {
	t    *testing.T
	hc   *hubClient
	csrf string
	name string
	// cookie, when set, is sent instead of the jar's (a session made
	// directly, which costs no sign-in attempt).
	cookie string
}

func (ah *accountsHub) signIn(t *testing.T, name, password string) (*actor, int, string) {
	t.Helper()
	hc := newHubClient(t, ah.h, "https")
	code, body, csrf := hc.login(name, password)
	return &actor{t: t, hc: hc, csrf: csrf, name: name}, code, body
}

func (ah *accountsHub) mustSignIn(t *testing.T, name, password string) *actor {
	t.Helper()
	a, code, body := ah.signIn(t, name, password)
	if code != 200 {
		t.Fatalf("sign in %s = %d %s", name, code, body)
	}
	return a
}

// extraSession starts one more session of the user without signing in (the
// login limiter counts sign-ins per account).
func (ah *accountsHub) extraSession(t *testing.T, name string) *actor {
	t.Helper()
	row, err := ah.st.UserByIdentifier(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	is, err := ah.mgr.Create(context.Background(), row, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &actor{t: t, hc: newHubClient(t, ah.h, "https"), csrf: is.CSRFToken, name: name, cookie: is.Token}
}

type result struct {
	code int
	body string
	err  proto.Code
	det  map[string]any
}

// op posts a REST operation. NOTE: it injects a fresh request_id into every
// write, which the real browser form does not (see the Story 3.7
// change-password form, forms.js): this helper is not the web contract, and a
// test that must pin the form's own body shape has to post that body itself.
func (a *actor) op(name string, args map[string]any) result {
	a.t.Helper()
	method := "POST"
	if args == nil {
		method = "GET"
	} else if _, ok := args["request_id"]; !ok && name != ops.OpUsersList && name != ops.OpAuditList {
		args["request_id"] = ops.NewUUIDv7()
	}
	h := map[string]string{"X-Minion-CSRF": a.csrf}
	if a.cookie != "" {
		h["Cookie"] = session.CookieName + "=" + a.cookie
	}
	code, body, _ := a.hc.do(method, "/api/v1/ops/"+name, args, h)
	res := result{code: code, body: body}
	if code >= 400 {
		var e struct {
			Error struct {
				Code    proto.Code     `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			a.t.Fatalf("%s: not an error envelope: %s", name, body)
		}
		res.err, res.det = e.Error.Code, e.Error.Details
	}
	return res
}

func (r result) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("body %s: %v", r.body, err)
	}
}

type createdBody struct {
	Outcome           string       `json:"outcome"`
	TemporaryPassword string       `json:"temporary_password"`
	User              ops.UserInfo `json:"user"`
}

func (ah *accountsHub) listUsers(t *testing.T, admin *actor) map[string]ops.UserInfo {
	t.Helper()
	r := admin.op(ops.OpUsersList, nil)
	if r.code != 200 {
		t.Fatalf("users_list = %d %s", r.code, r.body)
	}
	var out ops.UsersListResult
	r.json(t, &out)
	m := map[string]ops.UserInfo{}
	for _, u := range out.Users {
		m[u.Username] = u
	}
	return m
}

func TestAccountAdministrationOverREST(t *testing.T) {
	ah := startAccountsHub(t)
	admin := ah.mustSignIn(t, "root", "root-integration-pw-1")

	// user_create: a temporary password, shown once, flagged must-change.
	r := admin.op(ops.OpUserCreate, map[string]any{"username": "ann", "email": "ann@example.com"})
	if r.code != 200 {
		t.Fatalf("user_create = %d %s", r.code, r.body)
	}
	var created createdBody
	r.json(t, &created)
	temp := created.TemporaryPassword
	if created.Outcome != "delivered" || len(temp) < 20 || created.User.Role != "user" || !created.User.MustChangePassword || !created.User.Active {
		t.Fatalf("created = %+v", created)
	}
	users := ah.listUsers(t, admin)
	if ann := users["ann"]; ann.ID != created.User.ID || !ann.MustChangePassword || ann.Role != "user" || !ann.HasPassword {
		t.Errorf("listed ann = %+v", ann)
	}
	if users["root"].Role != "admin" || len(users["root"].AdminSources) != 1 || users["root"].AdminSources[0] != "local" {
		t.Errorf("listed root = %+v", users["root"])
	}
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "ANN"}); r.code != 409 || r.err != proto.CodeConflict || r.det["field"] != "username" {
		t.Errorf("duplicate user name = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "ann2", "email": "ANN@example.com"}); r.code != 409 || r.det["field"] != "email" {
		t.Errorf("duplicate e-mail = %d %s", r.code, r.body)
	}
	// A user name equal to another account's e-mail would capture that
	// account's sign-in identifier; so would an e-mail equal to a user name.
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "Ann@Example.com"}); r.code != 409 || r.err != proto.CodeConflict || r.det["field"] != "username" {
		t.Errorf("user name equal to an e-mail = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "dan@example.org"}); r.code != 200 {
		t.Fatalf("user name that looks like an e-mail = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "dan2", "email": "DAN@example.org"}); r.code != 409 || r.err != proto.CodeConflict || r.det["field"] != "email" {
		t.Errorf("e-mail equal to a user name = %d %s", r.code, r.body)
	}

	// The user signs in with the temporary password (must-change reported); a
	// second session tells the session that changes the password from the others.
	annA, code, body := ah.signIn(t, "ann", temp)
	if code != 200 || !strings.Contains(body, `"must_change_password":true`) {
		t.Fatalf("ann signs in = %d %s", code, body)
	}
	annB := ah.extraSession(t, "ann")

	// A plain user cannot administer accounts.
	for name, args := range map[string]map[string]any{
		ops.OpUserCreate:        {"username": "mallory"},
		ops.OpUserResetPassword: {"user_id": created.User.ID},
		ops.OpUserSetRole:       {"user_id": created.User.ID, "role": "admin"},
		ops.OpUserDeactivate:    {"user_id": created.User.ID},
	} {
		if r := annA.op(name, args); r.code != 403 || r.err != proto.CodeForbidden {
			t.Errorf("%s as a plain user = %d %s", name, r.code, r.body)
		}
	}
	if r := annA.op(ops.OpUsersList, nil); r.code != 403 {
		t.Errorf("users_list as a plain user = %d", r.code)
	}
	// Another user's account does not exist for them.
	if r := annA.op(ops.OpUserDeactivate, map[string]any{"user_id": users["root"].ID}); r.code != 404 || r.err != proto.CodeNotFound {
		t.Errorf("deactivate another user as a plain user = %d %s", r.code, r.body)
	}

	// password_change: the old password is verified.
	const mine = "ann's own new password"
	if r := annA.op(ops.OpPasswordChange, map[string]any{"old_password": "not-the-password", "new_password": mine}); r.code != 400 || r.det["field"] != "old_password" {
		t.Errorf("wrong old password = %d %s", r.code, r.body)
	}
	if r := annA.op(ops.OpPasswordChange, map[string]any{"old_password": temp, "new_password": "short"}); r.code != 400 || r.det["field"] != "new_password" {
		t.Errorf("short new password = %d %s", r.code, r.body)
	}
	if r := annA.op(ops.OpPasswordChange, map[string]any{"old_password": temp, "new_password": mine}); r.code != 200 {
		t.Fatalf("password_change = %d %s", r.code, r.body)
	}
	// All other sessions of ann are revoked; the one that changed it is not.
	if r := annB.op(ops.OpUsersList, nil); r.code != 401 {
		t.Errorf("another session after the change = %d %s (want 401)", r.code, r.body)
	}
	if r := annA.op(ops.OpUsersList, nil); r.code != 403 {
		t.Errorf("the changing session after the change = %d %s (want a valid session)", r.code, r.body)
	}
	if _, code, _ := ah.signIn(t, "ann", temp); code != 401 {
		t.Errorf("the temporary password still works: %d", code)
	}
	if _, code, body := ah.signIn(t, "ann", mine); code != 200 || !strings.Contains(body, `"must_change_password":false`) {
		t.Errorf("new password sign in = %d %s", code, body)
	}

	// user_reset_password: a new temporary password, all sessions revoked.
	r = admin.op(ops.OpUserResetPassword, map[string]any{"user_id": created.User.ID})
	if r.code != 200 {
		t.Fatalf("user_reset_password = %d %s", r.code, r.body)
	}
	var reset struct {
		TemporaryPassword string `json:"temporary_password"`
	}
	r.json(t, &reset)
	if reset.TemporaryPassword == "" || reset.TemporaryPassword == temp {
		t.Errorf("reset password = %q", reset.TemporaryPassword)
	}
	if r := annA.op(ops.OpUsersList, nil); r.code != 401 {
		t.Errorf("session after a reset = %d, want 401", r.code)
	}
	annC, code, body := ah.signIn(t, "ann", reset.TemporaryPassword)
	if code != 200 || !strings.Contains(body, `"must_change_password":true`) {
		t.Fatalf("sign in with the reset password = %d %s", code, body)
	}

	// user_set_role.
	if r := admin.op(ops.OpUserSetRole, map[string]any{"user_id": created.User.ID, "role": "admin"}); r.code != 200 {
		t.Fatalf("promote = %d %s", r.code, r.body)
	}
	if r := annC.op(ops.OpUsersList, nil); r.code != 200 {
		t.Errorf("a promoted user cannot list users: %d %s", r.code, r.body)
	}
	if got := ah.listUsers(t, admin)["ann"]; got.Role != "admin" {
		t.Errorf("ann after promotion = %+v", got)
	}
	if r := admin.op(ops.OpUserSetRole, map[string]any{"user_id": created.User.ID, "role": "user"}); r.code != 200 {
		t.Fatalf("demote = %d %s", r.code, r.body)
	}
	if r := annC.op(ops.OpUsersList, nil); r.code != 403 {
		t.Errorf("a demoted user can still list users: %d", r.code)
	}
	if r := admin.op(ops.OpUserSetRole, map[string]any{"user_id": created.User.ID, "role": "root"}); r.code != 400 {
		t.Errorf("unknown role = %d", r.code)
	}

	// user_deactivate: no more sign in, sessions end, Workers stay.
	workerID := store.NewID()
	if err := ah.st.InsertWorker(context.Background(), nil, store.WorkerRow{
		ID: workerID, Pub: []byte(strings.Repeat("k", 32)), Name: "laptop", OwnerID: created.User.ID, Kind: store.WorkerKindPrivate, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if r := admin.op(ops.OpUserDeactivate, map[string]any{"user_id": created.User.ID}); r.code != 200 {
		t.Fatalf("deactivate = %d %s", r.code, r.body)
	}
	if r := annC.op(ops.OpUsersList, nil); r.code != 401 {
		t.Errorf("session of a deactivated user = %d, want 401", r.code)
	}
	if _, code, _ := ah.signIn(t, "ann", reset.TemporaryPassword); code != 401 {
		t.Errorf("a deactivated user signed in: %d", code)
	}
	if got := ah.listUsers(t, admin)["ann"]; got.Active {
		t.Errorf("ann after deactivation = %+v", got)
	}
	if owner, err := ah.st.WorkerOwner(context.Background(), workerID); err != nil || owner != created.User.ID {
		t.Errorf("Worker owner after deactivation = %q, %v (Workers stay with the account)", owner, err)
	}
	if r := admin.op(ops.OpUserResetPassword, map[string]any{"user_id": created.User.ID}); r.code != 409 {
		t.Errorf("reset of a deactivated account = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserDeactivate, map[string]any{"user_id": ah.listUsers(t, admin)["root"].ID}); r.code != 409 || !strings.Contains(r.body, "last active admin") {
		t.Errorf("deactivating the last admin = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserDeactivate, map[string]any{"user_id": store.NewID()}); r.code != 404 {
		t.Errorf("unknown user = %d", r.code)
	}

	// Every write is in the audit log, by operation and by effect, without a
	// password anywhere in it, in the response log or the Hub log.
	ar := admin.op(ops.OpAuditList, map[string]any{"limit": 200})
	if ar.code != 200 {
		t.Fatalf("audit_list = %d %s", ar.code, ar.body)
	}
	for _, action := range []string{"user_create", "user_reset_password", "user_set_role", "user_deactivate", "password_change",
		"user.create", "user.reset_password", "user.set_role", "user.deactivate", "user.password_change"} {
		if !strings.Contains(ar.body, `"action":"`+action+`"`) {
			t.Errorf("audit log has no %s event", action)
		}
	}
	for _, secret := range []string{temp, mine, reset.TemporaryPassword, "root-integration-pw-1"} {
		if strings.Contains(ar.body, secret) || strings.Contains(ah.h.logs.String(), secret) {
			t.Errorf("a password reached the audit log or the Hub log")
		}
	}
}

// The accounts module provides the *local.Admin, so the API key store (a later
// module) can set RevokeKeys without editing this module. The hook runs in the
// transaction of user_deactivate over REST; a failing hook rolls the
// deactivation back.
func TestDeactivateRunsTheKeyRevocationHookInItsTransaction(t *testing.T) {
	ah := startAccountsHub(t)
	if ah.admin == nil || ah.admin.RevokeKeys == nil {
		t.Fatal("the probe module did not get the admin service and set RevokeKeys")
	}
	admin := ah.mustSignIn(t, "root", "root-integration-pw-1")
	var created createdBody
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "zed"}); r.code != 200 {
		t.Fatalf("user_create = %d %s", r.code, r.body)
	} else {
		r.json(t, &created)
	}

	// A failing hook rolls the deactivation back.
	boom := errors.New("key store is down")
	var hookUser string
	ah.setKeyHook(func(_ context.Context, _ *sql.Tx, userID string) error {
		hookUser = userID
		return boom
	})
	if r := admin.op(ops.OpUserDeactivate, map[string]any{"user_id": created.User.ID}); r.code != 500 {
		t.Errorf("user_deactivate with a failing hook = %d %s", r.code, r.body)
	}
	if hookUser != created.User.ID {
		t.Errorf("hook user = %q", hookUser)
	}
	if !ah.listUsers(t, admin)["zed"].Active {
		t.Error("the deactivation was not rolled back")
	}

	// A working hook runs inside the transaction: its write commits with it.
	ah.setKeyHook(func(ctx context.Context, tx *sql.Tx, userID string) error {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT active FROM users WHERE id = ?`, userID).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return errors.New("the hook ran before the deactivation, outside its transaction")
		}
		return nil
	})
	if r := admin.op(ops.OpUserDeactivate, map[string]any{"user_id": created.User.ID}); r.code != 200 {
		t.Fatalf("user_deactivate = %d %s", r.code, r.body)
	}
	if ah.listUsers(t, admin)["zed"].Active {
		t.Error("the account is still active")
	}
}

// A write that carries a temporary password is POST only: a GET would skip
// the CSRF check and could be caused by another site.
func TestAccountWritesAreNotAvailableOverGET(t *testing.T) {
	ah := startAccountsHub(t)
	admin := ah.mustSignIn(t, "root", "root-integration-pw-1")
	for _, n := range []string{ops.OpUserCreate, ops.OpUserResetPassword, ops.OpPasswordChange} {
		code, _, _ := admin.hc.do("GET", "/api/v1/ops/"+n+"?username=x", nil, nil)
		if code != 405 {
			t.Errorf("GET %s = %d, want 405", n, code)
		}
	}
	// Without the CSRF header a POST is refused too.
	code, _, _ := admin.hc.do("POST", "/api/v1/ops/"+ops.OpUserCreate, map[string]any{"username": "x", "request_id": ops.NewUUIDv7()}, nil)
	if code != 403 {
		t.Errorf("POST without CSRF = %d, want 403", code)
	}
}

func TestAccountsModuleRegistersHandlersOnTheAPIRegistry(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("accounts") < 0 || idx("accounts") < idx("api") {
		t.Errorf("modules = %v", names)
	}
}

func TestAccountsModuleNeedsTheRESTFramework(t *testing.T) {
	r := NewRegistry()
	registerAccounts(r, ops.Default)
	if err := NewKernel(r).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "accounts") {
		t.Errorf("err = %v", err)
	}
	// A catalog without the account operations needs nothing.
	r = NewRegistry()
	registerAccounts(r, ops.NewRegistry())
	k := NewKernel(r)
	if err := k.Start(context.Background()); err != nil {
		t.Errorf("empty catalog: %v", err)
	}
}

// insertKey stores a key row directly, like data written before the grant
// rules tightened, and returns its text.
func (ah *accountsHub) insertKey(t *testing.T, owner string, actions []string, scope store.APIKeyScopeRow) string {
	t.Helper()
	key := identity.NewAPIKey()
	hash := key.Hash()
	row := store.APIKeyRow{
		ID: store.NewID(), OwnerID: owner, Name: "stored", Hash: hash[:], Hint: key.Hint(),
		Actions: actions, CreatedAt: store.UnixMS(time.Now()), Scopes: []store.APIKeyScopeRow{scope},
	}
	if err := ah.st.CreateAPIKey(context.Background(), row, nil); err != nil {
		t.Fatal(err)
	}
	return key.String()
}

func (ah *accountsHub) keyOp(t *testing.T, key, name string, args map[string]any) result {
	t.Helper()
	args["request_id"] = ops.NewUUIDv7()
	code, body, _ := newHubClient(t, ah.h, "https").do("POST", "/api/v1/ops/"+name, args, asKey(key))
	res := result{code: code, body: body}
	if code >= 400 {
		var e struct {
			Error struct {
				Code proto.Code `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		res.err = e.Error.Code
	}
	return res
}

func TestStoredNarrowManageKeyCannotAdministerAccounts(t *testing.T) {
	ah := startAccountsHub(t)
	admin := ah.mustSignIn(t, "root", "root-integration-pw-1")
	var created createdBody
	if r := admin.op(ops.OpUserCreate, map[string]any{"username": "ann"}); r.code != 200 {
		t.Fatalf("user_create = %d %s", r.code, r.body)
	} else {
		r.json(t, &created)
	}
	annID := created.User.ID
	root := ah.listUsers(t, admin)["root"].ID

	// A pre-upgrade row: manage on the single user ann.
	narrow := ah.insertKey(t, root, []string{"manage", "read"}, store.APIKeyScopeRow{Kind: "user", UserID: annID})
	for name, args := range map[string]map[string]any{
		ops.OpUserSetRole:       {"user_id": annID, "role": "admin"},
		ops.OpUserResetPassword: {"user_id": annID},
		ops.OpUserDeactivate:    {"user_id": annID},
	} {
		if r := ah.keyOp(t, narrow, name, args); r.code != 403 || r.err != proto.CodeForbidden {
			t.Errorf("%s with a stored user:X manage key = %d %s", name, r.code, r.body)
		}
	}
	ann := ah.listUsers(t, admin)["ann"]
	if ann.Role != "user" || !ann.Active || !ann.MustChangePassword {
		t.Errorf("ann changed through a narrow key: %+v", ann)
	}
	if _, code, _ := ah.signIn(t, "ann", created.TemporaryPassword); code != 200 {
		t.Errorf("ann's password was reset through a narrow key: %d", code)
	}

	// A narrow key without manage is refused as before.
	readKey := ah.insertKey(t, root, []string{"read"}, store.APIKeyScopeRow{Kind: "user", UserID: annID})
	if r := ah.keyOp(t, readKey, ops.OpUserSetRole, map[string]any{"user_id": annID, "role": "admin"}); r.code != 403 {
		t.Errorf("read-only user:X key on user_set_role = %d %s", r.code, r.body)
	}

	// An everything key and the browser admin session still work.
	all := ah.insertKey(t, root, []string{"manage", "read"}, store.APIKeyScopeRow{Kind: "everything"})
	if r := ah.keyOp(t, all, ops.OpUserSetRole, map[string]any{"user_id": annID, "role": "admin"}); r.code != 200 {
		t.Errorf("everything key user_set_role = %d %s", r.code, r.body)
	}
	if r := ah.keyOp(t, all, ops.OpUserResetPassword, map[string]any{"user_id": annID}); r.code != 200 {
		t.Errorf("everything key user_reset_password = %d %s", r.code, r.body)
	}
	if r := admin.op(ops.OpUserSetRole, map[string]any{"user_id": annID, "role": "user"}); r.code != 200 {
		t.Errorf("admin session user_set_role = %d %s", r.code, r.body)
	}
	if r := ah.keyOp(t, all, ops.OpUserDeactivate, map[string]any{"user_id": annID}); r.code != 200 {
		t.Errorf("everything key user_deactivate = %d %s", r.code, r.body)
	}
}
