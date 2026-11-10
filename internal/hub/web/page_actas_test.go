package web

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

var actAsNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// actAsSessions attaches a session of admin "alice" that acts as target (empty:
// not acting) until expires.
type actAsSessions struct {
	target  string
	expires time.Time
}

func (a actAsSessions) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := &session.Session{ID: "s1", UserID: "u-alice", Username: "alice", Role: authz.RoleAdmin, CSRFToken: "csrf"}
		if a.target != "" {
			s.ActAs = &session.ActAs{TargetID: "u-target", TargetUsername: a.target, ExpiresAt: a.expires}
		}
		next.ServeHTTP(w, r.WithContext(session.WithSession(r.Context(), s)))
	})
}

func actAsServer(t *testing.T, layer SessionLayer, reg *Registry) *Server {
	t.Helper()
	s, err := New(Config{Registry: reg, Sessions: layer, Templates: testTemplates(t), ContextKey: testContextKey,
		Secure: func(*http.Request) bool { return false }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func actAsRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterBanner(ActAsBanner(func() time.Time { return actAsNow })); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestActAsBannerNamesTargetAdminAndRemainingTime(t *testing.T) {
	s := actAsServer(t, actAsSessions{target: "bob", expires: actAsNow.Add(42 * time.Minute)}, actAsRegistry(t))
	body := get(t, s, "/").Body.String()
	mustHave(t, body,
		`<div class="banner banner-warning" role="status"><span>Acting as bob (admin alice) - 42 min left</span>`,
		`<form data-json-action="/api/v1/ops/act_as_stop"><button type="submit" class="btn-secondary">Exit</button></form>`)
}

func TestActAsBannerRoundsUpAndNeverShowsZero(t *testing.T) {
	for _, tc := range []struct {
		left time.Duration
		want string
	}{
		{time.Hour, "60 min left"},
		{61 * time.Second, "2 min left"},
		{time.Second, "1 min left"},
	} {
		s := actAsServer(t, actAsSessions{target: "bob", expires: actAsNow.Add(tc.left)}, actAsRegistry(t))
		mustHave(t, get(t, s, "/").Body.String(), tc.want)
	}
}

func TestActAsBannerIsAbsentWhenNotActing(t *testing.T) {
	s := actAsServer(t, actAsSessions{}, actAsRegistry(t))
	mustNotHave(t, get(t, s, "/").Body.String(), "Acting as", "act_as_stop")
	// Nor for an anonymous visitor.
	anon := actAsServer(t, fakeSessions{}, actAsRegistry(t))
	mustNotHave(t, get(t, anon, "/login").Body.String(), "Acting as")
}

func TestActAsBannerEscapesNames(t *testing.T) {
	s := actAsServer(t, actAsSessions{target: `<script>alert(1)</script>`, expires: actAsNow.Add(time.Hour)}, actAsRegistry(t))
	body := get(t, s, "/").Body.String()
	mustNotHave(t, body, "<script>alert(1)</script>")
	mustHave(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
}

func TestActAsBannerIsRegisteredInTheDefaultRegistry(t *testing.T) {
	s := actAsServer(t, actAsSessions{target: "bob", expires: time.Now().Add(time.Hour)}, DefaultRegistry)
	mustHave(t, get(t, s, "/").Body.String(), "Acting as bob (admin alice)")
}

func TestActAsStopURLIsTheCatalogRoute(t *testing.T) {
	if ActAsStopURL != "/api/v1/ops/act_as_stop" || !sameOriginPath(ActAsStopURL) {
		t.Errorf("ActAsStopURL = %q", ActAsStopURL)
	}
}
