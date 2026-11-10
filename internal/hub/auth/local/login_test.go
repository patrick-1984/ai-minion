package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// ---- fakes -----------------------------------------------------------------

type fakeUsers struct {
	mu    sync.Mutex
	users []store.UserRow
	err   error
}

func (f *fakeUsers) UserByIdentifier(_ context.Context, ident string) (store.UserRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.UserRow{}, f.err
	}
	for _, u := range f.users {
		if strings.EqualFold(u.Username, ident) {
			return u, nil
		}
	}
	for _, u := range f.users {
		if u.Email != "" && strings.EqualFold(u.Email, ident) {
			return u, nil
		}
	}
	return store.UserRow{}, store.ErrNotFound
}

type recAuditor struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *recAuditor) Record(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recAuditor) last(t *testing.T) audit.Event {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		t.Fatal("no audit event was recorded")
	}
	return r.events[len(r.events)-1]
}

func (r *recAuditor) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.events) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type memSessions struct {
	mu      sync.Mutex
	rows    map[string]store.SessionRow
	users   map[string]store.UserRow
	failDel error
}

func (m *memSessions) CreateSession(_ context.Context, row store.SessionRow, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[row.TokenHash] = row
	return nil
}

func (m *memSessions) SessionByTokenHash(_ context.Context, h string) (store.SessionRow, store.UserRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[h]
	if !ok {
		return store.SessionRow{}, store.UserRow{}, store.ErrNotFound
	}
	return r, m.users[r.UserID], nil
}

func (m *memSessions) DeleteSession(_ context.Context, h string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDel != nil {
		return false, m.failDel
	}
	_, ok := m.rows[h]
	delete(m.rows, h)
	return ok, nil
}

// ---- fixtures --------------------------------------------------------------

const alicePassword = "correct horse battery"

type fixture struct {
	t       *testing.T
	users   *fakeUsers
	audit   *recAuditor
	clock   *clock
	limiter *ratelimit.Limiter
	auth    *Authenticator
	sess    *memSessions
	mgr     *session.Manager
	mw      *session.Middleware
	handler *Handler
	mux     *http.ServeMux
	secure  bool
	ttl     time.Duration
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	h := fastHasher()
	hash, err := h.Hash(context.Background(), alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	alice := store.UserRow{ID: "u-alice", Username: "alice", Email: "alice@example.com", PasswordHash: hash,
		Active: true, Role: "admin", CredentialVersion: 1}
	f := &fixture{
		t:     t,
		users: &fakeUsers{users: []store.UserRow{alice}},
		audit: &recAuditor{},
		clock: &clock{t: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)},
		ttl:   2 * time.Hour,
	}
	f.sess = &memSessions{rows: map[string]store.SessionRow{}, users: map[string]store.UserRow{"u-alice": alice}}
	f.limiter, err = ratelimit.New(ratelimit.Config{Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	f.auth = &Authenticator{Store: f.users, Hasher: h, Limiter: f.limiter, Audit: f.audit}
	f.mgr = session.NewManager(f.sess, session.WithClock(f.clock.Now))
	f.mw = &session.Middleware{Manager: f.mgr}
	f.secure = true
	f.handler = &Handler{
		Auth: f.auth, Sessions: f.mgr, Audit: f.audit,
		TTL:      func(context.Context) (time.Duration, error) { return f.ttl, nil },
		Secure:   func(*http.Request) bool { return f.secure },
		ClientIP: func(r *http.Request) netip.Addr { ip, _ := ratelimit.ParseRemote(r.RemoteAddr); return ip },
	}
	f.mux = http.NewServeMux()
	if err := f.handler.Register(f.mux, f.mw); err != nil {
		t.Fatal(err)
	}
	return f
}

var ip1 = netip.MustParseAddr("192.0.2.10")

func (f *fixture) login(ident, password string) (store.UserRow, error) {
	return f.auth.Authenticate(context.Background(), Credentials{Identifier: ident, Password: password, IP: ip1})
}

func (f *fixture) post(path string, body string, contentType string, mod func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) loginJSON(ident, password string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"username": ident, "password": password})
	return f.post("/auth/login", string(b), "application/json", nil)
}

// ---- Authenticator ---------------------------------------------------------

func TestAuthenticateByUsernameOrEmail(t *testing.T) {
	f := newFixture(t)
	for _, ident := range []string{"alice", "ALICE", " alice ", "alice@example.com", "Alice@Example.COM"} {
		u, err := f.login(ident, alicePassword)
		if err != nil || u.ID != "u-alice" {
			t.Errorf("login %q = %+v, %v", ident, u, err)
		}
	}
}

func TestAuthenticateRefusals(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]struct{ ident, pw string }{
		"wrong password": {"alice", "nope"},
		"unknown user":   {"mallory", alicePassword},
		"empty ident":    {"", alicePassword},
		"empty password": {"alice", ""},
		"long password":  {"alice", strings.Repeat("x", 5000)},
		"binary ident":   {"\xff\xfe", "x"},
	} {
		if _, err := f.login(c.ident, c.pw); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}
}

func TestAuthenticateInactiveAccountLooksLikeAWrongPassword(t *testing.T) {
	f := newFixture(t)
	f.users.users[0].Active = false
	if _, err := f.login("alice", alicePassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
	if ev := f.audit.last(t); ev.Outcome != OutcomeInactive || ev.Actor != "u-alice" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestAuthenticateAccountWithoutLocalPassword(t *testing.T) {
	f := newFixture(t)
	f.users.users[0].PasswordHash = "" // an IdP-only account
	if _, err := f.login("alice", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("empty password: %v", err)
	}
	if _, err := f.login("alice", "anything"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("any password: %v", err)
	}
}

func TestEveryAttemptIsAuditedWithoutThePassword(t *testing.T) {
	f := newFixture(t)
	const typed = "S3cr3t-typed-into-the-wrong-field"
	_, _ = f.login("alice", typed) // wrong password, known account
	_, _ = f.login(typed, typed)   // password typed as user name, unknown account
	_, _ = f.login("mallory", "x") // unknown account
	if f.audit.count() != 3 {
		t.Fatalf("%d audit events for 3 failed attempts", f.audit.count())
	}
	for i, ev := range f.audit.events[:3] {
		if ev.Action != AuditActionLogin || ev.Outcome != OutcomeFailed {
			t.Errorf("event %d = %+v", i, ev)
		}
		if err := ev.Validate(); err != nil {
			t.Errorf("event %d does not satisfy the audit rules: %v", i, err)
		}
		if strings.Contains(ev.Actor+ev.Target+ev.ActingAs, typed) || strings.Contains(ev.Actor+ev.Target, "mallory") {
			t.Errorf("event %d carries typed input: %+v", i, ev)
		}
	}
	if ev := f.audit.events[0]; ev.Actor != "u-alice" || ev.Target != "alice" {
		t.Errorf("known account event = %+v", ev)
	}
	if ev := f.audit.events[1]; ev.Actor != "anonymous" || !strings.HasPrefix(ev.Target, "unknown:") {
		t.Errorf("unknown account event = %+v", ev)
	}
	// The digest is keyed: it is not the plain SHA-256 prefix of the typed
	// input, so a password dictionary cannot be checked against the log.
	for _, in := range []string{typed, "mallory"} {
		sum := sha256.Sum256([]byte(strings.ToLower(in)))
		if plain := "unknown:" + hex.EncodeToString(sum[:4]); f.audit.events[1].Target == plain || f.audit.events[2].Target == plain {
			t.Errorf("an audit target is the unkeyed SHA-256 prefix of %q", in)
		}
	}
	// The same input maps to the same target within one process, so that the
	// log can still show repeated attempts.
	_, _ = f.login("mallory", "y")
	if f.audit.events[2].Target != f.audit.events[3].Target {
		t.Error("the same unknown name maps to different targets")
	}
	// An unknown name and a known one with a wrong password are told apart in
	// the audit log only.
	if f.audit.events[1].Target == f.audit.events[2].Target {
		t.Error("different unknown names share a target")
	}
}

func TestAuditFailureDoesNotChangeTheRefusal(t *testing.T) {
	f := newFixture(t)
	f.audit.err = errors.New("audit down")
	if _, err := f.login("alice", "nope"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreFailureIsNotAnInvalidLogin(t *testing.T) {
	f := newFixture(t)
	f.users.err = errors.New("db down")
	_, err := f.login("alice", alicePassword)
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestMalformedStoredHashIsAnInternalError(t *testing.T) {
	f := newFixture(t)
	f.users.users[0].PasswordHash = "garbage"
	_, err := f.login("alice", alicePassword)
	if err == nil || errors.Is(err, ErrInvalidCredentials) || !errors.Is(err, ErrBadHash) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoffAfterRepeatedFailuresWithFakeClock(t *testing.T) {
	f := newFixture(t)
	// DefaultBackoff: three free failures, then 1 s doubling. Use different IPs
	// so only the per-account back-off and the account bucket act.
	for i := range 3 {
		_, err := f.auth.Authenticate(context.Background(), Credentials{Identifier: "alice", Password: "bad", IP: netip.MustParseAddr("198.51.100." + strconv.Itoa(i+1))})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	// The fourth failure is verified (no back-off yet) and starts the block.
	if _, err := f.login("alice", "bad"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("fourth failure: %v", err)
	}
	// Now even the right password is refused until the block ends.
	_, err := f.login("alice", alicePassword)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter <= 0 {
		t.Fatalf("during back-off: %v", err)
	}
	if ev := f.audit.last(t); ev.Outcome != OutcomeRateLimited || strings.Contains(ev.Actor+ev.Target, alicePassword) {
		t.Errorf("audit of the refused attempt = %+v", ev)
	}
	// The back-off alone lasts one second; the account's token bucket (burst 5,
	// one token per 12 s) is the longer wait here.
	f.clock.Add(rl.RetryAfter)
	if _, err := f.login("alice", alicePassword); !errors.As(err, &rl) {
		t.Fatalf("before the account bucket refilled: %v", err)
	}
	f.clock.Add(rl.RetryAfter)
	if _, err := f.login("alice", alicePassword); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
	// A success forgets the failures: three more failures are free again.
	f.clock.Add(time.Minute) // refill the buckets
	for i := range 3 {
		if _, err := f.login("alice", "bad"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("free failure %d after reset: %v", i+1, err)
		}
	}
	if _, err := f.login("alice", "bad"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("fourth failure after reset: %v", err)
	}
	if _, err := f.login("alice", alicePassword); !errors.As(err, &rl) {
		t.Fatalf("back-off did not restart: %v", err)
	}
}

func TestBackoffIsSharedByUsernameAndEmail(t *testing.T) {
	f := newFixture(t)
	idents := []string{"alice", "alice@example.com"}
	for i := range 4 {
		ip := netip.MustParseAddr("198.51.100." + strconv.Itoa(i+1))
		_, _ = f.auth.Authenticate(context.Background(), Credentials{Identifier: idents[i%2], Password: "bad", IP: ip})
	}
	_, err := f.login("alice@example.com", alicePassword)
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("alternating between user name and e-mail escaped the back-off: %v", err)
	}
}

func TestLoginPolicyLimitsAnIPAndRefusesWithoutVerifying(t *testing.T) {
	f := newFixture(t)
	// Burst of the login IP bucket is 10; a different account name each time
	// keeps the per-account limits out of the picture.
	var limited *RateLimitedError
	attempts := 0
	for i := range 30 {
		_, err := f.login("ghost"+strconv.Itoa(i), "x")
		attempts++
		if errors.As(err, &limited) {
			break
		}
	}
	if limited == nil || attempts > 12 {
		t.Fatalf("no rate limit after %d attempts", attempts)
	}
	if ev := f.audit.last(t); ev.Outcome != OutcomeRateLimited {
		t.Errorf("audit = %+v", ev)
	}
	// Even the right password from the limited address is refused.
	if _, err := f.login("alice", alicePassword); !errors.As(err, &limited) {
		t.Errorf("a limited address got through: %v", err)
	}
	// Another address is not affected, and waiting lifts the limit.
	if _, err := f.auth.Authenticate(context.Background(), Credentials{Identifier: "alice", Password: alicePassword, IP: netip.MustParseAddr("203.0.113.9")}); err != nil {
		t.Errorf("another address: %v", err)
	}
	f.clock.Add(limited.RetryAfter)
	if _, err := f.login("alice", alicePassword); err != nil {
		t.Errorf("after the wait: %v", err)
	}
}

// ---- Handler: login --------------------------------------------------------

type loginBody struct {
	User struct {
		ID                 string `json:"id"`
		Username           string `json:"username"`
		Role               string `json:"role"`
		MustChangePassword bool   `json:"must_change_password"`
	} `json:"user"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// withHeader sets one request header, to mark a request as a browser request.
func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.CookieName {
			return c
		}
	}
	return nil
}

func TestLoginIssuesCookieAndCSRFToken(t *testing.T) {
	f := newFixture(t)
	rec := f.loginJSON("alice", alicePassword)
	if rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	var body loginBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.User.ID != "u-alice" || body.User.Username != "alice" || body.User.Role != "admin" || body.User.MustChangePassword {
		t.Errorf("user = %+v", body.User)
	}
	c := sessionCookie(t, rec)
	if c == nil {
		t.Fatal("no minion_session cookie")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie attributes = %+v", c)
	}
	if !c.Expires.Equal(f.clock.Now().Add(f.ttl).Truncate(time.Second)) {
		t.Errorf("cookie expires %v, want now + ttl %v", c.Expires, f.clock.Now().Add(f.ttl))
	}
	if !session.ValidCSRF(c.Value, body.CSRFToken) {
		t.Error("the issued CSRF token is not bound to the cookie")
	}
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Error("the cookie value is in the response body")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("login response is cacheable")
	}
	// Only the hash is stored.
	for h, row := range f.sess.rows {
		if h == c.Value || row.TokenHash == c.Value {
			t.Error("the cookie value is stored")
		}
		if want, _ := session.HashToken(c.Value); h != want {
			t.Errorf("stored hash %q, want %q", h, want)
		}
	}
	if ev := f.audit.last(t); ev.Action != "login" || ev.Outcome != OutcomeSuccess || ev.Actor != "u-alice" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestLoginCookieSecureFlagFollowsTheDecision(t *testing.T) {
	f := newFixture(t)
	f.secure = false // MINION_PUBLIC_URL is plain-http loopback
	if c := sessionCookie(t, f.loginJSON("alice", alicePassword)); c == nil || c.Secure {
		t.Errorf("cookie for loopback http = %+v", c)
	}
	f.secure = true
	if c := sessionCookie(t, f.loginJSON("alice", alicePassword)); c == nil || !c.Secure {
		t.Errorf("cookie for https = %+v", c)
	}
}

func TestLoginAcceptsAFormAndAnEmailField(t *testing.T) {
	f := newFixture(t)
	form := url.Values{"username": {"alice"}, "password": {alicePassword}}.Encode()
	if rec := f.post("/auth/login", form, "application/x-www-form-urlencoded", nil); rec.Code != 200 || sessionCookie(t, rec) == nil {
		t.Errorf("form login = %d %s", rec.Code, rec.Body)
	}
	b, _ := json.Marshal(map[string]string{"email": "alice@example.com", "password": alicePassword})
	if rec := f.post("/auth/login", string(b), "application/json", nil); rec.Code != 200 {
		t.Errorf("e-mail login = %d %s", rec.Code, rec.Body)
	}
}

func TestLoginMustChangeFlagIsReported(t *testing.T) {
	f := newFixture(t)
	f.users.users[0].MustChangePassword = true
	var body loginBody
	rec := f.loginJSON("alice", alicePassword)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.User.MustChangePassword {
		t.Errorf("must_change_password missing: %s", rec.Body)
	}
}

func errEnvelope(t *testing.T, rec *httptest.ResponseRecorder) proto.Code {
	t.Helper()
	var body struct {
		Error struct {
			Code    proto.Code     `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an error envelope: %q", rec.Body)
	}
	if rec.Code != proto.HTTPStatus(body.Error.Code) {
		t.Errorf("status %d for code %s", rec.Code, body.Error.Code)
	}
	return body.Error.Code
}

func TestLoginErrors(t *testing.T) {
	f := newFixture(t)
	rec := f.loginJSON("alice", "wrong")
	if errEnvelope(t, rec) != proto.CodeUnauthenticated || sessionCookie(t, rec) != nil {
		t.Errorf("wrong password = %d %s", rec.Code, rec.Body)
	}
	rec = f.loginJSON("nobody", "wrong")
	if errEnvelope(t, rec) != proto.CodeUnauthenticated || strings.Contains(rec.Body.String(), "nobody") {
		t.Errorf("unknown user = %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{"empty": "", "bad json": "{", "no password": `{"username":"alice"}`, "no user": `{"password":"x"}`, "huge": strings.Repeat("x", 20000)} {
		rec := f.post("/auth/login", body, "application/json", nil)
		if errEnvelope(t, rec) != proto.CodeInvalidArgument {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.post("/auth/login", "", "", func(r *http.Request) { r.Method = http.MethodGet }); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/login = %d", rec.Code)
	}
}

func TestLoginRateLimitedResponse(t *testing.T) {
	f := newFixture(t)
	var rec *httptest.ResponseRecorder
	for range 20 {
		if rec = f.loginJSON("alice", "bad"); rec.Code == http.StatusTooManyRequests {
			break
		}
	}
	if rec.Code != http.StatusTooManyRequests || errEnvelope(t, rec) != proto.CodeRateLimited {
		t.Fatalf("last response = %d %s", rec.Code, rec.Body)
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if sessionCookie(t, rec) != nil {
		t.Error("a limited response sets a cookie")
	}
	// Each attempt, limited or not, is in the audit log, never with the password.
	for _, ev := range f.audit.events {
		if ev.Action != "login" || strings.Contains(ev.Actor+ev.Target+ev.Outcome, "bad") {
			t.Errorf("event %+v", ev)
		}
	}
}

func TestLoginFailsWhenTheSuccessCannotBeAudited(t *testing.T) {
	f := newFixture(t)
	f.audit.err = errors.New("audit down")
	rec := f.loginJSON("alice", alicePassword)
	if rec.Code != http.StatusInternalServerError || sessionCookie(t, rec) != nil {
		t.Fatalf("login = %d, cookie %v", rec.Code, sessionCookie(t, rec))
	}
	if len(f.sess.rows) != 0 {
		t.Error("the session survived a login that was reported as failed")
	}
	if strings.Contains(rec.Body.String(), "audit down") {
		t.Error("internal detail in the response")
	}
}

func TestLoginTTLFailureIsInternal(t *testing.T) {
	f := newFixture(t)
	f.handler.TTL = func(context.Context) (time.Duration, error) { return 0, errors.New("bad setting") }
	if rec := f.loginJSON("alice", alicePassword); rec.Code != http.StatusInternalServerError {
		t.Errorf("login = %d", rec.Code)
	}
}

// ---- Handler: logout -------------------------------------------------------

func signIn(t *testing.T, f *fixture) (cookie *http.Cookie, csrf string) {
	t.Helper()
	rec := f.loginJSON("alice", alicePassword)
	if rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	var body loginBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return sessionCookie(t, rec), body.CSRFToken
}

func logout(f *fixture, c *http.Cookie, csrf string) *httptest.ResponseRecorder {
	return f.post("/auth/logout", "", "", func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
		if csrf != "" {
			r.Header.Set(session.CSRFHeader, csrf)
		}
	})
}

func TestLogoutDeletesTheSessionAndClearsTheCookie(t *testing.T) {
	f := newFixture(t)
	c, csrf := signIn(t, f)
	rec := logout(f, c, csrf)
	if rec.Code != 200 {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body)
	}
	if len(f.sess.rows) != 0 {
		t.Error("the session row was not deleted")
	}
	if _, err := f.mgr.Resolve(context.Background(), c.Value); !errors.Is(err, session.ErrNoSession) {
		t.Errorf("the session still resolves: %v", err)
	}
	cleared := sessionCookie(t, rec)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", cleared)
	}
	if ev := f.audit.last(t); ev.Action != "logout" || ev.Outcome != OutcomeSuccess || ev.Actor != "u-alice" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestLogoutReportsAFailedDeletion(t *testing.T) {
	f := newFixture(t)
	c, csrf := signIn(t, f)
	f.sess.failDel = errors.New("database is locked")
	rec := logout(f, c, csrf)
	if rec.Code != http.StatusInternalServerError || errEnvelope(t, rec) != proto.CodeInternal {
		t.Fatalf("logout with a failing delete = %d %s; must not report success", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "locked") {
		t.Error("internal detail in the response")
	}
	if sessionCookie(t, rec) != nil {
		t.Error("the cookie was cleared although the server-side session still exists")
	}
	if len(f.sess.rows) != 1 {
		t.Error("the row should still exist")
	}
	if ev := f.audit.last(t); ev.Action != "logout" || ev.Outcome != OutcomeFailed {
		t.Errorf("audit = %+v", ev)
	}
	// A retry once the store works succeeds.
	f.sess.failDel = nil
	if rec := logout(f, c, csrf); rec.Code != 200 || len(f.sess.rows) != 0 {
		t.Errorf("retry = %d, rows %d", rec.Code, len(f.sess.rows))
	}
}

func TestLogoutRequiresCSRF(t *testing.T) {
	f := newFixture(t)
	c, csrf := signIn(t, f)
	for name, tok := range map[string]string{"missing": "", "wrong": "nope", "cookie value": c.Value} {
		rec := logout(f, c, tok)
		if rec.Code != http.StatusForbidden || errEnvelope(t, rec) != proto.CodeForbidden {
			t.Errorf("%s CSRF: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(f.sess.rows) != 1 {
		t.Fatal("a forged logout deleted the session")
	}
	if rec := logout(f, c, csrf); rec.Code != 200 {
		t.Errorf("valid CSRF = %d", rec.Code)
	}
}

func TestLogoutWithoutASession(t *testing.T) {
	f := newFixture(t)
	if rec := logout(f, nil, ""); rec.Code != 200 {
		t.Errorf("logout without a cookie = %d", rec.Code)
	}
	// A cookie for a session that is already gone: signing out is idempotent.
	c, csrf := signIn(t, f)
	f.sess.rows = map[string]store.SessionRow{}
	rec := logout(f, c, csrf)
	if rec.Code != 200 || sessionCookie(t, rec) == nil || sessionCookie(t, rec).MaxAge >= 0 {
		t.Errorf("logout of a stale session = %d %v", rec.Code, rec.Header())
	}
}

// ---- timing, time format, cross-origin -------------------------------------

func TestOverlongPasswordKnownAndUnknownAccountDoTheSameWork(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("p", 2000)
	for _, ident := range []string{"alice", "nobody"} {
		before := f.auth.Hasher.computations.Load()
		if _, err := f.login(ident, long); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%s: err = %v", ident, err)
		}
		if n := f.auth.Hasher.computations.Load() - before; n != 1 {
			t.Errorf("%s: %d Argon2id computations for an over-long password, want exactly 1", ident, n)
		}
	}
}

func TestLoginExpiryIsRFC3339WithMilliseconds(t *testing.T) {
	f := newFixture(t) // the fake clock sits on a whole second
	rec := f.loginJSON("alice", alicePassword)
	if rec.Code != 200 {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	var raw struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`\.\d{3}Z$`).MatchString(raw.ExpiresAt) {
		t.Errorf("expires_at = %q, want RFC 3339 UTC with milliseconds", raw.ExpiresAt)
	}
	if want := "2026-11-09T11:00:00.000Z"; raw.ExpiresAt != want {
		t.Errorf("expires_at = %q, want %q", raw.ExpiresAt, want)
	}
}

func TestCrossOriginLoginIsForbiddenAndSetsNoCookie(t *testing.T) {
	body := `{"username":"alice","password":"` + alicePassword + `"}`
	form := "username=alice&password=" + url.QueryEscape(alicePassword)
	cases := map[string]struct {
		ctype, body string
		mod         func(*http.Request)
	}{
		"Sec-Fetch-Site cross-site":     {"application/json", body, withHeader("Sec-Fetch-Site", "cross-site")},
		"Sec-Fetch-Site same-site":      {"application/json", body, withHeader("Sec-Fetch-Site", "same-site")},
		"foreign Origin":                {"application/json", body, withHeader("Origin", "https://evil.example")},
		"cross-site form":               {"application/x-www-form-urlencoded", form, withHeader("Sec-Fetch-Site", "cross-site")},
		"cross-site text/plain as JSON": {"text/plain", body, withHeader("Sec-Fetch-Site", "cross-site")},
	}
	for name, c := range cases {
		f := newFixture(t)
		rec := f.post("/auth/login", c.body, c.ctype, c.mod)
		if rec.Code != http.StatusForbidden || errEnvelope(t, rec) != proto.CodeForbidden {
			t.Errorf("%s: login = %d %s, want 403 forbidden", name, rec.Code, rec.Body)
		}
		if sessionCookie(t, rec) != nil || len(f.sess.rows) != 0 {
			t.Errorf("%s: a session was issued", name)
		}
		if f.audit.count() != 0 {
			t.Errorf("%s: the refused request reached the authenticator", name)
		}
	}
}

func TestSameOriginAndNonBrowserLoginsSucceed(t *testing.T) {
	for name, mod := range map[string]func(*http.Request){
		"no browser headers":         nil,
		"Sec-Fetch-Site same-origin": withHeader("Sec-Fetch-Site", "same-origin"),
		"Sec-Fetch-Site none":        withHeader("Sec-Fetch-Site", "none"),
		"Origin equals Host":         func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) },
	} {
		f := newFixture(t)
		b, _ := json.Marshal(map[string]string{"username": "alice", "password": alicePassword})
		rec := f.post("/auth/login", string(b), "application/json", mod)
		if rec.Code != 200 || sessionCookie(t, rec) == nil {
			t.Errorf("%s: login = %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestTrustedOriginMakesAProxiedLoginSameOrigin(t *testing.T) {
	f := newFixture(t)
	f.mux = http.NewServeMux()
	if err := f.handler.Register(f.mux, f.mw, "https://hub.example.com"); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"username": "alice", "password": alicePassword})
	rec := f.post("/auth/login", string(b), "application/json", withHeader("Origin", "https://hub.example.com"))
	if rec.Code != 200 {
		t.Errorf("login from the trusted origin = %d %s", rec.Code, rec.Body)
	}
	if err := f.handler.Register(http.NewServeMux(), f.mw, "not an origin"); err == nil {
		t.Error("an invalid trusted origin was accepted")
	}
}

func TestCrossOriginLogoutIsForbidden(t *testing.T) {
	f := newFixture(t)
	c, csrf := signIn(t, f)
	rec := f.post("/auth/logout", "", "", func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set(session.CSRFHeader, csrf)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden || errEnvelope(t, rec) != proto.CodeForbidden {
		t.Errorf("cross-site logout = %d %s", rec.Code, rec.Body)
	}
	if len(f.sess.rows) != 1 {
		t.Error("a cross-site logout deleted the session")
	}
}
