package web

// Tests of the identity context of the session inspector (second review round): the server
// derives a public, opaque context from the principal its session layer authenticated, the page
// binds everything it keeps to it, and two read-only routes answer with it. The unit tests use
// the fake session layer; the same routes run against a real Hub with real session cookies,
// Alice and Bob, in internal/hub/server (inspector_context_native_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func ctxRequest(sess *session.Session) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/inspector/principal", nil)
	r = r.WithContext(context.WithValue(r.Context(), contextKeyKey{}, testContextKey))
	if sess != nil {
		r = r.WithContext(session.WithSession(r.Context(), sess))
	}
	return r
}

func TestPrincipalContextBindsUserLoginAndActAsAndLeaksNothing(t *testing.T) {
	base := &session.Session{ID: "sess-AAAA", UserID: "u-alice", Username: "alice", Role: authz.RoleUser, CSRFToken: "csrf-secret-token"}
	c0 := principalContext(ctxRequest(base))
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(c0) {
		t.Fatalf("context = %q, want 32 hex digits", c0)
	}
	if c1 := principalContext(ctxRequest(&session.Session{ID: "sess-AAAA", UserID: "u-alice", Username: "alice", Role: authz.RoleUser, CSRFToken: "other"})); c1 != c0 {
		t.Errorf("the same login gives a different context: %q and %q", c0, c1)
	}
	others := map[string]*session.Session{
		"another user":                 {ID: "sess-AAAA", UserID: "u-bob", Username: "bob", Role: authz.RoleUser},
		"the same user, a new login":   {ID: "sess-BBBB", UserID: "u-alice", Username: "alice", Role: authz.RoleUser},
		"the same login acting as bob": {ID: "sess-AAAA", UserID: "u-alice", Username: "alice", Role: authz.RoleUser, ActAs: &session.ActAs{TargetID: "u-bob"}},
		"acting as someone else":       {ID: "sess-AAAA", UserID: "u-alice", Username: "alice", Role: authz.RoleUser, ActAs: &session.ActAs{TargetID: "u-carol"}},
	}
	seen := map[string]string{c0: "alice"}
	for what, sess := range others {
		c := principalContext(ctxRequest(sess))
		if c == "" || c == c0 {
			t.Errorf("%s: the context did not change (%q)", what, c)
		}
		if prev, dup := seen[c]; dup {
			t.Errorf("%s has the same context as %s", what, prev)
		}
		seen[c] = what
	}
	// Nothing that identifies or authenticates is in it: no id, no name, no CSRF token, no raw hash of them.
	for _, secret := range []string{"sess-AAAA", "u-alice", "alice", "csrf-secret-token"} {
		if strings.Contains(c0, secret) {
			t.Errorf("the context contains %q", secret)
		}
	}
	if principalContext(ctxRequest(nil)) != "" {
		t.Error("an anonymous request has a context")
	}
}

// The context is a function of the persisted login and the Hub's key only: a restarted Hub (another
// Server with the same key, here another request context) answers the same, a Hub with another key
// does not, and a request that carries no key (it did not pass through a Server) has no context at all.
func TestPrincipalContextIsStableForTheSameKeyAndFailsClosedWithoutOne(t *testing.T) {
	sess := &session.Session{ID: "sess-AAAA", UserID: "u-alice", Role: authz.RoleUser}
	c0 := principalContext(ctxRequest(sess))
	r := httptest.NewRequest(http.MethodGet, "/inspector/principal", nil)
	r = r.WithContext(session.WithSession(context.WithValue(r.Context(), contextKeyKey{}, append([]byte(nil), testContextKey...)), sess))
	if c1 := principalContext(r); c1 != c0 {
		t.Errorf("the same key and login gave %q then %q", c0, c1)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/inspector/principal", nil)
	r2 = r2.WithContext(session.WithSession(context.WithValue(r2.Context(), contextKeyKey{}, bytes.Repeat([]byte{0x11}, MinContextKey)), sess))
	if c2 := principalContext(r2); c2 == "" || c2 == c0 {
		t.Errorf("another Hub key gave %q (first %q)", c2, c0)
	}
	bare := httptest.NewRequest(http.MethodGet, "/inspector/principal", nil)
	bare = bare.WithContext(session.WithSession(bare.Context(), sess))
	if principalContext(bare) != "" {
		t.Error("a request with no Hub key has a context: there must be no process-random fallback")
	}
	rec := httptest.NewRecorder()
	handlePrincipal(rec, bare)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), `"context"`) {
		t.Errorf("handlePrincipal without a key = %d %s", rec.Code, rec.Body)
	}
}

func setSnapErr(f *fakeSource, err error) {
	f.mu.Lock()
	f.snapErr = err
	f.mu.Unlock()
}

func ctxGet(t *testing.T, s *Server, path, user string) *httptest.ResponseRecorder {
	t.Helper()
	if user == "" {
		return get(t, s, path)
	}
	return get(t, s, path, "X-Test-Session", user)
}

func TestInspectorPrincipalRouteAnswersTheContextOfTheRequestAndOnlyThat(t *testing.T) {
	s, _ := sessionOps(t)
	rec := ctxGet(t, s, "/inspector/principal", sessUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", rec.Header())
	}
	var a contextAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil || a.Context == "" || a.WorkerID != "" || a.Session != "" {
		t.Fatalf("answer = %q (%v)", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), "csrf") || strings.Contains(rec.Body.String(), "bob") {
		t.Errorf("the answer names the principal or a secret: %s", rec.Body)
	}
	rec2 := ctxGet(t, s, "/inspector/principal", "carol:user")
	var b contextAnswer
	_ = json.Unmarshal(rec2.Body.Bytes(), &b)
	if b.Context == "" || b.Context == a.Context {
		t.Errorf("another user has context %q, first %q", b.Context, a.Context)
	}
	// Signed out: the session layer's own answer (a redirect to sign-in), never a context.
	out := ctxGet(t, s, "/inspector/principal", "")
	if out.Code != http.StatusSeeOther || !strings.HasPrefix(out.Header().Get("Location"), "/login") || strings.Contains(out.Body.String(), a.Context) {
		t.Errorf("signed out = %d %q", out.Code, out.Header().Get("Location"))
	}
	// A must-change account is held at the password page like every other route.
	must := ctxGet(t, s, "/inspector/principal", "ann:user:must")
	if must.Code != http.StatusSeeOther || must.Header().Get("Location") != "/account/password" {
		t.Errorf("must change = %d %q", must.Code, must.Header().Get("Location"))
	}
	// A POST is not a route.
	post := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/inspector/principal", nil)
	req.Header.Set("X-Test-Session", sessUser)
	s.ServeHTTP(post, req)
	if post.Code == http.StatusOK {
		t.Errorf("POST /inspector/principal = %d", post.Code)
	}
}

// The resource check: a Worker the caller cannot see, a session the snapshot does not list, a refused or failing read all fail without
// leaking; a listed session of a visible Worker answers with the context of this very request.
func TestSessionContextRouteChecksTheResourceAgainstTheAuthorisedSnapshot(t *testing.T) {
	f := newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{sessionWorker()}})
	connect(t, f)
	src := newFakeSource(Dashboard{Workers: []WorkerRow{{ID: sessWorkerID, Name: "pi-1", Owner: "bob", Online: true}},
		Sessions: []SessionRow{{WorkerID: sessWorkerID, Worker: "pi-1", Owner: "bob", Name: "minion-1", State: "stable", Online: true}}}, Dashboard{})
	SetLiveSource(src)
	t.Cleanup(func() { SetLiveSource(nil) })
	s := adminServer(t)

	rec := ctxGet(t, s, "/s/pi-1/minion-1/context", sessUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	var a contextAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	p := ctxGet(t, s, "/inspector/principal", sessUser)
	var pa contextAnswer
	_ = json.Unmarshal(p.Body.Bytes(), &pa)
	if a.Context == "" || a.Context != pa.Context || a.WorkerID != sessWorkerID || a.Session != "minion-1" || a.Online == nil || !*a.Online {
		t.Errorf("answer %+v, principal %+v", a, pa)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	// By the canonical id as well.
	if rec := ctxGet(t, s, "/s/"+sessWorkerID+"/minion-1/context", sessUser); rec.Code != http.StatusOK {
		t.Errorf("by id = %d", rec.Code)
	}
	// Not leaking: an unknown session, an unknown Worker and a Worker of another owner answer the same plain 404.
	notFound := map[string]*httptest.ResponseRecorder{
		"unknown session": ctxGet(t, s, "/s/pi-1/nope/context", sessUser),
		"unknown worker":  ctxGet(t, s, "/s/nope/minion-1/context", sessUser),
	}
	for what, r := range notFound {
		if r.Code != http.StatusNotFound {
			t.Errorf("%s = %d %s", what, r.Code, r.Body)
		}
	}
	if notFound["unknown session"].Body.String() != notFound["unknown worker"].Body.String() {
		t.Errorf("the two 404 differ: %q and %q", notFound["unknown session"].Body, notFound["unknown worker"].Body)
	}
	// Signed out: no context and no resource information.
	out := ctxGet(t, s, "/s/pi-1/minion-1/context", "")
	if out.Code != http.StatusSeeOther || strings.Contains(out.Body.String(), a.Context) {
		t.Errorf("signed out = %d", out.Code)
	}
	// The snapshot fails: 502, nothing proven. It is refused: the plain 404.
	setSnapErr(src, &OpError{Status: 502, Code: "internal", Message: "secret detail"})
	if r := ctxGet(t, s, "/s/pi-1/minion-1/context", sessUser); r.Code != http.StatusBadGateway || strings.Contains(r.Body.String(), "secret") {
		t.Errorf("failing snapshot = %d %s", r.Code, r.Body)
	}
	setSnapErr(src, &OpError{Status: 403, Code: "forbidden", Message: "no"})
	if r := ctxGet(t, s, "/s/pi-1/minion-1/context", sessUser); r.Code != http.StatusNotFound {
		t.Errorf("refused snapshot = %d", r.Code)
	}
}

// The pages the inspector lives on carry the context of their own request, so a kept frame can be compared with it.
func TestPagesCarryTheIdentityContextOfTheirRequest(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	ctxOf := func(body string) string {
		m := regexp.MustCompile(`id="inspector"[^>]*data-context="([0-9a-f]{32})"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no data-context on the inspector of the page:\n%s", body)
		}
		return m[1]
	}
	for _, path := range []string{"/", "/workers", "/sessions"} {
		_, alice := page(t, s, path, "alice:user")
		_, bob := page(t, s, path, "bob:user")
		if ctxOf(alice) == ctxOf(bob) {
			t.Errorf("%s: two users share a context", path)
		}
		_, again := page(t, s, path, "alice:user")
		if ctxOf(alice) != ctxOf(again) {
			t.Errorf("%s: the context of one login is not stable", path)
		}
	}
	// The principal route says what the page says.
	_, alicePage := page(t, s, "/sessions", "alice:user")
	var a contextAnswer
	rec := ctxGet(t, s, "/inspector/principal", "alice:user")
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	if a.Context != ctxOf(alicePage) {
		t.Errorf("the principal route (%q) and the page (%q) disagree", a.Context, ctxOf(alicePage))
	}
	// And the compact session view carries it for the frame to repeat in its ready message.
	ss, _ := sessionOps(t)
	body := sessGet(t, ss, "/s/pi-1/minion-1?embed=1", sessUser).Body.String()
	if m := regexp.MustCompile(`<div id="session-view" data-embed="true" data-context="([0-9a-f]{32})"`).FindStringSubmatch(body); m == nil {
		t.Errorf("the compact session view has no data-context:\n%s", body)
	}
}
