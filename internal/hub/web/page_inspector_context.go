package web

// The identity context of the session inspector (Story 5.6, first and second
// review rounds). The inspector keeps whole session views (screens, drafts) in
// hidden frames of the page that hosts it. A cookie is shared by every tab of a
// browser, so another sign-in (another user, another act-as setting, a new login)
// in ANOTHER tab changes who the next request is made as, while the old page and
// its kept frames still show what the first person saw and typed. The inspector
// therefore binds everything it keeps to the authenticated context it was
// rendered for, and asks the server, with the real cookie, before it shows a kept
// frame again and every 30 seconds while it keeps frames.
//
// The context is a public, opaque value the SERVER derives from the principal
// the existing session layer authenticated for this very request: the user, the
// act-as target and the browser session. It is an HMAC under a key only the Hub has, so it contains no session id, token or CSRF value, cannot
// be reversed, and is not a credential: nothing in the Hub accepts it, it grants
// no read or write, and it is compared by the page, never trusted by the server.
// A client that claims one learns nothing and gains nothing. After a restart of
// the Hub the value is the SAME for an unchanged login: the key is derived from the
// Hub's persisted data key (a purpose of its own, the data key itself is never used or
// exposed) and every other input (browser session row, user, act-as target) is persisted
// too. A restart therefore keeps what the page holds (drafts, requests on their way,
// unknown deliveries), while another user, a new login or another act-as setting still
// changes it, and a Hub that cannot give a key does not start (there is no random key).
//
// Two narrow, read-only routes answer with it, behind the same session layer, the
// same must-change-password gate and the same Worker resolution as every other
// page (nothing new decides who may see what):
//
//	GET /inspector/principal               who is this request? (200 and the context)
//	GET /s/<worker>/<session>/context      may this request read this session NOW?
//
// The second one is the resource check: the Worker is looked up with workers_list
// exactly as the session page does (a Worker the caller cannot see is the same
// plain 404), and the session must be in the same authorised snapshot that feeds
// the session lists (the Hub's own REST read as this caller). A session that is
// not listed answers 404 with the same text as a Worker that does not exist, so
// nothing leaks. Any other failure is a 502 and proves nothing.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

const pathInspectorPrincipal = "/inspector/principal"

// contextKeyKey is the request-context key under which the Server hands the HMAC key of
// the inspector context to the handlers of its own Hub. It is the [Config.ContextKey] the Hub
// derived from its persisted data key, so there is no process-random key, no global and no
// fallback: a request that did not pass through a Server has no key and therefore no context.
type contextKeyKey struct{}

// principalContext returns the public context of the principal of r, or "" when
// nobody is signed in (or this request carries no context key, which fails closed).
// Two requests have the same context exactly when they are made by the same browser
// session as the same user with the same act-as target, whichever process of the Hub
// answers them: every input is persisted, and so is the key.
func principalContext(r *http.Request) string {
	key, _ := r.Context().Value(contextKeyKey{}).([]byte)
	if len(key) < MinContextKey {
		return ""
	}
	sess, ok := session.FromContext(r.Context())
	if !ok || sess == nil {
		return ""
	}
	actAs := ""
	if sess.ActAs != nil {
		actAs = sess.ActAs.TargetID
	}
	m := hmac.New(sha256.New, key)
	for _, part := range []string{"minion-inspector-context-v1", sess.ID, sess.UserID, actAs} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// contextAnswer is the body of both routes. Nothing in it is secret or a credential.
type contextAnswer struct {
	Context  string `json:"context"`
	WorkerID string `json:"worker_id,omitempty"`
	Session  string `json:"session,omitempty"`
	Online   *bool  `json:"online,omitempty"`
}

func writeContext(w http.ResponseWriter, a contextAnswer) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(a)
}

func handlePrincipal(w http.ResponseWriter, r *http.Request) {
	c := principalContext(r)
	if c == "" {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	writeContext(w, contextAnswer{Context: c})
}

// handleSessionContext is the resource check: may this request read this session now? The
// Worker was resolved by the shared route wrapper (a Worker this caller cannot see is a
// plain 404). The session must be in the SAME authorised snapshot that feeds the session
// lists (the Hub's own REST read as this caller, which omits sessions the caller may not
// read), so nothing here decides who may see what and the check works for an offline
// Worker too (its last known sessions are listed).
func handleSessionContext(w http.ResponseWriter, r *http.Request, t target) {
	src := currentLiveSource()
	if src == nil {
		http.Error(w, "the session could not be checked", http.StatusServiceUnavailable)
		return
	}
	_, admin, _ := requestUser(r)
	d, err := src.Snapshot(r, admin)
	if err != nil {
		var oe *OpError
		if errors.As(err, &oe) && (oe.Status == http.StatusNotFound || oe.Status == http.StatusForbidden) {
			http.Error(w, "That page does not exist.", http.StatusNotFound)
			return
		}
		http.Error(w, "the session could not be checked", http.StatusBadGateway)
		return
	}
	found := false
	for _, x := range d.Sessions {
		if x.WorkerID == t.worker.ID && x.Name == t.session {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "That page does not exist.", http.StatusNotFound)
		return
	}
	online := t.worker.Online
	writeContext(w, contextAnswer{Context: principalContext(r), WorkerID: t.worker.ID, Session: t.session, Online: &online})
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathInspectorPrincipal, Handler: http.HandlerFunc(handlePrincipal)})
	RegisterRoute(Route{Pattern: "GET " + pathSessionPattern + "/context", Handler: sessionRoute(handleSessionContext)})
}
