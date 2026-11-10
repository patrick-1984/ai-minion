package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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

// Middleware authenticates browser sessions and enforces CSRF protection.
type Middleware struct {
	Manager *Manager
	// Log receives store failures; nil means [slog.Default].
	Log *slog.Logger
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
//     use [Middleware.Require] to refuse it.
//
// A store failure answers internal, never "anonymous".
func (mw *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !safeMethod(r.Method) && !ValidCSRF(c.Value, r.Header.Get(CSRFHeader)) {
			WriteError(w, proto.CodeForbidden, "missing or invalid "+CSRFHeader+" header", nil)
			return
		}
		s, err := mw.Manager.Resolve(r.Context(), c.Value)
		switch {
		case err == nil:
			r = r.WithContext(WithSession(r.Context(), s))
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
