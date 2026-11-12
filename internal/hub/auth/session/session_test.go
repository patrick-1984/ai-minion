package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// memStore is an in-memory Store with switchable failures.
type memStore struct {
	mu       sync.Mutex
	sessions map[string]store.SessionRow // by token hash
	users    map[string]store.UserRow    // by id
	failRead error
	failDel  error
}

func newMemStore(users ...store.UserRow) *memStore {
	m := &memStore{sessions: map[string]store.SessionRow{}, users: map[string]store.UserRow{}}
	for _, u := range users {
		m.users[u.ID] = u
	}
	return m
}

func (m *memStore) CreateSession(_ context.Context, row store.SessionRow, nowMS int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, s := range m.sessions {
		if s.ExpiresAt <= nowMS {
			delete(m.sessions, h)
		}
	}
	m.sessions[row.TokenHash] = row
	return nil
}

func (m *memStore) SessionByTokenHash(_ context.Context, h string) (store.SessionRow, store.UserRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRead != nil {
		return store.SessionRow{}, store.UserRow{}, m.failRead
	}
	s, ok := m.sessions[h]
	if !ok {
		return store.SessionRow{}, store.UserRow{}, store.ErrNotFound
	}
	return s, m.users[s.UserID], nil
}

func (m *memStore) DeleteSession(_ context.Context, h string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDel != nil {
		return false, m.failDel
	}
	_, ok := m.sessions[h]
	delete(m.sessions, h)
	return ok, nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var alice = store.UserRow{ID: "u-alice", Username: "alice", Role: "admin", Active: true, CredentialVersion: 3}

func newTestManager(t *testing.T) (*Manager, *memStore, *fakeClock) {
	t.Helper()
	ms := newMemStore(alice)
	clk := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	return NewManager(ms, WithClock(clk.Now)), ms, clk
}

func TestCreateStoresOnlyTheHashOfA32ByteToken(t *testing.T) {
	m, ms, _ := newTestManager(t)
	is, err := m.Create(context.Background(), alice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(is.Token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q decodes to %d bytes (%v), want 32", is.Token, len(raw), err)
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	if len(ms.sessions) != 1 {
		t.Fatalf("%d rows stored", len(ms.sessions))
	}
	for h, row := range ms.sessions {
		if h != want || row.TokenHash != want {
			t.Errorf("stored hash %q, want sha256 %q", h, want)
		}
		if strings.Contains(row.ID+row.TokenHash+row.UserID, is.Token) {
			t.Error("the cookie value is stored")
		}
	}
	if is.CSRFToken == "" || is.CSRFToken == is.Token {
		t.Errorf("csrf token %q", is.CSRFToken)
	}
}

func TestTokensAreUnique(t *testing.T) {
	m, _, _ := newTestManager(t)
	seen := map[string]bool{}
	for range 50 {
		is, err := m.Create(context.Background(), alice, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if seen[is.Token] {
			t.Fatal("duplicate token")
		}
		seen[is.Token] = true
	}
}

func TestCreateRejectsNonPositiveTTL(t *testing.T) {
	m, _, _ := newTestManager(t)
	if _, err := m.Create(context.Background(), alice, 0); err == nil {
		t.Fatal("ttl 0 accepted")
	}
}

func TestResolve(t *testing.T) {
	m, ms, clk := newTestManager(t)
	ctx := context.Background()
	is, _ := m.Create(ctx, alice, time.Hour)

	s, err := m.Resolve(ctx, is.Token)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != alice.ID || s.Username != "alice" || s.Role != authz.RoleAdmin || s.CSRFToken != is.CSRFToken {
		t.Errorf("session = %+v", s)
	}
	if p := s.Principal(); p.Kind != authz.KindBrowserSession || p.UserID != alice.ID {
		t.Errorf("principal = %+v", p)
	}

	for name, token := range map[string]string{"empty": "", "garbage": "!!!", "short": "AAAA", "unknown": base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := m.Resolve(ctx, token); !errors.Is(err, ErrNoSession) {
			t.Errorf("%s token: err = %v, want ErrNoSession", name, err)
		}
	}

	// Credential change invalidates the session.
	u := alice
	u.CredentialVersion = 4
	ms.users[alice.ID] = u
	if _, err := m.Resolve(ctx, is.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("after a credential change: err = %v", err)
	}
	// Deactivation invalidates the session.
	u = alice
	u.Active = false
	ms.users[alice.ID] = u
	if _, err := m.Resolve(ctx, is.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("deactivated user: err = %v", err)
	}
	ms.users[alice.ID] = alice

	// Expiry.
	if _, err := m.Resolve(ctx, is.Token); err != nil {
		t.Fatalf("restored session: %v", err)
	}
	clk.Add(time.Hour)
	if _, err := m.Resolve(ctx, is.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("at expiry: err = %v", err)
	}
}

func TestResolveStoreFailureIsNotNoSession(t *testing.T) {
	m, ms, _ := newTestManager(t)
	is, _ := m.Create(context.Background(), alice, time.Hour)
	ms.failRead = errors.New("disk on fire")
	_, err := m.Resolve(context.Background(), is.Token)
	if err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want the store error", err)
	}
}

func TestRevoke(t *testing.T) {
	m, ms, _ := newTestManager(t)
	ctx := context.Background()
	is, _ := m.Create(ctx, alice, time.Hour)
	if err := m.Revoke(ctx, is.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve(ctx, is.Token); !errors.Is(err, ErrNoSession) {
		t.Errorf("revoked session resolves: %v", err)
	}
	if err := m.Revoke(ctx, is.Token); err != nil {
		t.Errorf("revoking a missing session: %v", err)
	}
	if err := m.Revoke(ctx, "garbage"); err != nil {
		t.Errorf("revoking a malformed token: %v", err)
	}
	is, _ = m.Create(ctx, alice, time.Hour)
	ms.failDel = errors.New("locked")
	if err := m.Revoke(ctx, is.Token); err == nil {
		t.Fatal("a failed delete was reported as success")
	}
	if _, err := m.Resolve(ctx, is.Token); err != nil {
		t.Errorf("session must still be valid after a failed revoke: %v", err)
	}
}

func TestCSRFTokenIsBoundToTheSession(t *testing.T) {
	m, _, _ := newTestManager(t)
	a, _ := m.Create(context.Background(), alice, time.Hour)
	b, _ := m.Create(context.Background(), alice, time.Hour)
	if a.CSRFToken == b.CSRFToken {
		t.Fatal("two sessions share a CSRF token")
	}
	if !ValidCSRF(a.Token, a.CSRFToken) {
		t.Error("own token rejected")
	}
	if ValidCSRF(a.Token, b.CSRFToken) {
		t.Error("another session's token accepted")
	}
	if ValidCSRF(a.Token, "") || ValidCSRF("garbage", a.CSRFToken) || ValidCSRF(a.Token, a.Token) {
		t.Error("an invalid combination was accepted")
	}
	if CSRFToken("garbage") != "" {
		t.Error("a CSRF token was derived from a malformed cookie value")
	}
}

func TestCookieAttributes(t *testing.T) {
	m, _, _ := newTestManager(t)
	is, _ := m.Create(context.Background(), alice, 2*time.Hour)
	c := Cookie(is, true)
	if c.Name != "minion_session" || c.Value != is.Token || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	if !c.Expires.Equal(is.ExpiresAt) {
		t.Errorf("expires %v, want %v", c.Expires, is.ExpiresAt)
	}
	if c := Cookie(is, false); c.Secure {
		t.Error("Secure set when not requested")
	}
	clr := ClearCookie(true)
	if clr.Name != "minion_session" || clr.Value != "" || clr.MaxAge >= 0 || !clr.HttpOnly || !clr.Secure || clr.SameSite != http.SameSiteLaxMode {
		t.Errorf("clearing cookie = %+v", clr)
	}
}

func TestCookieSecureFollowsThePublicURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"", true}, // the Hub terminates TLS itself
		{"https://hub.example.com", true},
		{"https://localhost:8443", true},
		{"http://localhost:8080", false},
		{"http://LOCALHOST:8080", false},
		{"http://127.0.0.1:8080", false},
		{"http://127.1.2.3:8080", false},
		{"http://[::1]:8080", false},
		{"http://app.localhost:8080", false},
		{"http://hub.example.com", true}, // plain http beyond loopback: stays Secure
		{"http://10.0.0.5:8080", true},
		{"http://localhost.evil.com", true},
		{"::not a url::", true},
	} {
		if got := CookieSecure(tc.url); got != tc.want {
			t.Errorf("CookieSecure(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func (m *memStore) SetSessionAuth(_ context.Context, id string, at int64, method string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, r := range m.sessions {
		if r.ID == id {
			r.AuthAt, r.AuthMethod = at, method
			m.sessions[h] = r
			return true, nil
		}
	}
	return false, nil
}

func TestAPlainSessionIsNeverFreshAndFreshnessExpiresAfterFiveMinutes(t *testing.T) {
	m, _, clk := newTestManager(t)
	ctx := context.Background()
	plain, err := m.Create(ctx, alice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Fresh(clk.Now()) || !plain.AuthAt.IsZero() || plain.AuthMethod != "" {
		t.Errorf("Create made a fresh session: %+v", plain.Session)
	}
	// A method without a proved time records how the session started and is not fresh.
	sso, err := m.CreateAuth(ctx, alice, time.Hour, Auth{Method: "oidc:corp"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Resolve(ctx, sso.Token); got.Fresh(clk.Now()) || got.AuthMethod != "oidc:corp" {
		t.Errorf("method without time: %+v", got)
	}
	checked, err := m.CreateAuth(ctx, alice, time.Hour, Auth{At: clk.Now(), Method: AuthMethodLocal})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.Resolve(ctx, checked.Token)
	for _, c := range []struct {
		after time.Duration
		fresh bool
	}{{0, true}, {4*time.Minute + 59*time.Second, true}, {5 * time.Minute, true}, {5*time.Minute + time.Millisecond, false}, {time.Hour - time.Second, false}} {
		if got.Fresh(clk.Now().Add(c.after)) != c.fresh {
			t.Errorf("after %v: fresh = %v, want %v", c.after, !c.fresh, c.fresh)
		}
	}
	// A time in the future (a clock that went back) is not fresh.
	if got.Fresh(clk.Now().Add(-time.Minute)) {
		t.Error("a session authenticated in the future is fresh")
	}
	var nilSession *Session
	if nilSession.Fresh(clk.Now()) {
		t.Error("a nil session is fresh")
	}
}

func TestMarkAuthenticatedRaisesOnlyTheNamedSession(t *testing.T) {
	m, _, clk := newTestManager(t)
	ctx := context.Background()
	a, _ := m.Create(ctx, alice, time.Hour)
	b, _ := m.Create(ctx, alice, time.Hour)
	clk.Add(time.Minute)
	at, err := m.MarkAuthenticated(ctx, a.ID, "oidc:corp")
	if err != nil || !at.Equal(clk.Now()) {
		t.Fatalf("MarkAuthenticated = %v, %v", at, err)
	}
	ga, _ := m.Resolve(ctx, a.Token)
	gb, _ := m.Resolve(ctx, b.Token)
	if !ga.Fresh(clk.Now()) || ga.AuthMethod != "oidc:corp" || gb.Fresh(clk.Now()) {
		t.Errorf("a = %+v b = %+v", ga, gb)
	}
	if _, err := m.MarkAuthenticated(ctx, "missing", "local"); !errors.Is(err, ErrNoSession) {
		t.Errorf("missing session: %v", err)
	}
	if _, err := m.MarkAuthenticated(ctx, a.ID, ""); err == nil {
		t.Error("an empty method was accepted")
	}
}
