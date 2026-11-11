package apikey

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// sqlUsers is the user port over the real users table, like hub/server's.
type sqlUsers struct{ st *store.Store }

func (u sqlUsers) User(ctx context.Context, id string) (authz.UserState, error) {
	row, err := u.st.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return authz.UserState{}, authz.ErrNotFound
	}
	if err != nil {
		return authz.UserState{}, err
	}
	return authz.UserState{Role: authz.Role(row.Role), Active: row.Active}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t       *testing.T
	st      *store.Store
	svc     *Service
	az      *authz.Authorizer
	clock   *clock
	workers fixedWorkers
	admin   string
	alice   string // plain user, owns wAlice
	bob     string // plain user, owns wBob
	wAlice  string
	wBob    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, workers: fixedWorkers{}, clock: &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}}
	admin := store.NewID()
	if _, err := st.SeedAdmin(context.Background(), store.SeedAdminParams{
		User:   store.UserRow{ID: admin, Username: "root", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1},
		Marker: "admin_seed",
	}); err != nil {
		t.Fatal(err)
	}
	h.admin, h.alice, h.bob = admin, h.addUser("alice"), h.addUser("bob")
	h.wAlice, h.wBob = store.NewID(), store.NewID()
	h.workers[h.wAlice], h.workers[h.wBob] = h.alice, h.bob
	h.az = authz.New(sqlUsers{st}, h.workers, authz.WithClock(h.clock.now))
	h.svc, err = New(Config{Repo: st, Authz: h.az, Users: sqlUsers{st}, Now: h.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) addUser(name string) string {
	h.t.Helper()
	id := store.NewID()
	if _, err := h.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, id, name); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.st.DB().Exec(q, args...); err != nil {
		h.t.Fatal(err)
	}
}

func session(userID string) authz.Principal { return authz.NewSessionPrincipal(userID) }

func mustCreate(t *testing.T, h *harness, p authz.Principal, args ops.APIKeyCreateArgs) ops.APIKeyCreateResult {
	t.Helper()
	res, err := h.svc.Create(issuerCtx(), p, args)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return res
}

func args(res []ops.KeyResource, actions ...ops.Action) ops.APIKeyCreateArgs {
	return ops.APIKeyCreateArgs{Name: "test key", Resources: res, Actions: actions}
}

func workerRef(id string) ops.KeyResource {
	return ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: id}
}
func userRef(id string) ops.KeyResource {
	return ops.KeyResource{Kind: ops.KeyResourceUser, UserID: id}
}
func everything() ops.KeyResource { return ops.KeyResource{Kind: ops.KeyResourceEverything} }
func sessionRef(w, n string) ops.KeyResource {
	return ops.KeyResource{Kind: ops.KeyResourceSession, WorkerID: w, SessionName: n}
}

func wantCode(t *testing.T, err error, code proto.Code) {
	t.Helper()
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestNewNeedsItsPorts(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New accepted an empty config")
	}
}

func TestCreateReturnsTheKeyOnceAndStoresOnlyHashAndHint(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead, ops.ActionOperate))

	key, err := identity.ParseAPIKey(res.Key)
	if err != nil {
		t.Fatalf("the returned key is not a valid mn_ key: %v", err)
	}
	if !strings.HasPrefix(res.Key, "mn_") || len(res.Key) != 3+52 {
		t.Errorf("key = %q, want mn_ and 52 base32 characters", res.Key)
	}
	if res.Info.Hint != key.Hint() || len(res.Info.Hint) != 8 || res.Info.OwnerID != h.alice {
		t.Errorf("info = %+v", res.Info)
	}
	row, err := h.st.APIKeyByID(ctx, res.Info.ID)
	if err != nil {
		t.Fatal(err)
	}
	sum := key.Hash()
	if string(row.Hash) != string(sum[:]) || row.Hint != key.Hint() {
		t.Errorf("stored hash/hint = %x / %q", row.Hash, row.Hint)
	}
	// The key text is nowhere in the stored row, the info or the audit rows.
	if strings.Contains(res.Info.Name+res.Info.Hint+row.Hint, res.Key) {
		t.Error("the key text leaked into the description")
	}
	events, err := h.st.ListAudit(ctx, AuditQueryAll)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Actor+e.Action+e.Target+e.Outcome, res.Key) {
			t.Errorf("audit row holds the key: %+v", e)
		}
		if e.Action == AuditActionCreate && e.Target == res.Info.ID && e.Actor == h.alice && e.Outcome == "success" {
			found = true
		}
	}
	if !found {
		t.Errorf("no apikey.create audit row for the key: %+v", events)
	}
	// Listing never shows the key again.
	list, err := h.svc.List(ctx, session(h.alice), ops.APIKeyListArgs{})
	if err != nil || len(list.Keys) != 1 || list.Keys[0].Hint != key.Hint() {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// AuditQueryAll reads every audit row of a test.
var AuditQueryAll = store.AuditQuery{Limit: 500}

func TestCreateStoresTheGrantsWithTheKey(t *testing.T) {
	h := newHarness(t)
	exp := ops.Timestamp(h.clock.now().Add(48 * time.Hour))
	a := args([]ops.KeyResource{workerRef(h.wAlice), sessionRef(h.wAlice, "main"), workerRef(h.wAlice), userRef(h.alice)},
		ops.ActionMessage, ops.ActionRead, ops.ActionRead)
	a.ExpiresAt, a.BoundWorkerID = &exp, h.wAlice
	res := mustCreate(t, h, session(h.alice), a)

	row, err := h.st.APIKeyByID(context.Background(), res.Info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Scopes) != 3 {
		t.Errorf("scope rows = %+v, want 3 (the duplicate collapses)", row.Scopes)
	}
	if strings.Join(row.Actions, ",") != "message,read" {
		t.Errorf("actions = %v", row.Actions)
	}
	if row.BoundWorkerID != h.wAlice || row.ExpiresAt != exp.Time().UnixMilli() {
		t.Errorf("row = %+v", row)
	}
	if res.Info.ExpiresAt == nil || !res.Info.ExpiresAt.Time().Equal(exp.Time().Truncate(time.Millisecond)) {
		t.Errorf("info expiry = %v", res.Info.ExpiresAt)
	}
}

func TestCreateGrantRules(t *testing.T) {
	h := newHarness(t)
	sAlice, sBob, sAdmin := session(h.alice), session(h.bob), session(h.admin)
	tests := []struct {
		name string
		p    authz.Principal
		a    ops.APIKeyCreateArgs
		want proto.Code // empty: allowed
	}{
		{"user: own user, all own actions", sAlice, args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead, ops.ActionOperate, ops.ActionMessage), ""},
		{"user: own worker", sAlice, args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), ""},
		{"user: own session", sAlice, args([]ops.KeyResource{sessionRef(h.wAlice, "main")}, ops.ActionMessage), ""},
		{"user: everything is admin only", sAlice, args([]ops.KeyResource{everything()}, ops.ActionRead), proto.CodeForbidden},
		{"user: manage is admin only", sAlice, args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionManage), proto.CodeForbidden},
		{"user: manage on own user is still admin only", sAlice, args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead, ops.ActionManage), proto.CodeForbidden},
		{"user: another user", sAlice, args([]ops.KeyResource{userRef(h.bob)}, ops.ActionRead), proto.CodeForbidden},
		{"user: another user's worker", sAlice, args([]ops.KeyResource{workerRef(h.wBob)}, ops.ActionRead), proto.CodeForbidden},
		{"user: another user's session", sAlice, args([]ops.KeyResource{sessionRef(h.wBob, "main")}, ops.ActionRead), proto.CodeForbidden},
		{"user: one foreign resource among own ones", sAlice, args([]ops.KeyResource{workerRef(h.wAlice), workerRef(h.wBob)}, ops.ActionRead), proto.CodeForbidden},
		{"user: a worker that does not exist (no existence leak)", sAlice, args([]ops.KeyResource{workerRef(store.NewID())}, ops.ActionRead), proto.CodeForbidden},
		{"user: a user that does not exist", sAlice, args([]ops.KeyResource{userRef(store.NewID())}, ops.ActionRead), proto.CodeForbidden},
		{"user: bound to another user's worker", sAlice, withBound(args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), h.wBob), proto.CodeForbidden},
		{"user: bound to an unknown worker", sAlice, withBound(args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), store.NewID()), proto.CodeForbidden},
		{"user: bound to own worker", sAlice, withBound(args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionMessage), h.wAlice), ""},
		{"other user: the same worker is not theirs", sBob, args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), proto.CodeForbidden},
		{"admin: everything", sAdmin, args([]ops.KeyResource{everything()}, ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage), ""},
		{"admin: chosen users", sAdmin, args([]ops.KeyResource{userRef(h.alice), userRef(h.bob)}, ops.ActionRead), ""},
		{"admin: a foreign worker", sAdmin, args([]ops.KeyResource{workerRef(h.wBob)}, ops.ActionOperate), ""},
		{"admin: manage on a chosen user is refused (F-03)", sAdmin, args([]ops.KeyResource{userRef(h.admin)}, ops.ActionManage), proto.CodeForbidden},
		{"admin: manage on a chosen user among others", sAdmin, args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead, ops.ActionManage), proto.CodeForbidden},
		{"admin: manage on a worker is refused", sAdmin, args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionManage), proto.CodeForbidden},
		{"admin: manage on a session is refused", sAdmin, args([]ops.KeyResource{sessionRef(h.wAlice, "main")}, ops.ActionManage), proto.CodeForbidden},
		{"admin: a narrow key without manage is unaffected", sAdmin, args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead, ops.ActionOperate, ops.ActionMessage), ""},
		{"admin: a worker that does not exist", sAdmin, args([]ops.KeyResource{workerRef(store.NewID())}, ops.ActionRead), proto.CodeForbidden},
		{"admin: a user that does not exist", sAdmin, args([]ops.KeyResource{userRef(store.NewID())}, ops.ActionRead), proto.CodeForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.Create(issuerCtx(), tc.p, tc.a)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			wantCode(t, err, tc.want)
		})
	}
	// Nothing was stored for the refused ones: only the allowed rows exist.
	allowed := 0
	for _, tc := range tests {
		if tc.want == "" {
			allowed++
		}
	}
	all, err := h.st.ListAPIKeys(context.Background(), "")
	if err != nil || len(all) != allowed {
		t.Errorf("stored keys = %d, want %d (%v)", len(all), allowed, err)
	}
}

func withBound(a ops.APIKeyCreateArgs, w string) ops.APIKeyCreateArgs {
	a.BoundWorkerID = w
	return a
}

func TestCreateRefusesEverythingForADemotedOrDeactivatedAdmin(t *testing.T) {
	h := newHarness(t)
	h.exec(`UPDATE users SET active = 0 WHERE id = ?`, h.admin)
	_, err := h.svc.Create(issuerCtx(), session(h.admin), args([]ops.KeyResource{everything()}, ops.ActionRead))
	wantCode(t, err, proto.CodeForbidden)

	h.exec(`UPDATE users SET active = 1 WHERE id = ?`, h.admin)
	h.exec(`DELETE FROM user_roles WHERE user_id = ?`, h.admin)
	_, err = h.svc.Create(issuerCtx(), session(h.admin), args([]ops.KeyResource{everything()}, ops.ActionRead))
	wantCode(t, err, proto.CodeForbidden)
}

// createRows returns the apikey.create audit rows of the harness store.
func createRows(t *testing.T, h *harness) []store.AuditRow {
	t.Helper()
	events, err := h.st.ListAudit(context.Background(), store.AuditQuery{Action: AuditActionCreate, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestRefusedCreationsAreAuditedAsDenied(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	scope := NewScope([]Grant{{Kind: ops.KeyResourceEverything}}, allFour)
	tests := []struct {
		name              string
		p                 authz.Principal
		a                 ops.APIKeyCreateArgs
		wantActor, wantAs string
		wantTarget        string
	}{
		{"user asks for everything", session(h.alice), args([]ops.KeyResource{everything()}, ops.ActionRead), h.alice, "", h.alice},
		{"user asks for manage", session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionManage), h.alice, "", h.alice},
		{"user asks for a foreign worker", session(h.alice), args([]ops.KeyResource{workerRef(h.wBob)}, ops.ActionRead), h.alice, "", h.alice},
		{"api key principal", authz.NewAPIKeyPrincipal(h.admin, scope, ""), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), h.admin, "", h.admin},
		{"act-as principal", authz.NewActAsPrincipal(h.admin, h.alice, h.clock.now().Add(time.Hour)), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead), h.admin, h.alice, h.alice},
	}
	for i, tc := range tests {
		_, err := h.svc.Create(ctx, tc.p, tc.a)
		wantCode(t, err, proto.CodeForbidden)
		rows := createRows(t, h)
		if len(rows) != i+1 {
			t.Fatalf("%s: %d apikey.create rows, want %d", tc.name, len(rows), i+1)
		}
		var found bool
		for _, r := range rows {
			if r.Outcome == "denied" && r.Actor == tc.wantActor && r.ActingAs == tc.wantAs && r.Target == tc.wantTarget {
				found = true
			}
			if strings.Contains(r.Actor+r.Action+r.Target+r.Outcome, "mn_") {
				t.Errorf("%s: audit row mentions a key: %+v", tc.name, r)
			}
		}
		if !found {
			t.Errorf("%s: no matching denied row in %+v", tc.name, rows)
		}
	}
	// Nothing is audited for a typing error.
	bad := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	bad.Name = ""
	_, _ = h.svc.Create(ctx, session(h.alice), bad)
	if n := len(createRows(t, h)); n != len(tests) {
		t.Errorf("rows after an invalid request = %d, want %d", n, len(tests))
	}
}

type failingCreateRepo struct{ Repository }

func (failingCreateRepo) CreateAPIKey(context.Context, store.APIKeyRow, *store.AuditRow) error {
	return sql.ErrConnDone
}

func TestAFailedCreationIsAuditedAsFailed(t *testing.T) {
	h := newHarness(t)
	svc, err := New(Config{Repo: failingCreateRepo{h.st}, Authz: h.az, Users: sqlUsers{h.st}, Now: h.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(issuerCtx(), session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	if err == nil {
		t.Fatal("Create succeeded")
	}
	rows := createRows(t, h)
	if len(rows) != 1 || rows[0].Outcome != "failed" || rows[0].Actor != h.alice || rows[0].Target != h.alice {
		t.Errorf("rows = %+v, want one failed row", rows)
	}
}

func TestCreateValidatesAndRefusesPastExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	past := ops.Timestamp(h.clock.now().Add(-time.Second))
	now := ops.Timestamp(h.clock.now())
	for name, ts := range map[string]ops.Timestamp{"past": past, "now": now} {
		a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
		a.ExpiresAt = &ts
		_, err := h.svc.Create(ctx, session(h.alice), a)
		var ae *ops.ArgumentError
		if !errors.As(err, &ae) || ae.Field != "expires_at" {
			t.Errorf("%s expiry: err = %v", name, err)
		}
	}
	bad := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	bad.Name = ""
	if _, err := h.svc.Create(ctx, session(h.alice), bad); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Errorf("invalid args: %v", err)
	}
}

func TestOnlyBrowserSessionsCreateKeys(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	scope := NewScope([]Grant{{Kind: ops.KeyResourceEverything}}, allFour)
	for name, p := range map[string]authz.Principal{
		"api key":        authz.NewAPIKeyPrincipal(h.admin, scope, ""),
		"worker":         authz.NewWorkerPrincipal(h.wAlice),
		"act-as":         authz.NewActAsPrincipal(h.admin, h.alice, h.clock.now().Add(time.Hour)),
		"zero principal": {},
	} {
		_, err := h.svc.Create(ctx, p, a)
		if name == "zero principal" {
			if err == nil {
				t.Errorf("%s: Create succeeded", name)
			}
			continue
		}
		wantCode(t, err, proto.CodeForbidden)
	}
	// An API key can neither list nor revoke keys.
	kp := authz.NewAPIKeyPrincipal(h.admin, scope, "")
	if _, err := h.svc.List(ctx, kp, ops.APIKeyListArgs{}); err == nil {
		t.Error("an API key listed keys")
	} else {
		wantCode(t, err, proto.CodeForbidden)
	}
	if err := h.svc.Revoke(ctx, kp, store.NewID()); err == nil {
		t.Error("an API key revoked a key")
	} else {
		wantCode(t, err, proto.CodeForbidden)
	}
}

// --- authentication ---

func authenticate(h *harness, token string) (Identity, error) {
	return h.svc.AuthenticateKey(context.Background(), token)
}

func TestAuthenticateReturnsTheScopedPrincipal(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	id, err := authenticate(h, res.Key)
	if err != nil {
		t.Fatal(err)
	}
	p := id.Principal
	if id.KeyID != res.Info.ID || p.Kind != authz.KindAPIKey || p.UserID != h.alice || p.WorkerID != "" || p.Scope == nil {
		t.Fatalf("identity = %+v", id)
	}
	// The principal works with the real authorizer: read on the chosen worker only.
	if err := h.az.Decide(context.Background(), p, ops.MustActionSet(ops.ActionRead), authz.WorkerResource{ID: h.wAlice}); err != nil {
		t.Errorf("read on the chosen worker: %v", err)
	}
	wantCode(t, h.az.Decide(context.Background(), p, ops.MustActionSet(ops.ActionOperate), authz.WorkerResource{ID: h.wAlice}), proto.CodeForbidden)
	wantCode(t, h.az.Decide(context.Background(), p, ops.MustActionSet(ops.ActionRead), authz.WorkerResource{ID: h.wBob}), proto.CodeNotFound)
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	stranger := identity.NewAPIKey().String()
	for name, tok := range map[string]string{
		"empty":        "",
		"no prefix":    strings.TrimPrefix(res.Key, "mn_"),
		"wrong prefix": "mx_" + strings.TrimPrefix(res.Key, "mn_"),
		"truncated":    res.Key[:len(res.Key)-1],
		"extended":     res.Key + "a",
		"uppercase":    strings.ToUpper(res.Key),
		"unknown key":  stranger,
		"hint only":    "mn_" + res.Info.Hint,
		"whitespace":   " " + res.Key,
	} {
		if _, err := authenticate(h, tok); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
}

func TestExpiredKeyFailsAsInvalidAtTheExpiryInstant(t *testing.T) {
	h := newHarness(t)
	exp := ops.Timestamp(h.clock.now().Add(time.Hour))
	a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	a.ExpiresAt = &exp
	res := mustCreate(t, h, session(h.alice), a)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	h.clock.advance(time.Hour - time.Millisecond)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatalf("a millisecond before expiry: %v", err)
	}
	h.clock.advance(time.Millisecond)
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("at expiry: %v", err)
	}
	// A key without a named expiry lives 90 days (F-07).
	dflt := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	if dflt.Info.ExpiresAt == nil || !dflt.Info.ExpiresAt.Time().Equal(h.clock.now().Add(ops.DefaultAPIKeyLifetime).Truncate(time.Millisecond)) {
		t.Errorf("default expiry = %v", dflt.Info.ExpiresAt)
	}
	h.clock.advance(ops.DefaultAPIKeyLifetime - time.Hour)
	if _, err := authenticate(h, dflt.Key); err != nil {
		t.Errorf("default key before 90 days: %v", err)
	}
	h.clock.advance(2 * time.Hour)
	if _, err := authenticate(h, dflt.Key); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("default key after 90 days: %v", err)
	}
}

func TestNeverExpiringKeysAreForAdminsOnly(t *testing.T) {
	h := newHarness(t)
	a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	a.Never = true
	_, err := h.svc.Create(issuerCtx(), session(h.alice), a)
	wantCode(t, err, proto.CodeForbidden)
	res := mustCreate(t, h, session(h.admin), a)
	if res.Info.ExpiresAt != nil {
		t.Errorf("never key has expiry %v", res.Info.ExpiresAt)
	}
	h.clock.advance(100 * 365 * 24 * time.Hour)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Errorf("admin never key: %v", err)
	}
	both := a
	exp := ops.Timestamp(h.clock.now().Add(time.Hour))
	both.ExpiresAt = &exp
	if err := both.Validate(); err == nil {
		t.Error("never with expires_at validated")
	}
}

func TestRevokedKeyFailsImmediately(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Revoke(context.Background(), session(h.alice), res.Info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("after revoke: %v", err)
	}
}

// Expiry and revocation end API keys only; an enrolled Worker authenticates
// through its own path (AC4: expiry never affects enrolled Workers).
func TestKeyExpiryAndRevocationNeverAffectEnrolledWorkers(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	worker := authz.NewWorkerPrincipal(h.wAlice)
	read := ops.MustActionSet(ops.ActionRead)
	main := authz.SessionResource{WorkerID: h.wAlice, Name: "main"}
	stillWorks := func(when string) {
		t.Helper()
		if err := h.az.Decide(ctx, worker, read, main); err != nil {
			t.Errorf("%s: the Worker identity lost access: %v", when, err)
		}
	}

	exp := ops.Timestamp(h.clock.now().Add(time.Hour))
	a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	a.ExpiresAt, a.BoundWorkerID = &exp, h.wAlice
	expiring := mustCreate(t, h, session(h.alice), a)
	if _, err := h.svc.AuthenticateCall(ctx, expiring.Key, h.wAlice); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	h.clock.advance(time.Hour + time.Second)
	if _, err := h.svc.AuthenticateKey(ctx, expiring.Key); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("AuthenticateKey after expiry: %v", err)
	}
	if _, err := h.svc.AuthenticateCall(ctx, expiring.Key, h.wAlice); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("AuthenticateCall after expiry: %v", err)
	}
	stillWorks("after expiry")

	b := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	b.BoundWorkerID = h.wAlice
	revoked := mustCreate(t, h, session(h.alice), b)
	if err := h.svc.Revoke(ctx, session(h.alice), revoked.Info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AuthenticateKey(ctx, revoked.Key); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("AuthenticateKey after revoke: %v", err)
	}
	if _, err := h.svc.AuthenticateCall(ctx, revoked.Key, h.wAlice); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("AuthenticateCall after revoke: %v", err)
	}
	stillWorks("after revoke")
}

func TestDeactivatedOrDeletedOwnerInvalidatesTheKey(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	h.exec(`UPDATE users SET active = 0 WHERE id = ?`, h.alice)
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("deactivated owner: %v", err)
	}
	h.exec(`UPDATE users SET active = 1 WHERE id = ?`, h.alice)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatalf("reactivated owner: %v", err)
	}
	// No positive cache: the next call sees the change.
	h.exec(`UPDATE users SET active = 0 WHERE id = ?`, h.alice)
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("deactivated again: %v", err)
	}
}

func TestDemotingTheOwnerReducesTheKeyOnTheNextRequest(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	// A second admin owns a key for everything.
	boss := h.addUser("boss")
	h.exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'local')`, boss)
	res := mustCreate(t, h, session(boss), args([]ops.KeyResource{everything()}, ops.ActionRead, ops.ActionOperate, ops.ActionManage))

	decide := func(r authz.Resource, a ops.Action) error {
		id, err := authenticate(h, res.Key)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		return h.az.Decide(ctx, id.Principal, ops.MustActionSet(a), r)
	}
	if err := decide(authz.WorkerResource{ID: h.wBob}, ops.ActionOperate); err != nil {
		t.Fatalf("admin key on a foreign worker: %v", err)
	}
	if err := decide(authz.UserResource{ID: boss}, ops.ActionManage); err != nil {
		t.Fatalf("admin key manage: %v", err)
	}
	// Demote between two calls with the same stored key.
	h.exec(`DELETE FROM user_roles WHERE user_id = ?`, boss)
	h.exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'user', 'local')`, boss)
	wantCode(t, decide(authz.WorkerResource{ID: h.wBob}, ops.ActionOperate), proto.CodeNotFound)
	wantCode(t, decide(authz.UserResource{ID: boss}, ops.ActionManage), proto.CodeForbidden)
	// The key still works for what the plain owner may do (own user data).
	if err := decide(authz.UserResource{ID: boss}, ops.ActionRead); err != nil {
		t.Errorf("read on own user after demotion: %v", err)
	}
}

// --- Worker-bound keys ---

func TestBoundKeyIsAcceptedOnlyOnItsWorkersCallStream(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	res := mustCreate(t, h, session(h.alice), withBound(args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionMessage), h.wAlice))
	id, err := authenticate(h, res.Key)
	if err != nil || id.Principal.WorkerID != h.wAlice {
		t.Fatalf("bound key = %+v, %v", id, err)
	}
	if got, err := h.svc.AuthenticateCall(ctx, res.Key, h.wAlice); err != nil || got.KeyID != res.Info.ID {
		t.Errorf("own call stream: %+v, %v", got, err)
	}
	if _, err := h.svc.AuthenticateCall(ctx, res.Key, h.wBob); !errors.Is(err, ErrWrongWorker) {
		t.Errorf("another Worker's call stream: %v", err)
	}
	if _, err := h.svc.AuthenticateCall(ctx, res.Key, ""); !errors.Is(err, ErrWrongWorker) {
		t.Errorf("a call stream without a Worker identity: %v", err)
	}
	// A key that is not bound is for REST and MCP, not for call streams.
	plain := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	if _, err := h.svc.AuthenticateCall(ctx, plain.Key, h.wAlice); !errors.Is(err, ErrWrongWorker) {
		t.Errorf("unbound key on a call stream: %v", err)
	}
	// A bad token stays an invalid key, whatever the stream.
	if _, err := h.svc.AuthenticateCall(ctx, identity.NewAPIKey().String(), h.wAlice); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("unknown key on a call stream: %v", err)
	}
	// A revoked bound key is refused on its own stream too.
	if err := h.svc.Revoke(ctx, session(h.alice), res.Info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AuthenticateCall(ctx, res.Key, h.wAlice); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("revoked bound key: %v", err)
	}
}

func TestCheckCallBinding(t *testing.T) {
	bound := Identity{Principal: authz.NewAPIKeyPrincipal("u", NewScope(nil, nil), "w1")}
	unbound := Identity{Principal: authz.NewAPIKeyPrincipal("u", NewScope(nil, nil), "")}
	if err := CheckCallBinding(bound, "w1"); err != nil {
		t.Errorf("same Worker: %v", err)
	}
	for name, c := range map[string]struct {
		id Identity
		w  string
	}{"other worker": {bound, "w2"}, "empty worker": {bound, ""}, "unbound": {unbound, "w1"}, "unbound empty": {unbound, ""}} {
		if err := CheckCallBinding(c.id, c.w); !errors.Is(err, ErrWrongWorker) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// --- list and revoke ---

func TestListScopesTheResultToTheCaller(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	a := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	b := mustCreate(t, h, session(h.bob), args([]ops.KeyResource{workerRef(h.wBob)}, ops.ActionRead))
	root := mustCreate(t, h, session(h.admin), args([]ops.KeyResource{everything()}, ops.ActionRead))

	ids := func(r ops.APIKeyListResult) []string {
		var out []string
		for _, k := range r.Keys {
			out = append(out, k.ID)
		}
		return out
	}
	list := func(p authz.Principal, args ops.APIKeyListArgs) (ops.APIKeyListResult, error) {
		return h.svc.List(ctx, p, args)
	}

	if r, err := list(session(h.alice), ops.APIKeyListArgs{}); err != nil || strings.Join(ids(r), ",") != a.Info.ID {
		t.Errorf("alice's own keys = %v, %v", ids(r), err)
	}
	if r, err := list(session(h.alice), ops.APIKeyListArgs{OwnerID: h.alice}); err != nil || len(r.Keys) != 1 {
		t.Errorf("alice naming herself = %v, %v", ids(r), err)
	}
	// Another user's keys: not found, like any foreign resource.
	_, err := list(session(h.alice), ops.APIKeyListArgs{OwnerID: h.bob})
	wantCode(t, err, proto.CodeNotFound)
	// all is for admins.
	_, err = list(session(h.alice), ops.APIKeyListArgs{All: true})
	wantCode(t, err, proto.CodeForbidden)
	// An admin lists one user, or everyone.
	if r, err := list(session(h.admin), ops.APIKeyListArgs{OwnerID: h.bob}); err != nil || strings.Join(ids(r), ",") != b.Info.ID {
		t.Errorf("admin listing bob = %v, %v", ids(r), err)
	}
	if r, err := list(session(h.admin), ops.APIKeyListArgs{All: true}); err != nil || len(r.Keys) != 3 {
		t.Errorf("admin listing all = %v, %v", ids(r), err)
	}
	if r, err := list(session(h.admin), ops.APIKeyListArgs{}); err != nil || strings.Join(ids(r), ",") != root.Info.ID {
		t.Errorf("admin default = %v, %v", ids(r), err)
	}
	// A user with no keys gets an empty list.
	carol := h.addUser("carol")
	if r, err := list(session(carol), ops.APIKeyListArgs{}); err != nil || r.Keys == nil || len(r.Keys) != 0 {
		t.Errorf("carol = %+v, %v", r, err)
	}
}

func TestListShowsRevokedAndExpiredKeysWithTheirState(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	exp := ops.Timestamp(h.clock.now().Add(time.Hour))
	a := args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead)
	a.ExpiresAt = &exp
	res := mustCreate(t, h, session(h.alice), a)
	h.clock.advance(time.Minute)
	if err := h.svc.Revoke(ctx, session(h.alice), res.Info.ID); err != nil {
		t.Fatal(err)
	}
	r, err := h.svc.List(ctx, session(h.alice), ops.APIKeyListArgs{})
	if err != nil || len(r.Keys) != 1 {
		t.Fatal(r, err)
	}
	k := r.Keys[0]
	if k.RevokedAt == nil || !k.RevokedAt.Time().Equal(h.clock.now()) || k.ExpiresAt == nil {
		t.Errorf("listed key = %+v", k)
	}
}

func TestRevokeRules(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	mk := func(owner string, w string) ops.APIKeyInfo {
		return mustCreate(t, h, session(owner), args([]ops.KeyResource{workerRef(w)}, ops.ActionRead)).Info
	}
	aliceKey, bobKey := mk(h.alice, h.wAlice), mk(h.bob, h.wBob)
	revoked := func(id string) bool {
		row, err := h.st.APIKeyByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row.RevokedAt != 0
	}

	// A user cannot touch another user's key, and cannot tell it exists.
	wantCode(t, h.svc.Revoke(ctx, session(h.alice), bobKey.ID), proto.CodeNotFound)
	wantCode(t, h.svc.Revoke(ctx, session(h.alice), store.NewID()), proto.CodeNotFound)
	if revoked(bobKey.ID) {
		t.Fatal("a foreign key was revoked")
	}
	// The owner revokes their own key.
	if err := h.svc.Revoke(ctx, session(h.alice), aliceKey.ID); err != nil || !revoked(aliceKey.ID) {
		t.Fatalf("owner revoke: %v", err)
	}
	// Revoking again succeeds and keeps the first time.
	first, _ := h.st.APIKeyByID(ctx, aliceKey.ID)
	h.clock.advance(time.Hour)
	if err := h.svc.Revoke(ctx, session(h.alice), aliceKey.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if again, _ := h.st.APIKeyByID(ctx, aliceKey.ID); again.RevokedAt != first.RevokedAt {
		t.Error("the revocation time moved")
	}
	// An admin revokes anyone's key, audited with the admin as actor.
	if err := h.svc.Revoke(ctx, session(h.admin), bobKey.ID); err != nil || !revoked(bobKey.ID) {
		t.Fatalf("admin revoke: %v", err)
	}
	events, _ := h.st.ListAudit(ctx, AuditQueryAll)
	var byAdmin, byAlice int
	for _, e := range events {
		if e.Action != AuditActionRevoke {
			continue
		}
		switch {
		case e.Actor == h.admin && e.Target == bobKey.ID:
			byAdmin++
		case e.Actor == h.alice && e.Target == aliceKey.ID:
			byAlice++
		}
	}
	if byAdmin != 1 || byAlice != 1 {
		t.Errorf("revoke audit rows: admin %d, alice %d, want 1 each (a repeat is not audited again)", byAdmin, byAlice)
	}
}

func TestActAsIsAuditedWithBothIdentities(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	act := authz.NewActAsPrincipal(h.admin, h.alice, h.clock.now().Add(time.Hour))
	if r, err := h.svc.List(ctx, act, ops.APIKeyListArgs{}); err != nil || len(r.Keys) != 1 {
		t.Fatalf("act-as list = %+v, %v", r, err)
	}
	if err := h.svc.Revoke(ctx, act, res.Info.ID); err != nil {
		t.Fatalf("act-as revoke: %v", err)
	}
	events, _ := h.st.ListAudit(ctx, AuditQueryAll)
	for _, e := range events {
		if e.Action == AuditActionRevoke {
			if e.Actor != h.admin || e.ActingAs != h.alice {
				t.Errorf("audit row = %+v, want actor admin acting as alice", e)
			}
			return
		}
	}
	t.Error("no revoke audit row")
}

// --- failures never carry the token ---

type failingRepo struct{ Repository }

func (failingRepo) APIKeyByHash(context.Context, []byte) (store.APIKeyRow, error) {
	return store.APIKeyRow{}, sql.ErrConnDone
}

func TestAStoreFailureIsNotAFailedAttemptAndCarriesNoToken(t *testing.T) {
	h := newHarness(t)
	svc, err := New(Config{Repo: failingRepo{h.st}, Authz: h.az, Users: sqlUsers{h.st}})
	if err != nil {
		t.Fatal(err)
	}
	tok := identity.NewAPIKey().String()
	_, err = svc.AuthenticateKey(context.Background(), tok)
	if err == nil || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err = %v, want an internal failure that is not ErrInvalidKey", err)
	}
	if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tok[3:]) {
		t.Errorf("the error carries the token: %v", err)
	}
}

// The story's last-use criterion through the real service and store: create,
// authenticate, list. Nothing is invented: a key that never authenticated has no
// last use, a refused token does not set one, and the time is the clock of the
// service.
func TestListShowsTheLastUseOfAKeyThatAuthenticated(t *testing.T) {
	h := newHarness(t)
	ctx := issuerCtx()
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	lastUsed := func() *ops.Timestamp {
		t.Helper()
		list, err := h.svc.List(ctx, session(h.alice), ops.APIKeyListArgs{})
		if err != nil || len(list.Keys) != 1 {
			t.Fatalf("list = %+v, %v", list, err)
		}
		return list.Keys[0].LastUsedAt
	}
	if got := lastUsed(); got != nil {
		t.Fatalf("a key that never authenticated shows a last use: %v", got.Time())
	}

	first := h.clock.now()
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); got == nil || !got.Time().Equal(first) {
		t.Fatalf("last use after the first authentication = %v, want %v", got, first)
	}

	// A busy key is written at most once a minute.
	h.clock.advance(30 * time.Second)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Time().Equal(first) {
		t.Errorf("a use within the minute moved the last use to %v", got.Time())
	}
	h.clock.advance(31 * time.Second)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Time().Equal(h.clock.now()) {
		t.Errorf("last use after a minute = %v, want %v", got.Time(), h.clock.now())
	}

	// A revoked key does not authenticate and does not count as used.
	before := h.clock.now()
	if err := h.svc.Revoke(ctx, session(h.alice), res.Info.ID); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key: %v", err)
	}
	if got := lastUsed(); !got.Time().Equal(before) {
		t.Errorf("a refused authentication set the last use to %v", got.Time())
	}
}

type failingTouchRepo struct{ Repository }

func (failingTouchRepo) TouchAPIKey(context.Context, string, int64) (bool, error) {
	return false, sql.ErrConnDone
}

// Recording the last use is bookkeeping: when it fails, the good key still
// authenticates.
func TestAFailingLastUseWriteDoesNotRefuseAGoodKey(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{workerRef(h.wAlice)}, ops.ActionRead))
	svc, err := New(Config{Repo: failingTouchRepo{h.st}, Authz: h.az, Users: sqlUsers{h.st}, Now: h.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthenticateKey(context.Background(), res.Key); err != nil {
		t.Fatalf("a failing last-use write refused the key: %v", err)
	}
}

// issuerCtx is the context of a request authenticated by a session whose
// credential_version is 1 (the version of every account in these tests).
func issuerCtx() context.Context { return WithIssuerVersion(context.Background(), 1) }

// raceRepo runs before between the authorization of a creation and its insert.
type raceRepo struct {
	Repository
	before func()
}

func (r raceRepo) CreateAPIKey(ctx context.Context, k store.APIKeyRow, a *store.AuditRow) error {
	r.before()
	return r.Repository.CreateAPIKey(ctx, k, a)
}

// F-07 issuance race, deterministic: the request is authenticated and
// authorized, then the password is reset (which revokes the keys), and only
// then the insert runs. The insert must refuse; no key survives.
func TestAKeyRequestAuthenticatedBeforeAPasswordResetCannotMintAKeyAfterIt(t *testing.T) {
	for name, change := range map[string]func(h *harness){
		"admin reset": func(h *harness) {
			if err := h.st.ResetPassword(context.Background(), store.ResetPasswordParams{UserID: h.alice, PasswordHash: "new", NowMS: 5}); err != nil {
				t.Fatal(err)
			}
		},
		"self-service change": func(h *harness) {
			if _, err := h.st.ChangePassword(context.Background(), store.ChangePasswordParams{UserID: h.alice, ExpectedVersion: 1, PasswordHash: "new", NowMS: 5}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.exec(`UPDATE users SET password_hash = 'old' WHERE id = ?`, h.alice)
			svc, err := New(Config{Repo: raceRepo{Repository: h.st, before: func() { change(h) }}, Authz: h.az, Users: sqlUsers{h.st}, Now: h.clock.now})
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.Create(issuerCtx(), session(h.alice), args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead))
			wantCode(t, err, proto.CodeUnauthenticated)
			keys, err := h.st.ListAPIKeys(context.Background(), h.alice)
			if err != nil || len(keys) != 0 {
				t.Fatalf("keys after the race = %d, %v", len(keys), err)
			}
		})
	}
}

func TestACreationWithoutASessionVersionIsRefused(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.Create(context.Background(), session(h.alice), args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead))
	wantCode(t, err, proto.CodeForbidden)
}

// An owner without any effective grant (all IdP roles gone) has an inert key
// that works again when a grant returns.
func TestAKeyOfAnOwnerWithoutGrantsIsInertUntilAGrantReturns(t *testing.T) {
	h := newHarness(t)
	res := mustCreate(t, h, session(h.alice), args([]ops.KeyResource{userRef(h.alice)}, ops.ActionRead))
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM user_roles WHERE user_id = ?`, h.alice)
	if _, err := authenticate(h, res.Key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("key of an owner without grants: %v", err)
	}
	// The authorizer refuses every kind of principal of that account too.
	if err := h.az.Decide(context.Background(), session(h.alice), ops.MustActionSet(ops.ActionRead), authz.UserResource{ID: h.alice}); err == nil {
		t.Error("a session principal of an account without grants was allowed")
	}
	if err := h.az.Decide(context.Background(), authz.NewWorkerPrincipal(h.wAlice), ops.MustActionSet(ops.ActionRead), authz.WorkerResource{ID: h.wAlice}); err == nil {
		t.Error("a Worker principal of an owner without grants was allowed")
	}
	h.exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'user', 'idp')`, h.alice)
	if _, err := authenticate(h, res.Key); err != nil {
		t.Fatalf("key after the grant returned: %v", err)
	}
}
