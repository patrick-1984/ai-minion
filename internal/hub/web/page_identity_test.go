package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func tsp(t time.Time) *ops.Timestamp { v := ops.Timestamp(t); return &v }

func identityStatus(mod func(*ops.HubIdentityStatusResult)) ops.HubIdentityStatusResult {
	now := time.Now()
	st := ops.HubIdentityStatusResult{
		State: "B3", StateLabel: "B3 established", PassphraseSet: true, Serving: true,
		RootFingerprint: "ROOTFINGERPRINT0123456789", OpFingerprint: "OPFINGERPRINT", Serial: 1700000000123,
		NotBefore: tsp(now.AddDate(0, 0, -10)), NotAfter: tsp(now.AddDate(0, 0, 170)), DaysLeft: 170,
		LastOp: "reissue", LastOpAt: tsp(now.Add(-time.Hour)),
	}
	if mod != nil {
		mod(&st)
	}
	return st
}

func serveIdentityPage(t *testing.T, st ops.HubIdentityStatusResult, user string) (*Server, string) {
	t.Helper()
	connect(t, newFakeOps().reply(ops.OpHubIdentityStatus, st))
	s := adminServer(t)
	return s, get(t, s, "/admin/identity", "X-Test-Session", user).Body.String()
}

func TestIdentityPageShowsTheStateAndOnlyTheButtonsThatFit(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*ops.HubIdentityStatusResult)
		has  []string
		not  []string
	}{
		{"B0", func(s *ops.HubIdentityStatusResult) {
			*s = ops.HubIdentityStatusResult{State: "B0", StateLabel: "B0 pristine"}
		}, []string{"B0 pristine", "not set yet", `data-identity-op="hub_identity_set_passphrase"`},
			[]string{`data-identity-op="hub_identity_reissue"`, `data-identity-op="hub_identity_rotate"`, `data-identity-op="hub_identity_change_passphrase"`}},
		{"B1", func(s *ops.HubIdentityStatusResult) {
			*s = ops.HubIdentityStatusResult{State: "B1", StateLabel: "B1 plain identity", RootFingerprint: "ROOTFP", MigratedFrom06: true}
		}, []string{"B1 plain identity", `data-identity-op="hub_identity_set_passphrase"`, "migrated from version 0.6"},
			[]string{`data-identity-op="hub_identity_reissue"`, `data-identity-op="hub_identity_rotate"`}},
		{"B2", func(s *ops.HubIdentityStatusResult) {
			*s = ops.HubIdentityStatusResult{State: "B2", StateLabel: "B2 sealed, no certificate", PassphraseSet: true, RootFingerprint: "ROOTFP"}
		}, []string{`data-identity-op="hub_identity_reissue"`, `data-identity-op="hub_identity_change_passphrase"`},
			[]string{`data-identity-op="hub_identity_set_passphrase"`, `data-identity-op="hub_identity_rotate"`}},
		{"B3", nil, []string{"B3 established", "ROOTFINGERPRINT0123456789", "OPFINGERPRINT", "1700000000123", "170 days left", "Last change",
			`data-identity-op="hub_identity_reissue"`, `data-identity-op="hub_identity_rotate"`, `data-identity-op="hub_identity_change_passphrase"`, `name="word"`},
			[]string{`data-identity-op="hub_identity_set_passphrase"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, body := serveIdentityPage(t, identityStatus(c.mod), adminHeader)
			mustHave(t, body, append(c.has, "<h1>Hub identity</h1>", `src="/static/identity.js"`)...)
			mustNotHave(t, body, c.not...)
		})
	}
}

func TestIdentityPageNeverEchoesAPassphraseAndTurnsOffAutofill(t *testing.T) {
	_, body := serveIdentityPage(t, identityStatus(nil), adminHeader)
	main := mainOf(t, body)
	inputs := regexp.MustCompile(`<input[^>]*type="password"[^>]*>`).FindAllString(main, -1)
	if len(inputs) < 8 {
		t.Fatalf("found %d password fields", len(inputs))
	}
	for _, in := range inputs {
		if strings.Contains(in, "value=") {
			t.Errorf("a password field carries a value: %s", in)
		}
		if !strings.Contains(in, `autocomplete="new-password"`) && !strings.Contains(in, `autocomplete="current-password"`) {
			t.Errorf("a password field without the autocomplete hint: %s", in)
		}
	}
	for _, form := range regexp.MustCompile(`<form[^>]*>`).FindAllString(main, -1) {
		if strings.Contains(form, "action=") || strings.Contains(form, "hx-post") {
			t.Errorf("a form posts by itself instead of through identity.js: %s", form)
		}
		if strings.Contains(strings.ToLower(form), `method="get"`) {
			t.Errorf("a passphrase form uses GET: %s", form)
		}
	}
}

func TestIdentityPageIsForAdministratorsOnly(t *testing.T) {
	fo := newFakeOps().reply(ops.OpHubIdentityStatus, identityStatus(nil))
	connect(t, fo)
	s := adminServer(t)
	for _, c := range []struct{ user string }{{userHeader}, {""}} {
		rec := get(t, s, "/admin/identity", "X-Test-Session", c.user)
		if rec.Code == http.StatusOK {
			t.Errorf("user %q got the page", c.user)
		}
	}
	if n := len(fo.called(ops.OpHubIdentityStatus)); n != 0 {
		t.Errorf("the status was read %d times for a non-admin", n)
	}
	// The navigation entry is the administrators'.
	user := get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	mustNotHave(t, user, "/admin/identity")
	mustHave(t, get(t, s, "/account", "X-Test-Session", adminHeader).Body.String(), `href="/admin/identity"`)
}

func TestIdentityPageShowsTheLockoutWithTheTimeLeft(t *testing.T) {
	until := time.Now().Add(14*time.Minute + 30*time.Second)
	_, body := serveIdentityPage(t, identityStatus(func(s *ops.HubIdentityStatusResult) {
		s.Web = ops.IdentityScopeStatus{Failures: 5, Locked: true, LockedUntil: tsp(until), RetryAfterSecs: 870}
	}), adminHeader)
	mustHave(t, body, `id="id-lock"`, `data-until="`, `<span id="id-lock-left">870</span> seconds`, "has its own counter")
	// No lock, no notice.
	_, body = serveIdentityPage(t, identityStatus(nil), adminHeader)
	mustNotHave(t, body, `id="id-lock"`)
}

func TestIdentityPageOffersTheRightReauthentication(t *testing.T) {
	_, local := serveIdentityPage(t, identityStatus(nil), "alice:admin")
	mustHave(t, local, `data-reauth="local"`, `data-fresh="0"`, "Changes need a sign-in from the last five minutes")
	mustNotHave(t, local, `data-reauth="oidc"`)
	_, fresh := serveIdentityPage(t, identityStatus(nil), "alice:admin:fresh")
	mustHave(t, fresh, `data-fresh="1"`, "Your sign-in is fresh until")
	_, sso := serveIdentityPage(t, identityStatus(nil), "alice:admin:oidc")
	mustHave(t, sso, `data-reauth="oidc"`, "Sign in again with the identity provider")
	mustNotHave(t, sso, `data-reauth="local"`)
	// The result of a re-authentication that returned to the page.
	s, _ := serveIdentityPage(t, identityStatus(nil), "alice:admin:oidc")
	body := get(t, s, "/admin/identity?reauth=unavailable", "X-Test-Session", "alice:admin:oidc").Body.String()
	mustHave(t, body, "reauth_unavailable", "Nothing was changed")
	body = get(t, s, "/admin/identity?reauth=ok", "X-Test-Session", "alice:admin:oidcfresh").Body.String()
	mustHave(t, body, "Signed in again.")
	// An unknown value shows nothing.
	body = get(t, s, "/admin/identity?reauth=%3Cscript%3E", "X-Test-Session", "alice:admin").Body.String()
	mustNotHave(t, body, "<script>alert", "reauth_unavailable", "Signed in again.")
}

func TestExpiryBannerFromThirtyDaysAndWhenNotServing(t *testing.T) {
	notAfter := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		st   IdentityStatus
		want []string
		kind string
	}{
		{"valid for long", IdentityStatus{Label: "B3 established", Serving: true, Established: true, NotAfter: notAfter, DaysLeft: 120}, nil, ""},
		{"29 days", IdentityStatus{Label: "B3 established", Serving: true, Established: true, NotAfter: notAfter, DaysLeft: 29, Warning: "warning"},
			[]string{"The Hub certificate expires on 2026-12-20 (29 days). Re-issue it."}, "banner-warning"},
		{"7 days is red", IdentityStatus{Label: "B3 established", Serving: true, Established: true, NotAfter: notAfter, DaysLeft: 6, Warning: "error"},
			[]string{"The Hub certificate expires on 2026-12-20 (6 days). Re-issue it."}, "banner-danger"},
		{"expired", IdentityStatus{Label: "B3 established", Established: true, NotAfter: notAfter, DaysLeft: -1, Warning: "expired"},
			[]string{"expired on 2026-12-20", "Workers are refused"}, "banner-danger"},
		{"B0", IdentityStatus{Label: "B0 pristine"}, []string{"Set the Hub passphrase", "Workers cannot connect"}, "banner-warning"},
		{"B1", IdentityStatus{Label: "B1 plain identity"}, []string{"Set the Hub passphrase"}, "banner-warning"},
		{"B2", IdentityStatus{Label: "B2 sealed, no certificate"}, []string{"no certificate", "re-issue"}, "banner-warning"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			release := SetIdentityStatus(func(context.Context) (IdentityStatus, bool) { return c.st, true })
			defer release()
			connect(t, newFakeOps())
			s := adminServer(t)
			// Every page for an administrator carries it, not only the identity page.
			for _, path := range []string{"/account", "/admin/audit"} {
				body := get(t, s, path, "X-Test-Session", adminHeader).Body.String()
				if c.want == nil {
					mustNotHave(t, body, "Hub certificate expires", "Set the Hub passphrase")
					continue
				}
				mustHave(t, body, append(c.want, c.kind)...)
			}
			// Other users see none.
			mustNotHave(t, get(t, s, "/account", "X-Test-Session", userHeader).Body.String(), "Hub certificate", "Set the Hub passphrase", "no certificate")
		})
	}
	// Without a connected source there is no banner and nothing fails.
	connect(t, newFakeOps())
	mustNotHave(t, get(t, adminServer(t), "/account", "X-Test-Session", adminHeader).Body.String(), "Hub certificate")
}

func TestSetIdentityStatusReleaseOnlyDisconnectsItself(t *testing.T) {
	first := SetIdentityStatus(func(context.Context) (IdentityStatus, bool) { return IdentityStatus{Label: "first"}, true })
	second := SetIdentityStatus(func(context.Context) (IdentityStatus, bool) { return IdentityStatus{Label: "second"}, true })
	first() // an earlier Hub stopping must not disconnect the later one
	if st, ok := currentIdentityStatus(context.Background()); !ok || st.Label != "second" {
		t.Errorf("status = %+v %v", st, ok)
	}
	second()
	if _, ok := currentIdentityStatus(context.Background()); ok {
		t.Error("still connected")
	}
}

func TestWorkersCountdown(t *testing.T) {
	notAfter := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		st       IdentityStatus
		wantText string
		wantTone string
	}{
		{"long", IdentityStatus{Established: true, NotAfter: notAfter, DaysLeft: 120}, "Hub certificate valid until 2026-12-20 (120 days left).", "ok"},
		{"warning", IdentityStatus{Established: true, NotAfter: notAfter, DaysLeft: 12, Warning: "warning"}, "Hub certificate: 12 days left (expires 2026-12-20).", "warning"},
		{"urgent", IdentityStatus{Established: true, NotAfter: notAfter, DaysLeft: 1, Warning: "error"}, "Hub certificate: 1 day left (expires 2026-12-20).", "danger"},
		{"expired", IdentityStatus{Established: true, NotAfter: notAfter, DaysLeft: -2, Warning: "expired"}, "Hub certificate expired on 2026-12-20: Workers are refused.", "danger"},
	} {
		t.Run(c.name, func(t *testing.T) {
			release := SetIdentityStatus(func(context.Context) (IdentityStatus, bool) { return c.st, true })
			defer release()
			req := withTestSession(t, "alice:admin")
			text, tone := workersCountdown(req)
			if text != c.wantText || tone != c.wantTone {
				t.Errorf("countdown = %q %q, want %q %q", text, tone, c.wantText, c.wantTone)
			}
			if text, _ := workersCountdown(withTestSession(t, "bob:user")); text != "" {
				t.Errorf("a non-admin sees the countdown %q", text)
			}
		})
	}
}

// withTestSession returns a request whose context carries the session that
// the fake session layer builds for the header value.
func withTestSession(t *testing.T, header string) *http.Request {
	t.Helper()
	var got *http.Request
	fakeSessions{}.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })).
		ServeHTTP(nopResponse{}, func() *http.Request {
			r, _ := http.NewRequest("GET", "/workers", nil)
			r.Header.Set("X-Test-Session", header)
			return r
		}())
	if got == nil {
		t.Fatal("no request")
	}
	return got
}

type nopResponse struct{}

func (nopResponse) Header() http.Header         { return http.Header{} }
func (nopResponse) Write(b []byte) (int, error) { return len(b), nil }
func (nopResponse) WriteHeader(int)             {}

func TestTheWorkersPageHeaderShowsTheCountdownToAdministrators(t *testing.T) {
	SetAdminOps(nil)
	SetLiveSource(newFakeSource(Dashboard{}, Dashboard{}))
	t.Cleanup(func() { SetLiveSource(nil) })
	release := SetIdentityStatus(func(context.Context) (IdentityStatus, bool) {
		return IdentityStatus{Established: true, NotAfter: time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC), DaysLeft: 12, Warning: "warning"}, true
	})
	defer release()
	s := adminServer(t)
	body := get(t, s, "/workers", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `id="identity-countdown"`, `data-tone="warning"`, "Hub certificate: 12 days left (expires 2026-12-20).", `href="/admin/identity"`)
	mustNotHave(t, get(t, s, "/workers", "X-Test-Session", userHeader).Body.String(), "identity-countdown", "Hub certificate")
}

// F9: before set, change and rotate the page states the passphrase policy.
func TestIdentityPageShowsThePassphrasePolicyBeforeSetChangeAndRotate(t *testing.T) {
	policy := []string{"At least 14 characters", "Unicode code points", "at most 1024 bytes", "recommended", "Unicode characters", `data-count-passphrase`}
	_, b3 := serveIdentityPage(t, identityStatus(nil), adminHeader)
	main := mainOf(t, b3)
	if n := strings.Count(main, `data-passphrase-policy`); n != 2 { // rotate and change
		t.Errorf("B3 shows the policy %d times, want 2 (rotate, change)", n)
	}
	mustHave(t, main, policy...)
	_, b0 := serveIdentityPage(t, identityStatus(func(s *ops.HubIdentityStatusResult) {
		*s = ops.HubIdentityStatusResult{State: "B0", StateLabel: "B0 pristine"}
	}), adminHeader)
	mustHave(t, mainOf(t, b0), policy...)
	if n := strings.Count(mainOf(t, b0), `data-passphrase-policy`); n != 1 {
		t.Errorf("B0 shows the policy %d times, want 1", n)
	}
}
