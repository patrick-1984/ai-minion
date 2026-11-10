package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

// ---- fakes ----------------------------------------------------------------------

// fakeClock fires its timers only when a test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
	delays  []time.Duration
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.delays = append(c.delays, d)
	c.waiters = append(c.waiters, clockWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var keep []clockWaiter
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

func (c *fakeClock) requested() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.delays)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// refreshIdP is the token endpoint for the refresh-token grant. The discovery
// document, the key set and the ID token signing are the fake issuer's: it
// signs the ID token with the claims and the subject that the issuer holds now,
// so a test changes a group exactly as it does for a sign-in.
type refreshIdP struct {
	srv *httptest.Server
	idp *fakeIdP

	mu     sync.Mutex
	mode   string // ok, reject, reject5xx, invalid_client, down, noid, wrongsub, slow
	accept string // the refresh token the endpoint accepts
	rotate string // when not empty, "ok" answers with this new refresh token
	delay  time.Duration
	hits   int
	seen   []string
}

func newRefreshIdP(t *testing.T, idp *fakeIdP, accept string) *refreshIdP {
	t.Helper()
	r := &refreshIdP{idp: idp, mode: "ok", accept: accept}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *refreshIdP) set(fn func(r *refreshIdP)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

func (r *refreshIdP) hitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func (r *refreshIdP) serve(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	r.mu.Lock()
	r.hits++
	mode, accept, rotate, delay := r.mode, r.accept, r.rotate, r.delay
	r.seen = append(r.seen, req.PostForm.Get("refresh_token"))
	r.mu.Unlock()
	time.Sleep(delay)

	if req.PostForm.Get("grant_type") != "refresh_token" {
		jsonError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	switch mode {
	case "down":
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
		return
	case "reject5xx":
		jsonError(w, http.StatusInternalServerError, "invalid_grant", "")
		return
	case "invalid_client":
		jsonError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	case "reject":
		jsonError(w, http.StatusBadRequest, "invalid_grant", "Token is not active")
		return
	}
	if req.PostForm.Get("refresh_token") != accept {
		jsonError(w, http.StatusBadRequest, "invalid_grant", "")
		return
	}
	resp := map[string]any{"access_token": "at-" + randomHex(), "token_type": "Bearer", "expires_in": 300}
	if rotate != "" {
		resp["refresh_token"] = rotate
	}
	if mode != "noid" {
		r.idp.set(func(f *fakeIdP) {
			sub := f.subject
			if mode == "wrongsub" {
				sub = "somebody-else"
			}
			resp["id_token"] = f.signIDToken("", sub, f.claims)
		})
	}
	writeJSON(w, resp)
}

// jsonError answers with an OAuth error in JSON; the content type is set
// before the status, as a real token endpoint does.
func jsonError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	_ = json.NewEncoder(w).Encode(body)
}

// refreshHub is a Hub with one signed-in OIDC account (alice, admin and user
// through the IdP) whose refresh token is "rt-1".
type refreshHub struct {
	*testHub
	rs     *refreshIdP
	r      *Refresher
	clock  *fakeClock
	rlogs  *bytes.Buffer
	userID string
	token  string // the browser session token of the first sign-in
	cfgs   *staticSource
}

func newRefreshHub(t *testing.T) *refreshHub {
	t.Helper()
	h := newTestHub(t, nil)
	// Every sign-in gets a later time than the one before, as it has in a real
	// Hub: the sign-in revision of an account is the time of its last sign-in.
	var tick atomic.Int64
	start := time.Now()
	h.svc.Now = func() time.Time { return start.Add(time.Duration(tick.Add(1)) * time.Second) }
	h.groups("minion-admins", "minion-users")
	h.idp.set(func(f *fakeIdP) { f.claims["preferred_username"] = "alice"; f.refreshToken = "rt-1" })
	r := h.signIn()
	sess, ok := h.sessionOf(r)
	if !ok {
		t.Fatalf("first sign-in failed: %d %s", r.status, r.body)
	}
	// The sign-in used the discovery document as it was; from now on the token
	// endpoint is the refresh endpoint.
	rs := newRefreshIdP(t, h.idp, "rt-1")
	h.idp.set(func(f *fakeIdP) { f.tokenURL = rs.srv.URL })

	cfg := baseConfig(h.idp)
	cfg.InsecureSkipVerify = true // the refresh endpoint of this test is plain http
	rh := &refreshHub{testHub: h, rs: rs, clock: newFakeClock(), rlogs: &bytes.Buffer{}, userID: sess.UserID,
		token: r.cookie(session.CookieName).Value, cfgs: &staticSource{cfgs: []Config{cfg}}}
	rh.r = &Refresher{
		Configs: rh.cfgs, Store: &SQLStore{Store: h.st}, Audit: h.aud, Clock: rh.clock,
		Log: slog.New(slog.NewTextHandler(rh.rlogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return rh
}

func (rh *refreshHub) run() RunSummary {
	rh.t.Helper()
	sum, err := rh.r.RunOnce(context.Background())
	if err != nil {
		rh.t.Fatalf("RunOnce: %v", err)
	}
	return sum
}

func (rh *refreshHub) storedToken() string {
	rh.t.Helper()
	var sealed []byte
	var id string
	if err := rh.st.DB().QueryRow(`SELECT id, refresh_token_enc FROM external_identities WHERE user_id = ?`, rh.userID).Scan(&id, &sealed); err != nil {
		rh.t.Fatal(err)
	}
	if len(sealed) == 0 {
		return ""
	}
	plain, err := rh.st.Box().Open("external_identities", "refresh_token", id, sealed)
	if err != nil {
		rh.t.Fatal(err)
	}
	return string(plain)
}

func (rh *refreshHub) sessionAlive(token string) bool {
	_, err := session.NewManager(rh.st).Resolve(context.Background(), token)
	return err == nil
}

func (rh *refreshHub) grantLocal(role string) {
	rh.t.Helper()
	if _, err := rh.st.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, ?, 'local')`, rh.userID, role); err != nil {
		rh.t.Fatal(err)
	}
}

func (rh *refreshHub) localRoles() []string {
	rh.t.Helper()
	rows, err := rh.st.DB().Query(`SELECT role FROM user_roles WHERE user_id = ? AND source = 'local' ORDER BY role`, rh.userID)
	if err != nil {
		rh.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	return out
}

func (rh *refreshHub) addWorker() {
	rh.t.Helper()
	pub := bytes.Repeat([]byte{7}, 32)
	if _, err := rh.st.DB().Exec(`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('w1', ?, 'lab', ?, 'private', 1)`, pub, rh.userID); err != nil {
		rh.t.Fatal(err)
	}
}

func (rh *refreshHub) workerRows() string {
	rh.t.Helper()
	var s string
	err := rh.st.DB().QueryRow(`SELECT group_concat(id || ':' || name || ':' || owner_id || ':' || kind || ':' || created_at, ',') FROM workers`).Scan(&s)
	if err != nil {
		rh.t.Fatal(err)
	}
	return s
}

func (rh *refreshHub) auditHas(action, outcome string) bool { return rh.aud.has(action, outcome) }

func wantRoles(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// ---- AC: fresh claims re-evaluate the roles --------------------------------------

func TestRefreshDropsRoleWhenGroupRemoved(t *testing.T) {
	rh := newRefreshHub(t)
	wantRoles(t, "roles after sign-in", rh.idpRoles(rh.userID), "admin", "user")

	auditBefore := len(rh.aud.all())
	rh.groups("minion-users") // removed from minion-admins at the IdP
	sum := rh.run()

	wantRoles(t, "roles after refresh", rh.idpRoles(rh.userID), "user")
	if sum.Targets != 1 || sum.Refreshed != 1 || sum.Changed != 1 || sum.Suspended != 0 || sum.Unchanged != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if len(rh.aud.all()) != auditBefore+1 {
		t.Errorf("want exactly one new audit row: %+v", rh.aud.all()[auditBefore:])
	}
	if ev := rh.aud.all()[auditBefore]; ev.Action != AuditActionRoleSync || ev.Outcome != OutcomeChanged || ev.Actor != "system" || ev.Target != rh.userID {
		t.Errorf("audit row = %+v", ev)
	}
	if rh.storedToken() != "rt-1" {
		t.Errorf("the token changed without rotation: %q", rh.storedToken())
	}
}

func TestRefreshWithUnchangedGroupsChangesNothingAndWritesNoAudit(t *testing.T) {
	rh := newRefreshHub(t)
	before := len(rh.aud.all())
	sum := rh.run()
	wantRoles(t, "roles", rh.idpRoles(rh.userID), "admin", "user")
	if sum.Refreshed != 1 || sum.Changed != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if len(rh.aud.all()) != before {
		t.Errorf("audit rows were written for an unchanged refresh: %+v", rh.aud.all()[before:])
	}
}

func TestRefreshAddsRoleWhenGroupAdded(t *testing.T) {
	rh := newRefreshHub(t)
	rh.groups("minion-users")
	rh.run()
	wantRoles(t, "roles", rh.idpRoles(rh.userID), "user")
	rh.groups("minion-admins", "minion-users")
	rh.run()
	wantRoles(t, "roles", rh.idpRoles(rh.userID), "admin", "user")
}

func TestRefreshDemotesTheLastAdmin(t *testing.T) {
	rh := newRefreshHub(t)
	rh.groups("minion-users")
	rh.run()
	u, err := rh.st.UserByID(context.Background(), rh.userID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != "user" {
		t.Errorf("effective role = %q after demotion, want user", u.Role)
	}
}

func TestRefreshWithNoMatchingGroupDropsRolesAndEndsSessions(t *testing.T) {
	for name, claims := range map[string]map[string]any{
		"no rule matches": {"groups": []any{"strangers"}},
		"claim missing":   {"email": "x@example.com"},
		"claim empty":     {"groups": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			rh := newRefreshHub(t)
			if !rh.sessionAlive(rh.token) {
				t.Fatal("setup: the session is not alive")
			}
			rh.idp.set(func(f *fakeIdP) { f.claims = claims })
			sum := rh.run()
			wantRoles(t, "roles", rh.idpRoles(rh.userID))
			if sum.Changed != 1 || sum.Suspended != 0 {
				t.Errorf("summary = %+v", sum)
			}
			if rh.sessionAlive(rh.token) {
				t.Error("the session of an account without any IdP role survived")
			}
			if rh.storedToken() != "rt-1" {
				t.Error("fresh claims without roles must keep the token (the next sign-in or refresh may bring the roles back)")
			}
		})
	}
}

func TestRefreshStoresARotatedToken(t *testing.T) {
	rh := newRefreshHub(t)
	rh.rs.set(func(r *refreshIdP) { r.rotate = "rt-2" })
	rh.run()
	if got := rh.storedToken(); got != "rt-2" {
		t.Fatalf("stored token = %q, want the rotated one", got)
	}
	// The old token is dead now; the next pass must use the new one.
	rh.rs.set(func(r *refreshIdP) { r.accept = "rt-2"; r.rotate = "" })
	rh.groups("minion-users")
	rh.run()
	wantRoles(t, "roles", rh.idpRoles(rh.userID), "user")
}

// ---- AC: no fresh claims, no change ---------------------------------------------------

func TestUnreachableIdPChangesNothing(t *testing.T) {
	for name, setup := range map[string]func(rh *refreshHub){
		"token endpoint answers 503": func(rh *refreshHub) { rh.rs.set(func(r *refreshIdP) { r.mode = "down" }) },
		"token endpoint is closed":   func(rh *refreshHub) { rh.rs.srv.Close() },
		"token endpoint is too slow": func(rh *refreshHub) {
			rh.cfgs.set(func(c *Config) { c.Timeout = 150 * time.Millisecond })
			rh.rs.set(func(r *refreshIdP) { r.mode = "slow"; r.delay = time.Second })
		},
		"5xx that mentions invalid_grant": func(rh *refreshHub) { rh.rs.set(func(r *refreshIdP) { r.mode = "reject5xx" }) },
		"client credentials refused":      func(rh *refreshHub) { rh.rs.set(func(r *refreshIdP) { r.mode = "invalid_client" }) },
		"discovery fails": func(rh *refreshHub) {
			rh.r.Discover = func(context.Context, Config) (*Provider, error) {
				return nil, errors.New("dial tcp: connection refused")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rh := newRefreshHub(t)
			rh.grantLocal("admin")
			rh.groups("strangers") // would drop every role if the refresh counted
			auditBefore := len(rh.aud.all())
			setup(rh)

			sum := rh.run()

			wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
			wantRoles(t, "local roles", rh.localRoles(), "admin")
			if rh.storedToken() != "rt-1" {
				t.Errorf("stored token = %q, the token must stay", rh.storedToken())
			}
			if !rh.sessionAlive(rh.token) {
				t.Error("the session ended although nothing was learned")
			}
			if sum.Unchanged != 1 || sum.Refreshed != 0 || sum.Suspended != 0 || sum.Changed != 0 {
				t.Errorf("summary = %+v", sum)
			}
			if len(rh.aud.all()) != auditBefore {
				t.Errorf("audit rows written: %+v", rh.aud.all()[auditBefore:])
			}
		})
	}
}

func TestRefreshWithoutUsableClaimsChangesNothing(t *testing.T) {
	for name, mode := range map[string]string{
		"no ID token in the answer":  "noid",
		"ID token of another person": "wrongsub",
	} {
		t.Run(name, func(t *testing.T) {
			rh := newRefreshHub(t)
			rh.groups("strangers")
			rh.rs.set(func(r *refreshIdP) { r.mode = mode })
			sum := rh.run()
			wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
			if sum.Unchanged != 1 || sum.Refreshed != 0 {
				t.Errorf("summary = %+v", sum)
			}
			if !rh.sessionAlive(rh.token) {
				t.Error("the session ended")
			}
		})
	}
}

func TestRotatedTokenIsKeptEvenWithoutClaims(t *testing.T) {
	rh := newRefreshHub(t)
	rh.groups("strangers")
	rh.rs.set(func(r *refreshIdP) { r.mode = "noid"; r.rotate = "rt-2" })
	rh.run()
	wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
	if got := rh.storedToken(); got != "rt-2" {
		t.Errorf("stored token = %q, want the rotated token (the old one may be dead)", got)
	}
}

// ---- AC: a rejected token suspends the IdP roles until the next login -----------------

func TestRejectedRefreshTokenSuspendsIdPRolesUntilNextLogin(t *testing.T) {
	rh := newRefreshHub(t)
	rh.grantLocal("user")
	rh.addWorker()
	workers := rh.workerRows()
	if workers == "" {
		t.Fatal("setup: no worker row")
	}

	rh.rs.set(func(r *refreshIdP) { r.mode = "reject" })
	sum := rh.run()

	if sum.Suspended != 1 || sum.Refreshed != 0 {
		t.Errorf("summary = %+v", sum)
	}
	wantRoles(t, "idp roles", rh.idpRoles(rh.userID))
	wantRoles(t, "local roles", rh.localRoles(), "user")
	if rh.storedToken() != "" {
		t.Error("the rejected token was not dropped")
	}
	if rh.sessionAlive(rh.token) {
		t.Error("the session survived the suspension")
	}
	if got := rh.workerRows(); got != workers {
		t.Errorf("Worker rows changed: %q -> %q", workers, got)
	}
	if !rh.auditHas(AuditActionRoleSuspend, OutcomeSuccess) {
		t.Errorf("no role.suspend audit row: %+v", rh.aud.all())
	}

	// Suspended means suspended: the next pass has nothing to refresh and does
	// not ask the IdP again, even when the IdP would accept the token now.
	hits := rh.rs.hitCount()
	rh.rs.set(func(r *refreshIdP) { r.mode = "ok" })
	sum = rh.run()
	if sum.Targets != 0 || rh.rs.hitCount() != hits {
		t.Errorf("a suspended account was refreshed: %+v, hits %d -> %d", sum, hits, rh.rs.hitCount())
	}
	wantRoles(t, "idp roles", rh.idpRoles(rh.userID))

	// The next OIDC login restores the roles from the fresh claims and stores
	// a token again; the refresh works from then on.
	r := rh.signIn()
	if r.status != http.StatusSeeOther {
		t.Fatalf("sign-in = %d\n%s", r.status, r.body)
	}
	wantRoles(t, "idp roles after login", rh.idpRoles(rh.userID), "admin", "user")
	if rh.storedToken() != "rt-1" {
		t.Errorf("stored token after login = %q", rh.storedToken())
	}
	rh.groups("minion-users")
	if sum = rh.run(); sum.Refreshed != 1 {
		t.Errorf("summary after login = %+v", sum)
	}
	wantRoles(t, "idp roles after refresh", rh.idpRoles(rh.userID), "user")
	wantRoles(t, "local roles", rh.localRoles(), "user")
	if got := rh.workerRows(); got != workers {
		t.Errorf("Worker rows changed: %q -> %q", workers, got)
	}
}

func TestSuspensionOfAnAccountWithNoIdPRolesStillDropsTheToken(t *testing.T) {
	rh := newRefreshHub(t)
	rh.groups("strangers")
	rh.run() // fresh claims without roles: the account has none now
	rh.rs.set(func(r *refreshIdP) { r.mode = "reject" })
	sum := rh.run()
	if sum.Suspended != 1 || rh.storedToken() != "" {
		t.Errorf("summary = %+v, token %q", sum, rh.storedToken())
	}
}

func TestLocalGrantsSurviveEveryOutcome(t *testing.T) {
	for _, mode := range []string{"ok", "reject", "down", "noid"} {
		t.Run(mode, func(t *testing.T) {
			rh := newRefreshHub(t)
			rh.grantLocal("admin")
			rh.grantLocal("user")
			rh.addWorker()
			workers := rh.workerRows()
			rh.groups("strangers")
			rh.rs.set(func(r *refreshIdP) { r.mode = mode })
			rh.run()
			wantRoles(t, "local roles", rh.localRoles(), "admin", "user")
			if got := rh.workerRows(); got != workers {
				t.Errorf("Worker rows changed: %q -> %q", workers, got)
			}
		})
	}
}

func TestLaterSignInWinsOverARefreshThatStartedBefore(t *testing.T) {
	// The store-level cases are in internal/hub/store/oidc_refresh_test.go;
	// this checks that the adapter hands the token and the sign-in revision of
	// the target to the store.
	for name, token := range map[string]string{"new token": "rt-new", "reused token": "rt-1"} {
		t.Run(name, func(t *testing.T) {
			rh := newRefreshHub(t)
			ctx := context.Background()
			sqls := &SQLStore{Store: rh.st}
			targets, err := sqls.RefreshTargets(ctx)
			if err != nil || len(targets) != 1 {
				t.Fatalf("targets = %v, %v", targets, err)
			}
			stale := targets[0]
			if stale.LastLoginAt == 0 || stale.RefreshToken != "rt-1" {
				t.Fatalf("target = %v", stale)
			}

			// A sign-in happens while the refresh with the old token runs.
			rh.idp.set(func(f *fakeIdP) { f.refreshToken = token })
			if r := rh.signIn(); r.status != http.StatusSeeOther {
				t.Fatalf("sign-in = %d", r.status)
			}

			for name, u := range map[string]Update{
				"suspend":       {Target: stale, Suspend: true},
				"replace":       {Target: stale, ReplaceRoles: true, Roles: nil},
				"store a token": {Target: stale, NewToken: "rt-x"},
			} {
				res, err := sqls.Apply(ctx, u)
				if err != nil || res.Applied {
					t.Errorf("%s: res = %+v, err = %v; a stale update must not apply", name, res, err)
				}
			}
			wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
			if rh.storedToken() != token {
				t.Errorf("stored token = %q", rh.storedToken())
			}
		})
	}
}

func TestDeactivatedAccountsAreNotRefreshed(t *testing.T) {
	rh := newRefreshHub(t)
	if _, err := rh.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, rh.userID); err != nil {
		t.Fatal(err)
	}
	rh.groups("strangers")
	sum := rh.run()
	if sum.Targets != 0 || rh.rs.hitCount() != 0 {
		t.Errorf("a deactivated account was refreshed: %+v", sum)
	}
	wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
}

func TestAccountsOfAnUnknownDisabledOrMovedProviderAreSkipped(t *testing.T) {
	for name, mod := range map[string]func(c *Config){
		"disabled":     func(c *Config) { c.Disabled = true },
		"other id":     func(c *Config) { c.ID = "second" },
		"other issuer": func(c *Config) { c.Issuer = "https://other.example" },
	} {
		t.Run(name, func(t *testing.T) {
			rh := newRefreshHub(t)
			rh.cfgs.set(mod)
			rh.groups("strangers")
			sum := rh.run()
			if sum.Skipped != 1 || rh.rs.hitCount() != 0 {
				t.Errorf("summary = %+v, hits = %d", sum, rh.rs.hitCount())
			}
			wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
		})
	}
}

func TestListingFailureIsAnErrorAndChangesNothing(t *testing.T) {
	rh := newRefreshHub(t)
	rh.cfgs.mu.Lock()
	rh.cfgs.err = errors.New("settings unavailable")
	rh.cfgs.mu.Unlock()
	if _, err := rh.r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce did not report the failure")
	}
	wantRoles(t, "idp roles", rh.idpRoles(rh.userID), "admin", "user")
}

func TestNoSecretReachesTheLog(t *testing.T) {
	rh := newRefreshHub(t)
	rh.rs.set(func(r *refreshIdP) { r.mode = "noid"; r.rotate = "rt-2-secret" })
	rh.run()
	rh.rs.set(func(r *refreshIdP) { r.mode = "reject" })
	rh.run()
	if rh.rlogs.Len() == 0 {
		t.Fatal("nothing was logged")
	}
	for _, secret := range []string{"rt-1", "rt-2-secret", "s3cret-for-tests", "Token is not active"} {
		if strings.Contains(rh.rlogs.String(), secret) {
			t.Errorf("%q reached the log:\n%s", secret, rh.rlogs)
		}
	}
	for _, e := range rh.aud.all() {
		if strings.Contains(e.Target+e.Action+e.Actor, "rt-") {
			t.Errorf("audit row carries a token: %+v", e)
		}
	}
}

// ---- the rules of this pass decide, not those of the cached provider ------------------

// The discovered provider is cached between passes and its fingerprint leaves
// the role rules out, so a rule change must still apply at the next pass.

func (rh *refreshHub) setRules(mod func(c *Config)) {
	rh.cfgs.set(mod)         // the refresher
	rh.testHub.cfgs.set(mod) // the sign-in
}

func TestRefreshUsesTheRoleRulesOfNowNotOfTheCachedProvider_AdminValues(t *testing.T) {
	rh := newRefreshHub(t)
	rh.run() // the first pass discovers and caches the provider
	wantRoles(t, "roles after the first pass", rh.idpRoles(rh.userID), "admin", "user")

	// An administrator removes the admin mapping; the IdP group is unchanged.
	rh.setRules(func(c *Config) { c.AdminValues = []string{"hub-admins"} })
	auditBefore := len(rh.aud.all())
	sum := rh.run()

	wantRoles(t, "roles after the rule change", rh.idpRoles(rh.userID), "user")
	if sum.Changed != 1 || len(rh.aud.all()) != auditBefore+1 {
		t.Errorf("summary = %+v, audit rows +%d", sum, len(rh.aud.all())-auditBefore)
	}
	// And back: the mapping that returns is applied as well.
	rh.setRules(func(c *Config) { c.AdminValues = []string{"minion-admins"} })
	rh.run()
	wantRoles(t, "roles after the rule is restored", rh.idpRoles(rh.userID), "admin", "user")
}

func TestRefreshUsesTheRoleRulesOfNowNotOfTheCachedProvider_RoleClaim(t *testing.T) {
	rh := newRefreshHub(t)
	rh.run()
	wantRoles(t, "roles after the first pass", rh.idpRoles(rh.userID), "admin", "user")

	rh.idp.set(func(f *fakeIdP) { f.claims["idp_roles"] = anySlice([]string{"minion-users"}) })
	rh.setRules(func(c *Config) { c.RoleClaim = "idp_roles" })
	rh.run()

	wantRoles(t, "roles after the claim name changed", rh.idpRoles(rh.userID), "user")
}

func TestASignInUnderNewRulesIsNotUndoneByTheNextRefresh(t *testing.T) {
	rh := newRefreshHub(t)
	rh.run() // caches the provider with the old rules

	rh.setRules(func(c *Config) { c.AdminValues = []string{"hub-admins"} })
	if r := rh.signIn(); r.status != http.StatusSeeOther {
		t.Fatalf("sign-in = %d\n%s", r.status, r.body)
	}
	wantRoles(t, "roles after the sign-in under the new rules", rh.idpRoles(rh.userID), "user")

	auditBefore := len(rh.aud.all())
	sum := rh.run()

	wantRoles(t, "roles after the refresh", rh.idpRoles(rh.userID), "user")
	if sum.Refreshed != 1 || sum.Changed != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if len(rh.aud.all()) != auditBefore {
		t.Errorf("the refresh wrote audit rows: %+v", rh.aud.all()[auditBefore:])
	}
}

// ---- the interval and the schedule ----------------------------------------------------

func TestRefreshIntervalSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"": DefaultRefreshInterval, "24h": 24 * time.Hour, "90m": 90 * time.Minute, "1m": time.Minute, "720h": 720 * time.Hour} {
		got, err := ParseRefreshInterval(in)
		if err != nil || got != want {
			t.Errorf("ParseRefreshInterval(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"soon", "30s", "-1h", "0", "721h", "24"} {
		if _, err := ParseRefreshInterval(in); err == nil {
			t.Errorf("ParseRefreshInterval(%q) was accepted", in)
		}
	}
	if DefaultRefreshInterval != 24*time.Hour || SettingRefreshInterval != "oidc_refresh_interval" {
		t.Errorf("default %v, key %q", DefaultRefreshInterval, SettingRefreshInterval)
	}
}

// mapPanel is a panel source that tests change between calls.
type mapPanel struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *mapPanel) Lookup(key string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[key]
	return v, ok, nil
}

func (p *mapPanel) set(key, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[key] = v
}

// intervalResolver is a Hub resolver over a test registry that declares the
// setting like the Hub does.
func intervalResolver(env map[string]string, panel *mapPanel) *config.Resolver {
	reg := config.NewRegistry()
	reg.Declare(config.Setting{Key: config.KeyOIDCRefreshInterval, Scope: config.ScopeHub, Default: "24h"})
	return &config.Resolver{
		Registry: reg, Role: config.ScopeHub, Panel: panel,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}
}

func TestIntervalFromResolverFollowsEnvironmentThenPanelThenDefault(t *testing.T) {
	ctx := context.Background()
	panel := &mapPanel{m: map[string]string{}}
	env := map[string]string{}
	get := IntervalFromResolver(intervalResolver(env, panel))

	if d, err := get(ctx); err != nil || d != 24*time.Hour {
		t.Errorf("default = %v, %v; want 24h", d, err)
	}
	// The panel beats the default, and is read on every call.
	panel.set(SettingRefreshInterval, "6h")
	if d, err := get(ctx); err != nil || d != 6*time.Hour {
		t.Errorf("panel = %v, %v; want 6h", d, err)
	}
	panel.set(SettingRefreshInterval, " 90m ")
	if d, err := get(ctx); err != nil || d != 90*time.Minute {
		t.Errorf("panel with spaces = %v, %v; want 90m", d, err)
	}
	// The environment beats the panel (AD-19).
	env["MINION_OIDC_REFRESH_INTERVAL"] = "2h"
	if d, err := get(ctx); err != nil || d != 2*time.Hour {
		t.Errorf("env over panel = %v, %v; want 2h", d, err)
	}
	env["MINION_OIDC_REFRESH_INTERVAL"] = "  3h\n"
	if d, err := get(ctx); err != nil || d != 3*time.Hour {
		t.Errorf("env with spaces = %v, %v; want 3h", d, err)
	}
	// An empty value is the default; the bounds apply.
	delete(env, "MINION_OIDC_REFRESH_INTERVAL")
	panel.set(SettingRefreshInterval, "")
	if d, err := get(ctx); err != nil || d != 24*time.Hour {
		t.Errorf("empty panel value = %v, %v; want 24h", d, err)
	}
	for _, bad := range []string{"banana", "30s", "721h", "24"} {
		panel.set(SettingRefreshInterval, bad)
		if _, err := get(ctx); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	// An invalid value of the environment is an error even when the panel is fine.
	panel.set(SettingRefreshInterval, "6h")
	env["MINION_OIDC_REFRESH_INTERVAL"] = "soon"
	if _, err := get(ctx); err == nil || !strings.Contains(err.Error(), "env") {
		t.Errorf("invalid env value: err = %v", err)
	}
}

func TestIntervalOfASetupWithoutTheDeclarationIsAnError(t *testing.T) {
	r := &config.Resolver{Registry: config.NewRegistry(), Role: config.ScopeHub, LookupEnv: func(string) (string, bool) { return "", false }}
	if _, err := IntervalFromResolver(r)(context.Background()); err == nil {
		t.Error("an undeclared setting resolved")
	}
}

func TestAnInvalidIntervalFallsBackToTheDefaultWithAWarning(t *testing.T) {
	var logs bytes.Buffer
	panel := &mapPanel{m: map[string]string{SettingRefreshInterval: "banana"}}
	r := &Refresher{
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
		Interval: IntervalFromResolver(intervalResolver(nil, panel)),
	}
	if d := r.interval(context.Background()); d != DefaultRefreshInterval {
		t.Errorf("interval = %v, want the default", d)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), SettingRefreshInterval) {
		t.Errorf("no warning:\n%s", logs.String())
	}
	panel.set(SettingRefreshInterval, "4h")
	if d := r.interval(context.Background()); d != 4*time.Hour {
		t.Errorf("interval = %v after the fix, want 4h", d)
	}
}

func TestIntervalFallsBackAndIsBounded(t *testing.T) {
	r := &Refresher{Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	ctx := context.Background()
	if d := r.interval(ctx); d != 24*time.Hour {
		t.Errorf("nil Interval = %v", d)
	}
	for _, c := range []struct {
		d    time.Duration
		err  error
		want time.Duration
	}{
		{2 * time.Hour, nil, 2 * time.Hour},
		{time.Second, nil, MinRefreshInterval},
		{0, nil, MinRefreshInterval},
		{10000 * time.Hour, nil, MaxRefreshInterval},
		{time.Hour, errors.New("bad setting"), 24 * time.Hour},
	} {
		r.Interval = func(context.Context) (time.Duration, error) { return c.d, c.err }
		if got := r.interval(ctx); got != c.want {
			t.Errorf("Interval (%v, %v) -> %v, want %v", c.d, c.err, got, c.want)
		}
	}
}

type countingStore struct {
	RefreshStore
	passes atomic.Int32
}

func (c *countingStore) RefreshTargets(ctx context.Context) ([]Target, error) {
	c.passes.Add(1)
	return c.RefreshStore.RefreshTargets(ctx)
}

func TestRunRefreshesOnTheClockAndReadsTheIntervalEveryCycle(t *testing.T) {
	rh := newRefreshHub(t)
	cs := &countingStore{RefreshStore: rh.r.Store}
	rh.r.Store = cs
	var interval atomic.Int64
	interval.Store(int64(24 * time.Hour))
	rh.r.Interval = func(context.Context) (time.Duration, error) { return time.Duration(interval.Load()), nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rh.r.Run(ctx) }()

	// Nothing happens before the start delay.
	waitFor(t, "the start timer", func() bool { return rh.clock.pending() == 1 })
	if got := rh.clock.requested(); got[0] != DefaultRefreshStartDelay {
		t.Errorf("first wait = %v, want %v", got[0], DefaultRefreshStartDelay)
	}
	rh.clock.Advance(DefaultRefreshStartDelay - time.Second)
	time.Sleep(20 * time.Millisecond)
	if cs.passes.Load() != 0 {
		t.Fatal("a pass ran before the start delay")
	}

	// A group removal is picked up by the first pass, 24 h interval after it.
	rh.groups("minion-users")
	rh.clock.Advance(time.Second)
	waitFor(t, "pass 1", func() bool { return cs.passes.Load() == 1 })
	waitFor(t, "the interval timer", func() bool { return rh.clock.pending() == 1 })
	wantRoles(t, "roles after pass 1", rh.idpRoles(rh.userID), "user")
	if got := rh.clock.requested(); got[len(got)-1] != 24*time.Hour {
		t.Errorf("second wait = %v, want 24h", got[len(got)-1])
	}

	// A shorter interval applies from the next cycle on.
	interval.Store(int64(2 * time.Hour))
	rh.clock.Advance(23 * time.Hour)
	time.Sleep(20 * time.Millisecond)
	if cs.passes.Load() != 1 {
		t.Fatal("a pass ran before the interval")
	}
	rh.clock.Advance(time.Hour)
	waitFor(t, "pass 2", func() bool { return cs.passes.Load() == 2 })
	waitFor(t, "the third timer", func() bool { return rh.clock.pending() == 1 })
	if got := rh.clock.requested(); got[len(got)-1] != 2*time.Hour {
		t.Errorf("third wait = %v, want 2h", got[len(got)-1])
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunSurvivesAFailingPass(t *testing.T) {
	rh := newRefreshHub(t)
	rh.cfgs.mu.Lock()
	rh.cfgs.err = errors.New("settings unavailable")
	rh.cfgs.mu.Unlock()
	rh.r.StartDelay = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rh.r.Run(ctx) }()
	waitFor(t, "the start timer", func() bool { return rh.clock.pending() == 1 })
	rh.clock.Advance(time.Minute)
	waitFor(t, "the next timer after the failed pass", func() bool { return rh.clock.pending() == 1 && len(rh.clock.requested()) == 2 })
	if !strings.Contains(rh.rlogs.String(), "pass failed") {
		t.Errorf("the failure was not logged:\n%s", rh.rlogs)
	}
}
