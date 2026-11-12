package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// Names and sizes of the session mechanism.
const (
	// CookieName is the name of the session cookie.
	CookieName = "minion_session"
	// CSRFHeader is the request header that carries the CSRF token.
	CSRFHeader = "X-Minion-CSRF"
	// TokenBytes is the size of the random cookie value before encoding.
	TokenBytes = 32
)

// csrfLabel separates the CSRF derivation from every other use of the token.
const csrfLabel = "minion-csrf-v1"

// ErrNoSession is returned by [Manager.Resolve] when the cookie value does not
// name a valid session: unknown, expired, of a deactivated user or issued
// before the last credential change.
var ErrNoSession = errors.New("session: no valid session")

// Store is the persistence port of the [Manager]; *store.Store implements it.
type Store interface {
	CreateSession(ctx context.Context, row store.SessionRow, nowMS int64) error
	SessionByTokenHash(ctx context.Context, tokenHash string) (store.SessionRow, store.UserRow, error)
	DeleteSession(ctx context.Context, tokenHash string) (bool, error)
}

var _ Store = (*store.Store)(nil)

// Session is a valid browser session as the rest of the Hub sees it. It holds
// no secret except CSRFToken, which belongs to the session's own browser.
type Session struct {
	ID       string
	UserID   string
	Username string
	Role     authz.Role
	// CredentialVersion is the credential_version of the account that the
	// session was resolved against. The API-key issuance check compares it
	// inside the insert transaction (F-07).
	CredentialVersion  int64
	MustChangePassword bool
	ExpiresAt          time.Time
	// CSRFToken is the token the browser must send in X-Minion-CSRF.
	CSRFToken string
	// AuthAt is when the server last checked a credential for this session
	// (a local password, or an OIDC callback with a verified auth_time); the
	// zero time means it never did, and the session is then never fresh.
	AuthAt time.Time
	// AuthMethod is "local", "oidc:<provider id>" or "".
	AuthMethod string
	// ActAs is set while the session's admin is acting as another user (Story
	// 6.5). UserID, Username and Role stay those of the admin, the actor.
	ActAs *ActAs
}

// ReauthWindow is how recent the last credential check of a session must be
// for an identity operation (section 8.1 item 2).
const ReauthWindow = 5 * time.Minute

// Auth methods of a session.
const (
	// AuthMethodLocal is a verified local password.
	AuthMethodLocal = "local"
	// AuthMethodOIDCPrefix starts "oidc:<provider id>".
	AuthMethodOIDCPrefix = "oidc:"
)

// Auth is the authentication a session starts with or is raised to. The zero
// value means "no verified authentication".
type Auth struct {
	At     time.Time
	Method string
}

// Fresh reports whether the session's last credential check is at most
// [ReauthWindow] old at now. A session without one is never fresh, and a time
// in the future (a clock that went back) is not fresh either.
func (s *Session) Fresh(now time.Time) bool {
	if s == nil || s.AuthAt.IsZero() || s.AuthMethod == "" {
		return false
	}
	age := now.Sub(s.AuthAt)
	return age >= 0 && age <= ReauthWindow
}

// Principal returns the authz principal of the session (AD-10): the act-as
// principal while the session acts as another user, else the browser-session
// principal of its user.
func (s *Session) Principal() authz.Principal {
	if s.ActAs != nil {
		return authz.NewActAsPrincipal(s.UserID, s.ActAs.TargetID, s.ActAs.ExpiresAt)
	}
	return authz.NewSessionPrincipal(s.UserID)
}

// Issued is a freshly created session together with the cookie value.
type Issued struct {
	Session
	// Token is the cookie value. It exists only here and in the browser.
	Token string
}

// Manager creates, resolves and revokes browser sessions. It is safe for
// concurrent use.
type Manager struct {
	store Store
	now   func() time.Time
	rand  io.Reader
	newID func() string
	// actAs is the act-as port. It is nil when the store does not offer one,
	// and then no session acts as another user.
	actAs ActAsStore
}

// Option changes how [NewManager] builds a Manager. They exist for tests.
type Option func(*Manager)

// WithClock sets the time source.
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// WithRandom sets the source of cookie values.
func WithRandom(r io.Reader) Option { return func(m *Manager) { m.rand = r } }

// NewManager returns a Manager on st.
func NewManager(st Store, opts ...Option) *Manager {
	m := &Manager{store: st, now: time.Now, rand: rand.Reader, newID: store.NewID}
	if a, ok := st.(ActAsStore); ok {
		m.actAs = a // *store.Store offers both ports
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// HashToken returns the value stored for a cookie value: the lower-case hex
// SHA-256 of the raw 32 bytes. It reports false for a value that is not a
// well-formed token, which cannot name any session.
func HashToken(token string) (string, bool) {
	raw, ok := decodeToken(token)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

func decodeToken(token string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != TokenBytes {
		return nil, false
	}
	return raw, true
}

// CSRFToken returns the CSRF token bound to the session whose cookie value is
// token, or "" for a malformed token.
func CSRFToken(token string) string {
	raw, ok := decodeToken(token)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(csrfLabel))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ValidCSRF reports whether presented is the CSRF token of the session whose
// cookie value is token. The comparison is constant-time.
func ValidCSRF(token, presented string) bool {
	want := CSRFToken(token)
	if want == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}

// Create starts a session of user that lasts ttl and returns it with the
// cookie value.
func (m *Manager) Create(ctx context.Context, user store.UserRow, ttl time.Duration) (Issued, error) {
	return m.CreateAuth(ctx, user, ttl, Auth{})
}

// CreateAuth is [Manager.Create] for a session whose start was a credential
// check that the server itself performed (a verified local password, an OIDC
// callback with a verifiable auth_time). The zero Auth starts a session that
// is never fresh.
func (m *Manager) CreateAuth(ctx context.Context, user store.UserRow, ttl time.Duration, auth Auth) (Issued, error) {
	if ttl <= 0 {
		return Issued{}, errors.New("session: ttl must be positive")
	}
	raw := make([]byte, TokenBytes)
	if _, err := io.ReadFull(m.rand, raw); err != nil {
		return Issued{}, fmt.Errorf("session: read random token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash, _ := HashToken(token)
	now := m.now()
	row := store.SessionRow{
		ID:                m.newID(),
		TokenHash:         hash,
		UserID:            user.ID,
		CredentialVersion: user.CredentialVersion,
		CreatedAt:         store.UnixMS(now),
		ExpiresAt:         store.UnixMS(now.Add(ttl)),
	}
	// The method says how the session was started even when no credential
	// check can be proved (auth_at stays 0): the page then offers the right
	// re-authentication.
	row.AuthMethod = auth.Method
	if !auth.At.IsZero() && auth.Method != "" {
		row.AuthAt = store.UnixMS(auth.At)
	}
	if err := m.store.CreateSession(ctx, row, row.CreatedAt); err != nil {
		return Issued{}, err
	}
	return Issued{Session: toSession(row, user, token), Token: token}, nil
}

func toSession(row store.SessionRow, u store.UserRow, token string) Session {
	s := Session{
		ID:                 row.ID,
		UserID:             u.ID,
		Username:           u.Username,
		Role:               authz.Role(u.Role),
		CredentialVersion:  u.CredentialVersion,
		MustChangePassword: u.MustChangePassword,
		ExpiresAt:          store.FromUnixMS(row.ExpiresAt),
		CSRFToken:          CSRFToken(token),
		AuthMethod:         row.AuthMethod,
	}
	if row.AuthAt > 0 {
		s.AuthAt = store.FromUnixMS(row.AuthAt)
	}
	return s
}

// Resolve returns the valid session named by the cookie value token. It
// returns [ErrNoSession] when there is none, and any other error when the
// store fails (the caller must not treat that as "logged out").
func (m *Manager) Resolve(ctx context.Context, token string) (*Session, error) {
	hash, ok := HashToken(token)
	if !ok {
		return nil, ErrNoSession
	}
	row, user, err := m.store.SessionByTokenHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if !m.now().Before(store.FromUnixMS(row.ExpiresAt)) || !user.Active || user.Role == "" || row.CredentialVersion != user.CredentialVersion {
		return nil, ErrNoSession
	}
	s := toSession(row, user, token)
	if err := m.attachActAs(ctx, &s, row, user); err != nil {
		return nil, err
	}
	return &s, nil
}

// Revoke deletes the session named by token. A token that names no session is
// not an error (the session is gone, which is what the caller wanted); a
// failed delete is, and the caller must report it (AD-10: logout reports a
// failed server-side revocation as a failure).
func (m *Manager) Revoke(ctx context.Context, token string) error {
	hash, ok := HashToken(token)
	if !ok {
		return nil
	}
	if _, err := m.store.DeleteSession(ctx, hash); err != nil {
		return fmt.Errorf("session: revoke: %w", err)
	}
	return nil
}

// Cookie returns the session cookie for a freshly issued session.
func Cookie(is Issued, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    is.Token,
		Path:     "/",
		Expires:  is.ExpiresAt,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie returns a cookie that makes the browser forget the session.
func ClearCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// CookieSecure decides the Secure attribute of the session cookie from
// MINION_PUBLIC_URL: the cookie is Secure unless the public URL is plain-http
// loopback (http://localhost, http://127.0.0.1, http://[::1]), where browsers
// accept insecure cookies for development. An empty public URL means the Hub
// serves HTTPS itself, so the cookie is Secure.
func CookieSecure(publicURL string) bool {
	if strings.TrimSpace(publicURL) == "" {
		return true
	}
	u, err := url.Parse(publicURL)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return true
	}
	return !isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}

// AuthStore is the optional persistence port of [Manager.MarkAuthenticated];
// *store.Store offers it.
type AuthStore interface {
	SetSessionAuth(ctx context.Context, sessionID string, authAt int64, method string) (bool, error)
}

// ErrNoAuthStore is returned by [Manager.MarkAuthenticated] when the store
// cannot record an authentication.
var ErrNoAuthStore = errors.New("session: the store cannot record an authentication")

// MarkAuthenticated raises the session to a fresh authentication at now with
// method. Only the server calls it, after a credential check it performed
// itself (the local password re-entry, the OIDC re-authentication callback).
// It returns [ErrNoSession] when the session no longer exists.
func (m *Manager) MarkAuthenticated(ctx context.Context, sessionID, method string) (time.Time, error) {
	return m.MarkAuthenticatedAt(ctx, sessionID, method, time.Time{})
}

// MarkAuthenticatedAt is [Manager.MarkAuthenticated] for a credential check
// that happened at a known time (the verified auth_time of an identity
// provider). A zero or future at is clamped to now. It returns the time stored.
func (m *Manager) MarkAuthenticatedAt(ctx context.Context, sessionID, method string, at time.Time) (time.Time, error) {
	a, ok := m.store.(AuthStore)
	if !ok {
		return time.Time{}, ErrNoAuthStore
	}
	if method == "" {
		return time.Time{}, errors.New("session: an authentication needs a method")
	}
	now := m.now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	found, err := a.SetSessionAuth(ctx, sessionID, store.UnixMS(at), method)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, ErrNoSession
	}
	return at, nil
}

// Now returns the manager's clock; handlers that judge freshness use it so
// that tests can drive the time.
func (m *Manager) Now() time.Time { return m.now() }
