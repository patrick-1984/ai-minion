package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
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
