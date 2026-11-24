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

// countingRenewer answers every renewal with out and counts the calls.
type countingRenewer struct {
	out   RenewOutcome
	calls atomic.Int32
}

func (c *countingRenewer) RenewSession(context.Context, *Session) RenewOutcome {
	c.calls.Add(1)
	return c.out
}

func idleRig(t *testing.T, method string, ren Renewer) (*Manager, *fakeClock, http.Handler, Issued) {
	t.Helper()
	ms := newMemStore(alice)
	clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	m := NewManager(ms, WithClock(clk.Now), WithIdleTTL(func(context.Context) (time.Duration, error) { return 2 * time.Hour, nil }))
	is, err := m.CreateAuth(context.Background(), alice, 2*time.Hour, Auth{At: clk.Now(), Method: method})
	if err != nil {
		t.Fatal(err)
	}
	mw := &Middleware{Manager: m, Renew: ren}
	h := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	return m, clk, h, is
}

func get(h http.Handler, token string, background bool) int {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if background {
		r.Header.Set(BackgroundHeader, "1")
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
	ren := &countingRenewer{out: RenewOK}
	_, clk, h, is := idleRig(t, AuthMethodLocal, ren)
	// Active every 90 minutes: the session lives on past the first 2 h.
	for range 4 {
		clk.Add(90 * time.Minute)
		if code := get(h, is.Token, false); code != http.StatusOK {
			t.Fatalf("active session at %v = %d", clk.Now(), code)
		}
	}
	// Background polls alone do not keep it alive.
	clk.Add(90 * time.Minute)
	get(h, is.Token, true)
	clk.Add(31 * time.Minute)
	if code := get(h, is.Token, false); code != http.StatusUnauthorized {
		t.Fatalf("idle local session = %d, want signed out", code)
	}
	if ren.calls.Load() != 0 {
		t.Errorf("a local session asked the renewer %d times", ren.calls.Load())
	}
}

func TestLocalSessionEndsAtItsAbsoluteEnd(t *testing.T) {
	_, clk, h, is := idleRig(t, AuthMethodLocal, nil)
	for clk.Now().Before(is.CreatedAt.Add(LocalMaxAge - time.Hour)) {
		clk.Add(time.Hour)
		if code := get(h, is.Token, false); code != http.StatusOK {
			t.Fatalf("active session at %v = %d", clk.Now(), code)
		}
	}
	clk.Add(time.Hour)
	if code := get(h, is.Token, false); code != http.StatusUnauthorized {
		t.Fatalf("local session past %v = %d, want signed out", LocalMaxAge, code)
	}
}

func TestExpiredOIDCSessionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  RenewOutcome
		want int
	}{
		{"renewed", RenewOK, http.StatusOK},
		{"refused", RenewFailed, http.StatusUnauthorized},
		{"unreachable within the grace", RenewUnavailable, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ren := &countingRenewer{out: tc.out}
			m, clk, h, is := idleRig(t, "oidc:authentik", ren)
			clk.Add(2*time.Hour + time.Minute)
			if code := get(h, is.Token, false); code != tc.want {
				t.Fatalf("page load = %d, want %d", code, tc.want)
			}
			_, err := m.Resolve(context.Background(), is.Token)
			switch tc.out {
			case RenewOK:
				if err != nil {
					t.Errorf("renewed session does not resolve: %v", err)
				}
			case RenewFailed:
				if _, ok := err.(*ExpiredError); ok || err == nil {
					t.Errorf("refused session still exists: %v", err)
				}
			}
		})
	}
}

func TestCookieOfAnOIDCSessionOutlivesTheIdleEnd(t *testing.T) {
	_, _, _, is := idleRig(t, "oidc:authentik", nil)
	if c := Cookie(is, true); !c.Expires.Equal(is.CreatedAt.Add(OIDCMaxAge)) {
		t.Fatalf("cookie expires %v, want %v", c.Expires, is.CreatedAt.Add(OIDCMaxAge))
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
			s, err := mw.ResolveRequest(httptest.NewRequest(http.MethodGet, "/", nil), is.Token)
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

func TestForegroundAndCrossOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		ctx  bool
		fg   bool
	}{
		{"plain (curl)", nil, false, true},
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, false, true},
		{"typed address", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-User": "?1"}, false, true},
		{"same-site sibling", map[string]string{"Sec-Fetch-Site": "same-site"}, false, false},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false, false},
		{"foreign Origin", map[string]string{"Origin": "https://evil.example"}, false, false},
		{"own Origin", map[string]string{"Origin": "http://example.com"}, false, true},
		{"null Origin", map[string]string{"Origin": "null"}, false, false},
		{"background header", map[string]string{BackgroundHeader: "1"}, false, false},
		{"event stream", map[string]string{"Accept": "text/event-stream"}, false, false},
		{"background context", nil, true, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		if tc.ctx {
			r = r.WithContext(AsBackground(r.Context()))
		}
		if got := Foreground(r); got != tc.fg {
			t.Errorf("%s: Foreground = %v, want %v", tc.name, got, tc.fg)
		}
	}
}
