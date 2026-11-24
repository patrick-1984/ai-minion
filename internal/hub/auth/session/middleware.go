package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

type ctxKey struct{}

// FromContext returns the session that [Middleware.Wrap] attached to the
// request context.
func FromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Session)
	return s, ok && s != nil
}

// WithSession returns ctx carrying s. [Middleware.Wrap] uses it; tests of
// handlers behind the middleware use it too.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// BackgroundHeader marks a request that the page sends on its own (a timed
// poll, a refresh after a live event), not because the person did something.
// Such a request never extends the idle lifetime of the session, and a
// session whose idle lifetime ran out answers it with a quiet 401 (see
// [Idle]), so the screen stays as it is.
const BackgroundHeader = "X-Minion-Background"

// IdleHeader is set to "idle" on the quiet 401 that a request of a session
// whose idle lifetime ran out gets when it is not a top-level page load: the
// page pauses (keeps what it holds) instead of treating it as a sign-out. The
// API error carries the same as details.reason [IdleReason].
const IdleHeader = "X-Minion-Session"

// IdleReason is the details.reason of the API error for a request of a
// session whose idle lifetime ran out.
const IdleReason = "session_idle"

type backgroundKey struct{}

type idleKey struct{}

// AsBackground returns ctx marked so that a request carrying it is
// [Background]. Recurring authorization checks of a stream use it: they are
// not the person acting.
func AsBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// Background reports whether r is a background request: it carries
// [BackgroundHeader] "1", it opens an event stream (a live page reconnecting
// its stream is not the person acting), or its context was marked with
// [AsBackground] (also by [Middleware.Wrap] for a route that
// [Middleware.Polls] names).
func Background(r *http.Request) bool {
	if v, _ := r.Context().Value(backgroundKey{}).(bool); v {
		return true
	}
	return r.Header.Get(BackgroundHeader) == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// IdleSession is what the request context records about a session cookie
// whose idle lifetime ran out (see [WithIdle]).
type IdleSession struct {
	// Method is the auth method of the expired session ("local",
	// "oidc:<provider>").
	Method string
	// Reauth is set when the request may re-authenticate silently with the
	// identity provider (prompt=none): a foreground top-level page load of an
	// expired OIDC session. It is the provider id.
	Reauth string
}

// WithIdle returns ctx marked as [Idle]. [Middleware.Wrap] uses it; tests of
// handlers behind the middleware use it too.
func WithIdle(ctx context.Context, s IdleSession) context.Context {
	return context.WithValue(ctx, idleKey{}, s)
}

// Idle reports whether the request carried the cookie of a session whose
// idle lifetime ran out (the request goes on anonymously), and what is known
// about it. [Middleware.Wrap] sets it.
func Idle(ctx context.Context) (IdleSession, bool) {
	v, ok := ctx.Value(idleKey{}).(IdleSession)
	return v, ok
}

// Navigation reports whether r is a top-level page load (the address bar, a
// link, a reload, a form GET): a GET or HEAD that is not htmx, not
// [Background], and that the browser fetches as a document. A browser
// without Fetch Metadata is judged by its Accept header.
func Navigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("HX-Request") == "true" || Background(r) {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// Middleware authenticates browser sessions and enforces CSRF protection.
type Middleware struct {
	Manager *Manager
	// Log receives store failures; nil means [slog.Default].
	Log *slog.Logger
	// Origin returns the trusted public origin of the Hub for a request
	// (scheme, host and port, no path), which a browser without Fetch
	// Metadata must name in Origin or Referer for the request to count as
	// [Middleware.Foreground]. Nil or "" means no such browser request is
	// foreground.
	Origin func(r *http.Request) string
	// Polls, when set, names the routes that a page polls on its own (also
	// pages loaded before this version, which do not send
	// [BackgroundHeader]): such a request is [Background].
	Polls func(r *http.Request) bool
}

// Foreground reports whether r counts as the person's own activity, which
// moves the idle end of the session. It needs affirmative evidence:
//
//   - it is not [Background] (nor a route of [Middleware.Polls]);
//   - Sec-Fetch-Site is "same-origin", or "none" with Sec-Fetch-User "?1"
//     (the person typed the address, used a bookmark or reloaded); or, from a
//     browser that sends no Fetch Metadata, Origin or else Referer names
//     exactly the trusted public origin ([Middleware.Origin]).
//
// Everything else (another site, a sibling origin, a program without these
// headers) is not.
func (mw *Middleware) Foreground(r *http.Request) bool {
	if mw.background(r) {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "none":
		return r.Header.Get("Sec-Fetch-User") == "?1"
	case "":
	default:
		return false
	}
	if mw.Origin == nil {
		return false
	}
	want := normalOrigin(mw.Origin(r))
	if want == "" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return normalOrigin(o) == want
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return normalOrigin(ref) == want
	}
	return false
}

// normalOrigin returns scheme://host[:port] of a URL in lower case, with the
// default port of the scheme left out, or "" when it has no http(s) origin.
func normalOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

func (mw *Middleware) background(r *http.Request) bool {
	return Background(r) || (mw.Polls != nil && mw.Polls(r))
}

func (mw *Middleware) logger() *slog.Logger {
	if mw.Log != nil {
		return mw.Log
	}
	return slog.Default()
}

// safeMethod reports whether a request with this method is exempt from the
// CSRF check. AD-10 exempts GET only: every other method, HEAD and OPTIONS
// included, needs the X-Minion-CSRF header bound to the session.
func safeMethod(m string) bool {
	return m == http.MethodGet
}

// Wrap returns a handler that
//
//  1. rejects with forbidden every request other than GET that carries a
//     session cookie but not the matching X-Minion-CSRF header
//     (a cookie that is stale or malformed gets the same answer: the browser
//     is acting on a session it cannot prove to own);
//  2. resolves the cookie and attaches the [Session] to the request context
//     when it is valid. A request without a valid session goes on anonymously;
//     use [Middleware.Require] to refuse it;
//  3. records activity: a [Middleware.Foreground] request moves the idle end
//     of the session ([Manager.Touch]); a background or cross-site one never
//     does;
//  4. marks a request that carries a session whose idle lifetime ran out
//     with [Idle]: it goes on anonymously. Nothing renews such a session; a
//     foreground top-level page load of an OIDC session is marked for a
//     silent re-authentication with the identity provider
//     ([IdleSession.Reauth]), which signs in anew.
//
// A route of [Middleware.Polls] is marked [AsBackground] for the handlers.
//
// A store failure answers internal, never "anonymous".
func (mw *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mw.Polls != nil && mw.Polls(r) {
			r = r.WithContext(AsBackground(r.Context()))
		}
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !safeMethod(r.Method) && !ValidCSRF(c.Value, r.Header.Get(CSRFHeader)) {
			WriteError(w, proto.CodeForbidden, "missing or invalid "+CSRFHeader+" header", nil)
			return
		}
		s, err := mw.ResolveRequest(r, c.Value)
		var expired *ExpiredError
		switch {
		case err == nil:
			r = r.WithContext(WithSession(r.Context(), s))
		case errors.As(err, &expired):
			idle := IdleSession{Method: expired.Session.AuthMethod}
			if p, ok := strings.CutPrefix(idle.Method, AuthMethodOIDCPrefix); ok && p != "" && Navigation(r) && mw.Foreground(r) {
				idle.Reauth = p
			}
			r = r.WithContext(WithIdle(r.Context(), idle))
		case errors.Is(err, ErrNoSession):
			// anonymous
		default:
			mw.logger().Error("session lookup failed", "error", err)
			WriteError(w, proto.CodeInternal, "the session could not be checked", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Resolve is [Manager.Resolve]: it judges the cookie value alone, without
// recording activity.
func (mw *Middleware) Resolve(ctx context.Context, token string) (*Session, error) {
	return mw.Manager.Resolve(ctx, token)
}

// ResolveRequest resolves the session cookie value token of the request r as
// [Middleware.Wrap] does: a [Middleware.Foreground] request records activity
// ([Manager.Touch]). It returns [ErrNoSession] when the request has no valid
// session; for a session whose idle lifetime ran out the error is an
// *[ExpiredError] (which matches ErrNoSession), so that the caller can answer
// with the quiet idle 401. Any other error is a store failure. The REST API's
// cookie authentication uses it, so that a form posting JSON behaves as a
// page does.
func (mw *Middleware) ResolveRequest(r *http.Request, token string) (*Session, error) {
	s, err := mw.Manager.Resolve(r.Context(), token)
	if err != nil {
		return nil, err
	}
	if mw.Foreground(r) {
		if terr := mw.Manager.Touch(r.Context(), s); terr != nil {
			// The request is still valid; the next one tries again.
			mw.logger().Warn("session activity not recorded", "error", terr)
		}
	}
	return s, nil
}

// Require returns a handler that answers unauthenticated unless the request
// context holds a session. Put it inside [Middleware.Wrap].
func (mw *Middleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			WriteError(w, proto.CodeUnauthenticated, "sign in first", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Protect is Wrap followed by Require.
func (mw *Middleware) Protect(next http.Handler) http.Handler {
	return mw.Wrap(mw.Require(next))
}

// errorBody is the AD-18 error envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    proto.Code     `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// WriteError writes the AD-18 error envelope {"error":{"code","message",
// "details"}} with the HTTP status of code. details may be nil.
func WriteError(w http.ResponseWriter, code proto.Code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	WriteJSON(w, proto.HTTPStatus(code), errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
}

// WriteJSON writes v as a JSON response with the given status. Auth
// responses are never cached.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
