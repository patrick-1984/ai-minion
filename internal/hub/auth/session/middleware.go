package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

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
// Such a request does not extend the idle lifetime of the session, and a
// session that ended (and could not be renewed) gets a plain 401 without a
// redirect, so the screen stays as it is.
const BackgroundHeader = "X-Minion-Background"

// Background reports whether r is a background request: it carries
// [BackgroundHeader] "1", or it opens an event stream (a live page
// reconnecting its stream is not the person acting).
func Background(r *http.Request) bool {
	return r.Header.Get(BackgroundHeader) == "1" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// RenewOutcome is the answer of a [Renewer].
type RenewOutcome int

const (
	// RenewFailed means the identity provider no longer vouches for the user
	// (a revoked or rejected refresh token, a deactivated account, no role
	// left, no refresh token stored): the session ends.
	RenewFailed RenewOutcome = iota
	// RenewOK means the identity provider vouched for the user again with
	// fresh claims and the roles still give access: the session goes on.
	RenewOK
	// RenewUnavailable means nothing was learned (the identity provider could
	// not be reached): the session is served within [RenewGrace].
	RenewUnavailable
)

// Renewer re-validates the user of an expired OIDC session with the identity
// provider. The OIDC refresher implements it; it records the audit rows.
type Renewer interface {
	RenewSession(ctx context.Context, s *Session) RenewOutcome
}

// renewRetry is how long after an unreachable identity provider the next
// renewal of the same session is attempted; requests in between are served
// within the grace without asking the provider again.
const renewRetry = 30 * time.Second

// renewTimeout bounds one renewal, whatever the request's own context does:
// a refresh that the provider answered must still be stored when the browser
// went away.
const renewTimeout = 30 * time.Second

// Middleware authenticates browser sessions and enforces CSRF protection.
type Middleware struct {
	Manager *Manager
	// Log receives store failures; nil means [slog.Default].
	Log *slog.Logger
	// Renew, when set, renews an OIDC session whose idle lifetime ran out
	// (see [Middleware.Wrap]). Nil means an expired session is signed out.
	Renew Renewer

	mu      sync.Mutex
	locks   map[string]*renewLock
	backoff map[string]time.Time
}

type renewLock struct {
	mu   sync.Mutex
	refs int
}

// lockRenew serialises the renewals of one session: concurrent requests of
// the same browser wait for the first one, and then find the session renewed.
func (mw *Middleware) lockRenew(id string) func() {
	mw.mu.Lock()
	if mw.locks == nil {
		mw.locks = map[string]*renewLock{}
	}
	l := mw.locks[id]
	if l == nil {
		l = &renewLock{}
		mw.locks[id] = l
	}
	l.refs++
	mw.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		mw.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(mw.locks, id)
		}
		mw.mu.Unlock()
	}
}

func (mw *Middleware) backoffUntil(id string) time.Time {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	return mw.backoff[id]
}

func (mw *Middleware) setBackoff(id string, until time.Time) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if mw.backoff == nil {
		mw.backoff = map[string]time.Time{}
	}
	if until.IsZero() {
		delete(mw.backoff, id)
		return
	}
	now := mw.Manager.Now()
	for k, t := range mw.backoff { // keep the map small: drop what is over
		if !t.After(now) {
			delete(mw.backoff, k)
		}
	}
	mw.backoff[id] = until
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
//  3. records activity: a request that is not [Background] moves the idle end
//     of the session ([Manager.Touch]);
//  4. renews an OIDC session whose idle lifetime ran out, when [Middleware.Renew]
//     is set (a [Background] request too: the renewal is a check with the
//     identity provider, not activity): the identity provider is
//     asked with the stored refresh token, and when it vouches for the user
//     the session goes on with the same cookie and the request is served as
//     if it had never expired. An unreachable provider is tolerated for
//     [RenewGrace] after the idle end; a refusal signs the session out.
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
		s, err := mw.ResolveRequest(r, c.Value)
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

// Resolve is [Manager.Resolve]: it judges the cookie value alone, without
// recording activity or renewing.
func (mw *Middleware) Resolve(ctx context.Context, token string) (*Session, error) {
	return mw.Manager.Resolve(ctx, token)
}

// ResolveRequest resolves the session cookie value token of the request r as
// [Middleware.Wrap] does: a request that is not [Background] records activity
// ([Manager.Touch]), and an expired OIDC session is renewed when
// [Middleware.Renew] is set. It returns [ErrNoSession] when the request has no
// valid session, and any other error when the store fails. The REST API's
// cookie authentication uses it, so that a form posting JSON behaves as a
// page does.
func (mw *Middleware) ResolveRequest(r *http.Request, token string) (*Session, error) {
	s, err := mw.Manager.Resolve(r.Context(), token)
	var expired *ExpiredError
	switch {
	case err == nil:
		if !Background(r) {
			if terr := mw.Manager.Touch(r.Context(), s); terr != nil {
				// The request is still valid; the next one tries again.
				mw.logger().Warn("session activity not recorded", "error", terr)
			}
		}
		return s, nil
	case errors.As(err, &expired):
		if rs := mw.renew(r, token, expired); rs != nil {
			return rs, nil
		}
		return nil, ErrNoSession
	default:
		return nil, err
	}
}

// renew tries to renew the expired session of r. It returns the session to
// serve the request with, or nil for "signed out".
func (mw *Middleware) renew(r *http.Request, token string, exp *ExpiredError) *Session {
	// A background request renews too: the renewal is itself a check with the
	// identity provider, and refusing it would clear what the page holds (the
	// inspector drops its kept frames on a lost sign-in, a live list stops).
	// It never extends a valid session (see Wrap).
	if mw.Renew == nil || !strings.HasPrefix(exp.Session.AuthMethod, AuthMethodOIDCPrefix) {
		return nil
	}
	id := exp.Session.ID
	unlock := mw.lockRenew(id)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), renewTimeout)
	defer cancel()

	// Another request of the same browser may have renewed it meanwhile.
	s, err := mw.Manager.Resolve(ctx, token)
	var again *ExpiredError
	switch {
	case err == nil:
		return s
	case errors.As(err, &again):
		exp = again
	default:
		return nil // gone, or a store failure: signed out
	}

	now := mw.Manager.Now()
	outcome := RenewUnavailable
	if !now.Before(mw.backoffUntil(id)) {
		outcome = mw.Renew.RenewSession(ctx, &exp.Session)
	}
	switch outcome {
	case RenewOK:
		mw.setBackoff(id, time.Time{})
		if err := mw.Manager.Renewed(ctx, &exp.Session); err != nil {
			mw.logger().Error("session renewal could not be stored", "session", id, "error", err)
			return nil
		}
		s, err := mw.Manager.Resolve(ctx, token)
		if err != nil {
			return nil
		}
		return s
	case RenewUnavailable:
		if now.Before(exp.Session.ExpiresAt.Add(RenewGrace)) {
			if !now.Before(mw.backoffUntil(id)) {
				mw.setBackoff(id, now.Add(renewRetry))
			}
			mw.logger().Warn("identity provider unreachable; the expired session is served within the grace", "session", id)
			return &exp.Session
		}
		mw.setBackoff(id, time.Time{})
		return nil
	default:
		mw.setBackoff(id, time.Time{})
		if err := mw.Manager.Revoke(ctx, token); err != nil {
			mw.logger().Error("session could not be ended after a refused renewal", "session", id, "error", err)
		}
		return nil
	}
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
