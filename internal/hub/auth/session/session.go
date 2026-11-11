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
	// ActAs is set while the session's admin is acting as another user (Story
	// 6.5). UserID, Username and Role stay those of the admin, the actor.
	ActAs *ActAs
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
	if err := m.store.CreateSession(ctx, row, row.CreatedAt); err != nil {
		return Issued{}, err
	}
	return Issued{Session: toSession(row, user, token), Token: token}, nil
}

func toSession(row store.SessionRow, u store.UserRow, token string) Session {
	return Session{
		ID:                 row.ID,
		UserID:             u.ID,
		Username:           u.Username,
		Role:               authz.Role(u.Role),
		CredentialVersion:  u.CredentialVersion,
		MustChangePassword: u.MustChangePassword,
		ExpiresAt:          store.FromUnixMS(row.ExpiresAt),
		CSRFToken:          CSRFToken(token),
	}
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
