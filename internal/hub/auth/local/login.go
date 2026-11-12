package local

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/httpbody"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
)

// Audit vocabulary of login and logout (AD-20). No event carries a password.
const (
	AuditActionLogin  = "login"
	AuditActionLogout = "logout"

	OutcomeSuccess     = "success"
	OutcomeFailed      = "failed"
	OutcomeInactive    = "inactive"
	OutcomeRateLimited = "rate_limited"

	// anonymousActor is the audit actor of an attempt that named no account.
	anonymousActor = "anonymous"
)

// maxLoginBody bounds the body of a login request.
const maxLoginBody = 8 << 10

// LoginStore is the persistence port of the [Authenticator]; *store.Store
// implements it.
type LoginStore interface {
	UserByIdentifier(ctx context.Context, ident string) (store.UserRow, error)
}

var _ LoginStore = (*store.Store)(nil)

// Limiter is the part of the shared rate limiter that login uses;
// *ratelimit.Limiter implements it.
type Limiter interface {
	Allow(policy ratelimit.Policy, ip netip.Addr, target string) (bool, time.Duration)
	Backoff(account string) time.Duration
	BackoffFail(account string) time.Duration
	BackoffReset(account string)
}

var _ Limiter = (*ratelimit.Limiter)(nil)

// Auditor records audit events; *audit.Log implements it.
type Auditor interface {
	Record(ctx context.Context, ev audit.Event) error
}

var _ Auditor = (*audit.Log)(nil)

// ErrInvalidCredentials is returned for a wrong password, an unknown account
// and a deactivated account alike, so that the answer does not reveal which
// accounts exist.
var ErrInvalidCredentials = errors.New("local: invalid credentials")

// RateLimitedError is returned when the shared limiter or the per-account
// back-off refuses a login attempt. The password was not checked.
type RateLimitedError struct {
	// RetryAfter is how long the client should wait.
	RetryAfter time.Duration
}

// Error implements error.
func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("local: too many login attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Credentials is one login attempt.
type Credentials struct {
	// Identifier is the user name or e-mail address.
	Identifier string
	Password   string
	// IP is the client address, for the rate limiter.
	IP netip.Addr
}

// Authenticator checks local credentials behind the shared rate limiter and
// audits every attempt that does not succeed. The caller audits success after
// it has issued the session ([Handler] does).
type Authenticator struct {
	Store   LoginStore
	Hasher  *Hasher
	Limiter Limiter
	Audit   Auditor
	Log     *slog.Logger

	// targetKey keys the digest of unknown names in the audit log. It is read
	// from crypto/rand once, on first use, and never persisted or logged.
	targetOnce sync.Once
	targetKey  [32]byte
}

func (a *Authenticator) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Authenticate returns the user whose credentials c are. The order of checks
// is: look the account up; apply the "login" policy (per IP and per account)
// and the per-account back-off; verify the password. An unknown account costs
// the same Argon2id time as a known one and shares the limiter, keyed by the
// account id when there is one and by the name otherwise, so that neither
// timing nor limits reveal which accounts exist. Both the user name and the
// e-mail address of an account count against the same account.
//
// It returns [ErrInvalidCredentials], a [*RateLimitedError], or an internal
// error. Every attempt that fails is audited without the password.
func (a *Authenticator) Authenticate(ctx context.Context, c Credentials) (store.UserRow, error) {
	ident := strings.TrimSpace(c.Identifier)
	var (
		user  store.UserRow
		found bool
	)
	if ident != "" && utf8.ValidString(ident) && len(ident) <= 320 {
		u, err := a.Store.UserByIdentifier(ctx, ident)
		switch {
		case err == nil:
			user, found = u, true
		case !errors.Is(err, store.ErrNotFound):
			return store.UserRow{}, fmt.Errorf("local: look up account: %w", err)
		}
	}
	key := "name:" + ident
	actor, target := anonymousActor, a.unknownTarget(ident)
	if found {
		key, actor, target = "user:"+user.ID, user.ID, sanitizeTarget(user.Username)
	}

	if ok, wait := a.Limiter.Allow(ratelimit.PolicyLogin, c.IP, key); !ok {
		a.record(ctx, actor, target, OutcomeRateLimited)
		return store.UserRow{}, &RateLimitedError{RetryAfter: wait}
	}
	if wait := a.Limiter.Backoff(key); wait > 0 {
		a.record(ctx, actor, target, OutcomeRateLimited)
		return store.UserRow{}, &RateLimitedError{RetryAfter: wait}
	}

	fail := func(outcome string) (store.UserRow, error) {
		a.Limiter.BackoffFail(key)
		a.record(ctx, actor, target, outcome)
		return store.UserRow{}, ErrInvalidCredentials
	}

	if !found || user.PasswordHash == "" {
		if err := a.Hasher.Burn(ctx, c.Password); err != nil {
			return store.UserRow{}, err
		}
		return fail(OutcomeFailed)
	}
	ok, _, err := a.Hasher.Verify(ctx, c.Password, user.PasswordHash)
	if err != nil {
		if errors.Is(err, ErrBadHash) {
			a.logger().Error("stored password hash is malformed", "user_id", user.ID)
		}
		return store.UserRow{}, fmt.Errorf("local: verify password: %w", err)
	}
	if !ok {
		return fail(OutcomeFailed)
	}
	if !user.Active {
		// The password was right, but a deactivated account cannot sign in. The
		// caller sees the same answer as for a wrong password.
		return fail(OutcomeInactive)
	}
	if user.Role == "" {
		// An account without any effective grant has no access (F-07).
		return fail(OutcomeInactive)
	}
	a.Limiter.BackoffReset(key)
	return user, nil
}

// record audits one failed attempt. A failing audit write does not change the
// refusal, but it is logged, so it is never silent.
func (a *Authenticator) record(ctx context.Context, actor, target, outcome string) {
	err := a.Audit.Record(context.WithoutCancel(ctx), audit.Event{Actor: actor, Action: AuditActionLogin, Target: target, Outcome: outcome})
	if err != nil {
		a.logger().Error("audit of a login attempt failed", "outcome", outcome, "error", err)
	}
}

// unknownTarget is the audit target of an attempt for a name that matches no
// account: a short digest, never the name, because a mistyped password in the
// user name field must not reach the audit log. The digest is an HMAC-SHA256
// under a random per-process key that is never stored, so a reader of the
// audit log cannot test a password dictionary against it offline.
func (a *Authenticator) unknownTarget(ident string) string {
	a.targetOnce.Do(func() {
		// crypto/rand.Read does not return an error (it terminates the
		// process when the system has no entropy).
		_, _ = rand.Read(a.targetKey[:])
	})
	mac := hmac.New(sha256.New, a.targetKey[:])
	mac.Write([]byte(strings.ToLower(ident)))
	return "unknown:" + hex.EncodeToString(mac.Sum(nil)[:4])
}

// Handler serves POST /auth/login and POST /auth/logout.
type Handler struct {
	Auth     *Authenticator
	Sessions *session.Manager
	Audit    Auditor
	// TTL returns the session lifetime; it is called on every login, so a live
	// setting applies without a restart.
	TTL func(ctx context.Context) (time.Duration, error)
	// Secure decides the Secure attribute of the cookie for a request.
	Secure func(r *http.Request) bool
	// ClientIP returns the client address for the rate limiter.
	ClientIP func(r *http.Request) netip.Addr
	// BodyTimeout bounds the time a client may take to send the login body;
	// zero means httpbody.DefaultTimeout. The Hub also guards every request
	// with a body before routing (internal/hub/httpbody); the login arms its own
	// deadline as well, so that it is safe behind any server.
	BodyTimeout time.Duration
	Log         *slog.Logger
}

// Register adds the endpoints to mux. Both go through a
// [http.CrossOriginProtection]: a browser request that another site caused (by
// Sec-Fetch-Site, or by an Origin that does not match the Host) is refused with
// forbidden before anything else happens. That stops login CSRF, where a
// foreign page signs the victim's browser into the attacker's account, and a
// forged logout. Requests without either header (curl, other programs) pass.
// Logout also goes through mw, which enforces the session-bound CSRF token for
// a request that carries a session cookie. Login does not: the caller has no
// session yet, and a stale cookie must not lock anybody out of signing in.
//
// trustedOrigins are extra origins ("https://hub.example.com") whose browser
// requests count as same-origin, for a Hub behind a proxy that rewrites the
// Host header. An invalid origin is an error.
func (h *Handler) Register(mux *http.ServeMux, mw *session.Middleware, trustedOrigins ...string) error {
	cop := http.NewCrossOriginProtection()
	for _, o := range trustedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return fmt.Errorf("local: trusted origin %q: %w", o, err)
		}
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		session.WriteError(w, proto.CodeForbidden, "cross-origin request refused", nil)
	}))
	mux.Handle("POST /auth/login", cop.Handler(http.HandlerFunc(h.Login)))
	mux.Handle("POST /auth/logout", cop.Handler(mw.Wrap(http.HandlerFunc(h.Logout))))
	mux.Handle("POST /auth/reauth", cop.Handler(mw.Protect(http.HandlerFunc(h.Reauth))))
	return nil
}

func (h *Handler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// loginRequest is the body of POST /auth/login, as JSON or as a form.
type loginRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// loginResponse is the success body.
type loginResponse struct {
	User struct {
		ID                 string `json:"id"`
		Username           string `json:"username"`
		Role               string `json:"role"`
		MustChangePassword bool   `json:"must_change_password"`
	} `json:"user"`
	CSRFToken string        `json:"csrf_token"`
	ExpiresAt ops.Timestamp `json:"expires_at"`
}

func parseLogin(r *http.Request) (loginRequest, error) {
	var req loginRequest
	body := http.MaxBytesReader(nil, r.Body, maxLoginBody)
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "application/x-www-form-urlencoded" {
		raw, err := io.ReadAll(body)
		if err != nil {
			return req, err
		}
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			return req, err
		}
		req.Username, req.Email, req.Password = vals.Get("username"), vals.Get("email"), vals.Get("password")
		return req, nil
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	// The body is complete only at its end, and only if nothing but white
	// space follows the object. body is capped by maxLoginBody in total and
	// returns an error, not an end, at the cap, so a body that goes on or that
	// stops is an error here (the deadline cuts it) and never a login.
	rest, err := io.ReadAll(io.MultiReader(dec.Buffered(), body))
	if err != nil {
		return req, err
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return req, errors.New("local: unexpected data after the login object")
	}
	return req, nil
}

// Login handles POST /auth/login: verify, then issue the session cookie and
// its CSRF token.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	// The body is read before anything else, by a client nobody has
	// authenticated: it gets a deadline first. The deadline is lifted only by
	// the end of the body: parseLogin reads to the end, because one complete
	// JSON object says nothing about the bytes a longer declared body still owes.
	httpbody.Arm(w, r, h.BodyTimeout)
	req, err := parseLogin(r)
	ident := req.Username
	if ident == "" {
		ident = req.Email
	}
	if err != nil || ident == "" || req.Password == "" {
		session.WriteError(w, proto.CodeInvalidArgument, "send a user name (or e-mail) and a password", nil)
		return
	}
	ttl, err := h.TTL(r.Context())
	if err != nil {
		h.fail(w, "session lifetime", err)
		return
	}
	user, err := h.Auth.Authenticate(r.Context(), Credentials{Identifier: ident, Password: req.Password, IP: h.ClientIP(r)})
	var limited *RateLimitedError
	switch {
	case errors.As(err, &limited):
		secs := int(limited.RetryAfter/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		session.WriteError(w, proto.CodeRateLimited, "too many sign-in attempts; retry later",
			map[string]any{"retry_after_seconds": secs})
		return
	case errors.Is(err, ErrInvalidCredentials):
		session.WriteError(w, proto.CodeUnauthenticated, "invalid credentials", nil)
		return
	case err != nil:
		h.fail(w, "sign-in", err)
		return
	}

	// The password was just verified by this server: the session starts fresh.
	issued, err := h.Sessions.CreateAuth(r.Context(), user, ttl, session.Auth{At: h.Sessions.Now(), Method: session.AuthMethodLocal})
	if err != nil {
		h.fail(w, "create session", err)
		return
	}
	// The success audit row is written before the cookie is handed out: when
	// it cannot be written the session is revoked again and the login fails.
	if err := h.Audit.Record(r.Context(), audit.Event{Actor: user.ID, Action: AuditActionLogin, Target: sanitizeTarget(user.Username), Outcome: OutcomeSuccess}); err != nil {
		_ = h.Sessions.Revoke(context.WithoutCancel(r.Context()), issued.Token)
		h.fail(w, "audit login", err)
		return
	}

	http.SetCookie(w, session.Cookie(issued, h.Secure(r)))
	var resp loginResponse
	resp.User.ID, resp.User.Username, resp.User.Role, resp.User.MustChangePassword =
		user.ID, user.Username, user.Role, user.MustChangePassword
	resp.CSRFToken, resp.ExpiresAt = issued.CSRFToken, ops.Timestamp(issued.ExpiresAt)
	session.WriteJSON(w, http.StatusOK, resp)
}

// Logout handles POST /auth/logout: delete the session row, then clear the
// cookie. When the delete fails the response says so (500) and the cookie is
// kept, so the user can retry; success is never reported for a session that is
// still valid on the server.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		session.WriteJSON(w, http.StatusOK, map[string]any{"status": "signed_out"})
		return
	}
	sess, _ := session.FromContext(r.Context())
	if err := h.Sessions.Revoke(r.Context(), c.Value); err != nil {
		if sess != nil {
			h.audit(r.Context(), sess.UserID, sess.Username, AuditActionLogout, OutcomeFailed)
		}
		h.fail(w, "revoke session", err)
		return
	}
	if sess != nil {
		h.audit(r.Context(), sess.UserID, sess.Username, AuditActionLogout, OutcomeSuccess)
	}
	http.SetCookie(w, session.ClearCookie(h.Secure(r)))
	session.WriteJSON(w, http.StatusOK, map[string]any{"status": "signed_out"})
}

func (h *Handler) audit(ctx context.Context, actor, target, action, outcome string) {
	if err := h.Audit.Record(context.WithoutCancel(ctx), audit.Event{Actor: actor, Action: action, Target: sanitizeTarget(target), Outcome: outcome}); err != nil {
		h.logger().Error("audit failed", "action", action, "outcome", outcome, "error", err)
	}
}

// sanitizeTarget keeps an audit target within the audit rules.
func sanitizeTarget(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// fail logs err and answers internal; the response carries no detail.
func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.logger().Error("auth request failed", "step", what, "error", err)
	session.WriteError(w, proto.CodeInternal, "the request could not be completed", nil)
}
