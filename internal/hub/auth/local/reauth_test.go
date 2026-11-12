package local

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SetSessionAuth lets the in-memory store offer the optional port of
// session.Manager.MarkAuthenticated.
func (m *memSessions) SetSessionAuth(_ context.Context, id string, at int64, method string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, r := range m.rows {
		if r.ID == id {
			r.AuthAt, r.AuthMethod = at, method
			m.rows[h] = r
			return true, nil
		}
	}
	return false, nil
}

func (f *fixture) sessionWith(auth session.Auth) session.Issued {
	f.t.Helper()
	is, err := f.mgr.CreateAuth(context.Background(), f.users.users[0], time.Hour, auth)
	if err != nil {
		f.t.Fatal(err)
	}
	return is
}

func (f *fixture) reauth(is session.Issued, password string, csrf bool) *httptestResp {
	b, _ := json.Marshal(map[string]string{"password": password})
	rec := f.post("/auth/reauth", string(b), "application/json", func(r *http.Request) {
		r.Header.Set("Cookie", session.CookieName+"="+is.Token)
		if csrf {
			r.Header.Set(session.CSRFHeader, is.CSRFToken)
		}
	})
	return &httptestResp{code: rec.Code, body: rec.Body.String()}
}

type httptestResp struct {
	code int
	body string
}

func (r *httptestResp) errCode(t *testing.T) proto.Code {
	t.Helper()
	var env struct {
		Error struct {
			Code proto.Code `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	return env.Error.Code
}

func TestLoginStartsAFreshSession(t *testing.T) {
	f := newFixture(t)
	rec := f.loginJSON("alice", alicePassword)
	if rec.Code != 200 {
		t.Fatalf("login = %d", rec.Code)
	}
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.CookieName {
			tok = c.Value
		}
	}
	s, err := f.mgr.Resolve(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Fresh(f.clock.Now()) || s.AuthMethod != session.AuthMethodLocal {
		t.Errorf("session = fresh %v method %q", s.Fresh(f.clock.Now()), s.AuthMethod)
	}
	f.clock.Add(6 * time.Minute)
	if s.Fresh(f.clock.Now()) {
		t.Error("the session is still fresh after six minutes")
	}
}

func TestReauthWithThePasswordRaisesOnlyThatSession(t *testing.T) {
	f := newFixture(t)
	stale := f.sessionWith(session.Auth{At: f.clock.Now().Add(-time.Hour), Method: session.AuthMethodLocal})
	other := f.sessionWith(session.Auth{})
	r := f.reauth(stale, alicePassword, true)
	if r.code != 200 {
		t.Fatalf("reauth = %d %s", r.code, r.body)
	}
	var out struct {
		AuthMethod string `json:"auth_method"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.AuthMethod != "local" {
		t.Errorf("body = %s", r.body)
	}
	s, _ := f.mgr.Resolve(context.Background(), stale.Token)
	if !s.Fresh(f.clock.Now()) || !s.AuthAt.Equal(f.clock.Now()) {
		t.Errorf("session = %+v", s)
	}
	o, _ := f.mgr.Resolve(context.Background(), other.Token)
	if o.Fresh(f.clock.Now()) {
		t.Error("another session of the same user became fresh")
	}
	// A request does not extend it: after five minutes it is stale again.
	f.clock.Add(5*time.Minute + time.Second)
	s, _ = f.mgr.Resolve(context.Background(), stale.Token)
	if s.Fresh(f.clock.Now()) {
		t.Error("the session stayed fresh for more than five minutes")
	}
}

func TestReauthRefusals(t *testing.T) {
	f := newFixture(t)
	is := f.sessionWith(session.Auth{At: f.clock.Now().Add(-time.Hour), Method: session.AuthMethodLocal})
	for name, c := range map[string]struct {
		do   func() *httptestResp
		code int
		err  proto.Code
	}{
		"wrong password": {func() *httptestResp { return f.reauth(is, "nope", true) }, 403, proto.CodeReauthRequired},
		"no csrf token":  {func() *httptestResp { return f.reauth(is, alicePassword, false) }, 403, proto.CodeForbidden},
		"empty password": {func() *httptestResp { return f.reauth(is, "", true) }, 400, proto.CodeInvalidArgument},
		"no session": {func() *httptestResp {
			rec := f.post("/auth/reauth", `{"password":"x"}`, "application/json", nil)
			return &httptestResp{code: rec.Code, body: rec.Body.String()}
		}, 401, proto.CodeUnauthenticated},
	} {
		r := c.do()
		if r.code != c.code || r.errCode(t) != c.err {
			t.Errorf("%s = %d %s", name, r.code, r.body)
		}
	}
	s, _ := f.mgr.Resolve(context.Background(), is.Token)
	if s.Fresh(f.clock.Now()) {
		t.Error("a refused re-authentication raised the session")
	}
	// A failed re-authentication is audited, without the password.
	found := false
	f.audit.mu.Lock()
	for _, ev := range f.audit.events {
		if ev.Action == AuditActionReauthFailed {
			found = true
			if ev.Actor != "u-alice" || ev.Target == "" {
				t.Errorf("event = %+v", ev)
			}
		}
	}
	f.audit.mu.Unlock()
	if !found {
		t.Error("no hub_identity.reauth_failed event")
	}
}

func TestReauthIsNotAvailableToASessionThatSignedInThroughAnIdP(t *testing.T) {
	f := newFixture(t)
	is := f.sessionWith(session.Auth{Method: "oidc:default"})
	r := f.reauth(is, alicePassword, true)
	if r.code != 403 || r.errCode(t) != proto.CodeReauthUnavailable {
		t.Errorf("reauth = %d %s", r.code, r.body)
	}
	s, _ := f.mgr.Resolve(context.Background(), is.Token)
	if s.Fresh(f.clock.Now()) || s.AuthMethod != "oidc:default" {
		t.Errorf("session = %+v", s)
	}
}

func TestReauthFailuresShareTheLoginBackoff(t *testing.T) {
	f := newFixture(t)
	is := f.sessionWith(session.Auth{At: f.clock.Now().Add(-time.Hour), Method: session.AuthMethodLocal})
	limited := false
	for i := 0; i < 30 && !limited; i++ {
		r := f.reauth(is, "wrong", true)
		limited = r.code == 429
	}
	if !limited {
		t.Error("repeated wrong passwords were never rate limited")
	}
	if r := f.reauth(is, alicePassword, true); r.code != 429 {
		t.Errorf("the right password during the back-off = %d, want 429", r.code)
	}
}
