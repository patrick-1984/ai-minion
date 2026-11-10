package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

const goodKey = "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"

type fakeSessions struct {
	sess *session.Session
	err  error
	got  string
}

func (f *fakeSessions) Resolve(_ context.Context, token string) (*session.Session, error) {
	f.got = token
	return f.sess, f.err
}

type fakeKeys struct {
	mu    sync.Mutex
	known map[string]KeyIdentity
	err   error
	calls int
}

func (f *fakeKeys) AuthenticateKey(_ context.Context, token string) (KeyIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return KeyIdentity{}, f.err
	}
	if ki, ok := f.known[token]; ok {
		return ki, nil
	}
	return KeyIdentity{}, ErrInvalidKey
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type authRig struct {
	a        *Authenticator
	sessions *fakeSessions
	keys     *fakeKeys
	audit    *fakeAudit
	clock    *fakeClock
	logs     *strings.Builder
}

type allScope struct{}

func (allScope) Actions(authz.ResolvedResource) []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionOperate}
}

func newAuthRig(t *testing.T) *authRig {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	lim, err := ratelimit.New(ratelimit.Config{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	r := &authRig{
		sessions: &fakeSessions{}, keys: &fakeKeys{known: map[string]KeyIdentity{}},
		audit: &fakeAudit{}, clock: clock, logs: &strings.Builder{},
	}
	r.a = &Authenticator{
		Sessions: r.sessions, Limiter: lim, Audit: r.audit, Now: clock.Now, Log: testLogger(r.logs),
		ClientIP: func(*http.Request) netip.Addr { return netip.MustParseAddr("198.51.100.4") },
	}
	if err := r.a.SetKeyAuthenticator(r.keys); err != nil {
		t.Fatal(err)
	}
	return r
}

func keyIdentity() KeyIdentity {
	return KeyIdentity{KeyID: "key-1", Principal: authz.NewAPIKeyPrincipal("owner-1", allScope{}, "")}
}

func bearerReq(method, token string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/ops/x", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func sessionCookieReq(method string, token string, csrf string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/ops/x", nil)
	r.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	if csrf != "" {
		r.Header.Set(session.CSRFHeader, csrf)
	}
	return r
}

// A real cookie value: 32 bytes, base64url without padding.
const cookieToken = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func wantCode(t *testing.T, err error, code proto.Code) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	return e
}

// requireCode is wantCode for call sites that ignore the error.
func requireCode(t *testing.T, err error, code proto.Code) {
	t.Helper()
	_ = wantCode(t, err, code)
}

func TestNoCredentialsIsUnauthenticated(t *testing.T) {
	r := newAuthRig(t)
	_, err := r.a.Authenticate(httptest.NewRequest("GET", "/api/v1/ops/x", nil))
	requireCode(t, err, proto.CodeUnauthenticated)
}

func TestCookieSafeMethodNeedsNoCSRF(t *testing.T) {
	r := newAuthRig(t)
	r.sessions.sess = &session.Session{ID: "s1", UserID: "user-1", Role: authz.RoleUser}
	id, err := r.a.Authenticate(sessionCookieReq("GET", cookieToken, ""))
	if err != nil {
		t.Fatal(err)
	}
	if id.Via != ViaSession || id.Principal.Kind != authz.KindBrowserSession || id.Principal.UserID != "user-1" ||
		id.Actor != "user-1" || id.IP != netip.MustParseAddr("198.51.100.4") || id.Session == nil {
		t.Errorf("identity = %+v", id)
	}
	if r.sessions.got != cookieToken {
		t.Error("the cookie value was not resolved")
	}
}

func TestCookieUnsafeMethodsNeedTheBoundCSRFToken(t *testing.T) {
	r := newAuthRig(t)
	r.sessions.sess = &session.Session{UserID: "user-1"}
	good := session.CSRFToken(cookieToken)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if _, err := r.a.Authenticate(sessionCookieReq(m, cookieToken, good)); err != nil {
			t.Errorf("%s with the right CSRF token: %v", m, err)
		}
		for name, csrf := range map[string]string{"missing": "", "forged": "forged", "other session": session.CSRFToken("BAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")} {
			_, err := r.a.Authenticate(sessionCookieReq(m, cookieToken, csrf))
			if e := ErrorOf(err); e.Code != proto.CodeForbidden {
				t.Errorf("%s with a %s CSRF token: error = %v, want forbidden", m, name, err)
			}
		}
	}
}

func TestCSRFIsCheckedBeforeTheSessionLookup(t *testing.T) {
	r := newAuthRig(t)
	r.sessions.err = session.ErrNoSession
	// A stale cookie on a POST without the header is refused as forbidden,
	// exactly like session.Middleware does.
	_, err := r.a.Authenticate(sessionCookieReq("POST", cookieToken, ""))
	requireCode(t, err, proto.CodeForbidden)
	if r.sessions.got != "" {
		t.Error("the session was looked up before the CSRF check")
	}
	// With the header, a stale session is unauthenticated.
	_, err = r.a.Authenticate(sessionCookieReq("POST", cookieToken, session.CSRFToken(cookieToken)))
	requireCode(t, err, proto.CodeUnauthenticated)
}

func TestSessionStoreFailureIsInternalNotAnonymous(t *testing.T) {
	r := newAuthRig(t)
	r.sessions.err = errors.New("db down")
	_, err := r.a.Authenticate(sessionCookieReq("GET", cookieToken, ""))
	requireCode(t, err, proto.CodeInternal)
	if !strings.Contains(r.logs.String(), "session lookup failed") {
		t.Errorf("logs = %s", r.logs)
	}
}

func TestBearerKeyAuthenticates(t *testing.T) {
	r := newAuthRig(t)
	r.keys.known[goodKey] = keyIdentity()
	// A bearer request needs no CSRF header: there is no ambient credential.
	id, err := r.a.Authenticate(bearerReq("POST", goodKey))
	if err != nil {
		t.Fatal(err)
	}
	if id.Via != ViaAPIKey || id.Principal.Kind != authz.KindAPIKey || id.Principal.UserID != "owner-1" || id.Actor != "key:key-1" {
		t.Errorf("identity = %+v", id)
	}
	if len(r.audit.all()) != 0 {
		t.Errorf("a good key was audited as a failure: %+v", r.audit.all())
	}
	// The scheme is case-insensitive.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "bearer "+goodKey)
	if _, err := r.a.Authenticate(req); err != nil {
		t.Errorf("lower-case scheme: %v", err)
	}
}

func TestBearerBeatsACookie(t *testing.T) {
	r := newAuthRig(t)
	r.sessions.sess = &session.Session{UserID: "user-1"}
	req := bearerReq("GET", "mn_wrong")
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: cookieToken})
	_, err := r.a.Authenticate(req)
	requireCode(t, err, proto.CodeUnauthenticated)
	if r.sessions.got != "" {
		t.Error("a stray cookie was consulted next to an Authorization header")
	}
}

func TestMalformedAuthorizationHeaders(t *testing.T) {
	r := newAuthRig(t)
	for name, vals := range map[string][]string{
		"basic":    {"Basic dXNlcjpwdw=="},
		"no token": {"Bearer"},
		"two":      {"Bearer " + goodKey, "Bearer " + goodKey},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header["Authorization"] = vals
		_, err := r.a.Authenticate(req)
		if e := wantCode(t, err, proto.CodeUnauthenticated); !e.challenge {
			t.Errorf("%s: no challenge", name)
		}
	}
	if r.keys.calls != 0 {
		t.Error("the key store was asked about a malformed header")
	}
}

func TestFailedBearerIsAuditedWithoutTheTokenAndCountedPerIP(t *testing.T) {
	r := newAuthRig(t)
	secret := "mn_THISISTHESECRETPARTOFTHEKEY0123456789abcdefghijk"
	_, err := r.a.Authenticate(bearerReq("GET", secret))
	e := wantCode(t, err, proto.CodeUnauthenticated)
	ev := r.audit.all()
	if len(ev) != 1 {
		t.Fatalf("audit = %+v", ev)
	}
	if ev[0].Action != AuditActionKeyAuth || ev[0].Outcome != AuditOutcomeFailed || ev[0].Actor != "anonymous" || ev[0].Target != "198.51.100.4" {
		t.Errorf("event = %+v", ev[0])
	}
	blob := ev[0].Actor + ev[0].Action + ev[0].Target + ev[0].Outcome + e.Message + r.logs.String()
	if strings.Contains(blob, "SECRET") || strings.Contains(blob, secret[3:12]) {
		t.Errorf("the token leaked into the audit event, the error or the log: %s", blob)
	}
}

func TestApikeyFailLimiterHitPerIP(t *testing.T) {
	r := newAuthRig(t)
	r.keys.known[goodKey] = keyIdentity()
	burst := ratelimit.DefaultPolicies()[ratelimit.PolicyAPIKeyFail].IP.Burst
	for i := 0; i < burst; i++ {
		_, err := r.a.Authenticate(bearerReq("GET", "mn_bad"))
		requireCode(t, err, proto.CodeUnauthenticated)
	}
	// The next failure exhausts the budget: rate_limited with Retry-After.
	_, err := r.a.Authenticate(bearerReq("GET", "mn_bad"))
	e := wantCode(t, err, proto.CodeRateLimited)
	if e.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v", e.RetryAfter)
	}
	last := r.audit.all()[burst]
	if last.Outcome != AuditOutcomeLimited || last.Action != AuditActionKeyAuth {
		t.Errorf("last event = %+v", last)
	}
	// The blocked client is now refused even with a good key, before the
	// token is looked at, and the attempt costs no audit row.
	calls, events := r.keys.calls, len(r.audit.all())
	_, err = r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeRateLimited)
	if r.keys.calls != calls || len(r.audit.all()) != events {
		t.Error("a blocked client reached the key store or the audit log")
	}
	// Another address is unaffected.
	r.a.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("203.0.113.9") }
	if _, err := r.a.Authenticate(bearerReq("GET", goodKey)); err != nil {
		t.Errorf("another IP was limited: %v", err)
	}
	// After the wait the first client may try again.
	r.a.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("198.51.100.4") }
	r.clock.advance(e.RetryAfter + time.Second)
	if _, err := r.a.Authenticate(bearerReq("GET", goodKey)); err != nil {
		t.Errorf("after the wait: %v", err)
	}
}

func TestSuccessfulKeysDoNotUseUpTheFailureBudget(t *testing.T) {
	r := newAuthRig(t)
	r.keys.known[goodKey] = keyIdentity()
	for i := 0; i < 50; i++ {
		if _, err := r.a.Authenticate(bearerReq("GET", goodKey)); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}

func TestBearerWithoutKeyModuleFailsClosed(t *testing.T) {
	r := newAuthRig(t)
	r.a.keys.Store(nil)
	_, err := r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeUnauthenticated)
	if len(r.audit.all()) != 1 {
		t.Errorf("audit = %+v", r.audit.all())
	}
}

func TestTokensWithoutThePrefixOrTooLongNeverReachTheStore(t *testing.T) {
	r := newAuthRig(t)
	for _, tok := range []string{"eyJhbGciOiJIUzI1NiJ9.x.y", "", "mn_" + strings.Repeat("a", 200)} {
		_, err := r.a.Authenticate(bearerReq("GET", tok))
		requireCode(t, err, proto.CodeUnauthenticated)
	}
	if r.keys.calls != 0 {
		t.Errorf("the store saw %d malformed tokens", r.keys.calls)
	}
}

func TestKeyStoreFailureIsInternalAndNotCounted(t *testing.T) {
	r := newAuthRig(t)
	r.keys.err = errors.New("db down")
	_, err := r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeInternal)
	if len(r.audit.all()) != 0 || !strings.Contains(r.logs.String(), "api key lookup failed") {
		t.Errorf("audit = %+v logs = %s", r.audit.all(), r.logs)
	}
	if strings.Contains(r.logs.String(), goodKey) {
		t.Error("the token reached the log")
	}
}

func TestWorkerBoundKeyIsRefusedOnREST(t *testing.T) {
	r := newAuthRig(t)
	r.keys.known[goodKey] = KeyIdentity{KeyID: "k", Principal: authz.NewAPIKeyPrincipal("owner-1", allScope{}, "worker-1")}
	_, err := r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeForbidden)
}

func TestKeyPortReturningAnotherPrincipalKindFailsClosed(t *testing.T) {
	r := newAuthRig(t)
	r.keys.known[goodKey] = KeyIdentity{Principal: authz.NewSessionPrincipal("u")}
	_, err := r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeInternal)
	r.keys.known[goodKey] = KeyIdentity{Principal: authz.NewWorkerPrincipal("w")}
	_, err = r.a.Authenticate(bearerReq("GET", goodKey))
	requireCode(t, err, proto.CodeInternal)
}

func TestActAsIdentityAuditsBothUsers(t *testing.T) {
	p := authz.NewActAsPrincipal("admin-1", "user-9", time.Now().Add(time.Hour))
	id := identityOf(p, ViaSession, p.UserID, nil)
	if id.Actor != "admin-1" || id.ActingAs != "user-9" {
		t.Errorf("identity = %+v", id)
	}
}

func TestSetKeyAuthenticatorOnce(t *testing.T) {
	a := &Authenticator{}
	if err := a.SetKeyAuthenticator(nil); err == nil {
		t.Error("nil accepted")
	}
	if err := a.SetKeyAuthenticator(&fakeKeys{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetKeyAuthenticator(&fakeKeys{}); err == nil {
		t.Error("a second authenticator was accepted")
	}
}

func TestDefaultClientIPIsThePeer(t *testing.T) {
	a := &Authenticator{}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "[2001:db8::5]:4000"
	if got := a.clientIP(req); got != netip.MustParseAddr("2001:db8::5") {
		t.Errorf("ip = %v", got)
	}
	req.RemoteAddr = "garbage"
	if a.clientIP(req).IsValid() {
		t.Error("garbage produced an address")
	}
}

func TestBlocklistIsBoundedAndExpires(t *testing.T) {
	var b blocklist
	now := time.Unix(1000, 0)
	for i := 0; i < maxBlocked+10; i++ {
		b.block(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), now, time.Minute)
	}
	b.mu.Lock()
	n := len(b.until)
	b.mu.Unlock()
	if n > maxBlocked {
		t.Errorf("blocklist grew to %d entries", n)
	}
	ip := netip.MustParseAddr("192.0.2.1")
	b.block(ip, now, 5*time.Second)
	if w := b.wait(ip, now.Add(time.Second)); w != 4*time.Second {
		t.Errorf("wait = %v", w)
	}
	if w := b.wait(ip, now.Add(6*time.Second)); w != 0 {
		t.Errorf("wait after expiry = %v", w)
	}
}

// The Authenticator is what the Dispatcher runs: an unauthenticated request
// to a catalog route is a 401 with the challenge header and no handler call.
func TestAuthenticatorBehindTheDispatcher(t *testing.T) {
	r := newRig(t)
	ar := newAuthRig(t)
	r.d.Auth = ar.a
	mustHandle(t, r.reg, opPing, echoHandler)
	w := r.do("POST", "/api/v1/ops/ping_read", `{}`)
	if errCode(t, w) != proto.CodeUnauthenticated || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	ar.keys.known[goodKey] = keyIdentity()
	req := httptest.NewRequest("POST", "/api/v1/ops/ping_read", strings.NewReader(`{"message":"k"}`))
	req.Header.Set("Authorization", "Bearer "+goodKey)
	w = httptest.NewRecorder()
	r.mux.ServeHTTP(w, req)
	if w.Code != 200 || r.decide.calls[0].p.Kind != authz.KindAPIKey {
		t.Errorf("%d %s calls=%+v", w.Code, w.Body, r.decide.calls)
	}
}
