package server

// The identity context of the session inspector on a composed Hub: the real web, API, routing,
// events and session modules, real HTTP and REAL session cookies (nothing is faked: the browser
// session of each person is created by the session manager and sent as the cookie). It proves
// what a fake fetch cannot: that the guard reads tell Alice, Bob, a second login of Alice and an
// admin apart, that an empty 200 list proves nothing about who asks, that Bob and an admin who
// MAY read Alice's worker still get a different context (so a frame Alice parked is cleared
// even though the read succeeds), and that nothing secret is in an answer.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

type ctxAnswer struct {
	Context  string `json:"context"`
	WorkerID string `json:"worker_id"`
	Session  string `json:"session"`
	Online   *bool  `json:"online"`
}

func TestInspectorContextWithRealSessionCookies(t *testing.T) {
	d := startDashHub(t)
	hc := d.hc
	aliceID := d.user("alice")
	d.user("bob")
	alice1, csrfA1 := d.login("alice")
	alice2, _ := d.login("alice") // the same person, a second login (another tab, another browser session)
	bob, _ := d.login("bob")
	root, _ := d.login("root")

	wid := d.enrol("lab", aliceID, store.WorkerKindPrivate)
	d.connect(wid, "lab", aliceID, "private", map[string]string{"minion-1": "task"})

	read := func(path string, hdr map[string]string) (int, string) {
		t.Helper()
		code, body, _ := hc.do("GET", path, nil, hdr)
		return code, body
	}
	principal := func(hdr map[string]string) string {
		t.Helper()
		code, body := read("/inspector/principal", hdr)
		if code != 200 {
			t.Fatalf("GET /inspector/principal = %d %s", code, body)
		}
		var a ctxAnswer
		if err := json.Unmarshal([]byte(body), &a); err != nil || a.Context == "" {
			t.Fatalf("principal answer %q (%v)", body, err)
		}
		return a.Context
	}
	cA1, cA2, cB, cRoot := principal(alice1), principal(alice2), principal(bob), principal(root)

	// One login is one context, however often it asks; every other principal or login is another.
	if again := principal(alice1); again != cA1 {
		t.Errorf("one login, two contexts: %q and %q", cA1, again)
	}
	distinct := map[string]string{cA1: "alice login 1", cA2: "alice login 2", cB: "bob", cRoot: "root"}
	if len(distinct) != 4 {
		t.Errorf("two principals or logins share a context: %v", distinct)
	}

	// The page the inspector lives on carries the context of ITS request, the same value the principal route gives.
	page := func(hdr map[string]string) string {
		m := regexp.MustCompile(`id="inspector"[^>]*data-context="([0-9a-f]{32})"`).FindStringSubmatch(d.page("/sessions", hdr))
		if m == nil {
			t.Fatal("no data-context on the inspector of /sessions")
		}
		return m[1]
	}
	if page(alice1) != cA1 || page(bob) != cB || page(alice2) != cA2 {
		t.Errorf("a page and the principal route disagree: %q/%q, %q/%q", page(alice1), cA1, page(bob), cB)
	}

	// The resource check of Alice's session: Alice may read it, as login 1 and as login 2; the context in the answer is the one of THAT login.
	ctxPath := "/s/" + wid + "/minion-1/context"
	for who, c := range map[string]struct {
		hdr map[string]string
		ctx string
	}{"alice login 1": {alice1, cA1}, "alice login 2": {alice2, cA2}} {
		code, body := read(ctxPath, c.hdr)
		var a ctxAnswer
		_ = json.Unmarshal([]byte(body), &a)
		if code != 200 || a.Context != c.ctx || a.WorkerID != wid || a.Session != "minion-1" {
			t.Errorf("%s: GET %s = %d %s (want context %q)", who, ctxPath, code, body, c.ctx)
		}
	}
	// An admin may read the same worker: 200, and ANOTHER context. A frame bound to Alice's context must go even though this read succeeds.
	code, body := read(ctxPath, root)
	var ra ctxAnswer
	_ = json.Unmarshal([]byte(body), &ra)
	if code != 200 || ra.Context != cRoot || ra.Context == cA1 {
		t.Errorf("admin reading Alice's session: %d %s, want 200 with the admin's own context", code, body)
	}
	// Bob may NOT read it (a private worker of Alice): the plain 404, the same text as a worker that does not exist, nothing of Alice's.
	bc, bbody := read(ctxPath, bob)
	nc, nbody := read("/s/"+store.NewID()+"/minion-1/context", bob)
	if bc != 404 || nc != 404 || bbody != nbody || strings.Contains(bbody, "minion-1") || strings.Contains(bbody, wid) {
		t.Errorf("Bob on Alice's worker = %d %q, an unknown worker = %d %q", bc, bbody, nc, nbody)
	}
	// An unknown session of Alice's worker: the same 404.
	if code, body := read("/s/"+wid+"/nope/context", alice1); code != 404 || body != nbody {
		t.Errorf("unknown session = %d %q", code, body)
	}

	// Why the first guard was wrong: Bob's read of the sessions of Alice's worker is a plain 200 with nothing in it. That says nothing about WHO asks.
	code, body = read("/api/v1/sessions?all=true&worker="+wid, bob)
	if code != 200 || strings.Contains(body, "minion-1") {
		t.Errorf("Bob's sessions read = %d %s (the empty 200 the old guard took for \"still Alice\")", code, body)
	}

	// Signed out (a cookie that names no session): the sign-in page, never a context.
	for _, p := range []string{"/inspector/principal", ctxPath} {
		code, body, resp := hc.do("GET", p, nil, map[string]string{"Cookie": "minion_session=not-a-session"})
		if resp.Request.URL.Path != "/login" || strings.Contains(body, cA1) || code == 0 {
			t.Errorf("signed out %s ended at %s (%d)", p, resp.Request.URL.Path, code)
		}
	}

	// Nothing secret in any answer: not the cookie, not the CSRF token, not a user id, not a name.
	cookie := strings.TrimPrefix(alice1["Cookie"], "minion_session=")
	for _, p := range []string{"/inspector/principal", ctxPath} {
		_, body := read(p, alice1)
		for _, secret := range []string{cookie, csrfA1, aliceID, "alice"} {
			if secret != "" && strings.Contains(body, secret) {
				t.Errorf("%s leaks %q: %s", p, secret, body)
			}
		}
	}
	_, pageBody, _ := hc.do("GET", "/sessions", nil, alice1)
	if strings.Contains(pageBody, cookie) {
		t.Error("the page contains the session cookie")
	}
}
