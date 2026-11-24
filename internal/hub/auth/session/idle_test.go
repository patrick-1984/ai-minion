package session

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// idleRig serves a handler behind the middleware that answers 200 with a
// session, and 401 without one; the last request's [Idle] record is in *seen.
func idleRig(t *testing.T, method string) (*Manager, *fakeClock, http.Handler, Issued, *IdleSession) {
	t.Helper()
	ms := newMemStore(alice)
	clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	m := NewManager(ms, WithClock(clk.Now), WithIdleTTL(func(context.Context) (time.Duration, error) { return 2 * time.Hour, nil }))
	is, err := m.CreateAuth(context.Background(), alice, 2*time.Hour, Auth{At: clk.Now(), Method: method})
	if err != nil {
		t.Fatal(err)
	}
	seen := &IdleSession{}
	mw := &Middleware{Manager: m, Origin: func(*http.Request) string { return "https://hub.example" }}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = IdleSession{}
		if v, ok := Idle(r.Context()); ok {
			*seen = v
			seen.Method = "idle:" + v.Method
		}
		if _, ok := FromContext(r.Context()); !ok {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	return m, clk, h, is, seen
}

// kinds of request of the tests
const (
	fgNav   = iota // the person loads a page (Fetch Metadata same-origin, document)
	fgFetch        // the person clicks a button (htmx, same-origin)
	bgPoll         // a poll of the page (background header)
	xsNav          // a page load from another site
)

func send(h http.Handler, token string, kind int) int {
	r := httptest.NewRequest(http.MethodGet, "https://hub.example/sessions", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	switch kind {
	case fgNav:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Dest", "document")
	case fgFetch:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Sec-Fetch-Dest", "empty")
		r.Header.Set("HX-Request", "true")
	case bgPoll:
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set(BackgroundHeader, "1")
	case xsNav:
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Dest", "document")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestDefaultIdleLifetimeIsTwoHours(t *testing.T) {
	m := NewManager(newMemStore(alice))
	if got := m.IdleTTL(context.Background()); got != 2*time.Hour || config.DefaultSessionTTL != 2*time.Hour {
		t.Fatalf("IdleTTL = %v, want 2h", got)
	}
}

func TestLocalSessionSlidesWithActivityAndEndsWhenIdle(t *testing.T) {
	_, clk, h, is, seen := idleRig(t, AuthMethodLocal)
	// Active every 90 minutes: the session lives on past the first 2 h.
	for i := range 4 {
		clk.Add(90 * time.Minute)
		if code := send(h, is.Token, []int{fgNav, fgFetch}[i%2]); code != http.StatusOK {
			t.Fatalf("active session at %v = %d", clk.Now(), code)
		}
	}
	// Background polls and page loads from other sites do not keep it alive.
	clk.Add(60 * time.Minute)
	send(h, is.Token, bgPoll)
	send(h, is.Token, xsNav)
	clk.Add(61 * time.Minute)
	if code := send(h, is.Token, fgNav); code != http.StatusUnauthorized {
		t.Fatalf("idle local session = %d, want signed out", code)
	}
	// It is marked idle, and a local session is never re-authenticated
	// silently: the page load leads to the sign-in page.
	if *seen != (IdleSession{Method: "idle:" + AuthMethodLocal}) {
		t.Errorf("idle record = %+v", *seen)
	}
}

func TestLocalSessionEndsAtItsAbsoluteEnd(t *testing.T) {
	_, clk, h, is, seen := idleRig(t, AuthMethodLocal)
	for clk.Now().Before(is.CreatedAt.Add(LocalMaxAge - time.Hour)) {
		clk.Add(time.Hour)
		if code := send(h, is.Token, fgNav); code != http.StatusOK {
			t.Fatalf("active session at %v = %d", clk.Now(), code)
		}
	}
	clk.Add(time.Hour)
	if code := send(h, is.Token, fgNav); code != http.StatusUnauthorized {
		t.Fatalf("local session past %v = %d, want signed out", LocalMaxAge, code)
	}
	if seen.Method != "" {
		t.Errorf("a session past its absolute end is marked idle: %+v", *seen)
	}
}

// An expired OIDC session is never renewed: nothing serves the request with
// it. Only a foreground top-level page load is marked for the silent
// re-authentication with its provider; htmx, polls and page loads from other
// sites are not.
func TestExpiredOIDCSessionIsMarkedForSilentReauthOnlyOnAForegroundPageLoad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   int
		reauth string
	}{
		{"page load", fgNav, "authentik"},
		{"htmx click", fgFetch, ""},
		{"poll", bgPoll, ""},
		{"page load from another site", xsNav, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, clk, h, is, seen := idleRig(t, "oidc:authentik")
			clk.Add(2*time.Hour + time.Minute)
			if code := send(h, is.Token, tc.kind); code != http.StatusUnauthorized {
				t.Fatalf("expired session served: %d", code)
			}
			if want := (IdleSession{Method: "idle:oidc:authentik", Reauth: tc.reauth}); *seen != want {
				t.Errorf("idle record = %+v, want %+v", *seen, want)
			}
			// Still expired: no request renewed it.
			if _, err := m.Resolve(context.Background(), is.Token); err == nil {
				t.Error("the expired session resolves again")
			}
		})
	}
}

func TestCookieOfAnOIDCSessionOutlivesTheIdleEnd(t *testing.T) {
	_, _, _, is, _ := idleRig(t, "oidc:authentik")
	if c := Cookie(is, true); !c.Expires.Equal(is.CreatedAt.Add(OIDCMaxAge)) {
		t.Fatalf("cookie expires %v, want %v", c.Expires, is.CreatedAt.Add(OIDCMaxAge))
	}
}

// The first idle end is clamped to the longest idle lifetime too, not only
// to the cap: a session_ttl of 24 h issues an OIDC session that is idle after
// 12 h even when it is first used after that.
func TestFirstIdleEndIsClampedToMaxIdle(t *testing.T) {
	for _, method := range []string{AuthMethodLocal, "oidc:authentik"} {
		ms := newMemStore(alice)
		clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
		m := NewManager(ms, WithClock(clk.Now), WithIdleTTL(func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }))
		is, err := m.CreateAuth(context.Background(), alice, 24*time.Hour, Auth{At: clk.Now(), Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if want := is.CreatedAt.Add(MaxIdle); !is.ExpiresAt.Equal(want) {
			t.Errorf("%s: first idle end %v, want sign-in + %v", method, is.ExpiresAt.Sub(is.CreatedAt), MaxIdle)
		}
		clk.Add(13 * time.Hour)
		if _, err := m.Resolve(context.Background(), is.Token); err == nil {
			t.Errorf("%s: first use after 13 h is still valid", method)
		}
	}
}

// rowOf returns the stored row of the session token.
func rowOf(t *testing.T, ms *memStore, token string) store.SessionRow {
	t.Helper()
	h, _ := HashToken(token)
	ms.mu.Lock()
	defer ms.mu.Unlock()
	r, ok := ms.sessions[h]
	if !ok {
		t.Fatal("no session row")
	}
	return r
}

// The absolute caps do not depend on the idle setting: a session_ttl above
// them neither raises a cap nor issues an idle end past it.
func TestAbsoluteCapsAreIndependentOfTheIdleSetting(t *testing.T) {
	for _, method := range []string{AuthMethodLocal, "oidc:authentik"} {
		ms := newMemStore(alice)
		clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
		m := NewManager(ms, WithClock(clk.Now), WithIdleTTL(func(context.Context) (time.Duration, error) { return 720 * time.Hour, nil }))
		is, err := m.CreateAuth(context.Background(), alice, 720*time.Hour, Auth{At: clk.Now(), Method: method})
		if err != nil {
			t.Fatal(err)
		}
		want := LocalMaxAge
		if method != AuthMethodLocal {
			want = OIDCMaxAge
		}
		if !is.EndsAt.Equal(is.CreatedAt.Add(want)) {
			t.Errorf("%s: absolute end %v, want created + %v", method, is.EndsAt.Sub(is.CreatedAt), want)
		}
		if is.ExpiresAt.After(is.EndsAt) {
			t.Errorf("%s: first idle end %v is past the absolute end %v", method, is.ExpiresAt, is.EndsAt)
		}
		if got := m.IdleTTL(context.Background()); got != MaxIdle {
			t.Errorf("IdleTTL = %v, want it clamped to %v", got, MaxIdle)
		}
		// Activity until the cap never moves the idle end past it.
		mw := &Middleware{Manager: m}
		for clk.Now().Before(is.EndsAt) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			s, err := mw.ResolveRequest(r, is.Token)
			if err != nil {
				break
			}
			if s.ExpiresAt.After(is.EndsAt) {
				t.Fatalf("%s: idle end %v past the absolute end %v", method, s.ExpiresAt, is.EndsAt)
			}
			clk.Add(6 * time.Hour)
		}
		clk.t = is.EndsAt
		if _, err := m.Resolve(context.Background(), is.Token); err == nil {
			t.Errorf("%s: resolved at its absolute end", method)
		}
	}
}

// A row of v0.20.4 (a fixed 24 h expiry) is clamped to the idle lifetime of
// now on its first use, and its cap counts from its sign-in; a lowered
// session_ttl applies to outstanding sessions on their next request.
func TestOldAndLongerIdleEndsAreClampedOnUse(t *testing.T) {
	ms := newMemStore(alice)
	clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	var idle atomic.Int64
	idle.Store(int64(2 * time.Hour))
	m := NewManager(ms, WithClock(clk.Now), WithIdleTTL(func(context.Context) (time.Duration, error) { return time.Duration(idle.Load()), nil }))
	// The v0.20.4 row: issued an hour ago, valid for 24 h.
	created := clk.Now().Add(-time.Hour)
	tok := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes))
	h, _ := HashToken(tok)
	if err := ms.CreateSession(context.Background(), store.SessionRow{
		ID: "old", TokenHash: h, UserID: alice.ID, CredentialVersion: alice.CredentialVersion,
		CreatedAt: store.UnixMS(created), ExpiresAt: store.UnixMS(created.Add(24 * time.Hour)), AuthMethod: "oidc:authentik",
	}, store.UnixMS(created)); err != nil {
		t.Fatal(err)
	}
	s, err := m.Resolve(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.Now().Add(2 * time.Hour); !s.ExpiresAt.Equal(want) || rowOf(t, ms, tok).ExpiresAt != store.UnixMS(want) {
		t.Fatalf("idle end %v (stored %d), want now + 2h", s.ExpiresAt, rowOf(t, ms, tok).ExpiresAt)
	}
	if !s.EndsAt.Equal(created.Add(OIDCMaxAge)) {
		t.Errorf("absolute end %v, want sign-in + %v", s.EndsAt, OIDCMaxAge)
	}
	// Background polls alone: the session ends idle after 2 h, not 24 h.
	clk.Add(2*time.Hour + time.Second)
	if _, err := m.Resolve(context.Background(), tok); err == nil {
		t.Fatal("the old row is still valid past the clamped idle end")
	}

	// A lowered setting applies to an outstanding session on its next request.
	is, err := m.CreateAuth(context.Background(), alice, 2*time.Hour, Auth{At: clk.Now(), Method: AuthMethodLocal})
	if err != nil {
		t.Fatal(err)
	}
	idle.Store(int64(10 * time.Minute))
	clk.Add(idleCacheFor + time.Second) // the cached value expires
	s, err = m.Resolve(context.Background(), is.Token)
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.Now().Add(10 * time.Minute); !s.ExpiresAt.Equal(want) {
		t.Errorf("idle end %v, want now + 10m after the setting was lowered", s.ExpiresAt)
	}
	clk.Add(11 * time.Minute)
	if _, err := m.Resolve(context.Background(), is.Token); err == nil {
		t.Error("the session outlived the lowered idle lifetime")
	}
}

// A request extends the session only with affirmative evidence that the
// person made it on the Hub's own pages.
func TestForegroundNeedsAffirmativeEvidence(t *testing.T) {
	const own = "https://hub.example:8443"
	mw := &Middleware{
		Origin: func(*http.Request) string { return own },
		Polls:  func(r *http.Request) bool { return r.URL.Path == "/poll" },
	}
	for _, tc := range []struct {
		name string
		path string
		hdr  map[string]string
		ctx  bool
		fg   bool
	}{
		{"no headers (curl, an old browser)", "/", nil, false, false},
		{"same-origin", "/", map[string]string{"Sec-Fetch-Site": "same-origin"}, false, true},
		{"typed address", "/", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-User": "?1"}, false, true},
		{"none without a user gesture", "/", map[string]string{"Sec-Fetch-Site": "none"}, false, false},
		{"same-site sibling", "/", map[string]string{"Sec-Fetch-Site": "same-site"}, false, false},
		{"cross-site", "/", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, false},
		{"cross-site with our Origin", "/", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": own}, false, false},
		{"own Origin", "/", map[string]string{"Origin": own}, false, true},
		{"own Origin, other case and default-less", "/", map[string]string{"Origin": "HTTPS://Hub.Example:8443"}, false, true},
		{"own host over http", "/", map[string]string{"Origin": "http://hub.example:8443"}, false, false},
		{"own host on another port", "/", map[string]string{"Origin": "https://hub.example"}, false, false},
		{"foreign Origin", "/", map[string]string{"Origin": "https://evil.example"}, false, false},
		{"null Origin", "/", map[string]string{"Origin": "null"}, false, false},
		{"foreign Origin with our Referer", "/", map[string]string{"Origin": "https://evil.example", "Referer": own + "/x"}, false, false},
		{"own Referer", "/", map[string]string{"Referer": own + "/sessions?x=1"}, false, true},
		{"foreign Referer", "/", map[string]string{"Referer": "https://evil.example/" + own}, false, false},
		{"background header", "/", map[string]string{"Sec-Fetch-Site": "same-origin", BackgroundHeader: "1"}, false, false},
		{"event stream", "/", map[string]string{"Sec-Fetch-Site": "same-origin", "Accept": "text/event-stream"}, false, false},
		{"background context", "/", map[string]string{"Sec-Fetch-Site": "same-origin"}, true, false},
		{"a polled route of an old page", "/poll", map[string]string{"Sec-Fetch-Site": "same-origin"}, false, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.example:8443"+tc.path, nil)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if tc.ctx {
			r = r.WithContext(AsBackground(r.Context()))
		}
		if got := mw.Foreground(r); got != tc.fg {
			t.Errorf("%s: Foreground = %v, want %v", tc.name, got, tc.fg)
		}
	}
	// Without a trusted origin only Fetch Metadata counts.
	r := httptest.NewRequest(http.MethodGet, "https://hub.example/", nil)
	r.Header.Set("Origin", "https://hub.example")
	if (&Middleware{}).Foreground(r) {
		t.Error("Origin counted without a trusted origin")
	}
}

func TestNavigation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		hdr    map[string]string
		nav    bool
	}{
		{"document", http.MethodGet, map[string]string{"Sec-Fetch-Dest": "document"}, true},
		{"iframe", http.MethodGet, map[string]string{"Sec-Fetch-Dest": "iframe"}, false},
		{"fetch", http.MethodGet, map[string]string{"Sec-Fetch-Dest": "empty"}, false},
		{"htmx", http.MethodGet, map[string]string{"Sec-Fetch-Dest": "document", "HX-Request": "true"}, false},
		{"background", http.MethodGet, map[string]string{"Sec-Fetch-Dest": "document", BackgroundHeader: "1"}, false},
		{"post", http.MethodPost, map[string]string{"Sec-Fetch-Dest": "document"}, false},
		{"old browser page", http.MethodGet, map[string]string{"Accept": "text/html,*/*"}, true},
		{"old browser json", http.MethodGet, map[string]string{"Accept": "application/json"}, false},
	} {
		r := httptest.NewRequest(tc.method, "/", nil)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if got := Navigation(r); got != tc.nav {
			t.Errorf("%s: Navigation = %v, want %v", tc.name, got, tc.nav)
		}
	}
}

// Wrap marks a polled route as background for the handlers behind it.
func TestWrapMarksPolledRoutesAsBackground(t *testing.T) {
	mw := &Middleware{Manager: NewManager(newMemStore(alice)), Polls: func(r *http.Request) bool { return r.URL.Path == "/poll" }}
	var bg []bool
	h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { bg = append(bg, Background(r)) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/poll", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/page", nil))
	if len(bg) != 2 || !bg[0] || bg[1] {
		t.Errorf("background = %v, want [true false]", bg)
	}
}
