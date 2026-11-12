package server

import (
	"context"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/oidc"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
)

// refreshClock fires its first timer at once (the start delay) and never
// fires another, so a Hub runs exactly one pass. It records the waits.
type refreshClock struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (c *refreshClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays = append(c.delays, d)
	ch := make(chan time.Time, 1)
	if len(c.delays) == 1 {
		ch <- time.Now()
	}
	return ch
}

func (c *refreshClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.delays)
}

func refreshRegistry(t *testing.T, clock oidc.Clock) *Registry {
	t.Helper()
	wr := web.NewRegistry()
	r := webRegistryWithoutWeb(t)
	registerOIDC(r, wr)
	registerOIDCRefresh(r, func(ref *oidc.Refresher) { ref.Clock = clock })
	registerWeb(r, wr)
	return r
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOIDCRefreshModuleIsRegisteredInTheDefaultRegistry(t *testing.T) {
	if !slices.Contains(DefaultRegistry.Names(), "oidc-refresh") {
		t.Errorf("modules = %v", DefaultRegistry.Names())
	}
	if !IsLiveSetting(config.KeyOIDCRefreshInterval) {
		t.Error("oidc_refresh_interval is not declared live")
	}
}

func TestOIDCRefresherRunsWithTheHubAndStopsWithIt(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	t.Setenv("MINION_OIDC_REFRESH_INTERVAL", "2h")

	// One OIDC account with a refresh token whose identity provider does not
	// answer: the pass must reach the account through the Hub's store and
	// providers, and change nothing.
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const issuer = "https://127.0.0.1:1"
	if err := st.PutOIDCProvider(ctx, store.OIDCProviderRow{ID: "corp", Name: "Corp", Issuer: issuer, ClientID: "c",
		ClientSecret: "s", Enabled: true, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRoleRules(ctx, "corp", "admin", []string{"ops"}); err != nil {
		t.Fatal(err)
	}
	res, err := st.SyncOIDCLogin(ctx, store.OIDCSyncParams{Issuer: issuer, Subject: "alice", ProviderID: "corp",
		PreferredUsername: "alice", Roles: []string{"admin"}, CreateIfMissing: true, RefreshToken: "rt-secret", NowMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	clock := &refreshClock{}
	h := startHub(t, &Config{DataDir: dir}, refreshRegistry(t, clock))

	// The refresher started with the Hub: its first wait is the start delay,
	// and it ran a pass over the account.
	waitUntil(t, "the first pass", func() bool { return strings.Contains(h.logs.String(), "oidc refresh: pass done") })
	logs := h.logs.String()
	if !strings.Contains(logs, "targets=1") || !strings.Contains(logs, "unchanged=1") || !strings.Contains(logs, "identity provider not reachable") {
		t.Errorf("the pass did not reach the account:\n%s", logs)
	}
	// The interval comes from the environment through the resolver (AD-19).
	waitUntil(t, "the second wait", func() bool { return len(clock.waits()) == 2 })
	if got := clock.waits(); got[0] != oidc.DefaultRefreshStartDelay || got[1] != time.Hour {
		// The configured 2h interval is capped at 1h while an account holds
		// an IdP admin grant (F-07).
		t.Errorf("waits = %v, want [%v 1h]", got, oidc.DefaultRefreshStartDelay)
	}
	if strings.Contains(logs, "rt-secret") {
		t.Errorf("the refresh token reached the log:\n%s", logs)
	}
	if strings.Contains(logs, "background refresh stopped") {
		t.Fatal("the refresher stopped before the Hub did")
	}

	// Stopping the Hub stops the refresher, and Stop waited for it.
	h.stop(t)
	if !strings.Contains(h.logs.String(), "oidc refresh: background refresh stopped") {
		t.Errorf("the refresher did not stop with the Hub:\n%s", h.logs)
	}

	// The unreachable IdP changed nothing.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	u, err := st2.UserByID(ctx, res.User.ID)
	if err != nil || u.Role != "admin" {
		t.Errorf("user = %+v, %v", u, err)
	}
}

func TestOIDCRefreshIntervalIsAPanelSettingThatAppliesAtOnce(t *testing.T) {
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "integration-password-1")
	r := refreshRegistry(t, &refreshClock{})
	var svc *SettingsService
	if err := r.Register("settings-probe", 60, func(_ context.Context, env *Env) error {
		var err error
		svc, err = Require[*SettingsService](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	startHub(t, &Config{DataDir: hubstore.Dir(t)}, r)
	ctx := context.Background()

	res, err := svc.Set(ctx, admin, setArgs(config.KeyOIDCRefreshInterval, "6h"))
	if err != nil {
		t.Fatalf("the settings API refused oidc_refresh_interval: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("a live setting asked for a restart: %+v", res.Warnings)
	}
	if info := listByKey(t, svc)[config.KeyOIDCRefreshInterval]; info.Value != "6h" || info.Source != "panel" || info.Env != "MINION_OIDC_REFRESH_INTERVAL" {
		t.Errorf("setting = %+v", info)
	}
	if d, err := oidc.IntervalFromResolver(svc.Resolver())(ctx); err != nil || d != 6*time.Hour {
		t.Errorf("interval = %v, %v; want 6h at once", d, err)
	}

	// The environment wins over the panel, so the API refuses the change.
	t.Setenv("MINION_OIDC_REFRESH_INTERVAL", "3h")
	if _, err := svc.Set(ctx, admin, setArgs(config.KeyOIDCRefreshInterval, "12h")); err == nil {
		t.Error("the panel changed a setting that the environment sets")
	}
	if d, err := oidc.IntervalFromResolver(svc.Resolver())(ctx); err != nil || d != 3*time.Hour {
		t.Errorf("interval = %v, %v; want 3h from the environment", d, err)
	}
}
