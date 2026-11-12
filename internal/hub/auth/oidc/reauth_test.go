package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

// reauthHub is a Hub with an administrator who signed in through the IdP with
// a fresh auth_time.
type reauthHub struct {
	*testHub
	sess  *session.Session
	token string
}

func newReauthHub(t *testing.T, authTime func() any) *reauthHub {
	t.Helper()
	h := newTestHub(t, nil)
	h.groups("minion-admins")
	if authTime != nil {
		h.idp.set(func(f *fakeIdP) { f.claims["auth_time"] = authTime() })
	}
	r := h.signIn()
	if r.status != http.StatusSeeOther {
		t.Fatalf("sign-in = %d %s", r.status, r.body)
	}
	sess, ok := h.sessionOf(r)
	if !ok {
		t.Fatal("no session")
	}
	return &reauthHub{testHub: h, sess: sess, token: r.cookie(session.CookieName).Value}
}

func (h *reauthHub) setAuthTime(v any) {
	h.idp.set(func(f *fakeIdP) {
		if v == nil {
			delete(f.claims, "auth_time")
			return
		}
		f.claims["auth_time"] = v
	})
}

func (h *reauthHub) current(t *testing.T) *session.Session {
	t.Helper()
	s, err := session.NewManager(h.st).Resolve(context.Background(), h.token)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// start posts the start of a re-authentication as the browser and returns the
// URL of the IdP.
func (h *reauthHub) start(t *testing.T) (idpURL string, status int) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+ReauthStartPath, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(session.CSRFHeader, session.CSRFToken(h.token))
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.RedirectURL, resp.StatusCode
}

// walk takes the browser to the IdP and returns the callback URL.
func (h *reauthHub) walk(t *testing.T, idpURL string) string {
	t.Helper()
	r := h.get(h.idpCl, idpURL)
	if r.status != http.StatusFound {
		t.Fatalf("authorize = %d %s", r.status, r.body)
	}
	return r.location
}

func TestAnOIDCLoginWithAFreshAuthTimeStartsFreshAndWithoutOneNever(t *testing.T) {
	h := newReauthHub(t, func() any { return time.Now().Unix() })
	if !h.sess.Fresh(time.Now()) || h.sess.AuthMethod != "oidc:default" {
		t.Errorf("session with auth_time: fresh=%v method=%q at=%v", h.sess.Fresh(time.Now()), h.sess.AuthMethod, h.sess.AuthAt)
	}
	// A plain SSO redirect that returns no auth_time is never fresh.
	plain := newReauthHub(t, nil)
	if plain.sess.Fresh(time.Now()) || !plain.sess.AuthAt.IsZero() {
		t.Errorf("session without auth_time: %+v", plain.sess)
	}
	// An old auth_time (the IdP session is old) is not fresh either.
	old := newReauthHub(t, func() any { return time.Now().Add(-time.Hour).Unix() })
	if old.sess.Fresh(time.Now()) {
		t.Error("a session whose auth_time is an hour old is fresh")
	}
	// A bogus auth_time is ignored.
	for _, v := range []any{"yesterday", -5, 0, time.Now().Add(time.Hour).Unix(), true} {
		bad := newReauthHub(t, func() any { return v })
		if bad.sess.Fresh(time.Now()) {
			t.Errorf("auth_time %v made the session fresh", v)
		}
	}
}

func TestReauthStartAsksForANewAuthenticationAndOnlyWhenSignedInThroughTheIdP(t *testing.T) {
	h := newReauthHub(t, nil)
	u, status := h.start(t)
	if status != http.StatusOK {
		t.Fatalf("start = %d", status)
	}
	q, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if q.Query().Get("max_age") != "0" || q.Query().Get("prompt") != "login" || q.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("authorization request = %s", u)
	}
	// No session cookie: unauthenticated.
	req, _ := http.NewRequest("POST", h.srv.URL+ReauthStartPath, strings.NewReader("{}"))
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Errorf("anonymous start = %d", resp.StatusCode)
	}
	// A session that signed in with a local password cannot start it.
	user, _ := h.st.UserByIdentifier(context.Background(), h.sess.Username)
	local, err := session.NewManager(h.st).CreateAuth(context.Background(), user, time.Hour, session.Auth{At: time.Now(), Method: session.AuthMethodLocal})
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("POST", h.srv.URL+ReauthStartPath, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", session.CookieName+"="+local.Token)
	req.Header.Set(session.CSRFHeader, local.CSRFToken)
	resp, err = (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("start by a local session = %d, want 403 reauth_unavailable", resp.StatusCode)
	}
}

func TestReauthRaisesTheSessionOnlyWithAFreshAuthTimeAndTheSameSubject(t *testing.T) {
	cases := []struct {
		name     string
		authTime func() any // nil: the claim is absent
		subject  string
		wantOK   bool
	}{
		{"fresh auth_time, same subject", func() any { return time.Now().Unix() }, "", true},
		{"auth_time 40 s ahead of the clock is within the skew", func() any { return time.Now().Add(40 * time.Second).Unix() }, "", true},
		{"auth_time 20 s before the request", func() any { return time.Now().Add(-20 * time.Second).Unix() }, "", true},
		{"auth_time beyond the 60 s skew ahead", func() any { return time.Now().Add(90 * time.Second).Unix() }, "", false},
		{"auth_time older than the freshness window", func() any { return time.Now().Add(-6 * time.Minute).Unix() }, "", false},
		{"provider returns no auth_time", nil, "", false},
		{"auth_time before the request started", func() any { return time.Now().Add(-10 * time.Minute).Unix() }, "", false},
		{"auth_time in the far future", func() any { return time.Now().Add(time.Hour).Unix() }, "", false},
		{"auth_time is not a number", func() any { return "now" }, "", false},
		{"another subject", func() any { return time.Now().Unix() }, "user-2", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newReauthHub(t, nil) // the login itself is not fresh
			before := h.current(t)
			if before.Fresh(time.Now()) {
				t.Fatal("the starting session is fresh")
			}
			idpURL, status := h.start(t)
			if status != http.StatusOK {
				t.Fatalf("start = %d", status)
			}
			if c.authTime != nil {
				h.setAuthTime(c.authTime())
			}
			if c.subject != "" {
				h.idp.set(func(f *fakeIdP) { f.subject = c.subject })
			}
			cb := h.walk(t, idpURL)
			r := h.get(h.client, cb)
			if r.status != http.StatusSeeOther {
				t.Fatalf("callback = %d %s", r.status, r.body)
			}
			after := h.current(t)
			if c.wantOK {
				if r.location != ReauthResultPath+"?reauth=ok" || !after.Fresh(time.Now()) || after.AuthMethod != "oidc:default" {
					t.Errorf("location=%q fresh=%v method=%q", r.location, after.Fresh(time.Now()), after.AuthMethod)
				}
				if after.ID != before.ID {
					t.Error("the re-authentication changed the session")
				}
				// auth_at is the verified auth_time (clamped to the
				// clock), not the time of the callback.
				claim, _ := authTime(map[string]any{"auth_time": float64(c.authTime().(int64))})
				want := claim
				if want.After(time.Now()) {
					want = time.Now()
				}
				if d := after.AuthAt.Sub(want); d > 3*time.Second || d < -3*time.Second {
					t.Errorf("auth_at = %v, want about %v", after.AuthAt, want)
				}
				return
			}
			if r.location != ReauthResultPath+"?reauth=unavailable" {
				t.Errorf("location = %q, want reauth=unavailable", r.location)
			}
			if !after.AuthAt.Equal(before.AuthAt) || after.AuthMethod != before.AuthMethod || after.Fresh(time.Now()) {
				t.Errorf("a refused re-authentication updated the session: %+v", after)
			}
			if !h.aud.has(AuditActionReauthFailed, OutcomeFailed) {
				t.Error("no hub_identity.reauth_failed audit event")
			}
		})
	}
}

func TestReauthStateIsOneTimeBoundToTheBrowserAndExpires(t *testing.T) {
	t.Run("used twice", func(t *testing.T) {
		h := newReauthHub(t, nil)
		h.setAuthTime(time.Now().Unix())
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		if r := h.get(h.client, cb); r.location != ReauthResultPath+"?reauth=ok" {
			t.Fatalf("first use = %d %q", r.status, r.location)
		}
		stale := time.Now().Add(-time.Hour)
		if _, err := h.st.SetSessionAuth(context.Background(), h.sess.ID, stale.UnixMilli(), "oidc:default"); err != nil {
			t.Fatal(err)
		}
		h.aud.reset()
		r := h.get(h.client, cb)
		if r.status != http.StatusSeeOther || r.location != ReauthResultPath+"?reauth=unavailable" {
			t.Errorf("second use = %d %q", r.status, r.location)
		}
		if !h.aud.has(AuditActionReauthFailed, OutcomeFailed) {
			t.Error("a replayed state wrote no hub_identity.reauth_failed event")
		}
		if h.current(t).Fresh(time.Now()) {
			t.Error("a replayed callback made the session fresh")
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		h := newReauthHub(t, nil)
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		u, _ := url.Parse(cb)
		q := u.Query()
		q.Set("state", "not-a-state")
		u.RawQuery = q.Encode()
		if r := h.get(h.client, u.String()); r.status != http.StatusSeeOther || r.location != ReauthResultPath+"?reauth=unavailable" {
			t.Errorf("unknown state = %d %q", r.status, r.location)
		}
		if !h.aud.has(AuditActionReauthFailed, OutcomeFailed) {
			t.Error("an unknown state wrote no hub_identity.reauth_failed event")
		}
		if h.current(t).Fresh(time.Now()) {
			t.Error("fresh")
		}
	})
	t.Run("another browser", func(t *testing.T) {
		h := newReauthHub(t, nil)
		h.setAuthTime(time.Now().Unix())
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		other := newBrowser(t, nil)
		r := h.get(other, cb)
		if r.location != ReauthResultPath+"?reauth=unavailable" {
			t.Errorf("callback in another browser = %d %q", r.status, r.location)
		}
		if h.current(t).Fresh(time.Now()) {
			t.Error("fresh")
		}
	})
	t.Run("expired", func(t *testing.T) {
		h := newReauthHub(t, nil)
		h.setAuthTime(time.Now().Unix())
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		h.svc.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
		r := h.get(h.client, cb)
		if r.location != ReauthResultPath+"?reauth=unavailable" || h.current(t).Fresh(time.Now()) {
			t.Errorf("expired callback = %d %q", r.status, r.location)
		}
	})
	t.Run("the IdP reports an error", func(t *testing.T) {
		h := newReauthHub(t, nil)
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		u, _ := url.Parse(cb)
		q := u.Query()
		q.Del("code")
		q.Set("error", "login_required")
		u.RawQuery = q.Encode()
		if r := h.get(h.client, u.String()); r.location != ReauthResultPath+"?reauth=unavailable" {
			t.Errorf("callback = %d %q", r.status, r.location)
		}
	})
	t.Run("the session was deleted", func(t *testing.T) {
		h := newReauthHub(t, nil)
		h.setAuthTime(time.Now().Unix())
		idpURL, _ := h.start(t)
		cb := h.walk(t, idpURL)
		if err := session.NewManager(h.st).Revoke(context.Background(), h.token); err != nil {
			t.Fatal(err)
		}
		if r := h.get(h.client, cb); r.location != ReauthResultPath+"?reauth=unavailable" {
			t.Errorf("callback = %d %q", r.status, r.location)
		}
	})
}
