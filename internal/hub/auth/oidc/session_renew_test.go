package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// renewRig is the session middleware of a Hub, with the refresher as its
// renewer, in front of a page that says who is signed in. The clock of the
// session manager is movable; the session of the sign-in lasts one hour.
type renewRig struct {
	*refreshHub
	mu  sync.Mutex
	at  time.Time
	mgr *session.Manager
	mw  *session.Middleware
	h   http.Handler
}

func newRenewRig(t *testing.T) *renewRig {
	t.Helper()
	rh := newRefreshHub(t)
	g := &renewRig{refreshHub: rh, at: time.Now()}
	g.mgr = session.NewManager(rh.st, session.WithClock(g.now),
		session.WithIdleTTL(func(context.Context) (time.Duration, error) { return time.Hour, nil }))
	g.mw = &session.Middleware{Manager: g.mgr, Renew: rh.r, Log: rh.r.Log}
	g.h = g.mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, ok := session.FromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("signed out"))
			return
		}
		_, _ = w.Write([]byte("page of " + s.Username))
	}))
	return g
}

func (g *renewRig) now() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.at }

func (g *renewRig) advance(d time.Duration) { g.mu.Lock(); g.at = g.at.Add(d); g.mu.Unlock() }

// expiresAt returns the stored idle end of the sign-in's session, and false
// when the row is gone.
func (g *renewRig) expiresAt() (time.Time, bool) {
	g.t.Helper()
	hash, _ := session.HashToken(g.token)
	var ms int64
	if err := g.st.DB().QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, hash).Scan(&ms); err != nil {
		return time.Time{}, false
	}
	return store.FromUnixMS(ms), true
}

type req struct {
	method string
	hx     bool
	bg     bool
	sse    bool
}

func (g *renewRig) do(q req) *httptest.ResponseRecorder {
	g.t.Helper()
	if q.method == "" {
		q.method = http.MethodGet
	}
	r := httptest.NewRequest(q.method, "/sessions", nil)
	r.AddCookie(&http.Cookie{Name: session.CookieName, Value: g.token})
	if q.method != http.MethodGet {
		r.Header.Set(session.CSRFHeader, session.CSRFToken(g.token))
	}
	if q.hx {
		r.Header.Set("HX-Request", "true")
	}
	if q.bg {
		r.Header.Set(session.BackgroundHeader, "1")
	}
	if q.sse {
		r.Header.Set("Accept", "text/event-stream")
	}
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, r)
	return rec
}

func (g *renewRig) renewAudits(outcome string) int {
	n := 0
	for _, e := range g.aud.all() {
		if e.Action == AuditActionSessionRenew && e.Outcome == outcome {
			n++
		}
	}
	return n
}

func served(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusOK && strings.HasPrefix(rec.Body.String(), "page of alice")
}

func TestExpiredOIDCSessionIsRenewedOnPageLoad(t *testing.T) {
	g := newRenewRig(t)
	g.advance(90 * time.Minute) // the idle hour is over
	hits := g.rs.hitCount()

	rec := g.do(req{})
	if !served(rec) {
		t.Fatalf("page load after the idle end = %d %q, want the page", rec.Code, rec.Body)
	}
	if g.rs.hitCount() != hits+1 {
		t.Errorf("token endpoint hits = %d, want one refresh", g.rs.hitCount()-hits)
	}
	if end, ok := g.expiresAt(); !ok || !end.Equal(g.now().Add(time.Hour).Truncate(time.Millisecond)) {
		t.Errorf("idle end after the renewal = %v (row %v), want now + 1h", end, ok)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("the renewal changed the cookie: %q", rec.Header().Get("Set-Cookie"))
	}
	if g.renewAudits(OutcomeSuccess) != 1 {
		t.Errorf("audit rows: %+v", g.aud.all())
	}
	// The next request is a plain valid session: no second refresh.
	if rec := g.do(req{}); !served(rec) || g.rs.hitCount() != hits+1 {
		t.Errorf("second request = %d, hits %d", rec.Code, g.rs.hitCount()-hits)
	}
}

func TestExpiredOIDCSessionIsRenewedOnHtmxRequest(t *testing.T) {
	g := newRenewRig(t)
	g.advance(2 * time.Hour)
	for _, q := range []req{{hx: true}, {method: http.MethodPost, hx: true}} {
		if rec := g.do(q); !served(rec) {
			t.Fatalf("%+v after the idle end = %d %q, want it served", q, rec.Code, rec.Body)
		}
		if rec := g.do(q); rec.Header().Get("HX-Redirect") != "" {
			t.Fatalf("htmx request redirected: %v", rec.Header())
		}
		g.advance(2 * time.Hour)
	}
	if g.renewAudits(OutcomeSuccess) != 2 {
		t.Errorf("want two renewals: %+v", g.aud.all())
	}
}

func TestRenewalStoresARotatedToken(t *testing.T) {
	g := newRenewRig(t)
	g.rs.set(func(r *refreshIdP) { r.rotate = "rt-2" })
	g.advance(90 * time.Minute)
	if rec := g.do(req{}); !served(rec) {
		t.Fatalf("= %d", rec.Code)
	}
	if got := g.storedToken(); got != "rt-2" {
		t.Errorf("stored token = %q, want the rotated one", got)
	}
}

func TestActivityExtendsTheIdleLifetimeButBackgroundDoesNot(t *testing.T) {
	g := newRenewRig(t)
	start, _ := g.expiresAt()
	g.advance(30 * time.Minute)
	g.do(req{bg: true})
	g.do(req{sse: true})
	if end, _ := g.expiresAt(); !end.Equal(start) {
		t.Fatalf("a background request moved the end: %v -> %v", start, end)
	}
	g.do(req{hx: true})
	if end, _ := g.expiresAt(); !end.Equal(g.now().Add(time.Hour).Truncate(time.Millisecond)) {
		t.Fatalf("activity did not move the end to now + 1h: %v", end)
	}
	if g.rs.hitCount() != 0 {
		t.Errorf("a valid session asked the IdP %d times", g.rs.hitCount())
	}
}

// A background request of an expired OIDC session renews it (the renewal
// is a check with the IdP and keeps the screen), but a background request of
// a valid session does not extend it.
func TestBackgroundRequestRenewsAnExpiredSession(t *testing.T) {
	g := newRenewRig(t)
	g.advance(90 * time.Minute)
	if rec := g.do(req{bg: true, hx: true}); !served(rec) {
		t.Fatalf("background request after the idle end = %d", rec.Code)
	}
	if g.rs.hitCount() != 1 || g.renewAudits(OutcomeSuccess) != 1 {
		t.Errorf("hits %d, audits %+v", g.rs.hitCount(), g.aud.all())
	}
	end, _ := g.expiresAt()
	g.advance(30 * time.Minute)
	g.do(req{sse: true})
	if e2, _ := g.expiresAt(); !e2.Equal(end) {
		t.Errorf("a stream reconnect extended the session: %v -> %v", end, e2)
	}
}

func TestRejectedRefreshSignsOut(t *testing.T) {
	g := newRenewRig(t)
	g.rs.set(func(r *refreshIdP) { r.mode = "reject" })
	g.advance(90 * time.Minute)
	if rec := g.do(req{}); served(rec) {
		t.Fatal("served after the IdP rejected the refresh token")
	}
	if _, ok := g.expiresAt(); ok {
		t.Error("the session row is still there")
	}
	if g.storedToken() != "" {
		t.Error("the rejected token is still stored")
	}
	if g.renewAudits(OutcomeFailed) != 1 || !g.auditHas(AuditActionRoleSuspend, OutcomeSuccess) {
		t.Errorf("audit rows: %+v", g.aud.all())
	}
}

func TestRenewalWithoutARoleSignsOut(t *testing.T) {
	g := newRenewRig(t)
	g.groups("strangers")
	g.advance(90 * time.Minute)
	if rec := g.do(req{}); served(rec) {
		t.Fatal("served although no rule gives a role any more")
	}
	if g.renewAudits(OutcomeFailed) != 1 {
		t.Errorf("audit rows: %+v", g.aud.all())
	}
}

func TestDeactivatedUserIsNotRenewed(t *testing.T) {
	g := newRenewRig(t)
	if _, err := g.st.DB().Exec(`UPDATE users SET active = 0 WHERE id = ?`, g.userID); err != nil {
		t.Fatal(err)
	}
	g.advance(90 * time.Minute)
	if rec := g.do(req{}); served(rec) {
		t.Fatal("a deactivated user was served")
	}
	if g.rs.hitCount() != 0 {
		t.Error("the IdP was asked for a deactivated user")
	}
}

func TestUnreachableIdPIsToleratedOnlyWithinTheGrace(t *testing.T) {
	g := newRenewRig(t)
	g.rs.set(func(r *refreshIdP) { r.mode = "down" })
	g.advance(time.Hour + 5*time.Minute) // 5 minutes after the idle end
	if rec := g.do(req{}); !served(rec) {
		t.Fatalf("within the grace = %d, want the page", rec.Code)
	}
	if end, _ := g.expiresAt(); !end.Before(g.now()) {
		t.Error("an unreachable IdP extended the session")
	}
	hits := g.rs.hitCount()
	if rec := g.do(req{}); !served(rec) || g.rs.hitCount() != hits {
		t.Errorf("a retry within the back-off asked the IdP again (%d hits)", g.rs.hitCount()-hits)
	}
	if g.renewAudits(OutcomeRenewUnavailable) != 1 {
		t.Errorf("audit rows: %+v", g.aud.all())
	}
	g.advance(session.RenewGrace) // past the grace
	if rec := g.do(req{}); served(rec) {
		t.Fatal("served past the grace while the IdP is down")
	}
	// Once the IdP answers again the next request renews.
	g.rs.set(func(r *refreshIdP) { r.mode = "ok" })
	if rec := g.do(req{}); !served(rec) {
		t.Fatalf("after the IdP came back = %d", rec.Code)
	}
}

func TestConcurrentRequestsRenewOnce(t *testing.T) {
	g := newRenewRig(t)
	g.rs.set(func(r *refreshIdP) { r.rotate = "rt-2"; r.delay = 50 * time.Millisecond })
	g.advance(90 * time.Minute)
	var wg sync.WaitGroup
	results := make([]bool, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = served(g.do(req{hx: true}))
		}()
	}
	wg.Wait()
	for i, ok := range results {
		if !ok {
			t.Errorf("request %d was not served", i)
		}
	}
	if g.rs.hitCount() != 1 {
		t.Errorf("token endpoint hits = %d, want one", g.rs.hitCount())
	}
}

func TestSessionPastItsAbsoluteEndIsNotRenewed(t *testing.T) {
	g := newRenewRig(t)
	g.advance(session.OIDCMaxAge + time.Minute)
	if rec := g.do(req{}); served(rec) {
		t.Fatal("served past the absolute end")
	}
	if g.rs.hitCount() != 0 {
		t.Error("the IdP was asked past the absolute end")
	}
}

func TestRenewalNeverExposesTheRefreshToken(t *testing.T) {
	g := newRenewRig(t)
	g.rs.set(func(r *refreshIdP) { r.rotate = "rt-2-secret" })
	g.advance(90 * time.Minute)
	rec := g.do(req{})
	g.rs.set(func(r *refreshIdP) { r.mode = "reject" })
	g.advance(90 * time.Minute)
	rec2 := g.do(req{hx: true})
	for _, secret := range []string{"rt-1", "rt-2-secret"} {
		for _, r := range []*httptest.ResponseRecorder{rec, rec2} {
			if strings.Contains(r.Body.String(), secret) || strings.Contains(strings.Join(r.Header().Values("Set-Cookie"), ";"), secret) {
				t.Errorf("%q reached a response", secret)
			}
		}
		if strings.Contains(g.rlogs.String(), secret) {
			t.Errorf("%q reached the log", secret)
		}
		for _, e := range g.aud.all() {
			if strings.Contains(e.Actor+e.Action+e.Target+e.Outcome, secret) {
				t.Errorf("%q reached the audit: %+v", secret, e)
			}
		}
	}
}

// The REST API resolves the cookie through the middleware, so a JSON form
// post on an expired OIDC session is renewed as a page load is.
func TestResolveRequestRenewsForTheRESTAPI(t *testing.T) {
	g := newRenewRig(t)
	g.advance(90 * time.Minute)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/ops/password_change", nil)
	s, err := g.mw.ResolveRequest(r, g.token)
	if err != nil || s.Username != "alice" {
		t.Fatalf("ResolveRequest = %+v, %v", s, err)
	}
	if g.renewAudits(OutcomeSuccess) != 1 {
		t.Errorf("audit rows: %+v", g.aud.all())
	}
}
